package assets

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// reverseTransfer reverses a posted transfer of either kind (ADR-0057 §8):
// under its assets' locks and the transfer's, checked, each ledger's
// entries reversed through the ledger's reversal path (reverseTransferEntries)
// in ledger order, its movements negated with each asset's next seq,
// booked_through returned in each ledger to its value before the catch-up,
// a center transfer's previous centers restored with their valid_from, the
// assets' status derived again, audited and published as
// AssetMovementReversed.
func (e *Endpoints) reverseTransfer(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, ifMatch func(int32) error, date *time.Time,
	reason string) (Transfer, error) {
	assets, q, err := lockTransfer(ctx, tx, id)
	if err != nil {
		return Transfer{}, err
	}
	if err := ifMatch(q.Version); err != nil {
		return Transfer{}, err
	}
	on := q.PostingDate
	if date != nil {
		on = *date
	}
	if err := transferReversible(assets, q, on); err != nil {
		return Transfer{}, err
	}
	reversals, per, err := e.reverseTransferEntries(ctx, q, on)
	if err != nil {
		return Transfer{}, err
	}
	if err := writeTransferReversal(ctx, tx, p, q, assets, on, per, reason, reversals); err != nil {
		return Transfer{}, err
	}
	out, err := readTransfer(ctx, tx, id)
	if err != nil {
		return Transfer{}, err
	}
	return out, recordTransferReversal(ctx, tx, out, assets[q.AssetID].Status)
}

// lockTransfer reads the transfer (404 for one of a company the caller
// may not act in), then its asset and an asset transfer's receiver FOR
// UPDATE in id order, then the transfer again FOR UPDATE: the order a
// posting takes them (the assets, then the document, then the ledger's
// number ranges, ADR-0008).
func lockTransfer(ctx context.Context, tx pgx.Tx, id uuid.UUID) (map[uuid.UUID]Asset, Transfer, error) {
	q, err := readTransfer(ctx, tx, id)
	if err != nil {
		return nil, Transfer{}, err
	}
	ids := []uuid.UUID{q.AssetID}
	if q.ToAssetID != nil {
		ids = append(ids, *q.ToAssetID)
	}
	assets := map[uuid.UUID]Asset{}
	for _, a := range sortedIDs(ids) {
		if assets[a], err = readAsset(ctx, tx, a, true); err != nil {
			return nil, Transfer{}, err
		}
	}
	locked, err := scanTransfer(tx.QueryRow(ctx, transferSelect+` WHERE q.id = $1 FOR UPDATE OF q`, id))
	if err != nil {
		return nil, Transfer{}, fmt.Errorf("assets: lock transfer %s: %w", id, err)
	}
	locked.Ledgers, locked.Entries = q.Ledgers, q.Entries // what it posted never changes (transfer_fixed)
	return assets, locked, nil
}

// transferReversible refuses a reversal (ADR-0057 §8, decided by default
// 20): of a transfer not posted (409 assets.transfer_not_posted); dated
// before it (422 assets.reversal_before_original); and once anything was
// posted after it on the asset or an asset transfer's receiver, a
// document, reversal, assignment or run batch, which took a higher seq
// (409 assets.later_movements). The two keep the reversal on or after
// both assets' latest assignments with no check of their own.
func transferReversible(assets map[uuid.UUID]Asset, q Transfer, on time.Time) error {
	asset := assets[q.AssetID]
	if q.Status != TransferPosted {
		return problem.Conflict("assets.transfer_not_posted", fmt.Sprintf("The transfer of asset %s is %s; only a posted one is reversed.",
			asset.Number, q.Status))
	}
	if on.Before(q.PostingDate) {
		return invalid("assets.reversal_before_original", fmt.Sprintf("The reversal's posting date %s is before the transfer's, %s.",
			on.Format(time.DateOnly), q.PostingDate.Format(time.DateOnly)), "posting_date", "on or after "+q.PostingDate.Format(time.DateOnly))
	}
	later := asset.LastSeq != q.Seq
	if q.ToAssetID != nil {
		later = later || assets[*q.ToAssetID].LastSeq != *q.ToSeq
	}
	if later {
		return problem.Conflict("assets.later_movements", fmt.Sprintf("Something was posted on asset %s, or on the asset it was transferred to, after "+
			"this transfer (a depreciation run, a document or a reversal); only the latest posting of the assets is reversed.", asset.Number))
	}
	return nil
}

