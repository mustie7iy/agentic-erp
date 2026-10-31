package procurement

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestTodayUnknownZone: a stored document whose company names a zone this
// build cannot load (validation refuses one, so only a zone database that
// lost it) fails the requisition rather than dating it in UTC (T72n5).
func TestTodayUnknownZone(t *testing.T) {
	e := &Endpoints{Settings: zoneSettings{zone: "America/Nowhere"},
		Now: func() time.Time { return time.Date(2026, 4, 16, 21, 0, 0, 0, time.UTC) }}
	if _, _, err := e.today(context.Background(), "2000"); err == nil || !strings.Contains(err.Error(), "procurement: company 2000") {
		t.Errorf("got %v, want the company's zone refused", err)
	}
}

// TestReversalDateUnknownZone: a goods movement reversed without a posting
// date fails when its company's zone does not load, rather than dating the
// reversal in UTC; a given date needs no zone.
func TestReversalDateUnknownZone(t *testing.T) {
	e := &Endpoints{LegalEntities: zoneDirectory{}, Settings: zoneSettings{zone: "America/Nowhere"},
		Now: func() time.Time { return time.Date(2026, 4, 1, 2, 0, 0, 0, time.UTC) }}
	if _, err := e.reversalDate(context.Background(), GoodsReceipt{}, nil); err == nil || !strings.Contains(err.Error(), "procurement: company 2000") {
		t.Errorf("got %v, want the company's zone refused", err)
	}
	given := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	if got, err := e.reversalDate(context.Background(), GoodsReceipt{}, &given); err != nil || !got.Equal(given) {
		t.Errorf("a given date: got %v, %v, want %v", got, err, given)
	}
}
