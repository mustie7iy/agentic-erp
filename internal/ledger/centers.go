package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Cost centers, profit centers and segments (ADR-0050 §3, T56a) are the
// ledger's master data; journal lines carry them from T56d. A center is
// created, and its periods and companies changed, only through an approved
// center change (centerchange.go); its texts change at once.

// Center statuses.
const (
	CenterPending  = "pending"
	CenterActive   = "active"
	CenterRejected = "rejected"
)

func init() {
	problem.RegisterConstraint("cost_center_code_once", func() *problem.Problem {
		return problem.Conflict("ledger.cost_center_exists", "The legal entity already has a cost center with this code.")
	})
	problem.RegisterConstraint("profit_center_code_once", func() *problem.Problem {
		return problem.Conflict("ledger.profit_center_exists", "The tenant already has a profit center with this code.")
	})
	problem.RegisterConstraint("segment_code_once", func() *problem.Problem {
		return problem.Conflict("ledger.segment_exists", "The tenant already has a segment with this code.")
	})
	problem.RegisterConstraint("center_change_one_pending", func() *problem.Problem {
		return problem.Conflict("ledger.center_change_pending", "The center already has a pending change; it must be decided or withdrawn first.")
	})
}

// Texts are a center's texts, which change without approval.
type Texts struct {
	Name        string
	Description *string
	Responsible string
}

// centerHead is what cost and profit centers have in common.
type centerHead struct {
	ID              uuid.UUID
	Code            string
	Texts           Texts
	Status          string
	PendingChangeID *uuid.UUID
	Version         int32
	CreatedAt       time.Time
	CreatedBy       uuid.UUID
	UpdatedAt       time.Time
	UpdatedBy       uuid.UUID
}

// CostCenterAttrs are what a cost center period says.
type CostCenterAttrs struct {
	Category       string
	ProfitCenterID uuid.UUID // uuid.Nil: none
	LockCost       bool
	LockRevenue    bool
	LockPlan       bool
}

// ProfitCenterAttrs are what a profit center period says.
type ProfitCenterAttrs struct {
	Locked bool
}

// CostCenter is a row of ledger.cost_center with its active periods.
type CostCenter struct {
	centerHead
	LegalEntityID uuid.UUID
	Periods       []span[CostCenterAttrs]
}

// ProfitCenter is a row of ledger.profit_center with its companies and
// active periods.
type ProfitCenter struct {
	centerHead
	SegmentID      *uuid.UUID
	LegalEntityIDs []uuid.UUID
	Periods        []span[ProfitCenterAttrs]
}

// Segment is a row of ledger.segment.
type Segment struct {
	ID          uuid.UUID
	Code        string
	Name        string
	Description *string
	Version     int32
	CreatedAt   time.Time
	CreatedBy   uuid.UUID
	UpdatedAt   time.Time
	UpdatedBy   uuid.UUID
}

const headColumns = `c.id, c.code, c.name, c.description, c.responsible, c.status, c.version, c.created_at, c.created_by, c.updated_at, c.updated_by`

func headDest(h *centerHead) []any {
	return []any{&h.ID, &h.Code, &h.Texts.Name, &h.Texts.Description, &h.Texts.Responsible, &h.Status, &h.Version, &h.CreatedAt, &h.CreatedBy,
		&h.UpdatedAt, &h.UpdatedBy, &h.PendingChangeID}
}

const costCenterSelect = `SELECT ` + headColumns + `,
		(SELECT ch.id FROM ledger.center_change ch WHERE ch.tenant_id = c.tenant_id AND ch.cost_center_id = c.id AND ch.status = 'pending') AS pending_change_id,
		c.legal_entity_id FROM ledger.cost_center c`

const profitCenterSelect = `SELECT ` + headColumns + `,
		(SELECT ch.id FROM ledger.center_change ch WHERE ch.tenant_id = c.tenant_id AND ch.profit_center_id = c.id AND ch.status = 'pending') AS pending_change_id,
		c.segment_id, ARRAY(SELECT pc.legal_entity_id FROM ledger.profit_center_company pc
			WHERE pc.tenant_id = c.tenant_id AND pc.profit_center_id = c.id ORDER BY pc.legal_entity_id)
	FROM ledger.profit_center c`

func scanCostCenter(row pgx.Row) (CostCenter, error) {
	var c CostCenter
	err := row.Scan(append(headDest(&c.centerHead), &c.LegalEntityID)...)
	return c, errs.Wrapf(err, "scan cost center")
}

func scanProfitCenter(row pgx.Row) (ProfitCenter, error) {
	var c ProfitCenter
	err := row.Scan(append(headDest(&c.centerHead), &c.SegmentID, &c.LegalEntityIDs)...)
	return c, errs.Wrapf(err, "scan profit center")
}

