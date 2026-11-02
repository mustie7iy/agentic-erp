package main

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow/workflowtest"
)

// holder is an activity that is running when the server's shutdown
// begins: once its worker is stopping it counts the replicas recorded,
// as an activity reading the configuration would be counted (ADR-0038),
// and notes whether its context was still live.
type holder struct {
	admin   func(ctx context.Context) (int, error) // counts config.replica's rows
	running chan struct{}
	mu      sync.Mutex
	rows    int
	err     error // the count's failure, or the context's at the count
	done    bool
}

func (h *holder) Hold(ctx context.Context) error {
	close(h.running)
	<-activity.GetWorkerStopChannel(ctx)
	rows, err := h.admin(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rows, h.err, h.done = rows, err, true
	return err
}

// hold is the workflow that runs Hold once.
func hold(ctx sdkworkflow.Context) error {
	ctx = sdkworkflow.WithActivityOptions(ctx, sdkworkflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	return sdkworkflow.ExecuteActivity(ctx, "Hold").Get(ctx, nil)
}

// TestTheRecordOutlivesTheWorkflowWorker: an activity still running when
// the shutdown begins finishes, its context live and the replica's record
// still there, before the worker returns, and the record is deleted only
// after it (review:T72m). A worker given no stop timeout cancels the
// activity at once and returns without waiting for it, so the activity's
// count would fail or come after the deletion. The worker here is the
// server's engine's, with the stop timeout run gives it
// (ERP_SHUTDOWN_TIMEOUT, 5s in environment), on a queue of the test's.
func TestTheRecordOutlivesTheWorkflowWorker(t *testing.T) {
	env := environment(t, authntest.New(t), "")
	env["ERP_TEMPORAL_ADDRESS"] = workflowtest.Server(t)
	env["ERP_TEMPORAL_TASK_QUEUE"] = "erp-test-" + uuid.NewString()[:8]
	env["ERP_TEMPORAL_CODEC_KEY"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32)))
	before := replicaRows(t)
	conn := pgtest.Admin(t) // the activity's alone, used once
	h := &holder{running: make(chan struct{}), admin: func(ctx context.Context) (n int, err error) {
		err = conn.QueryRow(ctx, `SELECT count(*) FROM config.replica`).Scan(&n)
		return n, err
	}}
	queue := "probe-" + uuid.NewString()[:8]
	var s *server
	startWorkersHook = func(srv *server, runCtx context.Context) {
		s = srv
		srv.startWorkers(runCtx)
		srv.spawnCritical("probe worker", func() error {
			return srv.engine.Worker(runCtx, queue, func(r workflow.Registry) {
				r.RegisterWorkflowWithOptions(hold, sdkworkflow.RegisterOptions{Name: "Hold"})
				r.RegisterActivity(h)
			})
		})
	}
	defer func() { startWorkersHook = (*server).startWorkers }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs pgtest.SyncBuffer // the server logs on its goroutines while the test reads
	var startErr error
	started := func(net.Listener, net.Listener) {
		defer cancel() // the shutdown begins with the activity running
		if _, startErr = s.engine.Start(ctx, workflow.Start{ID: queue, Queue: queue, Name: "Hold"}); startErr != nil {
			return
		}
		select {
		case <-h.running:
		case <-time.After(30 * time.Second):
			startErr = context.DeadlineExceeded
		}
	}
	if code := cli(ctx, nil, func(k string) string { return env[k] }, io.Discard, &logs, started); code != 0 || startErr != nil {
		t.Fatalf("exit %d, start %v: want the activity running and a clean stop\n%s", code, startErr, logs.String())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.done {
		t.Fatal("activity: got not finished when the server returned, want finished before the worker returned")
	}
	if h.err != nil {
		t.Errorf("activity's count: got %v, want its context live while the worker stops", h.err)
	}
	if h.rows != before+1 {
		t.Errorf("replicas during the worker's stop: got %d, want %d (this replica's record still there)", h.rows, before+1)
	}
	if got := replicaRows(t); got != before {
		t.Errorf("replicas once stopped: got %d, want %d", got, before)
	}
}
