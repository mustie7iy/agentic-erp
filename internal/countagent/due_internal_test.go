package countagent

import (
	"context"
	"strings"
	"testing"
)

// TestSurveyDay: a day that is no date stops the survey before it reads;
// Plan takes the day from the API's time zone, so only a broken clock or
// zone could give one.
func TestSurveyDay(t *testing.T) {
	if _, err := (&Agent{}).survey(context.Background(), plant{}, "2026-13-01"); err == nil || !strings.Contains(err.Error(), `the day "2026-13-01"`) {
		t.Errorf("got %v, want the day refused", err)
	}
}
