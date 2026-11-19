package receivables_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
)

const reverseBecause = `{"reason": "Entered twice."}`

// reverse reverses the posted memo at version 2 and returns it.
func (e env) reverse(t *testing.T, id, body string) map[string]any {
	t.Helper()
	got, _ := e.Must(200, "POST", "/v1/customer-invoices/"+id+":reverse", body, `"2"`)
	return got
}

// TestReverseDebitMemo (T75k acceptance): a posted debit memo is reversed
// on its own posting date: the reversal entry is its entry with the signs
// turned, document type, reference and source kept, its tax items taken
// back; the memo's item and the reversal's are cleared and neither is
// open; the document is reversed with its reversal and reason, the event
// and the audit record carry no reason; a replay answers the same and
// the ledger's own reversal names this path.
func TestReverseDebitMemo(t *testing.T) {
	e := setup(t)
	memo := e.post(t, e.entered(t, e.debitMemo(e.customer, "2026-03-10", e.item("50.00", "A1"))), "2026-03-12")
	id, entry := memo["id"].(string), memo["journal_entry_id"].(string)
	t.Run("the ledger names this path", func(t *testing.T) {
		_, tag := e.Must(200, "GET", "/v1/journal-entries/"+entry, "", "")
		p := e.Problem(409, "ledger.entry_owned_elsewhere", "POST", "/v1/journal-entries/"+entry+":reverse", `{}`, tag)
		if d, _ := p["detail"].(string); !strings.Contains(d, "/v1/customer-invoices/"+id+":reverse") {
			t.Errorf("detail: got %q, want it to name the memo's reversal", d)
		}
	})
	req := apitest.Req{Method: "POST", Path: "/v1/customer-invoices/" + id + ":reverse", Body: `{"reason": "Freight charged twice."}`,
		Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"2"`}
	first, second := e.API.Do(req), e.API.Do(req)
	if first.Code != 200 || second.Code != 200 || second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("statuses: got %d and %d (replayed %q), want 200 and its replay: %s", first.Code, second.Code,
			second.Header().Get("Idempotent-Replayed"), second.Body)
	}
	got := apitest.JSON(t, first)
	if tag := first.Header().Get("ETag"); tag != `"3"` {
		t.Errorf("ETag: got %s, want \"3\"", tag)
	}
	fields(t, "the reversed memo", got, map[string]any{"status": "reversed", "kind": "debit_memo", "reversal_posting_date": "2026-03-12",
		"reversal_reason": "Freight charged twice.", "reversal_entry_year": 2026, "journal_entry_id": entry, "gross_amount": eur("59.50")})
	reversal := got["reversal_entry_id"].(string)
	var docType, source, text, number, reversalOf, sourceDoc string
	pgtest.Scan(t, `SELECT j.document_type, j.source_context, j.header_text, j.document_number, r.original_entry_id::text, j.source_document_id::text
		FROM ledger.journal_entry j JOIN ledger.reversal r ON r.tenant_id = j.tenant_id AND r.reversal_entry_id = j.id
		WHERE j.tenant_id = $1 AND j.id = $2`, []any{e.Tenant.ID, reversal}, &docType, &source, &text, &number, &reversalOf, &sourceDoc)
	fields(t, "the reversal entry", map[string]any{"document_type": docType, "source_context": source, "header_text": text, "number": number,
		"reversal_of": reversalOf, "source_document_id": sourceDoc}, map[string]any{"document_type": "DD", "source_context": "receivables",
		"header_text": "Reversal of customer debit memo " + memo["document_number"].(string), "number": got["reversal_entry_number"],
		"reversal_of": entry, "source_document_id": id})
	if got, want := show(e.entryLines(t, reversal)), "120000 -5950 - true; 400000 5000 A1 false; 230000 950 A1 false; "; got != want {
		t.Errorf("the reversal's lines: got %s, want %s", got, want)
	}
	var taxItems, tax int64
	pgtest.Scan(t, `SELECT count(*), coalesce(sum(tax_tc), 0)::bigint FROM ledger.tax_item WHERE tenant_id = $1 AND journal_entry_id IN ($2, $3)`,
		[]any{e.Tenant.ID, entry, reversal}, &taxItems, &tax)
	is(t, "the tax items of the memo and its reversal", fmt.Sprint(taxItems, " adding up to ", tax), "2 adding up to 0")
	is(t, "the customer's open items", len(e.openItems(t, e.customer)), 0)
	var cleared int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.clearing_item WHERE tenant_id = $1 AND journal_entry_id IN ($2, $3)`,
		[]any{e.Tenant.ID, entry, reversal}, &cleared)
	is(t, "the receivable lines cleared, the memo's and the reversal's", cleared, 2)
	event := e.outboxPayload(t, id, "customer_invoice_reversed")
	fields(t, "CustomerInvoiceReversed", event, map[string]any{"kind": "debit_memo", "gross_amount": eur("59.50"), "journal_entry_id": entry,
		"journal_entry_number": memo["journal_entry_number"], "reversal_entry_id": reversal, "reversal_entry_number": got["reversal_entry_number"],
		"reversal_posting_date": "2026-03-12", "status": "reversed", "status_before": "posted", "version": 3, "customer_number": "BP70000001",
		"reference_invoice_id": nil})
	if _, ok := event["reason"]; ok {
		t.Errorf("the event carries the reason: %v", event)
	}
	var audits int
	var detail string
	pgtest.Scan(t, `SELECT count(*), coalesce(max(detail::text), '') FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2
		AND action = 'receivables.customer_invoice.reverse'`, []any{e.Tenant.ID, id}, &audits, &detail)
	is(t, "audit records of the reversal", audits, 1)
	if strings.Contains(detail, "Freight") || !strings.Contains(detail, reversal) {
		t.Errorf("the audit detail: got %s, want the reversal entry and no reason", detail)
	}
	e.Problem(409, "receivables.invalid_status", "POST", "/v1/customer-invoices/"+id+":reverse", reverseBecause, `"3"`)
}

