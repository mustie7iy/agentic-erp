package ledger

import (
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Schedule is when a template's entry recurs (ADR-0056, product owner
// 2026-11-01): every month, quarter or year on day Day of the month, the
// month's last day when it is shorter, from StartsOn on; until EndsOn, for
// Count dates, or without end. Its dates are company-local days: a date is
// due once it is today or past in the company's time zone.
type Schedule struct {
	Frequency string
	Day       int
	StartsOn  time.Time
	EndsOn    *time.Time
	Count     *int32
}

// Schedule frequencies.
const (
	Monthly   = "monthly"
	Quarterly = "quarterly"
	Yearly    = "yearly"
)

// frequencyMonths is the months between two dates of each frequency.
var frequencyMonths = map[string]int{Monthly: 1, Quarterly: 3, Yearly: 12}

// check refuses a schedule its fields cannot describe.
func (s Schedule) check() error {
	var bad []problem.FieldError
	if _, ok := frequencyMonths[s.Frequency]; !ok {
		bad = append(bad, problem.FieldError{Field: "schedule.frequency", Code: "invalid", Message: "monthly, quarterly or yearly"})
	}
	if s.Day < 1 || s.Day > 31 {
		bad = append(bad, problem.FieldError{Field: "schedule.day_of_month", Code: "invalid", Message: "a day of the month from 1 to 31"})
	}
	if s.EndsOn != nil && s.Count != nil {
		bad = append(bad, problem.FieldError{Field: "schedule.occurrences", Code: "conflict", Message: "ends_on or occurrences, not both"})
	}
	if s.EndsOn != nil && s.EndsOn.Before(s.StartsOn) {
		bad = append(bad, problem.FieldError{Field: "schedule.ends_on", Code: "invalid", Message: "on or after starts_on"})
	}
	if s.Count != nil && *s.Count < 1 {
		bad = append(bad, problem.FieldError{Field: "schedule.occurrences", Code: "invalid", Message: "at least 1"})
	}
	if len(bad) > 0 {
		return problem.Unprocessable("ledger.invalid_schedule", "The schedule cannot be kept.", bad...)
	}
	return nil
}

// date is the schedule's n-th date (from 0), its end aside: day Day of the
// n-th period's month, clamped to the month's last day. The first period
// is StartsOn's month, or the next when that month's date is before
// StartsOn.
func (s Schedule) date(n int) time.Time {
	k := frequencyMonths[s.Frequency]
	y, m, _ := s.StartsOn.Date()
	if onDay(y, m, s.Day).Before(s.StartsOn) {
		m += time.Month(k)
	}
	return onDay(y, m+time.Month(n*k), s.Day)
}

// onDay is day d of month m of year y (m may run past December), or the
// month's last day when it has fewer days.
func onDay(y int, m time.Month, d int) time.Time {
	first := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	return first.AddDate(0, 0, min(d, last)-1)
}

// within tells whether the schedule's n-th date, t, is one it keeps: before
// its count runs out and not after its end.
func (s Schedule) within(n int, t time.Time) bool {
	if s.Count != nil && n >= int(*s.Count) {
		return false
	}
	return s.EndsOn == nil || !t.After(*s.EndsOn)
}

// after is the index of the schedule's first date after day (its first
// date when day is nil), its end aside.
func (s Schedule) after(day *time.Time) int {
	if day == nil {
		return 0
	}
	k := frequencyMonths[s.Frequency]
	first := s.date(0)
	n := max(0, (monthsBetween(first, *day)/k)-1) // at or before the first date after day
	for !s.date(n).After(*day) {
		n++
	}
	return n
}

// monthsBetween is the calendar months from a to b.
func monthsBetween(a, b time.Time) int {
	return (b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month())
}

// Next is the schedule's first date after day (its first when day is
// nil), or nil once it has ended.
func (s Schedule) Next(day *time.Time) *time.Time {
	n := s.after(day)
	t := s.date(n)
	if !s.within(n, t) {
		return nil
	}
	return &t
}
