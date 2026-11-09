package app

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// TestEInvoiceFetchKind: the workflows dispatcher, which may not import the
// einvoicing context (a leaf, ADR-0061 §2), dispatches its fetches by the
// kind of their Operation, and the in-process fetcher finds them by it.
func TestEInvoiceFetchKind(t *testing.T) {
	if workflows.EInvoiceFetchKind != einvoicing.KindFetch {
		t.Errorf("got %s, want %s", workflows.EInvoiceFetchKind, einvoicing.KindFetch)
	}
}
