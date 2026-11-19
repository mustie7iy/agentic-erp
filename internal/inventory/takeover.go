package inventory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The stock an SAP migration takes over (S2p; ADR-0082 As built S2p): the
// integration context's migration run values a material in a plant as
// SAP's MBEW does and posts SAP's unrestricted stock at the plant's
// storage locations (MARD) as one initial entry (561) through TakeOver,
// each checked, audited and published as the API's valuations and goods
// movements are, and reads back what it took over for the reconciliation
// (TakenOver).

// TakeOverItem is the stock at a storage location of the plant: a
// positive quantity in the material's base unit and, for a moving price,
// what it is worth (nil: valued at the standard price).
type TakeOverItem struct {
	StorageLocationID uuid.UUID
	Quantity          string
	Amount            *Amount
}

// StockTakeOver is a material's valuation in a plant and its stock there,
// the initial entry's header text naming where it comes from. No items:
// the material is valued with no stock.
type StockTakeOver struct {
	Valuation  ValuationInput
	HeaderText string
	Items      []TakeOverItem
}

// TakenOverStock is what a take-over created: the valuation, and the
// initial entry's material document when it posted stock.
type TakenOverStock struct {
	Valuation Valuation
	Document  *Document
}

// TakeOver values the material in the plant (CreateValuation) and posts
// its stock as a 561 material document in the request's transaction,
// dated the company's date today and numbered from its material_document
// range, each item checked as an initial entry's is (an active material
// kept at an active storage location of the plant), valued at what it is
// worth or at the standard price, its value posted on the class's stock
// account against its initial entry account (the migration clearing
// account at a cutover, ADR-0082 §8), audited and published
// (MaterialDocumentPosted). The valuation is created first and has no
// stock, so it is locked by no one else; the document takes its stock
// rows and then the company's range, as every posting does (ADR-0008).
func (e *Endpoints) TakeOver(ctx context.Context, in StockTakeOver) (TakenOverStock, error) {
	v, err := e.CreateValuation(ctx, in.Valuation)
	if err != nil || len(in.Items) == 0 {
		return TakenOverStock{Valuation: v}, err
	}
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return TakenOverStock{}, err
	}
	items := make([]ItemInput, len(in.Items))
	for i, it := range in.Items {
		items[i] = ItemInput{MaterialID: in.Valuation.MaterialID, PlantID: in.Valuation.PlantID, StorageLocationID: it.StorageLocationID,
			Quantity: it.Quantity, Amount: it.Amount}
	}
	r, err := e.inputRefs(ctx, items)
	if err != nil {
		return TakenOverStock{}, err
	}
	for i := range items {
		items[i].Unit = r.materials[in.Valuation.MaterialID].BaseUnit // valued above, so read
	}
	company, lines, err := e.items(items, movementTypes["561"], r)
	if err != nil {
		return TakenOverStock{}, err
	}
	c, err := e.companyOf(ctx, company)
	if err != nil {
		return TakenOverStock{}, err
	}
	d, err := e.write(ctx, tx, p, posting{company: c.le, movementType: "561", day: c.day.Date, headerText: &in.HeaderText, lines: lines, items: items}, r)
	if err != nil {
		return TakenOverStock{}, err
	}
	if err := e.record(ctx, "inventory.material_document.post", d, r, map[string]any{"has_header_text": true}); err != nil {
		return TakenOverStock{}, err
	}
	if v, err = loadValuation(ctx, tx, v.ID, false); err != nil {
		return TakenOverStock{}, err
	}
	return TakenOverStock{Valuation: v, Document: &d}, nil
}

// LockNumbers takes the company's material_document range of the fiscal
// year of its date today, the range TakeOver draws from, without drawing
// a number (ADR-0082 As built S2p, Locks): a migration run takes it
// before the GR/IR locks it posts under, as a goods receipt into stock
// takes its material document number before its order's GR/IR lock, so
// the two never wait on each other in a cycle.
func (e *Endpoints) LockNumbers(ctx context.Context, company uuid.UUID) error {
	c, err := e.companyOf(ctx, company)
	if err != nil {
		return err
	}
	year, err := e.Calendar.FiscalYear(ctx, company, c.day.Date)
	if err != nil {
		return fmt.Errorf("inventory: fiscal year of %s in company %s: %w", c.day.Date.Format(time.DateOnly), c.le.Code, err)
	}
	if err := e.Numbers.Lock(ctx, company, NumberRange, year); err != nil {
		return fmt.Errorf("inventory: lock the material document range of company %s: %w", c.le.Code, err)
	}
	return nil
}

// HeldStock is a valuation as it is now with the stock of its material
// at each storage location of its plant (by id, in its base unit as a
// plain decimal), every location the material was ever moved at.
type HeldStock struct {
	Valuation Valuation
	Locations map[uuid.UUID]string
}

// TakenOver reads the valuations with the ids, of companies the caller
// may act in, with their stock by storage location, and the journal
// entries the material documents with the ids posted (by document; none
// for one that posted none), for the reconciliation of what migration
// runs took over. An id of none is left out.
func (e *Endpoints) TakenOver(ctx context.Context, valuations, documents []uuid.UUID) (map[uuid.UUID]HeldStock, map[uuid.UUID]uuid.UUID, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, nil, err
	}
	a, err := access(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, _ := tx.Query(ctx, valuationSelect+` WHERE v.id = ANY($1) ORDER BY v.id`, valuations)
	vs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Valuation, error) { return scanValuation(row) })
	if err != nil {
		return nil, nil, fmt.Errorf("inventory: read %d valuations: %w", len(valuations), err)
	}
	held := map[uuid.UUID]HeldStock{}
	var ms, plants []uuid.UUID
	for _, v := range vs {
		if a.Allows(v.LegalEntityID) {
			held[v.ID] = HeldStock{Valuation: v, Locations: map[uuid.UUID]string{}}
			ms, plants = append(ms, v.MaterialID), append(plants, v.PlantID)
		}
	}
	byKey := map[valuationKey]uuid.UUID{}
	for id, h := range held {
		byKey[valuationKey{h.Valuation.MaterialID, h.Valuation.PlantID}] = id
	}
	rows, _ = tx.Query(ctx, `SELECT s.material_id, s.plant_id, s.storage_location_id, trim_scale(s.quantity)::text FROM inventory.stock s
		JOIN unnest($1::uuid[], $2::uuid[]) AS k (material_id, plant_id) ON k.material_id = s.material_id AND k.plant_id = s.plant_id
		ORDER BY s.material_id, s.plant_id, s.storage_location_id`, ms, plants)
	var material, plant, location uuid.UUID
	var quantity string
	if _, err := pgx.ForEachRow(rows, []any{&material, &plant, &location, &quantity}, func() error {
		held[byKey[valuationKey{material, plant}]].Locations[location] = quantity
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("inventory: read the stock of %d valuations: %w", len(held), err)
	}
	entries, err := documentEntries(ctx, tx, a, documents)
	return held, entries, err
}

// documentEntries are the journal entries the material documents with
// the ids posted, of companies a allows, by document.
func documentEntries(ctx context.Context, tx pgx.Tx, a authz.Access, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT id, legal_entity_id, journal_entry_id FROM inventory.material_document WHERE id = ANY($1) AND journal_entry_id IS NOT NULL
		ORDER BY id`, ids)
	out := map[uuid.UUID]uuid.UUID{}
	var id, company, entry uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&id, &company, &entry}, func() error {
		if a.Allows(company) {
			out[id] = entry
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("inventory: read the journal entries of %d material documents: %w", len(ids), err)
	}
	return out, nil
}
