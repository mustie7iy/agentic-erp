package nfr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus/natstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// abortingTB is a test whose Fatal ends the goroutine, as testing's does,
// and whose cleanups are kept to be run by the caller.
type abortingTB struct {
	testing.TB
	fatal    string
	cleanups []func()
}

func (a *abortingTB) Helper()           {}
func (a *abortingTB) Cleanup(f func())  { a.cleanups = append(a.cleanups, f) }
func (a *abortingTB) Fatal(args ...any) { a.fatal = fmt.Sprint(args...); runtime.Goexit() }
func (a *abortingTB) FailNow()          { a.fatal = "FailNow"; runtime.Goexit() }
func (a *abortingTB) Fatalf(f string, args ...any) {
	a.fatal = fmt.Sprintf(f, args...)
	runtime.Goexit()
}

// TestAFailedStartIsCleanedUp: when the bus's streams cannot be made, Start
// fails the test with its workers already running; the cleanups it
// registered stop them (the workers' context ends) and the server.
func TestAFailedStartIsCleanedUp(t *testing.T) {
	var workers context.Context
	ensureStreams = func(_ *natsbus.Bus, ctx context.Context, _ string) error {
		workers = ctx
		return errors.New("no streams")
	}
	t.Cleanup(func() { ensureStreams = (*natsbus.Bus).EnsureStreams })
	tb := &abortingTB{TB: t}
	done := make(chan struct{})
	go func() { defer close(done); Start(tb, Options{Users: 1, Events: true}) }()
	<-done
	for i := len(tb.cleanups) - 1; i >= 0; i-- {
		tb.cleanups[i]()
	}
	if tb.fatal != "no streams" {
		t.Errorf("got fatal %q, want %q", tb.fatal, "no streams")
	}
	if workers == nil {
		t.Fatal("got no stream setup, want Start to reach it")
	}
	if workers.Err() == nil {
		t.Error("got the workers' context still live after the cleanups, want it cancelled")
	}
}

// servingTestServers counts the goroutines serving an httptest server: one
// per server started and not yet closed. It counts across the process, so
// it relies on this package's tests running one at a time (none uses
// t.Parallel).
func servingTestServers() int {
	buf := make([]byte, 1<<22)
	return strings.Count(string(buf[:runtime.Stack(buf, true)]), "net/http/httptest.(*Server).goServe")
}

// TestAFailedNATSStartLeavesNoServer: when the stack's NATS server cannot
// start, Start fails the test without leaving its API server serving.
func TestAFailedNATSStartLeavesNoServer(t *testing.T) {
	newRestartable = func(tb testing.TB) *natstest.Restartable {
		tb.Helper()
		tb.Fatal("no nats")
		return nil
	}
	t.Cleanup(func() { newRestartable = natstest.NewRestartable })
	before := servingTestServers()
	tb := &abortingTB{TB: t}
	done := make(chan struct{})
	go func() { defer close(done); Start(tb, Options{Users: 1, Events: true}) }()
	<-done
	for i := len(tb.cleanups) - 1; i >= 0; i-- {
		tb.cleanups[i]()
	}
	if tb.fatal != "no nats" {
		t.Errorf("got fatal %q, want %q", tb.fatal, "no nats")
	}
	if after := servingTestServers(); after > before {
		t.Errorf("got %d test servers serving after the cleanups, want at most %d", after, before)
	}
}

// TestConsistentChecksALostAnswerByItsKey: a post answered 201 whose body
// was cut off (the run ended while it was read) has no id, but its key is
// its reference: Consistent finds the entry by it rather than reporting the
// lost body, and still reports such an entry that is not in the journal and
// a complete 201 that has no id.
func TestConsistentChecksALostAnswerByItsKey(t *testing.T) {
	s := Start(t, Options{Users: 1})
	ctx := context.Background()
	key := uuid.NewString()
	s.Must(ctx, Req{Method: "POST", Path: "/v1/journal-entries", Body: s.Entry(2, key), As: s.Users[0].Actor, Key: key})
	lost := errors.New("unexpected EOF")
	s.record(Posted{Key: key, Status: http.StatusCreated, Err: lost})
	if err := s.Drain(30 * time.Second); err != nil { // Consistent compares the projected balances too
		t.Fatal(err)
	}
	if err := s.Consistent(ctx); err != nil {
		t.Fatalf("got %v, want the entry found by its key", err)
	}
	s.record(Posted{Key: uuid.NewString(), Status: http.StatusCreated, Err: lost})
	pgtest.CheckErr(t, s.Consistent(ctx), "1 entries answered 201 are not in the journal")
	s.posted = s.posted[:1]
	s.record(Posted{Key: uuid.NewString(), Status: http.StatusCreated})
	pgtest.CheckErr(t, s.Consistent(ctx), "an entry answered 201 without a readable id")
	s.posted = s.posted[:1]
	// A complete answer that is not JSON is reported, not looked up by its key.
	s.record(Posted{Key: key, Status: http.StatusCreated, Err: fmt.Errorf("POST: 201 with a %w", errNotJSON)})
	pgtest.CheckErr(t, s.Consistent(ctx), "an entry answered 201 without a readable id")
}
