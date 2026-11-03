package assets

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// reverseAcquisition reverses a posted acquisition (ADR-0057 §6): under
// the asset's lock and the acquisition's, checked, its entries reversed
// in every ledger through the ledger's reversal path on the date (the
// acquisition's posting date by default, never before it), its movements
// negated with the asset's next seq, the amount given back to its line,
// the asset's status and capitalisation date derived again, audited and
// published as AssetMovementReversed.
func (e *Endpoints) reverseAcquisition(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, date *time.Time, reason string) (Acquisition, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Acquisition{}, err
	}
	asset, q, err := lockAcquisition(ctx, tx, id)
	if err != nil {
		return Acquisition{}, err
	}
	if err := ifMatch(q.Version); err != nil {
		return Acquisition{}, err
	}
	on := q.PostingDate
	if date != nil {
		on = *date
	}
	if err := e.reversible(ctx, tx, asset, q, on); err != nil {
		return Acquisition{}, err
	}
	reversals, err := e.reverseEntries(ctx, q, on)
	if err != nil {
		return Acquisition{}, err
	}
	seq := asset.LastSeq + 1
	if err := writeReversal(ctx, tx, p, q, seq, on, reason, reversals); err != nil {
		return Acquisition{}, err
	}
	out, err := readAcquisition(ctx, tx, id)
	if err != nil {
		return Acquisition{}, err
	}
	return out, recordReversal(ctx, tx, out)
}

// lockAcquisition reads the acquisition (404 for one of a company the
// caller may not act in), then its asset and the acquisition again FOR
// UPDATE, in that order, as every posting on the asset locks it first.
func lockAcquisition(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Asset, Acquisition, error) {
	q, err := readAcquisition(ctx, tx, id)
	if err != nil {
		return Asset{}, Acquisition{}, err
	}
	asset, err := readAsset(ctx, tx, q.AssetID, true)
	if err != nil {
		return Asset{}, Acquisition{}, err
	}
	q, err = scanAcquisition(tx.QueryRow(ctx, acquisitionSelect+` WHERE q.id = $1 FOR UPDATE OF q`, id))
	if err != nil {
		return Asset{}, Acquisition{}, fmt.Errorf("assets: lock acquisition %s: %w", id, err)
	}
	entries, err := readEntries(ctx, tx, "acquisition", []uuid.UUID{id})
	q.Entries = entries[id]
	return asset, q, err
}

// reversible refuses a reversal (ADR-0057 §6): of an acquisition not
// posted (409 assets.acquisition_not_posted); dated before it (422
// assets.reversal_before_original); while a retirement or transfer posted
// on the asset after it stands (409 assets.later_movements); of an
// acquisition from a line while one from that line's reversal's line
// stands on the asset (409 assets.reversal_line_capitalised); and where
// it would leave the asset's cost below zero in a ledger (422
// assets.cost_negative). The floor and the later movements keep the
// reversal on or after the asset's latest assignment, with no check of
// its own.
func (e *Endpoints) reversible(ctx context.Context, tx pgx.Tx, asset Asset, q Acquisition, on time.Time) error {
	if q.Status != AcquisitionPosted {
		return problem.Conflict("assets.acquisition_not_posted", fmt.Sprintf("The acquisition onto asset %s is %s; only a posted one is reversed.",
			asset.Number, q.Status))
	}
	if on.Before(q.PostingDate) {
		return invalid("assets.reversal_before_original", fmt.Sprintf("The reversal's posting date %s is before the acquisition's, %s.",
			on.Format(time.DateOnly), q.PostingDate.Format(time.DateOnly)), "posting_date", "on or after "+q.PostingDate.Format(time.DateOnly))
	}
	if err := laterMovements(ctx, tx, asset, q.Seq); err != nil {
		return err
	}
	if err := e.reversalLineCapitalised(ctx, tx, asset, q); err != nil {
		return err
	}
	return costStaysAfterReversal(ctx, tx, asset, q)
}

