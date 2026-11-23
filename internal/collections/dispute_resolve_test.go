package collections_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
)

// resolvingPerms are what resolving a case and checking its documents
// take beside perms: entering and posting customer invoices, reading
// them and the journal, and resolving cases.
var resolvingPerms = []string{"collections.dispute.resolve", "receivables.invoice.read", "receivables.invoice.write", "receivables.invoice.post",
	"ledger.entry.read"}

// resolving gives company 2000, beside its dunning, what customer invoices
// and their credit memos post with (output tax A1 at 19% on 230000, the
// ranges journal_dr and journal_dg) and the payment differences account
// 690000 a write-off debits.
func resolving(doc *config.Config) {
	dunning(doc)
	resolvable(doc)
}

// resolvable is resolving without the dunning levels, for a
// configuration that sets them up its own way.
func resolvable(doc *config.Config) {
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "230000", Name: "Output tax", Type: config.Liability},
		config.Account{Number: "690000", Name: "Payment differences", Type: config.Expense})
	de.AccountDetermination.PaymentDifferenceExpense = "690000"
	for _, y := range fixture.Years() {
		doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "2000", DocumentType: "journal_dr", FiscalYear: y, First: 100000, Last: 999999},
			config.NumberRange{Company: "2000", DocumentType: "journal_dg", FiscalYear: y, First: 100000, Last: 999999})
	}
	de.TaxCodes = append(de.TaxCodes, config.TaxCode{Code: "A1", Description: "Output tax A1", Direction: config.TaxOutput,
		Components: []config.TaxComponent{{Key: "vat", AccountKey: config.KeyOutput, Account: "230000", Rates: []config.TaxRate{{RatePercent: "19", ValidFrom: "2026-01-01"}}}}})
}

// setupResolving is setup with resolving's configuration, the permissions
// it takes and Northwind set up in company 2000 on 120000 with NT30.
func setupResolving(t *testing.T) env {
	t.Helper()
	return setupResolvingWith(t, resolving)
}

// setupResolvingWith is setupResolving with the configuration as
// configure leaves it.
func setupResolvingWith(t *testing.T, configure func(*config.Config)) env {
	t.Helper()
	f := fixture.NewWith(t, nil, configure, append(perms, resolvingPerms...)...)
	e := env{Env: f, receivable: f.Account(t, "2000", "120000"), revenue: f.Account(t, "2000", "400000")}
	e.north = e.customer(t, "BP70000002", "Northwind Traders")
	e.south = e.customer(t, "BP70000001", "Southwind Ltd")
	e.east = e.customer(t, "BP70000003", "Eastwind AG")
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.8, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, e.Tenant.ID, e.north, e.DE, e.receivable, e.Tenant.Human.ID)
	return e
}

