package controlling_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// compared is setup's tenant with a 2026 budget and actuals in company
// 2000. By cost center: plan 100.00 in May and 50.00 in June on the Admin
// cost center's office supplies (520000), 20.00 in June on Sales'; actual
// 120.00 in May (projected) and 30.00 in June (still queued) on Admin's,
// and in June 10.00 on Sales' office supplies and 80.00 of revenue
// (400000) on Sales, which has no plan. By profit center (T56g3): plan
// 200.00 in May on PC1's office supplies and 40.00 in June on PC2's,
// which has no actual; actual PC1's, derived from Admin's lines, and in
// June 25.00 of revenue with PC1 entered (queued), which has no plan,
// against the bank with PC1 entered, a balance sheet line no report shows.
type compared struct {
	plan
	budget string
	pc2    uuid.UUID
}

func setupCompared(t *testing.T) compared {
	t.Helper()
	c := compared{plan: setup(t)}
	pc, _ := c.Must(201, "POST", "/v1/profit-centers", fmt.Sprintf(`{"code": "PC2", "name": "PC 2", "responsible": "Jane Doe",
		"legal_entity_ids": [%q], "valid_from": "2026-01-01", "reason": "r"}`, c.DE), "")
	c.Approve(t, pc["pending_change_id"].(string))
	c.pc2 = uuid.MustParse(pc["id"].(string))
	c.budget = c.category(t, "PLAN")
	c.batch(t, c.budget, "budget", line(c.c.Admin, c.office, 5, "100.00"), line(c.c.Admin, c.office, 6, "50.00"), line(c.c.Sales, c.office, 6, "20.00"))
	c.batch(t, c.budget, "profit", pcLine(c.c.Profit, c.office, 5, "200.00"), pcLine(c.pc2, c.office, 6, "40.00"))
	c.post(t, "2026-05-10", on(c.office, 12000, &c.c.Admin), on(c.bank, -12000, nil))
	project(t)
	c.post(t, "2026-06-15", on(c.office, 3000, &c.c.Admin), on(c.office, 1000, &c.c.Sales), on(c.revenue, -8000, &c.c.Sales), on(c.bank, 4000, nil))
	revenue, bank := on(c.revenue, -2500, nil), on(c.bank, 2500, nil)
	revenue.ProfitCenterID, bank.ProfitCenterID = &c.c.Profit, &c.c.Profit
	c.post(t, "2026-06-20", revenue, bank)
	return c
}

// with is c calling as p's principal.
func (c compared) with(p plan) compared {
	c.plan = p
	return c
}

// compare lists the rows of a query of the cost center report as
// "center/account plan actual variance percent", the next page's token
// last as "next:...".
func (c compared) compare(t *testing.T, query string) []string {
	t.Helper()
	return c.compareBy(t, "cost", query)
}

// compareBy is compare of the cost or profit center report.
func (c compared) compareBy(t *testing.T, kind, query string) []string {
	t.Helper()
	page, _ := c.Must(200, "GET", "/v1/"+kind+"-center-plan-actual?plan_category_id="+c.budget+"&legal_entity_id="+c.DE.String()+"&fiscal_year=2026"+query,
		"", "")
	var out []string
	for _, it := range page["items"].([]any) {
		r := it.(map[string]any)
		amount := func(k string) any { return r[k].(map[string]any)["amount"] }
		pct, ok := r["variance_percent"].(string)
		if !ok {
			pct = "-"
		}
		center := c.code(r[kind+"_center_id"].(string))
		if center == c.pc2.String() {
			center = "PC2"
		}
		out = append(out, fmt.Sprintf("%s/%s %v %v %v %s", center, c.code(r["account_id"].(string)), amount("plan"),
			amount("actual"), amount("variance"), pct))
	}
	if tok, ok := page["next_page_token"].(string); ok {
		out = append(out, "next:"+tok)
	}
	return out
}

