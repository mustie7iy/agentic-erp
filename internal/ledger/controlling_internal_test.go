package ledger

import (
	"testing"
	"time"
)

// A fiscal year is named after the calendar year it ends in.
func TestFiscalYearOf(t *testing.T) {
	for _, tc := range []struct {
		date        string
		start, want int
	}{{"2026-03-31", 4, 2026}, {"2026-04-01", 4, 2027}, {"2027-01-15", 4, 2027}, {"2026-12-31", 1, 2026}} {
		t.Run(tc.date, func(t *testing.T) {
			d, err := time.Parse(time.DateOnly, tc.date)
			if err != nil {
				t.Fatal(err)
			}
			if got := fiscalYearOf(d, tc.start); got != tc.want {
				t.Errorf("start month %d: got %d, want %d", tc.start, got, tc.want)
			}
		})
	}
}
