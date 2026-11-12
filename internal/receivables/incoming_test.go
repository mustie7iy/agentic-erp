package receivables_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
	"github.com/mustie7iy/agentic-erp/internal/taxfiling"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// postings is the ledger's posting port as the API wires it: the tests
// post receipts with it as the payments context does.
func postings() ledger.Postings {
	return ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}, TaxReturns: taxfiling.Holds{},
		Settings: configuration.Settings{}}
}

// committed posts with fn in a committed request and returns the entry.
func (e env) committed(t *testing.T, fn func(ctx context.Context) (ledger.Entry, error)) ledger.Entry {
	t.Helper()
	var entry ledger.Entry
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		entry, err = fn(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return entry
}

// receipt posts a bank statement line of house bank MAIN as the payments
// context posts one matched as incoming: the amount on the main bank
// account 102000 against the incoming payments clearing account 103000.
// It returns the entry, which is the receipt.
func (e env) receipt(t *testing.T, amount int64, day string) ledger.Entry {
	t.Helper()
	text := "Bank statement DB-" + day + " line 1"
	return e.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().PostBank(ctx, ledger.BankPosting{LegalEntityID: e.DE, DocumentType: "SA", PostingDate: date(t, day), Currency: "EUR",
			BankAccountID: e.bank, Amount: money.Minor(amount), CounterAccountID: e.incoming, HeaderText: &text, SourceContext: "payments",
			SourceDocumentID: uuid.New()})
	})
}

// invoiced enters and posts an invoice of the amount, without tax, to the
// customer and returns its open item as the request names it.
func (e env) invoiced(t *testing.T, customer uuid.UUID, amount, day string) string {
	t.Helper()
	got := e.post(t, e.entered(t, e.invoice(customer, day, e.item(amount, ""))), day)
	return itemRef(got["journal_entry_id"], 1)
}

// credited posts a credit of the customer's on its receivables account,
// as a credit memo will (T75d), and returns its open item.
func (e env) credited(t *testing.T, customer uuid.UUID, amount int64, day string) string {
	t.Helper()
	entry := e.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: date(t, day), PostingDate: date(t, day),
			Currency: "EUR", SourceContext: "receivables", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
				{AccountID: e.receivable, Amount: money.Minor(-amount), PartnerID: &customer}, {AccountID: e.revenue, Amount: money.Minor(amount)}}})
	})
	return itemRef(entry.ID, 1)
}

func itemRef(entry any, line int) string {
	return fmt.Sprintf(`{"fiscal_year": 2026, "journal_entry_id": "%v", "line_no": %d}`, entry, line)
}

// entryOf is the journal entry an item's reference names.
func entryOf(t *testing.T, ref string) uuid.UUID {
	t.Helper()
	var r struct {
		Entry uuid.UUID `json:"journal_entry_id"`
	}
	if err := json.Unmarshal([]byte(ref), &r); err != nil {
		t.Fatal(err)
	}
	return r.Entry
}

