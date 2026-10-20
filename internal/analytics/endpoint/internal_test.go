package endpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func TestSlots(t *testing.T) {
	e := (&Endpoints{Fallbacks: 1}).Init()
	release, err := e.slot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.slot(); problem.From(err).Code != "analytics.busy" {
		t.Errorf("second query: got %v, want analytics.busy", err)
	}
	release()
	if _, err := e.slot(); err != nil {
		t.Errorf("after the release: got %v, want a slot", err)
	}
	if got := cap((&Endpoints{}).Init().slots); got != 2 {
		t.Errorf("default slots: got %d, want 2", got)
	}
}

// codes answers company codes, or fails.
type codes struct{ err error }

func (c codes) CompanyCode(context.Context, uuid.UUID) (string, bool, error) {
	return "2000", true, c.err
}

// inRequest runs fn with a request's transaction and principal: the
// tenant's agent, given access to one company; the transaction fails its
// statements from failAt on (0: none).
func inRequest(t *testing.T, failAt int, fn func(ctx context.Context)) {
	t.Helper()
	ten := pgtest.NewTenant(t, "analytics.query.run")
	pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), $2, gen_random_uuid(), current_date - 1, $3)`, ten.ID, ten.Agent.ID, ten.Human.ID)
	ctx := context.Background()
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only work: nothing to keep
	reqTx := tx
	if failAt > 0 {
		reqTx = &pgtest.FailingTx{Tx: tx, FailAt: failAt}
	}
	fn(reqctx.WithPrincipal(reqctx.WithTx(ctx, reqTx), reqctx.Principal{ID: ten.Agent.ID, Kind: reqctx.KindAgent, TenantID: ten.ID}))
}

// TestFailures: each step of a query failing fails the query.
func TestFailures(t *testing.T) {
	m, _ := analytics.DefaultModel()
	req := &gen.AnalyticsQuery{Measures: []string{"revenue"}}
	run := func(e *Endpoints, ctx context.Context) error {
		_, err := e.RunAnalyticsQuery(ctx, req, gen.RunAnalyticsQueryParams{})
		return err
	}
	e := (&Endpoints{Model: m, Companies: codes{}, Loopback: &httpapi.Loopback{}}).Init()
	pgtest.CheckErr(t, run(e, context.Background()), "no transaction and principal")
	t.Run("company code", func(t *testing.T) {
		inRequest(t, 0, func(ctx context.Context) {
			failing := (&Endpoints{Model: m, Companies: codes{errors.New("directory down")}}).Init()
			pgtest.CheckErr(t, run(failing, ctx), "directory down")
		})
	})
	t.Run("permissions", func(t *testing.T) {
		inRequest(t, 2, func(ctx context.Context) { pgtest.CheckErr(t, run(e, ctx), pgtest.ErrInjected.Error()) })
	})
	t.Run("busy", func(t *testing.T) {
		busy := (&Endpoints{Model: m, Companies: codes{}, Fallbacks: 1}).Init()
		busy.slots <- struct{}{}
		inRequest(t, 0, func(ctx context.Context) {
			if err := run(busy, ctx); problem.From(err).Code != "analytics.busy" {
				t.Errorf("got %v, want analytics.busy", err)
			}
		})
	})
	t.Run("database path", func(t *testing.T) {
		// No claims in the context: the loopback serves no one.
		inRequest(t, 0, func(ctx context.Context) { pgtest.CheckErr(t, run(e, ctx), "loopback") })
	})
}
