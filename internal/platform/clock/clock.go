// Package clock reads the time through an injectable clock: workers and
// services hold a Now field, nil in production, that tests set to a fixed or
// advancing time instead of sleeping (the fake clock of
// 06-engineering-process §6.6; CLAUDE.md, tests). It replaces the
// `if x.Now != nil` fallback each of them used to repeat.
package clock

import "time"

// Now returns now() when a clock was injected and the wall clock otherwise.
func Now(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}

// Day is t's calendar date in UTC, at midnight: the business date of an
// instant where no time zone of the company is known.
func Day(t time.Time) time.Time { return DayIn(t, time.UTC) }

// DayIn is t's calendar date in zone loc, at midnight UTC as every date is
// held: the business date of an instant for a company in that zone (its
// configured time_zone, T72n1), so a Toronto evening is still that day.
func DayIn(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// AddMonths is the date n months after d on d's day of the month, or that
// month's last day when it has fewer: 31 January, then 28 February, then
// 31 March. Each date is counted from d, never from the one before, so a
// series of them does not drift to the shorter months' days.
func AddMonths(d time.Time, n int) time.Time {
	first := time.Date(d.Year(), d.Month()+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	return first.AddDate(0, 0, min(d.Day(), last)-1)
}

// Later is the later of two times; Earlier the earlier. Where both are
// the same instant, either is returned.
func Later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Earlier is the earlier of two times (Later).
func Earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
