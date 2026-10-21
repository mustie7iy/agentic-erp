package ledger_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Controlling rules (T56c, ADR-0050 §4): what the ledger's setup checks
// and keeps of the configuration's cost elements, categories, center
// accounting and default assignments.

// at is a clock standing at the date.
func at(tb testing.TB, date string) func() time.Time {
	tb.Helper()
	t, err := time.Parse(time.DateOnly, date)
	if err != nil {
		tb.Fatal(err)
	}
	return func() time.Time { return t }
}

// controlling is the ledger tests' document with company 2000's cost and
// profit center accounting on, default profit center DUMMY and cost center
// 1000 the default of account 520000, changed by mutate.
func controlling(t *testing.T, mutate func(c *config.Config, co *config.Company)) *config.Config {
	t.Helper()
	c := document(t)
	co := c.Company("2000")
	co.CostCenterAccounting, co.ProfitCenterAccounting, co.DefaultProfitCenter = true, true, "DUMMY"
	co.DefaultAssignments = []config.DefaultAssignment{{Account: "520000", CostCenter: "1000"}}
	if mutate != nil {
		mutate(c, co)
	}
	return c
}

// check is what the ledger's setup finds wrong with the document on the
// date, on top of base.
func (c centers) check(t *testing.T, date string, base, doc *config.Config) []config.Finding {
	t.Helper()
	var out []config.Finding
	if err := apitest.Committed(t, c.ten, c.ten.Human, func(ctx context.Context) error {
		var err error
		out, err = ledger.Setup{LegalEntities: tenancy.Directory{}, Now: at(t, date)}.Check(ctx, base, doc)
		return err
	}); err != nil {
		t.Fatalf("check: %v", err)
	}
	return out
}

// apply applies the document on the date.
func (c centers) apply(t *testing.T, date string, doc *config.Config) error {
	t.Helper()
	return apitest.Committed(t, c.ten, c.ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}, Now: at(t, date)}.Apply(ctx, doc)
	})
}

// wantFinding checks that the findings have one at the path whose message
// says want.
func wantFinding(t *testing.T, fs []config.Finding, path, want string) {
	t.Helper()
	if !slices.ContainsFunc(fs, func(f config.Finding) bool { return f.Path == path && strings.Contains(f.Message, want) }) {
		t.Errorf("findings: got %v, want one at %s saying %q", fs, path, want)
	}
}

// approved creates and approves a cost center of legal entity 2000 and
// returns its path.
func (c centers) approved(t *testing.T, code, extra string) string {
	t.Helper()
	cc := c.costCenter(t, code, extra)
	c.decide(t, cc["pending_change_id"], "approve")
	return "/v1/cost-centers/" + cc["id"].(string)
}

