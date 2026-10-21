package payments_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/capture"
)

// documents is an extractor that reads each document as the extraction
// given for its content.
type documents map[string]*capture.Extraction

func (d documents) Extract(_ context.Context, document []byte) (*capture.Extraction, error) {
	if x, ok := d[string(document)]; ok {
		return x, nil
	}
	return nil, errors.New("not a document of the test")
}

// creditMemo is the extraction of a credit memo on the order numbered po
// crediting 2 EA of the invoice INV-1 at the order price, printed negative
// as many are.
func creditMemo(number, po string) *capture.Extraction {
	return &capture.Extraction{DocumentKind: "credit_memo", CreditedInvoiceNumber: "INV-1", VendorName: "Supplier", InvoiceNumber: number,
		InvoiceDate: "2026-03-12", PurchaseOrder: po, Currency: "EUR",
		Lines:     []capture.Line{{Description: "Paper", Quantity: "2", Amount: "-24.00", POLine: 1}},
		TaxAmount: "-4.56", GrossAmount: "-28.56", Confidence: "high"}
}

// TestCreditMemoExitCriterion walks T34's exit criterion through the API:
// credit memos captured from documents and a subsequent debit entered,
// matched to their invoice and order, posted (credits as KG) and
// reversed; an invoice is not reversed while a credit memo naming it is
// posted; in the payment run the credit memo nets with its supplier's
// invoice, and a supplier with only a credit memo is paid nothing.
func TestCreditMemoExitCriterion(t *testing.T) {
	docs := documents{}
	e := setupWith(t, []string{"procurement.order.read", "procurement.order.create", "procurement.order.release", "procurement.receipt.post",
		"payables.invoice.review"}, func(cfg *app.Config) { cfg.Extractor = docs })
	e.GRIR(t)
	grir, diff, tax, ap := e.Account(t, "2000", "219000").String(), e.Account(t, "2000", "590000").String(), e.Account(t, "2000", "140000").String(),
		e.Account(t, "2000", "211000").String()
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	e.bankAccount(t, e.Vendor, "DE02120300000000202051", "Vendor", "2026-01-01", "", "create")
	po, number := e.receivedOrder(t)
	docs["%PDF memo 1"], docs["%PDF memo 2"] = creditMemo("CM-1", number), creditMemo("CM-2", number)
	invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "INV-1",
		"invoice_date": "2026-03-10", "gross_amount": {"amount": "142.80", "currency": "EUR"}, "tax_amount": {"amount": "22.80", "currency": "EUR"},
		"items": [%s]}`, e.DE, e.Supplier, orderItem(po, "10", "120.00")), "")
	e.matchedAndPosted(t, invoice, "2026-03-10")

	first := e.captured(t, "%PDF memo 1")
	if first["kind"] != "credit_memo" {
		t.Errorf("captured kind: got %v, want credit_memo", first["kind"])
	}
	if first["reference_invoice_id"] != id(invoice) {
		t.Errorf("captured reference: got %v, want the invoice INV-1", first["reference_invoice_id"])
	}
	if got := first["gross_amount"].(map[string]any)["amount"]; got != "28.56" {
		t.Errorf("captured gross: got %v, want 28.56 (unsigned; the kind gives the direction)", got)
	}
	posted := e.matchedAndPosted(t, first, "2026-03-12")
	e.entry(t, "credit memo", posted["journal_entry_id"], "KG", map[string]string{grir: "-24.00", tax: "-4.56", ap: "28.56"})
	reversal := e.reversed(t, posted, "2026-03-12")
	e.entry(t, "credit memo reversal", reversal["reversal_entry_id"], "KG", map[string]string{grir: "24.00", tax: "4.56", ap: "-28.56"})
	second := e.matchedAndPosted(t, e.captured(t, "%PDF memo 2"), "2026-03-12")

	debit, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "SD-1",
		"invoice_date": "2026-03-14", "kind": "subsequent_debit", "reference_invoice_id": %q, "purchase_order_id": %q,
		"gross_amount": {"amount": "5.95", "currency": "EUR"}, "tax_amount": {"amount": "0.95", "currency": "EUR"}, "items": [%s]}`,
		e.DE, e.Supplier, id(invoice), po, orderItem(po, "0", "5.00")), "")
	posted = e.matchedAndPosted(t, debit, "2026-03-14")
	e.entry(t, "subsequent debit", posted["journal_entry_id"], "KR", map[string]string{grir: "0.00", diff: "5.00", tax: "0.95", ap: "-5.95"})
	reversal = e.reversed(t, posted, "2026-03-14")
	e.entry(t, "subsequent debit reversal", reversal["reversal_entry_id"], "KR", map[string]string{grir: "0.00", diff: "-5.00", tax: "-0.95", ap: "5.95"})

	_, tag := e.Must(200, "GET", "/v1/supplier-invoices/"+id(invoice), "", "")
	e.Problem(409, "payables.invoice_has_corrections", "POST", "/v1/supplier-invoices/"+id(invoice)+":reverse", `{"reason": "wrong"}`, tag)
	e.document(t, "credit_memo", e.Vendor, "CM-9", "30.00", "2026-03-20")

	run := e.run(t, "2026-04-10", "2026-04-10")
	pays := e.payments(t, run)
	if len(pays) != 1 {
		t.Fatalf("payments: got %d, want 1: %v", len(pays), pays)
	}
	pay := pays[0].(map[string]any)
	if pay["supplier_id"] != e.Supplier.String() {
		t.Errorf("payment to: got %v, want the supplier", pay["supplier_id"])
	}
	if got := pay["amount"].(map[string]any)["amount"]; got != "114.24" {
		t.Errorf("payment to the supplier: got %v, want 114.24 (142.80 less the posted credit memo's 28.56)", got)
	}
	if n := len(pay["items"].([]any)); n != 2 {
		t.Errorf("items paid: got %d, want the invoice and the posted credit memo %s", n, id(second))
	}
	if got := e.exclusions(t, run)[e.Vendor.String()]; got != "debit_balance -30.00" {
		t.Errorf("supplier with only a credit memo: got %q, want debit_balance -30.00", got)
	}
}

