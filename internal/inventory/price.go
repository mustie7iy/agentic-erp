package inventory

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
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
	c, err := e.companyOf(ctx, v.LegalEntityID)
	if err != nil {
		return Valuation{}, err
	}
	r, err := e.read(ctx, []uuid.UUID{v.MaterialID}, []uuid.UUID{v.PlantID}, nil, nil)
	if err != nil {
		return Valuation{}, err
	}
	number := r.materials[v.MaterialID].Number
	switch _, known := c.co.InventoryClass(class); {
	case class == v.ValuationClass:
		return Valuation{}, problem.Unprocessable("inventory.nothing_to_change", "Material "+number+" is of valuation class "+class+" already.")
	case !known:
		return Valuation{}, unknownClass(c.le.Code, class)
	case v.qty().Sign() > 0:
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
	r, err := e.read(ctx, []uuid.UUID{v.MaterialID}, []uuid.UUID{v.PlantID}, nil, []uuid.UUID{v.LegalEntityID})
	if err != nil {
		return Valuation{}, err
	}
	number := r.materials[v.MaterialID].Number
	price, err := newPrice(v, number, Amount{Value: amount, Currency: currency})
	if err != nil {
		return Valuation{}, err
	}
	co, err := e.companyOf(ctx, v.LegalEntityID)
	if err != nil {
		return Valuation{}, err
	}
	day := co.day.Date
	if err := e.Postings.Postable(ctx, co.le.ID, day); err != nil {
		return Valuation{}, fmt.Errorf("inventory: change the price of material %s on %s: %w", number, day.Format(time.DateOnly), err)
	}
	// money.Round of an exact quotient fails only beyond an amount's range.
	worth, err := money.Round(new(big.Rat).Quo(new(big.Rat).Mul(v.qty(), rat(price)), rat(money.Minor(v.PriceUnit))), 0)
	if err != nil {
		return Valuation{}, problem.Unprocessable("inventory.amount_too_large", "Material "+number+"'s stock at the new price is beyond the range of an amount.")
	}
	change := priceChange{id: uuid.Must(uuid.NewV7()), v: v, price: price, amount: worth - v.Value, day: day}
	entry, err := e.revalue(ctx, change, co.le.Code, r.materials[v.MaterialID])
	if err != nil {
		return Valuation{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.price_change (tenant_id, id, valuation_id, old_price, new_price, quantity, amount, posting_date,
			journal_entry_id, journal_entry_year, journal_entry_number, created_by) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9, $10, $11, $12)`,
		p.TenantID, change.id, v.ID, v.Price, price, v.Quantity, change.amount, day, entry.id, entry.year, entry.number, p.ID); err != nil {
		return Valuation{}, fmt.Errorf("inventory: keep the price change of material %s: %w", number, err)
	}
	after, err := loadValuation(ctx, tx, id, false)
	if err != nil {
		return Valuation{}, err
	}
	return after, recordPrice(ctx, change, after, r, entry)
}

// newPrice reads a valuation's new price: in its currency, not negative,
// another than its price, and above zero for a standard price.
func newPrice(v Valuation, number string, a Amount) (money.Minor, error) {
	price, err := amountOf("price", a, v.Currency, v.Exponent, "inventory.invalid_price")
	switch {
	case err != nil:
		return 0, err
	case price == v.Price:
		return 0, problem.Unprocessable("inventory.nothing_to_change", "Material "+number+"'s price is "+price.Plain(v.Exponent)+" already.")
	case price == 0 && v.PriceControl == Standard:
		return 0, problem.Invalid("inventory.invalid_price", "price.amount", "A standard price is above zero.")
	}
	return price, nil
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
	a := e.accounts(c.v.LegalEntityID, company)
	stock, err := a.of(ctx, config.InventoryStock, c.v.ValuationClass)
	if err != nil {
		return entryRef{}, err
	}
	reval, err := a.of(ctx, config.InventoryRevaluation, c.v.ValuationClass)
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
	return entryOf(entry), nil
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
	return errs.Wrapf(audit.Record(ctx, "inventory.valuation.change_price", "inventory.valuation", v.ID, payload,
		outbox.Event{Context: "inventory", Name: "material_price_changed", AggregateType: "inventory.valuation", AggregateID: v.ID, Payload: payload}),
		"inventory: record the price change of valuation %s", v.ID)
}
