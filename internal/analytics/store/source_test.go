package store_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/analytics/store"
	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var storeAsOf = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)

// inRequest runs fn in a read-only request of tenant, as erp-server's unit
// of work runs a handler; wrap may replace the transaction.
func inRequest(t *testing.T, tenant uuid.UUID, wrap func(pgx.Tx) pgx.Tx, fn func(ctx context.Context)) {
	t.Helper()
	job := uow.Job{Begin: pgtest.Pool(t).Begin, Name: "test", Scope: database.Scope{Role: "erp_app", TenantID: tenant, ReadOnly: true}}
	if err := job.Tx(context.Background(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		if wrap != nil {
			tx = wrap(tx)
		}
		fn(reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), reqctx.Principal{ID: uuid.New(), Kind: "human", TenantID: tenant}))
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// pointer gives tenant a snapshot pointer checked at checked (nil: never).
func pointer(t *testing.T, tenant uuid.UUID, checked *time.Time) string {
	t.Helper()
	key := extract.ManifestKey(tenant, 1, "0123456789abcdef")
	pgtest.Exec(t, `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of, checked_at) VALUES ($1, $2, '1'::xid8, $3, $4)`,
		tenant, key, storeAsOf, checked)
	return key
}

type fakeReader struct {
	calls []wire.Request
	err   error
}

func (r *fakeReader) Query(_ context.Context, req wire.Request) (*wire.Result, error) {
	r.calls = append(r.calls, req)
	if r.err != nil {
		return nil, r.err
	}
	res := &wire.Result{ManifestKey: req.ManifestKey, AsOf: storeAsOf, DataThrough: fmt.Sprintf("2026-03-%02d", len(r.calls))}
	if req.Plan.Aggregate == "count_distinct" {
		res.Rows = []wire.Row{{By: []string{"2000"}, Count: 3}, {By: []string{"1000"}, Count: 0}}
	} else {
		res.Rows = []wire.Row{{By: []string{"2000"}, Currency: "EUR", Exponent: 2, Amount: 1250}}
	}
	return res, nil
}

type fakeDatabase struct {
	called int
	err    error
}

func (d *fakeDatabase) Facts(context.Context, *analytics.Model, time.Time) (*analytics.Facts, error) {
	d.called++
	return &analytics.Facts{Source: analytics.SourceDatabase}, d.err
}

func source(r store.Querier, d store.Database) *store.Source {
	return &store.Source{Reader: r, Database: d, Datasets: []string{"gl_lines", "po_lines", "ap_invoices"}, Log: slog.New(slog.DiscardHandler),
		Now: func() time.Time { return storeAsOf }}
}

func model(t *testing.T) *analytics.Model {
	t.Helper()
	m, err := analytics.DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestFromTheStore: a fresh snapshot answers from the store, a plan per
// base metric in the leading ledger unless the query names another, and
// the facts carry the store's lineage.
func TestFromTheStore(t *testing.T) {
	e := fixture.New(t)
	now := time.Now()
	key := pointer(t, e.Tenant.ID, &now)
	var leading string
	pgtest.Scan(t, `SELECT id::text FROM ledger.ledger WHERE tenant_id = $1 AND is_leading`, []any{e.Tenant.ID}, &leading)
	m := model(t)
	r, d := &fakeReader{}, &fakeDatabase{}
	q := analytics.Query{Measures: []string{"operating_profit", "purchase_order_count"}, By: []string{"company"}}
	var f *analytics.Facts
	inRequest(t, e.Tenant.ID, nil, func(ctx context.Context) {
		var err error
		if f, err = source(r, d).Facts(ctx, m, analytics.Principal{Companies: []string{"2000"}}, q, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	})
	if d.called != 0 || f.Source != analytics.SourceStore || !f.AsOf.Equal(storeAsOf) || f.DataThrough != "2026-03-03" {
		t.Fatalf("facts: got %s as of %s through %s (database called %d times), want the store's as of %s through 2026-03-03",
			f.Source, f.AsOf, f.DataThrough, d.called, storeAsOf)
	}
	if len(r.calls) != 3 {
		t.Fatalf("plans: got %d, want expenses, purchase_order_count and revenue", len(r.calls))
	}
	for _, c := range r.calls {
		if c.TenantID != e.Tenant.ID || c.ManifestKey != key || c.Plan.Ledger != leading || c.Plan.FiscalYearStartMonth != 1 || c.Scope.AllCompanies ||
			len(c.Scope.Companies) != 1 {
			t.Errorf("request: got %+v, want the tenant's manifest, the leading ledger %s, company 2000 alone", c, leading)
		}
	}
	if got := f.Aggregated["purchase_order_count"]; len(got) != 1 || got[0].Count != 3 {
		t.Errorf("counts: got %+v, want 3 for 2000 and no group counting 0", got)
	}
	if f.Exponents["EUR"] != 2 {
		t.Errorf("exponents: got %v, want EUR 2", f.Exponents)
	}
	res, err := analytics.Execute(m, f, analytics.Principal{Companies: []string{"2000"}}, q)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != analytics.SourceStore || len(res.Rows) != 1 {
		t.Errorf("result: got %s with rows %v, want the store's, one row", res.Source, res.Rows)
	}
	t.Run("asked ledger", func(t *testing.T) {
		r := &fakeReader{}
		other := uuid.NewString()
		inRequest(t, e.Tenant.ID, nil, func(ctx context.Context) {
			if _, err := source(r, d).Facts(ctx, m, analytics.Principal{}, analytics.Query{Measures: []string{"revenue"}, Ledger: other}, time.Time{}); err != nil {
				t.Fatal(err)
			}
		})
		if len(r.calls) != 1 || r.calls[0].Plan.Ledger != other || !r.calls[0].Scope.AllCompanies {
			t.Errorf("request: got %+v, want ledger %s and every company", r.calls, other)
		}
	})
}

// TestFallsBack: the database answers when the store cannot.
func TestFallsBack(t *testing.T) {
	m := model(t)
	now := time.Now()
	hourAgo, inAnHour := now.Add(-time.Hour), now.Add(time.Hour)
	for name, tc := range map[string]struct {
		checked   *time.Time
		none      bool
		notBefore time.Time
		target    time.Duration
		reader    error
		measure   string
	}{
		"no snapshot":       {none: true},
		"never checked":     {},
		"stale":             {checked: &hourAgo},
		"older than asked":  {checked: &now, notBefore: inAnHour},
		"reader fails":      {checked: &now, reader: errors.New("busy")},
		"plan fails":        {checked: &now, measure: "nope"},
		"entry it lacks":    {checked: &inAnHour, target: time.Millisecond}, // posted longer ago than the target, at the watermark
		"entry within time": {checked: &inAnHour, target: time.Hour, reader: errors.New("reached the reader")},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixture.New(t, "ledger.entry.post")
			if !tc.none {
				pointer(t, f.Tenant.ID, tc.checked)
			}
			if tc.target > 0 {
				f.Must(201, "POST", "/v1/journal-entries", entry(f.DE, f.Account(t, "2000", "610000"), f.Account(t, "2000", "211000"), f.Supplier), "")
				pgtest.Eventually(t, time.Minute, func() bool {
					var old bool
					pgtest.Scan(t, `SELECT now() - max(posted_at) > interval '10 milliseconds' FROM ledger.journal_entry WHERE tenant_id = $1`, []any{f.Tenant.ID}, &old)
					return old
				}, "entry not yet older than the target")
			}
			r, d := &fakeReader{err: tc.reader}, &fakeDatabase{}
			s := source(r, d)
			s.Target = tc.target
			measure := cmpOr(tc.measure, "revenue")
			inRequest(t, f.Tenant.ID, nil, func(ctx context.Context) {
				facts, err := s.Facts(ctx, m, analytics.Principal{}, analytics.Query{Measures: []string{measure}}, tc.notBefore)
				if err != nil || facts.Source != analytics.SourceDatabase || d.called != 1 {
					t.Errorf("facts: got %v from %v (database called %d times), want the database's", err, facts, d.called)
				}
			})
			if name == "entry within time" && len(r.calls) != 1 {
				t.Errorf("reader: got %d calls, want 1: the entry is younger than the target", len(r.calls))
			}
		})
	}
}

func cmpOr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// entry is a journal entry of company 2000: an expense debited, the
// supplier credited.
func entry(le, expense, payables, supplier uuid.UUID) string {
	return `{"legal_entity_id": "` + le.String() + `", "document_date": "2026-03-05", "posting_date": "2026-03-05", "currency": "EUR",
		"lines": [{"account_id": "` + expense.String() + `", "amount": {"amount": "12.00", "currency": "EUR"}},
		{"account_id": "` + payables.String() + `", "amount": {"amount": "-12.00", "currency": "EUR"}, "partner_id": "` + supplier.String() + `"}]}`
}

// scripted answers every plan with rows, as of asOf.
type scripted struct {
	rows []wire.Row
	asOf []time.Time
	n    int
}

func (s *scripted) Query(_ context.Context, req wire.Request) (*wire.Result, error) {
	s.n++
	return &wire.Result{ManifestKey: req.ManifestKey, AsOf: s.asOf[(s.n-1)%len(s.asOf)], Rows: s.rows}, nil
}

// TestReaderAnswers: an answer that does not fit the plan, or names a
// currency with two exponents, is answered from the database; the store's
// as_of is the oldest of its answers; a source without a logger or clock
// still falls back.
func TestReaderAnswers(t *testing.T) {
	e := fixture.New(t)
	now := time.Now()
	pointer(t, e.Tenant.ID, &now)
	m := model(t)
	older := storeAsOf.Add(-time.Hour)
	eur := wire.Row{By: []string{"2000"}, Currency: "EUR", Exponent: 2, Amount: 100}
	for name, tc := range map[string]struct {
		reader *scripted
		db     bool
	}{
		"short row":     {&scripted{rows: []wire.Row{{Currency: "EUR", Exponent: 2}}, asOf: []time.Time{storeAsOf}}, true},
		"two exponents": {&scripted{rows: []wire.Row{eur, {By: []string{"1000"}, Currency: "EUR", Exponent: 3}}, asOf: []time.Time{storeAsOf}}, true},
		"oldest as of":  {&scripted{rows: []wire.Row{eur}, asOf: []time.Time{storeAsOf, older}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			d := &fakeDatabase{}
			s := source(tc.reader, d)
			s.Log, s.Now = nil, nil
			inRequest(t, e.Tenant.ID, nil, func(ctx context.Context) {
				f, err := s.Facts(ctx, m, analytics.Principal{}, analytics.Query{Measures: []string{"operating_profit"}, By: []string{"company"}}, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				if got := f.Source == analytics.SourceDatabase; got != tc.db {
					t.Errorf("from the database: got %v, want %v", got, tc.db)
				}
				if !tc.db && !f.AsOf.Equal(older) {
					t.Errorf("as of: got %s, want the oldest answer's %s", f.AsOf, older)
				}
			})
		})
	}
}

// TestFailures: a request without a unit of work, a failed pointer read
// and a failed database read are errors, not answers.
func TestFailures(t *testing.T) {
	e := fixture.New(t)
	m := model(t)
	q := analytics.Query{Measures: []string{"revenue"}}
	if _, err := source(&fakeReader{}, &fakeDatabase{}).Facts(context.Background(), m, analytics.Principal{}, q, time.Time{}); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("outside a request: got %v, want ErrOutsideRequest", err)
	}
	inRequest(t, e.Tenant.ID, func(tx pgx.Tx) pgx.Tx { return &pgtest.FailingTx{Tx: tx, FailAt: 1} }, func(ctx context.Context) {
		if _, err := source(&fakeReader{}, &fakeDatabase{}).Facts(ctx, m, analytics.Principal{}, q, time.Time{}); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("pointer: got %v, want the injected failure", err)
		}
	})
	inRequest(t, e.Tenant.ID, nil, func(ctx context.Context) {
		if _, err := source(&fakeReader{}, &fakeDatabase{err: errors.New("down")}).Facts(ctx, m, analytics.Principal{}, q, time.Time{}); err == nil {
			t.Error("database: got nil error, want its error")
		}
	})
}