// TestReverseCreditMemoFreesItsCredit (T75k acceptance): a credit memo
// that took an invoice's whole gross is reversed on a later date it is
// asked for, and a memo the limit refused before posts after it.
func TestReverseCreditMemoFreesItsCredit(t *testing.T) {
	e := setup(t)
	invoice := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1"))), "2026-03-12")["id"].(string) // 119.00
	full := e.post(t, e.entered(t, e.memo(e.customer, invoice, "2026-03-14", e.item("100.00", "A1"))), "2026-03-14")
	more := e.entered(t, e.memo(e.customer, invoice, "2026-03-15", e.item("10.00", "")))
	e.Problem(422, "receivables.credit_exceeds_invoice", "POST", "/v1/customer-invoices/"+more+":post", `{"posting_date": "2026-03-15"}`, `"1"`)
	got := e.reverse(t, full["id"].(string), `{"reason": "Wrong invoice credited.", "posting_date": "2026-03-20"}`)
	fields(t, "the reversed credit memo", got, map[string]any{"status": "reversed", "reversal_posting_date": "2026-03-20"})
	if got, want := show(e.entryLines(t, got["reversal_entry_id"])), "120000 11900 - true; 400000 -10000 A1 false; 230000 -1900 A1 false; "; got != want {
		t.Errorf("the reversal's lines: got %s, want %s", got, want)
	}
	if posted := e.post(t, more, "2026-03-21"); posted["status"] != "posted" {
		t.Errorf("the memo refused before: got %v, want it posted", posted["status"])
	}
	fields(t, "CustomerInvoiceReversed", e.outboxPayload(t, full["id"].(string), "customer_invoice_reversed"), map[string]any{
		"kind": "credit_memo", "gross_amount": eur("-119.00"), "reference_invoice_id": invoice})
}

