package assets

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// prepareCenterTransfer checks a center transfer (ADR-0057 §6, §8): valued
// on its posting period's first day (the default; 422
// assets.transfer_mid_period for another day of it,
// assets.value_date_outside_period for another period); nothing valued or
// booked in a later period (409 assets.later_value_date); its new centers
// derived and checked on that day (centersTo); then per ledger the
// catch-up through the period before on the previous centers, where it is
// posted (catchUpApart), its group amounts and, for a profit center
// change, the balances it moves.
func (e *Endpoints) prepareCenterTransfer(ctx context.Context, tx pgx.Tx, t *transferring) error {
	t.value = t.period.Start
	if t.in.ValueDate != nil {
		t.value = *t.in.ValueDate
	}
	if err := valuedInPeriod("transfer", t.value, t.posting, t.period); err != nil {
		return err
	}
	if !t.value.Equal(t.period.Start) {
		return invalid("assets.transfer_mid_period", fmt.Sprintf("A center transfer takes effect from the first day of its posting period, %s.",
			t.period.Start.Format(time.DateOnly)), "value_date", t.period.Start.Format(time.DateOnly))
	}
	if err := laterValueDate(ctx, tx, t.asset, t.period); err != nil {
		return err
	}
	if err := e.centersTo(ctx, t); err != nil {
		return err
	}
	ls, err := e.catchUps(ctx, tx, t.asset, t.company, t.period)
	if err != nil {
		return err
	}
	from, err := latestAssignment(ctx, tx, t.asset.ID)
	if err != nil {
		return err
	}
	for _, l := range ls {
		tl := &transferLedger{retiredLedger: l, catchUpOn: t.period.Start.AddDate(0, 0, -1)}
		if tl.apart, err = e.catchUpApart(ctx, t, tl, from); err != nil {
			return err
		}
		t.ledgers = append(t.ledgers, tl)
	}
	return e.centerGroupAmounts(ctx, t)
}

// centersTo derives and checks the new centers on the value date as the
// asset's at its creation are (assign, ADR-0057 §8): a new cost center,
// required for an asset with one, its profit center derived (422
// ledger.profit_center_mismatch for another given); or for an asset
// without one a profit center, or a cost center to take. Centers equal to
// the asset's, the derived profit center included, move nothing (422
// request.validation_failed); the same cost center whose profit center
// changed since is a profit center change.
func (e *Endpoints) centersTo(ctx context.Context, t *transferring) error {
	switch {
	case t.asset.CostCenterID != nil && t.in.CostCenterID == nil:
		return validation("cost_center_id", "the asset's new cost center: an asset with a cost center moves to another one")
	case t.in.CostCenterID == nil && t.in.ProfitCenterID == nil:
		return validation("profit_center_id", "the asset's new profit center, or a cost center")
	}
	pc, err := e.assign(ctx, t.company.ID, t.value, t.period.FiscalYear, t.classAccounts.Depreciation,
		NewAsset{CostCenterID: t.in.CostCenterID, ProfitCenterID: t.in.ProfitCenterID})
	if err != nil {
		return err
	}
	if sameID(t.in.CostCenterID, t.asset.CostCenterID) && sameID(pc, t.asset.ProfitCenterID) {
		return validation("cost_center_id", "centers other than the asset's")
	}
	t.costCenter, t.profit = t.in.CostCenterID, pc
	return nil
}

// sameID reports whether two optional ids are the same, both absent
// included.
func sameID(a, b *uuid.UUID) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// profitCenterChange reports whether a center transfer moves the asset to
// another profit center, which moves its balances (ADR-0057 §8).
func (t *transferring) profitCenterChange() bool {
	return t.in.Kind == TransferCenter && !sameID(t.profit, t.asset.ProfitCenterID)
}

// catchUpApart decides whether a ledger's catch-up is its own entry dated
// the last day of the period before (ADR-0057 §8): only when it posts
// something, the asset is not booked through the transfer's period there,
// the previous centers were in force by that day (their row's valid_from,
// from, on or before it) and that period is open in the ledger; otherwise
// it is posted in the transfer's main entry, on the previous centers.
func (e *Endpoints) catchUpApart(ctx context.Context, t *transferring, l *transferLedger, from time.Time) (bool, error) {
	booked := l.terms.BookedYear != nil && (postingPeriod{*l.terms.BookedYear, *l.terms.BookedPeriod}).atLeast(t.period)
	if l.catchUp == 0 || booked || from.After(l.catchUpOn) {
		return false, nil
	}
	err := e.Ledger.PeriodOpen(ctx, t.company.ID, l.ledger.ID, t.before.year, t.before.period)
	switch p := problem.From(err); {
	case err == nil:
		return true, nil
	case p.Code == "ledger.period_not_open" || p.Code == "ledger.no_fiscal_period":
		return false, nil
	}
	return false, fmt.Errorf("assets: whether %d/%d is open in ledger %s: %w", t.before.year, t.before.period, l.ledger.Code, err)
}

// atLeast reports whether the posting period is the period p or a later
// one.
func (pp postingPeriod) atLeast(p Period) bool {
	return pp.year > p.FiscalYear || (pp.year == p.FiscalYear && pp.period >= p.Number)
}

// centerGroupAmounts translates each ledger's catch-up at its posting
// date's rate, the period before's last day for one apart (ADR-0057 §7),
// and, for a profit center change, sets the balances moved: all the
// asset's booked cost and accumulated depreciation after the catch-up, at
// their local and group amounts (§8).
func (e *Endpoints) centerGroupAmounts(ctx context.Context, t *transferring) error {
	for _, apart := range []bool{true, false} {
		var at []*transferLedger
		var local []money.Minor
		for _, l := range t.ledgers {
			if l.apart == apart {
				at, local = append(at, l), append(local, l.catchUp)
			}
		}
		if len(at) == 0 {
			continue
		}
		on := t.posting
		if apart {
			on = at[0].catchUpOn
		}
		gc, err := e.Ledger.GroupAmounts(ctx, t.company.Currency, t.group, on, local)
		if err != nil {
			return fmt.Errorf("assets: group amounts of the transfer of asset %s: %w", t.asset.Number, err)
		}
		for i, l := range at {
			l.catchUpGC = gc[i]
		}
	}
	if !t.profitCenterChange() {
		return nil
	}
	for _, l := range t.ledgers {
		l.cost, l.costGC = l.values.Cost, l.values.CostGC
		l.accumulated, l.accumulatedGC = l.values.Accumulated-l.catchUp, l.values.AccumulatedGC-l.catchUpGC
	}
	return nil
}
