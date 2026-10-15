package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// engine stands in for the workflow engine: the API only needs one to be
// there to hand runs over.
type engine struct{}

func (engine) Start(context.Context, workflow.Start) (bool, error) { return true, nil }
func (engine) Cancel(context.Context, string) error                { return nil }

func withEngine(e env) *integration.Endpoints {
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return app.Integration(app.Config{Pages: pages, FieldKeys: apitest.FieldKeys(), ExtractDir: e.dir, Workflows: engine{}})
}

// TestRunsWithAnEngine: with a workflow engine, :start and :reconcile hand
// the run over (running, under its operation) and Execute does the work in
// the activity's transaction: the load, the reconciliation, a source that
// cannot be read putting the run back, an operation already finished left
// alone; every database statement failing in turn is reported.
func TestRunsWithAnEngine(t *testing.T) {
	e := setup(t)
	h := withEngine(e)
	yes := func(int32) error { return nil }
	ctx := context.Background()
	if _, err := operations.Get(ctx, uuid.New()); err == nil {
		t.Fatal("operation read outside a unit of work")
	}
	good := e.connector(t, "extract.json")
	missing := e.connector(t, "nowhere.json")
	newRun := func(connector string) uuid.UUID {
		return uuid.MustParse(e.run(t, connector, "documents", false, e.US.String())["id"].(string))
	}
	hand := func(run uuid.UUID, reconcile bool) {
		t.Helper()
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			if reconcile {
				_, err := h.ReconcileRun(ctx, run, yes)
				return err
			}
			_, err := h.StartRun(ctx, run, yes)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	execute := func(run uuid.UUID) {
		t.Helper()
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { _, err := h.Execute(ctx, run); return err }); err != nil {
			t.Fatal(err)
		}
	}
	status := func(run uuid.UUID) string {
		got, _ := e.Must(200, "GET", "/v1/migration-runs/"+run.String(), "", "")
		return got["status"].(string)
	}
	// Runs in every state the engine path meets.
	queued := newRun(good)
	starting := newRun(good)
	hand(starting, false)
	loaded := newRun(good)
	hand(loaded, false)
	execute(loaded)
	reconciling := newRun(good)
	hand(reconciling, false)
	execute(reconciling)
	hand(reconciling, true)
	unreadable := newRun(missing)
	hand(unreadable, false)
	unreadableRec := newRun(good)
	hand(unreadableRec, false)
	execute(unreadableRec)
	hand(unreadableRec, true)
	// The loaded run's source goes away before its reconciliation.
	pgtest.Exec(t, `UPDATE integration.migration_run SET connector_id = $2, version = version + 1 WHERE id = $1`, unreadableRec, uuid.MustParse(missing))
	stillUnreadable := newRun(missing)
	hand(stillUnreadable, false)
	if got := []string{status(queued), status(starting), status(loaded), status(reconciling), status(unreadable), status(unreadableRec)}; strings.Join(got, " ") != "queued running loaded running running running" {
		t.Fatalf("states: %v", got)
	}
	// The source that cannot be read: back to queued (start) and loaded (reconciliation), the operation failed.
	execute(unreadable)
	execute(unreadableRec)
	if status(unreadable) != "queued" || status(unreadableRec) != "loaded" {
		t.Fatalf("after an unreadable source: %s %s", status(unreadable), status(unreadableRec))
	}
	// An operation already finished is left alone; so is a run no longer running.
	execute(loaded)
	cancelled := newRun(good)
	hand(cancelled, false)
	got, _ := e.Must(200, "GET", "/v1/migration-runs/"+cancelled.String(), "", "")
	pgtest.Exec(t, `UPDATE platform.operation SET status = 'cancelled', version = version + 1 WHERE id = $1`, uuid.MustParse(got["operation_id"].(string)))
	execute(cancelled)
	if status(cancelled) != "running" {
		t.Fatalf("cancelled operation: %s", status(cancelled))
	}
	// A disabled connector refuses the hand-over.
	write(t, e.dir, "copy.json", sap.GenerateMock(42, "2026"))
	disabledConn := e.connector(t, "copy.json")
	disabled := newRun(disabledConn)
	_, tag := e.Must(200, "GET", "/v1/connectors/"+disabledConn, "", "")
	e.Must(200, "POST", "/v1/connectors/"+disabledConn+":disable", `{"reason": "test"}`, tag)
	pgtest.WantErr(t, apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { _, err := h.StartRun(ctx, disabled, yes); return err }), "connector_disabled")
	// Every database statement failing in turn.
	for name, call := range map[string]func(ctx context.Context) error{
		"start":              func(ctx context.Context) error { _, err := h.StartRun(ctx, queued, yes); return err },
		"reconcile":          func(ctx context.Context) error { _, err := h.ReconcileRun(ctx, loaded, yes); return err },
		"execute load":       func(ctx context.Context) error { _, err := h.Execute(ctx, starting); return err },
		"execute reconcile":  func(ctx context.Context) error { _, err := h.Execute(ctx, reconciling); return err },
		"execute unreadable": func(ctx context.Context) error { _, err := h.Execute(ctx, stillUnreadable); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(ctx); err == nil {
				t.Errorf("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	// Putting an unreadable run back reports a missing audit buffer (a call
	// outside the unit of work's setup).
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: e.Tenant.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := database.SetActor(ctx, tx, e.Tenant.Human.ID); err != nil {
		t.Fatal(err)
	}
	bare := reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), reqctx.Principal{ID: e.Tenant.Human.ID, Kind: e.Tenant.Human.Kind, TenantID: e.Tenant.ID})
	if _, err := h.Execute(bare, stillUnreadable); !errors.Is(err, audit.ErrNoBuffer) {
		t.Fatalf("execute without an audit buffer: got %v, want %v", err, audit.ErrNoBuffer)
	}
}
