package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/einvoicing"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
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

// TestEnteredAs (T65f): a tax invoice is entered as an invoice and a tax
// credit note as a credit memo, the one mapping entering writes and a
// link compares (enteredAs).
func TestEnteredAs(t *testing.T) {
	for kind, want := range map[string]string{einvoicing.KindTaxInvoice: payables.KindInvoice, einvoicing.KindTaxCreditNote: payables.KindCreditMemo} {
		t.Run(kind, func(t *testing.T) {
			if got := (einvoicePayables{}).EnteredAs(kind); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// TestEInvoiceSendKind: the dispatcher starts the sends by the kind of
// their Operation (einvoicing.KindSend), and the in-process sender finds
// them by it; the send workflow's port acts as the einvoicing context's
// sender and hands its step's errors on.
func TestEInvoiceSendKind(t *testing.T) {
	if workflows.EInvoiceSendKind != einvoicing.KindSend {
		t.Errorf("got %s, want %s", workflows.EInvoiceSendKind, einvoicing.KindSend)
	}
	if workflows.EInvoiceTaxDataKind != einvoicing.KindTaxData {
		t.Errorf("got %s, want %s", workflows.EInvoiceTaxDataKind, einvoicing.KindTaxData)
	}
	port := EInvoiceSends(Config{})
	tenant := uuid.New()
	if p := port.Sender(tenant); p.ID != einvoicing.SenderActor || p.TenantID != tenant {
		t.Errorf("the sender: got %+v, want einvoicing's sender in the tenant", p)
	}
	down := errors.New("database down")
	if _, err := port.AdvanceEInvoiceSubmission(context.Background(), uuid.New(), uuid.New(),
		func(context.Context, func(context.Context) error) error { return down }); !errors.Is(err, down) {
		t.Errorf("a step failing: got %v, want its error", err)
	}
	if _, err := port.UnfinishedEInvoiceSends(context.Background(), &pgtest.FailingTx{FailAt: 1}, time.Now()); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("the sends to advance unreadable: got %v, want the injected failure", err)
	}
}
