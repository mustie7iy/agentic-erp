package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Posting period statuses.
const (
	Open   = "open"
	Closed = "closed"
	Locked = "locked"
)

// Period is the posting status of one fiscal period for one legal entity
// and ledger.
type Period struct {
	ID            uuid.UUID
	LegalEntityID uuid.UUID
	LedgerID      uuid.UUID
	CalendarID    uuid.UUID
	FiscalYear    int16
	Period        int16
	StartDate     time.Time
	EndDate       time.Time
	IsSpecial     bool
	Status        string
	Version       int32
	UpdatedAt     time.Time
	UpdatedBy     uuid.UUID
}

const periodSelect = `SELECT * FROM (SELECT s.id, s.legal_entity_id, s.ledger_id, s.calendar_id, s.fiscal_year, s.period, f.start_date, f.end_date,
		f.is_special, s.status, s.version, s.updated_at, s.updated_by
	FROM ledger.period_status s JOIN ledger.fiscal_period f ON f.tenant_id = s.tenant_id AND f.calendar_id = s.calendar_id
		AND f.fiscal_year = s.fiscal_year AND f.period = s.period) p`

func scanPeriod(row pgx.Row) (Period, error) {
	var p Period
	err := errs.Wrapf(row.Scan(&p.ID, &p.LegalEntityID, &p.LedgerID, &p.CalendarID, &p.FiscalYear, &p.Period, &p.StartDate, &p.EndDate,
		&p.IsSpecial, &p.Status, &p.Version, &p.UpdatedAt, &p.UpdatedBy), "scan fiscal period")
	return p, err
}

func getPeriod(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Period, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Period{}, err
	}
	if lock {
		if _, err := tx.Exec(ctx, `SELECT FROM ledger.period_status WHERE id = $1 FOR UPDATE`, id); err != nil {
			return Period{}, fmt.Errorf("ledger: lock fiscal period %s: %w", id, err)
		}
	}
	p, err := scanPeriod(tx.QueryRow(ctx, periodSelect+` WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(p.LegalEntityID)) {
		return Period{}, problem.NotFound("No fiscal period with this id.")
	}
	if err != nil {
		return Period{}, fmt.Errorf("ledger: read fiscal period %s: %w", id, err)
	}
	return p, nil
}

// GetPeriod returns a posting period.
func GetPeriod(ctx context.Context, id uuid.UUID) (Period, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Period{}, err
	}
	return getPeriod(ctx, tx, id, false)
}

// OpenPeriod opens a closed period for postings; a locked period stays
// locked.
func OpenPeriod(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Period, error) {
	return changePeriod(ctx, id, ifMatch, Closed, Open, "open", "period_opened")
}

// ClosePeriod closes an open period for postings.
func ClosePeriod(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Period, error) {
	return changePeriod(ctx, id, ifMatch, Open, Closed, "close", "period_closed")
}

func changePeriod(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, from, to, action, event string) (Period, error) {
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
	if p.Status != from {
		return Period{}, problem.Conflict("ledger.invalid_period_status", "The period is "+p.Status+"; this requires status "+from+".")
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.period_status SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1`, id, to, actor.ID); err != nil {
		return Period{}, fmt.Errorf("ledger: %s fiscal period %s: %w", action, id, err)
	}
	updated, err := getPeriod(ctx, tx, id, false)
	if err != nil {
		return Period{}, err
	}
	if _, err := outbox.Add(ctx, outbox.Event{Context: "ledger", Name: event, AggregateType: "ledger.period_status", AggregateID: id,
		Payload: map[string]any{"period_id": id, "legal_entity_id": p.LegalEntityID, "ledger_id": p.LedgerID, "calendar_id": p.CalendarID,
			"fiscal_year": p.FiscalYear, "period": p.Period, "start_date": p.StartDate.Format(time.DateOnly), "end_date": p.EndDate.Format(time.DateOnly),
			"status": to, "previous_status": from}}); err != nil {
		return Period{}, err
	}
	return updated, audit.Add(ctx, audit.Entry{Action: "ledger.fiscal_period." + action, ObjectType: "ledger.period_status", ObjectID: id,
		Outcome: "succeeded", Detail: map[string]any{"fiscal_year": p.FiscalYear, "period": p.Period, "from_status": from}})
}
