package receivables_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
)

// entered enters a draft and returns its id.
func (e env) entered(t *testing.T, body string) string {
	t.Helper()
	got, _ := e.Must(201, "POST", "/v1/customer-invoices", body, "")
	return got["id"].(string)
}

// post posts the draft on the date and returns the invoice.
func (e env) post(t *testing.T, id, date string) map[string]any {
	t.Helper()
	got, _ := e.Must(200, "POST", "/v1/customer-invoices/"+id+":post", fmt.Sprintf(`{"posting_date": %q}`, date), `"1"`)
	return got
}

// line is a journal line as the tests compare it.
type line struct {
	Account string
	Amount  int64
	Partner *uuid.UUID
	TaxCode *string
}

// entryLines are the leading ledger's lines of the entry, by line number.
func (e env) entryLines(t *testing.T, entry any) []line {
	t.Helper()
	var out []line
	pgtest.Scan(t, `SELECT coalesce(json_agg(json_build_object('Account', a.number, 'Amount', l.amount_tc, 'Partner', l.partner_id, 'TaxCode', l.tax_code)
			ORDER BY l.line_no), '[]')
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE l.tenant_id = $1 AND l.journal_entry_id = $2`, []any{e.Tenant.ID, entry}, &out)
	return out
}

func show(lines []line) string {
	var b strings.Builder
	for _, l := range lines {
		code := "-"
		if l.TaxCode != nil {
			code = *l.TaxCode
		}
		fmt.Fprintf(&b, "%s %d %s %v; ", l.Account, l.Amount, code, l.Partner != nil)
	}
	return b.String()
}