func TestControllingApplied(t *testing.T) {
	c := setupCenters(t)
	c.profitCenter(t, "DUMMY", "", c.de)
	c.approved(t, "1000", "")
	doc := controlling(t, func(_ *config.Config, co *config.Company) { co.Account("530000").CostElement = config.CostElementNone })
	if fs := c.check(t, "2026-05-01", document(t), doc); len(fs) != 0 {
		t.Fatalf("check: got %v, want no findings", fs)
	}
	if err := c.apply(t, "2026-05-01", doc); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Run("cost elements", func(t *testing.T) {
		for number, want := range map[string]string{"520000": "primary_cost", "530000": "none", "400000": "revenue", "100000": "none"} {
			var got string
			if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT g.cost_element FROM ledger.gl_account g
				JOIN ledger.chart_of_accounts c ON c.id = g.chart_id WHERE c.tenant_id = $1 AND c.code = '2000' AND g.number = $2`,
				c.ten.ID, number).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("account %s: got %s, want %s", number, got, want)
			}
		}
	})
	t.Run("center accounting from 2026 with the defaults", func(t *testing.T) {
		var got string
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT k.cost_center_accounting_from || '/' || k.profit_center_accounting_from || '/' ||
				p.code || '/' || (SELECT string_agg(g.number || '=' || cc.code, ',') FROM ledger.default_assignment d
					JOIN ledger.gl_account g ON g.id = d.account_id JOIN ledger.cost_center cc ON cc.id = d.cost_center_id
					WHERE d.legal_entity_id = k.legal_entity_id AND d.is_active)
			FROM ledger.company_controlling k JOIN ledger.profit_center p ON p.id = k.default_profit_center_id WHERE k.legal_entity_id = $1`,
			c.de).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want := "2026/2026/DUMMY/520000=1000"; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})
	t.Run("a default removed is deactivated, a year kept", func(t *testing.T) {
		later := controlling(t, func(_ *config.Config, co *config.Company) { co.DefaultAssignments = nil })
		if err := c.apply(t, "2027-02-01", later); err != nil {
			t.Fatalf("apply: %v", err)
		}
		var active, from int
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT (SELECT count(*) FROM ledger.default_assignment WHERE legal_entity_id = $1 AND is_active),
			(SELECT cost_center_accounting_from FROM ledger.company_controlling WHERE legal_entity_id = $1)`, c.de).Scan(&active, &from); err != nil {
			t.Fatal(err)
		}
		if active != 0 || from != 2026 {
			t.Errorf("got %d active defaults from %d, want 0 from 2026", active, from)
		}
		if err := c.apply(t, "2027-02-01", doc); err != nil {
			t.Fatalf("apply again: %v", err)
		}
	})
	t.Run("turned off", func(t *testing.T) {
		off := controlling(t, func(_ *config.Config, co *config.Company) {
			co.CostCenterAccounting, co.ProfitCenterAccounting, co.DefaultProfitCenter = false, false, ""
		})
		fs := c.check(t, "2027-02-01", doc, off)
		wantFinding(t, fs, "companies[2000].cost_center_accounting", "is on since fiscal year 2026 and stays on")
		wantFinding(t, fs, "companies[2000].profit_center_accounting", "is on since fiscal year 2026 and stays on")
		if p := problem.From(c.apply(t, "2027-02-01", off)); p.Code != "ledger.controlling_invalid" {
			t.Errorf("apply: got %v, want ledger.controlling_invalid", p)
		}
	})
}

// A default center must be usable by every automatic posting from today
// on: an active center of the company, valid from today with no end and
// never locked from today.
func TestControllingDefaults(t *testing.T) {
	c := setupCenters(t)
	c.profitCenter(t, "DUMMY", "", c.de)
	c.profitCenter(t, "USONLY", "", c.us)
	c.profitCenter(t, "ENDS", "2026-12-01", c.de)
	c.approved(t, "1000", "")
	c.approved(t, "ENDS", `, "valid_to": "2026-12-01"`)
	later, _ := c.Must(201, "POST", "/v1/cost-centers", c.ccBody(fmt.Sprintf(`{"legal_entity_id": %q, "code": "LATER", "name": "L",
		"responsible": "J", "category": "ADMIN", "valid_from": "2026-06-01", "reason": "r"}`, c.de)), "")
	c.decide(t, later["pending_change_id"], "approve")
	locked := c.approved(t, "LOCKED", "")
	_, tag := c.Must(200, "GET", locked, "", "")
	ch, _ := c.Must(200, "POST", locked+":lock", `{"valid_from": "2026-09-01", "lock_actual_cost": true, "reason": "Winding down"}`, tag)
	c.decide(t, ch["id"], "approve")
	const p = "companies[2000]"
	for name, tc := range map[string]struct {
		mutate     func(c *config.Config, co *config.Company)
		path, want string
	}{
		"an unknown cost center": {func(_ *config.Config, co *config.Company) { co.DefaultAssignments[0].CostCenter = "NOPE" },
			p + ".default_assignments[520000].cost_center", "NOPE is not an active cost center of the company"},
		"a cost center ending": {func(_ *config.Config, co *config.Company) { co.DefaultAssignments[0].CostCenter = "ENDS" },
			p + ".default_assignments[520000].cost_center", "ENDS is not valid from today with no end date"},
		"a cost center starting later": {func(_ *config.Config, co *config.Company) { co.DefaultAssignments[0].CostCenter = "LATER" },
			p + ".default_assignments[520000].cost_center", "LATER is not valid from today"},
		"a cost center locked later": {func(_ *config.Config, co *config.Company) { co.DefaultAssignments[0].CostCenter = "LOCKED" },
			p + ".default_assignments[520000].cost_center", "LOCKED is locked from 2026-09-01"},
		"an unknown profit center": {func(_ *config.Config, co *config.Company) { co.DefaultProfitCenter = "NOPE" },
			p + ".default_profit_center", "NOPE is not an active profit center"},
		"a profit center of another company": {func(_ *config.Config, co *config.Company) { co.DefaultProfitCenter = "USONLY" },
			p + ".default_profit_center", "USONLY is not assigned to the company"},
		"a profit center ending": {func(_ *config.Config, co *config.Company) { co.DefaultProfitCenter = "ENDS" },
			p + ".default_profit_center", "ENDS is not valid from today with no end date"},
	} {
		t.Run(name, func(t *testing.T) {
			doc := controlling(t, tc.mutate)
			wantFinding(t, c.in(t).check(t, "2026-05-01", nil, doc), tc.path, tc.want)
			if p := problem.From(c.in(t).apply(t, "2026-05-01", doc)); p.Code != "ledger.controlling_invalid" || !strings.Contains(p.Detail, tc.want) {
				t.Errorf("apply: got %v, want ledger.controlling_invalid saying %q", p, tc.want)
			}
		})
	}
	t.Run("a locked cost center from the day after", func(t *testing.T) {
		doc := controlling(t, func(_ *config.Config, co *config.Company) { co.DefaultAssignments[0].CostCenter = "LOCKED" })
		wantFinding(t, c.in(t).check(t, "2026-10-01", nil, doc), p+".default_assignments[520000].cost_center", "LOCKED is locked from 2026-10-01")
	})
}

// Turning center accounting on is allowed only while the company has no
// postings in the current fiscal year or later.
func TestControllingTurnedOnBeforeTheYearsPostings(t *testing.T) {
	c := setupCenters(t)
	c.profitCenter(t, "DUMMY", "", c.de)
	c.approved(t, "1000", "")
	bank, revenue := c.account(t, "2000", "100000"), c.account(t, "2000", "400000")
	c.Must(201, "POST", "/v1/journal-entries", entry(c.de, "2026-03-15", "EUR", line{bank, "100.00", ""}, line{revenue, "-100.00", ""}), "")
	doc := controlling(t, nil)
	fs := c.check(t, "2026-05-01", document(t), doc)
	wantFinding(t, fs, "companies[2000].cost_center_accounting", "has postings in fiscal year 2026 or later")
	wantFinding(t, fs, "companies[2000].profit_center_accounting", "has postings in fiscal year 2026 or later")
	if p := problem.From(c.apply(t, "2026-05-01", doc)); p.Code != "ledger.controlling_invalid" {
		t.Errorf("apply in 2026: got %v, want ledger.controlling_invalid", p)
	}
	t.Run("from the next fiscal year", func(t *testing.T) {
		if fs := c.in(t).check(t, "2027-01-04", document(t), doc); len(fs) != 0 {
			t.Fatalf("check in 2027: got %v, want none", fs)
		}
		if err := c.in(t).apply(t, "2027-01-04", doc); err != nil {
			t.Fatalf("apply in 2027: %v", err)
		}
		var from int
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT cost_center_accounting_from FROM ledger.company_controlling WHERE legal_entity_id = $1`,
			c.de).Scan(&from); err != nil {
			t.Fatal(err)
		}
		if from != 2027 {
			t.Errorf("cost center accounting from: got %d, want 2027", from)
		}
	})
}

