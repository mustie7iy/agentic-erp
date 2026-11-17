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

// TestDayIn: an instant's calendar date in a zone, at midnight UTC; the
// same instant is another day in Toronto, Dubai and UTC.
func TestDayIn(t *testing.T) {
	toronto, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	dubai, err := time.LoadLocation("Asia/Dubai")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 4, 18, 2, 0, 0, 0, time.UTC) // 22:00 on the 17th in Toronto, 06:00 on the 18th in Dubai
	for name, c := range map[string]struct {
		loc  *time.Location
		want time.Time
	}{
		"Toronto": {toronto, time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)},
		"Dubai":   {dubai, time.Date(2026, 4, 18, 0, 0, 0, 0, time.UTC)},
		"UTC":     {time.UTC, time.Date(2026, 4, 18, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(name, func(t *testing.T) {
			if got := DayIn(at, c.loc); !got.Equal(c.want) || got.Location() != time.UTC {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestAddMonths: a date n months on, on its day of the month or the
// month's last day when it has fewer, and back.
func TestAddMonths(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		name string
		from time.Time
		n    int
		want time.Time
	}{
		{"same day", day(2026, 1, 15), 1, day(2026, 2, 15)},
		{"to a shorter month", day(2026, 1, 31), 1, day(2026, 2, 28)},
		{"to a leap February", day(2028, 1, 31), 1, day(2028, 2, 29)},
		{"from the start, not the month before", day(2026, 1, 31), 2, day(2026, 3, 31)},
		{"over the year", day(2026, 11, 30), 3, day(2027, 2, 28)},
		{"back", day(2026, 3, 31), -1, day(2026, 2, 28)},
		{"none", day(2026, 3, 31), 0, day(2026, 3, 31)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AddMonths(c.from, c.n); !got.Equal(c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestLaterEarlier: each picks its time whichever order they are given in.
func TestLaterEarlier(t *testing.T) {
	a, b := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for name, args := range map[string][2]time.Time{"in order": {a, b}, "reversed": {b, a}} {
		t.Run(name, func(t *testing.T) {
			if got := Later(args[0], args[1]); !got.Equal(b) {
				t.Errorf("Later: got %v, want %v", got, b)
			}
			if got := Earlier(args[0], args[1]); !got.Equal(a) {
				t.Errorf("Earlier: got %v, want %v", got, a)
			}
		})
	}
}
