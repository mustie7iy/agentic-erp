package payables_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Account assignment of supplier invoice items (T56e, ADR-0050 §6): an
// item without an order gives its cost and profit center, checked as its
// posting will check them; an item on an order takes its order item's,
// and the price difference of an invoice on GR/IR carries it.

type assignedEnv struct {
	env
	c                 fixture.Centers
	other, grir, diff uuid.UUID // accounts 510000 (no default cost center), 219000 and 590000
}

func setupAssignment(t *testing.T) assignedEnv {
	t.Helper()
	f := fixture.New(t, append(perms, "procurement.receipt.post", "procurement.receipt.read")...)
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
	return assignedEnv{env: e, c: e.CostCenters(t, fixture.GRIRAccounts), other: f.Account(t, "2000", "510000"), grir: f.Account(t, "2000", "219000"),
		diff: f.Account(t, "2000", "590000")}
}

func (a assignedEnv) in(t *testing.T) assignedEnv {
	t.Helper()
	a.env = a.env.in(t)
	return a
}

// centeredItem is an untaxed item on the account with the centers given
// (uuid.Nil: none).
func centeredItem(account uuid.UUID, amount string, cc, pc uuid.UUID) string {
	extra := ""
	for field, v := range map[string]uuid.UUID{"cost_center_id": cc, "profit_center_id": pc} {
		if v != uuid.Nil {
			extra += fmt.Sprintf(`, %q: %q`, field, v)
		}
	}
	return strings.Replace(untaxedItem(account, amount), `"description"`, strings.TrimPrefix(extra+`, "description"`, ", "), 1)
}

// assignedLines are an entry's lines as account:cost center:profit center,
// "-" for none, sorted.
func (a assignedEnv) assignedLines(t *testing.T, entry any) []string {
	t.Helper()
	je, _ := a.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", entry), "", "")
	var out []string
	for _, l := range je["lines"].([]any) {
		m := l.(map[string]any)
		s := m["account_id"].(string)
		for _, f := range []string{"cost_center_id", "profit_center_id"} {
			v, ok := m[f].(string)
			if !ok {
				v = "-"
			}
			s += ":" + v
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func assignedLine(account, cc, pc uuid.UUID) string {
	s := account.String()
	for _, id := range []uuid.UUID{cc, pc} {
		if id == uuid.Nil {
			s += ":-"
		} else {
			s += ":" + id.String()
		}
	}
	return s
}

func sortedLines(lines ...string) []string {
	slices.Sort(lines)
	return lines
}

func TestInvoiceItemAssignment(t *testing.T) {
	e := setupAssignment(t)
	none := uuid.Nil
	t.Run("posted with the item's centers", func(t *testing.T) {
		e := e.in(t)
		inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-A1", "6.00", "0.00",
			centeredItem(e.other, "1.00", e.c.Sales, none), centeredItem(e.expense, "2.00", none, none), centeredItem(e.other, "3.00", e.c.Sales, e.c.Profit)), "")
		if it := inv["items"].([]any)[0].(map[string]any); it["cost_center_id"] != e.c.Sales.String() {
			t.Errorf("item 1 cost center: got %v, want %s", it["cost_center_id"], e.c.Sales)
		}
		posted := e.act(t, 200, id(inv), "post", "")
		want := sortedLines(assignedLine(e.other, e.c.Sales, none), assignedLine(e.expense, e.c.Admin, e.c.Profit), assignedLine(e.other, e.c.Sales, e.c.Profit),
			assignedLine(e.ap, none, none))
		if got := e.assignedLines(t, posted["journal_entry_id"]); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("lines: got %v, want %v", got, want)
		}
	})
	t.Run("checked at entry", func(t *testing.T) {
		for name, c := range map[string]struct {
			item, code, field, reason string
		}{
			"a cost center locked for costs": {centeredItem(e.other, "1.00", e.c.Locked, none), "ledger.cost_center_invalid", "items[1].cost_center_id",
				"locked_for_costs"},
			"a primary cost item without a cost center or default": {centeredItem(e.other, "1.00", none, none), "ledger.cost_center_required",
				"items[1].cost_center_id", "required"},
			"another profit center than the cost center's": {centeredItem(e.other, "1.00", e.c.Admin, uuid.New()), "ledger.profit_center_mismatch",
				"items[1].profit_center_id", "mismatch"},
		} {
			t.Run(name, func(t *testing.T) {
				p := e.in(t).Problem(422, c.code, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-A2", "2.00", "0.00",
					centeredItem(e.expense, "1.00", none, none), c.item), "")
				wantErrorField(t, p, c.field, c.reason)
			})
		}
	})
}

