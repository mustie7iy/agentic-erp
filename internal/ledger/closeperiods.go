package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
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
	rows, _ := tx.Query(ctx, `SELECT l.id, l.code, l.name, c.is_leading, p.id, p.start_date, p.end_date, p.status, p.version
		FROM ledger.company_ledger c JOIN ledger.ledger l ON l.tenant_id = c.tenant_id AND l.id = c.ledger_id
		LEFT JOIN (`+periodSelect+`) p ON p.legal_entity_id = c.legal_entity_id AND p.ledger_id = c.ledger_id AND p.fiscal_year = $4 AND p.period = $5
		WHERE ($1 OR c.legal_entity_id = ANY($2)) AND c.legal_entity_id = $3
		ORDER BY NOT c.is_leading, l.code`, access.All, access.IDs, legalEntity, fiscalYear, period)
	kept, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (KeptPeriod, error) {
		var k KeptPeriod
		var id *uuid.UUID
		var start, end *time.Time
		var status *string
		var version *int32
		err := row.Scan(&k.LedgerID, &k.LedgerCode, &k.LedgerName, &k.IsLeading, &id, &start, &end, &status, &version)
		if err == nil && id != nil {
			k.Period = &Period{ID: *id, LegalEntityID: legalEntity, LedgerID: k.LedgerID, FiscalYear: fiscalYear, Period: period,
				StartDate: *start, EndDate: *end, Status: *status, Version: *version}
		}
		return k, errs.Wrapf(err, "scan kept ledger")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: periods of %s, %d/%d, by ledger: %w", legalEntity, fiscalYear, period, err)
	}
	return kept, nil
}

// KeptYear is a ledger a legal entity keeps and its posting periods of a
// fiscal year, regular and special, by period (none: the year is not set
// up in it), as a year close reads them (ADR-0060 §2, §8: the periods of a
// company and year per ledger it keeps).
type KeptYear struct {
	LedgerID   uuid.UUID
	LedgerCode string
	LedgerName string
	IsLeading  bool
	Periods    []Period
}

// OfYear returns the ledgers the legal entity keeps, the leading one
// first, then by code, each with its posting periods of the fiscal year:
// none for a legal entity the caller may not act in.
func (ClosePeriods) OfYear(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) ([]KeptYear, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.id, l.code, l.name, c.is_leading, p.id, p.period, p.start_date, p.end_date, p.is_special, p.status, p.version
		FROM ledger.company_ledger c JOIN ledger.ledger l ON l.tenant_id = c.tenant_id AND l.id = c.ledger_id
		LEFT JOIN (`+periodSelect+`) p ON p.legal_entity_id = c.legal_entity_id AND p.ledger_id = c.ledger_id AND p.fiscal_year = $4
		WHERE ($1 OR c.legal_entity_id = ANY($2)) AND c.legal_entity_id = $3
		ORDER BY NOT c.is_leading, l.code, p.period`, access.All, access.IDs, legalEntity, fiscalYear)
	var out []KeptYear
	var k KeptYear
	var id *uuid.UUID
	var period *int16
	var version *int32
	var start, end *time.Time
	var special *bool
	var status *string
	_, err = pgx.ForEachRow(rows, []any{&k.LedgerID, &k.LedgerCode, &k.LedgerName, &k.IsLeading, &id, &period, &start, &end, &special, &status, &version},
		func() error {
			if len(out) == 0 || out[len(out)-1].LedgerID != k.LedgerID {
				out = append(out, KeptYear{LedgerID: k.LedgerID, LedgerCode: k.LedgerCode, LedgerName: k.LedgerName, IsLeading: k.IsLeading})
			}
			if id != nil {
				last := &out[len(out)-1]
				last.Periods = append(last.Periods, Period{ID: *id, LegalEntityID: legalEntity, LedgerID: k.LedgerID, FiscalYear: fiscalYear,
					Period: *period, StartDate: *start, EndDate: *end, IsSpecial: *special, Status: *status, Version: *version})
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("ledger: periods of %s in fiscal year %d, by ledger: %w", legalEntity, fiscalYear, err)
	}
	return out, nil
}

// LockYear reads the posting periods of the fiscal year, regular and
// special, of the legal entity's ledgers among ledgers FOR UPDATE, for a
// year close's sign-off (ADR-0060 §8): in one statement and in the
// project's one order, by period, then the leading ledger first, then by
// code (03 §3.7's entry locks), so that no period is opened or unlocked
// between the sign-off's checks and its commit, and a posting, which takes
// one period of every ledger it posts to in that order, never waits on
// the sign-off in the opposite order. Each row is a ledger with one
// period, in that order: none for a legal entity the caller may not act
// in.
func (ClosePeriods) LockYear(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16, ledgers []uuid.UUID) ([]KeptPeriod, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.id, l.code, l.name, l.is_leading, s.id, s.period, f.start_date, f.end_date, f.is_special, s.status, s.version
		FROM ledger.period_status s JOIN ledger.fiscal_period f ON f.tenant_id = s.tenant_id AND f.calendar_id = s.calendar_id
			AND f.fiscal_year = s.fiscal_year AND f.period = s.period
		JOIN ledger.ledger l ON l.tenant_id = s.tenant_id AND l.id = s.ledger_id
		WHERE ($1 OR s.legal_entity_id = ANY($2)) AND s.legal_entity_id = $3 AND s.fiscal_year = $4 AND s.ledger_id = ANY($5)
		ORDER BY s.period, NOT l.is_leading, l.code FOR UPDATE OF s`, access.All, access.IDs, legalEntity, fiscalYear, ledgers)
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (KeptPeriod, error) {
		var k KeptPeriod
		p := Period{LegalEntityID: legalEntity, FiscalYear: fiscalYear}
		err := row.Scan(&k.LedgerID, &k.LedgerCode, &k.LedgerName, &k.IsLeading, &p.ID, &p.Period, &p.StartDate, &p.EndDate, &p.IsSpecial,
			&p.Status, &p.Version)
		p.LedgerID, k.Period = k.LedgerID, &p
		return k, errs.Wrapf(err, "scan locked period")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: lock the periods of %s in fiscal year %d: %w", legalEntity, fiscalYear, err)
	}
	return out, nil
}

// Lock locks a closed period as lockFiscalPeriod does, audited and
// published as that is, for a close's sign-off (ADR-0058 §7): version is
// the one the sign-off read the period at, so a period changed since (an
// open, a close, an unlock) refuses it (412 request.version_mismatch)
// rather than being locked unseen. The caller locks a close's periods in
// the order postings take them, the leading ledger first, then by code
// (postingPeriod), so a sign-off and a posting never wait on each other's
// periods in opposite orders.
func (ClosePeriods) Lock(ctx context.Context, id uuid.UUID, version int32) (Period, error) {
	return LockPeriod(ctx, id, etag.IfMatch(etag.Format(version)))
}