// laterMovements refuses while a retirement or transfer of any kind
// posted on the asset after seq is not reversed (409
// assets.later_movements): a retirement's or transfer's share was taken
// of the values this posting set, and a center transfer moved its cost to
// another profit center. Each retirement and transfer writes movements on
// every asset it touches, a center transfer its catch-up's, so its
// movements show it.
func laterMovements(ctx context.Context, tx pgx.Tx, asset Asset, seq int32) error {
	var later bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM assets.movement m WHERE m.asset_id = $1 AND m.seq > $2
			AND m.document_kind IN ('retirement', 'transfer') AND m.reverses_id IS NULL
			AND NOT EXISTS (SELECT FROM assets.movement r WHERE r.tenant_id = m.tenant_id AND r.reverses_id = m.id))`, asset.ID, seq).Scan(&later)
	if err != nil {
		return fmt.Errorf("assets: later movements of asset %s: %w", asset.Number, err)
	}
	if later {
		return problem.Conflict("assets.later_movements", fmt.Sprintf("A retirement or transfer was posted on asset %s after this acquisition; it is "+
			"reversed first.", asset.Number))
	}
	return nil
}

// reversalLineCapitalised refuses reversing an acquisition from a line
// whose reversal's line an acquisition onto the same asset still takes
// from (409 assets.reversal_line_capitalised, decided by default 23): the
// reversal's take-off is bounded by what the asset holds from the line.
func (e *Endpoints) reversalLineCapitalised(ctx context.Context, tx pgx.Tx, asset Asset, q Acquisition) error {
	if q.Source == nil {
		return nil
	}
	l, _, err := e.Ledger.Line(ctx, q.Source.EntryID, q.Source.LineNo) // a capitalised line is posted and never removed
	if err != nil {
		return fmt.Errorf("assets: line %d of entry %s: %w", q.Source.LineNo, q.Source.EntryID, err)
	}
	if l.ReversedBy == nil {
		return nil
	}
	var taken bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM assets.acquisition WHERE asset_id = $1 AND source_entry_id = $2 AND source_line_no = $3
		AND status = 'posted')`, asset.ID, l.ReversedBy.EntryID, l.LineNo).Scan(&taken)
	if err != nil {
		return fmt.Errorf("assets: acquisitions of asset %s from entry %s: %w", asset.Number, l.ReversedBy.DocumentNumber, err)
	}
	if taken {
		return problem.Conflict("assets.reversal_line_capitalised", fmt.Sprintf("Asset %s took cost off through line %d of entry %s, which reverses "+
			"this acquisition's line; that acquisition is reversed first.", asset.Number, l.LineNo, l.ReversedBy.DocumentNumber))
	}
	return nil
}

// costStaysAfterReversal refuses a reversal that would leave the asset's
// cost below zero in a ledger (422 assets.cost_negative): a reduction
// capitalised since took off cost this acquisition brought.
func costStaysAfterReversal(ctx context.Context, tx pgx.Tx, asset Asset, q Acquisition) error {
	var short bool
	err := tx.QueryRow(ctx, `SELECT coalesce(bool_or(c < 0), false) FROM (SELECT sum(cost) FILTER (WHERE document_kind <> 'acquisition' OR document_id <> $2) AS c
		FROM assets.movement WHERE asset_id = $1 GROUP BY ledger_id) x`, asset.ID, q.ID).Scan(&short)
	if err != nil {
		return fmt.Errorf("assets: cost of asset %s: %w", asset.Number, err)
	}
	if short {
		return invalid("assets.cost_negative", fmt.Sprintf("Reversing the acquisition would leave asset %s's cost below zero in a ledger: a reduction "+
			"capitalised since took off cost it brought; that one is reversed first.", asset.Number), "id", "an acquisition whose cost the asset still holds")
	}
	return nil
}

// reverseEntries reverses the acquisition's posting group through its
// leading ledger's entry, the group's first, and returns the reversal's
// entries by ledger.
func (e *Endpoints) reverseEntries(ctx context.Context, q Acquisition, on time.Time) (map[uuid.UUID]PostedEntry, error) {
	ledgers, err := e.Ledger.Ledgers(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("assets: ledgers: %w", err)
	}
	// The ledgers come leading first, and an acquisition posts to it.
	first := slices.IndexFunc(q.Entries, func(en DocumentEntry) bool { return len(ledgers) > 0 && en.LedgerID == ledgers[0].ID })
	if first < 0 {
		return nil, errors.New("assets: acquisition " + q.ID.String() + " has no entry in the leading ledger")
	}
	posted, err := e.Ledger.Reverse(ctx, q.Entries[first].EntryID, on)
	if err != nil {
		return nil, fmt.Errorf("assets: reverse acquisition of asset %s: %w", q.AssetNumber, err)
	}
	out := make(map[uuid.UUID]PostedEntry, len(posted))
	for _, en := range posted {
		out[en.LedgerID] = en
	}
	return out, nil
}

