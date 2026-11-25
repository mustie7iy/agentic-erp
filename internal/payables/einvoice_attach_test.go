package payables_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/payables"
)

// TestAttachOutsideRequest: the reads and writes a link of a held
// e-invoice makes (ADR-0061 §7, T65f) need the request's transaction;
// TestLinkEInvoice (internal/einvoicing) covers them within one.
func TestAttachOutsideRequest(t *testing.T) {
	ctx := context.Background()
	if _, _, err := (payables.Invoices{}).Attachable(ctx, uuid.New()); err == nil {
		t.Error("attachable: got no error, want one")
	}
	if err := (payables.Invoices{}).Attach(ctx, uuid.New(), nil, ""); err == nil {
		t.Error("attach: got no error, want one")
	}
}
