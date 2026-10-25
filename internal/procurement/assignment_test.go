package procurement_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Account assignment of requisition and order items (T56e, ADR-0050 §6):
// an item names its assignment and cost center, checked against the
// ledger as the document is released, and its goods receipts post the
// item's account with its cost center.

// assignedEnv is a tenant whose company 2000 has cost centers and cost
// center accounting on, with GR/IR accounts.
type assignedEnv struct {
	env
	c fixture.Centers
}

func setupAssignment(t *testing.T) assignedEnv {
	t.Helper()
	e := setup(t)
	return assignedEnv{env: e, c: e.CostCenters(t, fixture.GRIRAccounts)}
}

// centered is an item body with a cost center (uuid.Nil: none) and the
// extra fields.
func centered(account, price string, cc uuid.UUID, extra string) string {
	if cc != uuid.Nil {
		extra = fmt.Sprintf(`, "cost_center_id": %q`, cc) + extra
	}
	return item(account, "10", "EA", price, "EUR", extra)
}

// wantField checks that a problem names the field with the code.
func wantField(t *testing.T, p map[string]any, field, code string) {
	t.Helper()
	errs, _ := p["errors"].([]any)
	for _, e := range errs {
		if f := e.(map[string]any); f["field"] == field && f["code"] == code {
			return
		}
	}
	t.Errorf("field errors: got %v, want %s with code %s", errs, field, code)
}

