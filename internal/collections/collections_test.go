package collections_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// perms are what the tests' person holds: dunning, the worklist, and
// clearing a customer's item in the ledger as an incoming payment does.
var perms = []string{"collections.dunning.read", "collections.dunning.run", "collections.worklist.read", "ledger.clearing.post"}

// keyDate is the key date of the tests' runs.
const keyDate = "2026-06-30"

// env is a configured tenant whose company 2000 (EUR) has the customer
// reconciliation account 120000 and the dunning levels 1 from 5 days
// overdue, 2 from 20 and 3 from 40; company 1000 has none. north, south
// and east are customers; a US dollar is 0.80 EUR.
type env struct {
	fixture.Env
	receivable, revenue uuid.UUID // accounts 120000 and 400000 of company 2000
	north, south, east  uuid.UUID
}

func dunning(doc *config.Config) {
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true})
	de.Dunning.Levels = []config.DunningLevel{{Level: 1, DaysOverdue: 5}, {Level: 2, DaysOverdue: 20}, {Level: 3, DaysOverdue: 40}}
}

func setup(t *testing.T) env {
	t.Helper()
	f := fixture.NewWith(t, nil, dunning, perms...)
	e := env{Env: f, receivable: f.Account(t, "2000", "120000"), revenue: f.Account(t, "2000", "400000")}
	// Numbered against their names' order, so the notices' order by number is seen.
	e.north = e.customer(t, "BP70000002", "Northwind Traders")
	e.south = e.customer(t, "BP70000001", "Southwind Ltd")
	e.east = e.customer(t, "BP70000003", "Eastwind AG")
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.8, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	return e
}

// customer inserts an active business partner with the customer role.
func (e env) customer(t *testing.T, number, name string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'organization', $4, 'DE', 'active', $5, $5)`, e.Tenant.ID, id, number, name, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'customer', $3)`, e.Tenant.ID, id, e.Tenant.Human.ID)
	return id
}

