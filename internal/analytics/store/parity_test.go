package store_test

import (
	"context"
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
}

var parityPerms = []string{"ledger.entry.post", "ledger.entry.read", "ledger.account.read", "payables.invoice.read", "procurement.order.read",
	"partner.supplier.read", "tenancy.org.read"}

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

// setupParity seeds the tenant: revenue, expenses and supplier items in
// both companies, one item cleared by a reversal, a parallel ledger's
// entry, a customer, orders and invoices; then extracts its store.
func setupParity(t *testing.T) parity {
	t.Helper()
	p := parity{Env: fixture.New(t, parityPerms...), parallel: uuid.New()}
	ten := p.Tenant
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.9, 'ECB', $2)`, ten.ID, ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.ledger (tenant_id, id, code, name, is_leading, accounting_principle, created_by) VALUES ($1, $2, '2L', 'Local GAAP', false, 'HGB', $3)`,
		ten.ID, p.parallel, ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.period_status (tenant_id, id, legal_entity_id, ledger_id, calendar_id, fiscal_year, period, status, updated_by)
		SELECT s.tenant_id, gen_random_uuid(), s.legal_entity_id, $2, s.calendar_id, s.fiscal_year, s.period, s.status, s.updated_by
		FROM ledger.period_status s JOIN ledger.ledger l ON l.tenant_id = s.tenant_id AND l.id = s.ledger_id AND l.is_leading WHERE s.tenant_id = $1`,
		ten.ID, p.parallel)
	customer := uuid.New()
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, 'BP80000001', 'organization', 'A Customer', 'DE', 'active', $3, $3)`, ten.ID, customer, ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'customer', $3)`, ten.ID, customer, ten.Human.ID)

	p.post(t, p.DE, "2000", "EUR", "2026-03-05", "610000", "211000", "120.00", &p.Supplier, nil)
	p.post(t, p.DE, "2000", "EUR", "2026-03-20", "100000", "400000", "500.00", nil, nil)
	p.post(t, p.DE, "2000", "EUR", "2026-07-02", "100000", "400000", "75.25", nil, nil)
	id, tag := p.post(t, p.DE, "2000", "EUR", "2026-05-02", "520000", "211000", "30.00", &p.Vendor, nil)
	p.Must(200, "POST", "/v1/journal-entries/"+id+":reverse", `{"posting_date": "2026-05-03"}`, tag)
	p.post(t, p.US, "1000", "USD", "2026-08-10", "510000", "211000", "45.50", &p.Supplier, nil)
	p.post(t, p.US, "1000", "USD", "2026-02-11", "100000", "400000", "800.00", nil, nil)
	p.post(t, p.DE, "2000", "EUR", "2026-03-05", "610000", "211000", "999.00", &p.Supplier, &p.parallel)
	p.order(t, p.DE, p.Supplier, "2000", "EUR", "2026-03-01", "2.5", 1234)
	p.order(t, p.DE, p.Vendor, "2000", "EUR", "2026-09-15", "1", 10000)
	p.order(t, p.US, p.Supplier, "1000", "USD", "2026-04-04", "0.333333", 999)
	p.invoice(t, p.DE, p.Supplier, "EUR", "2026-03-06", "parked", 14280)
	p.invoice(t, p.DE, p.Vendor, "EUR", "2026-06-30", "rejected", 5000)
	p.invoice(t, p.US, p.Supplier, "USD", "2026-08-11", "parked", 4550)

	p.extract(t)
	srv := httptest.NewServer(p.API.Handler)
	t.Cleanup(srv.Close)
	p.db = analytics.Source{API: &apitools.Client{BaseURL: srv.URL, Token: p.API.Issuer.Token(t, ten.ID, ten.Human.Subject, nil), HTTP: srv.Client()}}
	return p
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
	x := extract.Extractor{Replica: pool.Begin, Primary: pool.Begin, Role: "erp_app", Store: objects.Store()}
	res, err := x.Run(context.Background(), p.Tenant.ID)
	if err != nil || res.Pending {
		t.Fatalf("extract: %+v, %v; want it current", res, err)
	}
	pgtest.Scan(t, `SELECT as_of FROM analytics.snapshot WHERE tenant_id = $1`, []any{p.Tenant.ID}, &p.asOf)
	r, err := reader.New(objects.Store(), t.TempDir(), currency.Seeded(), reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
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
		if slices.Contains(dims, "posting_date") {
			out[met.Name+" from March 10"] = analytics.Query{Measures: ms, By: []string{"company"}, Where: []analytics.Filter{
				{Dim: "posting_date", Op: ">=", Values: []string{"2026-03-10"}}}}
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
		dbFacts, err := p.db.Facts(context.Background(), m, p.asOf)
		if err != nil {
			t.Fatal(err)
		}
		for who, pr := range map[string]analytics.Principal{"every company": {ID: "a"}, "2000": {ID: "b", Companies: []string{"2000"}}} {
			for name, q := range queries(m, p.parallel) {
				t.Run(calendar+"/"+who+"/"+name, func(t *testing.T) { p.same(t, m, dbFacts, pr, q) })
			}
		}
	}
	t.Run("not vacuous", func(t *testing.T) {
		m := model(t)
		dbFacts, err := p.db.Facts(context.Background(), m, p.asOf)
		if err != nil {
			t.Fatal(err)
		}
		for _, met := range m.Metrics {
			res, err := analytics.Execute(m, dbFacts, analytics.Principal{}, analytics.Query{Measures: []string{met.Name}, By: []string{"company"}})
			if err != nil || len(res.Rows) != 2 {
				t.Errorf("%s by company: got %v, %v; want a row for each company", met.Name, res, err)
			}
		}
		for ledger, want := range map[string]string{"": "120.00", p.parallel.String(): "999.00"} {
			res, err := analytics.Execute(m, dbFacts, analytics.Principal{Companies: []string{"2000"}}, analytics.Query{Measures: []string{"expenses"}, Ledger: ledger})
			if err != nil || len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != want {
				t.Errorf("expenses of 2000 in ledger %q: got %v, %v; want %s", ledger, res, err, want)
			}
		}
	})
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
