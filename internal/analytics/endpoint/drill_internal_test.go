package endpoint

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// drillReader answers the figure's plan with sums and a documents plan
// with one document of 2000.
type drillReader struct{ sums []wire.Row }

func (r drillReader) Query(_ context.Context, req wire.Request) (*wire.Result, error) {
	res := &wire.Result{ManifestKey: req.ManifestKey, AsOf: time.Now(), Rows: r.sums}
	if req.Plan.Aggregate == "documents" {
		res.Rows = []wire.Row{{By: []string{"2000"}, Document: uuid.NewString(), Currency: "EUR", Exponent: 2, Amount: -100}}
	}
	return res, nil
}

// TestDrillFailures: each step of a drill-through failing fails it.
func TestDrillFailures(t *testing.T) {
	pages, err := pagination.NewCodec(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	req := &gen.AnalyticsDrill{Query: gen.AnalyticsQuery{Measures: []string{"revenue"}}}
	drillWith := func(e *Endpoints, ctx context.Context, params gen.DrillAnalyticsQueryParams) error {
		_, err := e.DrillAnalyticsQuery(ctx, req, params)
		return err
	}
	drill := func(e *Endpoints, ctx context.Context) error {
		return drillWith(e, ctx, gen.DrillAnalyticsQueryParams{})
	}
	e := (&Endpoints{Configs: seed{}, Companies: codes{}, Loopback: &httpapi.Loopback{}, Pages: pages}).Init()
	pgtest.CheckErr(t, drill(e, context.Background()), "no transaction and principal")
	pgtest.CheckErr(t, drill((&Endpoints{Configs: seed{err: errors.New("no model")}}).Init(), context.Background()), "no model")
	if err := drillWith(e, context.Background(), gen.DrillAnalyticsQueryParams{PageSize: gen.NewOptInt32(0)}); problem.From(err).Code != "request.invalid_page_size" {
		t.Errorf("page size 0, which only a direct call can send: got %v, want request.invalid_page_size", err)
	}
	t.Run("permissions", func(t *testing.T) {
		inRequest(t, 2, func(ctx context.Context) { pgtest.CheckErr(t, drill(e, ctx), pgtest.ErrInjected.Error()) })
	})
	t.Run("busy", func(t *testing.T) {
		busy := (&Endpoints{Configs: seed{}, Companies: codes{}, Fallbacks: 1}).Init()
		busy.slots <- struct{}{}
		inRequest(t, 0, func(ctx context.Context) {
			if err := drill(busy, ctx); problem.From(err).Code != "analytics.busy" {
				t.Errorf("got %v, want analytics.busy", err)
			}
		})
	})
	t.Run("database path", func(t *testing.T) {
		// No claims in the context: the loopback serves no one.
		inRequest(t, 0, func(ctx context.Context) { pgtest.CheckErr(t, drill(e, ctx), "loopback") })
	})
	store := func(sums ...wire.Row) *Endpoints {
		return (&Endpoints{Configs: seed{}, Companies: codes{}, Loopback: &httpapi.Loopback{}, Pages: pages, Reader: drillReader{sums: sums}}).Init()
	}
	t.Run("a figure of two currencies", func(t *testing.T) {
		inRequestWith(t, allPermissions, 0, func(ctx context.Context) {
			fresh(t, ctx)
			two := store(wire.Row{Currency: "EUR", Exponent: 2, Amount: -100}, wire.Row{Currency: "USD", Exponent: 2, Amount: -1})
			if err := drill(two, ctx); problem.From(err).Code != "analytics.invalid_query" {
				t.Errorf("got %v, want analytics.invalid_query", err)
			}
		})
	})
	t.Run("the numbers' reads", func(t *testing.T) {
		inRequestWith(t, allPermissions, 0, func(ctx context.Context) {
			fresh(t, ctx)
			pgtest.CheckErr(t, drill(store(wire.Row{Currency: "EUR", Exponent: 2, Amount: -100}), ctx), "loopback")
		})
	})
}
