package taxfiling

// The return lifecycle (ADR-0049 §11-§13, T61i). A return of a
// registration's period is prepared from its draft: the snapshot of its
// boxes, the keys of the tax items it claims and their hash, and the pack
// version that defined it, stored with the items' reported_in set to it in
// the same transaction, under a lock on the registration. A person other
// than the preparer approves it through a tax_return approval request
// (approvals.TaxReturn), a person records it filed with the authority's
// confirmation (P3), which locks the fiscal periods it covers (T32), and
// before that it may be withdrawn, or its request rejected, which releases
// its items. A filed return is corrected by an amendment of its period,
// which claims the items dated in the period that arrived after the
// filing. Every transition is a person's (§13: x-agent false, and the
// approvals context's decisions are people's), audited and published
// through the tax outbox.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Return kinds and statuses.
const (
	Original  = "original"
	Amendment = "amendment"

	Prepared  = "prepared"
	Approved  = "approved"
	Filed     = "filed"
	Withdrawn = "withdrawn"
	Rejected  = "rejected"
)

func init() {
	problem.RegisterConstraint("tax_return_one_original", func() *problem.Problem {
		return problem.Conflict("tax.return_exists", "The period already has an original return that stands; amend it once it is filed.")
	})
	problem.RegisterConstraint("tax_return_in_progress", func() *problem.Problem {
		return problem.Conflict("tax.return_in_progress", "A return of the period is prepared or approved; file or withdraw it first.")
	})
}

// Opener opens and withdraws approval requests (approvals context).
type Opener interface {
	Open(ctx context.Context, in approvals.NewRequest) (uuid.UUID, bool, error)
	Withdraw(ctx context.Context, objectType string, objectID uuid.UUID, comment string) error
}

// FiscalPeriods are the ledger's posting periods a return covers
// (ledger.ReturnPeriods).
type FiscalPeriods interface {
	Overlapping(ctx context.Context, legalEntities []uuid.UUID, start, end time.Time) ([]ledger.Period, error)
	Lock(ctx context.Context, id uuid.UUID) (ledger.Period, error)
}

// Return is a stored tax return with its snapshot's boxes and its
// registration's companies.
type Return struct {
	ID, RegistrationID       uuid.UUID
	RegistrationCode         string
	LegalEntityID            uuid.UUID
	Members                  []uuid.UUID
	Kind                     string
	AmendsID                 *uuid.UUID
	PeriodStart, PeriodEnd   time.Time
	Status                   string
	ReturnType, ReturnName   string
	PackCountry, PackVersion string
	Currency                 string
	Exponent                 int
	ItemCount                int64
	ItemsHash                string
	PriorItemCount           int64
	PriorTax                 money.Minor
	PriorThreshold           *money.Minor
	AboveThreshold           bool
	Boxes                    []ReturnBox
	ApprovalRequestID        uuid.UUID
	ApprovedBy               *uuid.UUID
	ApprovedAt               *time.Time
	ConfirmationNumber       *string
	FiledOn                  *time.Time
	FiledBy                  *uuid.UUID
	FiledAt                  *time.Time
	DecisionComment          *string
	DecidedBy                *uuid.UUID
	DecidedAt                *time.Time
	Version                  int32
	CreatedAt                time.Time
	CreatedBy                uuid.UUID
	UpdatedAt                time.Time
	UpdatedBy                uuid.UUID
}

// entities are the companies the return covers, registrant first.
func (r Return) entities() []uuid.UUID { return append([]uuid.UUID{r.LegalEntityID}, r.Members...) }

// ReturnBox is a box of a return's snapshot, in minor units of its
// currency: what its items sum to, their prior-period part, and the
// period's figure after the return (an amendment's corrects the amended
// return's).
type ReturnBox struct {
	Code              string      `json:"code"`
	Label             string      `json:"label"`
	Kind              string      `json:"kind"`
	Amount            money.Minor `json:"amount"`
	PriorPeriodAmount money.Minor `json:"prior_period_amount"`
	CorrectedAmount   money.Minor `json:"corrected_amount"`
	ItemCount         *int64      `json:"item_count"`
}

// returnSelect reads returns with their registration's companies and their
// boxes; its columns are the outer query's, for the visible condition and
// keyset paging.
const returnSelect = `SELECT * FROM (SELECT r.id, r.registration_id, g.code AS registration_code, g.legal_entity_id, g.group_member_ids, r.kind,
		r.amends_id, r.period_start, r.period_end, r.status, r.return_type, r.return_name, r.pack_country, r.pack_version, r.currency, c.exponent,
		r.item_count, r.items_hash, r.prior_item_count, r.prior_tax, r.prior_threshold, r.above_threshold,
		coalesce((SELECT json_agg(json_build_object('code', b.code, 'label', b.label, 'kind', b.kind, 'amount', b.amount,
			'prior_period_amount', b.prior_period_amount, 'corrected_amount', b.corrected_amount, 'item_count', b.item_count) ORDER BY b.box_no)
		 FROM tax.tax_return_box b WHERE b.tenant_id = r.tenant_id AND b.tax_return_id = r.id), '[]') AS boxes,
		r.approval_request_id, r.approved_by, r.approved_at, r.confirmation_number, r.filed_on, r.filed_by, r.filed_at, r.decision_comment,
		r.decided_by, r.decided_at, r.version, r.created_at, r.created_by, r.updated_at, r.updated_by
	FROM tax.tax_return r JOIN tax.registration g ON g.tenant_id = r.tenant_id AND g.id = r.registration_id
	JOIN ref.currency c ON c.code = r.currency) r`