// TestPostInvoice: posting moves the draft to posted with its entry: Dr
// the customer's reconciliation account with the customer, Cr revenue per
// item and output tax; the entry balances and carries source_context
// receivables; the debit line is the customer's open item with the
// customer's terms and net due date, in GET /v1/open-items and in the
// ledger's reader; the audit record and CustomerInvoicePosted are written
// (T75b acceptance).
func TestPostInvoice(t *testing.T) {
	e := setup(t)
	id := e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.10", "A1"), e.item("33.33", "A1"), itemOn(e.other, "10.00", "EUR", ""),
		e.item("5.00", "A0")))
	got := e.post(t, id, "2026-03-12")
	for f, want := range map[string]any{"status": "posted", "posting_date": "2026-03-12", "payment_term_code": "NT30", "net_due_date": "2026-04-09",
		"version": float64(2), "journal_entry_year": float64(2026)} {
		if got[f] != want {
			t.Errorf("%s: got %v, want %v", f, got[f], want)
		}
	}
	entry, _ := got["journal_entry_id"].(string)
	var number, docType, sourceContext, reference string
	var source uuid.UUID
	var taxDate time.Time
	pgtest.Scan(t, `SELECT document_number, document_type, source_context, source_document_id, reference, tax_date FROM ledger.journal_entry
		WHERE tenant_id = $1 AND id = $2`, []any{e.Tenant.ID, entry}, &number, &docType, &sourceContext, &source, &reference, &taxDate)
	if got["journal_entry_number"] != number || docType != "DR" || sourceContext != "receivables" || source.String() != id ||
		reference != got["document_number"] || taxDate.Format(time.DateOnly) != "2026-03-10" {
		t.Errorf("entry: got number %s (the invoice names %v), type %s, source %s %s, reference %s, tax date %s; want a DR entry of receivables for the invoice, "+
			"referring to %v, taxed on 2026-03-10", number, got["journal_entry_number"], docType, sourceContext, source, reference, taxDate.Format(time.DateOnly),
			got["document_number"])
	}
	lines := e.entryLines(t, entry)
	if got, want := show(lines), "120000 17378 - true; 400000 -10010 A1 false; 400000 -3333 A1 false; 530000 -1000 - false; 400000 -500 A0 false; "+
		"230000 -2535 A1 false; "; got != want {
		t.Errorf("lines: got %s, want %s", got, want)
	}
	var sum int64
	for _, l := range lines {
		sum += l.Amount
	}
	if sum != 0 {
		t.Errorf("the entry's lines: got a sum of %d, want 0", sum)
	}
	if lines[0].Partner == nil || *lines[0].Partner != e.customer {
		t.Errorf("the receivable line's partner: got %v, want the customer %s", lines[0].Partner, e.customer)
	}
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.customer.String(), "", "")
	listed, _ := page["items"].([]any)
	if len(listed) != 1 {
		t.Fatalf("the customer's open items: got %d, want 1", len(listed))
	}
	item, _ := listed[0].(map[string]any)
	if a := amount(t, item, "amount"); a != "173.78 EUR" || item["journal_entry_id"] != entry {
		t.Errorf("open item: got %s of entry %v, want 173.78 EUR of %s", a, item["journal_entry_id"], entry)
	}
	if terms := fmt.Sprint(item["payment_terms"]); terms != "map[baseline_date:2026-03-10 code:NT30 discounts:[] net_due_date:2026-04-09]" {
		t.Errorf("open item's terms: got %s, want NT30 from 2026-03-10, due 2026-04-09", terms)
	}
	var items []ledger.ReceivableItem
	if err := e.inRequest(t, func(ctx context.Context) (err error) {
		items, err = ledger.OpenItems{}.Receivable(ctx, e.DE, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Amount != 17378 || items[0].PartnerID != e.customer || items[0].SourceContext != "receivables" ||
		items[0].SourceDocumentID == nil || items[0].SourceDocumentID.String() != id {
		t.Errorf("the ledger's customer items: got %+v, want one of 173.78 for the customer, posted by receivables for the invoice", items)
	}
	read, tag := e.Must(200, "GET", "/v1/customer-invoices/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b || tag != `"2"` {
		t.Errorf("read back: got %s (%s), want %s (\"2\")", a, tag, b)
	}
	e.posted(t, id, got)
}

// posted checks the audit record and the event of a posted invoice.
func (e env) posted(t *testing.T, id string, got map[string]any) {
	t.Helper()
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'receivables.customer_invoice.post' AND object_id = $2`,
		[]any{e.Tenant.ID, id}, &audits)
	if audits != 1 {
		t.Errorf("audit records of the posting: got %d, want 1", audits)
	}
	var events int
	var payload map[string]any
	pgtest.Scan(t, `SELECT count(*) OVER (), payload FROM receivables.outbox WHERE tenant_id = $1 AND aggregate_id = $2
		AND event_type = 'com.example.erp.receivables.customer_invoice_posted.v1'`, []any{e.Tenant.ID, id}, &events, &payload)
	if events != 1 {
		t.Fatalf("CustomerInvoicePosted events: got %d, want 1", events)
	}
	want := map[string]any{"customer_invoice_id": id, "document_number": got["document_number"], "legal_entity_id": e.DE.String(),
		"legal_entity_code": "2000", "customer_id": e.customer.String(), "customer_number": "BP70000001", "kind": "invoice",
		"reference_invoice_id": nil, "reference_invoice_number": nil, "invoice_date": "2026-03-10", "posting_date": "2026-03-12", "net_amount": map[string]any{"amount": "148.43", "currency": "EUR"},
		"tax_amount":   map[string]any{"amount": "25.35", "currency": "EUR"},
		"gross_amount": map[string]any{"amount": "173.78", "currency": "EUR"}, "payment_term_code": "NT30", "net_due_date": "2026-04-09",
		"journal_entry_id": got["journal_entry_id"], "journal_entry_year": float64(2026), "journal_entry_number": got["journal_entry_number"],
		"status": "posted", "status_before": "draft", "version": float64(2)}
	if a, b := mustJSON(t, payload), mustJSON(t, want); a != b {
		t.Errorf("event payload: got %s, want %s", a, b)
	}
	var changed int
	pgtest.Scan(t, `SELECT count(*) FROM receivables.outbox WHERE tenant_id = $1 AND aggregate_id = $2
		AND event_type = 'com.example.erp.receivables.customer_invoice_changed.v1'`, []any{e.Tenant.ID, id}, &changed)
	if changed != 1 {
		t.Errorf("CustomerInvoiceChanged events: got %d, want the entry's alone", changed)
	}
}

// entries counts the tenant's journal entries.
func (e env) entries(t *testing.T) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	return n
}

// TestPostOnce: a draft posts once. A replay with the same
// Idempotency-Key answers the same; a second post is refused, as is one
// without or with a stale If-Match; two posts at once post one entry; and
// the ledger's :reverse refuses the entry (04 §4.9).
func TestPostOnce(t *testing.T) {
	e := setup(t)
	id := e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1")))
	path := "/v1/customer-invoices/" + id + ":post"
	e.Problem(428, "request.precondition_required", "POST", path, `{}`, "")
	e.Problem(412, "request.version_mismatch", "POST", path, `{}`, `"7"`)
	req := apitest.Req{Method: "POST", Path: path, Body: `{"posting_date": "2026-03-12"}`, Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(),
		IfMatch: `"1"`}
	first, second := e.API.Do(req), e.API.Do(req)
	if first.Code != 200 || second.Code != 200 {
		t.Fatalf("statuses: got %d and %d, want 200 twice: %s", first.Code, second.Code, second.Body)
	}
	a, b := apitest.JSON(t, first), apitest.JSON(t, second)
	if mustJSON(t, a) != mustJSON(t, b) || second.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay: got %v (Idempotent-Replayed %q), want the first answer %v replayed", b, second.Header().Get("Idempotent-Replayed"), a)
	}
	if n := e.entries(t); n != 1 {
		t.Errorf("journal entries after a replay: got %d, want 1", n)
	}
	e.Problem(409, "receivables.invalid_status", "POST", path, `{}`, `"2"`)
	if n := e.entries(t); n != 1 {
		t.Errorf("journal entries after a second post: got %d, want 1", n)
	}
	t.Run("the ledger does not reverse it", func(t *testing.T) {
		entry := a["journal_entry_id"].(string)
		_, tag := e.Must(200, "GET", "/v1/journal-entries/"+entry, "", "")
		e.Problem(409, "ledger.entry_owned_elsewhere", "POST", "/v1/journal-entries/"+entry+":reverse", `{}`, tag)
	})
	t.Run("two posts at once", func(t *testing.T) {
		other := e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("50.00", "A1")))
		before := e.entries(t)
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Go(func() {
				codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: "/v1/customer-invoices/" + other + ":post", Body: `{"posting_date": "2026-03-12"}`,
					Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"1"`}).Code
			})
		}
		wg.Wait()
		// The second waits for the first's row lock and then finds the
		// invoice at another version.
		slices.Sort(codes)
		if codes[0] != 200 || codes[1] != 412 {
			t.Errorf("statuses: got %v, want one 200 and one 412", codes)
		}
		if n := e.entries(t) - before; n != 1 {
			t.Errorf("journal entries of two posts at once: got %d, want 1", n)
		}
	})
}

