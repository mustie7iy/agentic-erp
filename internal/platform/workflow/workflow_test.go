package workflow_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
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

// acts counts its calls and holds each one until release is closed, so a
// test can act while a workflow is certainly still running.
type acts struct {
	calls   atomic.Int32
	release chan struct{}
	once    sync.Once
}

func (a *acts) Shout(ctx context.Context, in string) (string, error) {
	a.calls.Add(1)
	select {
	case <-a.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return strings.ToUpper(in), nil
}

// unblock lets every held and future call finish.
func (a *acts) unblock() { a.once.Do(func() { close(a.release) }) }

// TestTemporal: the client starts a workflow once per id, cancels running
// ones, tolerates unknown ones, and a worker hosts what it registers.
func TestTemporal(t *testing.T) {
	ctx := context.Background()
	addr := workflowtest.Server(t)
	if _, err := workflow.Dial(ctx, workflow.Config{Address: "127.0.0.1:1", Namespace: "default"}); err == nil || !strings.Contains(err.Error(), "connecting to") {
		t.Fatalf("dial nothing: %v", err)
	}
	codec, _ := workflow.NewCodec([]byte(strings.Repeat("k", 32))) // payloads sealed on their way to the engine and back
	engine, err := workflow.Dial(ctx, workflow.Config{Address: addr, Namespace: "default", Codec: codec})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	a := &acts{release: make(chan struct{})}
	defer a.unblock() // a test that fails while the activity is held still lets the worker stop
	// A plain client reads the workflows' states, which carry no payloads.
	inspect, err := client.DialContext(ctx, client.Options{HostPort: addr, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer inspect.Close()
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
	pgtest.Eventually(t, 30*time.Second, func() bool { return a.calls.Load() > 0 }, "the worker did not run the activity")
	first := describe(t, inspect, id)
	if st := first.WorkflowExecutionInfo.Status; st != enums.WORKFLOW_EXECUTION_STATUS_RUNNING {
		t.Fatalf("while the activity is held: got %v, want running", st)
	}
	// The same id again starts nothing and is not an error, running or done:
	// the activity is held, so the first restart meets a running workflow,
	// and the second one meets it completed.
	restart := func(when string) {
		t.Helper()
		if started, err := engine.Start(ctx, workflow.Start{ID: id, Queue: queue, Name: "Echo", Args: []any{"again"}}); err != nil || started {
			t.Fatalf("restart %s: got %v %v, want not started and no error", when, started, err)
		}
	}
	restart("while running")
	a.unblock()
	pgtest.Eventually(t, 30*time.Second, func() bool {
		return describe(t, inspect, id).WorkflowExecutionInfo.Status == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED
	}, "the workflow did not complete")
	restart("once done")
	// The id's latest run is still the first: no restart made a run that
	// could call the activity again.
	if got, want := describe(t, inspect, id).WorkflowExecutionInfo.Execution.RunId, first.WorkflowExecutionInfo.Execution.RunId; got != want {
		t.Fatalf("latest run: got %s, want the first run %s", got, want)
	}
	if got := a.calls.Load(); got != 1 {
		t.Fatalf("activity calls: got %d, want 1", got)
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

// describe is the engine's view of the id's latest run.
func describe(t *testing.T, c client.Client, id string) *workflowservice.DescribeWorkflowExecutionResponse {
	t.Helper()
	d, err := c.DescribeWorkflowExecution(context.Background(), id, "")
	if err != nil {
		t.Fatalf("describe %s: %v", id, err)
	}
	return d
}

// TestWorkerOnAClosedClient: a worker whose client is closed cannot start,
// and the error names the queue.
func TestWorkerOnAClosedClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // a worker that did start returns when this ends
	defer cancel()
	engine, err := workflow.Dial(ctx, workflow.Config{Address: workflowtest.Server(t), Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	engine.Close()
	err = engine.Worker(ctx, "closed", func(r workflow.Registry) {
		r.RegisterWorkflowWithOptions(echo, sdkworkflow.RegisterOptions{Name: "Echo"})
	})
	if err == nil || !strings.Contains(err.Error(), "workflow: worker on queue closed") {
		t.Errorf("got %v, want the refused worker reported with its queue", err)
	}
}
