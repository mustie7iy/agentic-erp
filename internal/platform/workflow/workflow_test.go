package workflow_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow/workflowtest"
)

// echo is a workflow that runs one activity and returns its answer; wait
// is one that sleeps until cancelled.
func echo(ctx sdkworkflow.Context, in string) (string, error) {
	ctx = sdkworkflow.WithActivityOptions(ctx, sdkworkflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	var out string
	err := sdkworkflow.ExecuteActivity(ctx, "Shout", in).Get(ctx, &out)
	return out, err
}

func wait(ctx sdkworkflow.Context) error { return sdkworkflow.Sleep(ctx, time.Hour) }

type acts struct{ calls atomic.Int32 }

func (a *acts) Shout(_ context.Context, in string) (string, error) {
	a.calls.Add(1)
	return strings.ToUpper(in), nil
}

// TestTemporal: the client starts a workflow once per id, cancels running
// ones, tolerates unknown ones, and a worker hosts what it registers.
func TestTemporal(t *testing.T) {
	ctx := context.Background()
	addr := workflowtest.Server(t)
	if _, err := workflow.Dial(ctx, workflow.Config{Address: "127.0.0.1:1", Namespace: "default"}); err == nil || !strings.Contains(err.Error(), "connecting to") {
		t.Fatalf("dial nothing: %v", err)
	}
	engine, err := workflow.Dial(ctx, workflow.Config{Address: addr, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	a := &acts{}
	queue := "test-" + time.Now().Format("150405.000000000")
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- engine.Worker(workerCtx, queue, func(r workflow.Registry) {
			r.RegisterWorkflowWithOptions(echo, sdkworkflow.RegisterOptions{Name: "Echo"})
			r.RegisterWorkflowWithOptions(wait, sdkworkflow.RegisterOptions{Name: "Wait"})
			r.RegisterActivity(a)
		})
	}()
	id := "tenant-le-payment_run-" + queue
	started, err := engine.Start(ctx, workflow.Start{ID: id, Queue: queue, Name: "Echo", Args: []any{"hi"}})
	if err != nil || !started {
		t.Fatalf("start: %v %v", started, err)
	}
	for deadline := time.Now().Add(30 * time.Second); a.calls.Load() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the worker did not run the activity")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The same id again starts nothing and is not an error, running or done.
	for i := 0; i < 2; i++ {
		if started, err := engine.Start(ctx, workflow.Start{ID: id, Queue: queue, Name: "Echo", Args: []any{"again"}}); err != nil || started {
			t.Fatalf("restart %d: %v %v", i, started, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if a.calls.Load() != 1 {
		t.Fatalf("activity ran %d times", a.calls.Load())
	}
	// A waiting workflow is cancelled; an unknown one is nobody's problem.
	if started, err := engine.Start(ctx, workflow.Start{ID: id + "-wait", Queue: queue, Name: "Wait"}); err != nil || !started {
		t.Fatalf("start wait: %v %v", started, err)
	}
	if err := engine.Cancel(ctx, id+"-wait"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := engine.Cancel(ctx, "nobody-"+queue); err != nil {
		t.Fatalf("cancel unknown: %v", err)
	}
	// Refused starts and cancels are reported.
	if _, err := engine.Start(ctx, workflow.Start{ID: id + "-bad", Queue: "", Name: "Echo"}); err == nil || !strings.Contains(err.Error(), "starting") {
		t.Fatalf("start without a queue: %v", err)
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := engine.Start(gone, workflow.Start{ID: id + "-gone", Queue: queue, Name: "Echo"}); err == nil || !strings.Contains(err.Error(), "looking up") {
		t.Fatalf("start with a dead context: %v", err)
	}
	if err := engine.Cancel(gone, id+"-wait"); err == nil || !strings.Contains(err.Error(), "cancelling") {
		t.Fatalf("cancel with a dead context: %v", err)
	}
	if err := engine.Cancel(gone, id+"-wait"); !errors.Is(err, context.Canceled) && err == nil {
		t.Fatal("expected an error")
	}
	stopWorker()
	if err := <-workerDone; err != nil {
		t.Fatalf("worker: %v", err)
	}
}