// Categories come from the configuration: a cost center takes only an
// active one, starts with its locks, and a category in use stays.
func TestCostCenterCategories(t *testing.T) {
	c := setupCenters(t)
	c.Problem(422, "ledger.cost_center_category_invalid", "POST", "/v1/cost-centers", c.ccBody(fmt.Sprintf(`{"legal_entity_id": %q, "code": "P1",
		"name": "P", "responsible": "J", "category": "PROD", "valid_from": "2026-01-01", "reason": "r"}`, c.de)), "")
	doc := document(t)
	doc.Controlling.CostCenterCategories = append(doc.Controlling.CostCenterCategories,
		config.CostCenterCategory{Code: "PROD", Name: "Production", LockActualRevenue: true, LockPlan: true})
	if err := c.apply(t, "2026-05-01", doc); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cc, _ := c.Must(201, "POST", "/v1/cost-centers", c.ccBody(fmt.Sprintf(`{"legal_entity_id": %q, "code": "P1", "name": "P", "responsible": "J",
		"category": "PROD", "valid_from": "2026-01-01", "reason": "r"}`, c.de)), "")
	c.decide(t, cc["pending_change_id"], "approve")
	got, tag := c.Must(200, "GET", "/v1/cost-centers/"+cc["id"].(string), "", "")
	wantPeriods(t, "locks of a PROD cost center", periods(t, got, "lock_actual_revenue"), "2026-01-01..<nil> true")
	wantPeriods(t, "plan lock of a PROD cost center", periods(t, got, "lock_plan"), "2026-01-01..<nil> true")
	t.Run("a category in use stays", func(t *testing.T) {
		without := document(t)
		fs := c.in(t).check(t, "2026-05-01", doc, without)
		wantFinding(t, fs, "controlling.cost_center_categories", "category PROD is the category of cost centers P1")
		if p := problem.From(c.in(t).apply(t, "2026-05-01", without)); p.Code != "ledger.controlling_invalid" {
			t.Errorf("apply: got %v, want ledger.controlling_invalid", p)
		}
	})
	t.Run("a removed category is no longer given", func(t *testing.T) {
		c := c.in(t)
		without := document(t)
		without.Controlling.CostCenterCategories = without.Controlling.CostCenterCategories[:1] // ADMIN; SALES removed
		without.Controlling.CostCenterCategories = append(without.Controlling.CostCenterCategories, config.CostCenterCategory{Code: "PROD", Name: "Prod"})
		if err := c.apply(t, "2026-05-01", without); err != nil {
			t.Fatalf("apply: %v", err)
		}
		c.Problem(422, "ledger.cost_center_category_invalid", "POST", "/v1/cost-centers/"+cc["id"].(string)+":move",
			`{"valid_from": "2026-06-01", "category": "SALES", "reason": "r"}`, tag)
		var name string
		var active bool
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT name, is_active FROM ledger.cost_center_category WHERE tenant_id = $1 AND code = 'PROD'`,
			c.ten.ID).Scan(&name, &active); err != nil {
			t.Fatal(err)
		}
		if name != "Prod" || !active {
			t.Errorf("PROD: got %s active %v, want Prod active", name, active)
		}
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT is_active FROM ledger.cost_center_category WHERE tenant_id = $1 AND code = 'SALES'`,
			c.ten.ID).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active {
			t.Error("SALES: got active, want deactivated")
		}
	})
}

