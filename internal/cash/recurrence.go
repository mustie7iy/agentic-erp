package cash

import (
	"slices"
	"time"
)

// Frequencies of a series of memo records (ADR-0051 §7).
const (
	Weekly    = "weekly"
	Monthly   = "monthly"
	Quarterly = "quarterly"
)

// Recurrence is how a series repeats from its value date: weekly, monthly
// on the value date's day (the month's last day when it is shorter) or
// quarterly, until a day or for a count of occurrences, at most one of
// them; neither: until it is cancelled.
type Recurrence struct {
	Frequency string
	Until     *time.Time
	Count     *int32
}

// maxOccurrences bounds the expansion of a series without end, so a read
// over a long horizon of a weekly series started long ago still ends:
// twenty years of weeks.
const maxOccurrences = 1040

// nth is the series' occurrence k (from 0) from start, and whether the
// series has one.
func (r Recurrence) nth(start time.Time, k int) (time.Time, bool) {
	if k >= maxOccurrences || (r.Count != nil && k >= int(*r.Count)) {
		return time.Time{}, false
	}
	var d time.Time
	switch r.Frequency {
	case Weekly:
		d = start.AddDate(0, 0, 7*k)
	case Quarterly:
		d = addMonths(start, 3*k)
	default:
		d = addMonths(start, k)
	}
	if r.Until != nil && d.After(*r.Until) {
		return time.Time{}, false
	}
	return d, true
}

// addMonths is the day n months after d on d's day of the month, or the
// month's last day when it has fewer: 31 January, then 28 February, then
// 31 March.
func addMonths(d time.Time, n int) time.Time {
	first := time.Date(d.Year(), d.Month()+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	return first.AddDate(0, 0, min(d.Day(), last)-1)
}

// occurrences are the series' days from start up to to, in order.
func (r Recurrence) occurrences(start, to time.Time) []time.Time {
	var out []time.Time
	for k := 0; ; k++ {
		d, ok := r.nth(start, k)
		if !ok || d.After(to) {
			return out
		}
		out = append(out, d)
	}
}

// last is the series' last occurrence, false for a series without end. A
// bounded series has its value date as its first occurrence: its count is
// at least 2 and its end on or after the value date (memoValues.dates).
func (r Recurrence) last(start time.Time) (time.Time, bool) {
	if r.Until == nil && r.Count == nil {
		return time.Time{}, false
	}
	days := r.occurrences(start, farFuture)
	return days[len(days)-1], true
}

// farFuture is past every series' end.
var farFuture = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// has reports whether day is one of the series' occurrences.
func (r Recurrence) has(start, day time.Time) bool {
	return slices.ContainsFunc(r.occurrences(start, day), func(d time.Time) bool { return d.Equal(day) })
}
