package closing

import (
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// TestDueOn: day 0 is the period's last day even when it is no business
// day; the search for business days stops after maxDays calendar days, so
// a calendar without one still answers.
func TestDueOn(t *testing.T) {
	saturday := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	doc := &config.Config{}
	if got := dueOn(doc, "1000", saturday, 0); !got.Equal(saturday) {
		t.Errorf("day 0: got %s, want %s", got.Format(time.DateOnly), saturday.Format(time.DateOnly))
	}
	never := &config.Config{Calendars: []config.Calendar{{Company: "1000",
		Weekend: []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}}}}
	if got, want := dueOn(never, "1000", saturday, 1), saturday.AddDate(0, 0, maxDays); !got.Equal(want) {
		t.Errorf("no business day after: got %s, want %s", got.Format(time.DateOnly), want.Format(time.DateOnly))
	}
	if got, want := dueOn(never, "1000", saturday, -1), saturday.AddDate(0, 0, -maxDays); !got.Equal(want) {
		t.Errorf("no business day before: got %s, want %s", got.Format(time.DateOnly), want.Format(time.DateOnly))
	}
}
