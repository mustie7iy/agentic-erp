package payables_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// grirOrder inserts a released order valued through GR/IR, with e.order's
// items: item 1 is 10 EA at 12.00 with tax code V1, item 2 is 5 KG at 3.00.
func (e env) grirOrder(t *testing.T, number string) uuid.UUID {
	t.Helper()
	return e.grirOrderOn(t, number, "NT30")
}

// grirOrderOn is grirOrder on the payment term: a released order's term
// changes only through an order change (T47e), so it is set while draft.
func (e env) grirOrderOn(t *testing.T, number, term string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, 'EUR', '2026-03-01', $7, 'draft', $6, $6)`,
		e.Tenant.ID, id, number, e.DE, e.Supplier, e.Tenant.Human.ID, term)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id, tax_code, valuation_price, valuation_price_unit)
		VALUES ($1, $2, 1, 'Paper', 10, 'EA', 1200, 1, $3, 'V1', 1200, 1), ($1, $2, 2, 'Toner', 5, 'KG', 300, 1, $3, NULL, 300, 1)`,
		e.Tenant.ID, id, e.expense)
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', valuation = 'gr_ir', version = version + 1 WHERE id = $1`, id)
	return id
}

// line is a posted line: account, amount and order item.
type line struct {
	account string
	amount  string
	order   any
}

func (e env) entryLines(t *testing.T, entry any) []line {
	t.Helper()
	je, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", entry), "", "")
	var out []line
	for _, l := range je["lines"].([]any) {
		m := l.(map[string]any)
		out = append(out, line{m["account_id"].(string), m["amount"].(map[string]any)["amount"].(string), m["purchase_order_id"]})
	}
	return out
}

