package nfr

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestSoak runs the mix for a long time in windows and checks what a soak
// is for (05-scalability §5.8): latency that stays where it started, no
// goroutine or heap growth, and workers that keep up.
//
// ERP_SOAK_DURATION (default 30s) and ERP_SOAK_WINDOW (default 5s) size
// the run; the weekly suite runs it for hours.
func TestSoak(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("soak suite: runs without -short and without the race detector (make soak, weekly)")
	}
	duration, window := Setting("ERP_SOAK_DURATION", 30*time.Second), Setting("ERP_SOAK_WINDOW", 5*time.Second)
	s := Start(t, Options{Users: int(Setting("ERP_LOAD_USERS", 16)), Events: true})
	ctx := context.Background()
	s.Run(ctx, window, 0, NewStats()) // warm-up
	goroutines, heap := runtime.NumGoroutine(), heapInUse()
	var first *Stats
	var last *Stats
	for elapsed := time.Duration(0); elapsed < duration; elapsed += window {
		st := NewStats()
		s.Run(ctx, window, int(Setting("ERP_LOAD_RPS", 0)), st)
		st.Report(t, "window "+(elapsed+window).String())
		if f := st.Failures(); len(f) > 0 {
			t.Errorf("window %v: failed requests %v\n%s", elapsed, f, st.Errors())
		}
		if first == nil {
			first = st
		}
		last = st
		p, o := s.Backlogs()
		if p > 2000 {
			t.Errorf("window %v: projector falls behind: got %d entries unprojected, want at most 2000", elapsed, p)
		}
		if o > 2000 {
			t.Errorf("window %v: relay falls behind: got %d events unpublished, want at most 2000", elapsed, o)
		}
	}
	// Latency at the end is where it was at the start (within noise), or within budget.
	for op, b := range Budgets {
		if last.Count(op) == 0 || first.Count(op) == 0 {
			continue
		}
		if start, end := first.Percentile(op, 95), last.Percentile(op, 95); end > Budget(b[0]) && end > start*3/2 {
			t.Errorf("%s: p95 drifted from %v to %v", op, start.Round(time.Millisecond), end.Round(time.Millisecond))
		}
	}
	if err := s.Drain(30 * time.Second); err != nil {
		t.Error(err)
	}
	if err := s.Consistent(ctx); err != nil {
		t.Error(err)
	}
	if g := runtime.NumGoroutine(); g > goroutines+len(s.Users)+10 {
		t.Errorf("goroutines grew from %d to %d", goroutines, g)
	}
	if h := heapInUse(); h > 2*heap+64<<20 {
		t.Errorf("heap grew from %d MB to %d MB", heap>>20, h>>20)
	}
	t.Logf("soak of %v: goroutines %d → %d, heap %d MB → %d MB, %d entries posted", duration, goroutines, runtime.NumGoroutine(), heap>>20, heapInUse()>>20, len(s.Posted()))
}

func heapInUse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}
