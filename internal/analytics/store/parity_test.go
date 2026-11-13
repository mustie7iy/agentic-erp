package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/analytics/reader"
	"github.com/mustie7iy/agentic-erp/internal/analytics/store"
	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// direct is the reader in-process: parity is about the plans and the
// reader's SQL; TestClient covers the transport.
type direct struct{ r *reader.Reader }

func (d direct) Query(ctx context.Context, req wire.Request) (*wire.Result, error) {
	return d.r.Query(ctx, req)
}

// parity is a tenant with data in both companies and two ledgers, its
// store extracted, and both paths over it.
type parity struct {
	fixture.Env
	db       analytics.Source
	src      *store.Source
	parallel uuid.UUID
	asOf     time.Time // the manifest's
	r        *reader.Reader
	objects  blobstore.Store
}

var parityPerms = []string{"ledger.entry.post", "ledger.entry.read", "ledger.account.read", "payables.invoice.read", "procurement.order.read",
	"partner.supplier.read", "tenancy.org.read", "analytics.query.run", "config.change.read", "config.change.propose", "config.change.activate", "platform.operation.read",
	"cash.forecast.read", "sales.order.read", "sales.billing.read"}

// cashPort is the cash context as erp-server wires it into the analytics
// context, the database path's and the extract's.
func cashPort(t *testing.T) app.AnalyticsCash {
	t.Helper()
	pool := pgtest.Pool(t)
	return app.NewAnalyticsCash(app.Config{Pool: pool, Consistent: pool, AppRole: "erp_app"})
}

// post posts a journal entry in le debiting debit and crediting credit,
// the partner on the credit line, in ledger (nil: the leading one).
func (p parity) post(t *testing.T, le uuid.UUID, company, cur, date, debit, credit, amount string, partner *uuid.UUID, ledger *uuid.UUID) (string, string) {
	t.Helper()
	extra, onCredit := "", ""
	if ledger != nil {
		extra = fmt.Sprintf(`"ledger_id": %q, `, ledger)
	}
	if partner != nil {
		onCredit = fmt.Sprintf(`, "partner_id": %q`, partner)
	}
	je, tag := p.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{%s"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": %q,
		"lines": [{"account_id": %q, "amount": {"amount": %q, "currency": %q}},
		{"account_id": %q, "amount": {"amount": "-%s", "currency": %q}%s}]}`,
		extra, le, date, date, cur, p.Account(t, company, debit), amount, cur, p.Account(t, company, credit), amount, cur, onCredit), "")
	return je["id"].(string), tag
}

// order inserts a purchase order of le with one item, and a removed one.
func (p parity) order(t *testing.T, le, supplier uuid.UUID, company, cur, date, quantity string, price int64) {
	t.Helper()
	po := uuid.New()
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, 'NT30', 'draft', $8, $8)`,
		p.Tenant.ID, po, po.String()[:10], le, supplier, cur, date, p.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price, account_id)
		VALUES ($1, $2, 10, 'Paper', $3::numeric, 'EA', $4, $5), ($1, $2, 20, 'Toner', 2, 'EA', 1999, $6)`,
		p.Tenant.ID, po, quantity, price, p.Account(t, company, "610000"), p.Account(t, company, "520000"))
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price, account_id, is_removed)
		VALUES ($1, $2, 30, 'Gone', 1, 'EA', 5000, $3, true)`, p.Tenant.ID, po, p.Account(t, company, "610000"))
}

