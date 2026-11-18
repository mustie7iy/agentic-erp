package collections_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// promiseOf is the JSON of a promise of the customer to pay amount (EUR
// unless the amount names its currency) by the date, for line 1 of each
// entry.
func promiseOf(company, customer uuid.UUID, amount, by string, entries ...ledger.Entry) string {
	if !strings.Contains(amount, " ") {
		amount += " EUR"
	}
	value, code, _ := strings.Cut(amount, " ")
	items := make([]string, len(entries))
	for i, en := range entries {
		items[i] = fmt.Sprintf(`{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}`, en.ID)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "promised_amount": {"amount": %q, "currency": %q}, "promised_date": %q, "items": [%s]}`,
		company, customer, value, code, by, strings.Join(items, ", "))
}

// promise records a promise and returns it.
func (e env) promise(t *testing.T, body string) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", "/v1/promises-to-pay", body, "")
	return got
}

// promisedBack are a run's promised items as "reference promise date", by
// customer and item.
func promisedBack(t *testing.T, run map[string]any) string {
	t.Helper()
	var out []string
	for _, p := range objects(t, run, "promised_items") {
		out = append(out, fmt.Sprintf("%v %v %v", p["reference"], p["promise_to_pay_id"], p["promised_date"]))
	}
	return strings.Join(out, ", ")
}

// worklistRow is the customer's row of the worklist at the key date.
func (e env) worklistRow(t *testing.T, key string, customer uuid.UUID) map[string]any {
	t.Helper()
	got, _ := e.Must(200, "GET", "/v1/collection-worklist?legal_entity_id="+e.DE.String()+"&key_date="+key, "", "")
	for _, c := range objects(t, got, "items") {
		if c["customer_id"] == customer.String() {
			return c
		}
	}
	t.Fatalf("the worklist at %s: got no row of %s", key, customer)
	return nil
}

// TestPromisesToPay (T76f acceptance): a promise reads back open with
// what it names; a run at a key date up to its date leaves its items out
// of the notices and totals and names them with the promise, a run after
// it duns them; the worklist shows the open promise before its date and
// the broken one after it; once the items are cleared the promise is
// kept; an item promised once is not promised again while the date has
// not passed; a proposal with an item promised since is stale. Each
// promise is audited.
func TestPromisesToPay(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	got, tag := e.Must(201, "POST", "/v1/promises-to-pay", promiseOf(e.DE, e.north, "120.00", lastHeld, entries[0], entries[1]), "")
	id := apitest.String(t, got, "id")
	if g, w := fields(t, got, "status", "customer_number", "customer_name", "company_code", "promised_amount", "promised_date", "paid_amount",
		"created_by_name", "version"), "status open, customer_number BP70000002, customer_name Northwind Traders, company_code 2000, "+
		"promised_amount 120.00 EUR, promised_date "+lastHeld+", paid_amount 0.00 EUR, created_by_name human, version 1"; g != w {
		t.Errorf("the promise: got %s, want %s", g, w)
	}
	items := objects(t, got, "items")
	if g, w := len(items), 2; g != w {
		t.Fatalf("items: got %d, want %d", g, w)
	}
	if g, w := fields(t, items[0], "journal_entry_id", "document_number", "reference", "net_due_date", "amount", "local_amount", "open"),
		"journal_entry_id "+entries[0].ID.String()+", document_number "+entries[0].DocumentNumber+", reference CI-A, net_due_date 2026-03-31, "+
			"amount 100.00 EUR, local_amount 100.00 EUR, open true"; g != w {
		t.Errorf("the first item: got %s, want %s", g, w)
	}
	if read, readTag := e.Must(200, "GET", "/v1/promises-to-pay/"+id, "", ""); mustJSON(t, read) != mustJSON(t, got) || readTag != tag || tag != `"1"` {
		t.Errorf("read back: got %s (%s), want %s (%s, version 1)", mustJSON(t, read), readTag, mustJSON(t, got), tag)
	}

	started, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	if g, w := promisedBack(t, started), "CI-A "+id+" "+lastHeld+", CI-B "+id+" "+lastHeld; g != w {
		t.Errorf("promised items: got %q, want %q", g, w)
	}
	if g, w := fields(t, started, "notice_count", "overdue_amount", "promised_item_count", "promised_amount", "blocked_item_count"),
		"notice_count 1, overdue_amount 160.00 EUR, promised_item_count 2, promised_amount 150.00 EUR, blocked_item_count 0"; g != w {
		t.Errorf("the run: got %s, want %s", g, w)
	}
	if g := e.proposed(t, apitest.String(t, started, "id")); g != "CI-USD 0->1" {
		t.Errorf("proposed: got %q, want CI-USD alone", g)
	}
	if g, w := fields(t, e.worklistRow(t, keyDate, e.north), "promised_amount", "open_promises", "next_promised_date", "broken_promises"),
		"promised_amount 120.00 EUR, open_promises 1, next_promised_date "+lastHeld+", broken_promises 0"; g != w {
		t.Errorf("Northwind on the worklist: got %s, want %s", g, w)
	}
	e.Problem(409, "collections.promise_exists", "POST", "/v1/promises-to-pay", promiseOf(e.DE, e.north, "10.00", lastHeld, entries[1]), "")

	t.Run("after the date the items are dunned and the promise is broken", func(t *testing.T) {
		later, _ := e.In(t).Must(201, "POST", "/v1/dunning-runs", run(e.DE, "2100-12-31"), "")
		if g := promisedBack(t, later); g != "" {
			t.Errorf("promised items: got %q, want none", g)
		}
		if g, w := fields(t, e.In(t).worklistRow(t, "2100-12-31", e.north), "promised_amount", "open_promises", "next_promised_date", "broken_promises"),
			"promised_amount 0.00 EUR, open_promises 0, next_promised_date <nil>, broken_promises 1"; g != w {
			t.Errorf("Northwind on the worklist: got %s, want %s", g, w)
		}
	})
	t.Run("a proposal with an item promised since is stale", func(t *testing.T) {
		run, runTag := e.In(t).start(t, keyDate)
		e.In(t).promise(t, promiseOf(e.DE, e.south, "100.00", lastHeld, entries[7]))
		p := e.In(t).Problem(409, "collections.proposal_stale", "POST", "/v1/dunning-runs/"+run+":confirm", "", runTag)
		if d, _ := p["detail"].(string); !strings.Contains(d, "of its 1 items, 0 cleared, 0 dunned by another run, 0 blocked from dunning and 1 promised") {
			t.Errorf("detail: got %q, want Southwind's item promised", d)
		}
	})

	// Northwind pays CI-A and CI-B: both are cleared with the payment, and the promise is kept.
	payment := e.post(t, item{customer: e.north, amount: -15000, currency: "EUR", day: "2026-07-01", noTerms: true, ref: "PAYMENT"})[0]
	e.Must(201, "POST", "/v1/clearings", fmt.Sprintf(`{"legal_entity_id": %q, "clearing_date": "2026-07-01", "items": [
		{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}, {"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1},
		{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}]}`, e.DE, entries[0].ID, entries[1].ID, payment.ID), "")
	kept, _ := e.Must(200, "GET", "/v1/promises-to-pay/"+id, "", "")
	if g, w := fields(t, kept, "status", "paid_amount"), "status kept, paid_amount 150.00 EUR"; g != w {
		t.Errorf("the paid promise: got %s, want %s", g, w)
	}
	if g := fmt.Sprint(objects(t, kept, "items")[0]["open"], objects(t, kept, "items")[1]["open"]); g != "false false" {
		t.Errorf("the paid items open: got %s, want false false", g)
	}
	if g, w := fields(t, e.worklistRow(t, "2100-12-31", e.north), "broken_promises"), "broken_promises 0"; g != w {
		t.Errorf("Northwind on the worklist once paid, after the date: got %s, want %s", g, w)
	}
	if g, w := fields(t, e.worklistRow(t, keyDate, e.north), "promised_amount", "open_promises"), "promised_amount 0.00 EUR, open_promises 0"; g != w {
		t.Errorf("Northwind on the worklist once paid, before the date: got %s, want %s", g, w)
	}
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'collections.promise_to_pay.record'`, []any{e.Tenant.ID}, &audits)
	if audits != 2 {
		t.Errorf("audit records: got %d, want 2", audits)
	}
}