// reverseTransferEntries reverses each ledger's entries of the transfer
// in ledger order and returns the reversal entries by the entry they
// reverse, with the regular period of the date (for movements without an
// entry): the main entry on the date, a center transfer's catch-up entry
// on its own date while its period is still open in that ledger, else on
// the date too, on the same previous centers (ADR-0057 §8).
func (e *Endpoints) reverseTransferEntries(ctx context.Context, q Transfer, on time.Time) (map[uuid.UUID]reversedEntry, Period, error) {
	per, err := e.Ledger.Period(ctx, q.LegalEntityID, on)
	if err != nil {
		return nil, Period{}, fmt.Errorf("assets: period of %s: %w", on.Format(time.DateOnly), err)
	}
	ledgers, err := e.Ledger.Ledgers(ctx, nil)
	if err != nil {
		return nil, Period{}, fmt.Errorf("assets: ledgers: %w", err)
	}
	out := map[uuid.UUID]reversedEntry{}
	for _, l := range ledgers {
		for _, en := range q.Entries {
			if en.LedgerID != l.ID {
				continue
			}
			date, err := e.reversalDate(ctx, q, en, on)
			if err != nil {
				return nil, Period{}, err
			}
			posted, err := e.Ledger.Reverse(ctx, en.EntryID, date)
			if err != nil {
				return nil, Period{}, fmt.Errorf("assets: reverse the transfer of asset %s in ledger %s: %w", q.AssetNumber, l.Code, err)
			}
			out[en.EntryID] = reversedEntry{PostedEntry: posted[0], on: date} // a ledger-specific entry's reversal is one entry
		}
	}
	return out, per, nil
}

// reversedEntry is the reversal of one of a transfer's entries, posted on
// a date.
type reversedEntry struct {
	PostedEntry
	on time.Time
}

// reversalDate is the date an entry of the transfer is reversed on: a
// catch-up entry's own while its period is open in its ledger, else the
// reversal's date.
func (e *Endpoints) reversalDate(ctx context.Context, q Transfer, en DocumentEntry, on time.Time) (time.Time, error) {
	if en.Part != partCatchUp {
		return on, nil
	}
	for _, v := range q.Ledgers {
		if v.LedgerID != en.LedgerID {
			continue
		}
		err := e.Ledger.PeriodOpen(ctx, q.LegalEntityID, en.LedgerID, v.CatchUpIn.year, v.CatchUpIn.period)
		if err == nil {
			return v.CatchUpOn, nil
		}
		if p := problem.From(err); p.Code != "ledger.period_not_open" {
			return time.Time{}, fmt.Errorf("assets: whether %d/%d is open: %w", v.CatchUpIn.year, v.CatchUpIn.period, err)
		}
	}
	return on, nil
}

