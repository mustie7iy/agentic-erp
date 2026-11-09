package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// migrated runs fn as the tenant's human in a committed unit of work, as
// a migration run's load does.
func (c centers) migrated(t *testing.T, fn func(ctx context.Context, m ledger.CenterMigration) error) error {
	t.Helper()
	return apitest.Committed(t, c.ten, c.ten.Human, func(ctx context.Context) error { return fn(ctx, ledger.CenterMigration{}) })
}

func day(s string) time.Time { return date(s) }

// migratedCenters creates a segment, a profit center of company 2000 with
// two periods (the second locked) and a cost center with three slices, the
// first two alike, moving to the profit center on 2026-07-01.
func (c centers) migratedCenters(t *testing.T) (seg, pc, cc uuid.UUID) {
	t.Helper()
	if err := c.migrated(t, func(ctx context.Context, m ledger.CenterMigration) error {
		var err error
		if seg, err = m.CreateSegment(ctx, ledger.NewSegment{Code: "SEG-M", Name: "Migrated"}); err != nil {
			return err
		}
		desc := "Migrated from SAP"
		if pc, err = m.CreateProfitCenter(ctx, ledger.MigratedProfitCenter{Code: "P1", Texts: ledger.Texts{Name: "P 1", Responsible: "R"}, SegmentID: &seg,
			LegalEntityIDs: []uuid.UUID{c.de}, Reason: "Migration run x: SAP profit center 1000/P1", Periods: []ledger.MigratedPeriod[ledger.ProfitCenterAttrs]{
				{From: day("2020-01-01"), To: day("2026-10-01")}, {From: day("2026-10-01"), Attr: ledger.ProfitCenterAttrs{Locked: true}}}}); err != nil {
			return err
		}
		cc, err = m.CreateCostCenter(ctx, ledger.MigratedCostCenter{LegalEntityID: c.de, Code: "C1", Texts: ledger.Texts{Name: "C 1", Description: &desc, Responsible: "R"},
			Reason: "Migration run x: SAP cost center 1000/C1", Periods: []ledger.MigratedPeriod[ledger.CostCenterAttrs]{
				{From: day("2020-01-01"), To: day("2025-01-01"), Attr: ledger.CostCenterAttrs{Category: "ADMIN", LockRevenue: true}},
				{From: day("2025-01-01"), To: day("2026-07-01"), Attr: ledger.CostCenterAttrs{Category: "ADMIN", LockRevenue: true}},
				{From: day("2026-07-01"), Attr: ledger.CostCenterAttrs{Category: "ADMIN", ProfitCenterID: pc}}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return seg, pc, cc
}

// TestCenterMigration: a migration run's centers are created active at
// once (ADR-0050 §10, T56h), each creation an applied center change
// without an approval request, with their periods, at the standard
// hierarchy's UNASSIGNED leaf, which is created with the hierarchy.
func TestCenterMigration(t *testing.T) {
	c := setupBareCenters(t)
	seg, pc, cc := c.migratedCenters(t)
	got, _ := c.Must(200, "GET", "/v1/cost-centers/"+cc.String(), "", "")
	if got["status"] != "active" || got["description"] != "Migrated from SAP" {
		t.Errorf("cost center: got status %v, description %v, want active, Migrated from SAP", got["status"], got["description"])
	}
	wantPeriods(t, "cost center", periods(t, got, "lock_actual_revenue"), "2020-01-01..2026-07-01 true", "2026-07-01..<nil> false")
	wantPeriods(t, "cost center's profit centers", periods(t, got, "profit_center_id"), "2020-01-01..2026-07-01 <nil>", fmt.Sprintf("2026-07-01..<nil> %s", pc))
	p, _ := c.Must(200, "GET", "/v1/profit-centers/"+pc.String(), "", "")
	wantPeriods(t, "profit center", periods(t, p, "locked"), "2020-01-01..2026-10-01 false", "2026-10-01..<nil> true")
	if p["segment_id"] != seg.String() {
		t.Errorf("profit center's segment: got %v, want %s", p["segment_id"], seg)
	}
	page, _ := c.Must(200, "GET", "/v1/center-changes?cost_center_id="+cc.String(), "", "")
	items := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("cost center's changes: got %d, want 1", len(items))
	}
	ch := items[0].(map[string]any)
	if ch["status"] != "applied" || ch["kind"] != "create" || ch["workflow_request_id"] != nil || ch["reason"] != "Migration run x: SAP cost center 1000/C1" {
		t.Errorf("cost center's change: got %v, want an applied create without a workflow request", ch)
	}
	if n := len(ch["payload"].(map[string]any)["periods"].([]any)); n != 2 {
		t.Errorf("change's periods: got %d, want 2", n)
	}
	paths, _ := c.Must(200, "GET", "/v1/cost-centers/"+cc.String()+"/hierarchy-paths", "", "")
	if s := fmt.Sprint(paths["items"]); !strings.Contains(s, "UNASSIGNED") {
		t.Errorf("cost center's place: got %s, want the UNASSIGNED leaf", s)
	}
	for _, e := range []struct{ name, id string }{{"segment_changed", "segment_id"}, {"profit_center_changed", "profit_center_id"}, {"cost_center_changed", "cost_center_id"}} {
		got := apitest.Published(t, c.ten, outbox.Event{Context: "ledger", Name: e.name})
		if len(got) != 1 || got[0][e.id] == nil {
			t.Errorf("%s: got %v, want the migrated center's", e.name, got)
		}
	}
	c.migratedReads(t, seg, pc, cc)
}

// migratedReads: the reconciliation's reads give the centers as created.
func (c centers) migratedReads(t *testing.T, seg, pc, cc uuid.UUID) {
	t.Helper()
	if err := c.migrated(t, func(ctx context.Context, m ledger.CenterMigration) error {
		costs, err := m.CostCenters(ctx, []uuid.UUID{cc, uuid.New()})
		if err != nil {
			return err
		}
		if s := costs[cc]; len(costs) != 1 || s.Code != "C1" || s.LegalEntityID != c.de || s.Status != ledger.CenterActive || len(s.Periods) != 2 ||
			s.Periods[1].Attr.ProfitCenterID != pc {
			t.Errorf("cost centers: got %+v, want C1 of 2000, active, two periods", costs)
		}
		profits, err := m.ProfitCenters(ctx, []uuid.UUID{pc})
		if err != nil {
			return err
		}
		if s := profits[pc]; s.Code != "P1" || *s.SegmentID != seg || fmt.Sprint(s.LegalEntityIDs) != fmt.Sprint([]uuid.UUID{c.de}) || !s.Periods[1].Attr.Locked {
			t.Errorf("profit centers: got %+v, want P1 of 2000, locked from its second period", profits)
		}
		segs, err := m.Segments(ctx, []uuid.UUID{seg})
		if err != nil {
			return err
		}
		if segs[seg] != "SEG-M" {
			t.Errorf("segments: got %v, want SEG-M", segs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCenterMigrationAssigns: a profit center an earlier run created is
// assigned to more companies by an applied change; assigning it again
// changes nothing and is refused, as is a profit center not active.
func TestCenterMigrationAssigns(t *testing.T) {
	c := setupBareCenters(t)
	_, pc, _ := c.migratedCenters(t)
	assign := func(id uuid.UUID) error {
		return c.migrated(t, func(ctx context.Context, m ledger.CenterMigration) error {
			return m.AssignProfitCenter(ctx, id, []uuid.UUID{c.de, c.us}, "Migration run y")
		})
	}
	if err := assign(pc); err != nil {
		t.Fatal(err)
	}
	p, _ := c.Must(200, "GET", "/v1/profit-centers/"+pc.String(), "", "")
	if n := len(p["legal_entity_ids"].([]any)); n != 2 {
		t.Errorf("companies after the assignment: got %d, want 2", n)
	}
	page, _ := c.Must(200, "GET", "/v1/center-changes?profit_center_id="+pc.String(), "", "")
	kinds := ""
	for _, it := range page["items"].([]any) {
		kinds += it.(map[string]any)["kind"].(string) + "/" + it.(map[string]any)["status"].(string) + " "
	}
	if kinds != "create/applied assign/applied " {
		t.Errorf("profit center's changes: got %q, want an applied create and assign", kinds)
	}
	if code := problem.From(assign(pc)).Code; code != "ledger.nothing_to_change" {
		t.Errorf("assigned again: got %q, want ledger.nothing_to_change", code)
	}
	pending, _ := c.Must(201, "POST", "/v1/profit-centers", fmt.Sprintf(`{"code": "PEND", "name": "P", "responsible": "R", "legal_entity_ids": [%q],
		"valid_from": "2026-01-01", "reason": "r"}`, c.de), "")
	if code := problem.From(assign(uuid.MustParse(pending["id"].(string)))).Code; code != "ledger.invalid_center_status" {
		t.Errorf("a pending profit center: got %q, want ledger.invalid_center_status", code)
	}
	if code := problem.From(assign(uuid.New())).Code; code != "request.not_found" {
		t.Errorf("an unknown profit center: got %q, want request.not_found", code)
	}
	_, tag := c.Must(200, "GET", "/v1/profit-centers/"+pc.String(), "", "")
	c.Must(200, "POST", "/v1/profit-centers/"+pc.String()+":lock", `{"valid_from": "2027-01-01", "locked": false, "reason": "r"}`, tag)
	err := assign(pc)
	if code := problem.From(err).Code; code != "ledger.center_change_pending" {
		t.Errorf("assigned with a change pending: got %q, want ledger.center_change_pending", code)
	}
}

// TestCenterMigrationRefusals: a migrated center is checked as an
// approved create is.
func TestCenterMigrationRefusals(t *testing.T) {
	c := setupBareCenters(t)
	_, pc, _ := c.migratedCenters(t)
	other := uuid.Nil
	if err := c.migrated(t, func(ctx context.Context, m ledger.CenterMigration) error {
		var err error
		other, err = m.CreateProfitCenter(ctx, ledger.MigratedProfitCenter{Code: "P2", Texts: ledger.Texts{Name: "P 2", Responsible: "R"}, LegalEntityIDs: []uuid.UUID{c.us},
			Reason: "r", Periods: []ledger.MigratedPeriod[ledger.ProfitCenterAttrs]{{From: day("2026-01-01")}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	period := func(from, to string, a ledger.CostCenterAttrs) ledger.MigratedPeriod[ledger.CostCenterAttrs] {
		p := ledger.MigratedPeriod[ledger.CostCenterAttrs]{From: day(from), Attr: a}
		if to != "" {
			p.To = day(to)
		}
		return p
	}
	admin := ledger.CostCenterAttrs{Category: "ADMIN"}
	for name, tc := range map[string]struct {
		code    string
		periods []ledger.MigratedPeriod[ledger.CostCenterAttrs]
		want    string
	}{
		"no period":            {"X1", nil, "ledger.invalid_validity"},
		"periods with a gap":   {"X2", []ledger.MigratedPeriod[ledger.CostCenterAttrs]{period("2026-01-01", "2026-02-01", admin), period("2026-03-01", "", admin)}, "ledger.invalid_validity"},
		"an end before":        {"X3", []ledger.MigratedPeriod[ledger.CostCenterAttrs]{period("2026-01-01", "2025-01-01", admin)}, "ledger.invalid_validity"},
		"an unknown category":  {"X4", []ledger.MigratedPeriod[ledger.CostCenterAttrs]{period("2026-01-01", "", ledger.CostCenterAttrs{Category: "NOPE"})}, "ledger.cost_center_category_invalid"},
		"another company's PC": {"X5", []ledger.MigratedPeriod[ledger.CostCenterAttrs]{period("2026-01-01", "", ledger.CostCenterAttrs{Category: "ADMIN", ProfitCenterID: other})}, "ledger.profit_center_invalid"},
		"a PC not yet valid":   {"X6", []ledger.MigratedPeriod[ledger.CostCenterAttrs]{period("2019-01-01", "", ledger.CostCenterAttrs{Category: "ADMIN", ProfitCenterID: pc})}, "ledger.profit_center_invalid"},
		"a code taken":         {"C1", []ledger.MigratedPeriod[ledger.CostCenterAttrs]{period("2026-01-01", "", admin)}, "ledger.cost_center_exists"},
	} {
		t.Run(name, func(t *testing.T) {
			err := c.migrated(t, func(ctx context.Context, m ledger.CenterMigration) error {
				_, err := m.CreateCostCenter(ctx, ledger.MigratedCostCenter{LegalEntityID: c.de, Code: tc.code, Texts: ledger.Texts{Name: "N", Responsible: "R"},
					Reason: "r", Periods: tc.periods})
				return err
			})
			if code := problem.From(err).Code; code != tc.want {
				t.Errorf("got %q (%v), want %s", code, err, tc.want)
			}
		})
	}
	unknown := uuid.New()
	one := []ledger.MigratedPeriod[ledger.ProfitCenterAttrs]{{From: day("2026-01-01")}}
	for name, tc := range map[string]struct {
		in   ledger.MigratedProfitCenter
		want string
	}{
		"an unknown segment": {ledger.MigratedProfitCenter{Code: "P3", SegmentID: &unknown, Periods: one}, "request.reference_not_found"},
		"no period":          {ledger.MigratedProfitCenter{Code: "P4"}, "ledger.invalid_validity"},
		"a code taken":       {ledger.MigratedProfitCenter{Code: "P1", Periods: one}, "ledger.profit_center_exists"},
	} {
		t.Run(name, func(t *testing.T) {
			in := tc.in
			in.Texts, in.Reason, in.LegalEntityIDs = ledger.Texts{Name: "N", Responsible: "R"}, "r", []uuid.UUID{c.de}
			err := c.migrated(t, func(ctx context.Context, m ledger.CenterMigration) error {
				_, err := m.CreateProfitCenter(ctx, in)
				return err
			})
			if code := problem.From(err).Code; code != tc.want {
				t.Errorf("got %q (%v), want %s", code, err, tc.want)
			}
		})
	}
}

// TestCenterMigrationDatabaseFailures runs each of CenterMigration's calls
// outside a unit of work and with each of its statements failing in turn.
func TestCenterMigrationDatabaseFailures(t *testing.T) {
	c := setupBareCenters(t)
	seg, pc, cc := c.migratedCenters(t)
	m := ledger.CenterMigration{}
	for name, call := range map[string]func(ctx context.Context) error{
		"create segment": func(ctx context.Context) error {
			_, err := m.CreateSegment(ctx, ledger.NewSegment{Code: "S2", Name: "S"})
			return err
		},
		"create profit center": func(ctx context.Context) error {
			_, err := m.CreateProfitCenter(ctx, ledger.MigratedProfitCenter{Code: "P9", Texts: ledger.Texts{Name: "N", Responsible: "R"}, SegmentID: &seg,
				LegalEntityIDs: []uuid.UUID{c.de}, Reason: "r", Periods: []ledger.MigratedPeriod[ledger.ProfitCenterAttrs]{{From: day("2026-01-01")}}})
			return err
		},
		"assign profit center": func(ctx context.Context) error { return m.AssignProfitCenter(ctx, pc, []uuid.UUID{c.us}, "r") },
		"create cost center": func(ctx context.Context) error {
			_, err := m.CreateCostCenter(ctx, ledger.MigratedCostCenter{LegalEntityID: c.de, Code: "C9", Texts: ledger.Texts{Name: "N", Responsible: "R"}, Reason: "r",
				Periods: []ledger.MigratedPeriod[ledger.CostCenterAttrs]{{From: day("2026-01-01"), Attr: ledger.CostCenterAttrs{Category: "ADMIN", ProfitCenterID: pc}}}})
			return err
		},
		"read cost centers":   func(ctx context.Context) error { _, err := m.CostCenters(ctx, []uuid.UUID{cc}); return err },
		"read profit centers": func(ctx context.Context) error { _, err := m.ProfitCenters(ctx, []uuid.UUID{pc}); return err },
		"read segments":       func(ctx context.Context) error { _, err := m.Segments(ctx, []uuid.UUID{seg}); return err },
		"lock profit centers": func(ctx context.Context) error { return m.LockProfitCenters(ctx, []uuid.UUID{pc}) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, c.ten, c.ten.Human, name, call)
		})
	}
}
