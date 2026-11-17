package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestLockEntriesOutsideRequest: the entries' locks are taken only in a
// request's unit of work; the locks themselves and their order are tested
// through a settlement's reversal (internal/controlling, T60i).
func TestLockEntriesOutsideRequest(t *testing.T) {
	if err := (ledger.Postings{}).LockEntries(context.Background(), []uuid.UUID{uuid.New()}); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("locking entries outside a unit of work: got %v, want %v", err, reqctx.ErrOutsideRequest)
	}
}
