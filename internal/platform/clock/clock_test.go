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

// TestDay: an instant's UTC calendar date, at midnight UTC.
func TestDay(t *testing.T) {
	late := time.Date(2026, 4, 17, 23, 30, 0, 0, time.FixedZone("UTC-5", -5*3600))
	if got, want := Day(late), time.Date(2026, 4, 18, 0, 0, 0, 0, time.UTC); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("Day: got %v, want %v", got, want)
	}
}
