package assets

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
)

// gain is what a retirement gained (positive) or lost (negative) in a
// ledger, in local and group currency: the proceeds less the book value
// taken (IAS 16.68, 16.71, from memory; ADR-0057 §8).
func (q Retirement) gain(v RetiredValues) (money.Minor, money.Minor) {
	return q.Proceeds - v.Cost - v.Accumulated, q.ProceedsGC - v.CostGC - v.AccumulatedGC
}

// retirementLedgers is, per ledger the retirement posted an entry in, the
// change it made to the asset's values there (its amounts times sign: the
// cost and accumulated depreciation, catch-up included, it took off), its
// gain or loss and the entry fields of each ledger's entry. A ledger it
// posted nothing in (every amount zero) has no entry and is left out.
func retirementLedgers(ctx context.Context, tx pgx.Tx, q Retirement, sign money.Minor, entry func(DocumentEntry) map[string]any) ([]map[string]any, error) {
	expLC, err := currency.Exponent(ctx, tx, q.Currency, nil)
	if err != nil {
		return nil, fmt.Errorf("assets: currency %s: %w", q.Currency, err)
	}
	expGC, err := currency.Exponent(ctx, tx, q.CurrencyGC, nil)
	if err != nil {
		return nil, fmt.Errorf("assets: currency %s: %w", q.CurrencyGC, err)
	}
	lc := func(m money.Minor) map[string]string { return money.EventAmount(sign*m, q.Currency, expLC) }
	gc := func(m money.Minor) map[string]string { return money.EventAmount(sign*m, q.CurrencyGC, expGC) }
	out := []map[string]any{}
	for _, v := range q.Ledgers {
		en, ok := entryOf(q.Entries, v)
		if !ok {
			continue
		}
		gain, gainGC := q.gain(v)
		l := entry(en)
		l["ledger_id"], l["cost"], l["cost_gc"] = v.LedgerID, lc(-v.Cost), gc(-v.CostGC)
		l["accumulated"], l["accumulated_gc"] = lc(-v.CatchUp-v.Accumulated), gc(-v.CatchUpGC-v.AccumulatedGC)
		l["gain"], l["gain_gc"] = lc(gain), gc(gainGC)
		out = append(out, l)
	}
	return out, nil
}

// entryOf is a document's entry in the ledger the values are of.
func entryOf(es []DocumentEntry, v RetiredValues) (DocumentEntry, bool) {
	for _, en := range es {
		if en.LedgerID == v.LedgerID {
			return en, true
		}
	}
	return DocumentEntry{}, false
}

// retiredInLedgerOrder puts the retirement's values per ledger in ledger order
// (inLedgerOrder), which its audit records and events list them in: they
// are read grouped, in no order (batch 50's fix forward, ADR-0057), and
// returns the ledgers in that order for the asset's terms.
func (e *Endpoints) retiredInLedgerOrder(ctx context.Context, q Retirement) ([]LedgerRef, error) {
	return inPortLedgerOrder(ctx, e.Ledger, q.Ledgers, func(v RetiredValues) uuid.UUID { return v.LedgerID })
}

// recordRetirement audits the retirement and publishes it as
// AssetMovementPosted, with the asset's status and version after it and
// its ledgers in ledger order, and as AssetChanged (retired) when it
// retired the asset (ADR-0057 §10).
func (e *Endpoints) recordRetirement(ctx context.Context, tx pgx.Tx, q Retirement, before string) error {
	order, err := e.retiredInLedgerOrder(ctx, q)
	if err != nil {
		return err
	}
	ledgers, err := retirementLedgers(ctx, tx, q, 1, func(en DocumentEntry) map[string]any {
		return map[string]any{"journal_entry_id": en.EntryID, "fiscal_year": en.FiscalYear, "document_number": en.DocumentNumber}
	})
	if err != nil {
		return err
	}
	asset, err := readAsset(ctx, tx, q.AssetID, false)
	if err != nil {
		return err
	}
	detail := map[string]any{"asset_id": q.AssetID, "asset_number": q.AssetNumber, "seq": q.Seq, "kind": q.Kind, "value_date": q.ValueDate.Format(time.DateOnly),
		"posting_date": q.PostingDate.Format(time.DateOnly), "share_percent": q.SharePercent, "share_cost": q.ShareCost, "proceeds": int64(q.Proceeds),
		"ledgers": ledgers}
	if err := audit.Record(ctx, "assets.retirement.post", "assets.retirement", q.ID, detail,
		outbox.Event{Context: "assets", Name: "asset_movement_posted", AggregateType: "assets.retirement", AggregateID: q.ID,
			Payload: map[string]any{"document_kind": "retirement", "document_id": q.ID, "asset_id": q.AssetID, "legal_entity_id": q.LegalEntityID,
				"asset_number": q.AssetNumber, "class_code": asset.Class, "value_date": q.ValueDate.Format(time.DateOnly),
				"posting_date": q.PostingDate.Format(time.DateOnly), "currency_lc": q.Currency, "currency_gc": q.CurrencyGC, "ledgers": ledgers,
				"asset_status": asset.Status, "asset_version": asset.Version}}); err != nil {
		return err
	}
	if asset.Status == StatusRetired && before != StatusRetired {
		return record(ctx, asset, order, "retire", "retired")
	}
	return nil
}

// recordRetirementReversal audits the reversal and publishes it as
// AssetMovementReversed, with the asset's status and version after it and
// per ledger the change it made (the retirement's negated), its reversal
// entry and the entry it reversed; and as AssetChanged (reactivated) when
// it made a retired asset active again; its ledgers in ledger order.
func (e *Endpoints) recordRetirementReversal(ctx context.Context, tx pgx.Tx, q Retirement, before string) error {
	order, err := e.retiredInLedgerOrder(ctx, q)
	if err != nil {
		return err
	}
	ledgers, err := retirementLedgers(ctx, tx, q, -1, func(en DocumentEntry) map[string]any {
		r := en.Reversal // set for every entry by the reversal
		return map[string]any{"journal_entry_id": r.EntryID, "fiscal_year": r.FiscalYear, "document_number": r.DocumentNumber, "reversed_entry_id": en.EntryID}
	})
	if err != nil {
		return err
	}
	asset, err := readAsset(ctx, tx, q.AssetID, false)
	if err != nil {
		return err
	}
	on := q.Reversal.PostingDate.Format(time.DateOnly)
	detail := map[string]any{"asset_id": q.AssetID, "asset_number": q.AssetNumber, "seq": asset.LastSeq, "posting_date": on, "reason": q.Reversal.Reason,
		"ledgers": ledgers}
	if err := audit.Record(ctx, "assets.retirement.reverse", "assets.retirement", q.ID, detail,
		outbox.Event{Context: "assets", Name: "asset_movement_reversed", AggregateType: "assets.retirement", AggregateID: q.ID,
			Payload: map[string]any{"document_kind": "retirement", "document_id": q.ID, "asset_id": q.AssetID, "legal_entity_id": q.LegalEntityID,
				"asset_number": q.AssetNumber, "class_code": asset.Class, "posting_date": on, "reason": q.Reversal.Reason, "currency_lc": q.Currency,
				"currency_gc": q.CurrencyGC, "ledgers": ledgers, "asset_status": asset.Status, "asset_version": asset.Version}}); err != nil {
		return err
	}
	if before == StatusRetired && asset.Status != StatusRetired {
		return record(ctx, asset, order, "reactivate", "reactivated")
	}
	return nil
}
