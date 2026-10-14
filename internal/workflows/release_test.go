package workflows_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// releaseState is what a release activity changes: the run and the
// operation (status and version), the run's bank files, and the tenant's
// journal entries, events and audit records.
type releaseState struct {
	Run, Operation                  string
	Files, Entries, Events, Audited int
}

func stateOf(t *testing.T, in workflows.ReleaseInput) releaseState {
	t.Helper()
	var s releaseState
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT r.status || '/' || r.version, o.status || '/' || o.version,
			(SELECT count(*) FROM payments.bank_file f WHERE f.tenant_id = r.tenant_id AND f.payment_run_id = r.id),
			(SELECT count(*) FROM ledger.journal_entry j WHERE j.tenant_id = r.tenant_id),
			(SELECT count(*) FROM payments.outbox x WHERE x.tenant_id = r.tenant_id) + (SELECT count(*) FROM platform.outbox x WHERE x.tenant_id = r.tenant_id)
				+ (SELECT count(*) FROM ledger.outbox x WHERE x.tenant_id = r.tenant_id),
			(SELECT count(*) FROM audit.audit_event a WHERE a.tenant_id = r.tenant_id)
		FROM payments.payment_run r JOIN platform.operation o ON o.tenant_id = r.tenant_id AND o.id = $2 WHERE r.id = $1`, in.RunID, in.OperationID).
		Scan(&s.Run, &s.Operation, &s.Files, &s.Entries, &s.Events, &s.Audited); err != nil {
		t.Fatalf("read the release state: %v", err)
	}
	return s
}

// twice runs an activity whose first run committed and whose completion was
// lost, so it runs again: both runs succeed and the second changes nothing.
func twice(t *testing.T, name string, in workflows.ReleaseInput, activity func() error) releaseState {
	t.Helper()
	if err := activity(); err != nil {
		t.Fatalf("%s, first run: got %v, want nil", name, err)
	}
	first := stateOf(t, in)
	if err := activity(); err != nil {
		t.Fatalf("%s, run again: got %v, want nil", name, err)
	}
	if again := stateOf(t, in); again != first {
		t.Fatalf("%s, run again: got %+v, want %+v as after the first run", name, again, first)
	}
	return first
}

// TestReleaseActivitiesRunTwice: each release activity whose transaction
// committed but whose completion was lost runs again as a no-op success (ADR-0012):
// the same run, operation, bank file, postings, events and audit records.
func TestReleaseActivitiesRunTwice(t *testing.T) {
	e := setup(t)
	a := activities(t)
	ctx := context.Background()
	e.invoice(t, "INV-1", "100.00")
	op, run := e.propose(t, "2026-04-15")
	if err := execute(t, a, input(e, op, run), false); err != nil {
		t.Fatal(err)
	}
	in := e.releaseAsked(t, run)
	todo, err := a.PaymentsToPost(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, pay := range todo {
		twice(t, "post payment", in, func() error { return a.PostPayment(ctx, in, pay) })
	}
	var stale payments.Run // the run as it was before its release finished
	if err := apitest.InRequest(t, e.Tenant, e.Approver, func(ctx context.Context) (err error) {
		stale, err = payments.GetRun(ctx, in.RunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	done := twice(t, "finish release", in, func() error { return a.FinishRelease(ctx, in) })
	if !strings.HasPrefix(done.Run, "released/") {
		t.Fatalf("run after the release: got %s, want released", done.Run)
	}
	if done.Files != 1 {
		t.Fatalf("bank files of the run: got %d, want 1", done.Files)
	}
	// The released run: every activity of a late or overlapping workflow is a no-op.
	left, err := a.PaymentsToPost(ctx, in)
	if err != nil {
		t.Fatalf("payments to post of a released run: got %v, want nil", err)
	}
	if len(left) != 0 {
		t.Fatalf("payments to post of a released run: got %v, want none", left)
	}
	for _, pay := range todo {
		twice(t, "post payment of a released run", in, func() error { return a.PostPayment(ctx, in, pay) })
	}
	if got := twice(t, "fail a released run", in, func() error {
		return a.FailRelease(ctx, in, *problem.Unprocessable("payments.bank_file_invalid", "late"))
	}); got != done {
		t.Fatalf("after a late failure: got %+v, want %+v", got, done)
	}
	// Called with the run as it was, the release is refused by its status guard, and a late failure finds the operation finished.
	pgtest.WantErr(t, apitest.InRequest(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		_, err := a.Endpoints.FinishRelease(ctx, stale)
		return err
	}), "no longer releasing")
	if err := apitest.InRequest(t, e.Tenant, e.Approver, func(ctx context.Context) error {
		_, err := a.Endpoints.FailRelease(ctx, stale, problem.Unprocessable("payments.bank_file_invalid", "late"))
		return err
	}); err != nil {
		t.Fatalf("fail a finished release: got %v, want nil", err)
	}
	if got := stateOf(t, in); got != done {
		t.Fatalf("after the stale calls: got %+v, want %+v", got, done)
	}
}

// TestFailReleaseRunsTwice: a failure recorded once is not recorded again,
// whether the run kept its posted payments (releasing) or went back to
// proposed with nothing posted.
func TestFailReleaseRunsTwice(t *testing.T) {
	e := setup(t)
	a := activities(t)
	ctx := context.Background()
	why := *problem.Unprocessable("payments.bank_file_invalid", "test")
	e.invoice(t, "INV-1", "100.00")
	op, run := e.propose(t, "2026-04-15")
	if err := execute(t, a, input(e, op, run), false); err != nil {
		t.Fatal(err)
	}
	in := e.releaseAsked(t, run)
	if got := twice(t, "fail with nothing posted", in, func() error { return a.FailRelease(ctx, in, why) }); !strings.HasPrefix(got.Run, "proposed/") {
		t.Fatalf("run after a failure with nothing posted: got %s, want proposed", got.Run)
	}
	in = e.releaseAsked(t, run)
	todo, err := a.PaymentsToPost(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.PostPayment(ctx, in, todo[0]); err != nil {
		t.Fatal(err)
	}
	if got := twice(t, "fail with a payment posted", in, func() error { return a.FailRelease(ctx, in, why) }); !strings.HasPrefix(got.Run, "releasing/") {
		t.Fatalf("run after a failure with a payment posted: got %s, want releasing", got.Run)
	}
	// The read of the operation failing is reported, for the retry policy.
	pool := pgtest.Pool(t)
	a.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	pgtest.WantErr(t, a.FailRelease(ctx, in, why), "database down")
	a.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil {
			return nil, err
		}
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil
	}
	if err := a.FailRelease(ctx, in, why); !errors.Is(err, pgtest.ErrInjected) || !strings.Contains(err.Error(), "read the release of run") {
		t.Fatalf("fail with the read failing: got %v, want the injected failure", err)
	}
}

// TestOverlappingFinishRelease: two workflows finishing the same release at
// once serialise on the run: one writes the bank file and releases the run,
// the other finds it released and does nothing.
func TestOverlappingFinishRelease(t *testing.T) {
	e := setup(t)
	a := activities(t)
	ctx := context.Background()
	e.invoice(t, "INV-1", "100.00")
	op, run := e.propose(t, "2026-04-15")
	if err := execute(t, a, input(e, op, run), false); err != nil {
		t.Fatal(err)
	}
	in := e.releaseAsked(t, run)
	todo, err := a.PaymentsToPost(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, pay := range todo {
		if err := a.PostPayment(ctx, in, pay); err != nil {
			t.Fatal(err)
		}
	}
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = a.FinishRelease(ctx, in) }()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("finish release %d: got %v, want nil", i, err)
		}
	}
	if got := stateOf(t, in); got.Files != 1 {
		t.Fatalf("bank files of the run: got %d, want 1", got.Files)
	}
	if n := len(apitest.Published(t, e.Tenant, outbox.Event{Context: "payments", Name: "payment_run_released"})); n != 1 {
		t.Fatalf("payment_run_released events: got %d, want 1", n)
	}
}

// TestCancelledReleaseResumes: a release operation cancelled while the
// release goes on keeps its status. With nothing posted the run goes back
// to proposed; with payments posted (after the cancellation too) it stays
// releasing, and :release resumes it as it does a failed release.
func TestCancelledReleaseResumes(t *testing.T) {
	e := setup(t)
	a := activities(t)
	ctx := context.Background()
	why := *problem.Unprocessable("payments.bank_file_invalid", "test")
	cancel := func(in workflows.ReleaseInput) {
		e.Must(200, "POST", "/v1/operations/"+in.OperationID.String()+":cancel", "", "*")
	}
	operation := func(in workflows.ReleaseInput) string {
		got, _ := e.Must(200, "GET", "/v1/operations/"+in.OperationID.String(), "", "")
		return got["status"].(string)
	}
	e.invoice(t, "INV-1", "100.00")
	op, run := e.propose(t, "2026-04-15")
	if err := execute(t, a, input(e, op, run), false); err != nil {
		t.Fatal(err)
	}
	in := e.releaseAsked(t, run)
	cancel(in)
	if got := twice(t, "fail a cancelled release with nothing posted", in, func() error { return a.FailRelease(ctx, in, why) }); !strings.HasPrefix(got.Run, "proposed/") {
		t.Fatalf("run after a cancelled release with nothing posted: got %s, want proposed", got.Run)
	}
	if got := operation(in); got != "cancelled" {
		t.Fatalf("operation after the failure: got %s, want cancelled", got)
	}
	// Cancelled before its payment posts: the posting stands, the operation stays cancelled, the bank file is refused.
	in = e.releaseAsked(t, run)
	todo, err := a.PaymentsToPost(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	cancel(in)
	if err := a.PostPayment(ctx, in, todo[0]); err != nil {
		t.Fatal(err)
	}
	var app *temporal.ApplicationError
	if err := a.FinishRelease(ctx, in); !errors.As(err, &app) || app.Type() != workflows.ProblemError {
		t.Fatalf("finish a cancelled release: got %v, want a problem", err)
	}
	got := twice(t, "fail a cancelled release with a payment posted", in, func() error { return a.FailRelease(ctx, in, why) })
	if !strings.HasPrefix(got.Run, "releasing/") || got.Files != 0 {
		t.Fatalf("run after a cancelled release with a payment posted: got %+v, want releasing without a bank file", got)
	}
	if got := operation(in); got != "cancelled" {
		t.Fatalf("operation after the posting: got %s, want cancelled", got)
	}
	resumed := e.releaseAsked(t, run)
	if err := executeRelease(t, a, resumed); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, resumed); !strings.HasPrefix(got.Run, "released/") || !strings.HasPrefix(got.Operation, "succeeded/") || got.Files != 1 {
		t.Fatalf("resumed release: got %+v, want released, succeeded, one bank file", got)
	}
}

// TestStaleReleaseActivities: the activities of a workflow whose release
// operation was cancelled and taken over by a resumed :release do nothing
// and succeed. They neither post, report progress on, nor finish the newer
// operation, which the resumed workflow then completes on its own.
func TestStaleReleaseActivities(t *testing.T) {
	e := setup(t)
	a := activities(t)
	ctx := context.Background()
	e.invoice(t, "INV-1", "100.00")
	op, run := e.propose(t, "2026-04-15")
	if err := execute(t, a, input(e, op, run), false); err != nil {
		t.Fatal(err)
	}
	stale := e.releaseAsked(t, run)
	todo, err := a.PaymentsToPost(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	e.Must(200, "POST", "/v1/operations/"+stale.OperationID.String()+":cancel", "", "*")
	resumed := e.releaseAsked(t, run)
	before := stateOf(t, resumed)
	left, err := a.PaymentsToPost(ctx, stale)
	if err != nil {
		t.Fatalf("payments to post of a stale release: got %v, want nil", err)
	}
	if len(left) != 0 {
		t.Fatalf("payments to post of a stale release: got %v, want none", left)
	}
	for _, pay := range todo {
		if err := a.PostPayment(ctx, stale, pay); err != nil {
			t.Fatalf("post a payment of a stale release: got %v, want nil", err)
		}
	}
	if err := a.FinishRelease(ctx, stale); err != nil {
		t.Fatalf("finish a stale release: got %v, want nil", err)
	}
	if err := a.FailRelease(ctx, stale, *problem.Unprocessable("payments.bank_file_invalid", "late")); err != nil {
		t.Fatalf("fail a stale release: got %v, want nil", err)
	}
	if got := stateOf(t, resumed); got != before {
		t.Fatalf("resumed release after the stale activities: got %+v, want %+v", got, before)
	}
	if err := executeRelease(t, a, resumed); err != nil {
		t.Fatal(err)
	}
	got := stateOf(t, resumed)
	if !strings.HasPrefix(got.Run, "released/") {
		t.Errorf("resumed run: got %s, want released", got.Run)
	}
	if !strings.HasPrefix(got.Operation, "succeeded/") {
		t.Errorf("resumed operation: got %s, want succeeded", got.Operation)
	}
	if got.Files != 1 {
		t.Errorf("bank files of the resumed run: got %d, want 1", got.Files)
	}
}