// invoiceFor enters and posts a customer invoice of company 2000 to
// Northwind on 10 March 2026 with the items, each "net code" on 400000
// ("-" for no code), and returns the posted invoice.
func (e env) invoiceFor(t *testing.T, items ...string) map[string]any {
	t.Helper()
	list := make([]string, len(items))
	for i, it := range items {
		f := strings.Fields(it)
		code := ""
		if f[1] != "-" {
			code = fmt.Sprintf(`, "tax_code": %q`, f[1])
		}
		list[i] = fmt.Sprintf(`{"account_id": %q, "net_amount": {"amount": %q, "currency": "EUR"}%s}`, e.revenue, f[0], code)
	}
	draft, tag := e.Must(201, "POST", "/v1/customer-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "invoice_date": "2026-03-10",
		"currency": "EUR", "items": [%s]}`, e.DE, e.north, strings.Join(list, ", ")), "")
	posted, _ := e.Must(200, "POST", "/v1/customer-invoices/"+apitest.String(t, draft, "id")+":post", `{"posting_date": "2026-03-10"}`, tag)
	return posted
}

// caseOn opens a case over the invoices' items, each "entry amount", and
// returns its id and entity tag.
func (e env) caseOn(t *testing.T, items ...string) (id, tag string) {
	t.Helper()
	list := make([]string, len(items))
	for i, it := range items {
		list[i] = it + " EUR"
	}
	return e.open(t, disputeItems(e, list, ""))
}

// memo is a credit memo as "kind reference gross = net + tax, items" with
// its source.
func (e env) memo(t *testing.T, id string) string {
	t.Helper()
	m, _ := e.Must(200, "GET", "/v1/customer-invoices/"+id, "", "")
	var items []string
	for _, it := range objects(t, m, "items") {
		items = append(items, fmt.Sprintf("%s+%s", amount(t, it, "net_amount"), amount(t, it, "tax_amount")))
	}
	return fmt.Sprintf("%v %v %v %s = %s + %s, %s; %v", m["kind"], m["status"], m["reference_invoice_number"], amount(t, m, "gross_amount"),
		amount(t, m, "net_amount"), amount(t, m, "tax_amount"), strings.Join(items, " "), m["source_document_number"])
}

// TestResolveByCreditMemo: a case over two invoices is accepted in part,
// one invoice in full and the other in half; each gets a credit memo
// naming it, its items credited in proportion, net and tax alike; the
// case closes as accepted with what it accepted and the memos, audited
// and published; it is resolved once (T86d acceptance).
func TestResolveByCreditMemo(t *testing.T) {
	e := setupResolving(t)
	a := e.invoiceFor(t, "100.00 A1")            // 119.00
	b := e.invoiceFor(t, "200.00 A1", "50.00 -") // 288.00
	c := e.invoiceFor(t, "10.00 -", "0.01 -")    // 10.01, its second item too small to share in 1.00
	d := e.invoiceFor(t, "5.00 -")
	invoices := []map[string]any{a, b, c, d}
	id, tag := e.caseOn(t, apitest.String(t, a, "journal_entry_id")+" 59.50", apitest.String(t, b, "journal_entry_id")+" 144.00",
		apitest.String(t, c, "journal_entry_id")+" 1.00", apitest.String(t, d, "journal_entry_id")+" 5.00")
	body := `{"resolution": "credit_memo", "reason": "Late delivery", "posting_date": "2026-06-30", "items": [
		{"item_no": 1, "accepted_amount": {"amount": "59.50", "currency": "EUR"}}, {"item_no": 2, "accepted_amount": {"amount": "72.00", "currency": "EUR"}},
		{"item_no": 3, "accepted_amount": {"amount": "1.00", "currency": "EUR"}}]}`
	got, tag := e.Must(200, "POST", "/v1/dispute-cases/"+id+":resolve", body, tag)
	if c := fmt.Sprintf("%v %v %s %s %v %v", got["status"], got["resolution"], amount(t, got, "accepted_amount"), amount(t, got, "local_accepted_amount"),
		got["posting_date"], got["close_reason"]); c != "accepted credit_memo 132.50 EUR 132.50 EUR 2026-06-30 Late delivery" {
		t.Errorf("the case: got %s, want accepted by credit memo, 132.50 EUR on 2026-06-30", c)
	}
	// Each item's accepted amount and credit memo: the invoice it credits, its gross, net and tax, its items and its source.
	want := []string{"59.50 EUR: credit_memo posted %s 59.50 EUR = 50.00 EUR + 9.50 EUR, 50.00 EUR+9.50 EUR; %s/1",
		"72.00 EUR: credit_memo posted %s 72.00 EUR = 62.50 EUR + 9.50 EUR, 50.00 EUR+9.50 EUR 12.50 EUR+0.00 EUR; %s/2",
		"1.00 EUR: credit_memo posted %s 1.00 EUR = 1.00 EUR + 0.00 EUR, 1.00 EUR+0.00 EUR; %s/3", "0.00 EUR: no credit memo%.0s%.0s"}
	for i, it := range objects(t, got, "items") {
		memo := amount(t, it, "accepted_amount") + ": no credit memo"
		if m, ok := it["credit_memo_id"]; ok {
			memo = amount(t, it, "accepted_amount") + ": " + e.memo(t, fmt.Sprint(m))
		}
		if w := fmt.Sprintf(want[i], apitest.String(t, invoices[i], "document_number"), id); memo != w {
			t.Errorf("item %d: got %q, want %q", i+1, memo, w)
		}
	}
	read, _ := e.Must(200, "GET", "/v1/dispute-cases/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	events := e.caseEvents(t)
	last := events[len(events)-1]
	if r := apitest.Object(t, last, "resolution"); last["change"] != "resolved" || r["kind"] != "credit_memo" || r["write_off_journal_entry_id"] != nil {
		t.Errorf("the resolved event: got %v", last)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND action = 'collections.dispute_case.resolve'
		AND NOT detail ? 'reason' AND detail->>'reason_given' = 'true'`, []any{e.Tenant.ID, id}, &audited)
	if audited != 1 {
		t.Errorf("audit records of the resolution without the reason: got %d, want 1", audited)
	}
	t.Run("resolved once", func(t *testing.T) {
		e.In(t).Problem(409, "collections.dispute_closed", "POST", "/v1/dispute-cases/"+id+":resolve", `{"resolution": "write_off", "reason": "x"}`, tag)
	})
}

