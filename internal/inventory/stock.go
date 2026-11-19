package inventory

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// stockKey is what a stock row is of: a material at a storage location of
// a plant.
type stockKey struct {
	material, plant, location uuid.UUID
}

// compare orders stock keys as PostgreSQL orders their uuids (bytewise),
// the order every posting locks stock rows in.
func (k stockKey) compare(o stockKey) int {
	return cmp.Or(bytes.Compare(k.material[:], o.material[:]), bytes.Compare(k.plant[:], o.plant[:]), bytes.Compare(k.location[:], o.location[:]))
}

// maxStock is the least quantity a quantity column refuses: 10^14, a
// fifteenth integer digit (money.QuantityIntDigits).
var maxStock = new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(money.QuantityIntDigits), nil))

// takeStock locks the stock rows the lines move, writing a row with no
// quantity for a location moved the first time, in stockKey order, so
// two postings never wait on each other in a cycle; then it walks the
// lines in their order, as movement_stock applies them one by one, and
// refuses a line that takes a location's stock below zero (409
// inventory.insufficient_stock) or beyond a quantity column (422
// inventory.quantity_too_large). The check follows the lock, so two
// issues of the last quantity at once post one (ADR-0008). A location an
// open count document counts is refused first (blocked, S2c).
func takeStock(ctx context.Context, tx pgx.Tx, p reqctx.Principal, company uuid.UUID, lines []Line, r refs) error {
	var keys []stockKey
	for _, l := range lines {
		if k := keyOf(l); !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, stockKey.compare)
	current, err := lockStock(ctx, tx, p, company, keys)
	if err != nil {
		return err
	}
	if err := blocked(ctx, tx, keys, r); err != nil {
		return err
	}
	for _, l := range lines {
		k, move := keyOf(l), money.ParseRat(l.Quantity)
		after := new(big.Rat).Add(current[k], move)
		switch {
		case after.Sign() < 0:
			return r.shortage(k, current[k], move)
		case after.Cmp(maxStock) >= 0:
			return problem.Unprocessable("inventory.quantity_too_large", fmt.Sprintf("The stock of material %s at %s would have more than %d "+
				"integer digits.", r.materials[k.material].Number, r.where(k), money.QuantityIntDigits))
		}
		current[k] = after
	}
	return nil
}

func keyOf(l Line) stockKey { return stockKey{l.MaterialID, l.PlantID, l.StorageLocationID} }

// lockStock writes the stock rows of the keys a posting moves the first
// time, with no quantity, and locks every one, in the keys' order; it
// answers each row's quantity.
func lockStock(ctx context.Context, tx pgx.Tx, p reqctx.Principal, company uuid.UUID, keys []stockKey) (map[stockKey]*big.Rat, error) {
	ids, ms, plants, locations := make([]uuid.UUID, len(keys)), make([]uuid.UUID, len(keys)), make([]uuid.UUID, len(keys)), make([]uuid.UUID, len(keys))
	for i, k := range keys {
		ids[i], ms[i], plants[i], locations[i] = uuid.Must(uuid.NewV7()), k.material, k.plant, k.location
	}
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id)
		SELECT $1, x.id, $2, x.m, x.p, x.s FROM unnest($3::uuid[], $4::uuid[], $5::uuid[], $6::uuid[]) WITH ORDINALITY AS x (id, m, p, s, n)
		ORDER BY x.n ON CONFLICT (tenant_id, material_id, plant_id, storage_location_id) DO NOTHING`,
		p.TenantID, company, ids, ms, plants, locations); err != nil {
		return nil, fmt.Errorf("inventory: stock rows of %d locations: %w", len(keys), err)
	}
	rows, _ := tx.Query(ctx, `SELECT s.material_id, s.plant_id, s.storage_location_id, s.quantity::text FROM inventory.stock s
		JOIN unnest($1::uuid[], $2::uuid[], $3::uuid[]) AS x (m, p, l) ON s.material_id = x.m AND s.plant_id = x.p AND s.storage_location_id = x.l
		ORDER BY s.material_id, s.plant_id, s.storage_location_id FOR UPDATE OF s`, ms, plants, locations)
	out := map[stockKey]*big.Rat{}
	var k stockKey
	var q string
	_, err := pgx.ForEachRow(rows, []any{&k.material, &k.plant, &k.location, &q}, func() error {
		out[k] = money.ParseRat(q)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inventory: lock the stock of %d locations: %w", len(keys), err)
	}
	return out, nil
}

// shortage is the refusal of a posting that takes more of a location's
// stock than it holds, naming the material, the location and both
// quantities.
func (r refs) shortage(k stockKey, have, move *big.Rat) error {
	m := r.materials[k.material]
	takes := new(big.Rat).Neg(move)
	return problem.Conflict("inventory.insufficient_stock", fmt.Sprintf("Material %s %s has %s %s at %s when the document's line takes %s, "+
		"more than that.",
		m.Number, m.Description, money.PlainQuantity(have), m.BaseUnit, r.where(k), money.PlainQuantity(takes)))
}

// where names a stock's location by its plant's and storage location's
// codes.
func (r refs) where(k stockKey) string {
	return strings.Join([]string{"plant", r.units[k.plant].Code, "storage location", r.units[k.location].Code}, " ")
}
