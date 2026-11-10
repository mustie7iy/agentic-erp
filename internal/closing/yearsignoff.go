package closing

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// SignOffYearClose implements POST /v1/year-closes/{id}:signOff (ADR-0060
// §8), the period close's sign-off with the year's periods: under the
// close's lock, the year's periods of every covered ledger are read FOR
// UPDATE in the one order; every manual task done, every check passing
// when run again (its result then recorded), every regular period locked
// and every special period closed or locked; each closed special period
// is then locked through the ledger's period lock in the order read, and
// the sign-off is recorded with the signer's name, audited and published.
// A refusal keeps nothing: the request rolls back.
func (e *Endpoints) SignOffYearClose(ctx context.Context, req gen.OptPeriodCloseSignOffInput, params gen.SignOffYearCloseParams) (gen.SignOffYearCloseRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, periods, err := e.lockedYearForSignOff(ctx, tx, params.ID, params.IfMatch)
	if err != nil {
		return nil, err
	}
	tasks, err := tasksOf(ctx, tx, yearCloses, c.ID)
	if err != nil {
		return nil, err
	}
	if err := manualDone(tasks); err != nil {
		return nil, err
	}
	if err := e.checksPass(ctx, tx, yearCloses, p, tasks); err != nil {
		return nil, err
	}
	if err := yearPeriodsReady(c, periods); err != nil {
		return nil, err
	}
	special, err := e.lockSpecial(ctx, periods)
	if err != nil {
		return nil, err
	}
	v, err := e.view(ctx, []uuid.UUID{c.LegalEntityID}, nil, p.ID)
	if err != nil {
		return nil, err
	}
	name := v.people[p.ID].Name
	s := signOff{ID: uuid.Must(uuid.NewV7()), CloseID: c.ID, SignedBy: p.ID, SignedByName: &name, Note: apitypes.Ptr[string](req.Value.Note),
		Special: special}
	if c, s, err = signedOff(ctx, tx, yearCloses, p, s); err != nil {
		return nil, err
	}
	if err := recordYearSignOff(ctx, c, s); err != nil {
		return nil, err
	}
	return &gen.YearCloseSignOffHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: v.yearSignOffToAPI(c, s)}, nil
}

// lockedYearForSignOff locks the year close, checks If-Match against its
// version, reads it, and reads its covered ledgers' periods of the year
// FOR UPDATE through the ledger (by period, the leading ledger first, then
// by code), so no period is opened or unlocked before the sign-off
// commits; a year close signed off whose periods are all locked is
// refused: nothing is left to sign off.
func (e *Endpoints) lockedYearForSignOff(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch string) (closeRow, []LedgerPeriod, error) {
	version, err := lockClose(ctx, tx, yearCloses, id)
	if err != nil {
		return closeRow{}, nil, err
	}
	if err := etag.IfMatch(ifMatch)(version); err != nil {
		return closeRow{}, nil, err
	}
	c, err := readClose(ctx, tx, yearCloses, id)
	if err != nil {
		return closeRow{}, nil, err
	}
	covered, err := coveredIDs(ctx, tx, yearCloses, c.ID)
	if err != nil {
		return closeRow{}, nil, err
	}
	periods, err := e.Ledger.LockYear(ctx, c.LegalEntityID, c.FiscalYear, covered)
	if err != nil {
		return closeRow{}, nil, fmt.Errorf("closing: lock the periods of year close %s: %w", c.ID, err)
	}
	if c.Status == CloseSignedOff && !slices.ContainsFunc(periods, func(k LedgerPeriod) bool { return k.Period.Status != periodLocked }) {
		return closeRow{}, nil, signedOffConflict(yearCloses, c)
	}
	return c, periods, nil
}

