package payables_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestTaxBorneRecordedOnPriceDifferences: an invoice and a credit memo on
// an order valued through GR/IR post the tax their item bears with its
// price difference as one line, as before, and the ledger records beside
// that line its order item and the tax, signed as the line; the
// invoice's reversal records them negated (B10, po:2026-11-10).
func TestTaxBorneRecordedOnPriceDifferences(t *testing.T) {
	f := fixture.New(t, append(perms, "procurement.receipt.post", "procurement.receipt.read")...)
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
	doc := fixture.Document(t)
	fixture.GRIRAccounts(doc)
	// B0: input tax of 5%, none of it recovered (blocked), borne by the item.
	fixture.TaxCodes(t, e.Tenant, doc, config.TaxCode{Code: "B0", Description: "Blocked", Components: []config.TaxComponent{{Key: "tax",
		AccountKey: config.KeyInput, Account: "140000", RecoverablePercent: "0", Rates: []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}}}})
	grir, diff := e.Account(t, "2000", "219000").String(), e.Account(t, "2000", "590000").String()
	po := e.grirOrder(t, "PO-1")
	blocked := func(qty, amount string) string {
		return strings.Replace(poItem(po, 1, qty, "EA", amount), `"amount":`, `"tax_code": "B0", "amount":`, 1)
	}
	e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-05", "posting_date": "2026-03-05",
		"items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "10", "unit": "EA"}}]}`, e.DE, po), "")
	recorded := func(t *testing.T, entry any) string {
		t.Helper()
		var got *string
		pgtest.Scan(t, `SELECT string_agg(a.number || ' ' || l.amount_tc || ' item ' || i.po_item_no || ' of ' || i.purchase_order_id || ' tax ' || i.tax_borne_tc
				|| '/' || i.tax_borne_lc, '; ' ORDER BY i.line_no)
			FROM ledger.price_difference_item i
			JOIN ledger.journal_line l ON l.fiscal_year = i.fiscal_year AND l.journal_entry_id = i.journal_entry_id AND l.line_no = i.line_no
			JOIN ledger.gl_account a ON a.id = l.account_id WHERE i.journal_entry_id = $1`, []any{entry}, &got)
		if got == nil {
			return ""
		}
		return *got
	}
	// 10 at 12.50 against the order's 12.00: 5.00 of price difference and
	// 6.25 of tax on one line.
	invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "131.25", "6.25", blocked("10", "125.00")), "")
	posted := e.posted(t, invoice)
	want := []line{{grir, "120.00", po.String()}, {diff, "11.25", nil}, {e.ap.String(), "-131.25", nil}}
	if got := e.entryLines(t, posted["journal_entry_id"]); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("invoice lines: got %v, want %v", got, want)
	}
	if got, want := recorded(t, posted["journal_entry_id"]), fmt.Sprintf("590000 1125 item 1 of %s tax 625/625", po); got != want {
		t.Errorf("recorded for the invoice:\ngot  %s\nwant %s", got, want)
	}
	// A credit memo of 2 for 25.00: 1.00 over the order price and 1.25 of tax back.
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", "CM-1", "26.25", "1.25", "", blocked("2", "25.00")), "")
	credited := e.posted(t, memo)
	want = []line{{grir, "-24.00", po.String()}, {diff, "-2.25", nil}, {e.ap.String(), "26.25", nil}}
	if got := e.entryLines(t, credited["journal_entry_id"]); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
		t.Errorf("credit memo lines: got %v, want %v", got, want)
	}
	if got, want := recorded(t, credited["journal_entry_id"]), fmt.Sprintf("590000 -225 item 1 of %s tax -125/-125", po); got != want {
		t.Errorf("recorded for the credit memo:\ngot  %s\nwant %s", got, want)
	}
	e.act(t, 200, id(memo), "reverse", `{"reason": "wrong"}`)
	var reversal string
	pgtest.Scan(t, `SELECT reversal_entry_id::text FROM payables.supplier_invoice WHERE id = $1`, []any{id(memo)}, &reversal)
	if got, want := recorded(t, reversal), fmt.Sprintf("590000 225 item 1 of %s tax 125/125", po); got != want {
		t.Errorf("recorded for the credit memo's reversal:\ngot  %s\nwant %s", got, want)
	}
}
