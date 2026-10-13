package clock

import (
	"testing"
	"time"
)

// TestNow: an injected clock is read when set, the wall clock otherwise.
func TestNow(t *testing.T) {
	t.Run("injected", func(t *testing.T) {
		fixed := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		if got := Now(func() time.Time { return fixed }); !got.Equal(fixed) {
			t.Errorf("got %v, want %v", got, fixed)
		}
	})
	t.Run("wall clock", func(t *testing.T) {
		before := time.Now()
		got := Now(nil)
		if got.Before(before) || got.After(time.Now()) {
			t.Errorf("got %v, want a time between %v and now", got, before)
		}
	})
}
