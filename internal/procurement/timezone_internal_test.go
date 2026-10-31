package procurement

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// document is a configuration source holding one document.
type document struct{ doc *config.Config }

func (d document) Document(context.Context) (*config.Config, int32, error) { return d.doc, 1, nil }

// TestTodayUnknownZone: a stored document whose company names a zone this
// build cannot load (validation refuses one, so only a zone database that
// lost it) fails the requisition rather than dating it in UTC (T72n5).
func TestTodayUnknownZone(t *testing.T) {
	e := &Endpoints{Settings: document{&config.Config{Companies: []config.Company{{Code: "2000", TimeZone: "America/Nowhere"}}}},
		Now: func() time.Time { return time.Date(2026, 4, 16, 21, 0, 0, 0, time.UTC) }}
	if _, err := e.today(context.Background(), "2000"); err == nil || !strings.Contains(err.Error(), "procurement: company 2000") {
		t.Errorf("got %v, want the company's zone refused", err)
	}
}
