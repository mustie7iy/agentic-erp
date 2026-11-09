package assets

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A change in estimate (ADR-0057 §7, T57j; IAS 16.51, 16.61, IAS 8.36-38;
// the accounting specialist, po:2026-11-09, B13): once depreciation has
// been posted in a ledger, an asset's method, useful life, declining
// factor and residual value there change only from a regular period after
// the one it is booked through, so what was planned for the periods
// before, and booked, stays, and the runs from that period on depreciate
// the net book value then over the life left (the walk, Plan.Changes).

// EstimateFrom is the period a change in estimate applies from.
type EstimateFrom struct{ FiscalYear, Period int16 }

// termsKey is an asset's terms in one ledger.
type termsKey struct{ asset, ledger uuid.UUID }

// readChanges reads the assets' changes in estimate, by asset and ledger,
// oldest first.
func readChanges(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) (map[termsKey][]EstimateChange, error) {
	rows, _ := tx.Query(ctx, `SELECT asset_id, ledger_id, fiscal_year, fiscal_period, effective_from, method, useful_life_months,
			trim_scale(declining_factor)::text, residual_value, before_method, before_useful_life_months, trim_scale(before_declining_factor)::text,
			before_residual_value, created_at, created_by, seq
		FROM assets.estimate_change WHERE asset_id = ANY($1) ORDER BY asset_id, ledger_id, seq`, ids)
	out := map[termsKey][]EstimateChange{}
	var k termsKey
	var c EstimateChange
	_, err := pgx.ForEachRow(rows, []any{&k.asset, &k.ledger, &c.FiscalYear, &c.Period, &c.From, &c.Method, &c.UsefulLifeMonths, &c.DecliningFactor,
		&c.Residual, &c.Before.Method, &c.Before.UsefulLifeMonths, &c.Before.DecliningFactor, &c.Before.Residual, &c.CreatedAt, &c.CreatedBy, &c.seq}, func() error {
		out[k] = append(out[k], c)
		c = EstimateChange{} // the next row's pointers are scanned into a new value
		return nil
	})
	return out, errs.Wrapf(err, "assets: changes in estimate of %d assets", len(ids))
}

// withChanges reads the changes in estimate in the ledger into the
// assets whose terms there have some; none is read when none has.
func withChanges(ctx context.Context, tx pgx.Tx, ledger uuid.UUID, ds map[uuid.UUID]*depreciable) error {
	var ids []uuid.UUID
	for id, d := range ds {
		if d.terms.changes > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	changes, err := readChanges(ctx, tx, ids)
	for _, id := range ids {
		ds[id].terms.Changes = changes[termsKey{id, ledger}]
	}
	return err
}

// estimateField is a terms entry's field and the period it gives.
type estimateField struct {
	field string
	from  EstimateFrom
}

// estimatesOf are the patch's terms entries with effective_from, by
// ledger.
func estimatesOf(ch Patch) map[uuid.UUID]estimateField {
	out := map[uuid.UUID]estimateField{}
	for i, tp := range ch.Terms {
		if tp.EffectiveFrom != nil {
			out[tp.LedgerID] = estimateField{field: fmt.Sprintf("terms[%d].effective_from", i), from: *tp.EffectiveFrom}
		}
	}
	return out
}

// changedTerms are the terms that differ from the asset's: those changed
// directly, and those changed in estimate (an entry with effective_from).
// It refuses a direct change in a ledger depreciation has been posted in
// (409 assets.terms_in_use; the database holds it too,
// asset_terms_in_use), and effective_from in a ledger without posted
// depreciation or with a changed depreciation start (422
// request.validation_failed).
func changedTerms(old, terms []Terms, estimates map[uuid.UUID]estimateField) (direct, estimated []Terms, err error) {
	for _, t := range terms {
		i := slices.IndexFunc(old, func(o Terms) bool { return o.LedgerID == t.LedgerID })
		f, ok := estimates[t.LedgerID]
		if ok && (i < 0 || !old[i].booked()) {
			return nil, nil, validation(f.field, "only in a ledger depreciation has been posted in; before, its terms change without it")
		}
		if i >= 0 && sameTerms(old[i], t) {
			continue
		}
		switch {
		case ok && !sameTerms(old[i], Terms{Method: old[i].Method, UsefulLifeMonths: old[i].UsefulLifeMonths, DecliningFactor: old[i].DecliningFactor,
			Residual: old[i].Residual, DepreciationStart: t.DepreciationStart}):
			return nil, nil, validation(f.field, "a change in estimate keeps the depreciation start")
		case ok:
			estimated = append(estimated, t)
		case i >= 0 && old[i].booked():
			return nil, nil, termsInUse()
		default:
			direct = append(direct, t)
		}
	}
	return direct, estimated, nil
}

// estimateChanges are the changes in estimate the terms make, each from
// the regular period its entry gives (422 assets.special_period,
// ledger.no_fiscal_period): one after the period the ledger is booked
// through and not before the ledger's latest change's (422
// assets.change_not_prospective).
func (e *Endpoints) estimateChanges(ctx context.Context, a Asset, estimated []Terms, fields map[uuid.UUID]estimateField) ([]EstimateChange, error) {
	out := make([]EstimateChange, 0, len(estimated))
	for _, t := range estimated {
		f := fields[t.LedgerID]
		old, _ := termsIn(a, t.LedgerID) // changedTerms found them
		per, err := e.periodOf(ctx, a.LegalEntityID, f.from.FiscalYear, f.from.Period)
		if err != nil {
			return nil, err
		}
		if err := prospective(a.Number, old, per, f.field); err != nil {
			return nil, err
		}
		out = append(out, EstimateChange{ledger: t.LedgerID, FiscalYear: per.FiscalYear, Period: per.Number, From: per.Start,
			Estimate: Estimate{Method: t.Method, UsefulLifeMonths: t.UsefulLifeMonths, DecliningFactor: t.DecliningFactor, Residual: t.Residual},
			Before:   Estimate{Method: old.Method, UsefulLifeMonths: old.UsefulLifeMonths, DecliningFactor: old.DecliningFactor, Residual: old.Residual}})
	}
	return out, nil
}

// prospective refuses a change in estimate from a period on or before the
// one the terms are booked through, or before their latest change's (422
// assets.change_not_prospective; the database holds both,
// estimate_change_prospective).
func prospective(number string, t Terms, per Period, field string) error {
	if per.FiscalYear < *t.BookedYear || per.FiscalYear == *t.BookedYear && per.Number <= *t.BookedPeriod {
		return invalid("assets.change_not_prospective", fmt.Sprintf("Asset %s's depreciation is booked through %d/%02d in this ledger; a change in "+
			"estimate applies from a later period, so the depreciation booked stays.", number, *t.BookedYear, *t.BookedPeriod), field,
			fmt.Sprintf("a period after %d/%02d", *t.BookedYear, *t.BookedPeriod))
	}
	if n := len(t.Changes); n > 0 && per.Start.Before(t.Changes[n-1].From) {
		latest := t.Changes[n-1]
		return invalid("assets.change_not_prospective", fmt.Sprintf("Asset %s's latest change in estimate in this ledger applies from %d/%02d; a "+
			"later change applies from that period or after it.", number, latest.FiscalYear, latest.Period), field,
			fmt.Sprintf("%d/%02d or later", latest.FiscalYear, latest.Period))
	}
	return nil
}

// writeChanges records the changes in estimate under the asset's next
// seq (a change is a posting of the asset, ADR-0057 §5, so a document
// posted before it is no longer reversed) and sets each ledger's terms to
// its change's, counting it; with the asset's description and version.
func writeChanges(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, description string, changes []EstimateChange) error {
	var seq int32
	if err := tx.QueryRow(ctx, `UPDATE assets.asset SET description = $2, last_seq = last_seq + 1, version = version + 1, updated_by = $3
		WHERE id = $1 RETURNING last_seq`, id, description, p.ID).Scan(&seq); err != nil {
		return fmt.Errorf("assets: change asset %s: %w", id, err)
	}
	for _, c := range changes {
		if _, err := tx.Exec(ctx, `INSERT INTO assets.estimate_change (tenant_id, id, asset_id, ledger_id, seq, fiscal_year, fiscal_period,
				effective_from, method, useful_life_months, declining_factor, residual_value, before_method, before_useful_life_months,
				before_declining_factor, before_residual_value, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::numeric, $12, $13, $14, $15::numeric, $16, $17)`,
			p.TenantID, uuid.Must(uuid.NewV7()), id, c.ledger, seq, c.FiscalYear, c.Period, c.From, c.Method, c.UsefulLifeMonths, c.DecliningFactor,
			c.Residual, c.Before.Method, c.Before.UsefulLifeMonths, c.Before.DecliningFactor, c.Before.Residual, p.ID); err != nil {
			return fmt.Errorf("assets: change in estimate of asset %s in ledger %s: %w", id, c.ledger, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE assets.asset_terms SET method = $3, useful_life_months = $4, declining_factor = $5::numeric,
				residual_value = $6, estimate_changes = estimate_changes + 1, version = version + 1, updated_by = $7
			WHERE asset_id = $1 AND ledger_id = $2`, id, c.ledger, c.Method, c.UsefulLifeMonths, c.DecliningFactor, c.Residual, p.ID); err != nil {
			return fmt.Errorf("assets: terms of asset %s in ledger %s: %w", id, c.ledger, err)
		}
	}
	return nil
}

// heldForSaleChange is a ledger's classification as held for sale to
// write.
type heldForSaleChange struct {
	ledger uuid.UUID
	HeldForSale
}

// heldForSale are the patch's classifications as held for sale (ADR-0057
// §7, T57k; IAS 16.55, IFRS 5.25), each from the regular period its entry
// gives (422 assets.special_period, ledger.no_fiscal_period), one already
// made from the same period left out; terms are the patched terms, which
// hold every entry's ledger.
func (e *Endpoints) heldForSale(ctx context.Context, a Asset, ch Patch, terms []Terms) ([]heldForSaleChange, error) {
	var out []heldForSaleChange
	for i, tp := range ch.Terms {
		if tp.HeldForSaleFrom == nil {
			continue
		}
		field := fmt.Sprintf("terms[%d].held_for_sale_from", i)
		t, _ := termsIn(Asset{Terms: terms}, tp.LedgerID) // patchedTerms gave every entry's ledger terms
		per, err := e.periodOf(ctx, a.LegalEntityID, tp.HeldForSaleFrom.FiscalYear, tp.HeldForSaleFrom.Period)
		if err != nil {
			return nil, err
		}
		if t.HeldForSale != nil && t.HeldForSale.From.Equal(per.Start) {
			continue
		}
		if err := stopsProspectively(a.Number, t, per, field); err != nil {
			return nil, err
		}
		out = append(out, heldForSaleChange{ledger: t.LedgerID, HeldForSale: HeldForSale{FiscalYear: per.FiscalYear, Period: per.Number, From: per.Start}})
	}
	return out, nil
}

// stopsProspectively refuses a classification as held for sale from a
// period on or before the one the terms are booked through, and moving
// one whose period is booked (422 assets.change_not_prospective; the
// database holds both, asset_terms_held_for_sale_prospective): what was
// booked stays.
func stopsProspectively(number string, t Terms, per Period, field string) error {
	if !t.booked() {
		return nil
	}
	by, bp := *t.BookedYear, *t.BookedPeriod
	booked := func(year, period int16) bool { return year < by || year == by && period <= bp }
	if booked(per.FiscalYear, per.Number) {
		return invalid("assets.change_not_prospective", fmt.Sprintf("Asset %s's depreciation is booked through %d/%02d in this ledger; it is "+
			"classified as held for sale from a later period, so the depreciation booked stays.", number, by, bp), field,
			fmt.Sprintf("a period after %d/%02d", by, bp))
	}
	if h := t.HeldForSale; h != nil && booked(h.FiscalYear, h.Period) {
		return invalid("assets.change_not_prospective", fmt.Sprintf("Asset %s is held for sale from %d/%02d in this ledger, a period its "+
			"depreciation is booked through; the classification stays.", number, h.FiscalYear, h.Period), field, "no change")
	}
	return nil
}

// writeHeldForSale writes the classifications as held for sale.
func writeHeldForSale(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, held []heldForSaleChange) error {
	for _, h := range held {
		if _, err := tx.Exec(ctx, `UPDATE assets.asset_terms SET held_for_sale_year = $3, held_for_sale_period = $4, held_for_sale_from = $5,
				version = version + 1, updated_by = $6 WHERE asset_id = $1 AND ledger_id = $2`, id, h.ledger, h.FiscalYear, h.Period, h.From, p.ID); err != nil {
			return fmt.Errorf("assets: asset %s held for sale in ledger %s: %w", id, h.ledger, err)
		}
	}
	return nil
}
