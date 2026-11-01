package taxfiling

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// file records an approved return as filed with the authority's
// confirmation (P3) and locks the fiscal periods it covers that are not
// yet locked (§11, T32). The transition is guarded by the status it read
// under the row's lock, so a retry after a lost answer is refused rather
// than filed and published twice.
func (e *Endpoints) file(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, number string, filedOn time.Time) (Return, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Return{}, err
	}
	r, err := movable(ctx, tx, id)
	if err != nil {
		return Return{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return Return{}, err
	}
	if r.Status != Approved {
		return Return{}, statusConflict(r, Approved)
	}
	if number = strings.TrimSpace(number); number == "" {
		return Return{}, problem.Unprocessable("tax.confirmation_required", "The authority's confirmation number is required.",
			problem.FieldError{Field: "confirmation_number", Code: "required", Message: "the confirmation number the authority gave the filing"})
	}
	day, err := config.EntityDay(ctx, e.Settings, e.companyCode, r.LegalEntityID, e.Now())
	if err != nil {
		return Return{}, err
	}
	if !filedOn.After(r.PeriodEnd) || filedOn.After(day.Date) {
		return Return{}, problem.Unprocessable("tax.invalid_filed_on", fmt.Sprintf("A return of the period to %s is filed after it ends and by today, %s in %s.",
			r.PeriodEnd.Format(time.DateOnly), day.Date.Format(time.DateOnly), day.Zone),
			problem.FieldError{Field: "filed_on", Code: "out_of_range", Message: "a date after the period's end and not after the company's date today"})
	}
	locked, err := e.lockPeriods(ctx, r)
	if err != nil {
		return Return{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tax.tax_return SET status = 'filed', confirmation_number = $2, filed_on = $3, filed_by = $4, filed_at = now(),
			version = version + 1, updated_by = $4 WHERE id = $1 AND status = 'approved'`, r.ID, number, filedOn, p.ID); err != nil {
		return Return{}, fmt.Errorf("taxfiling: file return %s: %w", r.ID, err)
	}
	filed, err := readReturn(ctx, tx, r.ID, nil, false)
	if err != nil {
		return Return{}, err
	}
	payload := map[string]any{"confirmation_number": number, "filed_on": filedOn.Format(time.DateOnly), "item_count": r.ItemCount,
		"items_hash": r.ItemsHash, "locked_periods": locked}
	return filed, audit.Record(ctx, "tax.return.file", "tax.tax_return", r.ID, payload, event("tax_return_filed", filed, payload))
}

// lockPeriods locks the closed fiscal periods the return covers, once none
// is open, and names those it locked.
func (e *Endpoints) lockPeriods(ctx context.Context, r Return) ([]map[string]any, error) {
	codes := map[uuid.UUID]string{}
	for _, le := range r.entities() {
		code, _, err := e.companyCode(ctx, le) // legal entities are never deleted, and Setup names only existing ones
		if err != nil {
			return nil, err
		}
		codes[le] = code
	}
	periods, err := e.closedPeriods(ctx, r.entities(), codes, r.PeriodStart, r.PeriodEnd)
	if err != nil {
		return nil, err
	}
	locked := []map[string]any{}
	for _, fp := range periods {
		if fp.Status != ledger.Closed {
			continue
		}
		if _, err := e.Periods.Lock(ctx, fp.ID); err != nil {
			return nil, err
		}
		locked = append(locked, map[string]any{"period_id": fp.ID, "legal_entity_id": fp.LegalEntityID, "company_code": codes[fp.LegalEntityID],
			"fiscal_year": fp.FiscalYear, "period": fp.Period})
	}
	return locked, nil
}

// withdraw withdraws a return not yet filed: its pending approval request
// is cancelled and its items released (§11).
func (e *Endpoints) withdraw(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Return, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Return{}, err
	}
	r, err := movable(ctx, tx, id)
	if err != nil {
		return Return{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return Return{}, err
	}
	if r.Status != Prepared && r.Status != Approved {
		return Return{}, statusConflict(r, Prepared+" or "+Approved)
	}
	if r.Status == Prepared {
		if err := e.Approvals.Withdraw(ctx, approvals.TaxReturn, r.ID, reason); err != nil {
			return Return{}, err
		}
	}
	return e.end(ctx, tx, p, r, Withdrawn, "withdraw", reason)
}

// end withdraws or rejects the return and releases its items to the next
// return, guarded by the status it read under the row's lock.
func (e *Endpoints) end(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Return, status, action, comment string) (Return, error) {
	if _, err := tx.Exec(ctx, `UPDATE tax.tax_return SET status = $3, decision_comment = nullif($4, ''), decided_by = $5, decided_at = now(),
			version = version + 1, updated_by = $5 WHERE id = $1 AND status = $2`, r.ID, r.Status, status, comment, p.ID); err != nil {
		return Return{}, fmt.Errorf("taxfiling: %s return %s: %w", action, r.ID, err)
	}
	released, err := e.Items.Release(ctx, r.ID)
	if err != nil {
		return Return{}, err
	}
	ended, err := readReturn(ctx, tx, r.ID, nil, false)
	if err != nil {
		return Return{}, err
	}
	payload := map[string]any{"previous_status": r.Status, "status": status, "released_items": released, "comment": ended.DecisionComment}
	return ended, audit.Record(ctx, "tax.return."+action, "tax.tax_return", r.ID, payload, event("tax_return_"+status, ended, payload))
}

// TaxReturns moves a return on when its tax_return approval request
// completes (approvals.Handler), in the deciding transaction. Each acts
// only on a prepared return that names the request, so a decision
// repeated or on another request changes nothing and fails.
type TaxReturns struct {
	Endpoints *Endpoints
}

// pending locks the return and returns it if it is prepared on the
// request. The decider may not act in its companies: the request is what
// authorises the decision.
func pending(ctx context.Context, ret, request uuid.UUID) (pgx.Tx, reqctx.Principal, Return, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, p, Return{}, err
	}
	r, err := readReturn(ctx, tx, ret, nil, true)
	if err != nil {
		return nil, p, Return{}, err
	}
	if r.Status != Prepared || r.ApprovalRequestID != request {
		return nil, p, Return{}, fmt.Errorf("taxfiling: tax return %s (%s) is not prepared on request %s", ret, r.Status, request)
	}
	return tx, p, r, nil
}

// Approved approves the return: the decider is a person other than its
// preparer, as the approvals context holds the requester to, and the
// database to its creator (tax_return_four_eyes).
func (TaxReturns) Approved(ctx context.Context, ret, request uuid.UUID) error {
	tx, p, r, err := pending(ctx, ret, request)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE tax.tax_return SET status = 'approved', approved_by = $3, approved_at = now(), version = version + 1, updated_by = $3
		WHERE id = $1 AND approval_request_id = $2 AND status = 'prepared'`, ret, request, p.ID); err != nil {
		return fmt.Errorf("taxfiling: approve return %s: %w", ret, err)
	}
	payload := map[string]any{"approval_request_id": request, "approved_by": p.ID}
	return audit.Record(ctx, "tax.return.approve", "tax.tax_return", ret, payload, event("tax_return_approved", r, payload))
}

// Rejected rejects the return and releases its items.
func (h TaxReturns) Rejected(ctx context.Context, ret, request uuid.UUID, comment string) error {
	tx, p, r, err := pending(ctx, ret, request)
	if err != nil {
		return err
	}
	_, err = h.Endpoints.end(ctx, tx, p, r, Rejected, "reject", comment)
	return err
}

// Cancelled withdraws the return, as its preparer withdrew its request,
// and releases its items.
func (h TaxReturns) Cancelled(ctx context.Context, ret, request uuid.UUID, comment string) error {
	tx, p, r, err := pending(ctx, ret, request)
	if err != nil {
		return err
	}
	_, err = h.Endpoints.end(ctx, tx, p, r, Withdrawn, "withdraw", comment)
	return err
}

// Resources is where the API reads each tax return (approvals.Locator).
func (TaxReturns) Resources(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return approvals.Paths("/v1/tax-returns/%s", ids), nil
}