// A default center stays usable: a change that would end its validity or
// lock it is refused while a company defaults to it.
func TestDefaultCentersStayUsable(t *testing.T) {
	c := setupCenters(t)
	pc := c.profitCenter(t, "DUMMY", "", c.de)
	path := c.approved(t, "1000", "")
	if err := c.apply(t, "2026-05-01", controlling(t, nil)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	_, tag := c.Must(200, "GET", path, "", "")
	c.Problem(409, "ledger.default_center_in_use", "POST", path+":lock", `{"valid_from": "2027-01-01", "lock_actual_cost": true, "reason": "r"}`, tag)
	c.Problem(409, "ledger.default_center_in_use", "POST", path+":redate", `{"valid_to": "2030-01-01", "reason": "r"}`, tag)
	c.Must(200, "POST", path+":lock", `{"valid_from": "2027-01-01", "lock_plan": true, "reason": "Plan frozen"}`, tag)
	_, tag = c.Must(200, "GET", pc, "", "")
	c.Problem(409, "ledger.default_center_in_use", "POST", pc+":lock", `{"valid_from": "2027-01-01", "locked": true, "reason": "r"}`, tag)
	c.Problem(409, "ledger.default_center_in_use", "POST", pc+":redate", `{"valid_to": "2030-01-01", "reason": "r"}`, tag)
}

// TestControllingDatabaseFailures checks and applies controlling rules
// outside a unit of work and with each database statement failing in turn.
func TestControllingDatabaseFailures(t *testing.T) {
	c := setupCenters(t)
	pc := uuid.MustParse(strings.TrimPrefix(c.profitCenter(t, "DUMMY", "", c.de), "/v1/profit-centers/"))
	c.approved(t, "1000", "")
	setup := ledger.Setup{LegalEntities: tenancy.Directory{}, Now: at(t, "2026-05-01")}
	doc := controlling(t, nil)
	h := endpoints()
	for name, call := range map[string]func(ctx context.Context) error{
		"lock the profit center": func(ctx context.Context) error {
			_, err := h.LockProfitCenter(ctx, &gen.ProfitCenterLock{ValidFrom: date("2026-06-01"), Locked: true, Reason: "r"},
				gen.LockProfitCenterParams{ID: pc, IfMatch: "*"})
			return err
		},
		"check": func(ctx context.Context) error {
			_, err := setup.Check(ctx, document(t), doc)
			return err
		},
		"apply": func(ctx context.Context) error { return setup.Apply(ctx, doc) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, c.ten, c.ten.Human, name, call)
		})
	}
}

// TestCategoryRemovedWhileAMoveIsApproved: an activation removing a
// category waits for a move into it being approved, then sees the period
// it wrote and is refused, so no active period has an inactive category.
func TestCategoryRemovedWhileAMoveIsApproved(t *testing.T) {
	c := setupCenters(t)
	path := c.approved(t, "1000", "")
	_, tag := c.Must(200, "GET", path, "", "")
	move, _ := c.Must(200, "POST", path+":move", `{"valid_from": "2026-06-01", "category": "SALES", "reason": "r"}`, tag)
	without := document(t)
	without.Controlling.CostCenterCategories = without.Controlling.CostCenterCategories[:1] // ADMIN
	approve := func(ctx context.Context) error {
		return ledger.CenterChanges{}.Approved(ctx, uuid.MustParse(move["id"].(string)), uuid.MustParse(move["workflow_request_id"].(string)))
	}
	activate := func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}, Now: at(t, "2026-05-01")}.Apply(ctx, without)
	}
	first, second := serializeOn(t, c.ten, "the category", rowLockWaits(t), approve, activate)
	if first != nil {
		t.Fatalf("move: got %v, want it committed", first)
	}
	if got := problem.From(second); got.Code != "ledger.controlling_invalid" || !strings.Contains(got.Detail, "category SALES") {
		t.Errorf("activation: got %v, want ledger.controlling_invalid naming SALES", second)
	}
}