// costCenterPeriods reads the active periods of the cost centers, in date
// order, with the row lock given: none, or periodsForShare for a posting.
func costCenterPeriods(ctx context.Context, tx pgx.Tx, ids []uuid.UUID, lock string) (map[uuid.UUID][]span[CostCenterAttrs], error) {
	rows, _ := tx.Query(ctx, `SELECT cost_center_id, id, valid_from, valid_to, category, profit_center_id, lock_actual_cost, lock_actual_revenue, lock_plan
		FROM ledger.cost_center_period WHERE cost_center_id = ANY($1) AND retired_by_change_id IS NULL ORDER BY cost_center_id, valid_from`+lock, ids)
	out := map[uuid.UUID][]span[CostCenterAttrs]{}
	var owner uuid.UUID
	var s span[CostCenterAttrs]
	var pc *uuid.UUID
	var end *time.Time
	_, err := pgx.ForEachRow(rows, []any{&owner, &s.ID, &s.From, &end, &s.Attr.Category, &pc, &s.Attr.LockCost, &s.Attr.LockRevenue, &s.Attr.LockPlan},
		func() error {
			s.To, s.Attr.ProfitCenterID = time.Time{}, uuid.Nil
			if end != nil {
				s.To = *end
			}
			if pc != nil {
				s.Attr.ProfitCenterID = *pc
			}
			out[owner] = append(out[owner], s)
			return nil
		})
	return out, errs.Wrapf(err, "read cost center periods")
}

// profitCenterPeriods reads the active periods of the profit centers, in
// date order, with the row lock given (costCenterPeriods).
func profitCenterPeriods(ctx context.Context, tx pgx.Tx, ids []uuid.UUID, lock string) (map[uuid.UUID][]span[ProfitCenterAttrs], error) {
	rows, _ := tx.Query(ctx, `SELECT profit_center_id, id, valid_from, valid_to, locked
		FROM ledger.profit_center_period WHERE profit_center_id = ANY($1) AND retired_by_change_id IS NULL ORDER BY profit_center_id, valid_from`+lock, ids)
	out := map[uuid.UUID][]span[ProfitCenterAttrs]{}
	var owner uuid.UUID
	var s span[ProfitCenterAttrs]
	var end *time.Time
	_, err := pgx.ForEachRow(rows, []any{&owner, &s.ID, &s.From, &end, &s.Attr.Locked}, func() error {
		s.To = time.Time{}
		if end != nil {
			s.To = *end
		}
		out[owner] = append(out[owner], s)
		return nil
	})
	return out, errs.Wrapf(err, "read profit center periods")
}

// getCostCenter reads a cost center, locked for update when lock is set.
// Outside the caller's company access it is not found; the approvals
// callback reads with authz.Access{All: true}, since the request is what
// authorises the change.
func getCostCenter(ctx context.Context, tx pgx.Tx, access authz.Access, id uuid.UUID, lock bool) (CostCenter, error) {
	q := costCenterSelect + ` WHERE c.id = $1`
	if lock {
		q += ` FOR UPDATE OF c`
	}
	c, err := scanCostCenter(tx.QueryRow(ctx, q, id))
	if err := authz.Visible(err, access, c.LegalEntityID, problem.NotFoundOf("cost center"), "ledger: read cost center %s", id); err != nil {
		return CostCenter{}, err
	}
	periods, err := costCenterPeriods(ctx, tx, []uuid.UUID{id}, noLock)
	c.Periods = periods[id]
	return c, err
}

// getProfitCenter reads a profit center with the row lock given: for
// update to change it, for share while a cost center is given it, so that
// its validity does not change meanwhile.
func getProfitCenter(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock string) (ProfitCenter, error) {
	q := profitCenterSelect + ` WHERE c.id = $1` + lock
	c, err := scanProfitCenter(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProfitCenter{}, problem.NotFoundOf("profit center")
	}
	if err != nil {
		return ProfitCenter{}, fmt.Errorf("ledger: read profit center %s: %w", id, err)
	}
	periods, err := profitCenterPeriods(ctx, tx, []uuid.UUID{id}, noLock)
	c.Periods = periods[id]
	return c, err
}

// Row locks for getProfitCenter and the period readers. A posting holds
// the periods it reads for share (periodsForShare): every change of a
// center retires the periods it does not keep, an update that waits for
// the postings under way (ADR-0050 §5).
const (
	noLock          = ""
	forUpdate       = ` FOR UPDATE OF c`
	forShare        = ` FOR SHARE OF c`
	periodsForShare = ` FOR SHARE`
)

const segmentColumns = `id, code, name, description, version, created_at, created_by, updated_at, updated_by`

func scanSegment(row pgx.Row) (Segment, error) {
	var s Segment
	err := row.Scan(&s.ID, &s.Code, &s.Name, &s.Description, &s.Version, &s.CreatedAt, &s.CreatedBy, &s.UpdatedAt, &s.UpdatedBy)
	return s, errs.Wrapf(err, "scan segment")
}

func getSegment(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Segment, error) {
	q := `SELECT ` + segmentColumns + ` FROM ledger.segment WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	s, err := scanSegment(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Segment{}, problem.NotFoundOf("segment")
	}
	if err != nil {
		return Segment{}, fmt.Errorf("ledger: read segment %s: %w", id, err)
	}
	return s, nil
}
