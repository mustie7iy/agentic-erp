package receivables_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// memo is the JSON of a credit memo of company 2000 in EUR to the
// customer, naming the invoice ("" for none).
func (e env) memo(customer uuid.UUID, invoice, date string, items ...string) string {
	kind := `"kind": "credit_memo", `
	if invoice != "" {
		kind += fmt.Sprintf(`"reference_invoice_id": %q, `, invoice)
	}
	return strings.Replace(e.invoice(customer, date, items...), `"invoice_date"`, kind+`"invoice_date"`, 1)
}

// outboxPayload is the one event of the type about the document.
func (e env) outboxPayload(t *testing.T, id, event string) map[string]any {
	t.Helper()
	var n int
	var payload map[string]any
	pgtest.Scan(t, `SELECT count(*) OVER (), payload FROM receivables.outbox WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3`,
		[]any{e.Tenant.ID, id, "com.example.erp.receivables." + event + ".v1"}, &n, &payload)
	if n != 1 {
		t.Fatalf("%s events of %s: got %d, want 1", event, id, n)
	}
	return payload
}

// fields checks each field of got that want names, one message a field;
// a field got lacks fails, whatever is wanted of it.
func fields(t *testing.T, what string, got, want map[string]any) {
	t.Helper()
	for f, w := range want {
		if g, ok := got[f]; !ok || fmt.Sprint(g) != fmt.Sprint(w) {
			t.Errorf("%s, %s: got %v, want %v", what, f, got[f], w)
		}
	}
}

// eur is an amount in EUR as a response or an event carries it.
func eur(amount string) map[string]any { return map[string]any{"amount": amount, "currency": "EUR"} }

// openItems are the customer's open items in GET /v1/open-items, as
// "amount currency" by entry.
func (e env) openItems(t *testing.T, customer uuid.UUID) map[string]string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+customer.String(), "", "")
	out := map[string]string{}
	for _, it := range items(t, page) {
		out[it["journal_entry_id"].(string)] = amount(t, it, "amount")
	}
	return out
}