// writeTransferReversal writes the transfer reversed, its movements
// negated each on the reversal of its entry with its asset's next seq,
// each entry's reversal, the asset's booked_through in each ledger
// returned to what the catch-up's movement recorded before it, the
// assets' status derived again and, for a center transfer, a row
// restoring the previous centers with that row's valid_from.
func writeTransferReversal(ctx context.Context, tx pgx.Tx, p reqctx.Principal, q Transfer, assets map[uuid.UUID]Asset, on time.Time, per Period,
	reason string, reversals map[uuid.UUID]reversedEntry) error {
	// Guarded on the status, which this transaction read FOR UPDATE, so
	// the row always matches; transfer_transition holds it too.
	if _, err := tx.Exec(ctx, `UPDATE assets.transfer SET status = 'reversed', reversal_posting_date = $2, reversal_reason = $3, version = version + 1,
		updated_by = $4 WHERE id = $1 AND status = 'posted'`, q.ID, on, reason, p.ID); err != nil {
		return fmt.Errorf("assets: reverse transfer %s: %w", q.ID, err)
	}
	seqs := map[uuid.UUID]int32{}
	for id, a := range assets {
		seqs[id] = a.LastSeq + 1
	}
	if err := negateTransferMovements(ctx, tx, p, q, seqs, on, per, reversals); err != nil {
		return err
	}
	for _, en := range q.Entries {
		r := reversals[en.EntryID] // every entry was reversed
		if _, err := tx.Exec(ctx, `UPDATE assets.document_entry SET reversal_entry_year = $4, reversal_entry_id = $5, reversal_document_number = $6
			WHERE document_kind = 'transfer' AND document_id = $1 AND ledger_id = $2 AND part = $3`, q.ID, en.LedgerID, en.Part, r.FiscalYear, r.ID,
			r.DocumentNumber); err != nil {
			return fmt.Errorf("assets: reversal of transfer %s in ledger %s: %w", q.ID, en.LedgerID, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE assets.asset_terms t SET booked_through_year = m.booked_through_before_year,
			booked_through_period = m.booked_through_before_period, version = t.version + 1, updated_by = $2
		FROM assets.movement m WHERE m.tenant_id = t.tenant_id AND m.asset_id = t.asset_id AND m.ledger_id = t.ledger_id AND m.document_kind = 'transfer'
			AND m.document_id = $1 AND m.kind = 'depreciation' AND m.reverses_id IS NULL`, q.ID, p.ID); err != nil {
		return fmt.Errorf("assets: booked_through before transfer %s: %w", q.ID, err)
	}
	ids := sortedIDs(slices.Collect(maps.Keys(seqs)))
	for _, id := range ids {
		if err := derive(ctx, tx, p, id, seqs[id]); err != nil {
			return err
		}
	}
	if err := keepTotals(ctx, tx, ids...); err != nil {
		return err
	}
	if q.Kind != TransferCenter {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO assets.asset_assignment (tenant_id, asset_id, seq, valid_from, cost_center_id, profit_center_id, transfer_id,
			restores_seq, created_by)
		SELECT tenant_id, asset_id, $3, valid_from, cost_center_id, profit_center_id, $4, seq, $5 FROM assets.asset_assignment
		WHERE asset_id = $1 AND seq < $2 ORDER BY seq DESC LIMIT 1`, q.AssetID, q.Seq, seqs[q.AssetID], q.ID, p.ID)
	return errs.Wrapf(err, "assets: restore the centers of asset %s", q.AssetNumber)
}

// negateTransferMovements writes, for each of the transfer's movements, a
// row negating it with its asset's seq, on the reversal of its entry (in
// that entry's period), or on the reversal's date for one without.
func negateTransferMovements(ctx context.Context, tx pgx.Tx, p reqctx.Principal, q Transfer, seqs map[uuid.UUID]int32, on time.Time, per Period,
	reversals map[uuid.UUID]reversedEntry) error {
	type original struct {
		id, asset uuid.UUID
		entry     *uuid.UUID
	}
	rows, _ := tx.Query(ctx, `SELECT id, asset_id, journal_entry_id FROM assets.movement WHERE document_kind = 'transfer' AND document_id = $1
		AND reverses_id IS NULL ORDER BY ledger_id, asset_id, kind`, q.ID)
	ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (original, error) {
		var m original
		return m, r.Scan(&m.id, &m.asset, &m.entry)
	})
	if err != nil {
		return fmt.Errorf("assets: movements of transfer %s: %w", q.ID, err)
	}
	for _, m := range ms {
		posting, year, period := on, per.FiscalYear, per.Number
		var entryYear *int16
		var entry *uuid.UUID
		if m.entry != nil {
			r := reversals[*m.entry] // a movement's entry is the transfer's, which was reversed
			posting, year, period, entryYear, entry = r.on, r.FiscalYear, r.Period, &r.FiscalYear, &r.ID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date,
				posting_date, fiscal_year, fiscal_period, cost, cost_gc, accumulated, accumulated_gc, residual, proceeds, share, document_kind, document_id,
				journal_entry_year, journal_entry_id, reverses_id, created_by)
			SELECT tenant_id, $2, asset_id, $3, legal_entity_id, ledger_id, class_code, kind, value_date, $4, $5, $6, -cost, -cost_gc, -accumulated,
				-accumulated_gc, -residual, -proceeds, share, document_kind, document_id, $7, $8, id, $9
			FROM assets.movement WHERE id = $1`, m.id, uuid.Must(uuid.NewV7()), seqs[m.asset], posting, year, period, entryYear, entry, p.ID); err != nil {
			return fmt.Errorf("assets: reverse movement %s of transfer %s: %w", m.id, q.ID, err)
		}
	}
	return nil
}
