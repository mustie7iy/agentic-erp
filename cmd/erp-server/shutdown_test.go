package main

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/api"
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

// inFlight is a request still running when the server's shutdown begins.
// Once it is let go (its context ended, or its body stopped arriving) it
// counts the replicas recorded, on a connection of its own, as a commit
// that does not heed the request's context would see them (ADR-0038).
type inFlight struct {
	running chan struct{}
	count   func() (int, error)
	mu      sync.Mutex
	done    bool
	rows    int
	err     error // the count's
	ended   error // what let it go
}

func (f *inFlight) finish(ended error) {
	rows, err := f.count()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.err, f.ended, f.done = rows, err, ended, true
}

// newInFlight counts on a connection of its own, used by its handler alone.
func newInFlight(t *testing.T) *inFlight {
	t.Helper()
	conn := pgtest.Admin(t)
	return &inFlight{running: make(chan struct{}), count: func() (n int, err error) {
		err = conn.QueryRow(context.Background(), `SELECT count(*) FROM config.replica`).Scan(&n)
		return n, err
	}}
}

// send opens a connection to the API and writes raw, the start of a
// request, leaving it open.
func send(t *testing.T, ln net.Listener, raw string) (net.Conn, error) {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = conn.Close() }) // the test is over
	_, err = io.WriteString(conn, raw)
	return conn, err
}

// TestTheRecordOutlivesTheRequests: requests still running when the
// shutdown's drain (ERP_SHUTDOWN_TIMEOUT, 5s in environment) ends are
// ended, and the replica's record is deleted only once their handlers
// returned (review:T72m). One waits on its context, as a transaction does,
// its body unread, so closing its connection does not end its context
// (net/http watches a connection only once the body was read): only the
// requests' own cancellation does. The other waits on a body still
// arriving, its read deadline lifted to api.UploadTimeout as an upload's
// is: only closing its connection ends it. http.Server.Shutdown returns at
// its deadline with both still running and cancels neither, so without the
// requests' own ending the server deletes the record and returns while
// they run on.
func TestTheRecordOutlivesTheRequests(t *testing.T) {
	env := environment(t, authntest.New(t), "")
	before := replicaRows(t)
	held, upload := newInFlight(t), newInFlight(t)
	release := make(chan struct{})
	startWorkersHook = func(srv *server, runCtx context.Context) {
		srv.startWorkers(runCtx)
		mux := http.NewServeMux()
		mux.Handle("/", srv.apiHandler)
		mux.HandleFunc("POST /held", func(_ http.ResponseWriter, r *http.Request) {
			close(held.running)
			select {
			case <-r.Context().Done():
			case <-release: // the watchdog's: nothing else would end it
			}
			held.finish(r.Context().Err())
		})
		mux.HandleFunc("PUT /upload", func(w http.ResponseWriter, r *http.Request) {
			err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(api.UploadTimeout))
			close(upload.running)
			if err == nil {
				_, err = io.Copy(io.Discard, r.Body)
			}
			upload.finish(err)
		})
		srv.apiHandler = mux
	}
	defer func() { startWorkersHook = (*server).startWorkers }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs pgtest.SyncBuffer // the server logs on its goroutines while the test reads
	var startErr error
	var clientsMu sync.Mutex // started appends on the server's goroutine; the watchdog reads
	var clients []net.Conn
	started := func(apiLn, _ net.Listener) {
		defer cancel() // the shutdown begins with both requests running
		for _, raw := range []string{"POST /held HTTP/1.1\r\nHost: erp\r\nContent-Length: 1024\r\n\r\npart", "PUT /upload HTTP/1.1\r\nHost: erp\r\nContent-Length: 1024\r\n\r\npart"} {
			conn, err := send(t, apiLn, raw)
			if startErr = err; err != nil {
				return
			}
			clientsMu.Lock()
			clients = append(clients, conn)
			clientsMu.Unlock()
		}
		for _, f := range []*inFlight{held, upload} {
			select {
			case <-f.running:
			case <-time.After(30 * time.Second):
				startErr = context.DeadlineExceeded
				return
			}
		}
	}
	exit := make(chan int, 1)
	go func() { exit <- cli(ctx, nil, func(k string) string { return env[k] }, io.Discard, &logs, started) }()
	var code int
	select {
	case code = <-exit:
	case <-time.After(time.Minute):
		t.Error("server: got still stopping a minute on, want the requests it serves ended once its drain ended")
		close(release)
		clientsMu.Lock()
		for _, c := range clients {
			_ = c.Close() // lets the server's handlers go, so it returns
		}
		clientsMu.Unlock()
		code = <-exit
	}
	if code != 0 || startErr != nil {
		t.Fatalf("exit %d, start %v: want both requests running and a clean stop\n%s", code, startErr, logs.String())
	}
	for name, f := range map[string]*inFlight{"held": held, "upload": upload} {
		t.Run(name, func(t *testing.T) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.done {
				t.Fatal("request: got still running when the server returned, want its handler returned before the record went")
			}
			if f.ended == nil {
				t.Error("request: got let go without an error, want its context or its body ended by the shutdown")
			}
			if f.err != nil {
				t.Errorf("count: got %v, want nil", f.err)
			}
			if f.rows != before+1 {
				t.Errorf("replicas as the request ended: got %d, want %d (this replica's record still there)", f.rows, before+1)
			}
		})
	}
	if got := replicaRows(t); got != before {
		t.Errorf("replicas once stopped: got %d, want %d", got, before)
	}
}
