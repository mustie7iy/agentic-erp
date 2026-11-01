package ledger

import (
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// ymd is the date s names; the cases name valid dates, and an invalid
// one would read as the zero time and fail its case.
func ymd(s string) time.Time {
	t, _ := time.Parse(time.DateOnly, s) // see above
	return t
}

func dateList(ts []time.Time) string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Format(time.DateOnly)
	}
	return strings.Join(out, " ")
}

func ref[T any](v T) *T { return &v }

// TestScheduleDates: the day of the month clamps to the month's last day,
// the first date is in the start's month unless it falls before the
// start, and the schedule ends by date or count.
func TestScheduleDates(t *testing.T) {
	for _, c := range []struct {
		name    string
		s       Schedule
		after   *time.Time
		through string
		want    string
	}{
		{"monthly on the 31st clamps", Schedule{Frequency: Monthly, Day: 31, StartsOn: ymd("2026-01-01")}, nil, "2026-05-31",
			"2026-01-31 2026-02-28 2026-03-31 2026-04-30 2026-05-31"},
		{"a leap year's February", Schedule{Frequency: Monthly, Day: 30, StartsOn: ymd("2028-02-01")}, nil, "2028-03-31", "2028-02-29 2028-03-30"},
		{"the start's month passed", Schedule{Frequency: Monthly, Day: 1, StartsOn: ymd("2026-11-02")}, nil, "2027-01-31", "2026-12-01 2027-01-01"},
		{"on the start", Schedule{Frequency: Monthly, Day: 1, StartsOn: ymd("2026-11-01")}, nil, "2026-11-01", "2026-11-01"},
		{"quarterly", Schedule{Frequency: Quarterly, Day: 15, StartsOn: ymd("2026-01-20")}, nil, "2026-12-31", "2026-04-15 2026-07-15 2026-10-15"},
		{"yearly", Schedule{Frequency: Yearly, Day: 31, StartsOn: ymd("2026-12-01")}, nil, "2029-01-01", "2026-12-31 2027-12-31 2028-12-31"},
		{"after a date", Schedule{Frequency: Monthly, Day: 31, StartsOn: ymd("2026-01-01")}, ref(ymd("2026-03-30")), "2026-05-31", "2026-03-31 2026-04-30 2026-05-31"},
		{"after a date past the start", Schedule{Frequency: Quarterly, Day: 1, StartsOn: ymd("2026-01-01")}, ref(ymd("2026-10-01")), "2027-12-31",
			"2027-01-01 2027-04-01 2027-07-01 2027-10-01"},
		{"after a date before the start", Schedule{Frequency: Monthly, Day: 5, StartsOn: ymd("2026-06-01")}, ref(ymd("2026-01-01")), "2026-07-31",
			"2026-06-05 2026-07-05"},
		{"ending on a date", Schedule{Frequency: Monthly, Day: 1, StartsOn: ymd("2026-01-01"), EndsOn: ref(ymd("2026-03-01"))}, nil, "2026-12-31",
			"2026-01-01 2026-02-01 2026-03-01"},
		{"for a count", Schedule{Frequency: Monthly, Day: 1, StartsOn: ymd("2026-01-01"), Count: ref(int32(2))}, nil, "2026-12-31", "2026-01-01 2026-02-01"},
		{"nothing due yet", Schedule{Frequency: Monthly, Day: 1, StartsOn: ymd("2026-12-01")}, nil, "2026-11-30", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := dateList(c.s.Due(c.after, ymd(c.through))); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestScheduleCatchUp: one round lists at most maxCatchUp dates.
func TestScheduleCatchUp(t *testing.T) {
	s := Schedule{Frequency: Monthly, Day: 1, StartsOn: ymd("2020-01-01")}
	due := s.Due(nil, ymd("2026-11-01"))
	if len(due) != maxCatchUp {
		t.Fatalf("got %d dates, want %d", len(due), maxCatchUp)
	}
	if got := due[len(due)-1].Format(time.DateOnly); got != "2021-12-01" {
		t.Errorf("last date: got %s, want 2021-12-01", got)
	}
	if got := dateList(s.Due(&due[len(due)-1], ymd("2022-02-01"))); got != "2022-01-01 2022-02-01" {
		t.Errorf("next round: got %s, want 2022-01-01 2022-02-01", got)
	}
}

func TestScheduleNextAndKeeps(t *testing.T) {
	s := Schedule{Frequency: Monthly, Day: 31, StartsOn: ymd("2026-01-15"), Count: ref(int32(3))}
	if got := s.Next(nil); got == nil || !got.Equal(ymd("2026-01-31")) {
		t.Errorf("first date: got %v, want 2026-01-31", got)
	}
	if got := s.Next(ref(ymd("2026-02-28"))); got == nil || !got.Equal(ymd("2026-03-31")) {
		t.Errorf("after February: got %v, want 2026-03-31", got)
	}
	if got := s.Next(ref(ymd("2026-03-31"))); got != nil {
		t.Errorf("after the last: got %v, want none", got)
	}
	for d, want := range map[string]bool{"2026-02-28": true, "2026-02-27": false, "2026-03-31": true, "2026-04-30": false, "2025-12-31": false} {
		if got := s.Keeps(ymd(d)); got != want {
			t.Errorf("keeps %s: got %v, want %v", d, got, want)
		}
	}
}

func TestScheduleCheck(t *testing.T) {
	for _, c := range []struct {
		name  string
		s     Schedule
		field string // empty: valid
	}{
		{"valid", Schedule{Frequency: Yearly, Day: 31, StartsOn: ymd("2026-01-01"), EndsOn: ref(ymd("2026-01-01"))}, ""},
		{"weekly", Schedule{Frequency: "weekly", Day: 1, StartsOn: ymd("2026-01-01")}, "schedule.frequency"},
		{"day 0", Schedule{Frequency: Monthly, Day: 0, StartsOn: ymd("2026-01-01")}, "schedule.day_of_month"},
		{"day 32", Schedule{Frequency: Monthly, Day: 32, StartsOn: ymd("2026-01-01")}, "schedule.day_of_month"},
		{"end and count", Schedule{Frequency: Monthly, Day: 1, StartsOn: ymd("2026-01-01"), EndsOn: ref(ymd("2026-12-01")), Count: ref(int32(2))},
			"schedule.occurrences"},
		{"end before start", Schedule{Frequency: Monthly, Day: 1, StartsOn: ymd("2026-01-01"), EndsOn: ref(ymd("2025-12-01"))}, "schedule.ends_on"},
		{"no count", Schedule{Frequency: Monthly, Day: 1, StartsOn: ymd("2026-01-01"), Count: ref(int32(0))}, "schedule.occurrences"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.s.check()
			if c.field == "" {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			p := problem.From(err)
			if p.Code != "ledger.invalid_schedule" {
				t.Fatalf("code: got %s, want ledger.invalid_schedule", p.Code)
			}
			if len(p.Errors) != 1 || p.Errors[0].Field != c.field {
				t.Errorf("fields: got %v, want %s", p.Errors, c.field)
			}
		})
	}
}