// TestInvoiceClearsGRIR: an invoice on an order valued through GR/IR
// debits GR/IR at the order price of the quantity it invoices, each item
// naming its order item, puts what it differs from the order price on the
// price difference account, and posts an item without an order item on
// its own account; with the receipt it leaves the order's GR/IR position
// at zero, and its reversal restores what was received.
func TestInvoiceClearsGRIR(t *testing.T) {
	f := fixture.New(t, append(perms, "procurement.receipt.post", "procurement.receipt.read")...)
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
	e.GRIR(t)
	grir, diff, fees := e.Account(t, "2000", "219000").String(), e.Account(t, "2000", "590000").String(), e.Account(t, "2000", "530000")
	po := e.grirOrder(t, "PO-1")
	e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-05", "posting_date": "2026-03-05",
		"items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "10", "unit": "EA"}},
			{"purchase_order_id": %q, "po_item_no": 2, "quantity": {"value": "5", "unit": "KG"}}]}`, e.DE, po, po), "")
	position := func() string {
		var balance string
		if err := pgtest.Admin(t).QueryRow(t.Context(), `SELECT coalesce(sum(amount_tc), 0)::text FROM ledger.journal_line WHERE purchase_order_id = $1`, po).
			Scan(&balance); err != nil {
			t.Fatal(err)
		}
		return balance
	}
	if got := position(); got != "-13500" {
		t.Fatalf("position after the receipt: got %s, want -13500", got)
	}
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "164.35", "23.75",
		poItem(po, 1, "10", "EA", "120.00"), poItem(po, 2, "5", "KG", "15.60"), accountItem(fees, "5.00")), "")
	if m := e.act(t, 200, id(inv), "match", ""); m["is_clean"] != true {
		t.Fatalf("match: got %v, want clean", m["issues"])
	}
	h := endpoints(nil)
	t.Run("database failures", func(t *testing.T) {
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "post on GR/IR", func(ctx context.Context) error {
			_, err := h.PostSupplierInvoice(ctx, gen.OptSupplierInvoicePost{}, gen.PostSupplierInvoiceParams{ID: uuid.MustParse(id(inv)), IfMatch: "*"})
			return err
		})
	})
	posted := e.act(t, 200, id(inv), "post", `{"posting_date": "2026-03-15"}`)
	got := e.entryLines(t, posted["journal_entry_id"])
	want := []line{{grir, "120.00", po.String()}, {grir, "15.00", po.String()}, {diff, "0.60", nil}, {fees.String(), "5.00", nil},
		{e.tax.String(), "23.75", nil}, {e.ap.String(), "-164.35", nil}}
	if fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("invoice lines: got %v, want %v", got, want)
	}
	if got := position(); got != "0" {
		t.Errorf("position after the invoice: got %s, want 0", got)
	}
	t.Run("tax items", func(t *testing.T) {
		// Item 1's GR/IR line and the fees are V1's base (item 2, whose
		// price difference this is, has no code), at the invoice date, the
		// tax date (T61c).
		var items string
		if err := pgtest.Admin(t).QueryRow(t.Context(), `SELECT string_agg(code || ' ' || base_tc || ' ' || tax_tc || ' ' || tax_date, '; ')
			FROM ledger.tax_item WHERE journal_entry_id = $1`, posted["journal_entry_id"]).Scan(&items); err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("V1 12500 2375 %v", inv["invoice_date"]); items != want {
			t.Errorf("items: got %s, want %s", items, want)
		}
	})
	e.act(t, 200, id(inv), "reverse", `{"reason": "wrong"}`)
	if got := position(); got != "-13500" {
		t.Errorf("position after the reversal: got %s, want -13500", got)
	}
}

// sorted orders lines by account and amount, as an entry's order of lines
// is the posting's, not part of the rule.
func sorted(lines []line) []line {
	return slices.SortedFunc(slices.Values(lines), func(a, b line) int {
		return cmp.Or(strings.Compare(a.account, b.account), strings.Compare(a.amount, b.amount))
	})
}

// TestForeignInvoiceClearsGRIR: an invoice in USD of a EUR company clears
// the receipt's GR/IR at the local value it was received at; the price
// difference takes the invoice's rate, and what the GR/IR line leaves is
// an exchange difference. The reversal restores the receipt's position.
func TestForeignInvoiceClearsGRIR(t *testing.T) {
	f := fixture.New(t, append(perms, "procurement.receipt.post", "procurement.receipt.read", "ledger.rate.maintain")...)
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
	e.GRIR(t)
	for _, r := range []struct{ from, rate string }{{"2026-03-01", "0.9"}, {"2026-03-12", "0.92"}} {
		e.Must(201, "POST", "/v1/exchange-rates", fmt.Sprintf(`{"rate_type": "average", "from_currency": "USD", "to_currency": "EUR", "valid_from": %q,
			"rate": %q, "source": "ECB"}`, r.from, r.rate), "")
	}
	po := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, 'PO-USD', $3, $4, 'USD', '2026-03-01', 'NT30', 'draft', $5, $5)`,
		e.Tenant.ID, po, e.DE, e.Supplier, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id, valuation_price, valuation_price_unit) VALUES ($1, $2, 1, 'Paper', 10, 'EA', 1200, 1, $3, 1200, 1)`, e.Tenant.ID, po, e.expense)
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', valuation = 'gr_ir', version = version + 1 WHERE id = $1`, po)
	e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-05", "posting_date": "2026-03-05",
		"items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "10", "unit": "EA"}}]}`, e.DE, po), "")
	position := func() [2]int64 {
		var p [2]int64
		if err := pgtest.Admin(t).QueryRow(t.Context(), `SELECT coalesce(sum(amount_tc), 0)::bigint, coalesce(sum(amount_lc), 0)::bigint
			FROM ledger.journal_line WHERE purchase_order_id = $1`, po).Scan(&p[0], &p[1]); err != nil {
			t.Fatal(err)
		}
		return p
	}
	inv := e.foreignInvoice(t, po, "USD", "INV-USD", "125.00", "0.00", "125.00")
	if m := e.act(t, 200, inv, "match", ""); m["is_clean"] != true {
		t.Fatalf("match: got %v, want clean", m["issues"])
	}
	posted := e.act(t, 200, inv, "post", `{"posting_date": "2026-03-15"}`)
	je, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", posted["journal_entry_id"]), "", "")
	local := map[string]money.Minor{} // per account: the exchange difference has a local and a group line
	for _, l := range je["lines"].([]any) {
		m := l.(map[string]any)
		amount, err := money.ParseAmount(m["amount_lc"].(map[string]any)["amount"].(string), 2)
		if err != nil {
			t.Fatal(err)
		}
		local[m["account_id"].(string)] += amount
	}
	for account, want := range map[string]money.Minor{e.Account(t, "2000", "219000").String(): 10800, e.Account(t, "2000", "590000").String(): 460,
		e.ap.String(): -11500, e.Account(t, "2000", "690000").String(): 240} {
		if local[account] != want {
			t.Errorf("local amount on %s: got %d, want %d (lines %v)", account, local[account], want, local)
		}
	}
	if got := position(); got != [2]int64{} {
		t.Errorf("position after the invoice: got %v, want zero", got)
	}
	e.act(t, 200, inv, "reverse", `{"reason": "wrong"}`)
	if got := position(); got != [2]int64{-12000, -10800} {
		t.Errorf("position after the reversal: got %v, want the receipt's -120.00 USD, -108.00 EUR", got)
	}
}