// TestResolveByWriteOff: a case on two items is written off in full: one
// entry debits the payment differences account and credits each item's
// account with the customer, at the item's booked rate; the case names
// the entry; a credit memo of an item that is no customer invoice is
// refused first, leaving the case open (T86d acceptance).
func TestResolveByWriteOff(t *testing.T) {
	e := setupResolving(t)
	entries := e.post(t, item{customer: e.south, amount: 20000, currency: "USD", day: "2026-03-31", ref: "CI-USD"},
		item{customer: e.south, amount: 1000, currency: "USD", day: "2026-04-30", ref: "CI-USD2"})
	id, tag := e.open(t, disputeItems(e, []string{entries[0].ID.String() + " 50.00 USD", entries[1].ID.String() + " 10.00 USD"}, ""))
	got := e.Problem(422, "collections.no_invoice", "POST", "/v1/dispute-cases/"+id+":resolve", `{"resolution": "credit_memo", "reason": "x"}`, tag)
	if f := fieldOf(t, got); f != "resolution" {
		t.Errorf("field: got %q, want resolution", f)
	}
	resolved, _ := e.Must(200, "POST", "/v1/dispute-cases/"+id+":resolve", `{"resolution": "write_off", "reason": "Not worth it", "posting_date": "2026-06-30",
		"items": [{"item_no": 1, "accepted_amount": {"amount": "50.00", "currency": "USD"}}]}`, tag)
	if c := fmt.Sprintf("%v %s %s %v", resolved["resolution"], amount(t, resolved, "accepted_amount"), amount(t, resolved, "local_accepted_amount"),
		resolved["write_off_journal_entry_year"]); c != "write_off 50.00 USD 40.00 EUR 2026" {
		t.Errorf("the case: got %s, want written off, 50.00 USD at 40.00 EUR as booked", c)
	}
	entry, _ := e.Must(200, "GET", "/v1/journal-entries/"+apitest.String(t, resolved, "write_off_journal_entry_id"), "", "")
	numbers := map[any]string{e.receivable.String(): "120000", e.Account(t, "2000", "690000").String(): "690000"}
	var lines []string
	for _, l := range objects(t, entry, "lines") {
		lines = append(lines, fmt.Sprintf("%v %s %s %v", numbers[l["account_id"]], amount(t, l, "amount"), amount(t, l, "amount_lc"), l["partner_id"] != nil))
	}
	if c, want := fmt.Sprintf("%v %v %v %v", entry["document_type"], entry["source_context"], entry["document_number"] == resolved["write_off_journal_entry_number"],
		strings.Join(lines, ", ")), "SA collections true 690000 50.00 USD 40.00 EUR false, 120000 -50.00 USD -40.00 EUR true"; c != want {
		t.Errorf("the write-off: got %s, want %s", c, want)
	}
	for _, it := range objects(t, resolved, "items") {
		if _, ok := it["credit_memo_id"]; ok {
			t.Errorf("item %v of a write-off: got a credit memo %v", it["item_no"], it["credit_memo_id"])
		}
	}
}

