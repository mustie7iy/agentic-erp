package cash

import (
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// TestReadDateUnknownZone: a stored document whose company names a zone
// this build cannot load (validation refuses one, so only a zone database
// that lost it) fails the read rather than reading the date in UTC.
func TestReadDateUnknownZone(t *testing.T) {
	e := &Endpoints{Now: func() time.Time { return time.Date(2026, 4, 18, 2, 0, 0, 0, time.UTC) }}
	doc := &config.Config{Companies: []config.Company{{Code: "3000", TimeZone: "America/Nowhere"}}}
	if _, _, err := e.readDate(doc, "3000"); err == nil || !strings.Contains(err.Error(), "cash: company 3000") {
		t.Errorf("got %v, want the company's zone refused", err)
	}
}
