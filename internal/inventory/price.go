package inventory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ChangeValuationClass changes a valuation's class at its version
// (ADR-0082 §8): to another class of the company, while the material has
// no stock in the plant, as SAP refuses a class change with stock (from
// memory). Audited with the class before.
func (e *Endpoints) ChangeValuationClass(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, class string) (Valuation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Valuation{}, err
	}
	v, err := loadValuation(ctx, tx, id, true)
	if err != nil {
		return Valuation{}, err
	}
	if err := ifMatch(v.Version); err != nil {
		return Valuation{}, err
	}
	le, co, err := e.company(ctx, v.LegalEntityID)
	if err != nil {
		return Valuation{}, err
	}
	r, err := e.read(ctx, []uuid.UUID{v.MaterialID}, []uuid.UUID{v.PlantID}, nil, nil)
	if err != nil {
		return Valuation{}, err
	}
	number := r.materials[v.MaterialID].Number
	switch _, known := co.InventoryClass(class); {
	case class == v.ValuationClass:
		return Valuation{}, problem.Unprocessable("inventory.nothing_to_change", "Material "+number+" is of valuation class "+class+" already.")
	case !known:
		return Valuation{}, problem.Invalid("inventory.unknown_valuation_class", "valuation_class", "Company "+le.Code+" has no valuation class "+
			class+" (companies[].account_determination.inventory).")
	case money.ParseRat(v.Quantity).Sign() > 0:
		return Valuation{}, problem.Conflict("inventory.valuation_class_with_stock", "Material "+number+" has stock in plant "+
			r.units[v.PlantID].Code+"; its valuation class changes once it has none, as its stock stands on its class's account.")
	}
	if _, err := tx.Exec(ctx, `UPDATE inventory.valuation SET valuation_class = $2, version = version + 1, updated_by = $3 WHERE id = $1`,
		id, class, p.ID); err != nil {
		return Valuation{}, fmt.Errorf("inventory: change the valuation class of material %s: %w", number, err)
	}
	after, err := loadValuation(ctx, tx, id, false)
	if err != nil {
		return Valuation{}, err
	}
	return after, recordValuation(ctx, "inventory.valuation.change", after, number, r.units[v.PlantID].Code, map[string]any{"valuation_class": v.ValuationClass})
}