// TestPostingTermsAndDate: the open item keeps the term entered with the
// invoice, else the customer's in the company; the posting date is the
// request's, else the invoice's, else the company's date now.
func TestPostingTermsAndDate(t *testing.T) {
	e := setup(t)
	with := func(body, field, value string) string {
		return strings.Replace(body, `"currency": "EUR", "items"`, fmt.Sprintf(`%q: %q, "currency": "EUR", "items"`, field, value), 1)
	}
	base := e.invoice(e.customer, "2026-03-10", e.item("100.00", ""))
	t.Run("the invoice's term", func(t *testing.T) {
		entered, _ := e.Must(201, "POST", "/v1/customer-invoices", with(base, "payment_term_code", "NT14"), "")
		if entered["payment_term_code"] != "NT14" || entered["net_due_date"] != nil {
			t.Errorf("the draft: got term %v and due date %v, want NT14 and none", entered["payment_term_code"], entered["net_due_date"])
		}
		got := e.post(t, entered["id"].(string), "2026-03-12")
		if got["payment_term_code"] != "NT14" || got["net_due_date"] != "2026-03-24" {
			t.Errorf("posted: got term %v due %v, want NT14 due 2026-03-24", got["payment_term_code"], got["net_due_date"])
		}
		var code string
		var due time.Time
		pgtest.Scan(t, `SELECT payment_term_code, baseline_date + net_days FROM ledger.open_item_terms WHERE tenant_id = $1 AND journal_entry_id = $2`,
			[]any{e.Tenant.ID, got["journal_entry_id"]}, &code, &due)
		if code != "NT14" || due.Format(time.DateOnly) != "2026-03-24" {
			t.Errorf("the open item's terms: got %s due %s, want NT14 due 2026-03-24", code, due.Format(time.DateOnly))
		}
	})
	t.Run("a term with a cash discount tier", func(t *testing.T) {
		// A term with a tier needs a cash_discount account in every company's
		// configuration (T36a), which this fixture has not: the term is written as
		// an activated configuration would write it.
		pgtest.Exec(t, `INSERT INTO partner.payment_term (tenant_id, code, description, net_days, discount_days, discount_pct, created_by, updated_by)
			VALUES ($1, 'SK14', '14 days 2%, 30 net', 30, 14, 2, $2, $2)`, e.Tenant.ID, e.Tenant.Human.ID)
		got := e.post(t, e.entered(t, with(base, "payment_term_code", "SK14")), "2026-03-12")
		page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.customer.String()+"&due_date_to=2026-04-09", "", "")
		var terms string
		for _, it := range page["items"].([]any) {
			if m := it.(map[string]any); m["journal_entry_id"] == got["journal_entry_id"] {
				terms = fmt.Sprint(m["payment_terms"])
			}
		}
		if want := "map[baseline_date:2026-03-10 code:SK14 discounts:[map[percent:2 until:2026-03-24]] net_due_date:2026-04-09]"; terms != want {
			t.Errorf("the open item's terms: got %s, want %s", terms, want)
		}
	})
	t.Run("an unknown term is refused at entry", func(t *testing.T) {
		p := e.Problem(422, "receivables.unknown_payment_term", "POST", "/v1/customer-invoices", with(base, "payment_term_code", "ZZ99"), "")
		if f := field(p); f != "payment_term_code" {
			t.Errorf("field: got %v, want payment_term_code", f)
		}
	})
	t.Run("the invoice's posting date", func(t *testing.T) {
		id := e.entered(t, with(base, "posting_date", "2026-03-20"))
		got, _ := e.Must(200, "POST", "/v1/customer-invoices/"+id+":post", "", `"1"`)
		if got["posting_date"] != "2026-03-20" {
			t.Errorf("posting date: got %v, want the invoice's 2026-03-20", got["posting_date"])
		}
	})
	t.Run("the company's date now", func(t *testing.T) {
		h := endpoints(t)
		h.Now = func() time.Time { return time.Date(2026, 3, 15, 23, 30, 0, 0, time.UTC) }
		id := uuid.MustParse(e.entered(t, base))
		err := e.inRequest(t, func(ctx context.Context) error {
			v, err := h.PostInvoice(ctx, id, func(int32) error { return nil }, nil)
			// Company 2000 keeps UTC: the clock's day there.
			if err == nil && v.PostingDate.Format(time.DateOnly) != "2026-03-15" {
				t.Errorf("posting date: got %s, want 2026-03-15, the company's date", v.PostingDate.Format(time.DateOnly))
			}
			return err
		})
		if err != nil {
			t.Errorf("got %v, want the invoice posted", err)
		}
	})
}