// receivedOrder creates and releases an order of the supplier, valued
// through GR/IR (10 EA at 12.00, tax code V1), receives all of it and
// returns its id and number.
func (e env) receivedOrder(t *testing.T) (id, number string) {
	t.Helper()
	o, _ := e.Must(201, "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": "2026-03-01",
		"items": [{"description": "Paper", "quantity": {"value": "10", "unit": "EA"}, "unit_price": {"amount": "12.00", "currency": "EUR"},
			"account_id": %q, "tax_code": "V1"}]}`, e.DE, e.Supplier, e.expense), "")
	path := fmt.Sprintf("/v1/purchase-orders/%v", o["id"])
	_, tag := e.Must(200, "GET", path, "", "")
	e.Must(200, "POST", path+":release", "", tag)
	e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-05", "posting_date": "2026-03-05",
		"items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "10", "unit": "EA"}}]}`, e.DE, o["id"]), "")
	return o["id"].(string), o["document_number"].(string)
}

// orderItem is a document item on item 1 of the order.
func orderItem(po, qty, amount string) string {
	return fmt.Sprintf(`{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": %q, "unit": "EA"}, "amount": {"amount": %q, "currency": "EUR"}}`,
		po, qty, amount)
}

// captured captures the document through the API and returns the document
// entered.
func (e env) captured(t *testing.T, document string) map[string]any {
	t.Helper()
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/supplier-invoices:capture", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(),
		Header: map[string][]string{"Content-Type": {"multipart/form-data; boundary=b"}},
		Body: "--b\r\nContent-Disposition: form-data; name=\"legal_entity_id\"\r\n\r\n" + e.DE.String() +
			"\r\n--b\r\nContent-Disposition: form-data; name=\"document\"\r\n\r\n" + document + "\r\n--b--\r\n"})
	if r.Code != 202 {
		t.Fatalf("capture of %q: got %d %s, want 202", document, r.Code, r.Body)
	}
	op := apitest.JSON(t, r)
	if op["status"] != "succeeded" {
		t.Fatalf("capture of %q: operation %v, want succeeded", document, op)
	}
	v, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	return v
}

// matchedAndPosted matches the document clean and posts it on the date.
func (e env) matchedAndPosted(t *testing.T, v map[string]any, date string) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/supplier-invoices/"+id(v), "", "")
	m, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id(v)+":match", "", tag)
	if m["is_clean"] != true {
		t.Fatalf("match of %v %v: got issues %v, want clean", v["kind"], v["supplier_reference"], m["issues"])
	}
	_, tag = e.Must(200, "GET", "/v1/supplier-invoices/"+id(v), "", "")
	posted, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id(v)+":post", fmt.Sprintf(`{"posting_date": %q}`, date), tag)
	return posted
}

// reversed reverses the posted document on the date and returns it.
func (e env) reversed(t *testing.T, v map[string]any, date string) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/supplier-invoices/"+id(v), "", "")
	r, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id(v)+":reverse", fmt.Sprintf(`{"reason": "Wrong document", "posting_date": %q}`, date), tag)
	if r["status"] != "reversed" {
		t.Errorf("reversal of %v %v: got status %v, want reversed", v["kind"], v["supplier_reference"], r["status"])
	}
	return r
}

// entry checks a journal entry's document type and its amount per account.
func (e env) entry(t *testing.T, step string, entry any, documentType string, want map[string]string) {
	t.Helper()
	je, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v", entry), "", "")
	if je["document_type"] != documentType {
		t.Errorf("%s: document type got %v, want %s", step, je["document_type"], documentType)
	}
	got := map[string]string{}
	for _, l := range je["lines"].([]any) {
		m := l.(map[string]any)
		got[m["account_id"].(string)] = m["amount"].(map[string]any)["amount"].(string)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: lines got %v, want %v", step, got, want)
	}
}