// yearPeriodsReady refuses a year's sign-off while a regular period of a
// covered ledger is not locked (closing.year_periods_not_locked, naming
// each ledger with its periods: the period closes' sign-offs lock them)
// or a special period is open (closing.period_not_closed, naming each
// period with its ledgers): a sign-off locks only the special periods, and
// never closes a period itself.
func yearPeriodsReady(c closeRow, periods []LedgerPeriod) error {
	var ledgers []string
	unlocked := map[string][]string{}
	var specials []int16
	open := map[int16][]string{}
	for _, k := range periods {
		switch q := k.Period; {
		case !q.Special && q.Status != periodLocked:
			if _, ok := unlocked[k.Code]; !ok {
				ledgers = append(ledgers, k.Code)
			}
			unlocked[k.Code] = append(unlocked[k.Code], fmt.Sprint(q.Number))
		case q.Special && q.Status == periodOpen:
			if _, ok := open[q.Number]; !ok {
				specials = append(specials, q.Number)
			}
			open[q.Number] = append(open[q.Number], k.Code)
		}
	}
	if len(ledgers) > 0 {
		var named []string
		for _, l := range ledgers {
			noun := "periods "
			if len(unlocked[l]) == 1 {
				noun = "period "
			}
			named = append(named, noun+strings.Join(unlocked[l], ", ")+" in ledger "+l)
		}
		return problem.Conflict("closing.year_periods_not_locked", fmt.Sprintf("Every regular period of %d must be locked before the year is signed off; "+
			"not locked: %s. The period closes' sign-offs lock them (signOffPeriodClose), as does lockFiscalPeriod.", c.FiscalYear, strings.Join(named, "; ")))
	}
	if len(specials) > 0 {
		var named []string
		for _, n := range specials {
			named = append(named, fmt.Sprintf("%d/%d in ledger %s", c.FiscalYear, n, strings.Join(open[n], ", ")))
		}
		return problem.Conflict("closing.period_not_closed", "Special period "+strings.Join(named, "; ")+" is open; close it (closeFiscalPeriod) "+
			"before signing off, so that no posting lands in it after the sign-off.")
	}
	return nil
}

// lockSpecial locks each closed special period through the ledger's
// period lock, at the version read under the sign-off's lock, in the order
// read (by period, the leading ledger first, then by code); a special
// period locked already is left as it is. It returns each covered ledger,
// in the order of its first period, with its special periods and whether
// this sign-off locked each.
func (e *Endpoints) lockSpecial(ctx context.Context, periods []LedgerPeriod) ([]specialLedger, error) {
	var out []specialLedger
	at := map[uuid.UUID]int{}
	for _, k := range periods {
		i, ok := at[k.LedgerID]
		if !ok {
			i, at[k.LedgerID] = len(out), len(out)
			out = append(out, specialLedger{LedgerID: k.LedgerID, LedgerCode: k.Code, LedgerName: k.Name, Leading: k.Leading, Periods: []specialPeriod{}})
		}
		q := k.Period
		if !q.Special {
			continue // locked: yearPeriodsReady refused any other
		}
		locked := q.Status == periodClosed // open: yearPeriodsReady refused it
		if locked {
			if err := e.Ledger.Lock(ctx, q.ID, q.Version); err != nil {
				return nil, fmt.Errorf("closing: lock period %d (%s) of ledger %s: %w", q.Number, q.ID, k.Code, err)
			}
		}
		out[i].Periods = append(out[i].Periods, specialPeriod{PeriodID: q.ID, Number: q.Number, Locked: locked})
	}
	return out, nil
}

// recordYearSignOff audits a year close's sign-off and publishes it, as a
// period close's (recordSignOff), with the fiscal year in place of the
// period and each covered ledger's special periods.
func recordYearSignOff(ctx context.Context, c closeRow, s signOff) error {
	periods := []map[string]any{} // a list, never null, in the event
	for _, l := range s.Special {
		for _, q := range l.Periods {
			periods = append(periods, map[string]any{"ledger_id": l.LedgerID, "ledger_code": l.LedgerCode, "fiscal_period_id": q.PeriodID,
				"period": q.Number, "locked": q.Locked})
		}
	}
	return audit.Record(ctx, "closing.year_close.sign_off", "closing.year_close", c.ID,
		map[string]any{"legal_entity_id": c.LegalEntityID, "fiscal_year": c.FiscalYear, "sign_off_id": s.ID, "note": s.Note, "periods": periods,
			"version": c.Version},
		outbox.Event{Context: "closing", Name: "year_close_signed_off", AggregateType: "closing.year_close", AggregateID: c.ID,
			Payload: map[string]any{"year_close_id": c.ID, "sign_off_id": s.ID, "legal_entity_id": c.LegalEntityID, "fiscal_year": c.FiscalYear,
				"signed_by": s.SignedBy, "note": s.Note, "periods": periods, "status": c.Status, "version": c.Version}})
}