// TestReverseRefusals: what is not reversed is refused with its problem
// and nothing posts: a draft, an invoice, a memo sales entered, a reason
// of white space, a date before the memo posted, a memo whose item is
// cleared, a closed period, a stale or missing If-Match.
func TestReverseRefusals(t *testing.T) {
	e := setup(t)
	posted := func(body string) string { return e.post(t, e.entered(t, body), "2026-03-12")["id"].(string) }
	invoice := posted(e.invoice(e.customer, "2026-03-10", e.item("100.00", "")))
	cleared := posted(e.memo(e.customer, invoice, "2026-03-10", e.item("100.00", "")))
	e.Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-03-13", "items": [%s, %s]}`, e.DE,
		itemRef(e.journalEntry(t, invoice), 1), itemRef(e.journalEntry(t, cleared), 1)), "")
	fromSales := e.salesMemo(t)
	notice := uuid.Must(uuid.NewV7())
	fromDunning := e.sourced(t, receivables.KindDebitMemo, notice, notice.String()).String()
	memo := posted(e.debitMemo(e.customer, "2026-03-10", e.item("10.00", "")))
	closing := posted(e.debitMemo(e.customer, "2026-03-10", e.item("20.00", "")))
	path := func(id string) string { return "/v1/customer-invoices/" + id + ":reverse" }
	cases := []struct {
		name, id, body, tag, code, field string
		status                           int
	}{
		{"a draft", e.entered(t, e.debitMemo(e.customer, "2026-03-10", e.item("10.00", ""))), reverseBecause, `"1"`, "receivables.invalid_status", "", 409},
		{"an invoice", invoice, reverseBecause, `"2"`, "receivables.invoice_not_reversible", "", 409},
		{"a memo sales entered", fromSales, reverseBecause, `"2"`, "receivables.entered_from_source", "", 409},
		{"a debit memo a dunning run entered (T76h)", fromDunning, reverseBecause, `"2"`, "receivables.entered_from_source", "", 409},
		{"a reason of white space", memo, `{"reason": "  "}`, `"2"`, "request.validation_failed", "reason", 422},
		{"a date before the memo posted", memo, `{"reason": "Too early.", "posting_date": "2026-03-11"}`, `"2"`, "receivables.reversal_before_posting",
			"posting_date", 422},
		{"a memo cleared against its invoice", cleared, reverseBecause, `"2"`, "receivables.document_cleared", "", 409},
		{"a stale If-Match", memo, reverseBecause, `"1"`, "request.version_mismatch", "", 412},
		{"no If-Match", memo, reverseBecause, "", "request.precondition_required", "", 428},
	}
	before := e.entries(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := e.Problem(c.status, c.code, "POST", path(c.id), c.body, c.tag)
			if c.field != "" && field(p) != c.field {
				t.Errorf("the field refused: got %v, want %s", field(p), c.field)
			}
		})
	}
	t.Run("a dunning notice's memo names its source", func(t *testing.T) {
		p := e.Problem(409, "receivables.entered_from_source", "POST", path(fromDunning), reverseBecause, `"2"`)
		if detail, _ := p["detail"].(string); !strings.Contains(detail, "dunning notice "+notice.String()) {
			t.Errorf("detail: got %q, want it to name dunning notice %s", detail, notice)
		}
	})
	t.Run("into a closed period", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 3`,
			e.Tenant.ID)
		e.Problem(409, "ledger.period_not_open", "POST", path(closing), reverseBecause, `"2"`)
		got := e.reverse(t, closing, `{"reason": "Reversed in April.", "posting_date": "2026-04-01"}`)
		is(t, "the reversal into the open period", got["reversal_posting_date"], "2026-04-01")
	})
	is(t, "journal entries the refusals posted", e.entries(t)-before, 1)
	var reversed int
	pgtest.Scan(t, `SELECT count(*) FROM receivables.customer_invoice WHERE tenant_id = $1 AND status = 'reversed'`, []any{e.Tenant.ID}, &reversed)
	is(t, "documents reversed", reversed, 1)
}

// journalEntry is the posted document's journal entry.
func (e env) journalEntry(t *testing.T, id string) any {
	t.Helper()
	got, _ := e.Must(200, "GET", "/v1/customer-invoices/"+id, "", "")
	return got["journal_entry_id"]
}

// salesMemo is a posted credit memo another context entered from its
// document, as sales enters a billing cancellation's (T79b, T85d).
func (e env) salesMemo(t *testing.T) string {
	t.Helper()
	return e.sourced(t, receivables.KindCreditMemo, uuid.Must(uuid.NewV7()), "BC100001").String()
}

// sourced is a memo of the kind another context entered from its
// document, the source named, and posted on 12 March.
func (e env) sourced(t *testing.T, kind string, source uuid.UUID, number string) uuid.UUID {
	t.Helper()
	h := endpoints(t)
	var id uuid.UUID
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		in := receivables.Input{LegalEntityID: e.DE, CustomerID: e.customer, Kind: kind, InvoiceDate: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),
			Currency: "EUR", Items: []receivables.ItemInput{{AccountID: e.revenue, Net: receivables.Amount{Value: "5.00", Currency: "EUR"}}},
			Source: &receivables.Source{ID: source, Number: number}}
		v, err := h.CreateInvoice(ctx, in)
		if err != nil {
			return err
		}
		day := time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC)
		v, err = h.PostInvoice(ctx, v.ID, etag.IfMatch(`"1"`), &day)
		id = v.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestReverseConcurrently (T75k acceptance): two reversals of one memo
// race; the second waits for the first's row lock, finds the memo at
// another version, and one reversal is posted.
func TestReverseConcurrently(t *testing.T) {
	e := setup(t)
	id := e.post(t, e.entered(t, e.debitMemo(e.customer, "2026-03-10", e.item("10.00", ""))), "2026-03-12")["id"].(string)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: "/v1/customer-invoices/" + id + ":reverse", Body: reverseBecause,
				Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"2"`}).Code
		})
	}
	wg.Wait()
	slices.Sort(codes)
	if codes[0] != 200 || codes[1] != 412 {
		t.Errorf("statuses: got %v, want one 200 and one 412", codes)
	}
	var reversals int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.reversal r JOIN receivables.customer_invoice i ON i.tenant_id = r.tenant_id AND i.journal_entry_id = r.original_entry_id
		WHERE r.tenant_id = $1 AND i.id = $2`, []any{e.Tenant.ID, id}, &reversals)
	is(t, "reversals of the memo", reversals, 1)
}

// notCleared is the ledger's posting port answering that no entry is
// cleared: what this context read before a clearing committed.
type notCleared struct{ receivables.Postings }

func (notCleared) IsCleared(context.Context, int16, uuid.UUID) (bool, error) { return false, nil }

// TestReverseRacesAClearing: a clearing that committed after this context
// read the memo's item open refuses the reversal under the entry's lock
// (ledger.entry_cleared), and nothing changes.
func TestReverseRacesAClearing(t *testing.T) {
	e := setup(t)
	invoice := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("10.00", ""))), "2026-03-12")["id"].(string)
	memo := e.post(t, e.entered(t, e.memo(e.customer, invoice, "2026-03-10", e.item("10.00", ""))), "2026-03-12")["id"].(string)
	e.Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-03-13", "items": [%s, %s]}`, e.DE,
		itemRef(e.journalEntry(t, invoice), 1), itemRef(e.journalEntry(t, memo), 1)), "")
	h := endpoints(t)
	h.Postings = notCleared{h.Postings}
	err := e.inRequest(t, func(ctx context.Context) error {
		_, err := h.ReverseInvoice(ctx, uuid.MustParse(memo), etag.IfMatch(`"2"`), receivables.Reversal{Reason: "Raced."})
		return err
	})
	if p := problem.From(err); p.Code != "ledger.entry_cleared" {
		t.Errorf("got %v, want 409 ledger.entry_cleared", err)
	}
	got, _ := e.Must(200, "GET", "/v1/customer-invoices/"+memo, "", "")
	is(t, "the memo's status", got["status"], "posted")
}