// TestPromiseLists: the promises are listed the latest first, page by
// page, filtered by company and customer.
func TestPromiseLists(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	first := apitest.String(t, e.promise(t, promiseOf(e.DE, e.north, "100.00", lastHeld, entries[0])), "id")
	second := apitest.String(t, e.promise(t, promiseOf(e.DE, e.south, "100.00", lastHeld, entries[7])), "id")
	page, _ := e.Must(200, "GET", "/v1/promises-to-pay?page_size=1", "", "")
	next := apitest.String(t, page, "next_page_token")
	rest, _ := e.Must(200, "GET", "/v1/promises-to-pay?page_size=1&page_token="+next, "", "")
	if a, b := apitest.String(t, objects(t, page, "items")[0], "id"), apitest.String(t, objects(t, rest, "items")[0], "id"); a != second || b != first {
		t.Errorf("pages of one: got %s then %s, want %s then %s", a, b, second, first)
	}
	if g := len(objects(t, objects(t, rest, "items")[0], "items")); g != 1 {
		t.Errorf("the second page's promise: got %d items, want 1", g)
	}
	for _, c := range []struct{ name, query, want string }{
		{"all", "", second + " " + first},
		{"the company's", "?legal_entity_id=" + e.DE.String(), second + " " + first},
		{"Northwind's", "?customer_id=" + e.north.String(), first},
		{"another company's", "?legal_entity_id=" + uuid.NewString(), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			page, _ := e.In(t).Must(200, "GET", "/v1/promises-to-pay"+c.query, "", "")
			var ids []string
			for _, p := range objects(t, page, "items") {
				ids = append(ids, apitest.String(t, p, "id"))
			}
			if g := strings.Join(ids, " "); g != c.want {
				t.Errorf("got %s, want %s", g, c.want)
			}
		})
	}
}

