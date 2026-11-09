package workflows_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// A running workflow waits while its tenant is suspended (po:2026-11-09,
// ADR-0015 As built): its next step fails, to be retried, and changes
// nothing; once the tenant is active again the step goes on.

// wantInactive checks err is the tenant being suspended, as an ordinary
// error the engine retries, not an application error it might not.
func wantInactive(t *testing.T, err error) {
	t.Helper()
	var inactive *database.TenantInactiveError
	if !errors.As(err, &inactive) || inactive.Status != "suspended" {
		t.Fatalf("got %v, want the tenant's suspension", err)
	}
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		t.Fatalf("got the application error %v (non-retryable %t), want an ordinary, retryable error", app, app.NonRetryable())
	}
}

// TestActivitiesWaitWhileSuspended: each step of a release started before
// its tenant's suspension fails with the suspension and changes nothing;
// once the tenant is active again the same steps release the run, and run
// again change nothing. A proposal and a simulation, whose activities
// begin their transactions elsewhere, wait the same way.
func TestActivitiesWaitWhileSuspended(t *testing.T) {
	ctx := context.Background()
	t.Run("payment release", func(t *testing.T) {
		e := setup(t)
		a := activities(t)
		e.invoice(t, "INV-1", "100.00")
		op, run := e.propose(t, "2026-04-15")
		if err := execute(t, a, input(e, op, run), false); err != nil {
			t.Fatal(err)
		}
		in := e.releaseAsked(t, run)
		todo, err := a.PaymentsToPost(ctx, in)
		if err != nil || len(todo) != 1 {
			t.Fatalf("payments to post: got %v, %v, want one", todo, err)
		}
		before := stateOf(t, in)
		e.Tenant.SetStatus(t, "suspended")
		steps := map[string]func() error{
			"payments to post": func() error { _, err := a.PaymentsToPost(ctx, in); return err },
			"post payment":     func() error { return a.PostPayment(ctx, in, todo[0]) },
			"finish release":   func() error { return a.FinishRelease(ctx, in) },
			"fail release": func() error {
				return a.FailRelease(ctx, in, *problem.Unprocessable("payments.bank_file_invalid", "late"))
			},
		}
		for name, step := range steps {
			t.Run(name, func(t *testing.T) {
				wantInactive(t, step())
				if got := stateOf(t, in); got != before {
					t.Errorf("after the refused step: got %+v, want %+v as before", got, before)
				}
			})
		}
		resume(t, e.Tenant.ID)
		twice(t, "post payment once active", in, func() error { return a.PostPayment(ctx, in, todo[0]) })
		if done := twice(t, "finish release once active", in, func() error { return a.FinishRelease(ctx, in) }); !strings.HasPrefix(done.Run, "released/") {
			t.Errorf("run once active again: got %s, want released", done.Run)
		}
	})
	t.Run("payment proposal", func(t *testing.T) {
		e := setup(t)
		a := activities(t)
		e.invoice(t, "INV-1", "100.00")
		op, run := e.propose(t, "2026-04-15")
		e.Tenant.SetStatus(t, "suspended")
		wantInactive(t, a.Propose(ctx, input(e, op, run)))
		resume(t, e.Tenant.ID)
		if got := e.get("/v1/payment-runs/" + run); got["status"] != "proposing" {
			t.Fatalf("run after the refused proposal: got %v, want proposing", got["status"])
		}
		for range 2 {
			if err := a.Propose(ctx, input(e, op, run)); err != nil {
				t.Fatalf("proposal once active: got %v, want nil", err)
			}
		}
		if got := e.get("/v1/payment-runs/" + run); got["status"] != "proposed" {
			t.Errorf("run once active again: got %v, want proposed", got["status"])
		}
	})
	t.Run("configuration simulation", func(t *testing.T) {
		e, in, _ := simulationAsked(t)
		s := simulations(t)
		status := func() string {
			var st string
			pgtest.Scan(t, `SELECT status FROM platform.operation WHERE id = $1`, []any{in.OperationID}, &st)
			return st
		}
		e.Tenant.SetStatus(t, "suspended")
		wantInactive(t, s.Simulate(ctx, in))
		if got := status(); got != "queued" {
			t.Fatalf("simulation after the refusal: got %s, want queued", got)
		}
		resume(t, e.Tenant.ID)
		for range 2 {
			if err := s.Simulate(ctx, in); err != nil {
				t.Fatalf("simulation once active: got %v, want nil", err)
			}
		}
		if got := status(); got != "succeeded" {
			t.Errorf("simulation once active again: got %s, want succeeded", got)
		}
	})
}

// waitScenario is a two-payment release whose tenant is suspended when
// the second payment is about to be posted, and the activities' starts.
type waitScenario struct {
	e       env
	a       *workflows.Payments
	in      workflows.ReleaseInput
	mu      sync.Mutex
	started map[string]int // attempts by activity
	failed  error          // a status change in a listener that failed
}