// TestCreditMemo (T75d acceptance): a credit memo naming a posted invoice
// is entered as the invoice is, with its tax at its own date's rates; it
// posts with the signs turned, Cr the customer and Dr revenue and output
// tax, as a DG entry of receivables; invoice and memo are the customer's
// open items, net what the customer owes; the memo's item is due when its
// invoice is; the events carry the memo's amounts negative and name the
// invoice.
func TestCreditMemo(t *testing.T) {
	e := setup(t)
	invoice := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1"))), "2026-03-12")
	invoiceID := invoice["id"].(string)
	// A1 is 19% in March and 21% from July: the memo's tax is at its own date.
	got, tag := e.Must(201, "POST", "/v1/customer-invoices", e.memo(e.customer, invoiceID, "2026-07-05", e.item("50.00", "A1")), "")
	id := got["id"].(string)
	for f, want := range map[string]any{"status": "draft", "kind": "credit_memo", "reference_invoice_id": invoiceID,
		"reference_invoice_number": invoice["document_number"], "invoice_date": "2026-07-05"} {
		if got[f] != want {
			t.Errorf("%s: got %v, want %v", f, got[f], want)
		}
	}
	for f, want := range map[string]string{"net_amount": "50.00 EUR", "tax_amount": "10.50 EUR", "gross_amount": "60.50 EUR"} {
		if a := amount(t, got, f); a != want {
			t.Errorf("%s: got %s, want %s", f, a, want)
		}
	}
	entered := e.outboxPayload(t, id, "customer_invoice_changed")
	fields(t, "CustomerInvoiceChanged", entered, map[string]any{"kind": "credit_memo", "reference_invoice_id": invoiceID,
		"reference_invoice_number": invoice["document_number"], "net_amount": eur("-50.00"), "tax_amount": eur("-10.50"), "gross_amount": eur("-60.50")})
	if n := len(e.openItems(t, e.customer)); n != 1 {
		t.Errorf("open items before the memo posts: got %d, want the invoice's alone", n)
	}

	posted, _ := e.Must(200, "POST", "/v1/customer-invoices/"+id+":post", `{"posting_date": "2026-07-06"}`, tag)
	for f, want := range map[string]any{"status": "posted", "kind": "credit_memo", "posting_date": "2026-07-06", "payment_term_code": "NT30",
		"net_due_date": invoice["net_due_date"], "reference_invoice_number": invoice["document_number"]} {
		if posted[f] != want {
			t.Errorf("posted %s: got %v, want %v", f, posted[f], want)
		}
	}
	if a := amount(t, posted, "gross_amount"); a != "60.50 EUR" {
		t.Errorf("the posted memo's gross amount: got %s, want 60.50 EUR, never negative", a)
	}
	entry := posted["journal_entry_id"].(string)
	var docType, source, text string
	pgtest.Scan(t, `SELECT document_type, source_context, header_text FROM ledger.journal_entry WHERE tenant_id = $1 AND id = $2`,
		[]any{e.Tenant.ID, entry}, &docType, &source, &text)
	fields(t, "the memo's entry", map[string]any{"document_type": docType, "source_context": source, "header_text": text},
		map[string]any{"document_type": "DG", "source_context": "receivables", "header_text": "Customer credit memo " + posted["document_number"].(string)})
	lines := e.entryLines(t, entry)
	if got, want := show(lines), "120000 -6050 - true; 400000 5000 A1 false; 230000 1050 A1 false; "; got != want {
		t.Errorf("lines: got %s, want %s", got, want)
	}
	if lines[0].Partner == nil || *lines[0].Partner != e.customer {
		t.Errorf("the receivable line's partner: got %v, want the customer %s", lines[0].Partner, e.customer)
	}
	open := e.openItems(t, e.customer)
	if want := map[string]string{invoice["journal_entry_id"].(string): "119.00 EUR", entry: "-60.50 EUR"}; !maps.Equal(open, want) {
		t.Errorf("the customer's open items by entry: got %v, want %v, 58.50 owed", open, want)
	}
	var items []ledger.ReceivableItem
	if err := e.inRequest(t, func(ctx context.Context) (err error) {
		items, err = ledger.OpenItems{}.Receivable(ctx, e.DE, []uuid.UUID{e.customer})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var owed int64
	for _, it := range items {
		owed += int64(it.AmountLC)
		if due := it.NetDueDate(); it.Terms == nil || due.Format("2006-01-02") != "2026-04-09" {
			t.Errorf("item of entry %s: got due %s by terms %+v, want 2026-04-09 by terms: the memo is due when its invoice is", it.EntryID, due, it.Terms)
		}
	}
	if len(items) != 2 || owed != 5850 {
		t.Errorf("the ledger's customer items: got %d owing %d, want two owing 5850", len(items), owed)
	}
	event := e.outboxPayload(t, id, "customer_invoice_posted")
	fields(t, "CustomerInvoicePosted", event, map[string]any{"kind": "credit_memo", "reference_invoice_id": invoiceID,
		"reference_invoice_number": invoice["document_number"], "net_amount": eur("-50.00"), "tax_amount": eur("-10.50"), "gross_amount": eur("-60.50"),
		"payment_term_code": "NT30", "net_due_date": "2026-04-09"})
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND detail->>'kind' = 'credit_memo'
		AND action IN ('receivables.customer_invoice.create', 'receivables.customer_invoice.post')`, []any{e.Tenant.ID, id}, &audits)
	if audits != 2 {
		t.Errorf("audit records of the memo: got %d, want its entry's and its posting's", audits)
	}
}

// TestCreditMemoTerms: a memo's item keeps the term entered with the
// memo, counted from its date; a memo naming none the customer's from its
// own date.
func TestCreditMemoTerms(t *testing.T) {
	e := setup(t)
	invoice := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", ""))), "2026-03-12")
	cases := []struct {
		name, body, due string
	}{
		{"a term entered with a memo naming an invoice", strings.Replace(e.memo(e.customer, invoice["id"].(string), "2026-03-20", e.item("10.00", "")),
			`"currency"`, `"payment_term_code": "NT14", "currency"`, 1), "2026-04-03"},
		{"a memo naming none", e.memo(e.customer, "", "2026-03-20", e.item("10.00", "")), "2026-04-19"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.post(t, e.entered(t, c.body), "2026-03-21")
			if got["net_due_date"] != c.due {
				t.Errorf("net due date: got %v, want %s", got["net_due_date"], c.due)
			}
		})
	}
}

// TestCreditLimit (plan default 5; ADR-0063 §7): the posted credit memos
// naming an invoice may not exceed its gross amount. A memo that would is
// refused with the figures and stays a draft, which does not count; one
// that fills the gross exactly posts; a memo naming no invoice has no
// limit.
func TestCreditLimit(t *testing.T) {
	e := setup(t)
	invoice := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1"))), "2026-03-12") // 119.00 gross
	invoiceID := invoice["id"].(string)
	naming := func(net string) string {
		return e.entered(t, e.memo(e.customer, invoiceID, "2026-03-20", e.item(net, "")))
	}
	post := func(id string) string { return "/v1/customer-invoices/" + id + ":post" }
	e.post(t, naming("100.00"), "2026-03-21")
	over := naming("19.01")
	before := e.entries(t)
	p := e.Problem(422, "receivables.credit_exceeds_invoice", "POST", post(over), `{"posting_date": "2026-03-21"}`, `"1"`)
	if f := field(p); f != "gross_amount" {
		t.Errorf("the field refused: got %v, want gross_amount", f)
	}
	limit, ok := p["credit_limit"].(map[string]any)
	if !ok {
		t.Fatalf("credit_limit of the refusal: got %v, want the figures", p["credit_limit"])
	}
	if limit["invoice_id"] != invoiceID || limit["invoice_number"] != invoice["document_number"] {
		t.Errorf("credit_limit's invoice: got %v %v, want %s %v", limit["invoice_id"], limit["invoice_number"], invoiceID, invoice["document_number"])
	}
	for f, want := range map[string]string{"invoice_gross": "119.00 EUR", "credited": "100.00 EUR", "attempted": "19.01 EUR"} {
		if a := amount(t, limit, f); a != want {
			t.Errorf("credit_limit.%s: got %s, want %s", f, a, want)
		}
	}
	if n := e.entries(t); n != before {
		t.Errorf("journal entries after the refusal: got %d, want %d", n, before)
	}
	if got, _ := e.Must(200, "GET", "/v1/customer-invoices/"+over, "", ""); got["status"] != "draft" {
		t.Errorf("the refused memo: got %v, want it still a draft", got["status"])
	}
	// The refused draft does not count: exactly what is left posts.
	e.post(t, naming("19.00"), "2026-03-21")
	e.Problem(422, "receivables.credit_exceeds_invoice", "POST", post(naming("0.01")), `{"posting_date": "2026-03-21"}`, `"1"`)
	t.Run("a memo naming no invoice is unlimited", func(t *testing.T) {
		got := e.post(t, e.entered(t, e.memo(e.customer, "", "2026-03-20", e.item("5000.00", "A1"))), "2026-03-21")
		if got["status"] != "posted" || got["reference_invoice_id"] != nil {
			t.Errorf("got %v naming %v, want it posted, naming no invoice", got["status"], got["reference_invoice_id"])
		}
	})
	t.Run("two memos at once that together exceed the invoice", func(t *testing.T) {
		second := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", ""))), "2026-03-12")
		memos := []string{e.entered(t, e.memo(e.customer, second["id"].(string), "2026-03-20", e.item("60.00", ""))),
			e.entered(t, e.memo(e.customer, second["id"].(string), "2026-03-20", e.item("60.00", "")))}
		codes := make([]int, len(memos))
		var wg sync.WaitGroup
		for i, id := range memos {
			wg.Go(func() {
				codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: post(id), Body: `{"posting_date": "2026-03-21"}`, Tenant: e.Tenant.ID,
					As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"1"`}).Code
			})
		}
		wg.Wait()
		// The second waits for the invoice's row lock and then counts the first.
		slices.Sort(codes)
		if codes[0] != 200 || codes[1] != 422 {
			t.Errorf("statuses: got %v, want one 200 and one 422", codes)
		}
		var credited int64
		pgtest.Scan(t, `SELECT coalesce(sum(gross_amount), 0)::bigint FROM receivables.customer_invoice WHERE tenant_id = $1 AND reference_invoice_id = $2
			AND status = 'posted'`, []any{e.Tenant.ID, second["id"]}, &credited)
		if credited != 6000 {
			t.Errorf("credited against the invoice: got %d, want 6000 of its 10000", credited)
		}
	})
}

