package pgtest

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// fatalTB records a Fatalf and ends the goroutine, as testing's does.
type fatalTB struct {
	testing.TB
	fatal string
}

func (f *fatalTB) Helper() {}
func (f *fatalTB) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func TestEventually(t *testing.T) {
	t.Run("holds after a few checks", func(t *testing.T) {
		checks := 0
		Eventually(t, time.Minute, func() bool { checks++; return checks == 3 }, "never held")
		if checks != 3 {
			t.Errorf("got %d checks, want 3", checks)
		}
	})
	t.Run("never holds", func(t *testing.T) {
		tb := &fatalTB{TB: t}
		done := make(chan struct{})
		go func() {
			defer close(done)
			Eventually(tb, 30*time.Millisecond, func() bool { return false }, "worker %s idle", "w1")
		}()
		<-done
		if want := "after 30ms: worker w1 idle"; tb.fatal != want {
			t.Errorf("got %q, want %q", tb.fatal, want)
		}
	})
	t.Run("holds at the deadline", func(t *testing.T) {
		start := time.Now()
		Eventually(t, 20*time.Millisecond, func() bool { return time.Since(start) >= 20*time.Millisecond }, "never held")
	})
}
