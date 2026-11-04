package assets

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// reverseRetirement reverses a posted retirement (ADR-0057 §8): under the
// asset's lock and the retirement's, checked, each ledger's entry
// reversed through the ledger's reversal path on the date (the
// retirement's posting date by default, never before it), the leading
// ledger's first and then by code, its movements negated with the asset's
// next seq, booked_through returned in each ledger to its value before the
// catch-up, the asset's status derived again, audited and published as
// AssetMovementReversed.
func (e *Endpoints) reverseRetirement(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, ifMatch func(int32) error, date *time.Time,
	reason string) (Retirement, error) {
	asset, q, err := lockRetirement(ctx, tx, id)
	if err != nil {
		return Retirement{}, err
	}
	if err := ifMatch(q.Version); err != nil {
		return Retirement{}, err
	}
	on := q.PostingDate
	if date != nil {
		on = *date
	}
	if err := retirementReversible(asset, q, on); err != nil {
		return Retirement{}, err
	}
	reversals, per, err := e.reverseRetirementEntries(ctx, q, on)
	if err != nil {
		return Retirement{}, err
	}
	if err := writeRetirementReversal(ctx, tx, p, q, asset.LastSeq+1, on, per, reason, reversals); err != nil {
		return Retirement{}, err
	}
	out, err := readRetirement(ctx, tx, id)
	if err != nil {
		return Retirement{}, err
	}
	return out, recordRetirementReversal(ctx, tx, out, asset.Status)
}

// lockRetirement reads the retirement (404 for one of a company the
// caller may not act in), then its asset and the retirement again FOR
// UPDATE, in that order: the order a posting takes them (the asset, then
// its document, then the ledger's number range, ADR-0008).
func lockRetirement(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Asset, Retirement, error) {
	q, err := readRetirement(ctx, tx, id)
	if err != nil {
		return Asset{}, Retirement{}, err
	}
	asset, err := readAsset(ctx, tx, q.AssetID, true)
	if err != nil {
		return Asset{}, Retirement{}, err
	}
	locked, err := scanRetirement(tx.QueryRow(ctx, retirementSelect+` WHERE q.id = $1 FOR UPDATE OF q`, id))
	if err != nil {
		return Asset{}, Retirement{}, fmt.Errorf("assets: lock retirement %s: %w", id, err)
	}
	locked.Ledgers, locked.Entries = q.Ledgers, q.Entries // what it posted never changes (retirement_fixed)
	return asset, locked, nil
}

// retirementReversible refuses a reversal (ADR-0057 §8, decided by
// default 20): of a retirement not posted (409
// assets.retirement_not_posted); dated before it (422
// assets.reversal_before_original); and once anything was posted on the
// asset after it, a document, reversal, assignment or run batch, which
// took a higher seq (409 assets.later_movements). The two keep the
// reversal on or after the asset's latest assignment with no check of
// their own.
func retirementReversible(asset Asset, q Retirement, on time.Time) error {
	if q.Status != RetirementPosted {
		return problem.Conflict("assets.retirement_not_posted", fmt.Sprintf("The retirement of asset %s is %s; only a posted one is reversed.",
			asset.Number, q.Status))
	}
	if on.Before(q.PostingDate) {
		return invalid("assets.reversal_before_original", fmt.Sprintf("The reversal's posting date %s is before the retirement's, %s.",
			on.Format(time.DateOnly), q.PostingDate.Format(time.DateOnly)), "posting_date", "on or after "+q.PostingDate.Format(time.DateOnly))
	}
	if asset.LastSeq != q.Seq {
		return problem.Conflict("assets.later_movements", fmt.Sprintf("Something was posted on asset %s after this retirement (a depreciation run, a "+
			"document or a reversal); only the asset's latest posting is reversed.", asset.Number))
	}
	return nil
}

// reverseRetirementEntries reverses each ledger's entry of the retirement
// on the date, in ledger order, and returns the reversal entries by
// ledger with the regular period of the date (for a ledger the retirement
// posted nothing in).
func (e *Endpoints) reverseRetirementEntries(ctx context.Context, q Retirement, on time.Time) (map[uuid.UUID]PostedEntry, Period, error) {
	per, err := e.Ledger.Period(ctx, q.LegalEntityID, on)
	if err != nil {
		return nil, Period{}, fmt.Errorf("assets: period of %s: %w", on.Format(time.DateOnly), err)
	}
	ledgers, err := e.Ledger.Ledgers(ctx, nil)
	if err != nil {
		return nil, Period{}, fmt.Errorf("assets: ledgers: %w", err)
	}
	out := map[uuid.UUID]PostedEntry{}
	for _, l := range ledgers {
		en, ok := entryOf(q.Entries, RetiredValues{LedgerID: l.ID})
		if !ok {
			continue
		}
		posted, err := e.Ledger.Reverse(ctx, en.EntryID, on)
		if err != nil {
			return nil, Period{}, fmt.Errorf("assets: reverse the retirement of asset %s in ledger %s: %w", q.AssetNumber, l.Code, err)
		}
		out[l.ID] = posted[0] // a ledger-specific entry's reversal is one entry
	}
	return out, per, nil
}

