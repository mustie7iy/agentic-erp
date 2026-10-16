package pgtest

import (
	"strings"
	"sync"
	"testing"
)

// TestSyncBuffer: writers on several goroutines and a reader do not race
// (the -race run is the check), and every write is kept.
func TestSyncBuffer(t *testing.T) {
	var b SyncBuffer
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Write([]byte("x")); err != nil {
				t.Errorf("write: %v", err)
			}
		}()
	}
	_ = b.String() // read while the writers run; the race detector judges it
	wg.Wait()
	if got := b.String(); got != strings.Repeat("x", 4) {
		t.Errorf("got %q, want %q", got, "xxxx")
	}
}

// TestSyncBufferLimit: past twice its limit, the buffer drops to the last
// Limit bytes from the first line that starts in them, and always keeps the
// latest write.
func TestSyncBufferLimit(t *testing.T) {
	for name, c := range map[string]struct {
		limit int
		lines []string
		want  string
	}{
		"cut inside a line":      {10, []string{"aaa", "bbb", "ccc", "ddd", "eee", "fff", "ggg", "hhh", "iii", "jjj"}, "iii\njjj\n"},
		"cut on a line start":    {8, []string{"aaa", "bbb", "ccc", "ddd", "eee"}, "ddd\neee\n"},
		"line longer than limit": {10, []string{strings.Repeat("x", 25)}, strings.Repeat("x", 9) + "\n"},
		"no limit":               {0, []string{"aaa", "bbb", "ccc", "ddd", "eee"}, "aaa\nbbb\nccc\nddd\neee\n"},
	} {
		t.Run(name, func(t *testing.T) {
			b := SyncBuffer{Limit: c.limit}
			for _, line := range c.lines {
				if _, err := b.Write([]byte(line + "\n")); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if got := b.String(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestSyncBufferLimitBoundsMemory: the backing array, which is what a heap
// check measures, stays near four times the limit however much is written.
func TestSyncBufferLimitBoundsMemory(t *testing.T) {
	const limit = 1 << 10
	b := SyncBuffer{Limit: limit}
	line := []byte("123456789\n")
	for range 100_000 {
		if _, err := b.Write(line); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got, want := b.buf.Cap(), 4*limit+2*len(line); got > want {
		t.Errorf("capacity: got %d, want at most %d", got, want)
	}
}
