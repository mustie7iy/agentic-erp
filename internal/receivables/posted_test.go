package receivables_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/receivables"
)

// TestIssuedInvoicesOutsideARequest: the listing reads in the request's
// transaction alone (its rows are read through the einvoicing worklist's
// tests, ADR-0061 As built E3).
func TestIssuedInvoicesOutsideARequest(t *testing.T) {
	if _, err := receivables.IssuedInvoices(context.Background(), uuid.New(), time.Now(), time.Now()); err == nil {
		t.Error("got nil error, want an error")
	}
}
