package ledger_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// TestCloseChecksOutsideRequest: a close's checks read the ledger only in
// a request's unit of work; their rows and database failures are tested
// through POST /v1/period-closes/{id}/tasks/{code}:check and
// /v1/year-closes/{id}/tasks/{code}:check (internal/closing).
func TestCloseChecksOutsideRequest(t *testing.T) {
	day := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	if _, _, err := (ledger.CloseChecks{}).Parked(context.Background(), uuid.New(), day, day, []string{"parked"}, false, 20); err == nil {
		t.Error("parked entries outside a unit of work: got no error, want one")
	}
	if _, _, err := (ledger.CloseChecks{}).PostedRevaluation(context.Background(), uuid.New(), uuid.New(), day); err == nil {
		t.Error("a revaluation run outside a unit of work: got no error, want one")
	}
	if _, err := (ledger.CloseChecks{}).CarryForward(context.Background(), uuid.New(), uuid.New(), 2026); err == nil {
		t.Error("a carry-forward outside a unit of work: got no error, want one")
	}
}