func TestItemAssignment(t *testing.T) {
	e := setupAssignment(t)
	t.Run("entered", func(t *testing.T) {
		e := e.in(t)
		r := e.requisition(t, items(centered(e.expense, "1.00", e.c.Admin, ""), centered(e.office, "1.00", uuid.Nil, "")), "")
		for i, want := range []string{"cost_center " + e.c.Admin.String(), "none <nil>"} {
			it := r["items"].([]any)[i].(map[string]any)
			if got := fmt.Sprintf("%v %v", it["account_assignment"], it["cost_center_id"]); got != want {
				t.Errorf("requisition item %d: got %s, want %s", i+1, got, want)
			}
		}
		o := e.order(t, items(centered(e.expense, "1.00", e.c.Admin, `, "account_assignment": "cost_center"`)), "")
		if it := o["items"].([]any)[0].(map[string]any); it["account_assignment"] != "cost_center" || it["cost_center_id"] != e.c.Admin.String() {
			t.Errorf("order item: got %v %v, want cost_center %s", it["account_assignment"], it["cost_center_id"], e.c.Admin)
		}
	})
	t.Run("assignment and cost center agree", func(t *testing.T) {
		e := e.in(t)
		for name, it := range map[string]string{
			"a cost center assignment without one": item(e.expense, "1", "EA", "1.00", "EUR", `, "account_assignment": "cost_center"`),
			"none with a cost center":              centered(e.expense, "1.00", e.c.Admin, `, "account_assignment": "none"`),
		} {
			t.Run(name, func(t *testing.T) {
				p := e.in(t).Problem(422, "procurement.invalid_account_assignment", "POST", "/v1/requisitions",
					fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": %s}`, e.DE, items(it)), "")
				wantField(t, p, "items[0].account_assignment", "invalid")
				e.in(t).Problem(422, "procurement.invalid_account_assignment", "POST", "/v1/purchase-orders",
					fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": "2026-03-10", "items": %s}`, e.DE, e.Supplier, items(it)), "")
			})
		}
	})
	t.Run("checked at submission and release", func(t *testing.T) {
		for name, c := range map[string]struct {
			cc          uuid.UUID
			account     string
			code, field string
		}{
			// The account's default (520000: 1000) is not taken for an item.
			"a primary cost item without a cost center": {uuid.Nil, e.office, "ledger.cost_center_required", "required"},
			"a cost center locked for costs":            {e.c.Locked, e.expense, "ledger.cost_center_invalid", "locked_for_costs"},
		} {
			t.Run(name, func(t *testing.T) {
				e := e.in(t)
				its := items(centered(e.expense, "1.00", e.c.Admin, ""), centered(c.account, "1.00", c.cc, ""))
				r := e.requisition(t, its, "")
				path := "/v1/requisitions/" + id(r)
				_, tag := e.Must(200, "GET", path, "", "")
				p := e.Problem(422, c.code, "POST", path+":submit", "", tag)
				wantField(t, p, "items[1].cost_center_id", c.field)
				o := e.order(t, its, "")
				path = "/v1/purchase-orders/" + id(o)
				_, tag = e.Must(200, "GET", path, "", "")
				p = e.Problem(422, c.code, "POST", path+":release", "", tag)
				wantField(t, p, "items[1].cost_center_id", c.field)
				if d, _ := p["detail"].(string); len(d) < 6 || d[:6] != "Item 2" {
					t.Errorf("detail: got %q, want it to name item 2", d)
				}
			})
		}
		t.Run("submitted and released", func(t *testing.T) {
			e := e.in(t)
			r := e.requisition(t, items(centered(e.expense, "1.00", e.c.Admin, "")), "")
			if got := e.act(t, "/v1/requisitions/"+id(r), "submit", ""); got["status"] == "draft" {
				t.Errorf("requisition: got %v, want submitted", got["status"])
			}
			o := e.order(t, items(centered(e.expense, "1.00", e.c.Admin, "")), "")
			if got := e.act(t, "/v1/purchase-orders/"+id(o), "release", ""); got["status"] != "released" {
				t.Errorf("order: got %v, want released", got["status"])
			}
		})
	})
	t.Run("receipts post the item's cost center", func(t *testing.T) {
		e := e.in(t)
		o := e.order(t, items(centered(e.expense, "5.00", e.c.Admin, ""), centered(e.expense, "1.00", e.c.Sales, "")), "")
		e.act(t, "/v1/purchase-orders/"+id(o), "release", "")
		code, gr := e.receipt(t, id(o), "1:2:EA", "2:3:EA")
		if code != 201 {
			t.Fatalf("receipt: got %d %v, want 201", code, gr)
		}
		// Each item's GR/IR line without centers, its counter line with the
		// item's cost center and the cost center's profit center.
		grir := e.Account(t, "2000", "219000").String()
		want := []string{grir + ":-:-", grir + ":-:-", e.expense + ":" + e.c.Admin.String() + ":" + e.c.Profit.String(), e.expense + ":" + e.c.Sales.String() + ":-"}
		if got := centersOn(t, gr["journal_entry_id"]); fmt.Sprint(got) != fmt.Sprint(sorted(want)) {
			t.Errorf("WE lines: got %v, want %v", got, sorted(want))
		}
	})
}

// TestItemAssignmentChanges: an order change keeps a released item's
// assignment and checks the items it adds; the approvals of a release and
// of a change check the assignment again.
func TestItemAssignmentChanges(t *testing.T) {
	e := setupAssignment(t)
	cc, _ := e.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": "4000", "name": "CC 4000", "responsible": "J",
		"category": "ADMIN", "valid_from": "2026-01-01", "reason": "r"}`, e.DE), "")
	e.Approve(t, cc["pending_change_id"].(string))
	later := uuid.MustParse(id(cc))
	small := e.order(t, items(centered(e.expense, "5.00", e.c.Admin, "")), "")
	path := "/v1/purchase-orders/" + id(small)
	e.act(t, path, "release", "")
	t.Run("a released item keeps its assignment", func(t *testing.T) {
		e := e.in(t)
		for name, it := range map[string]string{
			"another cost center": centered(e.expense, "5.00", e.c.Sales, ""),
			"none":                centered(e.expense, "5.00", uuid.Nil, ""),
		} {
			t.Run(name, func(t *testing.T) {
				e.in(t).Problem(422, "procurement.invalid_change", "POST", path+"/changes", `{"reason": "r", "items": `+items(it)+`}`, "")
			})
		}
	})
	t.Run("an added item is checked at submission", func(t *testing.T) {
		e := e.in(t)
		cpath, tag := e.draftChange(t, path, items(centered(e.expense, "5.00", e.c.Admin, ""), centered(e.expense, "1.00", e.c.Locked, "")))
		p := e.Problem(422, "ledger.cost_center_invalid", "POST", cpath+":submit", "", tag)
		wantField(t, p, "items[1].cost_center_id", "locked_for_costs")
		_, tag = e.Must(200, "GET", cpath, "", "")
		e.Must(200, "POST", cpath+":cancel", "", tag)
	})
	// A release and a change waiting for approval, on a cost center locked
	// before they are approved.
	big := e.order(t, items(centered(e.expense, "150.00", later, "")), "")
	if got := e.act(t, "/v1/purchase-orders/"+id(big), "release", ""); got["status"] != "pending_approval" {
		t.Fatalf("release: got %v, want pending_approval", got["status"])
	}
	change := e.submitted(t, path, items(centered(e.expense, "5.00", e.c.Admin, ""), centered(e.expense, "150.00", later, "")))
	if change["status"] != "pending_approval" {
		t.Fatalf("change: got %v, want pending_approval", change["status"])
	}
	lpath := "/v1/cost-centers/" + later.String()
	_, tag := e.Must(200, "GET", lpath, "", "")
	lock, _ := e.Must(200, "POST", lpath+":lock", `{"valid_from": "2026-01-01", "lock_actual_cost": true, "reason": "r"}`, tag)
	e.Approve(t, id(lock))
	for name, object := range map[string]string{"the release": id(big), "the change": id(change)} {
		t.Run(name+" is checked at approval", func(t *testing.T) {
			r := e.in(t).Pending(t, object)
			e.As(e.Approver).Problem(422, "ledger.cost_center_invalid", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve",
				`{"comment": "ok"}`, fmt.Sprintf(`"%v"`, r["version"]))
		})
	}
}

func (a assignedEnv) in(t *testing.T) assignedEnv {
	t.Helper()
	a.env = a.env.in(t)
	return a
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// centersOn are the lines of the entry as account:cost center:profit
// center, sorted, "-" for none.
func centersOn(t *testing.T, entry any) []string {
	t.Helper()
	rows, err := pgtest.Admin(t).Query(context.Background(), `SELECT account_id || ':' || coalesce(cost_center_id::text, '-') || ':' ||
			coalesce(profit_center_id::text, '-') FROM ledger.journal_line WHERE journal_entry_id = $1 ORDER BY 1`, entry)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}
