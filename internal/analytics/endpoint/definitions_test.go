package endpoint

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// section is the seed's analytics section with change applied to a copy.
func section(t *testing.T, change func(*config.Analytics)) *config.Config {
	t.Helper()
	m, err := analytics.DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	a := m.Analytics
	a.Metrics = append([]config.Metric(nil), a.Metrics...)
	change(&a)
	return &config.Config{Analytics: &a}
}

// metric is the section's metric named name.
func metric(a *config.Analytics, name string) *config.Metric {
	for i := range a.Metrics {
		if a.Metrics[i].Name == name {
			return &a.Metrics[i]
		}
	}
	return nil
}

// broken is a section that does not read: a metric without a source.
func broken(a *config.Analytics) {
	a.Metrics = append(a.Metrics, config.Metric{Name: "zz", Type: analytics.Money})
}

func TestSection(t *testing.T) {
	for name, c := range map[string]struct {
		doc      *config.Config
		findings int
	}{
		"no section": {&config.Config{}, 0},
		"valid":      {section(t, func(*config.Analytics) {}), 0},
		"invalid":    {section(t, broken), 1},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Section{}.Check(context.Background(), nil, c.doc)
			if err != nil || len(got) != c.findings {
				t.Errorf("got %v, %v; want %d findings", got, err, c.findings)
			}
		})
	}
	if err := (Section{}).Apply(context.Background(), &config.Config{}); err != nil {
		t.Errorf("apply: got %v, want nothing to do", err)
	}
}

// docs answers fixed documents, or fails.
type docs struct {
	doc *config.Config
	err error
}

func (d docs) Document(context.Context) (*config.Config, int32, error) { return d.doc, 3, d.err }

func (d docs) Version(context.Context, int32) (*config.Config, error) { return d.doc, d.err }

// TestModel: the model is the active version's or the one asked for; a
// failed read or a version that does not read fails the query.
func TestModel(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		configs Configs
		version int32
		want    string
	}{
		"active down":  {docs{err: errors.New("down")}, 0, "down"},
		"version down": {docs{err: errors.New("down")}, 2, "down"},
		"unreadable":   {docs{doc: section(t, broken)}, 2, "semantic model of configuration version 2"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := (&Endpoints{Configs: c.configs}).model(ctx, c.version)
			pgtest.CheckErr(t, err, c.want)
		})
	}
	if _, n, err := (&Endpoints{Configs: docs{}}).model(ctx, 0); err != nil || n != 3 {
		t.Errorf("active: got version %d, %v; want 3", n, err)
	}
	e := (&Endpoints{Configs: docs{err: problem.Conflict("config.not_bootstrapped", "none")}}).Init()
	if _, err := e.RunAnalyticsQuery(ctx, &gen.AnalyticsQuery{Measures: []string{"revenue"}}, gen.RunAnalyticsQueryParams{}); problem.From(err).Code != "config.not_bootstrapped" {
		t.Errorf("query: got %v, want config.not_bootstrapped", err)
	}
}

// reader answers every plan with one group per row it is given.
type reader struct{ rows []wire.Row }

func (r reader) Query(_ context.Context, req wire.Request) (*wire.Result, error) {
	return &wire.Result{ManifestKey: req.ManifestKey, AsOf: time.Now(), Rows: r.rows}, nil
}

var allPermissions = []string{"analytics.query.run", "ledger.entry.read", "tenancy.org.read", "partner.supplier.read", "ledger.account.read",
	"payables.invoice.read", "procurement.order.read"}

// fresh gives the request's tenant a snapshot checked now.
func fresh(t *testing.T, ctx context.Context) {
	t.Helper()
	p, _ := reqctx.PrincipalFrom(ctx)
	pgtest.Exec(t, `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of, checked_at) VALUES ($1, $2, '1'::xid8, now(), now())`,
		p.TenantID, extract.ManifestKey(p.TenantID, 1, "0123456789abcdef"))
}

// TestSimulation: a change of no metric's meaning passes alone; models
// that do not read, a principal outside a request, a permission the
// metrics' data needs and a query the semantic layer refuses fail it; a
// store that cannot answer is a failed simulation; values that cannot be
// computed are blank on both sides.
func TestSimulation(t *testing.T) {
	ctx := context.Background()
	margin := section(t, func(a *config.Analytics) {
		metric(a, "operating_margin").Derived = &config.Derived{Op: "divide", Left: "operating_profit", Right: "expenses"}
	})
	invoices := section(t, func(a *config.Analytics) {
		metric(a, "invoice_count").Filter = map[string][]string{"invoice_status": {"posted"}}
	})
	sim := func(r reader) Simulation {
		return Simulation{Endpoints: &Endpoints{Companies: codes{}, Reader: r}, Now: func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }}
	}
	if got, err := sim(reader{}).Simulate(ctx, nil, section(t, func(a *config.Analytics) { metric(a, "revenue").Label = "Sales" })); err != nil || !got.Passed || got.Summary != "" {
		t.Errorf("words only: got %+v, %v; want a silent pass", got, err)
	}
	pgtest.CheckErr(t, errOf(sim(reader{}).Simulate(ctx, section(t, broken), nil)), "the base model")
	pgtest.CheckErr(t, errOf(sim(reader{}).Simulate(ctx, nil, section(t, broken))), "the proposed model")
	pgtest.CheckErr(t, errOf(sim(reader{}).Simulate(ctx, nil, margin)), "no transaction and principal")
	inRequest(t, 0, func(ctx context.Context) {
		got, err := Simulation{Endpoints: &Endpoints{Companies: codes{}}}.Simulate(ctx, nil, margin)
		if err != nil || got.Passed || !strings.Contains(got.Summary, "operating_margin could not be computed") {
			t.Errorf("no store: got %+v, %v; want a failed simulation", got, err)
		}
		if problem.From(errOf(sim(reader{}).Simulate(ctx, nil, invoices))).Status != 403 {
			t.Error("invoices without payables.invoice.read: want 403")
		}
	})
	inRequestWith(t, allPermissions, 0, func(ctx context.Context) {
		fresh(t, ctx)
		zero := reader{rows: []wire.Row{{By: []string{"2000", "2026"}, Currency: "EUR", Exponent: 2}}}
		got, err := sim(zero).Simulate(ctx, nil, margin)
		if err != nil || !got.Passed || !strings.Contains(got.Summary, "operating_margin: 0 of 1 values") {
			t.Errorf("a ratio over zero: got %+v, %v; want blank values that do not differ", got, err)
		}
		added := section(t, func(a *config.Analytics) {
			a.Metrics = append(a.Metrics, config.Metric{Name: "aa_total", Type: analytics.Money, Source: "gl_lines", Aggregate: "sum"})
		})
		mixed := reader{rows: []wire.Row{{Currency: "EUR", Exponent: 2, Amount: 1}, {Currency: "USD", Exponent: 2, Amount: 1}}}
		pgtest.CheckErr(t, errOf(sim(mixed).Simulate(ctx, nil, added)), "simulate aa_total")
	})
}

// errOf is a simulation's error.
func errOf(_ any, err error) error { return err }
