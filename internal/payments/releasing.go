package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// PeriodReleases is the payments context's port for the ledger's period
// close and lock (ledger.PaymentReleases): the runs being released, or
// stopped and waiting to be resumed, with payments not posted yet. A run
// stopped with some payments posted is never rejected or proposed again;
// its release resumes, posting the rest on the run date, so that date's
// period must stay open until it has (ADR-0038 As built, review batch 77).
type PeriodReleases struct{}

// Releasing returns the legal entity's releasing runs dated from start to
// end with a payment neither posted nor dropped, by document number.
func (PeriodReleases) Releasing(ctx context.Context, legalEntity uuid.UUID, start, end time.Time) ([]ledger.ReleasingRun, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT r.id, r.document_number FROM payments.payment_run r
		WHERE r.legal_entity_id = $1 AND r.status = 'releasing' AND r.run_date BETWEEN $2 AND $3
			AND EXISTS (SELECT FROM payments.payment p WHERE p.tenant_id = r.tenant_id AND p.payment_run_id = r.id AND p.journal_entry_id IS NULL
				AND NOT `+dropped+`)
		ORDER BY r.document_number`, legalEntity, start, end)
	runs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ledger.ReleasingRun])
	if err != nil {
		return nil, fmt.Errorf("payments: the runs of %s releasing from %s to %s: %w", legalEntity, start.Format(time.DateOnly), end.Format(time.DateOnly), err)
	}
	return runs, nil
}
