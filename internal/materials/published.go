package materials

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Materials is the context's published API (ADR-0082 §2): the materials
// inventory, procurement and sales read by id, in the caller's
// transaction, whatever companies the caller may act in (each reader
// judges access by its own documents' companies).
type Materials struct{}

// Materials reads the materials with the ids, each with its plants,
// storage locations and alternative units, keyed by id; an id that is no
// material has no entry.
func (Materials) Materials(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Material, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, materialSelect+` WHERE m.id = ANY($1) ORDER BY m.id`, ids)
	ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Material, error) { return scanMaterial(r) })
	if err != nil {
		return nil, fmt.Errorf("materials: read %d materials: %w", len(ids), err)
	}
	out := make(map[uuid.UUID]Material, len(ms))
	for _, m := range ms {
		out[m.ID] = m
	}
	return out, nil
}

// ByNumber reads the materials with the numbers, as Materials does,
// keyed by number; a number that is no material's has no entry. A
// mapping of free-text keys onto materials finds them so (S1f2).
func (Materials) ByNumber(ctx context.Context, numbers []string) (map[string]Material, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, materialSelect+` WHERE m.number = ANY($1) ORDER BY m.number`, numbers)
	ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Material, error) { return scanMaterial(r) })
	if err != nil {
		return nil, fmt.Errorf("materials: read %d materials by number: %w", len(numbers), err)
	}
	out := make(map[string]Material, len(ms))
	for _, m := range ms {
		out[m.Number] = m
	}
	return out, nil
}

// stocked are the material types kept in stock: SAP's non-stock material
// (NLAG) and service (DIEN) have no quantity in stock (from memory).
var stocked = map[string]bool{"raw_material": true, "semi_finished": true, "finished_good": true, "trading_good": true}

// Stocked reports whether the material's type is kept in stock, so goods
// movements may move it.
func (m Material) Stocked() bool { return stocked[m.Type] }

// Plant is the material's plant with the id, ok = false when it is not
// extended to it.
func (m Material) Plant(id uuid.UUID) (Plant, bool) {
	i := slices.IndexFunc(m.Plants, func(p Plant) bool { return p.PlantID == id })
	if i < 0 {
		return Plant{}, false
	}
	return m.Plants[i], true
}

// KeptAt reports whether the material is kept at the storage location of
// the plant: extended to the plant and to the storage location there.
func (m Material) KeptAt(plant, storageLocation uuid.UUID) bool {
	p, ok := m.Plant(plant)
	return ok && slices.Contains(p.StorageLocations, storageLocation)
}

// ErrUnknownUnit is Convert's refusal of a unit that is neither the
// material's base unit nor one of its alternative units.
var ErrUnknownUnit = errors.New("not a unit of the material")

// ErrInexact is Convert's refusal of a quantity whose value in the base
// unit has more than money.QuantityDecimals decimals or more than
// money.QuantityIntDigits integer digits (ADR-0082 §5): refused, never
// rounded.
var ErrInexact = errors.New("not exact in the base unit")

// Factor is how many base units one unit is: 1 for the base unit, an
// alternative unit's numerator / denominator (ADR-0082 §5).
func (m Material) Factor(unit string) (*big.Rat, error) {
	if unit == m.BaseUnit {
		return big.NewRat(1, 1), nil
	}
	i := slices.IndexFunc(m.Units, func(u Unit) bool { return u.Code == unit })
	if i < 0 {
		return nil, fmt.Errorf("materials: %s of material %s: %w", unit, m.Number, ErrUnknownUnit)
	}
	return big.NewRat(int64(m.Units[i].Numerator), int64(m.Units[i].Denominator)), nil
}

// Convert is quantity, a positive plain decimal (money.PositiveQuantity)
// in unit, in the material's base unit, exactly: one alternative unit is
// numerator / denominator base units (ADR-0082 §5).
func (m Material) Convert(quantity *big.Rat, unit string) (*big.Rat, error) {
	f, err := m.Factor(unit)
	if err != nil {
		return nil, err
	}
	base := new(big.Rat).Mul(quantity, f)
	if money.PositiveQuantity(base.FloatString(money.QuantityDecimals)) != nil || !exact(base) {
		return nil, fmt.Errorf("materials: %s %s of material %s in %s: %w", quantity.FloatString(money.QuantityDecimals), unit, m.Number, m.BaseUnit,
			ErrInexact)
	}
	return base, nil
}

// exact reports whether r has at most money.QuantityDecimals decimals: its
// denominator divides 10^6.
func exact(r *big.Rat) bool {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(money.QuantityDecimals), nil)
	return new(big.Int).Mod(scale, r.Denom()).Sign() == 0
}
