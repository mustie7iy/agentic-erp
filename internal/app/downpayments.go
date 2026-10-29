package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/payments"
)

// downPayments gives the payments context the payables context's down
// payment requests (T46c, ADR-0037). The adapter lives here, in the
// composition root, so neither context imports the other.
type downPayments struct{ requests payables.DownPaymentRequests }

func (d downPayments) Due(ctx context.Context, legalEntity uuid.UUID, currency string, by time.Time) ([]payments.DownPayment, error) {
	due, err := d.requests.Due(ctx, legalEntity, currency, by)
	out := make([]payments.DownPayment, len(due))
	for i, r := range due {
		out[i] = downPayment(r)
	}
	return out, err
}

func (d downPayments) Request(ctx context.Context, id uuid.UUID) (payments.DownPayment, error) {
	r, err := d.requests.Request(ctx, id)
	return downPayment(r), err
}

func (d downPayments) Numbers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return d.requests.Numbers(ctx, ids)
}

func (d downPayments) Paid(ctx context.Context, id uuid.UUID, fiscalYear int16, entryID uuid.UUID) error {
	return d.requests.Paid(ctx, id, fiscalYear, entryID)
}

func downPayment(r payables.DownPaymentRequest) payments.DownPayment {
	return payments.DownPayment{ID: r.ID, SupplierID: r.SupplierID, DocumentNumber: r.DocumentNumber, Reference: r.Reference, Amount: r.Amount,
		TaxCode: r.TaxCode, Tax: r.TaxAmount, DueDate: r.DueDate}
}
