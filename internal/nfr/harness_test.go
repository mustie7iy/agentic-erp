package nfr

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
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
