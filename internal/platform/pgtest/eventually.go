package pgtest

import (
	"fmt"
	"testing"
	"time"
)

// pollEvery is how often Eventually checks its condition: often enough that
// a test waits little past the moment the condition holds, rarely enough
// that a condition reading the database does not load it.
const pollEvery = 10 * time.Millisecond

// Eventually waits until cond holds, checking it every pollEvery, and fails
// the test with the message when it still does not hold after timeout.
// Tests wait with it instead of sleeping (CLAUDE.md: no time.Sleep in
// tests), so they wait only as long as the work takes and a slow machine
// gets the whole timeout rather than a guess. It lives here because every
// test that waits for a worker or a server already imports pgtest.
func Eventually(tb testing.TB, timeout time.Duration, cond func() bool, format string, args ...any) {
	tb.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline.C:
			if cond() {
				return
			}
			tb.Fatalf("after %s: %s", timeout, fmt.Sprintf(format, args...))
		case <-tick.C:
		}
	}
}