func newWaitScenario(t *testing.T) *waitScenario {
	t.Helper()
	e := setup(t)
	a := activities(t)
	// The run is carried out by the test, never by the dispatcher, which
	// reads every tenant's queue: a release left queued (cancelled while it
	// waited) is marked as taken, so a later test's dispatcher does not
	// start it (R66's isolation, as deprSetup does).
	t.Cleanup(func() {
		pgtest.Exec(t, `UPDATE platform.operation SET workflow_id = 'carried out by the test', version = version + 1
			WHERE tenant_id = $1 AND kind = $2 AND workflow_id IS NULL`, e.Tenant.ID, payments.ReleaseKind)
	})
	e.payableVendor(t)
	e.invoice(t, "INV-1", "11.00")
	e.vendorInvoice(t, "INV-2", "13.00")
	op, run := e.propose(t, "2026-04-15")
	if err := execute(t, a, input(e, op, run), false); err != nil {
		t.Fatal(err)
	}
	return &waitScenario{e: e, a: a, in: e.releaseAsked(t, run), started: map[string]int{}}
}

// setStatus moves the tenant, as the control plane does, from the engine's
// listener: a failure is kept for the test to report.
func (s *waitScenario) setStatus(t *testing.T, status string) {
	t.Helper()
	if _, err := pgtest.Admin(t).Exec(context.Background(), `UPDATE tenancy.tenant SET status = $2, version = version + 1 WHERE id = $1`,
		s.e.Tenant.ID, status); err != nil {
		s.failed = err
	}
}

// release runs the release workflow with AwaitActive registered; on is
// called at each activity's start with its attempts so far, the scenario
// locked.
func (s *waitScenario) release(t *testing.T, on func(env *testsuite.TestWorkflowEnvironment, name string, n int)) error {
	t.Helper()
	t.Cleanup(func() { s.setStatus(t, "active") })
	err := executeRelease(t, s.a, s.in, func(env *testsuite.TestWorkflowEnvironment) {
		env.RegisterActivity(&workflows.Tenants{Pool: pgtest.Pool(t), AppRole: "erp_app"})
		env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.started[info.ActivityType.Name]++
			on(env, info.ActivityType.Name, s.started[info.ActivityType.Name])
		})
	})
	if s.failed != nil {
		t.Fatalf("set the tenant's status: %v", s.failed)
	}
	return err
}

// TestReleaseWaitsWhileSuspended: a release whose tenant is suspended
// after its first payment posted posts nothing more while suspended, hands
// the step to the wait without spending its attempts, and carries on where
// it stopped once the tenant is active again; cancelled while it waits, it
// ends cancelled with the run still releasing.
func TestReleaseWaitsWhileSuspended(t *testing.T) {
	t.Run("the run carries on once active again", func(t *testing.T) {
		s := newWaitScenario(t)
		var whileSuspended releaseState
		err := s.release(t, func(_ *testsuite.TestWorkflowEnvironment, name string, n int) {
			switch {
			case name == "PostPayment" && n == 2:
				s.setStatus(t, "suspended")
			case name == "AwaitActive" && n == 3:
				whileSuspended = stateOf(t, s.in)
				s.setStatus(t, "active")
			}
		})
		if err != nil {
			t.Fatalf("release: got %v, want nil", err)
		}
		if got := s.started["PostPayment"]; got != 1+1+1 {
			t.Errorf("attempts of PostPayment: got %d, want the first payment's, the second's one while suspended (handed to the wait at once) and its one after", got)
		}
		if got := s.started["AwaitActive"]; got != 3 {
			t.Errorf("attempts of AwaitActive: got %d, want 3", got)
		}
		done := stateOf(t, s.in)
		if !strings.HasPrefix(done.Run, "released/") || done.Files != 1 {
			t.Errorf("run once active again: got %+v, want released with its bank file", done)
		}
		if whileSuspended.Files != 0 || whileSuspended.Entries >= done.Entries || !strings.HasPrefix(whileSuspended.Run, "releasing/") {
			t.Errorf("while suspended: got %+v, want releasing with the first payment posted only (released: %+v)", whileSuspended, done)
		}
	})
	t.Run("cancelled while it waits", func(t *testing.T) {
		s := newWaitScenario(t)
		err := s.release(t, func(env *testsuite.TestWorkflowEnvironment, name string, n int) {
			switch {
			case name == "PaymentsToPost":
				s.setStatus(t, "suspended")
			case name == "AwaitActive" && n == 2:
				env.CancelWorkflow()
			}
		})
		if !temporal.IsCanceledError(err) {
			t.Fatalf("release cancelled while waiting: got %v, want cancelled", err)
		}
		if got := stateOf(t, s.in); !strings.HasPrefix(got.Run, "releasing/") || got.Files != 0 {
			t.Errorf("run of the cancelled release: got %+v, want releasing, nothing written", got)
		}
	})
}

// TestAwaitActive: the wait fails with the suspension while the tenant is
// suspended and succeeds once it is active, any number of times; it
// changes nothing.
func TestAwaitActive(t *testing.T) {
	ctx := context.Background()
	ten := pgtest.NewTenant(t)
	w := &workflows.Tenants{Pool: pgtest.Pool(t), AppRole: "erp_app"}
	ten.SetStatus(t, "suspended")
	for range 2 {
		wantInactive(t, w.AwaitActive(ctx, ten.ID))
	}
	resume(t, ten.ID)
	for range 2 {
		if err := w.AwaitActive(ctx, ten.ID); err != nil {
			t.Fatalf("once active: got %v, want nil", err)
		}
	}
}