// TestPromiseRefusals: an amount in another currency, of zero, above
// what the items amount to or with the wrong decimals; a date before
// today or after the latest key date; a customer that is no business
// partner; an item that is a credit, another customer's or named twice;
// and a company the caller may not act in are refused, each naming its
// field; an unknown promise is not found.
func TestPromiseRefusals(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	body := promiseOf(e.DE, e.north, "150.00", lastHeld, entries[0], entries[1])
	for _, c := range []struct {
		name, body, code, field string
	}{
		{"an amount in another currency", promiseOf(e.DE, e.north, "150.00 USD", lastHeld, entries[0]), "collections.currency_mismatch",
			"promised_amount.currency"},
		{"an amount of zero", promiseOf(e.DE, e.north, "0.00", lastHeld, entries[0]), "collections.invalid_promised_amount", "promised_amount.amount"},
		{"more than the items", promiseOf(e.DE, e.north, "150.01", lastHeld, entries[0], entries[1]), "collections.invalid_promised_amount",
			"promised_amount.amount"},
		{"a date before today", strings.Replace(body, lastHeld, "2000-01-01", 1), "collections.invalid_promised_date", "promised_date"},
		{"a date after the latest key date", strings.Replace(body, lastHeld, "2101-01-01", 1), "collections.invalid_promised_date", "promised_date"},
		{"a customer that is no business partner", strings.Replace(body, e.north.String(), uuid.NewString(), 1), "request.reference_not_found",
			"customer_id"},
		{"a credit item", promiseOf(e.DE, e.north, "1.00", lastHeld, entries[0], entries[5]), "collections.item_not_open", "items[1].journal_entry_id"},
		{"another customer's item", promiseOf(e.DE, e.north, "1.00", lastHeld, entries[7]), "collections.item_not_open", "items[0].journal_entry_id"},
		{"an item twice", promiseOf(e.DE, e.north, "1.00", lastHeld, entries[0], entries[0]), "request.validation_failed", "items[1]"},
		{"a company the caller may not act in", strings.Replace(body, e.DE.String(), uuid.NewString(), 1), "request.reference_not_found",
			"legal_entity_id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := e.In(t).Problem(422, c.code, "POST", "/v1/promises-to-pay", c.body, "")
			if !strings.Contains(mustJSON(t, p["errors"]), `"field":"`+c.field+`"`) {
				t.Errorf("errors: got %v, want one naming %s", p["errors"], c.field)
			}
		})
	}
	e.Problem(400, "request.invalid_amount", "POST", "/v1/promises-to-pay", promiseOf(e.DE, e.north, "1.5", lastHeld, entries[0]), "")
	e.Problem(404, "request.not_found", "GET", "/v1/promises-to-pay/"+uuid.NewString(), "", "")
}