// payment is the JSON that applies the receipt to the customer's items in
// company 2000 on the day ("" for the company's date now).
func (e env) payment(customer uuid.UUID, receipt any, day string, items ...string) string {
	posting := ""
	if day != "" {
		posting = fmt.Sprintf(`"posting_date": %q, `, day)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "receipt_journal_entry_id": "%v", %s"items": [%s]}`,
		e.DE, customer, receipt, posting, strings.Join(items, ", "))
}

// payments counts the tenant's incoming payments applied.
func (e env) payments(t *testing.T) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM receivables.incoming_payment WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	return n
}

// receipts lists the company's receipts not yet applied, by entry id.
func (e env) receipts(t *testing.T, query string) ([]map[string]any, map[string]any) {
	t.Helper()
	page, _ := e.In(t).Must(200, "GET", "/v1/unapplied-receipts?legal_entity_id="+e.DE.String()+query, "", "")
	return items(t, page), page
}

// TestApplyIncomingPayment (T75c acceptance): a receipt waiting on the
// house bank's incoming account is applied to two invoices of the
// customer and a credit of the customer's, which together are the receipt
// exactly; one DZ entry of receivables clears the receipt and the items;
// the customer has no open item left and the receipt is no longer listed;
// the document reads back the same, with the names of what it references;
// the audit record and IncomingPaymentApplied are written.
func TestApplyIncomingPayment(t *testing.T) {
	e := setup(t)
	receipt := e.receipt(t, 15000, "2026-03-20")
	first, second, credit := e.invoiced(t, e.customer, "100.00", "2026-03-10"), e.invoiced(t, e.customer, "80.00", "2026-03-11"),
		e.credited(t, e.customer, 3000, "2026-03-12")
	got, tag := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, receipt.ID, "2026-03-21", first, second, credit), "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	for f, want := range map[string]any{"company_code": "2000", "company_name": "Acme GmbH", "customer_id": e.customer.String(), "customer_number": "BP70000001",
		"customer_name": "Northwind Traders", "posting_date": "2026-03-21", "currency": "EUR", "legal_entity_id": e.DE.String(), "version": float64(1),
		"receipt_journal_entry_id": receipt.ID.String(), "receipt_journal_entry_year": float64(2026), "receipt_journal_entry_number": receipt.DocumentNumber,
		"receipt_account_id": e.incoming.String(), "receipt_account_number": "103000", "receipt_account_name": "Bank incoming - Deutsche Bank",
		"journal_entry_year": float64(2026), "updated_at": got["created_at"], "updated_by": got["created_by"]} {
		if got[f] != want {
			t.Errorf("%s: got %v, want %v", f, got[f], want)
		}
	}
	if n, _ := got["document_number"].(string); !strings.HasPrefix(n, "IP") {
		t.Errorf("document number: got %v, want one of the incoming_payment range (IP…)", got["document_number"])
	}
	if a := amount(t, got, "amount"); a != "150.00 EUR" {
		t.Errorf("amount: got %s, want 150.00 EUR", a)
	}
	its := items(t, got)
	if len(its) != 3 {
		t.Fatalf("items: got %d, want 3", len(its))
	}
	for i, want := range []string{"100.00 EUR", "80.00 EUR", "-30.00 EUR"} {
		if a := amount(t, its[i], "amount"); a != want || its[i]["item_no"] != float64(i+1) || its[i]["line_no"] != float64(1) || its[i]["fiscal_year"] != float64(2026) {
			t.Errorf("item %d: got %s as item %v, line %v of %v; want %s as item %d, line 1 of 2026", i+1, a, its[i]["item_no"], its[i]["line_no"],
				its[i]["fiscal_year"], want, i+1)
		}
	}
	var invoiceNumber, entryNumber string
	pgtest.Scan(t, `SELECT i.document_number, e.document_number FROM receivables.customer_invoice i JOIN ledger.journal_entry e ON e.id = i.journal_entry_id
		WHERE i.tenant_id = $1 AND i.journal_entry_id = $2`, []any{e.Tenant.ID, its[0]["journal_entry_id"]}, &invoiceNumber, &entryNumber)
	if its[0]["reference"] != invoiceNumber || its[0]["journal_entry_number"] != entryNumber {
		t.Errorf("item 1: got reference %v and entry %v, want the invoice's number %s and its entry's %s", its[0]["reference"], its[0]["journal_entry_number"],
			invoiceNumber, entryNumber)
	}
	if its[2]["reference"] != its[2]["journal_entry_number"] || its[2]["reference"] == "" {
		t.Errorf("item 3, an entry without a reference: got reference %v, want its entry's number %v", its[2]["reference"], its[2]["journal_entry_number"])
	}
	id, entry := got["id"].(string), got["journal_entry_id"].(string)
	var number, docType, sourceContext, header string
	var source uuid.UUID
	pgtest.Scan(t, `SELECT document_number, document_type, source_context, source_document_id, header_text FROM ledger.journal_entry
		WHERE tenant_id = $1 AND id = $2`, []any{e.Tenant.ID, entry}, &number, &docType, &sourceContext, &source, &header)
	if got["journal_entry_number"] != number || docType != "DZ" || sourceContext != "receivables" || source.String() != id ||
		header != "Incoming payment of customer BP70000001" {
		t.Errorf("clearing entry: got number %s (the payment names %v), type %s, source %s %s, text %q; want a DZ entry of receivables for the payment",
			number, got["journal_entry_number"], docType, sourceContext, source, header)
	}
	if got, want := show(e.entryLines(t, entry)), "103000 15000 - false; 120000 -10000 - true; 120000 -8000 - true; 120000 3000 - true; "; got != want {
		t.Errorf("clearing entry's lines: got %s, want %s", got, want)
	}
	if page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.customer.String(), "", ""); len(items(t, page)) != 0 {
		t.Errorf("the customer's open items: got %v, want none", page["items"])
	}
	if left, _ := e.receipts(t, ""); len(left) != 0 {
		t.Errorf("receipts not applied: got %v, want none", left)
	}
	var cleared int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.clearing_item WHERE tenant_id = $1 AND journal_entry_id = $2`, []any{e.Tenant.ID, receipt.ID}, &cleared)
	if cleared != 1 {
		t.Errorf("the receipt's cleared lines: got %d, want its line on the incoming account", cleared)
	}
	read, readTag := e.Must(200, "GET", "/v1/incoming-payments/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b || readTag != tag {
		t.Errorf("read back: got %s (%s), want %s (%s)", a, readTag, b, tag)
	}
	e.applied(t, got, receipt.ID.String())
	t.Run("the ledger does not reverse the clearing entry", func(t *testing.T) {
		_, tag := e.In(t).Must(200, "GET", "/v1/journal-entries/"+entry, "", "")
		e.In(t).Problem(409, "ledger.entry_owned_elsewhere", "POST", "/v1/journal-entries/"+entry+":reverse", `{}`, tag)
	})
}

