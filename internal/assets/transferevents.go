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

// transferLedgers is, per ledger the transfer posted an entry in, the
// change it made to the asset's values there (its amounts times sign: the
// cost an asset transfer took off, and the accumulated depreciation, its
// catch-up included; a center transfer changes no cost, only where it is
// booked) with the fields of its main entry (of its catch-up entry when
// it has no main one) and those of a catch-up entry of its own. A ledger
// it posted nothing in is left out.
func transferLedgers(ctx context.Context, tx pgx.Tx, q Transfer, sign money.Minor, entry func(DocumentEntry) map[string]any) ([]map[string]any, error) {
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
		parts := partsIn(q.Entries, v.LedgerID)
		main, ok := parts[partMain]
		if !ok {
			main, ok = parts[partCatchUp]
		}
		if !ok {
			continue
		}
		taken, takenGC, cost, costGC := v.Accumulated, v.AccumulatedGC, v.Cost, v.CostGC
		if q.Kind == TransferCenter { // the balances moved stay the asset's
			taken, takenGC, cost, costGC = 0, 0, 0, 0
		}
		l := entry(main)
		l["ledger_id"], l["cost"], l["cost_gc"] = v.LedgerID, lc(-cost), gc(-costGC)
		l["accumulated"], l["accumulated_gc"] = lc(-v.CatchUp-taken), gc(-v.CatchUpGC-takenGC)
		if c, ok := parts[partCatchUp]; ok && main.Part == partMain {
			for k, x := range entry(c) {
				l["catch_up_"+k] = x
			}
		}
		out = append(out, l)
	}
	return out, nil
}

// partsIn are a document's entries in the ledger by part.
func partsIn(es []DocumentEntry, ledger uuid.UUID) map[string]DocumentEntry {
	out := map[string]DocumentEntry{}
	for _, en := range es {
		if en.LedgerID == ledger {
			out[en.Part] = en
		}
	}
	return out
}

// transferPayload is what AssetMovementPosted and AssetMovementReversed
// carry of a transfer besides its ledgers: its kind, an asset transfer's
// receiver with its status and version after it, and a center transfer's
// centers before and after it.
func transferPayload(ctx context.Context, tx pgx.Tx, q Transfer, payload map[string]any) error {
	payload["transfer_kind"] = q.Kind
	if q.ToAssetID == nil {
		payload["from_cost_center_id"], payload["from_profit_center_id"] = q.FromCostCenterID, q.FromProfitCenterID
		payload["cost_center_id"], payload["profit_center_id"] = q.CostCenterID, q.ProfitCenterID
		return nil
	}
	to, err := readAsset(ctx, tx, *q.ToAssetID, false)
	if err != nil {
		return err
	}
	payload["to_asset_id"], payload["to_asset_number"], payload["to_class_code"] = to.ID, to.Number, to.Class
	payload["to_asset_status"], payload["to_asset_version"] = to.Status, to.Version
	return nil
}

// recordTransfer audits the transfer and publishes it as
// AssetMovementPosted, with the assets' status and version after it, and
// as AssetChanged (retired) when a full transfer retired the asset
// (ADR-0057 §10).
func recordTransfer(ctx context.Context, tx pgx.Tx, q Transfer, before string) error {
	ledgers, err := transferLedgers(ctx, tx, q, 1, func(en DocumentEntry) map[string]any {
		return map[string]any{"journal_entry_id": en.EntryID, "fiscal_year": en.FiscalYear, "document_number": en.DocumentNumber}
	})
	if err != nil {
		return err
	}
	asset, err := readAsset(ctx, tx, q.AssetID, false)
	if err != nil {
		return err
	}
	payload := map[string]any{"document_kind": "transfer", "document_id": q.ID, "asset_id": q.AssetID, "legal_entity_id": q.LegalEntityID,
		"asset_number": q.AssetNumber, "class_code": asset.Class, "value_date": q.ValueDate.Format(time.DateOnly),
		"posting_date": q.PostingDate.Format(time.DateOnly), "currency_lc": q.Currency, "currency_gc": q.CurrencyGC, "ledgers": ledgers,
		"asset_status": asset.Status, "asset_version": asset.Version}
	if err := transferPayload(ctx, tx, q, payload); err != nil {
		return err
	}
	detail := map[string]any{"asset_id": q.AssetID, "asset_number": q.AssetNumber, "seq": q.Seq, "kind": q.Kind, "to_asset_id": q.ToAssetID,
		"to_seq": q.ToSeq, "value_date": q.ValueDate.Format(time.DateOnly), "posting_date": q.PostingDate.Format(time.DateOnly),
		"share_percent": q.SharePercent, "share_cost": q.ShareCost, "from_cost_center_id": q.FromCostCenterID,
		"from_profit_center_id": q.FromProfitCenterID, "cost_center_id": q.CostCenterID, "profit_center_id": q.ProfitCenterID, "ledgers": ledgers}
	if err := audit.Record(ctx, "assets.transfer.post", "assets.transfer", q.ID, detail,
		outbox.Event{Context: "assets", Name: "asset_movement_posted", AggregateType: "assets.transfer", AggregateID: q.ID, Payload: payload}); err != nil {
		return err
	}
	if asset.Status == StatusRetired && before != StatusRetired {
		return record(ctx, asset, "retire", "retired")
	}
	return nil
}

// recordTransferReversal audits the reversal and publishes it as
// AssetMovementReversed, with the assets' status and version after it and
// per ledger the change it made (the transfer's negated), its reversal
// entries and the entries they reversed; and as AssetChanged
// (reactivated) when it made a retired asset active again.
func recordTransferReversal(ctx context.Context, tx pgx.Tx, q Transfer, before string) error {
	ledgers, err := transferLedgers(ctx, tx, q, -1, func(en DocumentEntry) map[string]any {
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
	payload := map[string]any{"document_kind": "transfer", "document_id": q.ID, "asset_id": q.AssetID, "legal_entity_id": q.LegalEntityID,
		"asset_number": q.AssetNumber, "class_code": asset.Class, "posting_date": on, "reason": q.Reversal.Reason, "currency_lc": q.Currency,
		"currency_gc": q.CurrencyGC, "ledgers": ledgers, "asset_status": asset.Status, "asset_version": asset.Version}
	if err := transferPayload(ctx, tx, q, payload); err != nil {
		return err
	}
	detail := map[string]any{"asset_id": q.AssetID, "asset_number": q.AssetNumber, "seq": asset.LastSeq, "posting_date": on, "reason": q.Reversal.Reason,
		"ledgers": ledgers}
	if err := audit.Record(ctx, "assets.transfer.reverse", "assets.transfer", q.ID, detail,
		outbox.Event{Context: "assets", Name: "asset_movement_reversed", AggregateType: "assets.transfer", AggregateID: q.ID, Payload: payload}); err != nil {
		return err
	}
	if before == StatusRetired && asset.Status != StatusRetired {
		return record(ctx, asset, "reactivate", "reactivated")
	}
	return nil
}