// TestPostRefusals: what posting refuses (T75b acceptance): a customer
// without company data in the company, blocked for posting there or no
// longer active; a closed period; tax that the codes no longer give; an
// invoice of a company the caller may not act in.
func TestPostRefusals(t *testing.T) {
	e := setup(t)
	bare := e.partner(t, "BP70000003", "Bare Ltd", "active", "customer")
	blocked := e.partner(t, "BP70000004", "Stopped Ltd", "active", "customer")
	gone := e.partner(t, "BP70000005", "Gone Ltd", "active", "customer")
	e.companyData(t, blocked, "NT30")
	e.companyData(t, gone, "NT30")
	draft := func(customer uuid.UUID, date string) string {
		return e.entered(t, e.invoice(customer, date, e.item("100.00", "A1")))
	}
	ids := map[string]string{"bare": draft(bare, "2026-03-10"), "blocked": draft(blocked, "2026-03-10"), "gone": draft(gone, "2026-03-10"),
		"closed": draft(e.customer, "2026-02-10"), "tax": draft(e.customer, "2026-03-10"), "other": draft(e.customer, "2026-03-10")}
	pgtest.Exec(t, `UPDATE partner.customer_company SET is_posting_blocked = true, version = version + 1 WHERE tenant_id = $1 AND partner_id = $2`,
		e.Tenant.ID, blocked)
	pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'blocked', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, gone)
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 2`,
		e.Tenant.ID)
	for _, c := range []struct {
		name, id, date string
		status         int
		code           string
	}{
		{"a customer without company data", ids["bare"], "2026-03-12", 422, "receivables.customer_not_usable"},
		{"a customer blocked for posting in the company", ids["blocked"], "2026-03-12", 422, "receivables.customer_not_usable"},
		{"a customer no longer active", ids["gone"], "2026-03-12", 422, "receivables.customer_not_usable"},
		{"a closed period", ids["closed"], "2026-02-12", 409, "ledger.period_not_open"},
		{"an invoice that does not exist", uuid.NewString(), "2026-03-12", 404, "request.not_found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := e.entries(t)
			e.In(t).Problem(c.status, c.code, "POST", "/v1/customer-invoices/"+c.id+":post", fmt.Sprintf(`{"posting_date": %q}`, c.date), `"1"`)
			if n := e.entries(t); n != before {
				t.Errorf("journal entries: got %d more, want none", n-before)
			}
			var status string
			if c.status != 404 {
				pgtest.Scan(t, `SELECT status FROM receivables.customer_invoice WHERE tenant_id = $1 AND id = $2`, []any{e.Tenant.ID, c.id}, &status)
				if status != "draft" {
					t.Errorf("the invoice: got %s, want it left a draft", status)
				}
			}
		})
	}
	t.Run("another company's invoice", func(t *testing.T) {
		reader := e.Reader(t, "us-only", e.US)
		e.As(reader).Problem(404, "request.not_found", "POST", "/v1/customer-invoices/"+ids["other"]+":post", `{}`, `"1"`)
	})
	t.Run("tax the codes no longer give", func(t *testing.T) {
		doc := fixture.Document(t)
		outputCodes("")(doc)
		a1 := doc.Company("2000").TaxCodes[slicesIndex(doc.Company("2000").TaxCodes, "A1")]
		a1.Components[0].Rates = []config.TaxRate{{RatePercent: "20", ValidFrom: "2026-01-01"}}
		fixture.TaxCodes(t, e.Tenant, doc, a1)
		p := e.Problem(409, "receivables.tax_changed", "POST", "/v1/customer-invoices/"+ids["tax"]+":post", `{"posting_date": "2026-03-12"}`, `"1"`)
		if d, _ := p["detail"].(string); !strings.Contains(d, "20.00 EUR") || !strings.Contains(d, "19.00") {
			t.Errorf("detail: got %q, want the tax now (20.00 EUR) and as entered (19.00)", d)
		}
	})
}

// slicesIndex is the index of the tax code in codes.
func slicesIndex(codes []config.TaxCode, code string) int {
	for i, c := range codes {
		if c.Code == code {
			return i
		}
	}
	return -1
}

// noAccount is a ledger whose output tax component has no account.
type noAccount struct{ ledger.Accounts }

func (noAccount) TaxCode(context.Context, uuid.UUID, string, time.Time) (ledger.TaxCode, bool, error) {
	return ledger.TaxCode{Code: "A1", Direction: config.TaxOutput, Components: []ledger.TaxComponent{{Key: "vat", AccountKey: config.KeyOutput,
		RecoverablePct: "100", RatePct: "19"}, {Key: "none", AccountKey: config.KeyOutput, RecoverablePct: "100", RatePct: "0"}}}, true, nil
}

// noCustomer is a partner context that does not know the customer.
type noCustomer struct{}

func (noCustomer) Customer(context.Context, uuid.UUID, uuid.UUID) (partner.Customer, bool, error) {
	return partner.Customer{}, false, nil
}

// noTerm is a partner context without the payment term.
type noTerm struct{}

func (noTerm) PaymentTerm(context.Context, string) (partner.PaymentTerm, bool, error) {
	return partner.PaymentTerm{}, false, nil
}

// TestPostThroughThePort: what only a port could answer at posting: a
// customer the partner context no longer finds is refused; a term that is
// not configured, which the foreign keys rule out, fails the post; a tax
// component without an account is refused; an item whose tax is out of
// range is refused.
func TestPostThroughThePort(t *testing.T) {
	e := setup(t)
	id := uuid.MustParse(e.entered(t, e.invoice(e.customer, "2026-03-10", e.item("100.00", "A1"))))
	post := func(h *receivables.Endpoints) error {
		day := date(t, "2026-03-12")
		return e.inRequest(t, func(ctx context.Context) error {
			_, err := h.PostInvoice(ctx, id, func(int32) error { return nil }, &day)
			return err
		})
	}
	t.Run("a customer the partner context does not find", func(t *testing.T) {
		h := endpoints(t)
		h.CustomerData = noCustomer{}
		if p := problem.From(post(h)); p.Code != "receivables.customer_not_usable" || p.Status != 422 {
			t.Errorf("got %v, want 422 receivables.customer_not_usable", p)
		}
	})
	t.Run("a term that is not configured", func(t *testing.T) {
		h := endpoints(t)
		h.Terms = noTerm{}
		err := post(h)
		var p *problem.Problem
		if err == nil || errors.As(err, &p) || !strings.Contains(err.Error(), "payment term NT30 is not configured") {
			t.Errorf("got %v, want an error that is no refusal, naming the term", err)
		}
	})
	t.Run("a tax component without an account", func(t *testing.T) {
		h := endpoints(t)
		h.Accounts = noAccount{}
		if p := problem.From(post(h)); p.Code != "receivables.no_tax_account" || p.Status != 422 {
			t.Errorf("got %v, want 422 receivables.no_tax_account", p)
		}
	})
	t.Run("tax out of range", func(t *testing.T) {
		// Two components of 60% of an amount near the range's end: each fits,
		// their sum does not. Entry refuses it, so the draft is written here.
		big := uuid.Must(uuid.NewV7())
		pgtest.Exec(t, `INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
				currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by)
			VALUES ($1, $2, 'CIBIG', $3, $4, 'invoice', '2026-03-10', 'EUR', 9000000000000000000, 0, 9000000000000000000, 'draft', $5, $5)`,
			e.Tenant.ID, big, e.DE, e.customer, e.Tenant.Human.ID)
		pgtest.Exec(t, `INSERT INTO receivables.customer_invoice_item (tenant_id, customer_invoice_id, item_no, account_id, net_amount, tax_code, tax_amount)
			VALUES ($1, $2, 1, $3, 9000000000000000000, 'A2', 0)`, e.Tenant.ID, big, e.revenue)
		h := endpoints(t)
		day := date(t, "2026-03-12")
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.PostInvoice(ctx, big, func(int32) error { return nil }, &day)
			return err
		})
		if p := problem.From(err); p.Code != "receivables.amount_too_large" || p.Status != 422 {
			t.Errorf("got %v, want 422 receivables.amount_too_large", err)
		}
	})
}
