package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Opener opens approval requests (workflow context).
type Opener interface {
	Open(ctx context.Context, in approvals.NewRequest) (uuid.UUID, bool, error)
}

// RequestUnlock opens the period_unlock approval request of a locked period
// and names it on the period (T32c). The period stays locked until the
// request is approved (PeriodUnlocks). The period is the only writer of
// unlock_request_id, and names only the request it has just opened: the
// database lets a locked period close when the request it names is
// cleared, and the approvals callback clears only the approved one.
func RequestUnlock(ctx context.Context, opener Opener, id uuid.UUID, ifMatch func(int32) error, reason string) (Period, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return Period{}, err
	}
	p, err := getPeriod(ctx, tx, id, true)
	if err != nil {
		return Period{}, err
	}
	if err := ifMatch(p.Version); err != nil {
		return Period{}, err
	}
	if p.Status != Locked {
		return Period{}, problem.Conflict("ledger.invalid_period_status", "The period is "+p.Status+"; this requires status "+Locked+".")
	}
	if p.UnlockRequestID != nil {
		return Period{}, problem.Conflict("ledger.unlock_pending", "The period already has a pending unlock request "+p.UnlockRequestID.String()+".")
	}
	request, needed, err := opener.Open(ctx, approvals.NewRequest{ObjectType: approvals.PeriodUnlock, ObjectID: id,
		Title: fmt.Sprintf("Unlock period %d/%d", p.FiscalYear, p.Period)})
	if err != nil {
		return Period{}, err
	}
	if !needed {
		return Period{}, fmt.Errorf("ledger: unlock of period %s: the approval policy requires no approval, which it always must", id)
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.period_status SET unlock_request_id = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		id, request, actor.ID); err != nil {
		return Period{}, fmt.Errorf("ledger: name unlock request %s on fiscal period %s: %w", request, id, err)
	}
	updated, err := getPeriod(ctx, tx, id, false)
	if err != nil {
		return Period{}, err
	}
	return updated, audit.Add(ctx, audit.Entry{Action: "ledger.fiscal_period.unlock_requested", ObjectType: "ledger.period_status", ObjectID: id,
		Outcome: "succeeded", Detail: map[string]any{"fiscal_year": p.FiscalYear, "period": p.Period, "unlock_request_id": request, "reason": reason}})
}

// PeriodUnlocks moves a locked period on when its unlock request completes
// (approvals.Handler), in the deciding transaction. Each acts only while
// the period still names that request, so a request that is not the
// period's pending one changes nothing and fails the decision.
type PeriodUnlocks struct{}

// Approved returns the period to closed and publishes PeriodUnlocked.
func (PeriodUnlocks) Approved(ctx context.Context, period, request uuid.UUID) error {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	p, err := namingRequest(ctx, tx, period, request)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.period_status SET status = 'closed', unlock_request_id = NULL, version = version + 1, updated_by = $3
		WHERE id = $1 AND unlock_request_id = $2`, period, request, actor.ID); err != nil {
		return fmt.Errorf("ledger: unlock fiscal period %s: %w", period, err)
	}
	return periodChanged(ctx, p, Closed, "unlock", "period_unlocked", map[string]any{"unlock_request_id": request})
}

// Rejected leaves the period locked and clears the request.
func (PeriodUnlocks) Rejected(ctx context.Context, period, request uuid.UUID, comment string) error {
	return endUnlock(ctx, period, request, "unlock_rejected", comment)
}

// Cancelled leaves the period locked and clears the request: its requester
// withdrew it.
func (PeriodUnlocks) Cancelled(ctx context.Context, period, request uuid.UUID, comment string) error {
	return endUnlock(ctx, period, request, "unlock_cancelled", comment)
}

func endUnlock(ctx context.Context, period, request uuid.UUID, action, comment string) error {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	p, err := namingRequest(ctx, tx, period, request)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.period_status SET unlock_request_id = NULL, version = version + 1, updated_by = $3
		WHERE id = $1 AND unlock_request_id = $2`, period, request, actor.ID); err != nil {
		return fmt.Errorf("ledger: clear unlock request %s of fiscal period %s: %w", request, period, err)
	}
	return audit.Add(ctx, audit.Entry{Action: "ledger.fiscal_period." + action, ObjectType: "ledger.period_status", ObjectID: period,
		Outcome: "succeeded", Detail: map[string]any{"fiscal_year": p.FiscalYear, "period": p.Period, "unlock_request_id": request, "comment": comment}})
}

// namingRequest locks the period and returns it if it is locked and names
// the request. The decider may not see the period's company: the request
// is what authorises the change, so the period is read without the
// caller's company filter.
func namingRequest(ctx context.Context, tx pgx.Tx, period, request uuid.UUID) (Period, error) {
	if _, err := tx.Exec(ctx, `SELECT FROM ledger.period_status WHERE id = $1 FOR UPDATE`, period); err != nil {
		return Period{}, fmt.Errorf("ledger: lock fiscal period %s of unlock request %s: %w", period, request, err)
	}
	p, err := scanPeriod(tx.QueryRow(ctx, periodSelect+` WHERE id = $1`, period))
	if err != nil {
		return Period{}, fmt.Errorf("ledger: read fiscal period %s of unlock request %s: %w", period, request, err)
	}
	if p.Status != Locked || p.UnlockRequestID == nil || *p.UnlockRequestID != request {
		return Period{}, fmt.Errorf("ledger: fiscal period %s (%s) does not name unlock request %s", period, p.Status, request)
	}
	return p, nil
}