func date(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// item is a customer item to post: the amount in minor units of the
// currency (debit positive) dated day, with NT30 from that day (due 30
// days later) unless it is posted without terms.
type item struct {
	customer uuid.UUID
	amount   money.Minor
	currency string
	day      string
	noTerms  bool
	ref      string
}

// post posts the items of company 2000 as receivables' postings would,
// each on the receivables account 120000 with its customer against
// revenue, and returns their entries.
func (e env) post(t *testing.T, items ...item) []ledger.Entry {
	t.Helper()
	post := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
	entries := make([]ledger.Entry, len(items))
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		for i, it := range items {
			day := date(t, it.day)
			in := ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: day, PostingDate: day, Currency: it.currency,
				Reference: &it.ref, SourceContext: "receivables", SourceDocumentID: uuid.Must(uuid.NewV7()),
				Lines: []ledger.PostingLine{{AccountID: e.receivable, Amount: it.amount, PartnerID: &it.customer}, {AccountID: e.revenue, Amount: -it.amount}}}
			if !it.noTerms {
				in.Terms = &ledger.ItemTerms{PaymentTermCode: "NT30", BaselineDate: day, NetDays: 30}
			}
			var err error
			if entries[i], err = post.Post(ctx, in); err != nil {
				return fmt.Errorf("post %s: %w", it.ref, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return entries
}

// story posts the items the tests' runs read, at the key date 30 June
// 2026: Northwind's invoice due 31 March (91 days overdue), one due 25
// June (5 days, the first level's), one due 26 June (4 days, a day short
// of it), one due on the key date, a debit without terms dated the key
// date (due that day by its document date, so not overdue; PO 2026-11-12),
// a credit without terms and an overdue credit with terms; Southwind's invoice in US
// dollars due 30 April (61 days); Eastwind's, not yet due. It returns the
// entries in that order.
func (e env) story(t *testing.T) []ledger.Entry {
	t.Helper()
	return e.post(t,
		item{customer: e.north, amount: 10000, currency: "EUR", day: "2026-03-01", ref: "CI-A"},
		item{customer: e.north, amount: 5000, currency: "EUR", day: "2026-05-26", ref: "CI-B"},
		item{customer: e.north, amount: 7000, currency: "EUR", day: "2026-05-27", ref: "CI-C"},
		item{customer: e.north, amount: 3000, currency: "EUR", day: "2026-05-31", ref: "CI-D"},
		item{customer: e.north, amount: 4000, currency: "EUR", day: "2026-06-30", noTerms: true, ref: "MANUAL"},
		item{customer: e.north, amount: -2000, currency: "EUR", day: "2026-03-03", noTerms: true, ref: "ON-ACCOUNT"},
		item{customer: e.north, amount: -1000, currency: "EUR", day: "2026-03-04", ref: "CREDIT"},
		item{customer: e.south, amount: 20000, currency: "USD", day: "2026-03-31", ref: "CI-USD"},
		item{customer: e.east, amount: 9000, currency: "EUR", day: "2026-06-15", ref: "CI-E"})
}

// run is the JSON of a dunning run of the company at the key date.
func run(company uuid.UUID, key string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "key_date": %q}`, company, key)
}

func amount(t *testing.T, m map[string]any, field string) string {
	t.Helper()
	a := apitest.Object(t, m, field)
	return fmt.Sprintf("%v %v", a["amount"], a["currency"])
}

// objects are the objects of a list field.
func objects(t *testing.T, m map[string]any, field string) []map[string]any {
	t.Helper()
	raw := apitest.List(t, m, field)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		o, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("%s[%d]: got %v, want an object", field, i, r)
		}
		out[i] = o
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// openItems are company 2000's open customer items as the ledger reads
// them.
func (e env) openItems(t *testing.T) string {
	t.Helper()
	var items []ledger.ReceivableItem
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		items, err = ledger.OpenItems{}.Receivable(ctx, e.DE, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return mustJSON(t, items)
}

// TestProposeAndRead: a run proposes, per customer, the items overdue on
// the key date at the level each would reach: only debits with terms that
// have reached the first level's days; credits are not dunned and reduce
// nothing; nothing changes on the items; the run and its notices read
// back with the names of what they reference, each amount with its
// currency, the notices by customer number; the audit record and
// DunningRunChanged are written (T76a acceptance).
func TestProposeAndRead(t *testing.T) {
	e := setup(t)
	entries := e.story(t)
	before := e.openItems(t)
	got, tag := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	for f, want := range map[string]any{"status": "proposed", "company_code": "2000", "company_name": "Acme GmbH", "key_date": keyDate,
		"currency": "EUR", "legal_entity_id": e.DE.String(), "notice_count": float64(2), "item_count": float64(3)} {
		if got[f] != want {
			t.Errorf("%s: got %v, want %v", f, got[f], want)
		}
	}
	// Northwind's 100.00 and 50.00, and Southwind's 200.00 USD, booked at 160.00 EUR.
	if a := amount(t, got, "overdue_amount"); a != "310.00 EUR" {
		t.Errorf("the run's overdue amount: got %s, want 310.00 EUR", a)
	}
	id := apitest.String(t, got, "id")
	read, readTag := e.Must(200, "GET", "/v1/dunning-runs/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b || readTag != tag {
		t.Errorf("read back: got %s (%s), want %s (%s)", a, readTag, b, tag)
	}
	if after := e.openItems(t); after != before {
		t.Errorf("the open items after the run: got %s, want them unchanged: %s", after, before)
	}

	page, _ := e.Must(200, "GET", "/v1/dunning-runs/"+id+"/notices", "", "")
	notices := objects(t, page, "items")
	if len(notices) != 2 || page["next_page_token"] != nil {
		t.Fatalf("notices: got %d and next page %v, want 2 on one page", len(notices), page["next_page_token"])
	}
	south, north := notices[0], notices[1]
	for f, want := range map[string]any{"dunning_run_id": id, "customer_id": e.north.String(), "customer_number": "BP70000002",
		"customer_name": "Northwind Traders", "level": float64(1), "item_count": float64(2)} {
		if north[f] != want {
			t.Errorf("Northwind's notice, %s: got %v, want %v", f, north[f], want)
		}
	}
	if a := amount(t, north, "overdue_amount"); a != "150.00 EUR" {
		t.Errorf("Northwind's overdue amount: got %s, want 150.00 EUR (its credits reduce nothing)", a)
	}
	wantItems := []struct {
		entry                   ledger.Entry
		ref, date, due          string
		days                    float64
		amount, local, ofNotice string
		notice                  map[string]any
		item                    int
	}{
		{entries[0], "CI-A", "2026-03-01", "2026-03-31", 91, "100.00 EUR", "100.00 EUR", "Northwind's first", north, 0},
		{entries[1], "CI-B", "2026-05-26", "2026-06-25", 5, "50.00 EUR", "50.00 EUR", "Northwind's second", north, 1},
		{entries[7], "CI-USD", "2026-03-31", "2026-04-30", 61, "200.00 USD", "160.00 EUR", "Southwind's", south, 0},
	}
	for _, w := range wantItems {
		t.Run(w.ofNotice+" item", func(t *testing.T) {
			its := objects(t, w.notice, "items")
			if len(its) <= w.item {
				t.Fatalf("items: got %d, want more than %d", len(its), w.item)
			}
			it := its[w.item]
			for f, want := range map[string]any{"item_no": float64(w.item + 1), "fiscal_year": float64(2026), "journal_entry_id": w.entry.ID.String(),
				"line_no": float64(1), "document_number": w.entry.DocumentNumber, "reference": w.ref, "document_date": w.date, "net_due_date": w.due,
				"days_overdue": w.days, "previous_level": float64(0), "level": float64(1)} {
				if it[f] != want {
					t.Errorf("%s: got %v, want %v", f, it[f], want)
				}
			}
			if a := amount(t, it, "amount"); a != w.amount {
				t.Errorf("amount: got %s, want %s", a, w.amount)
			}
			if a := amount(t, it, "local_amount"); a != w.local {
				t.Errorf("local amount: got %s, want %s", a, w.local)
			}
		})
	}
	if n := len(objects(t, north, "items")); n != 2 {
		t.Errorf("Northwind's items: got %d, want 2: the item a day short of the first level, the one due on the key date, "+
			"the one without terms, dated the key date, and the credits are left out", n)
	}
	if south["customer_number"] != "BP70000001" || south["customer_name"] != "Southwind Ltd" || amount(t, south, "overdue_amount") != "160.00 EUR" {
		t.Errorf("Southwind's notice: got %v %v with %s, want BP70000001 Southwind Ltd with 160.00 EUR", south["customer_number"],
			south["customer_name"], amount(t, south, "overdue_amount"))
	}
	e.recorded(t, id)
}

// recorded checks the audit record and the event of a proposed run.
func (e env) recorded(t *testing.T, id string) {
	t.Helper()
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'collections.dunning_run.propose' AND object_id = $2`,
		[]any{e.Tenant.ID, id}, &audits)
	if audits != 1 {
		t.Errorf("audit records of the proposal: got %d, want 1", audits)
	}
	events := apitest.CompanyNamed(t, apitest.Published(t, e.Tenant, outbox.Event{Context: "collections", Name: "dunning_run_changed"}))
	if len(events) != 1 {
		t.Fatalf("DunningRunChanged events: got %d, want 1", len(events))
	}
	want := map[string]any{"dunning_run_id": id, "legal_entity_id": e.DE.String(), "legal_entity_code": "2000", "key_date": keyDate,
		"notice_count": float64(2), "item_count": float64(3), "overdue_amount": map[string]any{"amount": "310.00", "currency": "EUR"},
		"status": "proposed", "status_before": nil, "version": float64(1)}
	if a, b := mustJSON(t, events[0]), mustJSON(t, want); a != b {
		t.Errorf("event payload: got %s, want %s", a, b)
	}
	for _, name := range []string{"Northwind", "Southwind"} {
		apitest.NeverPublished(t, e.Tenant, "collections", name)
	}
}

// TestItemsWithoutTerms (PO 2026-11-12): a customer item posted without
// payment terms is due on its document date. It is not overdue that day
// and is from the day after, for the worklist; a run proposes it once it
// is past that date by the first level's days, with the document date as
// its net due date; a credit without terms is never dunned and reduces
// nothing overdue.
func TestItemsWithoutTerms(t *testing.T) {
	e := setup(t)
	e.post(t, item{customer: e.north, amount: 4000, currency: "EUR", day: "2026-06-10", noTerms: true, ref: "MANUAL"},
		item{customer: e.north, amount: -2000, currency: "EUR", day: "2026-03-01", noTerms: true, ref: "ON-ACCOUNT"})
	for _, c := range []struct{ name, key, want string }{
		{"on its document date it is not overdue", "2026-06-10", ""},
		{"the day after it is", "2026-06-11", "BP70000002 20.00 EUR 40.00 EUR 2026-06-10 0 -"},
	} {
		t.Run("the worklist: "+c.name, func(t *testing.T) {
			if l := listed(t, e.In(t).worklist(t, c.key, "")); l != c.want {
				t.Errorf("the worklist at %s: got %q, want %q", c.key, l, c.want)
			}
		})
	}
	for _, c := range []struct{ name, key, want string }{
		{"on its document date", "2026-06-10", ""},
		{"a day short of the first level's days", "2026-06-14", ""},
		{"at the first level's days", "2026-06-15", "MANUAL 0->1 due 2026-06-10, 5 days"},
	} {
		t.Run("a run "+c.name, func(t *testing.T) {
			id, _ := e.In(t).start(t, c.key)
			page, _ := e.In(t).Must(200, "GET", "/v1/dunning-runs/"+id+"/notices", "", "")
			var got []string
			for _, n := range objects(t, page, "items") {
				for _, it := range objects(t, n, "items") {
					got = append(got, fmt.Sprintf("%v %v->%v due %v, %v days", it["reference"], it["previous_level"], it["level"], it["net_due_date"],
						it["days_overdue"]))
				}
			}
			if l := strings.Join(got, "; "); l != c.want {
				t.Errorf("proposed at %s: got %q, want %q", c.key, l, c.want)
			}
		})
	}
}

// TestKeyDates: what a run proposes follows its key date: before anything
// is due it proposes no notice, and stores a run without any; an item is
// first proposed the day it has been overdue for the first level's days.
func TestKeyDates(t *testing.T) {
	e := setup(t)
	e.story(t)
	for _, c := range []struct {
		name, key      string
		notices, items float64
		overdue        string
	}{
		{"before anything is due", "2026-03-31", 0, 0, "0.00 EUR"},
		{"four days after the first due date", "2026-04-04", 0, 0, "0.00 EUR"},
		{"five days after it", "2026-04-05", 1, 1, "100.00 EUR"},
		{"once everything is overdue, the debit without terms too", "2026-12-31", 3, 7, "540.00 EUR"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _ := e.In(t).Must(201, "POST", "/v1/dunning-runs", run(e.DE, c.key), "")
			if got["notice_count"] != c.notices || got["item_count"] != c.items || amount(t, got, "overdue_amount") != c.overdue {
				t.Errorf("run at %s: got %v notices, %v items, %s; want %v, %v, %s", c.key, got["notice_count"], got["item_count"],
					amount(t, got, "overdue_amount"), c.notices, c.items, c.overdue)
			}
			page, _ := e.In(t).Must(200, "GET", "/v1/dunning-runs/"+apitest.String(t, got, "id")+"/notices", "", "")
			if n := float64(len(objects(t, page, "items"))); n != c.notices {
				t.Errorf("notices read: got %v, want %v", n, c.notices)
			}
		})
	}
}