// TestInvoiceOnARemovedOrderItem: an invoice item whose order item the
// order no longer has cannot clear GR/IR, so the invoice is refused rather
// than posted on another account: matched again at :post, its clean match
// is outdated, the item missing, and an exception approved on that is
// refused as it posts.
func TestInvoiceOnARemovedOrderItem(t *testing.T) {
	e := setup(t)
	e.GRIR(t)
	po := e.grirOrder(t, "PO-1")
	e.receive(t, po, 2, "5")
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "15.00", "0.00", poItem(po, 2, "5", "KG", "15.00")), "")
	if m := e.act(t, 200, id(inv), "match", ""); m["is_clean"] != true {
		t.Fatalf("match: got %v, want clean", m["issues"])
	}
	// Order items of a released order do not change (a trigger refuses it);
	// the fixture removes one behind procurement's back, as superuser.
	if err := pgx.BeginFunc(context.Background(), pgtest.Admin(t), func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `UPDATE procurement.purchase_order_item SET is_removed = true WHERE purchase_order_id = $1 AND item_no = 2`, po)
		return err
	}); err != nil {
		t.Fatalf("remove the order item: %v", err)
	}
	m := e.Problem(409, "payables.match_outdated", "POST", "/v1/supplier-invoices/"+id(inv)+":post", "", e.tag(t, id(inv)))
	if detail, _ := m["detail"].(string); !strings.Contains(detail, "order item 2 is not on purchase order PO-1") {
		t.Errorf("refusal: got %q, want it to name the missing order item", detail)
	}
	// The same issue as fields, for a client's own words (ADR-0052 D3).
	issues, err := json.Marshal(m["match_issues"])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(issues), `[{"code":"order_item_missing","item_no":1,"message":"order item 2 is not on purchase order PO-1"}]`; got != want {
		t.Errorf("match_issues:\ngot  %s\nwant %s", got, want)
	}
	// Matched again it is held; an exception approved on the missing item
	// still does not let it clear GR/IR on another.
	e.act(t, 200, id(inv), "match", "")
	e.Approve(t, id(inv))
	e.Problem(422, "payables.order_item_missing", "POST", "/v1/supplier-invoices/"+id(inv)+":post", "", e.tag(t, id(inv)))
}

// TestInvoiceAtTheValuationPrice: an invoice clears GR/IR at the price the
// order item was released at, leaving what it does not invoice at that
// price; a price changed since puts its difference on the price difference
// account (T47a).
func TestInvoiceAtTheValuationPrice(t *testing.T) {
	f := fixture.New(t, append(perms, "procurement.receipt.post", "procurement.receipt.read")...)
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
	e.GRIR(t)
	grir, diff := e.Account(t, "2000", "219000").String(), e.Account(t, "2000", "590000").String()
	po := e.grirOrder(t, "PO-1")
	e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-05", "posting_date": "2026-03-05",
		"items": [{"purchase_order_id": %q, "po_item_no": 2, "quantity": {"value": "5", "unit": "KG"}}]}`, e.DE, po), "")
	// The price changes after the release, as an order change will (T47f).
	pgtest.Unguarded(t, `UPDATE procurement.purchase_order_item SET unit_price = 320 WHERE purchase_order_id = $1 AND item_no = 2`, po)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "6.40", "0.00", poItem(po, 2, "2", "KG", "6.40")), "")
	if m := e.act(t, 200, id(inv), "match", ""); m["is_clean"] != true {
		t.Fatalf("match: got %v, want clean", m["issues"])
	}
	posted := e.act(t, 200, id(inv), "post", `{"posting_date": "2026-03-15"}`)
	got := e.entryLines(t, posted["journal_entry_id"])
	// 2 of the 5 received leave 3 open at 3.00: GR/IR is debited 6.00, not
	// the 5.40 that revaluing the open 3 at 3.20 would leave.
	want := []line{{grir, "6.00", po.String()}, {diff, "0.40", nil}, {e.ap.String(), "-6.40", nil}}
	if fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("invoice lines: got %v, want %v", got, want)
	}
}
