package closing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
)

// CheckYearCloseTask implements POST
// /v1/year-closes/{id}/tasks/{code}:check (ADR-0060 §8): runs a year
// close's check task's check now, as a period close's (CheckCloseTask).
func (e *Endpoints) CheckYearCloseTask(ctx context.Context, params gen.CheckYearCloseTaskParams) (gen.CheckYearCloseTaskRes, error) {
	return yearTask(e.taskAction(ctx, yearCloses, params.ID, params.Code, params.IfMatch, e.check))
}

// runYear runs a year close's check on its covered ledgers' periods of
// the year as the ledger holds them now (ADR-0060 §8, §11); a check this
// build does not run on a year close is broken data (the template's
// validation admits only config.YearCloseChecks).
func (e *Endpoints) runYear(ctx context.Context, tx pgx.Tx, t task) (bool, found, error) {
	covered, err := e.yearCoveredOf(ctx, tx, t.CloseID, t.LegalEntityID, t.FiscalYear)
	if err != nil {
		return false, found{}, err
	}
	switch *t.Check {
	case CheckNoParked:
		// The leading ledger, always covered, first: the year's dates are
		// its regular periods', which its special periods share.
		regular := regularPeriods(covered[0])
		return e.noParked(ctx, t.LegalEntityID, regular[0].StartDate, regular[len(regular)-1].EndDate, true)
	case CheckRevaluation:
		return e.revaluationPosted(ctx, t.LegalEntityID, yearEnds(covered), "year")
	case CheckDepreciationYear:
		return e.depreciationYearPosted(ctx, tx, t.LegalEntityID, t.FiscalYear, covered)
	case CheckCarryForward:
		return e.carryForwardCurrent(ctx, t.LegalEntityID, t.FiscalYear, yearEnds(covered))
	}
	return false, found{}, fmt.Errorf("closing: task %s of year close %s runs check %q, which this build does not run on a year close", t.Code, t.CloseID,
		*t.Check)
}

// regularPeriods are a ledger's regular periods of the year, by period.
func regularPeriods(l LedgerYear) []Period {
	var out []Period
	for _, p := range l.Periods {
		if !p.Special {
			out = append(out, p)
		}
	}
	return out
}

// yearEnd is a ledger with its last regular period of the year, whose
// last day is the year's (the special periods share it). A ledger's
// periods of a year are set up whole (ledger.Setup), so a ledger with
// periods of the year has its regular ones.
func yearEnd(l LedgerYear) LedgerPeriod {
	regular := regularPeriods(l)
	return LedgerPeriod{LedgerID: l.LedgerID, Code: l.Code, Name: l.Name, Leading: l.Leading, Period: &regular[len(regular)-1]}
}

// yearEnds are the ledgers each with its last regular period of the year.
func yearEnds(ledgers []LedgerYear) []LedgerPeriod {
	out := make([]LedgerPeriod, len(ledgers))
	for i, l := range ledgers {
		out[i] = yearEnd(l)
	}
	return out
}

// depreciationYearPosted passes when T64k's depreciation check passes for
// each covered ledger in each regular period of the year (ADR-0060 §11): a
// posted run of the period, nothing a run of it would select, or a posted
// run of a later period, its assets then warnings. It counts the ledgers'
// periods that fail and names the first of them, by ledger, then period.
func (e *Endpoints) depreciationYearPosted(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, year int16, covered []LedgerYear) (bool, found, error) {
	f := found{}
	var failing []string
	for _, l := range covered {
		periods, err := e.ledgerYearDepreciation(ctx, tx, &f, legalEntity, year, l)
		if err != nil {
			return false, found{}, err
		}
		if len(periods) > 0 {
			failing = append(failing, fmt.Sprintf("%s (period %s)", l.Code, strings.Join(periods, ", ")))
		}
	}
	f.Summary = fmt.Sprintf("Every covered ledger has a posted depreciation run, nothing to depreciate or a posted run of a later period in "+
		"each regular period of %d.", year)
	if len(failing) > 0 {
		f.Summary = fmt.Sprintf("No posted depreciation run of %d in ledger %s; run the depreciation of those periods first.", year,
			strings.Join(failing, ", "))
	}
	if f.WarningCount > 0 {
		f.Summary += " See the warnings."
	}
	return len(failing) == 0, f, nil
}

