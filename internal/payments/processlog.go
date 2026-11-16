package payments

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ProcessLog reads which released payment runs paid posted documents for
// the processmining context's event logs (T103a, ADR-0080). It reads in
// the caller's transaction and changes nothing.
type ProcessLog struct{}

// LoggedPayment is a released run that paid an open item of a journal
// entry.
type LoggedPayment struct {
	EntryYear      int16
	EntryID        uuid.UUID
	RunID          uuid.UUID
	DocumentNumber string // the run's
}

// Paying returns the released runs that paid items of the journal
// entries, given by fiscal year and id in two lists of the same length,
// by entry and run.
func (ProcessLog) Paying(ctx context.Context, years []int16, entries []uuid.UUID) ([]LoggedPayment, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT DISTINCT i.item_fiscal_year, i.item_entry_id, r.id, r.document_number
		FROM unnest($1::smallint[], $2::uuid[]) k(year, entry)
		JOIN payments.payment_item i ON i.item_fiscal_year = k.year AND i.item_entry_id = k.entry AND i.is_active
		JOIN payments.payment p ON p.tenant_id = i.tenant_id AND p.id = i.payment_id
		JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id AND r.status = 'released'
		ORDER BY i.item_entry_id, r.id`, years, entries)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LoggedPayment, error) {
		var p LoggedPayment
		err := r.Scan(&p.EntryYear, &p.EntryID, &p.RunID, &p.DocumentNumber)
		return p, errs.Wrapf(err, "scan a payment")
	})
	if err != nil {
		return nil, fmt.Errorf("payments: the runs paying %d entries for an event log: %w", len(entries), err)
	}
	return out, nil
}