// writeReversal writes the acquisition reversed, its movements negated
// with the reversal's seq and entries, each entry's reversal, the amount
// given back to its line, and the asset's status and capitalisation date
// derived again (ADR-0057 §5).
func writeReversal(ctx context.Context, tx pgx.Tx, p reqctx.Principal, q Acquisition, seq int32, on time.Time, reason string,
	reversals map[uuid.UUID]PostedEntry) error {
	tag, err := tx.Exec(ctx, `UPDATE assets.acquisition SET status = 'reversed', reversal_posting_date = $2, reversal_reason = $3, version = version + 1,
		updated_by = $4 WHERE id = $1 AND status = 'posted'`, q.ID, on, reason, p.ID)
	if err != nil {
		return fmt.Errorf("assets: reverse acquisition %s: %w", q.ID, err)
	}
	if tag.RowsAffected() != 1 { // read posted FOR UPDATE in this transaction
		return fmt.Errorf("assets: acquisition %s is no longer posted", q.ID)
	}
	for _, en := range q.Entries {
		r := reversals[en.LedgerID]
		if _, err := tx.Exec(ctx, `INSERT INTO assets.movement (tenant_id, id, asset_id, seq, legal_entity_id, ledger_id, class_code, kind, value_date,
				posting_date, fiscal_year, fiscal_period, cost, cost_gc, document_kind, document_id, journal_entry_year, journal_entry_id, reverses_id, created_by)
			SELECT tenant_id, $3, asset_id, $4, legal_entity_id, ledger_id, class_code, kind, value_date, $5, $6, $7, -cost, -cost_gc, document_kind,
				document_id, $6, $8, id, $9
			FROM assets.movement WHERE document_kind = 'acquisition' AND document_id = $1 AND ledger_id = $2 AND reverses_id IS NULL`,
			q.ID, en.LedgerID, uuid.Must(uuid.NewV7()), seq, on, r.FiscalYear, r.Period, r.ID, p.ID); err != nil {
			return fmt.Errorf("assets: reverse the movement of acquisition %s in ledger %s: %w", q.ID, en.LedgerID, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE assets.document_entry SET reversal_entry_year = $3, reversal_entry_id = $4, reversal_document_number = $5
			WHERE document_kind = 'acquisition' AND document_id = $1 AND ledger_id = $2 AND part = 'main'`,
			q.ID, en.LedgerID, r.FiscalYear, r.ID, r.DocumentNumber); err != nil {
			return fmt.Errorf("assets: reversal of acquisition %s in ledger %s: %w", q.ID, en.LedgerID, err)
		}
	}
	if q.Source != nil {
		if err := useLine(ctx, tx, q.Source.LineRef, share{tc: -q.Amount, lc: -q.LC, gc: -q.GC}); err != nil {
			return err
		}
	}
	return derive(ctx, tx, p, q.AssetID, seq)
}

// recordReversal audits the reversal and publishes it as
// AssetMovementReversed, with the asset's status and version after it and
// per ledger the change of cost it made, its reversal entry and the entry
// it reversed.
func recordReversal(ctx context.Context, tx pgx.Tx, q Acquisition) error {
	asset, ledgers, err := eventLedgers(ctx, tx, q, -1, func(en DocumentEntry) map[string]any {
		r := en.Reversal // set for every entry by the reversal
		return map[string]any{"journal_entry_id": r.EntryID, "fiscal_year": r.FiscalYear, "document_number": r.DocumentNumber, "reversed_entry_id": en.EntryID}
	})
	if err != nil {
		return err
	}
	on := q.Reversal.PostingDate.Format(time.DateOnly)
	detail := map[string]any{"asset_id": q.AssetID, "asset_number": q.AssetNumber, "seq": asset.LastSeq, "posting_date": on, "reason": q.Reversal.Reason,
		"ledgers": ledgers}
	return audit.Record(ctx, "assets.acquisition.reverse", "assets.acquisition", q.ID, detail,
		outbox.Event{Context: "assets", Name: "asset_movement_reversed", AggregateType: "assets.acquisition", AggregateID: q.ID,
			Payload: map[string]any{"document_kind": "acquisition", "document_id": q.ID, "asset_id": q.AssetID, "legal_entity_id": q.LegalEntityID,
				"asset_number": q.AssetNumber, "class_code": asset.Class, "posting_date": on, "reason": q.Reversal.Reason, "currency_lc": q.CurrencyLC,
				"currency_gc": q.CurrencyGC, "ledgers": ledgers, "asset_status": asset.Status, "asset_version": asset.Version}})
}