// writeRetirementReversal writes the retirement reversed, its movements
// negated with the reversal's seq and entries, each entry's reversal, the
// asset's booked_through in each ledger returned to what the catch-up's
// movement recorded before it, and the asset's status derived again.
func writeRetirementReversal(ctx context.Context, tx pgx.Tx, p reqctx.Principal, q Retirement, seq int32, on time.Time, per Period, reason string,
	reversals map[uuid.UUID]PostedEntry) error {
	// Guarded on the status, which this transaction read FOR UPDATE, so
	// the row always matches; retirement_transition holds it too.
	if _, err := tx.Exec(ctx, `UPDATE assets.retirement SET status = 'reversed', reversal_posting_date = $2, reversal_reason = $3, version = version + 1,
		updated_by = $4 WHERE id = $1 AND status = 'posted'`, q.ID, on, reason, p.ID); err != nil {
		return fmt.Errorf("assets: reverse retirement %s: %w", q.ID, err)
	}
	for _, v := range q.Ledgers {
		if err := reverseRetiredLedger(ctx, tx, p, q, seq, on, per, v.LedgerID, reversals); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE assets.asset_terms t SET booked_through_year = m.booked_through_before_year,
			booked_through_period = m.booked_through_before_period, version = t.version + 1, updated_by = $2
		FROM assets.movement m WHERE m.tenant_id = t.tenant_id AND m.asset_id = t.asset_id AND m.ledger_id = t.ledger_id AND m.document_kind = 'retirement'
			AND m.document_id = $1 AND m.kind = 'depreciation' AND m.reverses_id IS NULL`, q.ID, p.ID); err != nil {
		return fmt.Errorf("assets: booked_through before retirement %s: %w", q.ID, err)
	}
	if err := derive(ctx, tx, p, q.AssetID, seq); err != nil {
		return err
	}
	return keepTotals(ctx, tx, q.AssetID)
}

// reverseRetiredLedger negates the retirement's movements in the ledger
// on its reversal entry (none when the retirement posted nothing there)
// and records the entry's reversal.
func reverseRetiredLedger(ctx context.Context, tx pgx.Tx, p reqctx.Principal, q Retirement, seq int32, on time.Time, per Period, ledger uuid.UUID,
	reversals map[uuid.UUID]PostedEntry) error {
	year, period := per.FiscalYear, per.Number
	var entryYear *int16
	var entry *uuid.UUID
	r, ok := reversals[ledger]
	if ok {
		year, period, entryYear, entry = r.FiscalYear, r.Period, &r.FiscalYear, &r.ID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date,
			posting_date, fiscal_year, fiscal_period, cost, cost_gc, accumulated, accumulated_gc, residual, proceeds, share, document_kind, document_id,
			journal_entry_year, journal_entry_id, reverses_id, created_by)
		SELECT tenant_id, CASE kind WHEN 'depreciation' THEN $3::uuid ELSE $4::uuid END, asset_id, $5, legal_entity_id, ledger_id, class_code, kind,
			value_date, $6, $7, $8, -cost, -cost_gc, -accumulated, -accumulated_gc, -residual, -proceeds, share, document_kind, document_id, $9, $10, id, $11
		FROM assets.movement WHERE document_kind = 'retirement' AND document_id = $1 AND ledger_id = $2 AND reverses_id IS NULL`,
		q.ID, ledger, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), seq, on, year, period, entryYear, entry, p.ID); err != nil {
		return fmt.Errorf("assets: reverse the movements of retirement %s in ledger %s: %w", q.ID, ledger, err)
	}
	if !ok {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE assets.document_entry SET reversal_entry_year = $3, reversal_entry_id = $4, reversal_document_number = $5
		WHERE document_kind = 'retirement' AND document_id = $1 AND ledger_id = $2 AND part = 'main'`, q.ID, ledger, r.FiscalYear, r.ID, r.DocumentNumber)
	return errs.Wrapf(err, "assets: reversal of retirement %s in ledger %s", q.ID, ledger)
}
