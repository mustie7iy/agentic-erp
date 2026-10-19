package payables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// DownPaymentRequests is what the payments context reads and records about
// down payment requests (T46c), through the adapter in internal/app: the
// open requests a run pays, and that a released run paid one. It reads
// without the caller's company access, as the run's worker does.
type DownPaymentRequests struct{}

// Due returns the legal entity's open requests in the currency due by the
// date, oldest due first. The caller holds the legal entity's proposals
// lock, which a cancellation takes too (Payments.RequestRunOf), so a
// request read here is not cancelled under the run.
func (DownPaymentRequests) Due(ctx context.Context, legalEntity uuid.UUID, currency string, by time.Time) ([]DownPaymentRequest, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, requestSelect+` WHERE r.legal_entity_id = $1 AND r.currency = $2 AND r.due_date <= $3 AND r.status = 'open'
		ORDER BY r.due_date, r.id`, legalEntity, currency, by)
	requests, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DownPaymentRequest, error) { return scanRequest(r) })
	if err != nil {
		return nil, fmt.Errorf("payables: the down payment requests of %s due by %s: %w", legalEntity, by.Format(time.DateOnly), err)
	}
	return requests, nil
}

// Request returns a request.
func (DownPaymentRequests) Request(ctx context.Context, id uuid.UUID) (DownPaymentRequest, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return DownPaymentRequest{}, err
	}
	return loadRequest(ctx, tx, id, false)
}

// Paid records that the journal entry paid the request: it moves to paid,
// is audited and DownPaymentRequestPaid is published. Recording the same
// entry again changes nothing, so the release's retry is safe; a request
// paid by another entry, or cancelled, is refused.
func (DownPaymentRequests) Paid(ctx context.Context, id uuid.UUID, fiscalYear int16, entry uuid.UUID) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	r, err := loadRequest(ctx, tx, id, true)
	if err != nil {
		return err
	}
	switch {
	case r.Status == RequestPaid && r.JournalEntryID != nil && *r.JournalEntryID == entry:
		return nil
	case r.Status != RequestOpen:
		return problem.Conflict("payables.invalid_status", "Down payment request "+r.DocumentNumber+" is "+r.Status+"; only an open request is paid.")
	}
	if _, err := tx.Exec(ctx, `UPDATE payables.down_payment_request SET status = $2, journal_entry_year = $3, journal_entry_id = $4, version = version + 1,
		updated_by = $5 WHERE id = $1 AND status = 'open'`, id, RequestPaid, fiscalYear, entry, p.ID); err != nil {
		return fmt.Errorf("payables: mark down payment request %s paid: %w", id, err)
	}
	return recordRequest(ctx, "payables.down_payment_request.pay", r, map[string]any{"journal_entry_id": entry, "journal_entry_year": fiscalYear},
		requestEvent(r, "down_payment_request_paid", map[string]any{"document_number": r.DocumentNumber, "journal_entry_id": entry,
			"journal_entry_year": fiscalYear, "amount": money.EventAmount(r.Amount, r.Currency, r.Exponent), "status": RequestPaid}))
}
