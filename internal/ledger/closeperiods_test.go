package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// TestClosePeriodsOutsideRequest: the close calendar's period read runs
// only in a request's unit of work; its rows and database failures are
// tested through GET /v1/close-calendar (internal/closing).
func TestClosePeriodsOutsideRequest(t *testing.T) {
	if _, err := (ledger.ClosePeriods{}).Leading(context.Background(), uuid.New(), 2026); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
}

// TestKeptPeriodsOutsideRequest: a period close's read of the periods by
// ledger runs only in a request's unit of work; its rows and database
// failures are tested through POST /v1/period-closes (internal/closing).
func TestKeptPeriodsOutsideRequest(t *testing.T) {
	if _, err := (ledger.ClosePeriods{}).Of(context.Background(), uuid.New(), 2026, 1); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
}