// invoice inserts a supplier invoice of le in status.
func (p parity) invoice(t *testing.T, le, supplier uuid.UUID, cur, date, status string, gross int64) {
	t.Helper()
	id := uuid.New()
	pgtest.Exec(t, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference,
		invoice_date, currency, gross_amount, tax_amount, status, source, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $3, $6, $7, $8, 0, $9, 'manual', $10, $10)`,
		p.Tenant.ID, id, id.String()[:10], le, supplier, date, cur, gross, status, p.Tenant.Human.ID)
}

// parityDoc is the fixture's document with a local-GAAP ledger 2L kept
// by company 2000 (ADR-0045).
func parityDoc(t *testing.T) *config.Config {
	t.Helper()
	doc := fixture.Document(t)
	parityLedgers(doc)
	return doc
}

// parityLedgers gives company 2000 the local-GAAP ledger 2L beside 0L,
// and the tenant the cash actuals' liquidity items.
func parityLedgers(doc *config.Config) {
	doc.Ledgers = []config.Ledger{{Code: "0L", Name: "Leading ledger", AccountingPrinciple: "IFRS"}, {Code: "2L", Name: "Local GAAP", AccountingPrinciple: "HGB"}}
	doc.Company("2000").Ledgers = []string{"0L", "2L"}
	fixture.CashItems(doc)
}

// setupParity seeds the tenant: revenue, expenses and supplier items in
// both companies, one item cleared by a reversal, a parallel ledger's
// entry, a customer, orders, invoices and bank statement lines classified
// three ways; then extracts its store.
func setupParity(t *testing.T) parity {
	t.Helper()
	// Company 2000 keeps a local-GAAP ledger 2L beside 0L (ADR-0045), its
	// periods opened with 0L's: every posting of 2000 is copied into it.
	p := parity{Env: fixture.NewWith(t, nil, parityLedgers, parityPerms...)}
	ten := p.Tenant
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.9, 'ECB', $2)`, ten.ID, ten.Human.ID)
	pgtest.Scan(t, `SELECT id FROM ledger.ledger WHERE tenant_id = $1 AND code = '2L'`, []any{ten.ID}, &p.parallel)
	customer := p.Customer(t, "BP80000001", "A Customer")

	p.post(t, p.DE, "2000", "EUR", "2026-03-05", "610000", "211000", "120.00", &p.Supplier, nil)
	p.post(t, p.DE, "2000", "EUR", "2026-03-20", "100000", "400000", "500.00", nil, nil)
	p.post(t, p.DE, "2000", "EUR", "2026-07-02", "100000", "400000", "75.25", nil, nil)
	id, tag := p.post(t, p.DE, "2000", "EUR", "2026-05-02", "520000", "211000", "30.00", &p.Vendor, nil)
	p.Must(200, "POST", "/v1/journal-entries/"+id+":reverse", `{"posting_date": "2026-05-03"}`, tag)
	p.post(t, p.US, "1000", "USD", "2026-08-10", "510000", "211000", "45.50", &p.Supplier, nil)
	p.post(t, p.US, "1000", "USD", "2026-02-11", "100000", "400000", "800.00", nil, nil)
	p.post(t, p.DE, "2000", "EUR", "2026-03-05", "610000", "100000", "999.00", nil, &p.parallel)
	p.order(t, p.DE, p.Supplier, "2000", "EUR", "2026-03-01", "2.5", 1234)
	p.order(t, p.DE, p.Vendor, "2000", "EUR", "2026-09-15", "1", 10000)
	p.order(t, p.US, p.Supplier, "1000", "USD", "2026-04-04", "0.333333", 999)
	p.invoice(t, p.DE, p.Supplier, "EUR", "2026-03-06", "parked", 14280)
	p.invoice(t, p.DE, p.Vendor, "EUR", "2026-06-30", "rejected", 5000)
	p.invoice(t, p.US, p.Supplier, "USD", "2026-08-11", "parked", 4550)
	p.sales(t, customer)
	p.Statement(t, p.DE, fixture.StatementLine{ValueDate: "2026-03-31", Amount: 50000}, fixture.StatementLine{ValueDate: "2026-04-02", Amount: -1500, Rule: "NCHG"},
		fixture.StatementLine{ValueDate: "2026-07-15", Amount: -20000})
	p.Statement(t, p.US, fixture.StatementLine{ValueDate: "2026-02-10", Amount: 10000}, fixture.StatementLine{ValueDate: "2026-08-20", Amount: -4500})
	p.snapshots(t)

	p.extract(t)
	srv := httptest.NewServer(p.API.Handler)
	t.Cleanup(srv.Close)
	p.db = analytics.Source{API: &apitools.Client{BaseURL: srv.URL, Token: p.API.Issuer.Token(t, ten.ID, ten.Human.Subject, nil), HTTP: srv.Client()},
		Cash: cashPort(t)}
	return p
}

