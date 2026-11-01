package taxfiling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Holds tells the ledger which return holds a tax date (ledger.TaxReturns,
// ADR-0049 P6): a chosen tax date lies in a period no return of the
// company's registrations reports yet. A return reports its period from
// the moment it is prepared, as it claims the period's items then, until
// it is withdrawn or rejected, which releases them.
type Holds struct{}

var _ ledger.TaxReturns = Holds{}

// Holding locks the registrations covering the legal entity, as registrant
// or tax group member, FOR SHARE and returns the latest prepared, approved
// or filed return of one of them whose period holds the date. Prepare
// locks its registration FOR UPDATE before claiming, so a posting either
// commits before the claim reads its items or waits for the return and
// sees it. It reads in the request's transaction whatever the caller may
// see: the rule holds for every caller.
func (Holds) Holding(ctx context.Context, legalEntity uuid.UUID, taxDate time.Time) (ledger.HeldReturn, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return ledger.HeldReturn{}, false, err
	}
	// Locked in a statement of its own: the read below then starts after
	// any prepare it waited for committed, and sees that return.
	if _, err := tx.Exec(ctx, `SELECT FROM tax.registration WHERE legal_entity_id = $1 OR $1 = ANY (group_member_ids) FOR SHARE`,
		legalEntity); err != nil {
		return ledger.HeldReturn{}, false, fmt.Errorf("taxfiling: lock the tax registrations of %s: %w", legalEntity, err)
	}
	var r ledger.HeldReturn
	err = tx.QueryRow(ctx, `SELECT r.id, g.code, r.period_start, r.period_end, r.kind, r.status
		FROM tax.tax_return r JOIN tax.registration g ON g.tenant_id = r.tenant_id AND g.id = r.registration_id
		WHERE (g.legal_entity_id = $1 OR $1 = ANY (g.group_member_ids)) AND r.status IN ('prepared', 'approved', 'filed')
			AND r.period_start <= $2 AND r.period_end >= $2
		ORDER BY r.created_at DESC, r.id DESC LIMIT 1`, legalEntity, taxDate).Scan(&r.ID, &r.Registration, &r.PeriodStart, &r.PeriodEnd, &r.Kind, &r.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.HeldReturn{}, false, nil
	}
	if err != nil {
		return ledger.HeldReturn{}, false, fmt.Errorf("taxfiling: the return holding %s of %s: %w", taxDate.Format(time.DateOnly), legalEntity, err)
	}
	return r, true, nil
}