// TestIdempotentRun: the same Idempotency-Key starts one run and answers
// it again.
func TestIdempotentRun(t *testing.T) {
	e := setup(t)
	e.story(t)
	req := apitest.Req{Method: "POST", Path: "/v1/dunning-runs", Body: run(e.DE, keyDate), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	first, second := e.API.Do(req), e.API.Do(req)
	if first.Code != 201 || second.Code != 201 {
		t.Fatalf("statuses: got %d and %d, want 201 twice: %s", first.Code, second.Code, second.Body)
	}
	if a, b := apitest.JSON(t, first), apitest.JSON(t, second); a["id"] != b["id"] {
		t.Errorf("replay: got run %v, want the first answer's %v", b["id"], a["id"])
	}
	if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", h)
	}
	if loc, want := first.Header().Get("Location"), "/v1/dunning-runs/"+apitest.String(t, apitest.JSON(t, first), "id"); loc != want {
		t.Errorf("Location: got %s, want %s", loc, want)
	}
	var runs, notices int
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM collections.dunning_run WHERE tenant_id = $1),
		(SELECT count(*) FROM collections.dunning_notice WHERE tenant_id = $1)`, []any{e.Tenant.ID}, &runs, &notices)
	if runs != 1 || notices != 2 {
		t.Errorf("stored: got %d runs with %d notices, want 1 with 2", runs, notices)
	}
}

// TestLists: runs are listed the latest key date first, filtered by
// company, status and key date, and paged; a run's notices are filtered
// by customer and level and paged by customer number.
func TestLists(t *testing.T) {
	e := setup(t)
	e.story(t)
	ids := map[string]string{}
	for _, key := range []string{"2026-04-30", keyDate, "2026-05-31"} {
		got, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, key), "")
		ids[key] = apitest.String(t, got, "id")
	}
	keys := func(t *testing.T, query string) string {
		t.Helper()
		page, _ := e.In(t).Must(200, "GET", "/v1/dunning-runs"+query, "", "")
		var out []string
		for _, r := range objects(t, page, "items") {
			out = append(out, apitest.String(t, r, "key_date"))
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct{ name, query, want string }{
		{"every run, the latest key date first", "", "2026-06-30 2026-05-31 2026-04-30"},
		{"the oldest first", "?order_by=key_date%20asc", "2026-04-30 2026-05-31 2026-06-30"},
		{"of the company", "?legal_entity_id=" + e.DE.String(), "2026-06-30 2026-05-31 2026-04-30"},
		{"of another company", "?legal_entity_id=" + e.US.String(), ""},
		{"proposed", "?status=proposed", "2026-06-30 2026-05-31 2026-04-30"},
		{"in another status", "?status=confirmed", ""},
		{"from a key date", "?key_date_from=2026-05-31", "2026-06-30 2026-05-31"},
		{"to a key date", "?key_date_to=2026-05-31", "2026-05-31 2026-04-30"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := keys(t, c.query); got != c.want {
				t.Errorf("key dates: got %q, want %q", got, c.want)
			}
		})
	}
	t.Run("runs paged", func(t *testing.T) {
		page, _ := e.In(t).Must(200, "GET", "/v1/dunning-runs?page_size=2", "", "")
		next := apitest.String(t, page, "next_page_token")
		if got := keys(t, "?page_size=2&page_token="+next); got != "2026-04-30" || len(objects(t, page, "items")) != 2 {
			t.Errorf("second page: got %q after %d runs, want 2026-04-30 after 2", got, len(objects(t, page, "items")))
		}
	})
	notices := "/v1/dunning-runs/" + ids[keyDate] + "/notices"
	customers := func(t *testing.T, query string) (string, map[string]any) {
		t.Helper()
		page, _ := e.In(t).Must(200, "GET", notices+query, "", "")
		var out []string
		for _, n := range objects(t, page, "items") {
			out = append(out, apitest.String(t, n, "customer_number"))
		}
		return strings.Join(out, " "), page
	}
	for _, c := range []struct{ name, query, want string }{
		{"every notice, by customer number", "", "BP70000001 BP70000002"},
		{"the highest number first", "?order_by=customer_number%20desc", "BP70000002 BP70000001"},
		{"of a customer", "?customer_id=" + e.north.String(), "BP70000002"},
		{"of a customer without a notice", "?customer_id=" + e.east.String(), ""},
		{"at level 1", "?level=1", "BP70000001 BP70000002"},
		{"at level 2", "?level=2", ""},
	} {
		t.Run("notices "+c.name, func(t *testing.T) {
			if got, _ := customers(t, c.query); got != c.want {
				t.Errorf("customers: got %q, want %q", got, c.want)
			}
		})
	}
	t.Run("notices paged", func(t *testing.T) {
		first, page := customers(t, "?page_size=1")
		second, last := customers(t, "?page_size=1&page_token="+apitest.String(t, page, "next_page_token"))
		if first != "BP70000001" || second != "BP70000002" || last["next_page_token"] != nil {
			t.Errorf("pages: got %q then %q with next page %v, want BP70000001 then BP70000002, the last", first, second, last["next_page_token"])
		}
		// The page's notices alone are read with their items.
		if n := len(objects(t, objects(t, last, "items")[0], "items")); n != 2 {
			t.Errorf("Northwind's items on its page: got %d, want 2", n)
		}
	})
}

// TestRefusals: a company without dunning levels has no dunning; a
// company the caller may not act in is answered as missing, on writing
// and on reading.
func TestRefusals(t *testing.T) {
	e := setup(t)
	e.story(t)
	t.Run("a company without dunning levels", func(t *testing.T) {
		got := e.In(t).Problem(422, "collections.no_dunning_levels", "POST", "/v1/dunning-runs", run(e.US, keyDate), "")
		if d, _ := got["detail"].(string); !strings.Contains(d, "Company 1000") {
			t.Errorf("detail: got %q, want it to name company 1000", d)
		}
	})
	t.Run("an unknown company", func(t *testing.T) {
		got := e.In(t).Problem(422, "request.reference_not_found", "POST", "/v1/dunning-runs", run(uuid.New(), keyDate), "")
		if f := field(got); f != "legal_entity_id" {
			t.Errorf("field: got %v, want legal_entity_id", f)
		}
	})
	run2000, _ := e.Must(201, "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	id := apitest.String(t, run2000, "id")
	other := e.As(e.Reader(t, "A person of company 1000", e.US))
	t.Run("a company the caller may not act in", func(t *testing.T) {
		got := other.In(t).Problem(422, "request.reference_not_found", "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
		if f := field(got); f != "legal_entity_id" {
			t.Errorf("field: got %v, want legal_entity_id", f)
		}
	})
	t.Run("its run read", func(t *testing.T) {
		other.In(t).Problem(404, "request.not_found", "GET", "/v1/dunning-runs/"+id, "", "")
	})
	t.Run("its run's notices read", func(t *testing.T) {
		other.In(t).Problem(404, "request.not_found", "GET", "/v1/dunning-runs/"+id+"/notices", "", "")
	})
	t.Run("its runs listed", func(t *testing.T) {
		page, _ := other.In(t).Must(200, "GET", "/v1/dunning-runs", "", "")
		if n := len(objects(t, page, "items")); n != 0 {
			t.Errorf("runs listed: got %d, want none", n)
		}
	})
	t.Run("a run that does not exist", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "GET", "/v1/dunning-runs/"+uuid.NewString(), "", "")
		e.In(t).Problem(404, "request.not_found", "GET", "/v1/dunning-runs/"+uuid.NewString()+"/notices", "", "")
	})
	t.Run("without the permission", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'collections.dunning.run'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", "/v1/dunning-runs", run(e.DE, keyDate), "")
	})
}

// field is the field the problem's first field error names.
func field(m map[string]any) any {
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 {
		return nil
	}
	e, _ := errs[0].(map[string]any)
	return e["field"]
}
