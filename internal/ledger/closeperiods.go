package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ClosePeriods is the ledger's port for the posting periods a financial
// close reads (ADR-0058 §2-§4): the close calendar is one row per regular
// period of a company's leading ledger, and a period close covers a
// period of each ledger it names.
type ClosePeriods struct{}

// Leading returns the leading ledger's regular posting periods of the
// legal entity in the fiscal year, by period: none for a legal entity the
// caller may not act in. Special periods are the year-end close's, which
// T64 leaves out (ADR-0058 §1).
func (ClosePeriods) Leading(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) ([]Period, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, periodSelect+` WHERE ($1 OR legal_entity_id = ANY($2)) AND legal_entity_id = $3 AND fiscal_year = $4 AND NOT is_special
		AND ledger_id = (SELECT g.id FROM ledger.ledger g WHERE g.is_leading) ORDER BY period`, access.All, access.IDs, legalEntity, fiscalYear)
	periods, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Period, error) { return scanPeriod(row) })
	if err != nil {
		return nil, fmt.Errorf("ledger: periods of %s in fiscal year %d: %w", legalEntity, fiscalYear, err)
	}
	return periods, nil
}

// KeptPeriod is a ledger a legal entity keeps and its posting period of a
// fiscal year and period, if it has one, as a period close reads them
// (ADR-0058 §4: the periods of a company, year and period per ledger).
type KeptPeriod struct {
	LedgerID   uuid.UUID
	LedgerCode string
	LedgerName string
	IsLeading  bool
	Period     *Period // nil: the ledger has no such period
}

// Of returns the ledgers the legal entity keeps, the leading one first,
// then by code, each with its posting period of the fiscal year and
// period: none for a legal entity the caller may not act in.
func (ClosePeriods) Of(ctx context.Context, legalEntity uuid.UUID, fiscalYear, period int16) ([]KeptPeriod, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.id, l.code, l.name, c.is_leading, p.id, p.start_date, p.end_date, p.status
		FROM ledger.company_ledger c JOIN ledger.ledger l ON l.tenant_id = c.tenant_id AND l.id = c.ledger_id
		LEFT JOIN (`+periodSelect+`) p ON p.legal_entity_id = c.legal_entity_id AND p.ledger_id = c.ledger_id AND p.fiscal_year = $4 AND p.period = $5
		WHERE ($1 OR c.legal_entity_id = ANY($2)) AND c.legal_entity_id = $3
		ORDER BY NOT c.is_leading, l.code`, access.All, access.IDs, legalEntity, fiscalYear, period)
	kept, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (KeptPeriod, error) {
		var k KeptPeriod
		var id *uuid.UUID
		var start, end *time.Time
		var status *string
		err := row.Scan(&k.LedgerID, &k.LedgerCode, &k.LedgerName, &k.IsLeading, &id, &start, &end, &status)
		if err == nil && id != nil {
			k.Period = &Period{ID: *id, LegalEntityID: legalEntity, LedgerID: k.LedgerID, FiscalYear: fiscalYear, Period: period,
				StartDate: *start, EndDate: *end, Status: *status}
		}
		return k, errs.Wrapf(err, "scan kept ledger")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: periods of %s, %d/%d, by ledger: %w", legalEntity, fiscalYear, period, err)
	}
	return kept, nil
}