// sales writes the customer's sales documents in both companies (T91a):
// 2000's confirmed order of two items, one without an item key, billed in
// a posted document the month after, a draft order, and a confirmed order
// whose billing document is created and not posted; 1000's confirmed
// order and its posted billing document. The drafts and the document not
// posted are in no metric.
func (p parity) sales(t *testing.T, customer uuid.UUID) {
	t.Helper()
	order := p.SalesOrder(t, p.DE, customer, "EUR", "2026-03-12", "confirmed", fixture.SalesItem{Key: "BRACKET-M8", Net: 150000}, fixture.SalesItem{Net: 23753})
	p.BillingDocument(t, order, "2026-04-01", true)
	p.SalesOrder(t, p.DE, customer, "EUR", "2026-03-13", "draft", fixture.SalesItem{Key: "DRAFT", Net: 99999})
	later := p.SalesOrder(t, p.DE, customer, "EUR", "2026-09-15", "confirmed", fixture.SalesItem{Key: "BRACKET-M8", Net: 5000})
	p.BillingDocument(t, later, "2026-09-16", false)
	us := p.SalesOrder(t, p.US, customer, "USD", "2026-02-20", "confirmed", fixture.SalesItem{Key: "INSTALL", Net: 9900})
	p.BillingDocument(t, us, "2026-02-25", true)
}

// snapshots keeps forecasts of both companies at lags 0, 7 and 30: 2000's
// daily snapshot of 1 March (one taken on demand that day not preferred),
// with a blocked supplier's line, one of no item and bank and a bucket's
// sum beyond the document level, and one taken on demand on 1 July; 1000's
// daily snapshot of 10 February.
func (p parity) snapshots(t *testing.T) {
	t.Helper()
	line := func(day, source, item string, amount int64, bank bool) fixture.SnapshotLine {
		return fixture.SnapshotLine{ValueDate: day, Source: source, Certainty: "expected", Item: item, Amount: amount, Bank: bank}
	}
	p.Snapshot(t, p.DE, "daily", "2026-03-01", line("2026-03-01", "payable", "OP_SUPPLIERS", -1000, true), line("2026-03-08", "payable", "OP_SUPPLIERS", -2000, true),
		line("2026-03-08", "payable_blocked", "OP_SUPPLIERS", -400, false), line("2026-03-08", "memo", "", 700, false),
		line("2026-03-31", "payment_run", "BANK_CHARGES", -300, true), line("2026-03-12", "payable", "OP_SUPPLIERS", -55, true),
		fixture.SnapshotLine{ValueDate: "2026-04-05", BucketEnd: "2026-04-11", Source: "payable", Certainty: "expected", Amount: -9999})
	p.Snapshot(t, p.DE, "on_demand", "2026-03-01", line("2026-03-01", "payable", "OP_SUPPLIERS", -1, true))
	p.Snapshot(t, p.DE, "on_demand", "2026-07-01", line("2026-07-01", "memo", "REC_CUSTOMERS", 5000, true), line("2026-07-08", "payable", "OP_SUPPLIERS", -600, true))
	p.Snapshot(t, p.US, "daily", "2026-02-10", line("2026-02-10", "payable", "OP_SUPPLIERS", -100, true), line("2026-02-17", "memo", "REC_CUSTOMERS", 250, true),
		line("2026-03-12", "payable", "OP_SUPPLIERS", -80, false))
}

