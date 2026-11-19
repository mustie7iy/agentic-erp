package taxfiling_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/taxfiling"
)

// TestInvoiceNumberOutsideARequest: outside a request nothing is read
// (the rest is the einvoicing context's TestRender*, which renders the
// seller's TRN through it).
func TestInvoiceNumberOutsideARequest(t *testing.T) {
	if _, _, _, err := (taxfiling.InvoiceNumbers{Keys: apitest.FieldKeys()}).Number(context.Background(), uuid.New(), "FTA", time.Now()); err == nil {
		t.Error("got nil error, want one")
	}
}
