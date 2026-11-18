package inventory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	platformcurrency "github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Price controls (SAP's VPRSV S and V; ADR-0082 §7).
const (
	Standard      = "standard"
	MovingAverage = "moving_average"
)

func init() {
	problem.RegisterConstraint("valuation_once", func() *problem.Problem {
		return problem.Conflict("inventory.valuation_exists", "The material is valued in the plant already; change its valuation instead.")
	})
}

// Valuation is a material's valuation in a plant (SAP MBEW): its price
// control, price in the company's currency per price unit of the base
// unit, valuation class, and the quantity and value of its stock there.
type Valuation struct {
	ID, LegalEntityID, MaterialID, PlantID uuid.UUID
	ValuationClass, PriceControl, Currency string
	Exponent                               int
	Price                                  money.Minor
	PriceUnit                              int32
	Quantity                               string
	Value                                  money.Minor
	Version                                int32
	CreatedAt, UpdatedAt                   time.Time
	CreatedBy, UpdatedBy                   uuid.UUID
}

const valuationSelect = `SELECT v.id, v.legal_entity_id, v.material_id, v.plant_id, v.valuation_class, v.price_control, v.currency, c.exponent,
		v.price, v.price_unit, trim_scale(v.quantity)::text, v.value, v.version, v.created_at, v.created_by, v.updated_at, v.updated_by
	FROM inventory.valuation v JOIN ref.currency c ON c.code = v.currency`

func scanValuation(row pgx.Row) (Valuation, error) {
	var v Valuation
	err := row.Scan(&v.ID, &v.LegalEntityID, &v.MaterialID, &v.PlantID, &v.ValuationClass, &v.PriceControl, &v.Currency, &v.Exponent, &v.Price,
		&v.PriceUnit, &v.Quantity, &v.Value, &v.Version, &v.CreatedAt, &v.CreatedBy, &v.UpdatedAt, &v.UpdatedBy)
	return v, errs.Wrapf(err, "scan valuation")
}

// loadValuation reads a valuation of a company the caller may act in,
// under its row lock when lock; another company's is not found.
func loadValuation(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Valuation, error) {
	a, err := access(ctx)
	if err != nil {
		return Valuation{}, err
	}
	q := valuationSelect + ` WHERE v.id = $1`
	if lock {
		q += ` FOR UPDATE OF v`
	}
	v, err := scanValuation(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !a.Allows(v.LegalEntityID)) {
		return Valuation{}, problem.NotFoundOf("material valuation")
	}
	if err != nil {
		return Valuation{}, fmt.Errorf("inventory: read valuation %s: %w", id, err)
	}
	return v, nil
}

// ValuationInput is a material's valuation in a plant as a request
// creates it; Price is in the plant's company's currency.
type ValuationInput struct {
	MaterialID, PlantID          uuid.UUID
	ValuationClass, PriceControl string
	Price, Currency              string
	PriceUnit                    int32
}