// TestCreditMemoRefusals: a kind that is not known, a reference on an
// invoice, and a reference that is no posted invoice of the same company
// and customer in the memo's currency are refused naming the request's
// field, and nothing is entered.
func TestCreditMemoRefusals(t *testing.T) {
	e := setup(t)
	other := e.partner(t, "BP70000002", "Contoso", "active", "customer")
	e.companyData(t, other, "NT30")
	posted := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", ""))), "2026-03-12")["id"].(string)
	draft := e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", "")))
	memo := e.post(t, e.entered(t, e.memo(e.customer, "", "2026-03-10", e.item("10.00", ""))), "2026-03-12")["id"].(string)
	usd := strings.ReplaceAll(e.memo(e.customer, posted, "2026-03-20", e.item("10.00", "")), "EUR", "USD")
	us := strings.Replace(e.memo(e.customer, posted, "2026-03-20", itemOn(e.Account(t, "1000", "400000"), "10.00", "EUR", "")), e.DE.String(), e.US.String(), 1)
	cases := []struct {
		name, body, code, field string
	}{
		{"an unknown kind", strings.Replace(e.memo(e.customer, "", "2026-03-20", e.item("10.00", "")), "credit_memo", "debit_memo", 1),
			"receivables.unknown_kind", "kind"},
		{"an invoice naming an invoice", strings.Replace(e.memo(e.customer, posted, "2026-03-20", e.item("10.00", "")), "credit_memo", "invoice", 1),
			"receivables.reference_not_allowed", "reference_invoice_id"},
		{"a draft named", e.memo(e.customer, draft, "2026-03-20", e.item("10.00", "")), "receivables.invalid_reference_invoice", "reference_invoice_id"},
		{"a credit memo named", e.memo(e.customer, memo, "2026-03-20", e.item("10.00", "")), "receivables.invalid_reference_invoice", "reference_invoice_id"},
		{"another customer's invoice", e.memo(other, posted, "2026-03-20", e.item("10.00", "")), "receivables.invalid_reference_invoice", "reference_invoice_id"},
		{"an invoice in another currency", usd, "receivables.invalid_reference_invoice", "reference_invoice_id"},
		{"another company's invoice", us, "receivables.invalid_reference_invoice", "reference_invoice_id"},
		{"an invoice that does not exist", e.memo(e.customer, uuid.NewString(), "2026-03-20", e.item("10.00", "")),
			"receivables.invalid_reference_invoice", "reference_invoice_id"},
	}
	var before int
	pgtest.Scan(t, `SELECT count(*) FROM receivables.customer_invoice WHERE tenant_id = $1`, []any{e.Tenant.ID}, &before)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := e.Problem(422, c.code, "POST", "/v1/customer-invoices", c.body, "")
			if f := field(p); f != c.field {
				t.Errorf("the field refused: got %v, want %s", f, c.field)
			}
		})
	}
	var after int
	pgtest.Scan(t, `SELECT count(*) FROM receivables.customer_invoice WHERE tenant_id = $1`, []any{e.Tenant.ID}, &after)
	if after != before {
		t.Errorf("documents after the refusals: got %d, want %d", after, before)
	}
}

