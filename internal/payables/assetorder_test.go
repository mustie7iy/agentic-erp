package payables_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// withAssetOrders gives the prototype's document asset classes (company
// 2000 clears its machines on 169000), company 2000 valuing receipts
// through GR/IR (219000) with its price differences on 590000, a primary
// cost account without a default cost center, a stock account 135000 that
// is no asset clearing account, and the tax code B0: input tax of 5%,
// none of it recovered, borne by the item.
func withAssetOrders(doc *config.Config) {
	fixture.AssetClasses(doc)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "219000", Name: "GR/IR clearing", Type: config.Liability},
		config.Account{Number: "590000", Name: "Price differences", Type: config.Expense}, config.Account{Number: "135000", Name: "Stock", Type: config.Asset})
	de.AccountDetermination.GRIR, de.AccountDetermination.PriceDifference = "219000", "590000"
	de.TaxCodes = append(de.TaxCodes, config.TaxCode{Code: "B0", Description: "Blocked", Components: []config.TaxComponent{{Key: "tax",
		AccountKey: config.KeyInput, Account: "140000", RecoverablePercent: "0", Rates: []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}}}})
}

// TestAssetOrderItemCapitalised: an invoice and a credit memo on an order
// item that is an asset's, one on an asset clearing account of the
// company, post what they differ from the order price and the tax the
// item bears to that clearing account, where the receipt put the order
// price, not to the price difference account: the cost of an asset is
// what was paid for it, non-refundable tax included (IAS 16.16; product
// owner 2026-11-10). The GR/IR line and the payable are as before, the
// line is recorded with its order item and tax, and a reversal mirrors
// it. It posts in a company with cost center accounting whose price
// difference account is a primary cost account, where the line was
// refused before (422 ledger.cost_center_required: an asset's order item
// has no cost center), as an item on another balance sheet account still
// is.
func TestAssetOrderItemCapitalised(t *testing.T) {
	f := fixture.NewWith(t, nil, withAssetOrders, append(perms, "procurement.receipt.post", "procurement.receipt.read")...)
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
	e.CostCenters(t, withAssetOrders)
	grir, diff, clearing := e.Account(t, "2000", "219000").String(), e.Account(t, "2000", "590000").String(), e.Account(t, "2000", "169000").String()
	po := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, 'PO-M', $3, $4, 'EUR', '2026-03-01', 'NT30', 'draft', $5, $5)`,
		e.Tenant.ID, po, e.DE, e.Supplier, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id, tax_code, valuation_price, valuation_price_unit)
		VALUES ($1, $2, 1, 'Milling machine', 10, 'EA', 1200, 1, $3, 'B0', 1200, 1), ($1, $2, 2, 'Steel', 10, 'EA', 100, 1, $4, NULL, 100, 1)`,
		e.Tenant.ID, po, clearing, e.Account(t, "2000", "135000"))
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', valuation = 'gr_ir', version = version + 1 WHERE id = $1`, po)
	e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-05", "posting_date": "2026-03-05",
		"items": [{"purchase_order_id": %[2]q, "po_item_no": 1, "quantity": {"value": "10", "unit": "EA"}},
			{"purchase_order_id": %[2]q, "po_item_no": 2, "quantity": {"value": "10", "unit": "EA"}}]}`, e.DE, po), "")
	blocked := func(qty, amount string) string {
		return strings.Replace(poItem(po, 1, qty, "EA", amount), `"amount":`, `"tax_code": "B0", "amount":`, 1)
	}
	recorded := func(t *testing.T, entry any) string {
		t.Helper()
		var got *string
		pgtest.Scan(t, `SELECT string_agg(a.number || ' ' || l.amount_tc || ' item ' || i.po_item_no || ' tax ' || i.tax_borne_tc || '/' || i.tax_borne_lc
				|| ' cost center ' || coalesce(l.cost_center_id::text, 'none'), '; ' ORDER BY i.line_no)
			FROM ledger.price_difference_item i
			JOIN ledger.journal_line l ON l.fiscal_year = i.fiscal_year AND l.journal_entry_id = i.journal_entry_id AND l.line_no = i.line_no
			JOIN ledger.gl_account a ON a.id = l.account_id WHERE i.journal_entry_id = $1`, []any{entry}, &got)
		if got == nil {
			return ""
		}
		return *got
	}
	lines := func(t *testing.T, what string, entry any, want ...line) {
		t.Helper()
		if got := e.entryLines(t, entry); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
			t.Errorf("%s lines: got %v, want %v", what, got, want)
		}
	}
	// 10 machines at 12.50 against the order's 12.00: 5.00 of price
	// difference and 6.25 of tax not recovered, one line on 169000.
	invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "131.25", "6.25", blocked("10", "125.00")), "")
	posted := e.posted(t, invoice)
	lines(t, "invoice", posted["journal_entry_id"], line{grir, "120.00", po.String()}, line{clearing, "11.25", nil}, line{e.ap.String(), "-131.25", nil})
	if got, want := recorded(t, posted["journal_entry_id"]), "169000 1125 item 1 tax 625/625 cost center none"; got != want {
		t.Errorf("recorded for the invoice:\ngot  %s\nwant %s", got, want)
	}
	var balance int64
	pgtest.Scan(t, `SELECT sum(amount_lc)::bigint FROM ledger.journal_line WHERE account_id = $1`, []any{clearing}, &balance)
	if balance != 13125 {
		t.Errorf("asset clearing account: got %d, want 13125: the order price received and what the invoice paid beyond it", balance)
	}
	// A credit memo of 2 for 25.00: 1.00 over the order price and 1.25 of
	// tax back, off the clearing account.
	memo, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.document("credit_memo", "CM-1", "26.25", "1.25", "", blocked("2", "25.00")), "")
	credited := e.posted(t, memo)
	lines(t, "credit memo", credited["journal_entry_id"], line{grir, "-24.00", po.String()}, line{clearing, "-2.25", nil}, line{e.ap.String(), "26.25", nil})
	if got, want := recorded(t, credited["journal_entry_id"]), "169000 -225 item 1 tax -125/-125 cost center none"; got != want {
		t.Errorf("recorded for the credit memo:\ngot  %s\nwant %s", got, want)
	}
	e.act(t, 200, id(memo), "reverse", `{"reason": "wrong"}`)
	var reversal string
	pgtest.Scan(t, `SELECT reversal_entry_id::text FROM payables.supplier_invoice WHERE id = $1`, []any{id(memo)}, &reversal)
	lines(t, "reversal", reversal, line{grir, "24.00", po.String()}, line{clearing, "2.25", nil}, line{e.ap.String(), "-26.25", nil})
	if got, want := recorded(t, reversal), "169000 225 item 1 tax 125/125 cost center none"; got != want {
		t.Errorf("recorded for the credit memo's reversal:\ngot  %s\nwant %s", got, want)
	}
	var expensed int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_line WHERE account_id = $1`, []any{diff}, &expensed)
	if expensed != 0 {
		t.Errorf("lines on the price difference account: got %d, want none", expensed)
	}
	// The steel, on a balance sheet account that is no asset clearing
	// account, 0.50 over: its price difference still goes to 590000, which
	// needs a cost center the item cannot give.
	steel, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-2", "10.50", "0.00", poItem(po, 2, "10", "EA", "10.50")), "")
	if m := e.act(t, 200, id(steel), "match", ""); m["is_clean"] != true {
		t.Fatalf("match of the steel: got issues %v, want clean", m["issues"])
	}
	e.Problem(422, "ledger.cost_center_required", "POST", "/v1/supplier-invoices/"+id(steel)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(steel)))
}