func scanReturn(row pgx.CollectableRow) (Return, error) {
	var r Return
	err := row.Scan(&r.ID, &r.RegistrationID, &r.RegistrationCode, &r.LegalEntityID, &r.Members, &r.Kind, &r.AmendsID, &r.PeriodStart, &r.PeriodEnd,
		&r.Status, &r.ReturnType, &r.ReturnName, &r.PackCountry, &r.PackVersion, &r.Currency, &r.Exponent, &r.ItemCount, &r.ItemsHash,
		&r.PriorItemCount, &r.PriorTax, &r.PriorThreshold, &r.AboveThreshold, &r.Boxes, &r.ApprovalRequestID, &r.ApprovedBy, &r.ApprovedAt,
		&r.ConfirmationNumber, &r.FiledOn, &r.FiledBy, &r.FiledAt, &r.DecisionComment, &r.DecidedBy, &r.DecidedAt, &r.Version, &r.CreatedAt,
		&r.CreatedBy, &r.UpdatedAt, &r.UpdatedBy)
	return r, errs.Wrapf(err, "scan tax return")
}

// readReturn reads a return of a registration covering a company the
// caller may act in, locked FOR UPDATE when lock is set; any return,
// whoever the caller, when access is nil (an approval's decider, whom the
// request authorises).
func readReturn(ctx context.Context, tx pgx.Tx, id uuid.UUID, access *authz.Access, lock bool) (Return, error) {
	cond, args := `id = $1`, []any{id}
	if access != nil {
		cond, args = `id = $1 AND `+visibleFrom(2), []any{id, access.All, access.IDs}
	}
	if lock {
		// Only a return the caller sees: one it may not must not stay
		// locked until rollback.
		if _, err := tx.Exec(ctx, `SELECT FROM tax.tax_return r JOIN tax.registration g ON g.tenant_id = r.tenant_id AND g.id = r.registration_id
			WHERE r.`+cond+` FOR UPDATE OF r`, args...); err != nil {
			return Return{}, fmt.Errorf("taxfiling: lock tax return %s: %w", id, err)
		}
	}
	rows, _ := tx.Query(ctx, returnSelect+` WHERE `+cond, args...)
	r, err := pgx.CollectExactlyOneRow(rows, scanReturn)
	if errors.Is(err, pgx.ErrNoRows) {
		return Return{}, problem.NotFoundOf("tax return")
	}
	if err != nil {
		return Return{}, fmt.Errorf("taxfiling: tax return %s: %w", id, err)
	}
	return r, nil
}

// visibleFrom is the visible condition with its two parameters numbered
// from n.
func visibleFrom(n int) string {
	return fmt.Sprintf(`($%d OR legal_entity_id = ANY($%d) OR group_member_ids && $%d::uuid[])`, n, n+1, n+1)
}

// movable reads a return to move, locked: one of a registration the caller
// sees, covering only companies they may act in, as preparing it needed.
func movable(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Return, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Return{}, err
	}
	r, err := readReturn(ctx, tx, id, &access, true)
	if err != nil {
		return Return{}, err
	}
	for _, le := range r.entities() {
		if !access.Allows(le) {
			return Return{}, problem.Forbidden("tax.registration_companies",
				"A return of registration "+r.RegistrationCode+" covers every company it does, and the caller may not act in all of them.")
		}
	}
	return r, nil
}

// statusConflict refuses a move the return's status does not allow.
func statusConflict(r Return, wants string) error {
	return problem.Conflict("tax.invalid_return_status", fmt.Sprintf("Tax return %s of %s for %s to %s is %s; this requires status %s.",
		r.ID, r.RegistrationCode, r.PeriodStart.Format(time.DateOnly), r.PeriodEnd.Format(time.DateOnly), r.Status, wants))
}

// event is a tax return's event.
func event(name string, r Return, payload map[string]any) outbox.Event {
	p := map[string]any{"tax_return_id": r.ID, "registration_id": r.RegistrationID, "registration_code": r.RegistrationCode,
		"legal_entity_id": r.LegalEntityID, "kind": r.Kind, "return_type": r.ReturnType, "period_start": r.PeriodStart.Format(time.DateOnly),
		"period_end": r.PeriodEnd.Format(time.DateOnly)}
	for k, v := range payload {
		p[k] = v
	}
	return outbox.Event{Context: "tax", Name: name, AggregateType: "tax.tax_return", AggregateID: r.ID, Payload: p}
}