// TestResolveRefusals: what a resolution must be; a refusal by
// receivables or the ledger leaves the case open and posts nothing (T86d
// acceptance).
func TestResolveRefusals(t *testing.T) {
	e := setupResolving(t)
	a := e.invoiceFor(t, "100.00 A1")
	ea := apitest.String(t, a, "journal_entry_id")
	b := e.invoiceFor(t, "10.00 -")
	id, tag := e.caseOn(t, ea+" 59.50", apitest.String(t, b, "journal_entry_id")+" 10.00")
	before := e.openItems(t)
	accept := func(items string) string {
		return `{"resolution": "credit_memo", "reason": "x", "items": [` + items + `]}`
	}
	for _, r := range []struct {
		name, body, code, field string
		status                  int
	}{
		{"an unknown resolution", `{"resolution": "refund", "reason": "x"}`, "collections.unknown_resolution", "resolution", 422},
		{"an unknown item", accept(`{"item_no": 3, "accepted_amount": {"amount": "1.00", "currency": "EUR"}}`), "collections.unknown_item", "items[0].item_no", 422},
		{"an item named twice", accept(`{"item_no": 1, "accepted_amount": {"amount": "1.00", "currency": "EUR"}}, {"item_no": 1, "accepted_amount": {"amount": "1.00", "currency": "EUR"}}`),
			"collections.unknown_item", "items[1].item_no", 422},
		{"another currency", accept(`{"item_no": 1, "accepted_amount": {"amount": "1.00", "currency": "USD"}}`), "collections.currency_mismatch",
			"items[0].accepted_amount.currency", 422},
		{"more than disputed", accept(`{"item_no": 1, "accepted_amount": {"amount": "59.51", "currency": "EUR"}}`), "collections.invalid_accepted_amount",
			"items[0].accepted_amount.amount", 422},
		{"an amount that is none", accept(`{"item_no": 1, "accepted_amount": {"amount": "1.001", "currency": "EUR"}}`), "request.invalid_amount",
			"items[0].accepted_amount.amount", 400},
		{"nothing accepted", accept(`{"item_no": 1, "accepted_amount": {"amount": "0.00", "currency": "EUR"}}`), "collections.invalid_accepted_amount", "items", 422},
		{"a reason of white space", `{"resolution": "write_off", "reason": " "}`, "request.validation_failed", "reason", 422},
		{"a date in no period", `{"resolution": "write_off", "reason": "x", "posting_date": "2099-01-31"}`, "ledger.no_fiscal_period", "", 422},
		{"a credit memo in no period", `{"resolution": "credit_memo", "reason": "x", "posting_date": "2099-01-31"}`, "ledger.no_fiscal_period", "", 422},
	} {
		t.Run(r.name, func(t *testing.T) {
			got := e.In(t).Problem(r.status, r.code, "POST", "/v1/dispute-cases/"+id+":resolve", r.body, tag)
			if r.field != "" {
				if f := fieldOf(t, got); f != r.field {
					t.Errorf("field: got %q, want %q", f, r.field)
				}
			}
		})
	}
	if after := e.openItems(t); after != before {
		t.Errorf("open items after the refusals: got %s, want them as before: %s", after, before)
	}
	t.Run("more than the invoice has left to credit", func(t *testing.T) {
		draft, dtag := e.In(t).Must(201, "POST", "/v1/customer-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "kind": "credit_memo",
			"reference_invoice_id": %q, "invoice_date": "2026-03-20", "currency": "EUR", "items": [{"account_id": %q, "net_amount": {"amount": "60.00",
			"currency": "EUR"}, "tax_code": "A1"}]}`, e.DE, e.north, apitest.String(t, a, "id"), e.revenue), "")
		e.In(t).Must(200, "POST", "/v1/customer-invoices/"+apitest.String(t, draft, "id")+":post", `{"posting_date": "2026-03-20"}`, dtag)
		got := e.In(t).Problem(422, "receivables.credit_exceeds_invoice", "POST", "/v1/dispute-cases/"+id+":resolve",
			`{"resolution": "credit_memo", "reason": "x", "posting_date": "2026-06-30"}`, tag)
		if d := fmt.Sprint(got["detail"]); !strings.HasPrefix(d, "Item 1's credit memo") {
			t.Errorf("detail: got %q, want it led by the item's credit memo", d)
		}
	})
	still, _ := e.Must(200, "GET", "/v1/dispute-cases/"+id, "", "")
	if still["status"] != "open" || still["version"] != float64(1) {
		t.Errorf("after the refusals: got %v at version %v, want the case open as it was", still["status"], still["version"])
	}
}

// TestResolveWithoutAnAccount: a company without the payment differences
// account is refused a write-off.
func TestResolveWithoutAnAccount(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	id, tag := e.open(t, dispute(e.DE, entries[0], "10.00", "EUR", ""))
	e.Tenant.Grant(t, "collections.dispute.resolve", e.Tenant.Human)
	e.Problem(422, "collections.no_write_off_account", "POST", "/v1/dispute-cases/"+id+":resolve", `{"resolution": "write_off", "reason": "x"}`, tag)
}

// TestResolveRace: of two resolutions of one case at once, the second
// waits for the first and finds it closed.
func TestResolveRace(t *testing.T) {
	e := setupResolving(t)
	entries := e.post(t, item{customer: e.north, amount: 10000, currency: "EUR", day: "2026-03-01", ref: "CI-A"})
	id, _ := e.open(t, dispute(e.DE, entries[0], "10.00", "EUR", ""))
	h := endpoints(t)
	resolve := func(ctx context.Context) error {
		_, err := h.ResolveCase(ctx, uuid.MustParse(id), func(int32) error { return nil }, collections.ResolutionInput{Kind: collections.WriteOff, Reason: "x"})
		return err
	}
	first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, resolve, resolve)
	if p := problem.From(second); first != nil || p.Code != "collections.dispute_closed" {
		t.Errorf("got %v and %v, want the first resolved and the second refused as collections.dispute_closed", first, second)
	}
}

// TestResolveFailures calls the resolution outside a unit of work and
// with each of its database statements failing in turn, by credit memo
// and by write-off.
func TestResolveFailures(t *testing.T) {
	e := setupResolving(t)
	inv := e.invoiceFor(t, "100.00 A1")
	id, _ := e.caseOn(t, apitest.String(t, inv, "journal_entry_id")+" 10.00")
	h := endpoints(t)
	for _, kind := range []string{collections.CreditMemo, collections.WriteOff} {
		call := func(ctx context.Context) error {
			_, err := h.ResolveDisputeCase(ctx, &gen.DisputeCaseResolve{Resolution: kind, Reason: "x", PostingDate: gen.NewOptDate(date(t, "2026-06-30"))},
				gen.ResolveDisputeCaseParams{ID: uuid.MustParse(id), IfMatch: `"1"`})
			return err
		}
		t.Run(kind, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, kind, call)
		})
	}
}

// TestResolveThroughThePort: what only a port could answer: an item whose
// entry names a document receivables does not know, or a customer invoice
// that is not a posted invoice, is no invoice to credit; a failing port is
// an error.
func TestResolveThroughThePort(t *testing.T) {
	e := setupResolving(t)
	inv := e.invoiceFor(t, "100.00 A1")
	id, _ := e.caseOn(t, apitest.String(t, inv, "journal_entry_id")+" 10.00")
	resolve := func(change func(*collections.Endpoints)) error {
		h := endpoints(t)
		change(h)
		return e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ResolveCase(ctx, uuid.MustParse(id), func(int32) error { return nil }, collections.ResolutionInput{Kind: collections.CreditMemo, Reason: "x"})
			return err
		})
	}
	for name, c := range map[string]struct {
		change func(*collections.Endpoints)
		want   string
	}{
		"a document receivables does not know": {func(h *collections.Endpoints) { h.Resolving.Sources = otherSource{} }, "collections.no_invoice"},
		"an entry of no receivables document":  {func(h *collections.Endpoints) { h.Resolving.Sources = noSource{} }, "collections.no_invoice"},
		"no company day":                       {func(h *collections.Endpoints) { h.Settings = noSettings{} }, ""},
		"a draft":                              {func(h *collections.Endpoints) { h.Invoices = drafts{h.Invoices} }, "collections.no_invoice"},
		"no source":                            {func(h *collections.Endpoints) { h.Resolving.Sources = failing{} }, ""},
		"no invoice":                           {func(h *collections.Endpoints) { h.Invoices = failingInvoices{h.Invoices} }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := resolve(c.change)
			if p := problem.From(err); c.want != "" && p.Code != c.want {
				t.Errorf("got %v, want %s", err, c.want)
			}
			if c.want == "" && (err == nil || problem.From(err).Status != 500) {
				t.Errorf("got %v, want an error", err)
			}
		})
	}
}

// otherSource names a document receivables does not know for every entry.
type otherSource struct{}

func (otherSource) Sources(_ context.Context, _ string, entries []ledger.EntryRef) (map[ledger.EntryRef]uuid.UUID, error) {
	out := map[ledger.EntryRef]uuid.UUID{}
	for _, e := range entries {
		out[e] = uuid.Must(uuid.NewV7())
	}
	return out, nil
}

// noSource names no document for any entry: a manual entry's, for one.
type noSource struct{}

func (noSource) Sources(context.Context, string, []ledger.EntryRef) (map[ledger.EntryRef]uuid.UUID, error) {
	return map[ledger.EntryRef]uuid.UUID{}, nil
}

// noSettings fails to read the configuration.
type noSettings struct{}

func (noSettings) Document(context.Context) (*config.Config, int32, error) {
	return nil, 0, fmt.Errorf("configuration unavailable")
}

// failing fails to name sources.
type failing struct{}

func (failing) Sources(context.Context, string, []ledger.EntryRef) (map[ledger.EntryRef]uuid.UUID, error) {
	return nil, fmt.Errorf("sources unavailable")
}

// drafts reads every invoice as a draft.
type drafts struct{ collections.Invoices }

func (d drafts) Invoice(ctx context.Context, id uuid.UUID) (receivables.Invoice, error) {
	v, err := d.Invoices.Invoice(ctx, id)
	v.Status = "draft"
	return v, err
}

// failingInvoices fails to read an invoice.
type failingInvoices struct{ collections.Invoices }

func (failingInvoices) Invoice(context.Context, uuid.UUID) (receivables.Invoice, error) {
	return receivables.Invoice{}, fmt.Errorf("invoices unavailable")
}

// TestCreditMemosInParts: an invoice of two taxed items with odd amounts
// credited in full by five cases' credit memos, one after the other: each
// memo's gross is exactly what its case accepted, together the invoice's
// gross, and the tax they give back, rounded once per memo (ADR-0049 §8),
// is within a minor unit per memo of the tax the invoice bore (ADR-0065
// As built T86d).
func TestCreditMemosInParts(t *testing.T) {
	e := setupResolving(t)
	inv := e.invoiceFor(t, "10.03 A1", "7.77 A1")
	if g := amount(t, inv, "gross_amount"); g != "21.18 EUR" {
		t.Fatalf("the invoice: got %s, want 21.18 EUR", g)
	}
	entry := apitest.String(t, inv, "journal_entry_id")
	var gross, tax money.Minor
	parts := []string{"2.11", "2.11", "2.11", "2.11", "12.74"}
	for _, part := range parts {
		id, tag := e.caseOn(t, entry+" "+part)
		got, _ := e.Must(200, "POST", "/v1/dispute-cases/"+id+":resolve", `{"resolution": "credit_memo", "reason": "x", "posting_date": "2026-06-30"}`, tag)
		memo, _ := e.Must(200, "GET", "/v1/customer-invoices/"+apitest.String(t, objects(t, got, "items")[0], "credit_memo_id"), "", "")
		if g := amount(t, memo, "gross_amount"); g != part+" EUR" {
			t.Errorf("the memo of %s: got gross %s, want exactly what was accepted", part, g)
		}
		gross += minor(t, memo, "gross_amount")
		tax += minor(t, memo, "tax_amount")
	}
	if want := minor(t, inv, "gross_amount"); gross != want {
		t.Errorf("the memos' gross together: got %d, want the invoice's %d", gross, want)
	}
	if bore, d := minor(t, inv, "tax_amount"), money.Minor(len(parts)); tax < bore-d || tax > bore+d {
		t.Errorf("the memos' tax together: got %d, want within %d of the %d the invoice bore", tax, d, bore)
	}
}

// minor is an amount field in minor units of a currency of two digits.
func minor(t *testing.T, m map[string]any, field string) money.Minor {
	t.Helper()
	v, err := money.ParseDecimal(fmt.Sprint(apitest.Object(t, m, field)["amount"]), 2)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestWriteOffOfADunningCharge: an item of a debit memo a dunning run
// posted its charges with is not written off, before or after the run is
// cancelled, since the cancellation reverses the memo and the write-off
// leaves its item open: the customer would get the charge back twice;
// nothing is posted and the case stays open; the run is cancelled all
// the same (batch 82 integration review, ADR-0065 As built T86d).
func TestWriteOffOfADunningCharge(t *testing.T) {
	e := setupResolvingWith(t, func(doc *config.Config) { posting(doc); resolvable(doc) })
	for _, c := range []uuid.UUID{e.south, e.east} {
		e.companyData(t, c)
	}
	e.story(t)
	run, tag := e.start(t, keyDate)
	e.confirm(t, run, tag)
	var entry uuid.UUID
	var line int32
	pgtest.Scan(t, `SELECT c.journal_entry_id, l.line_no FROM collections.dunning_charge c JOIN ledger.journal_line l
		ON l.tenant_id = c.tenant_id AND l.fiscal_year = c.journal_entry_year AND l.journal_entry_id = c.journal_entry_id AND l.partner_id IS NOT NULL
		WHERE c.tenant_id = $1 AND c.dunning_run_id = $2`, []any{e.Tenant.ID, run}, &entry, &line)
	body := strings.Replace(disputeItems(e, []string{entry.String() + " 5.00 EUR"}, ""), `"line_no": 1`, fmt.Sprintf(`"line_no": %d`, line), 1)
	id, caseTag := e.open(t, body)
	for _, when := range []string{"before the cancellation", "after it"} {
		t.Run(when, func(t *testing.T) {
			p := e.Problem(422, "collections.dunning_charge_write_off", "POST", "/v1/dispute-cases/"+id+":resolve", `{"resolution": "write_off", "reason": "x"}`, caseTag)
			if f := fieldOf(t, p); f != "resolution" {
				t.Errorf("field: got %q, want resolution", f)
			}
			c, _ := e.Must(200, "GET", "/v1/dispute-cases/"+id, "", "")
			if c["status"] != "open" {
				t.Errorf("the case: got %v, want open", c["status"])
			}
		})
		if when == "before the cancellation" {
			e.cancel(t, run, e.tagOf(t, run))
		}
	}
	if got := e.charges(t, run); got != "BP70000001 reversed" {
		t.Errorf("the run's memo: got %q, want it reversed by the cancellation alone", got)
	}
}

// TestDisputedAtMostWhatIsOwed (tracker leaf, src review:batch90): a
// resolution leaves the item open beside its credit, so what is still
// owed of an item is its amount less what resolved cases accepted of it.
// An invoice item of EUR 100.00 written off in full by one case is not
// written off again by a second (422 collections.invalid_disputed_amount
// naming the amount); one written off by 60.00 and credited by a memo of
// 10.00 is disputed again for at most 30.00, and a case closed as
// rejected accepted nothing.
func TestDisputedAtMostWhatIsOwed(t *testing.T) {
	e := setupResolving(t)
	whole := e.invoiceFor(t, "100.00 -")
	id, tag := e.caseOn(t, fmt.Sprint(whole["journal_entry_id"])+" 100.00")
	e.Must(200, "POST", "/v1/dispute-cases/"+id+":resolve", `{"resolution": "write_off", "reason": "Uncollectable", "posting_date": "2026-06-30"}`, tag)
	p := e.Problem(422, "collections.invalid_disputed_amount", "POST", "/v1/dispute-cases",
		disputeItems(e, []string{fmt.Sprint(whole["journal_entry_id"]) + " 100.00 EUR"}, ""), "")
	if f := fieldOf(t, p); f != "items[0].disputed_amount.amount" {
		t.Errorf("the field: got %s, want items[0].disputed_amount.amount", f)
	}
	part := e.invoiceFor(t, "100.00 -")
	entry := fmt.Sprint(part["journal_entry_id"])
	for _, resolution := range []string{`"write_off"`, `"credit_memo"`} {
		amount := map[string]string{`"write_off"`: "60.00", `"credit_memo"`: "10.00"}[resolution]
		id, tag := e.caseOn(t, entry+" "+amount)
		e.Must(200, "POST", "/v1/dispute-cases/"+id+":resolve", `{"resolution": `+resolution+`, "reason": "Accepted", "posting_date": "2026-06-30"}`, tag)
	}
	rejected, tag := e.caseOn(t, entry+" 30.00")
	e.Must(200, "POST", "/v1/dispute-cases/"+rejected+":close", `{"outcome": "rejected", "reason": "Priced as quoted"}`, tag)
	e.Problem(422, "collections.invalid_disputed_amount", "POST", "/v1/dispute-cases", disputeItems(e, []string{entry + " 30.01 EUR"}, ""), "")
	e.open(t, disputeItems(e, []string{entry + " 30.00 EUR"}, ""))
}