// TestCreditMemoNetsAndClears (T75d acceptance; ADR-0063 §6, §7): a
// receipt applied to an invoice and its credit memo together, which equal
// it exactly, clears all three (T75c nets the memo); the invoice's gross
// stays the limit once it is paid; and an invoice credited in full clears
// against its memo through the ledger's clearing (createClearing), no
// receipt involved.
func TestCreditMemoNetsAndClears(t *testing.T) {
	e := setup(t)
	item := func(doc map[string]any) string { return itemRef(doc["journal_entry_id"], 1) }
	t.Run("a receipt applied to an invoice and its credit memo", func(t *testing.T) {
		invoice := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1"))), "2026-03-12") // 119.00
		memo := e.post(t, e.entered(t, e.memo(e.customer, invoice["id"].(string), "2026-03-14", e.item("19.00", ""))), "2026-03-14")
		receipt := e.receipt(t, 10000, "2026-03-15")
		// The invoice alone is not the receipt: nothing is applied in part.
		e.Problem(422, "ledger.clearing_not_balanced", "POST", "/v1/incoming-payments", e.payment(e.customer, receipt.ID, "2026-03-15", item(invoice)), "")
		got, _ := e.Must(201, "POST", "/v1/incoming-payments", e.payment(e.customer, receipt.ID, "2026-03-15", item(invoice), item(memo)), "")
		if a := amount(t, got, "amount"); a != "100.00 EUR" {
			t.Errorf("the payment's amount: got %s, want 100.00 EUR", a)
		}
		applied := items(t, got)
		if len(applied) != 2 {
			t.Fatalf("the items applied: got %d, want the invoice and the memo", len(applied))
		}
		fields(t, "the invoice applied", applied[0], map[string]any{"amount": eur("119.00"), "reference": invoice["document_number"]})
		fields(t, "the memo applied", applied[1], map[string]any{"amount": eur("-19.00"), "reference": memo["document_number"]})
		if open := e.openItems(t, e.customer); len(open) != 0 {
			t.Errorf("the customer's open items: got %v, want none", open)
		}
		// The limit is the invoice's gross, not what is open of it: paid, it is credited still.
		late := e.post(t, e.entered(t, e.memo(e.customer, invoice["id"].(string), "2026-03-16", e.item("100.00", ""))), "2026-03-16")
		if late["status"] != "posted" {
			t.Errorf("a credit of the paid invoice's remaining gross: got %v, want it posted", late["status"])
		}
		// That credit clears against a further invoice of its amount, as any open credit item does.
		e.Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-03-16", "items": [%s, %s]}`, e.DE, item(late),
			e.invoiced(t, e.customer, "100.00", "2026-03-16")), "")
	})
	t.Run("an invoice credited in full clears against its memo", func(t *testing.T) {
		invoice := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1"))), "2026-03-12")
		memo := e.post(t, e.entered(t, e.memo(e.customer, invoice["id"].(string), "2026-03-14", e.item("100.00", "A1"))), "2026-03-14")
		if open := e.openItems(t, e.customer); len(open) != 2 {
			t.Fatalf("the customer's open items: got %v, want the invoice and the memo", open)
		}
		got, _ := e.Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-03-15", "items": [%s, %s]}`, e.DE,
			item(invoice), item(memo)), "")
		if n := len(items(t, got)); n != 2 {
			t.Errorf("the clearing's items: got %d, want 2", n)
		}
		if open := e.openItems(t, e.customer); len(open) != 0 {
			t.Errorf("the customer's open items: got %v, want none", open)
		}
	})
}

// TestListCreditMemos: the list filters by kind and by the invoice a
// credit memo names.
func TestListCreditMemos(t *testing.T) {
	e := setup(t)
	invoice := e.post(t, e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", ""))), "2026-03-12")["id"].(string)
	named := e.entered(t, e.memo(e.customer, invoice, "2026-03-20", e.item("10.00", "")))
	free := e.entered(t, e.memo(e.customer, "", "2026-03-21", e.item("10.00", "")))
	for query, want := range map[string][]string{
		"kind=credit_memo":                             {named, free},
		"kind=invoice":                                 {invoice},
		"reference_invoice_id=" + invoice:              {named},
		"reference_invoice_id=" + free:                 {},
		"kind=invoice&reference_invoice_id=" + invoice: {},
	} {
		t.Run(query, func(t *testing.T) {
			page, _ := e.Must(200, "GET", "/v1/customer-invoices?order_by=invoice_date&"+query, "", "")
			got := []string{}
			for _, it := range items(t, page) {
				got = append(got, it["id"].(string))
			}
			if !slices.Equal(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}