// TestPromiseStatusByTheClock: a promise's date is judged against the
// company's today by the endpoints' clock (today itself is accepted, the
// day before refused naming today), and its status too: open up to its
// date, broken the day after while its items are open; a zone that does
// not load fails recording and reading, as no refusal.
func TestPromiseStatusByTheClock(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	h := endpoints(t)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	h.Now = func() time.Time { return now }
	in := collections.PromiseInput{LegalEntityID: e.DE, CustomerID: e.north, Amount: "70.00", Currency: "EUR",
		Items: []ledger.ItemRef{{FiscalYear: 2026, EntryID: entries[2].ID, LineNo: 1}}}
	record := func(by string) (collections.Promise, error) {
		var p collections.Promise
		err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
			in.Date = date(t, by)
			p, err = h.RecordPromise(ctx, in)
			return err
		})
		return p, err
	}
	if _, err := record("2026-06-30"); !strings.Contains(problem.From(err).Detail, "2026-07-01") {
		t.Errorf("the day before today: got %v, want collections.invalid_promised_date naming 2026-07-01", err)
	}
	p, err := record("2026-07-01")
	if err != nil {
		t.Fatalf("today: got %v, want the promise recorded", err)
	}
	read := func(t *testing.T) (gen.PromiseToPay, error) {
		t.Helper()
		var out gen.PromiseToPay
		err := e.inRequest(t, func(ctx context.Context) error {
			got, err := h.GetPromiseToPay(ctx, gen.GetPromiseToPayParams{ID: p.ID})
			if err == nil {
				out = got.(*gen.GetPromiseToPayOKHeaders).Response
			}
			return err
		})
		return out, err
	}
	for _, c := range []struct {
		name, now, status string
	}{
		{"on the date", "2026-07-01T21:59:00Z", collections.PromiseOpen},
		{"the day after", "2026-07-02T22:00:00Z", collections.PromiseBroken},
	} {
		t.Run(c.name, func(t *testing.T) {
			now, _ = time.Parse(time.RFC3339, c.now)
			got, err := read(t)
			if err != nil || got.Status != c.status {
				t.Errorf("got %v (%v), want %s", got.Status, err, c.status)
			}
		})
	}
	h.Settings = badZone{h.Settings}
	var refusal *problem.Problem
	if _, err := record("2026-07-03"); err == nil || errors.As(err, &refusal) {
		t.Errorf("recording in a zone that does not load: got %v, want an error that is no refusal", err)
	}
	if _, err := read(t); err == nil || errors.As(err, &refusal) {
		t.Errorf("reading in a zone that does not load: got %v, want an error that is no refusal", err)
	}
}

// TestPromisesRace: two promises of one item recorded at once wait for
// each other on the customer's lock: the second sees the first and is
// refused (no write skew past the check).
func TestPromisesRace(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	h := endpoints(t)
	promise := func(ctx context.Context) error {
		_, err := h.RecordPromise(ctx, collections.PromiseInput{LegalEntityID: e.DE, CustomerID: e.north, Amount: "10.00", Currency: "EUR",
			Date: date(t, lastHeld), Items: []ledger.ItemRef{{FiscalYear: 2026, EntryID: entries[0].ID, LineNo: 1}}})
		return err
	}
	first, second := apitest.Race(t, e.Tenant, e.Tenant.Human, e.Tenant.Human, promise, promise)
	if p := problem.From(second); first != nil || p.Code != "collections.promise_exists" {
		t.Errorf("got %v and %v, want the first recorded and the second refused as collections.promise_exists", first, second)
	}
}

// hugeItems are open items whose amounts in the company's currency are
// each a quarter of what an amount holds: what the ledger's booked
// amounts never are.
type hugeItems struct{ ledger.OpenItems }

func (h hugeItems) ReceivableItem(ctx context.Context, legalEntity uuid.UUID, ref ledger.ItemRef) (ledger.ReceivableItem, bool, error) {
	it, ok, err := h.OpenItems.ReceivableItem(ctx, legalEntity, ref)
	it.AmountLC = money.Minor(1) << 62
	return it, ok, err
}

// TestPromisesThroughThePort: what only a port could answer: items whose
// amounts add up to more than an amount holds are refused; a Go caller
// naming no item is refused as the contract would.
func TestPromisesThroughThePort(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	h := endpoints(t)
	h.OpenItems = hugeItems{}
	err := e.inRequest(t, func(ctx context.Context) error {
		_, err := h.RecordPromise(ctx, collections.PromiseInput{LegalEntityID: e.DE, CustomerID: e.north, Amount: "1.00", Currency: "EUR",
			Date: date(t, lastHeld), Items: []ledger.ItemRef{{FiscalYear: 2026, EntryID: entries[0].ID, LineNo: 1}, {FiscalYear: 2026, EntryID: entries[1].ID, LineNo: 1}}})
		return err
	})
	if p := problem.From(err); p.Code != "collections.amount_too_large" {
		t.Errorf("got %v, want collections.amount_too_large", err)
	}
	t.Run("a Go caller naming no item", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := endpoints(t).RecordPromise(ctx, collections.PromiseInput{LegalEntityID: e.DE, CustomerID: e.north, Amount: "1.00", Currency: "EUR",
				Date: date(t, lastHeld)})
			return err
		})
		if p := problem.From(err); p.Code != "request.validation_failed" {
			t.Errorf("got %v, want request.validation_failed", err)
		}
	})
}