// CreateValuation values a material in a plant (ADR-0082 §8): a stocked
// material extended to an active plant of a company the caller may act
// in, a valuation class of the company's account determination, the
// price in its currency (a standard price above zero), no stock moved
// there unvalued. Audited in the request's transaction.
func (e *Endpoints) CreateValuation(ctx context.Context, in ValuationInput) (Valuation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Valuation{}, err
	}
	r, err := e.read(ctx, []uuid.UUID{in.MaterialID}, []uuid.UUID{in.PlantID}, nil, nil)
	if err != nil {
		return Valuation{}, err
	}
	m, ok := r.materials[in.MaterialID] // a pending or blocked material is valued too
	switch {
	case !ok:
		return Valuation{}, problem.Invalid("inventory.material_invalid", "material_id", "No material with this id.")
	case !m.Stocked():
		return Valuation{}, problem.Invalid("inventory.material_not_stocked", "material_id", "Material "+m.Number+" is of type "+m.Type+
			", which is kept in no stock.")
	}
	company, err := plantOf(m, in.PlantID, r)
	if err != nil {
		return Valuation{}, err
	}
	le, co, err := e.company(ctx, company)
	if err != nil {
		return Valuation{}, err
	}
	exp, err := platformcurrency.Exponent(ctx, tx, le.LocalCurrency, nil)
	if err != nil {
		return Valuation{}, fmt.Errorf("inventory: currency of company %s: %w", le.Code, err)
	}
	price, err := checkValuation(co, le, exp, in)
	if err != nil {
		return Valuation{}, err
	}
	if err := noStock(ctx, tx, in.MaterialID, in.PlantID, m.Number); err != nil {
		return Valuation{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.valuation (tenant_id, id, legal_entity_id, material_id, plant_id, valuation_class, price_control,
			currency, price, price_unit, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`,
		p.TenantID, id, le.ID, in.MaterialID, in.PlantID, in.ValuationClass, in.PriceControl, le.LocalCurrency, price, in.PriceUnit, p.ID); err != nil {
		return Valuation{}, fmt.Errorf("inventory: value material %s at plant %s: %w", m.Number, r.units[in.PlantID].Code, err)
	}
	v, err := loadValuation(ctx, tx, id, false)
	if err != nil {
		return Valuation{}, err
	}
	return v, recordValuation(ctx, "inventory.valuation.create", v, m.Number, r.units[v.PlantID].Code, nil)
}

// plantOf checks a plant a material is valued in: an active plant of a
// company the caller may act in that the material is extended to; it
// answers the plant's company.
func plantOf(m materials.Material, plant uuid.UUID, r refs) (uuid.UUID, error) {
	u, ok := r.units[plant]
	switch {
	case !ok || u.Kind != kindPlant || u.Status != tenancy.Active || !r.access.Allows(u.LegalEntityID):
		return uuid.Nil, problem.Invalid("inventory.plant_invalid", "plant_id", "No active plant with this id in a company you can access.")
	case !hasPlant(m, plant):
		return uuid.Nil, problem.Invalid("inventory.plant_not_extended", "plant_id", "Material "+m.Number+" is not extended to plant "+u.Code+".")
	}
	return u.LegalEntityID, nil
}

// company is the company and its settings in the active configuration,
// nil when the configuration holds none of it: then it has no valuation
// classes (config.Company.InventoryClass).
func (e *Endpoints) company(ctx context.Context, id uuid.UUID) (tenancy.Entity, *config.Company, error) {
	le, _, err := e.LegalEntities.Entity(ctx, id) // a plant's or a valuation's company exists
	if err != nil {
		return tenancy.Entity{}, nil, fmt.Errorf("inventory: company %s: %w", id, err)
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return tenancy.Entity{}, nil, fmt.Errorf("inventory: configuration of company %s: %w", le.Code, err)
	}
	return le, doc.Company(le.Code), nil
}

// checkValuation checks a valuation's class, price control and price,
// and answers the price in minor units.
func checkValuation(co *config.Company, le tenancy.Entity, exp int, in ValuationInput) (money.Minor, error) {
	if _, ok := co.InventoryClass(in.ValuationClass); !ok {
		return 0, problem.Invalid("inventory.unknown_valuation_class", "valuation_class", "Company "+le.Code+" has no valuation class "+
			in.ValuationClass+" (companies[].account_determination.inventory).")
	}
	if in.PriceControl != Standard && in.PriceControl != MovingAverage {
		return 0, problem.Invalid("inventory.unknown_price_control", "price_control", fmt.Sprintf("%q is not a price control; they are standard "+
			"and moving_average.", in.PriceControl))
	}
	price, err := priceOf("price", in.Price, in.Currency, le.LocalCurrency, exp)
	if err != nil {
		return 0, err
	}
	if in.PriceControl == Standard && price == 0 {
		return 0, problem.Invalid("inventory.invalid_price", "price.amount", "A standard price is above zero.")
	}
	return price, nil
}

// priceOf reads a price in the company's currency local, exp decimals:
// not negative, in the currency's form (400 request.invalid_amount
// otherwise).
func priceOf(field, amount, currency, local string, exp int) (money.Minor, error) {
	if currency != local {
		return 0, problem.Invalid("inventory.currency_mismatch", field+".currency", "Prices are in the company's currency "+local+".")
	}
	m, err := platformcurrency.ParseAmount(amount, local, exp, field+".amount")
	if err != nil {
		return 0, err
	}
	if m < 0 {
		return 0, problem.Invalid("inventory.invalid_price", field+".amount", "A price is not negative.")
	}
	return m, nil
}

// noStock refuses a valuation of a material whose stock in the plant was
// moved before it was valued (S1c): its value could not be told.
func noStock(ctx context.Context, tx pgx.Tx, material, plant uuid.UUID, number string) error {
	var any bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM inventory.stock WHERE material_id = $1 AND plant_id = $2 AND quantity <> 0)`,
		material, plant).Scan(&any); err != nil {
		return fmt.Errorf("inventory: stock of material %s: %w", number, err)
	}
	if any {
		return problem.Conflict("inventory.stock_without_valuation", "Material "+number+" has stock in the plant moved before it was valued; "+
			"reverse those documents first.")
	}
	return nil
}

// recordValuation adds the audit record of a valuation's change, with its
// values before when before is given.
func recordValuation(ctx context.Context, action string, v Valuation, material, plant string, before map[string]any) error {
	detail := map[string]any{"material_id": v.MaterialID, "material_number": material, "plant_id": v.PlantID, "plant_code": plant,
		"valuation_class": v.ValuationClass, "price_control": v.PriceControl, "price": money.EventAmount(v.Price, v.Currency, v.Exponent),
		"price_unit": v.PriceUnit}
	if before != nil {
		detail["before"] = before
	}
	return errs.Wrapf(audit.Record(ctx, action, "inventory.valuation", v.ID, detail), "inventory: record %s of valuation %s", action, v.ID)
}