// ChangePrice changes a valuation's price at its version (ADR-0082 §8,
// SAP MR21): its stock is revalued at the new price, quantity × price /
// price unit, and the difference posted between the class's stock and
// revaluation accounts on the company's date today, refused while that
// day's period is closed. A price change is kept (inventory.price_change),
// which sets the valuation's price and value; audited and published as
// MaterialPriceChanged.
func (e *Endpoints) ChangePrice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, amount, currency string) (Valuation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Valuation{}, err
	}
	v, err := loadValuation(ctx, tx, id, true)
	if err != nil {
		return Valuation{}, err
	}
	if err := ifMatch(v.Version); err != nil {
		return Valuation{}, err
	}
	price, err := priceOf("price", amount, currency, v.Currency, v.Exponent)
	if err != nil {
		return Valuation{}, err
	}
	r, err := e.read(ctx, []uuid.UUID{v.MaterialID}, []uuid.UUID{v.PlantID}, nil, []uuid.UUID{v.LegalEntityID})
	if err != nil {
		return Valuation{}, err
	}
	number := r.materials[v.MaterialID].Number
	switch {
	case price == v.Price:
		return Valuation{}, problem.Unprocessable("inventory.nothing_to_change", "Material "+number+"'s price is "+money.Minor(price).Plain(v.Exponent)+
			" already.")
	case price == 0 && v.PriceControl == Standard:
		return Valuation{}, problem.Invalid("inventory.invalid_price", "price.amount", "A standard price is above zero.")
	}
	le, day, err := e.today(ctx, v.LegalEntityID)
	if err != nil {
		return Valuation{}, err
	}
	if err := e.Postings.Postable(ctx, le.ID, day.Date); err != nil {
		return Valuation{}, fmt.Errorf("inventory: change the price of material %s on %s: %w", number, day.Date.Format(time.DateOnly), err)
	}
	worth, err := money.Scale(price, v.Quantity, int64(v.PriceUnit))
	if err != nil {
		return Valuation{}, problem.Unprocessable("inventory.amount_too_large", "Material "+number+"'s stock at the new price is beyond the range of an amount.")
	}
	change := priceChange{id: uuid.Must(uuid.NewV7()), v: v, price: price, amount: worth - v.Value, day: day.Date}
	entry, err := e.revalue(ctx, change, le.Code, r.materials[v.MaterialID])
	if err != nil {
		return Valuation{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.price_change (tenant_id, id, valuation_id, old_price, new_price, quantity, amount, posting_date,
			journal_entry_id, journal_entry_year, journal_entry_number, created_by) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9, $10, $11, $12)`,
		p.TenantID, change.id, v.ID, v.Price, price, v.Quantity, change.amount, day.Date, entry.id, entry.year, entry.number, p.ID); err != nil {
		return Valuation{}, fmt.Errorf("inventory: keep the price change of material %s: %w", number, err)
	}
	after, err := loadValuation(ctx, tx, id, false)
	if err != nil {
		return Valuation{}, err
	}
	return after, recordPrice(ctx, change, after, r, entry)
}

// priceChange is a price change about to be kept.
type priceChange struct {
	id     uuid.UUID
	v      Valuation
	price  money.Minor
	amount money.Minor
	day    time.Time
}

// revalue posts a price change's revaluation (document type PR, SAP's
// price change, from memory): the stock account against the class's
// revaluation account; nothing when nothing is revalued.
func (e *Endpoints) revalue(ctx context.Context, c priceChange, company string, m materials.Material) (entryRef, error) {
	number := m.Number
	if c.amount == 0 {
		return entryRef{}, nil
	}
	a := &accounts{e: e, company: c.v.LegalEntityID, code: company, found: map[string]uuid.UUID{}}
	stock, err := a.of(ctx, "stock", c.v.ValuationClass)
	if err != nil {
		return entryRef{}, err
	}
	reval, err := a.of(ctx, "revaluation", c.v.ValuationClass)
	if err != nil {
		return entryRef{}, err
	}
	text := "Price change of material " + number
	entry, err := e.Postings.Post(ctx, ledger.Posting{LegalEntityID: c.v.LegalEntityID, DocumentType: "PR", DocumentDate: c.day, PostingDate: c.day,
		Currency: c.v.Currency, Reference: &number, HeaderText: &text, SourceContext: "inventory", SourceDocumentID: c.id,
		Lines: []ledger.PostingLine{{AccountID: stock, Amount: c.amount, Quantity: &c.v.Quantity, Unit: &m.BaseUnit}, {AccountID: reval, Amount: -c.amount}}})
	if err != nil {
		return entryRef{}, fmt.Errorf("inventory: post the price change of material %s: %w", number, err)
	}
	return entryRef{id: &entry.ID, year: &entry.FiscalYear, number: &entry.DocumentNumber}, nil
}

// recordPrice adds the audit record of a price change and publishes
// MaterialPriceChanged in the same transaction (ADR-0007, ADR-0011),
// naming the material, plant and company by id and number or code.
func recordPrice(ctx context.Context, c priceChange, v Valuation, r refs, entry entryRef) error {
	payload := map[string]any{"price_change_id": c.id, "valuation_id": v.ID, "material_id": v.MaterialID, "material_number": r.materials[v.MaterialID].Number,
		"plant_id": v.PlantID, "plant_code": r.units[v.PlantID].Code, "legal_entity_id": v.LegalEntityID,
		"legal_entity_code": r.companies[v.LegalEntityID].Code, "price_control": v.PriceControl,
		"old_price": money.EventAmount(c.v.Price, v.Currency, v.Exponent), "new_price": money.EventAmount(c.price, v.Currency, v.Exponent),
		"price_unit": v.PriceUnit, "quantity": v.Quantity, "revaluation": money.EventAmount(c.amount, v.Currency, v.Exponent),
		"posting_date": c.day.Format(time.DateOnly), "journal_entry_id": entry.id, "journal_entry_year": entry.year, "journal_entry_number": entry.number,
		"version": v.Version}
	if err := audit.Record(ctx, "inventory.valuation.change_price", "inventory.valuation", v.ID, payload,
		outbox.Event{Context: "inventory", Name: "material_price_changed", AggregateType: "inventory.valuation", AggregateID: v.ID, Payload: payload}); err != nil {
		return fmt.Errorf("inventory: record the price change of valuation %s: %w", v.ID, err)
	}
	return nil
}
