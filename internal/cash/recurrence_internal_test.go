package cash

import (
	"fmt"
	"testing"
	"time"
)

func day(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s) // the tests' own literals, which parse
	return d
}

func dates(ds []time.Time) string {
	out := ""
	for _, d := range ds {
		out += " " + d.Format(time.DateOnly)
	}
	return out
}

// TestRecurrence: a series' occurrences from its value date, monthly ones
// on the day or the month's last day, up to its end (ADR-0051 §7).
func TestRecurrence(t *testing.T) {
	count, until := int32(3), day("2026-05-31")
	for name, c := range map[string]struct {
		r          Recurrence
		start, to  string
		want, last string
	}{
		"weekly, open":          {Recurrence{Frequency: Weekly}, "2026-04-03", "2026-04-24", " 2026-04-03 2026-04-10 2026-04-17 2026-04-24", "none"},
		"monthly on the 31st":   {Recurrence{Frequency: Monthly, Count: &count}, "2026-01-31", "2026-12-31", " 2026-01-31 2026-02-28 2026-03-31", "2026-03-31"},
		"quarterly until a day": {Recurrence{Frequency: Quarterly, Until: &until}, "2025-11-30", "2026-12-31", " 2025-11-30 2026-02-28 2026-05-30", "2026-05-30"},
		"monthly, nothing yet":  {Recurrence{Frequency: Monthly}, "2026-05-01", "2026-04-30", "", "none"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := dates(c.r.occurrences(day(c.start), day(c.to))); got != c.want {
				t.Errorf("occurrences: got %q, want %q", got, c.want)
			}
			last, ok := c.r.last(day(c.start))
			got := "none"
			if ok {
				got = last.Format(time.DateOnly)
			}
			if got != c.last {
				t.Errorf("last: got %s, want %s", got, c.last)
			}
		})
	}
	t.Run("bounded without end", func(t *testing.T) {
		if n := len(Recurrence{Frequency: Weekly}.occurrences(day("2026-01-01"), farFuture)); n != maxOccurrences {
			t.Errorf("occurrences: got %d, want %d", n, maxOccurrences)
		}
	})
	t.Run("has", func(t *testing.T) {
		r := Recurrence{Frequency: Weekly}
		got := fmt.Sprint(r.has(day("2026-04-03"), day("2026-04-17")), r.has(day("2026-04-03"), day("2026-04-18")))
		if got != "true false" {
			t.Errorf("has: got %s, want true false", got)
		}
	})
}

// TestStatusOn: an active record past its last expiry is expired; a
// series without end, or another status, is as stored.
func TestStatusOn(t *testing.T) {
	until := day("2026-04-16")
	for name, c := range map[string]struct {
		m    MemoRecord
		want string
	}{
		"expired":     {MemoRecord{Status: MemoActive, CountsUntil: &until}, MemoExpired},
		"counting":    {MemoRecord{Status: MemoActive, CountsUntil: new(day("2026-04-17"))}, MemoActive},
		"without end": {MemoRecord{Status: MemoActive}, MemoActive},
		"realised":    {MemoRecord{Status: MemoRealised, CountsUntil: &until}, MemoRealised},
	} {
		t.Run(name, func(t *testing.T) {
			if got := c.m.StatusOn(day("2026-04-17")); got != c.want {
				t.Errorf("status: got %s, want %s", got, c.want)
			}
		})
	}
}
