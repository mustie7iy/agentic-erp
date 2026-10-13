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
