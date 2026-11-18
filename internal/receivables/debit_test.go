package receivables_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// debitMemo is the JSON of a debit memo of company 2000 in EUR to the
// customer.
func (e env) debitMemo(customer uuid.UUID, date string, items ...string) string {
	return strings.Replace(e.invoice(customer, date, items...), `"invoice_date"`, `"kind": "debit_memo", "invoice_date"`, 1)
}

// TestDebitMemo (T75j acceptance): a debit memo is entered as an invoice
// is, with its tax at its own date's rates; it posts as an invoice does,
// Dr the customer and Cr revenue and output tax, as a DD entry of
// receivables with its own header text; its debit line is the customer's
// open item with the customer's term from its own date; the events carry
// its amounts positive and its kind, and it is listed by kind.
func TestDebitMemo(t *testing.T) {
	e := setup(t)
	invoice := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1"))), "2026-03-12")
	// A1 is 19% in March and 21% from July: the memo's tax is at its own date.
	got, tag := e.Must(201, "POST", "/v1/customer-invoices", e.debitMemo(e.customer, "2026-07-05", e.item("50.00", "A1")), "")
	id := got["id"].(string)
	fields(t, "the debit memo entered", got, map[string]any{"status": "draft", "kind": "debit_memo",
		"net_amount": eur("50.00"), "tax_amount": eur("10.50"), "gross_amount": eur("60.50")})
	if _, ok := got["reference_invoice_id"]; ok {
		t.Errorf("reference_invoice_id: got %v, want none on a debit memo", got["reference_invoice_id"])
	}
	fields(t, "CustomerInvoiceChanged", e.outboxPayload(t, id, "customer_invoice_changed"), map[string]any{"kind": "debit_memo",
		"net_amount": eur("50.00"), "tax_amount": eur("10.50"), "gross_amount": eur("60.50")})

	posted, _ := e.Must(200, "POST", "/v1/customer-invoices/"+id+":post", `{"posting_date": "2026-07-06"}`, tag)
	fields(t, "the debit memo posted", posted, map[string]any{"status": "posted", "kind": "debit_memo", "posting_date": "2026-07-06",
		"payment_term_code": "NT30", "net_due_date": "2026-08-04", "gross_amount": eur("60.50")})
	entry := posted["journal_entry_id"].(string)
	var docType, source, text, number string
	pgtest.Scan(t, `SELECT document_type, source_context, header_text, document_number FROM ledger.journal_entry WHERE tenant_id = $1 AND id = $2`,
		[]any{e.Tenant.ID, entry}, &docType, &source, &text, &number)
	fields(t, "the debit memo's entry", map[string]any{"document_type": docType, "source_context": source, "header_text": text,
		"journal_entry_number": number}, map[string]any{"document_type": "DD", "source_context": "receivables",
		"header_text": "Customer debit memo " + posted["document_number"].(string), "journal_entry_number": posted["journal_entry_number"]})
	if got, want := show(e.entryLines(t, entry)), "120000 6050 - true; 400000 -5000 A1 false; 230000 -1050 A1 false; "; got != want {
		t.Errorf("lines: got %s, want %s", got, want)
	}
	open := e.openItems(t, e.customer)
	if want := map[string]string{invoice["journal_entry_id"].(string): "119.00 EUR", entry: "60.50 EUR"}; !maps.Equal(open, want) {
		t.Errorf("the customer's open items by entry: got %v, want %v", open, want)
	}
	fields(t, "CustomerInvoicePosted", e.outboxPayload(t, id, "customer_invoice_posted"), map[string]any{"kind": "debit_memo",
		"reference_invoice_id": nil, "net_amount": eur("50.00"), "tax_amount": eur("10.50"), "gross_amount": eur("60.50"),
		"payment_term_code": "NT30", "net_due_date": "2026-08-04"})
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND detail->>'kind' = 'debit_memo'
		AND action IN ('receivables.customer_invoice.create', 'receivables.customer_invoice.post')`, []any{e.Tenant.ID, id}, &audits)
	if audits != 2 {
		t.Errorf("audit records of the debit memo: got %d, want its entry's and its posting's", audits)
	}
	page, _ := e.Must(200, "GET", "/v1/customer-invoices?kind=debit_memo", "", "")
	var listed []string
	for _, it := range items(t, page) {
		listed = append(listed, it["id"].(string))
	}
	if !slices.Equal(listed, []string{id}) {
		t.Errorf("the debit memos listed: got %v, want %s alone", listed, id)
	}
}

// TestDebitMemoWithoutRange: a company without the journal_dd number range
// cannot post a debit memo (422 config.no_number_range): it stays a draft
// and nothing posts; an invoice still posts.
func TestDebitMemoWithoutRange(t *testing.T) {
	f := fixture.NewWith(t, nil, func(doc *config.Config) {
		outputCodes("")(doc)
		doc.NumberRanges = slices.DeleteFunc(doc.NumberRanges, func(r config.NumberRange) bool { return r.DocumentType == "journal_dd" })
	}, perms...)
	e := env{Env: f, revenue: f.Account(t, "2000", "400000"), receivable: f.Account(t, "2000", "120000")}
	e.customer = e.partner(t, "BP70000001", "Northwind Traders", "active", "customer")
	e.companyData(t, e.customer, "NT30")
	memo := e.entered(t, e.debitMemo(e.customer, "2026-03-10", e.item("10.00", "")))
	before := e.entries(t)
	e.Problem(422, "config.no_number_range", "POST", "/v1/customer-invoices/"+memo+":post", `{"posting_date": "2026-03-12"}`, `"1"`)
	if n := e.entries(t); n != before {
		t.Errorf("journal entries: got %d, want %d, nothing posted", n, before)
	}
	got, _ := e.Must(200, "GET", "/v1/customer-invoices/"+memo, "", "")
	if got["status"] != "draft" {
		t.Errorf("status: got %v, want draft", got["status"])
	}
	e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("10.00", ""))), "2026-03-12")
}