// wantErrorField checks that a problem names the field with the code.
func wantErrorField(t *testing.T, p map[string]any, field, code string) {
	t.Helper()
	errs, _ := p["errors"].([]any)
	for _, e := range errs {
		if f := e.(map[string]any); f["field"] == field && f["code"] == code {
			return
		}
	}
	t.Errorf("field errors: got %v, want %s with code %s", errs, field, code)
}

// TestOrderItemAssignment: an invoice item on an order item takes the
// order item's cost center; its receipt and the invoice's price difference
// post with it, the price difference only on a primary cost account.
func TestOrderItemAssignment(t *testing.T) {
	e := setupAssignment(t)
	none := uuid.Nil
	po := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, 'PO-A', $3, $4, 'EUR', '2026-03-01', 'NT30', 'draft', $5, $5)`,
		e.Tenant.ID, po, e.DE, e.Supplier, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id, valuation_price, valuation_price_unit, account_assignment, cost_center_id)
		VALUES ($1, $2, 1, 'Paper', 10, 'EA', 1200, 1, $3, 1200, 1, 'cost_center', $4), ($1, $2, 2, 'Toner', 10, 'EA', 100, 1, $3, 100, 1, 'none', NULL)`,
		e.Tenant.ID, po, e.expense, e.c.Sales)
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', valuation = 'gr_ir', version = version + 1 WHERE id = $1`, po)
	gr, _ := e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-05", "posting_date": "2026-03-05",
		"items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "10", "unit": "EA"}},
			{"purchase_order_id": %q, "po_item_no": 2, "quantity": {"value": "10", "unit": "EA"}}]}`, e.DE, po, po), "")
	t.Run("the receipt", func(t *testing.T) {
		// Item 2, assigned to none, takes the account's default.
		want := sortedLines(assignedLine(e.grir, none, none), assignedLine(e.grir, none, none), assignedLine(e.expense, e.c.Sales, none),
			assignedLine(e.expense, e.c.Admin, e.c.Profit))
		if got := e.in(t).assignedLines(t, gr["journal_entry_id"]); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("WE lines: got %v, want %v", got, want)
		}
	})
	t.Run("an assignment of its own refused", func(t *testing.T) {
		for name, c := range map[string]struct{ cc, pc, field string }{
			"another cost center": {`, "cost_center_id": "` + e.c.Admin.String() + `"`, "", "items[0].cost_center_id"},
			"a profit center":     {"", `, "profit_center_id": "` + e.c.Profit.String() + `"`, "items[0].profit_center_id"},
		} {
			t.Run(name, func(t *testing.T) {
				it := strings.Replace(poItem(po, 1, "1", "EA", "12.00"), `"quantity"`, strings.TrimPrefix(c.cc+c.pc+`, "quantity"`, ", "), 1)
				p := e.in(t).Problem(422, "payables.assignment_from_order", "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-B0", "12.00", "0.00", it), "")
				wantErrorField(t, p, c.field, "invalid")
			})
		}
	})
	// The invoice prices half of item 1 0.06 above the order (within the
	// tolerance): the price difference carries its cost center while the
	// price difference account is a primary cost account.
	invoice := func(t *testing.T, ref string) []string {
		t.Helper()
		inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, ref, "6.06", "0.00", poItem(po, 1, "0.5", "EA", "6.06")), "")
		if it := inv["items"].([]any)[0].(map[string]any); it["cost_center_id"] != e.c.Sales.String() {
			t.Errorf("item cost center: got %v, want the order item's %s", it["cost_center_id"], e.c.Sales)
		}
		if m := e.act(t, 200, id(inv), "match", ""); m["is_clean"] != true {
			t.Fatalf("match: got %v, want clean", m["issues"])
		}
		posted := e.act(t, 200, id(inv), "post", "")
		return e.assignedLines(t, posted["journal_entry_id"])
	}
	t.Run("the price difference on a primary cost account", func(t *testing.T) {
		want := sortedLines(assignedLine(e.grir, none, none), assignedLine(e.diff, e.c.Sales, none), assignedLine(e.ap, none, none))
		if got := invoice(t, "INV-B1"); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("invoice lines: got %v, want %v", got, want)
		}
	})
	// Last: it changes the tenant's price difference account for good.
	t.Run("the price difference on an account taking no cost center", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.gl_account SET cost_element = 'none', version = version + 1 WHERE id = $1`, e.diff)
		want := sortedLines(assignedLine(e.grir, none, none), assignedLine(e.diff, none, none), assignedLine(e.ap, none, none))
		if got := invoice(t, "INV-B2"); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("invoice lines: got %v, want %v", got, want)
		}
	})
}