// late is a covered ledger's period whose depreciation is caught up by a
// later period's posted run, as its check read it.
type late struct {
	k LedgerPeriod
	d PeriodDepreciation
}

// ledgerYearDepreciation reads a covered ledger's depreciation of each
// regular period of the year into f, naming each period that fails, and
// returns their numbers. An asset late for a period is late for each
// period after it up to the later run's, its depreciation missing from
// that period through the run's (ADR-0057 §7), so the periods caught up by
// one later run warn of their assets once, as the last of them reads
// them, not once per period.
func (e *Endpoints) ledgerYearDepreciation(ctx context.Context, tx pgx.Tx, f *found, legalEntity uuid.UUID, year int16, l LedgerYear) ([]string, error) {
	var periods []string
	var read []late
	for _, p := range regularPeriods(l) {
		k := LedgerPeriod{LedgerID: l.LedgerID, Code: l.Code, Name: l.Name, Leading: l.Leading, Period: &p}
		got, d, err := e.ledgerDepreciation(ctx, tx, f, legalEntity, FiscalPeriod{year, p.Number}, k, false)
		if err != nil {
			return nil, err
		}
		read = append(read, late{k: k, d: d})
		if !got.missing() {
			continue
		}
		if f.FailingPeriods++; len(f.Ledgers) < maxNamed {
			got.Period = &p.Number
			f.Ledgers = append(f.Ledgers, got)
		}
		periods = append(periods, fmt.Sprint(p.Number))
	}
	for i, r := range read {
		last := i == len(read)-1 || read[i+1].laterID() != r.laterID()
		if r.laterID() != uuid.Nil && last {
			if err := f.warnLate(ctx, tx, r.k, r.d); err != nil {
				return nil, err
			}
		}
	}
	return periods, nil
}

// laterID is the later run that catches the period up, uuid.Nil when
// none does: it has a run of its own, nothing to depreciate or no later
// run.
func (r late) laterID() uuid.UUID {
	if r.d.Run != nil || r.d.Selected == 0 || r.d.Later == nil {
		return uuid.Nil
	}
	return r.d.Later.ID
}

// carryForwardCurrent passes when each covered ledger has a current
// balance carry-forward from the year that is not stale (ADR-0060 §6, §8),
// and names each ledger with its run, or without one, and the periods
// opened since that make it stale.
func (e *Endpoints) carryForwardCurrent(ctx context.Context, legalEntity uuid.UUID, year int16, covered []LedgerPeriod) (bool, found, error) {
	f := found{Ledgers: make([]foundLedger, len(covered))}
	var missing, stale []string
	for i, k := range covered {
		cf, err := e.Ledger.CarryForward(ctx, legalEntity, k.LedgerID, year)
		if err != nil {
			return false, found{}, fmt.Errorf("closing: carry-forward of ledger %s from %d: %w", k.Code, year, err)
		}
		l := foundLedger{LedgerID: k.LedgerID, LedgerCode: k.Code, LedgerName: k.Name, KeyDate: k.Period.EndDate.Format(time.DateOnly),
			CarryForward: cf.Run}
		if cf.Run == nil {
			missing = append(missing, k.Code)
			f.Ledgers[i] = l
			continue
		}
		isStale := len(cf.OpenedSince) > 0
		l.Stale, l.OpenedSince = &isStale, make([][2]int16, len(cf.OpenedSince))
		opened := make([]string, len(cf.OpenedSince))
		for j, p := range cf.OpenedSince {
			l.OpenedSince[j] = [2]int16{p.FiscalYear, p.Period}
			opened[j] = fmt.Sprintf("%d/%d", p.FiscalYear, p.Period)
		}
		if isStale {
			stale = append(stale, fmt.Sprintf("%s (%s opened since)", k.Code, strings.Join(opened, ", ")))
		}
		f.Ledgers[i] = l
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("No carry-forward from %d in ledger %s; carry %d forward.", year, strings.Join(missing, ", "), year))
	}
	if len(stale) > 0 {
		parts = append(parts, fmt.Sprintf("The carry-forward from %d is stale in ledger %s; close those periods and carry %d forward again.", year,
			strings.Join(stale, ", "), year))
	}
	f.Summary = fmt.Sprintf("Every covered ledger has a current carry-forward from %d that is not stale.", year)
	if len(parts) > 0 {
		f.Summary = strings.Join(parts, " ")
	}
	return len(parts) == 0, f, nil
}