func TestCostCenterPlanActual(t *testing.T) {
	c := setupCompared(t)
	var root, pcRoot string
	pgtest.Scan(t, `SELECT n.id::text FROM ledger.hierarchy h JOIN ledger.hierarchy_node n ON n.tenant_id = h.tenant_id AND n.hierarchy_id = h.id
		WHERE h.tenant_id = $1 AND h.kind = 'cost_center' AND h.is_standard AND n.parent_id IS NULL`, []any{c.Tenant.ID}, &root)
	pgtest.Scan(t, `SELECT n.id::text FROM ledger.hierarchy h JOIN ledger.hierarchy_node n ON n.tenant_id = h.tenant_id AND n.hierarchy_id = h.id
		WHERE h.tenant_id = $1 AND h.kind = 'profit_center' AND h.is_standard AND n.parent_id IS NULL`, []any{c.Tenant.ID}, &pcRoot)
	all := []string{"1000/520000 150.00 150.00 0.00 0.00", "2000/400000 0.00 -80.00 -80.00 -", "2000/520000 20.00 10.00 -10.00 -50.00"}
	for name, q := range map[string]struct {
		query string
		want  []string
	}{
		"every cost center":              {"", all},
		"June":                           {"&fiscal_period_from=6&fiscal_period_to=6", []string{"1000/520000 50.00 30.00 -20.00 -40.00", all[1], all[2]}},
		"May":                            {"&fiscal_period_to=5", []string{"1000/520000 100.00 120.00 20.00 20.00"}},
		"the cost center":                {"&cost_center_id=" + c.c.Admin.String(), all[:1]},
		"the standard hierarchy's root":  {"&hierarchy_node_id=" + root, all},
		"the root and the cost center":   {"&hierarchy_node_id=" + root + "&cost_center_id=" + c.c.Sales.String(), all[1:]},
		"a profit center hierarchy node": {"&hierarchy_node_id=" + pcRoot, nil},
		"a center not used":              {"&cost_center_id=" + c.c.Locked.String(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			wantList(t, "rows", sorted(c.with(c.in(t)).compare(t, q.query)), q.want)
		})
	}
	var pages []string
	query := "&page_size=1"
	for range 4 {
		page := c.compare(t, query)
		if n := len(page); n == 2 {
			pages, query = append(pages, page[0]), "&page_size=1&page_token="+strings.TrimPrefix(page[1], "next:")
			continue
		}
		pages = append(pages, page...)
		break
	}
	wantList(t, "pages of one row", sorted(pages), all)
	c.Problem(400, "request.invalid_parameter", "GET", "/v1/cost-center-plan-actual?plan_category_id="+c.budget+"&legal_entity_id="+c.DE.String()+
		"&fiscal_year=2026&fiscal_period_from=7&fiscal_period_to=6", "", "")
	c.Problem(400, "request.invalid_page_token", "GET", "/v1/cost-center-plan-actual?plan_category_id="+c.budget+"&legal_entity_id="+c.DE.String()+
		"&fiscal_year=2026&page_token=x", "", "")

	forecast := c
	forecast.budget = c.category(t, "FORECAST")
	wantList(t, "a category without a plan", sorted(forecast.compare(t, "")),
		[]string{"1000/520000 0.00 150.00 150.00 -", "2000/400000 0.00 -80.00 -80.00 -", "2000/520000 0.00 10.00 10.00 -"})
	unknown := c
	unknown.budget = uuid.NewString()
	wantList(t, "an unknown category", unknown.compare(t, ""), nil)
	wantList(t, "a company the reader cannot act in", c.with(c.as(c.Reader(t, "outsider"))).compare(t, ""), nil)
	pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'ledger.entry.read'`, c.Tenant.ID)
	c.Problem(403, "identity.permission_missing", "GET", "/v1/cost-center-plan-actual?plan_category_id="+c.budget+"&legal_entity_id="+c.DE.String()+
		"&fiscal_year=2026", "", "")
}

// TestProfitCenterPlanActual: the profit center report (T56g3) as the
// cost center report, by the profit center a line carries, on profit and
// loss accounts only, the cost center plan lines left out.
func TestProfitCenterPlanActual(t *testing.T) {
	c := setupCompared(t)
	var ccRoot, root string
	pgtest.Scan(t, `SELECT n.id::text FROM ledger.hierarchy h JOIN ledger.hierarchy_node n ON n.tenant_id = h.tenant_id AND n.hierarchy_id = h.id
		WHERE h.tenant_id = $1 AND h.kind = 'cost_center' AND h.is_standard AND n.parent_id IS NULL`, []any{c.Tenant.ID}, &ccRoot)
	pgtest.Scan(t, `SELECT n.id::text FROM ledger.hierarchy h JOIN ledger.hierarchy_node n ON n.tenant_id = h.tenant_id AND n.hierarchy_id = h.id
		WHERE h.tenant_id = $1 AND h.kind = 'profit_center' AND h.is_standard AND n.parent_id IS NULL`, []any{c.Tenant.ID}, &root)
	all := []string{"PC1/400000 0.00 -25.00 -25.00 -", "PC1/520000 200.00 150.00 -50.00 -25.00", "PC2/520000 40.00 0.00 -40.00 -100.00"}
	for name, q := range map[string]struct {
		query string
		want  []string
	}{
		"every profit center":              {"", all},
		"June":                             {"&fiscal_period_from=6&fiscal_period_to=6", []string{all[0], "PC1/520000 0.00 30.00 30.00 -", all[2]}},
		"May":                              {"&fiscal_period_to=5", []string{"PC1/520000 200.00 120.00 -80.00 -40.00"}},
		"the profit center":                {"&profit_center_id=" + c.c.Profit.String(), all[:2]},
		"a profit center with a plan only": {"&profit_center_id=" + c.pc2.String(), all[2:]},
		"the standard hierarchy's root":    {"&hierarchy_node_id=" + root, all},
		"the root and the profit center":   {"&hierarchy_node_id=" + root + "&profit_center_id=" + c.pc2.String(), all[2:]},
		"a cost center hierarchy node":     {"&hierarchy_node_id=" + ccRoot, nil},
		"a center not used":                {"&profit_center_id=" + uuid.NewString(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			wantList(t, "rows", sorted(c.with(c.in(t)).compareBy(t, "profit", q.query)), q.want)
		})
	}
	var pages []string
	query := "&page_size=1"
	for range 4 {
		page := c.compareBy(t, "profit", query)
		if n := len(page); n == 2 {
			pages, query = append(pages, page[0]), "&page_size=1&page_token="+strings.TrimPrefix(page[1], "next:")
			continue
		}
		pages = append(pages, page...)
		break
	}
	wantList(t, "pages of one row", sorted(pages), all)
	path := "/v1/profit-center-plan-actual?plan_category_id=" + c.budget + "&legal_entity_id=" + c.DE.String() + "&fiscal_year=2026"
	c.Problem(400, "request.invalid_parameter", "GET", path+"&fiscal_period_from=7&fiscal_period_to=6", "", "")
	c.Problem(400, "request.invalid_page_token", "GET", path+"&page_token=x", "", "")
	costToken := strings.TrimPrefix(c.compare(t, "&page_size=1")[1], "next:")
	c.Problem(400, "request.invalid_page_token", "GET", path+"&page_token="+costToken, "", "")

	forecast := c
	forecast.budget = c.category(t, "FORECAST")
	wantList(t, "a category without a plan", sorted(forecast.compareBy(t, "profit", "")),
		[]string{"PC1/400000 0.00 -25.00 -25.00 -", "PC1/520000 0.00 150.00 150.00 -"})
	unknown := c
	unknown.budget = uuid.NewString()
	wantList(t, "an unknown category", unknown.compareBy(t, "profit", ""), nil)
	wantList(t, "a company the reader cannot act in", c.with(c.as(c.Reader(t, "outsider"))).compareBy(t, "profit", ""), nil)
	other := c
	other.Client = fixture.New(t, perms...).In(t)
	wantList(t, "another tenant", other.compareBy(t, "profit", ""), nil)
	pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'ledger.entry.read'`, c.Tenant.ID)
	c.Problem(403, "identity.permission_missing", "GET", path, "", "")
}