// extract waits until the tenant's postings are below the snapshot
// horizon, runs its extract, and serves its store through a reader.
func (p *parity) extract(t *testing.T) {
	t.Helper()
	pgtest.Eventually(t, 5*time.Minute, func() bool { // the horizon waits for every open transaction of the cluster
		var done bool
		pgtest.Scan(t, `SELECT coalesce(max(posted_xid) < pg_snapshot_xmin(pg_current_snapshot()), true) FROM ledger.journal_entry WHERE tenant_id = $1`,
			[]any{p.Tenant.ID}, &done)
		return done
	}, "postings of tenant %s still below the snapshot horizon", p.Tenant.ID)
	objects := blobstoretest.New(t)
	pool := pgtest.Pool(t)
	x := extract.Extractor{Replica: pool.Begin, Primary: pool.Begin, Role: "erp_app", Store: objects.Store(), Cash: cashPort(t)}
	res, err := x.Run(context.Background(), p.Tenant.ID)
	if err != nil || res.Pending {
		t.Fatalf("extract: %+v, %v; want it current", res, err)
	}
	pgtest.Scan(t, `SELECT as_of FROM analytics.snapshot WHERE tenant_id = $1`, []any{p.Tenant.ID}, &p.asOf)
	r, err := reader.New(objects.Store(), t.TempDir(), currency.Seeded(), reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	p.r, p.objects = r, objects.Store()
	p.src = source(direct{r}, p.db)
	p.src.Now = nil
}

// queries are every metric's questions: alone, by each dimension it
// allows, by all of them, filtered by period, and, for journal metrics, in
// the parallel ledger.
func queries(m *analytics.Model, parallel uuid.UUID) map[string]analytics.Query {
	out := map[string]analytics.Query{}
	for _, met := range m.Metrics {
		dims := m.AllowedDimensions(met.Name)
		ms := []string{met.Name}
		out[met.Name] = analytics.Query{Measures: ms}
		out[met.Name+" by all"] = analytics.Query{Measures: ms, By: dims}
		for _, d := range dims {
			out[met.Name+" by "+d] = analytics.Query{Measures: ms, By: []string{d, "company"}}
		}
		out[met.Name+" in 2026 H1"] = analytics.Query{Measures: ms, By: []string{"company"}, Where: []analytics.Filter{
			{Dim: "fiscal_year", Op: "in", Values: []string{"2026", "2027"}}, {Dim: "fiscal_month", Op: "between", Values: []string{"1", "6"}}}}
		out[met.Name+" not 1000"] = analytics.Query{Measures: ms, By: []string{"fiscal_quarter"}, Where: []analytics.Filter{
			{Dim: "company", Op: "not_in", Values: []string{"1000"}}}}
		for _, date := range []string{"posting_date", "value_date"} {
			if slices.Contains(dims, date) {
				out[met.Name+" from March 10"] = analytics.Query{Measures: ms, By: []string{"company"}, Where: []analytics.Filter{
					{Dim: date, Op: ">=", Values: []string{"2026-03-10"}}}}
			}
		}
		if slices.Contains(m.BaseMetrics(met.Name), "revenue") || met.Source == "gl_lines" {
			out[met.Name+" in 2L"] = analytics.Query{Measures: ms, By: []string{"company"}, Ledger: parallel.String()}
		}
	}
	return out
}

// TestParity: for the same seeded tenant, the store and the database
// return the same answer to every metric of semantic.yaml, broken down and
// filtered every way it allows, for the fiscal year starting in January
// and in July, for every company and for one; each answer names its
// source.
func TestParity(t *testing.T) {
	p := setupParity(t)
	b, err := os.ReadFile("../semantic.yaml")
	if err != nil {
		t.Fatal(err)
	}
	july, err := analytics.ParseModel([]byte(strings.Replace(string(b), "fiscal_year_start_month: 1", "fiscal_year_start_month: 7", 1)))
	if err != nil {
		t.Fatal(err)
	}
	for calendar, m := range map[string]*analytics.Model{"january": model(t), "july": july} {
		dbFacts := p.dbFacts(t, m)
		for who, pr := range map[string]analytics.Principal{"every company": {ID: "a"}, "2000": {ID: "b", Companies: []string{"2000"}}} {
			for name, q := range queries(m, p.parallel) {
				t.Run(calendar+"/"+who+"/"+name, func(t *testing.T) { p.same(t, m, dbFacts, pr, q) })
			}
		}
	}
	t.Run("endpoint", p.endpoint)
	t.Run("permissions", p.permissions)
	t.Run("permissions in read order", p.permissionsInReadOrder)
	t.Run("metric change", p.metricChange) // last: it activates a configuration version
	t.Run("isolation", p.isolation)
	t.Run("not vacuous", func(t *testing.T) {
		m := model(t)
		dbFacts := p.dbFacts(t, m)
		for _, met := range m.Metrics {
			res, err := analytics.Execute(m, dbFacts, analytics.Principal{}, analytics.Query{Measures: []string{met.Name}, By: []string{"company"}})
			if err != nil || len(res.Rows) != 2 {
				t.Errorf("%s by company: got %v, %v; want a row for each company", met.Name, res, err)
			}
		}
		// Each lag's figure is of its own snapshots, a blocked supplier's
		// line and a bucket's sum left out, a snapshot on demand only
		// where its date has no daily one. 31 March's receipt was not
		// forecast a month ahead and its bank charges forecast were not
		// paid: 503.00 over 3.00, an accuracy of -166.6667; nothing
		// forecast a week ahead was paid on its day: 0.
		for metric, want := range map[string]string{"forecast_lag_0": "40.00", "forecast_lag_7": "-19.00", "forecast_lag_30": "-3.00",
			"variance_lag_30": "503.00", "absolute_variance_lag_30": "503.00", "accuracy_lag_30": "-166.6667", "accuracy_lag_7": "0.0000"} {
			res, err := analytics.Execute(m, dbFacts, analytics.Principal{Companies: []string{"2000"}}, analytics.Query{Measures: []string{metric}})
			if err != nil || len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != want {
				t.Errorf("%s of 2000: got %v, %v; want %s", metric, res, err, want)
			}
		}
		// The confirmed orders' items and the posted billing document's:
		// the draft order and the document not posted are not counted.
		for metric, want := range map[string]string{"order_intake": "1787.53", "billed_value": "1737.53", "billing_document_count": "1"} {
			res, err := analytics.Execute(m, dbFacts, analytics.Principal{Companies: []string{"2000"}}, analytics.Query{Measures: []string{metric}})
			if err != nil || len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != want {
				t.Errorf("%s of 2000: got %v, %v; want %s", metric, res, err, want)
			}
		}
		for ledger, want := range map[string]string{"": "120.00", p.parallel.String(): "1119.00"} {
			res, err := analytics.Execute(m, dbFacts, analytics.Principal{Companies: []string{"2000"}}, analytics.Query{Measures: []string{"expenses"}, Ledger: ledger})
			if err != nil || len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != want {
				t.Errorf("expenses of 2000 in ledger %q: got %v, %v; want %s", ledger, res, err, want)
			}
		}
	})
}

// dbFacts are the database path's facts, read as the tenant's person in a
// request, as the endpoint reads them: the cash actuals are read as the
// caller, whose company access the cash context checks.
func (p parity) dbFacts(t *testing.T, m *analytics.Model) *analytics.Facts {
	t.Helper()
	var f *analytics.Facts
	if err := apitest.InRequest(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) error {
		var err error
		f, err = p.db.Facts(ctx, m, p.asOf)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// same runs q on both paths and compares the answers.
func (p parity) same(t *testing.T, m *analytics.Model, dbFacts *analytics.Facts, pr analytics.Principal, q analytics.Query) {
	t.Helper()
	want, wantErr := analytics.Execute(m, dbFacts, pr, q)
	var got *analytics.Result
	var gotErr error
	inRequest(t, p.Tenant.ID, nil, func(ctx context.Context) {
		f, err := p.src.Facts(ctx, m, pr, q, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if f.Source != analytics.SourceStore {
			t.Fatalf("facts: got %s, want the store's", f.Source)
		}
		got, gotErr = analytics.Execute(m, f, pr, q)
	})
	if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
		t.Fatalf("error: got %v, want %v", gotErr, wantErr)
	}
	if wantErr != nil {
		return
	}
	if !reflect.DeepEqual(got.Columns, want.Columns) {
		t.Errorf("columns: got %+v, want %+v", got.Columns, want.Columns)
	}
	if !reflect.DeepEqual(got.Rows, want.Rows) {
		t.Errorf("rows: got %v, want %v", got.Rows, want.Rows)
	}
	if !reflect.DeepEqual(got.Definitions, want.Definitions) {
		t.Errorf("definitions: got %+v, want %+v", got.Definitions, want.Definitions)
	}
	if got.Source != analytics.SourceStore || !got.AsOf.Equal(p.asOf) || want.Source != analytics.SourceDatabase {
		t.Errorf("lineage: got %s as of %s and %s, want store as of %s and database", got.Source, got.AsOf, want.Source, p.asOf)
	}
}

// endpoint: erp-server's analytics endpoint answers from the store, and
// from the database when asked for data newer than the store's, alike.
func (p parity) endpoint(t *testing.T) {
	api := apitest.NewWith(t, func(c *app.Config) { c.Analytics = direct{p.r} })
	c := apitest.Client{T: t, API: api, Tenant: p.Tenant, As: p.Tenant.Human}
	q := `{"measures": ["revenue", "expenses", "invoice_count"], "by": ["company", "fiscal_quarter"], "order_by": "company"%s}`
	fromStore, _ := c.Must(200, "POST", "/v1/analytics-queries", fmt.Sprintf(q, ""), "")
	fromDB, _ := c.Must(200, "POST", "/v1/analytics-queries", fmt.Sprintf(q, `, "as_of": "2999-01-01T00:00:00Z"`), "")
	if fromStore["source"] != "store" || fromDB["source"] != "database" {
		t.Errorf("sources: got %v and %v, want store and database", fromStore["source"], fromDB["source"])
	}
	if !reflect.DeepEqual(fromStore["rows"], fromDB["rows"]) || len(fromStore["rows"].([]any)) == 0 {
		t.Errorf("rows: got %v from the store and %v from the database, want the same", fromStore["rows"], fromDB["rows"])
	}
}

// permissions: a caller lacking a permission the database path's reads
// need (the companies, the vendors' names, the accounts' names, the cash
// actuals), or barred from it by a segregation-of-duties rule in force
// (ADR-0010 As built 2026-11-02), is refused by the store exactly as by
// the database, before any dimension is named. The cash actuals name no
// vendor: a caller without partner.supplier.read reads them, and is still
// refused a query that names vendors (product owner 2026-11-02). The sales
// lines name customers and are read from the orders and the billing
// documents: a caller lacking partner.supplier.read, sales.order.read or
// sales.billing.read gets no row of them (T91a).
func (p parity) permissions(t *testing.T) {
	api := apitest.NewWith(t, func(c *app.Config) { c.Analytics = direct{p.r} })
	// Each by company too: an answer adds no two currencies together.
	queries := map[string]string{
		"accounts":         `{"measures": ["revenue"], "by": ["company", "account_name"]%s}`,
		"vendors":          `{"measures": ["invoice_count"], "by": ["company", "vendor_name"]%s}`,
		"orders":           `{"measures": ["purchase_order_value"], "by": ["company", "account_name", "vendor_name"]%s}`,
		"cash":             `{"measures": ["actual"], "by": ["company", "liquidity_item_name", "house_bank"]%s}`,
		"cash and vendors": `{"measures": ["actual", "invoice_count"], "by": ["company"]%s}`,
		"forecast":         `{"measures": ["forecast_lag_7"], "by": ["company", "liquidity_item_name", "house_bank", "flow_source", "certainty"]%s}`,
		"accuracy":         `{"measures": ["accuracy_lag_30", "variance_lag_30"], "by": ["company", "liquidity_item_name"]%s}`,
		"sales":            `{"measures": ["order_intake", "billed_value", "billing_document_count"], "by": ["company", "customer_name", "item_key"]%s}`,
	}
	// needs are the permissions of those checked each query's reads need.
	needs := map[string][]string{
		"accounts": {"tenancy.org.read", "partner.supplier.read", "ledger.account.read"}, "vendors": {"tenancy.org.read", "partner.supplier.read"},
		"orders": {"tenancy.org.read", "partner.supplier.read", "ledger.account.read"}, "cash": {"tenancy.org.read", "cash.forecast.read"},
		"cash and vendors": {"tenancy.org.read", "partner.supplier.read", "cash.forecast.read"}, "forecast": {"tenancy.org.read", "cash.forecast.read"},
		"accuracy": {"tenancy.org.read", "cash.forecast.read"},
		"sales":    {"tenancy.org.read", "partner.supplier.read", "sales.order.read", "sales.billing.read"},
	}
	for _, perm := range []string{"tenancy.org.read", "partner.supplier.read", "ledger.account.read", "cash.forecast.read", "sales.order.read", "sales.billing.read"} {
		for how, c := range map[string]struct {
			as   func(t *testing.T, perm string) pgtest.Actor
			code string
		}{"lacking": {p.lacking, "identity.permission_missing"}, "barred": {p.barred, "identity.sod_conflict"}} {
			t.Run(perm+"/"+how, func(t *testing.T) {
				cl := apitest.Client{T: t, API: api, Tenant: p.Tenant, As: c.as(t, perm)}
				for name, q := range queries {
					storeStatus, fromStore, _ := cl.Do("POST", "/v1/analytics-queries", fmt.Sprintf(q, ""), "")
					dbStatus, fromDB, _ := cl.Do("POST", "/v1/analytics-queries", fmt.Sprintf(q, `, "as_of": "2999-01-01T00:00:00Z"`), "")
					want, code := 200, ""
					if slices.Contains(needs[name], perm) {
						want, code = 403, c.code
					}
					if storeStatus != want || fromStore["code"] != nilIfEmpty(code) {
						t.Errorf("%s from the store: got %d %v, want %d %s", name, storeStatus, fromStore, want, code)
					}
					if storeStatus != dbStatus || fromStore["code"] != fromDB["code"] || fromStore["detail"] != fromDB["detail"] ||
						!reflect.DeepEqual(fromStore["rows"], fromDB["rows"]) {
						t.Errorf("%s: got %d %v from the store and %d %v from the database, want the same", name, storeStatus, fromStore, dbStatus, fromDB)
					}
					if want == 200 && (fromStore["source"] != "store" || len(fromStore["rows"].([]any)) == 0) {
						t.Errorf("%s from the store: got %v rows from %v, want the store's rows", name, fromStore["rows"], fromStore["source"])
					}
				}
			})
		}
	}
}

// permissionsInReadOrder: a caller barred from a permission the database
// path reads first and lacking one it reads later is refused for the first,
// by the store as by the database (each read is authorized before the
// next).
func (p parity) permissionsInReadOrder(t *testing.T) {
	api := apitest.NewWith(t, func(c *app.Config) { c.Analytics = direct{p.r} })
	cl := apitest.Client{T: t, API: api, Tenant: p.Tenant, As: p.barredLacking(t, "tenancy.org.read", "partner.supplier.read")}
	q := `{"measures": ["invoice_count"], "by": ["vendor_name"]%s}`
	storeStatus, fromStore, _ := cl.Do("POST", "/v1/analytics-queries", fmt.Sprintf(q, ""), "")
	dbStatus, fromDB, _ := cl.Do("POST", "/v1/analytics-queries", fmt.Sprintf(q, `, "as_of": "2999-01-01T00:00:00Z"`), "")
	if storeStatus != 403 || fromStore["code"] != "identity.sod_conflict" {
		t.Errorf("from the store: got %d %v, want 403 identity.sod_conflict", storeStatus, fromStore)
	}
	if storeStatus != dbStatus || fromStore["code"] != fromDB["code"] || fromStore["detail"] != fromDB["detail"] {
		t.Errorf("got %d %v from the store and %d %v from the database, want the same", storeStatus, fromStore, dbStatus, fromDB)
	}
}

// nilIfEmpty is code as a decoded body holds it: absent when "".
func nilIfEmpty(code string) any {
	if code == "" {
		return nil
	}
	return code
}

// barred is a person of every company holding the parity permissions and
// audit.chain.verify, which a segregation-of-duties rule in force pairs
// with perm until the test ends. No other person holds audit.chain.verify,
// so the rule bars this person alone.
func (p parity) barred(t *testing.T, perm string) pgtest.Actor {
	t.Helper()
	return p.barredLacking(t, perm, "")
}

// barredLacking is barred, lacking the parity permission lacking too.
func (p parity) barredLacking(t *testing.T, perm, lacking string) pgtest.Actor {
	t.Helper()
	a := p.person(t, lacking, nil)
	pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
		SELECT tenant_id, gen_random_uuid(), role_id, 'audit.chain.verify', current_date - 1, $2 FROM identity.principal_role WHERE tenant_id = $1 AND principal_id = $2`,
		p.Tenant.ID, a.ID)
	rule := uuid.New()
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
		VALUES ($1, $2, 'audit.chain.verify', $3, 'Read or verify', current_date, $4)`, p.Tenant.ID, rule, perm, a.ID)
	t.Cleanup(func() {
		pgtest.Exec(t, `DELETE FROM identity.sod_rule WHERE tenant_id = $1 AND id = $2`, p.Tenant.ID, rule)
	})
	return a
}

// lacking is a person of every company holding the parity permissions but
// perm.
func (p parity) lacking(t *testing.T, perm string) pgtest.Actor {
	t.Helper()
	return p.person(t, perm, nil)
}

// person is a person holding the parity permissions but lacking: of every
// company when companies is nil, else of those companies' legal entities.
func (p parity) person(t *testing.T, lacking string, companies []string) pgtest.Actor {
	t.Helper()
	a := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	a.Subject = "human|" + a.ID.String()
	role := uuid.New()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'Person', $5, 'active', $2, $2)`, p.Tenant.ID, a.ID, pgtest.Issuer(t), a.Subject, companies == nil)
	pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, le.id, current_date - 1, $2 FROM tenancy.legal_entity le WHERE le.tenant_id = $1 AND le.code = ANY($3)`, p.Tenant.ID, a.ID, companies)
	pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, $2, $3, 'Person', 'active', $4, $4)`,
		p.Tenant.ID, role, "person_"+a.ID.String()[24:], a.ID)
	pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, code, current_date - 1, $3 FROM unnest($4::text[]) code WHERE code <> $5`,
		p.Tenant.ID, role, a.ID, parityPerms, lacking)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`,
		p.Tenant.ID, a.ID, role)
	return a
}

// isolation: the store answers a tenant's cash actuals, forecast and
// accuracy from its own rows and a principal's from its companies' alone. Tampered with,
// it shows none of the others: the tenant's files of either dataset
// published again under another tenant's manifest give that tenant no
// rows (each row's tenant_id is the view's filter, not the key's), and a
// plan asking for a company the scope does not grant gives none of that
// company's.
func (p parity) isolation(t *testing.T) {
	ctx := context.Background()
	var key string
	pgtest.Scan(t, `SELECT manifest_key FROM analytics.snapshot WHERE tenant_id = $1`, []any{p.Tenant.ID}, &key)
	b, _, err := p.objects.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var m extract.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for dataset, want := range map[string][]string{
		"cash_actuals":   {"1000 5500 USD", "2000 28500 EUR"},
		"cash_snapshots": {"1000 70 USD", "2000 1345 EUR"},
		// The three figures of every cell at every lag added up.
		"cash_accuracy": {"1000 20790 USD", "2000 117400 EUR"},
		// The confirmed orders' items and the posted billing documents'.
		"sales_lines": {"1000 19800 USD", "2000 352506 EUR"},
	} {
		t.Run(dataset, func(t *testing.T) { p.isolated(t, key, m, dataset, want) })
	}
}

// isolated is isolation of one dataset, whose sums by company are want.
func (p parity) isolated(t *testing.T, key string, m extract.Manifest, dataset string, want []string) {
	t.Helper()
	ctx := context.Background()
	plan := wire.Plan{Dataset: dataset, Aggregate: "sum", By: []string{"company"}, FiscalYearStartMonth: 1}
	all := wire.Scope{AllCompanies: true, Datasets: []string{dataset}}
	query := func(t *testing.T, tenant uuid.UUID, key string, scope wire.Scope, plan wire.Plan) []string {
		t.Helper()
		res, err := p.r.Query(ctx, wire.Request{TenantID: tenant, ManifestKey: key, Scope: scope, Plan: plan})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range res.Rows {
			out = append(out, fmt.Sprintf("%s %d %s", strings.Join(r.By, "/"), r.Amount, r.Currency))
		}
		return out
	}
	if got := query(t, p.Tenant.ID, key, all, plan); !slices.Equal(got, want) {
		t.Fatalf("the tenant's %s by company: got %q, want %q", dataset, got, want)
	}
	t.Run("another tenant's manifest", func(t *testing.T) {
		other := uuid.New()
		if other == p.Tenant.ID {
			t.Fatal("the other tenant: got the tenant's id, want another")
		}
		ds := m.Datasets[dataset]
		ds.Files = nil
		for _, f := range m.Datasets[dataset].Files {
			content, _, err := p.objects.Get(ctx, f.Key)
			if err != nil {
				t.Fatal(err)
			}
			f.Key = strings.Replace(f.Key, "tenant="+p.Tenant.ID.String(), "tenant="+other.String(), 1)
			if err := p.objects.Put(ctx, f.Key, content, blobstore.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			ds.Files = append(ds.Files, f)
		}
		if len(ds.Files) == 0 {
			t.Fatalf("%s: got no files, want the tenant's", dataset)
		}
		forged, _ := json.Marshal(extract.Manifest{TenantID: other, Datasets: map[string]extract.Dataset{dataset: ds}}) // a Manifest always marshals
		forgedKey := extract.ManifestKey(other, 1, "0123456789abcdef")
		if err := p.objects.Put(ctx, forgedKey, forged, blobstore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := query(t, other, forgedKey, all, plan); len(got) != 0 {
			t.Errorf("the other tenant's %s: got %q, want none of tenant %s's", dataset, got, p.Tenant.ID)
		}
	})
	t.Run("a company not granted", func(t *testing.T) {
		only := wire.Scope{Companies: []string{"2000"}, Datasets: []string{dataset}}
		if got := query(t, p.Tenant.ID, key, only, plan); !slices.Equal(got, want[1:]) {
			t.Errorf("by company within 2000: got %q, want %q", got, want[1:])
		}
		tampered := plan
		tampered.Where = []wire.Filter{{Column: "company", Op: "in", Values: []string{"1000"}}}
		if got := query(t, p.Tenant.ID, key, only, tampered); len(got) != 0 {
			t.Errorf("company 1000 asked for within 2000: got %q, want none", got)
		}
	})
}
