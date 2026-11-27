package extract_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// What a reader sees of the centers' snapshots (T56i2).
type (
	costCenterRow struct {
		Company        string  `parquet:"company"`
		CostCenter     string  `parquet:"cost_center"`
		CostCenterName string  `parquet:"cost_center_name"`
		CostCenterNode *string `parquet:"cost_center_node,optional"`
	}
	profitCenterRow struct {
		ProfitCenter     string  `parquet:"profit_center"`
		ProfitCenterName string  `parquet:"profit_center_name"`
		ProfitCenterNode *string `parquet:"profit_center_node,optional"`
	}
	segmentRow struct {
		Segment     string `parquet:"segment"`
		SegmentName string `parquet:"segment_name"`
	}
)

// opt is a nullable column as the tests compare it: "-" for null.
func opt(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

// centerSnapshots renders the run's centers' snapshots as "company code
// name node", "code name node" and "code name", in file order.
func (e env) centerSnapshots(t *testing.T, key string) (cost, profit, segments []string) {
	t.Helper()
	m := e.manifest(t, key)
	for _, r := range read[costCenterRow](t, e, m, "cost_centers") {
		cost = append(cost, fmt.Sprintf("%s %s %s %s", r.Company, r.CostCenter, r.CostCenterName, opt(r.CostCenterNode)))
	}
	for _, r := range read[profitCenterRow](t, e, m, "profit_centers") {
		profit = append(profit, fmt.Sprintf("%s %s %s", r.ProfitCenter, r.ProfitCenterName, opt(r.ProfitCenterNode)))
	}
	for _, r := range read[segmentRow](t, e, m, "segments") {
		segments = append(segments, r.Segment+" "+r.SegmentName)
	}
	return cost, profit, segments
}

// approve has the approver approve the pending request of a center
// change, as fixture.Centers does.
func (e env) approve(t *testing.T, change any) {
	t.Helper()
	r := e.Pending(t, fmt.Sprint(change))
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`, fmt.Sprintf(`"%v"`, r["version"]))
}

// TestCenterSnapshots: the active cost centers by company and code, the
// active profit centers and the segments are extracted with their names
// and, for a center, the node of today's standard hierarchy it is placed
// at (ADR-0050 §3, decision P7). Two companies' centers of one code are
// two rows; a center placed in no standard hierarchy has no node, and one
// still pending is not extracted. A center moved to another node is under
// that node alone in the next run, and a renamed one under its new name,
// the unchanged datasets' files kept (ADR-0044 As built T56i2).
func TestCenterSnapshots(t *testing.T) {
	e := setup(t)
	e.centerEntry(t) // CC100 of 2000, PC10 and SEG1, each center at its standard hierarchy's LEAF
	e.CostCenter(t, e.US, "CC100")
	e.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": "CC200", "name": "Pending", "responsible": "Jane Doe",
		"category": "ADMIN", "valid_from": "2026-01-01", "reason": "Not approved"}`, e.DE), "")
	first := e.run(t)
	cost, profit, segments := e.centerSnapshots(t, first.ManifestKey)
	if want := []string{"1000 CC100 Cost center CC100 -", "2000 CC100 Cost center hundred LEAF"}; fmt.Sprint(cost) != fmt.Sprint(want) {
		t.Errorf("cost centers: got %q, want %q", cost, want)
	}
	if want := []string{"PC10 Profit center ten LEAF"}; fmt.Sprint(profit) != fmt.Sprint(want) {
		t.Errorf("profit centers: got %q, want %q", profit, want)
	}
	if want := []string{"SEG1 Segment one"}; fmt.Sprint(segments) != fmt.Sprint(want) {
		t.Errorf("segments: got %q, want %q", segments, want)
	}

	t.Run("moved and renamed", func(t *testing.T) {
		page, _ := e.Must(200, "GET", "/v1/hierarchies?kind=cost_center&is_standard=true", "", "")
		id := page["items"].([]any)[0].(map[string]any)["id"].(string)
		h, tag := e.Must(200, "GET", "/v1/hierarchies/"+id, "", "")
		ch, _ := e.Must(200, "POST", fmt.Sprintf("/v1/hierarchies/%s/nodes/%v:add", id, h["root_node_id"]), `{"code": "OTHER", "name": "Other", "reason": "r"}`, tag)
		e.approve(t, ch["id"])
		node := ch["payload"].(map[string]any)["node_id"]
		var center string
		pgtest.Scan(t, `SELECT id FROM ledger.cost_center WHERE tenant_id = $1 AND legal_entity_id = $2 AND code = 'CC100'`, []any{e.Tenant.ID, e.DE}, &center)
		_, tag = e.Must(200, "GET", "/v1/hierarchies/"+id, "", "")
		ch, _ = e.Must(200, "POST", "/v1/hierarchies/"+id+":place", fmt.Sprintf(`{"center_id": %q, "node_id": %q, "reason": "r"}`, center, node), tag)
		e.approve(t, ch["id"])
		pgtest.Exec(t, `UPDATE ledger.cost_center SET name = 'Finance office', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, center)
		next := e.run(t)
		cost, _, _ := e.centerSnapshots(t, next.ManifestKey)
		if want := []string{"1000 CC100 Cost center CC100 -", "2000 CC100 Finance office OTHER"}; fmt.Sprint(cost) != fmt.Sprint(want) {
			t.Errorf("cost centers: got %q, want %q", cost, want)
		}
		a, b := e.manifest(t, first.ManifestKey), e.manifest(t, next.ManifestKey)
		if fmt.Sprint(a.Datasets["profit_centers"].Files) != fmt.Sprint(b.Datasets["profit_centers"].Files) {
			t.Errorf("profit centers' files: got %v, want %v kept", b.Datasets["profit_centers"].Files, a.Datasets["profit_centers"].Files)
		}
	})
}