// applied checks the audit record and the event of an applied payment.
func (e env) applied(t *testing.T, got map[string]any, receipt string) {
	t.Helper()
	id := got["id"]
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'receivables.incoming_payment.apply' AND object_id = $2`,
		[]any{e.Tenant.ID, id}, &audits)
	if audits != 1 {
		t.Errorf("audit records of the application: got %d, want 1", audits)
	}
	var events int
	var payload map[string]any
	pgtest.Scan(t, `SELECT count(*) OVER (), payload FROM receivables.outbox WHERE tenant_id = $1 AND aggregate_id = $2
		AND event_type = 'com.example.erp.receivables.incoming_payment_applied.v1'`, []any{e.Tenant.ID, id}, &events, &payload)
	if events != 1 {
		t.Fatalf("IncomingPaymentApplied events: got %d, want 1", events)
	}
	want := map[string]any{"incoming_payment_id": id, "document_number": got["document_number"], "legal_entity_id": e.DE.String(),
		"legal_entity_code": "2000", "customer_id": e.customer.String(), "customer_number": "BP70000001", "posting_date": "2026-03-21",
		"amount": map[string]any{"amount": "150.00", "currency": "EUR"}, "receipt_journal_entry_id": receipt,
		"journal_entry_id": got["journal_entry_id"], "journal_entry_year": float64(2026), "journal_entry_number": got["journal_entry_number"],
		"item_count": float64(3)}
	if a, b := mustJSON(t, payload), mustJSON(t, want); a != b {
		t.Errorf("event payload: got %s, want %s", a, b)
	}
}

// TestApplyOnce (T75c acceptance): the same Idempotency-Key applies a
// receipt once and answers it again; another request for the receipt is
// refused, since it is no longer open, and posts nothing; requests at
// once apply it once.
func TestApplyOnce(t *testing.T) {
	e := setup(t)
	receipt := e.receipt(t, 10000, "2026-03-20")
	body := e.payment(e.customer, receipt.ID, "2026-03-21", e.invoiced(t, e.customer, "100.00", "2026-03-10"))
	req := apitest.Req{Method: "POST", Path: "/v1/incoming-payments", Body: body, Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	first, second := e.API.Do(req), e.API.Do(req)
	if first.Code != 201 || second.Code != 201 {
		t.Fatalf("statuses: got %d and %d, want 201 twice: %s", first.Code, second.Code, second.Body)
	}
	a, b := apitest.JSON(t, first), apitest.JSON(t, second)
	if mustJSON(t, a) != mustJSON(t, b) || second.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay: got %v (Idempotent-Replayed %q), want the first answer %v replayed", b, second.Header().Get("Idempotent-Replayed"), a)
	}
	if loc := first.Header().Get("Location"); loc != "/v1/incoming-payments/"+a["id"].(string) {
		t.Errorf("Location: got %q, want the payment's", loc)
	}
	entries := e.entries(t)
	p := e.Problem(409, "receivables.receipt_already_applied", "POST", "/v1/incoming-payments", body, "")
	if d, _ := p["detail"].(string); !strings.Contains(d, a["document_number"].(string)) {
		t.Errorf("detail: got %q, want it to name the payment %v", d, a["document_number"])
	}
	if n, m := e.entries(t), e.payments(t); n != entries || m != 1 {
		t.Errorf("after a second request: got %d journal entries more and %d payments, want none more and 1", n-entries, m)
	}
	t.Run("requests at once", func(t *testing.T) {
		other := e.receipt(t, 5000, "2026-03-20")
		body := e.payment(e.customer, other.ID, "2026-03-21", e.invoiced(t, e.customer, "50.00", "2026-03-10"))
		before := e.entries(t)
		codes := make([]int, 4)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Go(func() {
				codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: "/v1/incoming-payments", Body: body, Tenant: e.Tenant.ID, As: e.Tenant.Human,
					Key: apitest.NewKey()}).Code
			})
		}
		wg.Wait()
		// The others find the receipt applied, or their line already cleared.
		slices.Sort(codes)
		if !slices.Equal(codes, []int{201, 409, 409, 409}) {
			t.Errorf("statuses: got %v, want one 201 and three 409", codes)
		}
		if n, m := e.entries(t)-before, e.payments(t); n != 1 || m != 2 {
			t.Errorf("after requests at once: got %d journal entries more and %d payments, want 1 more and 2", n, m)
		}
	})
}

// TestApplyRefusals (T75c acceptance): items that do not equal the receipt
// exactly, items of another customer or company, cleared, reversed or
// reserved items, what is no receipt, an unknown customer, a period that
// is not open and a company the caller may not act in are refused, each
// with its code, and nothing is posted or written.
func TestApplyRefusals(t *testing.T) {
	e := setup(t)
	other := e.partner(t, "BP70000002", "Contoso AG", "active", "customer")
	e.companyData(t, other, "NT30")
	receipt := e.receipt(t, 10000, "2026-03-20").ID
	invoice, small, others := e.invoiced(t, e.customer, "100.00", "2026-03-10"), e.invoiced(t, e.customer, "99.99", "2026-03-10"),
		e.invoiced(t, other, "100.00", "2026-03-10")
	paid, credit := e.invoiced(t, e.customer, "40.00", "2026-03-10"), e.credited(t, e.customer, 4000, "2026-03-10")
	e.Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-03-15", "items": [%s, %s]}`, e.DE, paid, credit), "")
	held := e.invoiced(t, e.customer, "100.00", "2026-03-10")
	heldEntry := entryOf(t, held)
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return postings().Reserve(ctx, ledger.Owner{Context: "payments", ID: uuid.New(), Reference: "PR1"},
			[]ledger.ItemRef{{FiscalYear: 2026, EntryID: heldEntry, LineNo: 1}})
	}); err != nil {
		t.Fatal(err)
	}
	// A debit of 100.00, posted through the ledger's port and reversed there.
	reversed := e.credited(t, e.customer, -10000, "2026-03-10")
	e.committed(t, func(ctx context.Context) (ledger.Entry, error) {
		return postings().Reverse(ctx, entryOf(t, reversed), nil, nil)
	})
	out := e.receipt(t, -10000, "2026-03-20").ID // a payment out: a debit on the incoming account
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 2`,
		e.Tenant.ID)
	for _, c := range []struct {
		name, body  string
		status      int
		code, field string
	}{
		{"less than the receipt", e.payment(e.customer, receipt, "2026-03-21", small), 422, "ledger.clearing_not_balanced", "items"},
		{"more than the receipt", e.payment(e.customer, receipt, "2026-03-21", invoice, small), 422, "ledger.clearing_not_balanced", "items"},
		{"another customer's item", e.payment(e.customer, receipt, "2026-03-21", others), 422, "ledger.not_the_customers_item", "items[0]"},
		{"a revenue line", e.payment(e.customer, receipt, "2026-03-21", strings.Replace(invoice, `"line_no": 1`, `"line_no": 2`, 1)), 422,
			"ledger.not_an_open_item", "items[0]"},
		{"a cleared item", e.payment(e.customer, receipt, "2026-03-21", invoice, paid), 409, "ledger.item_already_cleared", ""},
		{"a reversed item", e.payment(e.customer, receipt, "2026-03-21", reversed), 409, "ledger.entry_reversed", ""},
		{"a reserved item", e.payment(e.customer, receipt, "2026-03-21", held), 409, "ledger.item_reserved", ""},
		{"an entry that is no receipt", e.payment(e.customer, heldEntry, "2026-03-21", invoice), 422, "receivables.receipt_not_open", "receipt_journal_entry_id"},
		{"a payment out", e.payment(e.customer, out, "2026-03-21", invoice), 422, "receivables.receipt_not_open", "receipt_journal_entry_id"},
		{"an unknown customer", e.payment(uuid.New(), receipt, "2026-03-21", invoice), 422, "request.reference_not_found", "customer_id"},
		{"a closed period", e.payment(e.customer, receipt, "2026-02-21", invoice), 409, "ledger.period_not_open", ""},
		{"a date without a fiscal year", e.payment(e.customer, receipt, "2019-03-21", invoice), 422, "ledger.no_fiscal_period", ""},
		{"an item twice", e.payment(e.customer, receipt, "2026-03-21", invoice, invoice), 400, "request.invalid_body", ""},
		{"no items", e.payment(e.customer, receipt, "2026-03-21"), 400, "request.invalid_body", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := e.entries(t)
			p := e.In(t).Problem(c.status, c.code, "POST", "/v1/incoming-payments", c.body, "")
			if f := field(p); c.field != "" && f != c.field {
				t.Errorf("field: got %v, want %s", f, c.field)
			}
			if n, m := e.entries(t)-before, e.payments(t); n != 0 || m != 0 {
				t.Errorf("got %d journal entries more and %d payments, want none", n, m)
			}
		})
	}
	t.Run("a company the caller may not act in", func(t *testing.T) {
		elsewhere := e.As(e.Reader(t, "us-only", e.US)).In(t)
		if p := elsewhere.Problem(422, "request.reference_not_found", "POST", "/v1/incoming-payments", e.payment(e.customer, receipt, "2026-03-21", invoice), ""); field(p) != "legal_entity_id" {
			t.Errorf("field: got %v, want legal_entity_id", field(p))
		}
	})
	t.Run("an agent, and a caller without the permission", func(t *testing.T) {
		body := e.payment(e.customer, receipt, "2026-03-21", invoice)
		e.As(e.Tenant.Agent).In(t).Problem(403, "identity.agent_not_allowed", "POST", "/v1/incoming-payments", body, "")
		none := fixture.New(t)
		none.Problem(403, "identity.permission_missing", "POST", "/v1/incoming-payments", body, "")
		none.Problem(403, "identity.permission_missing", "GET", "/v1/incoming-payments", "", "")
		none.Problem(403, "identity.permission_missing", "GET", "/v1/unapplied-receipts?legal_entity_id="+e.DE.String(), "", "")
	})
	if left, _ := e.receipts(t, ""); len(left) != 1 || left[0]["journal_entry_id"] != receipt.String() {
		t.Errorf("receipts not applied after the refusals: got %v, want the receipt alone", left)
	}
}

// TestUnappliedReceipts: the receipts waiting on the house bank's incoming
// account are listed by posting date with what names them: the entry's
// number and text, the amount, the account and the house bank; a payment
// out, a line another context posted there and a receipt applied or
// reversed are not receipts to apply; the list pages; only a company the
// caller may act in is read.
func TestUnappliedReceipts(t *testing.T) {
	e := setup(t)
	later, first, applied, undone := e.receipt(t, 20000, "2026-03-21"), e.receipt(t, 10000, "2026-03-20"), e.receipt(t, 5000, "2026-03-19"),
		e.receipt(t, 700, "2026-03-18")
	e.receipt(t, -900, "2026-03-20")                                 // a payment out
	e.committed(t, func(ctx context.Context) (ledger.Entry, error) { // a transfer the ledger's own context posted to the account
		return postings().Post(ctx, ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: date(t, "2026-03-20"), PostingDate: date(t, "2026-03-20"),
			Currency: "EUR", SourceContext: "ledger", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{{AccountID: e.bank, Amount: 300},
				{AccountID: e.incoming, Amount: -300}}})
	})
	e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, applied.ID, "2026-03-21", e.invoiced(t, e.customer, "50.00", "2026-03-10")), "")
	e.committed(t, func(ctx context.Context) (ledger.Entry, error) { return postings().Reverse(ctx, undone.ID, nil, nil) })
	got, _ := e.receipts(t, "")
	if len(got) != 2 || got[0]["journal_entry_id"] != first.ID.String() || got[1]["journal_entry_id"] != later.ID.String() {
		t.Fatalf("receipts not applied: got %v, want the receipts of 20 and 21 March in that order", got)
	}
	r := got[0]
	for f, want := range map[string]any{"journal_entry_year": float64(2026), "journal_entry_number": first.DocumentNumber, "text": "Bank statement DB-2026-03-20 line 1",
		"legal_entity_id": e.DE.String(), "posting_date": "2026-03-20", "account_id": e.incoming.String(), "account_number": "103000",
		"account_name": "Bank incoming - Deutsche Bank", "house_bank_code": "MAIN", "statement_match_id": first.SourceDocumentID.String()} {
		if r[f] != want {
			t.Errorf("%s: got %v, want %v", f, r[f], want)
		}
	}
	if a := amount(t, r, "amount"); a != "100.00 EUR" {
		t.Errorf("amount: got %s, want 100.00 EUR", a)
	}
	t.Run("in pages", func(t *testing.T) {
		page, raw := e.receipts(t, "&page_size=1")
		token, _ := raw["next_page_token"].(string)
		if len(page) != 1 || page[0]["journal_entry_id"] != first.ID.String() || token == "" {
			t.Fatalf("first page: got %v and token %q, want the first receipt and a token", page, token)
		}
		rest, raw := e.receipts(t, "&page_size=1&page_token="+token)
		if len(rest) != 1 || rest[0]["journal_entry_id"] != later.ID.String() || raw["next_page_token"] != nil {
			t.Errorf("second page: got %v, want the later receipt alone and no token", raw)
		}
		newest, _ := e.receipts(t, "&order_by=posting_date%20desc")
		if len(newest) != 2 || newest[0]["journal_entry_id"] != later.ID.String() {
			t.Errorf("newest first: got %v, want the later receipt first", newest)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		path := "/v1/unapplied-receipts?legal_entity_id=" + e.DE.String()
		e.In(t).Problem(400, "request.invalid_order_by", "GET", path+"&order_by=amount", "", "")
		e.In(t).Problem(400, "request.invalid_page_token", "GET", path+"&page_token=nonsense", "", "")
		e.In(t).Problem(400, "request.invalid_parameter", "GET", "/v1/unapplied-receipts", "", "")
		elsewhere := e.As(e.Reader(t, "us-only", e.US)).In(t)
		if p := elsewhere.Problem(422, "request.reference_not_found", "GET", path, "", ""); field(p) != "legal_entity_id" {
			t.Errorf("field: got %v, want legal_entity_id", field(p))
		}
	})
	t.Run("a company whose house bank takes no incoming payments", func(t *testing.T) {
		page, _ := e.In(t).Must(200, "GET", "/v1/unapplied-receipts?legal_entity_id="+e.US.String(), "", "")
		if len(items(t, page)) != 0 {
			t.Errorf("got %v, want none", page["items"])
		}
	})
}

// TestListIncomingPayments: the payments applied are listed by posting
// date or document number, filtered by company, customer, document number
// and posting date, in pages, each with what it names; one of a company
// the caller may not act in is neither listed nor found.
func TestListIncomingPayments(t *testing.T) {
	e := setup(t)
	second := e.partner(t, "BP70000003", "Contoso AG", "active", "customer")
	e.companyData(t, second, "NT30")
	var ids, numbers []string
	for i, c := range []struct {
		customer uuid.UUID
		day      string
	}{{e.customer, "2026-03-21"}, {second, "2026-03-22"}, {e.customer, "2026-03-23"}} {
		receipt := e.receipt(t, int64(1000*(i+1)), "2026-03-20")
		got, _ := e.Must(201, "POST", "/v1/incoming-payments",
			e.payment(c.customer, receipt.ID, c.day, e.invoiced(t, c.customer, fmt.Sprintf("%d.00", 10*(i+1)), "2026-03-10")), "")
		ids, numbers = append(ids, got["id"].(string)), append(numbers, got["document_number"].(string))
	}
	list := func(t *testing.T, query string) []string {
		t.Helper()
		m, _ := e.In(t).Must(200, "GET", "/v1/incoming-payments?"+query, "", "")
		var out []string
		for _, it := range items(t, m) {
			out = append(out, it["id"].(string))
		}
		return out
	}
	for _, c := range []struct {
		name, query string
		want        []string
	}{
		{"all, by posting date", "", ids},
		{"a customer's", "customer_id=" + e.customer.String(), []string{ids[0], ids[2]}},
		{"a company's", "legal_entity_id=" + e.DE.String(), ids},
		{"another company's", "legal_entity_id=" + e.US.String(), nil},
		{"by document number", "document_number=" + numbers[1], []string{ids[1]}},
		{"from a date", "posting_date_from=2026-03-22", []string{ids[1], ids[2]}},
		{"to a date", "posting_date_to=2026-03-22", []string{ids[0], ids[1]}},
		{"newest first", "order_by=posting_date%20desc", []string{ids[2], ids[1], ids[0]}},
		{"by number", "order_by=document_number", ids},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := list(t, c.query); strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	t.Run("in pages", func(t *testing.T) {
		for _, order := range []string{"posting_date", "document_number"} {
			first, _ := e.In(t).Must(200, "GET", "/v1/incoming-payments?page_size=2&order_by="+order, "", "")
			token, _ := first["next_page_token"].(string)
			if len(items(t, first)) != 2 || token == "" {
				t.Fatalf("%s, first page: got %d items and token %q, want 2 and a token", order, len(items(t, first)), token)
			}
			rest, _ := e.In(t).Must(200, "GET", "/v1/incoming-payments?page_size=2&order_by="+order+"&page_token="+token, "", "")
			if its := items(t, rest); len(its) != 1 || its[0]["id"] != ids[2] || rest["next_page_token"] != nil {
				t.Errorf("%s, second page: got %v, want the third payment alone and no token", order, rest)
			}
		}
	})
	t.Run("with the names", func(t *testing.T) {
		m, _ := e.In(t).Must(200, "GET", "/v1/incoming-payments?customer_id="+second.String(), "", "")
		it := items(t, m)[0]
		if it["customer_name"] != "Contoso AG" || it["customer_number"] != "BP70000003" || it["company_code"] != "2000" || it["receipt_account_number"] != "103000" {
			t.Errorf("got %v %v %v %v, want Contoso AG BP70000003 2000 103000", it["customer_name"], it["customer_number"], it["company_code"],
				it["receipt_account_number"])
		}
	})
	t.Run("company access", func(t *testing.T) {
		elsewhere := e.As(e.Reader(t, "us-only", e.US)).In(t)
		elsewhere.Problem(404, "request.not_found", "GET", "/v1/incoming-payments/"+ids[0], "", "")
		if got, _ := elsewhere.Must(200, "GET", "/v1/incoming-payments", "", ""); len(items(t, got)) != 0 {
			t.Errorf("another company's list: got %v, want none", got["items"])
		}
		e.As(e.Reader(t, "de-only", e.DE)).In(t).Must(200, "GET", "/v1/incoming-payments/"+ids[0], "", "")
		e.In(t).Problem(404, "request.not_found", "GET", "/v1/incoming-payments/"+uuid.NewString(), "", "")
		e.In(t).Problem(400, "request.invalid_order_by", "GET", "/v1/incoming-payments?order_by=amount", "", "")
	})
}

// TestApplyThroughThePort: what only a port could answer: without a
// posting date the payment posts on the company's date now; a company the
// configuration lacks has no incoming account, so nothing is a receipt
// there and none is listed.
func TestApplyThroughThePort(t *testing.T) {
	e := setup(t)
	receipt := e.receipt(t, 10000, "2026-03-20")
	in := receivables.PaymentInput{LegalEntityID: e.DE, CustomerID: e.customer, ReceiptEntryID: receipt.ID,
		Items: []ledger.ItemRef{{FiscalYear: 2026, EntryID: entryOf(t, e.invoiced(t, e.customer, "100.00", "2026-03-10")), LineNo: 1}}}
	t.Run("the company's date now", func(t *testing.T) {
		h := endpoints(t)
		h.Now = func() time.Time { return time.Date(2026, 3, 25, 23, 30, 0, 0, time.UTC) }
		err := e.inRequest(t, func(ctx context.Context) error {
			v, err := h.ApplyPayment(ctx, in)
			// Company 2000 keeps UTC: the clock's day there.
			if err == nil && (v.PostingDate.Format(time.DateOnly) != "2026-03-25" || v.Amount != 10000 || len(v.Items) != 1 || v.Items[0].Amount != 10000) {
				t.Errorf("got %+v, want 100.00 applied to one item on 2026-03-25, the company's date", v)
			}
			return err
		})
		if err != nil {
			t.Errorf("got %v, want the payment applied", err)
		}
	})
	t.Run("a company the configuration lacks", func(t *testing.T) {
		h := endpoints(t)
		h.Settings = noCompany{}
		day := date(t, "2026-03-21")
		lacking := in
		lacking.PostingDate = &day
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ApplyPayment(ctx, lacking)
			return err
		})
		if p := problem.From(err); p.Code != "receivables.receipt_not_open" || p.Status != 422 {
			t.Errorf("apply: got %v, want 422 receivables.receipt_not_open", err)
		}
		err = e.inRequest(t, func(ctx context.Context) error {
			res, err := h.ListUnappliedReceipts(ctx, gen.ListUnappliedReceiptsParams{LegalEntityID: e.DE})
			if page, ok := res.(*gen.UnappliedReceiptPage); err == nil && (!ok || len(page.Items) != 0) {
				t.Errorf("receipts: got %+v, want an empty page", res)
			}
			return err
		})
		if err != nil {
			t.Errorf("list: got %v, want an empty page", err)
		}
	})
}
