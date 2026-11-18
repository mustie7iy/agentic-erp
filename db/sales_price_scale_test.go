package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSalesPriceScaleRules (T80e, migration 000383; ADR-0067 §12): a
// record's scale is read by quantity, in a stated unit (a price's own),
// or, a discount's only, by value in a stated currency; a level is
// numbered 1 to 20, starts at a quantity or a value above zero and gives
// one rate, a price above zero or a percent above 0 and below 100; no two
// levels of a record start at the same place; levels are never changed
// or deleted, by the application or by the owner.
func TestSalesPriceScaleRules(t *testing.T) {
	need(t)
	const record = `INSERT INTO sales.price (tenant_id, id, legal_entity_id, item_key, condition_type, uom_code, currency, unit_price, price_unit,
		percent, valid_from, scale_basis, scale_uom_code, scale_currency, created_by)
		VALUES ($1, $2, $3, 'BOLT-M10', $4, $5, $6, $7, $8, $9::numeric, '2026-03-10', $10, $11, $12, $1)`
	const level = `INSERT INTO sales.price_scale (tenant_id, price_id, level, from_quantity, from_amount, unit_price, percent)
		VALUES ($1, $2, $3, $4::numeric, $5, $6, $7::numeric)`
	type scene struct{ tenant, company, price uuid.UUID }
	str := func(s string) *string { return &s }
	num := func(n int64) *int64 { return &n }
	var none *string
	var no *int64
	// price is a price of 0.80 EUR per EA with the scale given.
	price := func(s scene, basis, unit, currency *string) stmt {
		return step(record, s.tenant, s.price, s.company, none, "EA", "EUR", num(80), num(1), none, basis, unit, currency)
	}
	// discount is a discount of 1 percent with the scale given.
	discount := func(s scene, basis, unit, currency *string) stmt {
		return step(record, s.tenant, s.price, s.company, "K007", none, none, no, no, "1", basis, unit, currency)
	}
	quantityPrice := func(s scene) stmt { return price(s, str("quantity"), str("EA"), none) }
	at := func(s scene, n int, from *string, amount, unitPrice *int64, percent *string) stmt {
		return step(level, s.tenant, s.price, n, from, amount, unitPrice, percent)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a price with a quantity scale of two levels", func(tx pgx.Tx, s scene) error {
			return all(tx, quantityPrice(s), at(s, 1, str("1000"), no, num(70), none), at(s, 2, str("5000"), no, num(60), none))
		}, ""},
		{"a discount with a quantity scale in another unit than an item's", func(tx pgx.Tx, s scene) error {
			return all(tx, discount(s, str("quantity"), str("KG"), none), at(s, 1, str("10.5"), no, no, str("2")))
		}, ""},
		{"a discount with a value scale", func(tx pgx.Tx, s scene) error {
			return all(tx, discount(s, str("value"), none, str("EUR")), at(s, 1, none, num(100000), no, str("99.999999")))
		}, ""},
		{"a basis neither quantity nor value", func(tx pgx.Tx, s scene) error { return all(tx, price(s, str("weight"), str("EA"), none)) },
			"price_scale_basis_check"},
		{"a quantity scale without its unit", func(tx pgx.Tx, s scene) error { return all(tx, price(s, str("quantity"), none, none)) },
			"price_scale_stated"},
		{"a quantity scale with a currency", func(tx pgx.Tx, s scene) error { return all(tx, price(s, str("quantity"), str("EA"), str("EUR"))) },
			"price_scale_stated"},
		{"a value scale on a price", func(tx pgx.Tx, s scene) error { return all(tx, price(s, str("value"), none, str("EUR"))) },
			"price_scale_stated"},
		{"a value scale without its currency", func(tx pgx.Tx, s scene) error { return all(tx, discount(s, str("value"), none, none)) },
			"price_scale_stated"},
		{"a value scale with a unit", func(tx pgx.Tx, s scene) error { return all(tx, discount(s, str("value"), str("EA"), str("EUR"))) },
			"price_scale_stated"},
		{"a scale unit without a basis", func(tx pgx.Tx, s scene) error { return all(tx, discount(s, none, str("EA"), none)) },
			"price_scale_stated"},
		{"a scale currency without a basis", func(tx pgx.Tx, s scene) error { return all(tx, discount(s, none, none, str("EUR"))) },
			"price_scale_stated"},
		{"a price's quantity scale in another unit than its own", func(tx pgx.Tx, s scene) error {
			return all(tx, price(s, str("quantity"), str("KG"), none))
		}, "price_scale_unit"},
		{"an unknown scale unit", func(tx pgx.Tx, s scene) error { return all(tx, discount(s, str("quantity"), str("ZZZ"), none)) },
			"price_scale_uom_code_fkey"},
		{"an unknown scale currency", func(tx pgx.Tx, s scene) error { return all(tx, discount(s, str("value"), none, str("ZZZ"))) },
			"price_scale_currency_fkey"},
		{"a level of no record", func(tx pgx.Tx, s scene) error { return all(tx, at(s, 1, str("10"), no, num(70), none)) }, "price_scale_tenant_id_price_id_fkey"},
		{"level 0", func(tx pgx.Tx, s scene) error {
			return all(tx, quantityPrice(s), at(s, 0, str("10"), no, num(70), none))
		}, "price_scale_level_check"},
		{"level 21", func(tx pgx.Tx, s scene) error {
			return all(tx, quantityPrice(s), at(s, 21, str("10"), no, num(70), none))
		}, "price_scale_level_check"},
		{"a level from no quantity", func(tx pgx.Tx, s scene) error {
			return all(tx, quantityPrice(s), at(s, 1, str("0"), no, num(70), none))
		},
			"price_scale_from_quantity_check"},
		{"a level from no value", func(tx pgx.Tx, s scene) error {
			return all(tx, discount(s, str("value"), none, str("EUR")), at(s, 1, none, num(0), no, str("2")))
		}, "price_scale_from_amount_check"},
		{"a level from a quantity and a value", func(tx pgx.Tx, s scene) error {
			return all(tx, quantityPrice(s), at(s, 1, str("10"), num(100), num(70), none))
		}, "price_scale_from"},
		{"a level from nowhere", func(tx pgx.Tx, s scene) error { return all(tx, quantityPrice(s), at(s, 1, none, no, num(70), none)) }, "price_scale_from"},
		{"a level with a price and a percent", func(tx pgx.Tx, s scene) error {
			return all(tx, quantityPrice(s), at(s, 1, str("10"), no, num(70), str("2")))
		}, "price_scale_rate"},
		{"a level without a rate", func(tx pgx.Tx, s scene) error { return all(tx, quantityPrice(s), at(s, 1, str("10"), no, no, none)) }, "price_scale_rate"},
		{"a level's price of nothing", func(tx pgx.Tx, s scene) error {
			return all(tx, quantityPrice(s), at(s, 1, str("10"), no, num(0), none))
		},
			"price_scale_unit_price_check"},
		{"a level's percent of nothing", func(tx pgx.Tx, s scene) error {
			return all(tx, discount(s, str("quantity"), str("EA"), none), at(s, 1, str("10"), no, no, str("0")))
		}, "price_scale_percent_check"},
		{"a level's percent of everything", func(tx pgx.Tx, s scene) error {
			return all(tx, discount(s, str("quantity"), str("EA"), none), at(s, 1, str("10"), no, no, str("100")))
		}, "price_scale_percent_check"},
		{"two levels from one quantity", func(tx pgx.Tx, s scene) error {
			return all(tx, quantityPrice(s), at(s, 1, str("10"), no, num(70), none), at(s, 2, str("10.000"), no, num(60), none))
		}, "price_scale_by_quantity"},
		{"two levels from one value", func(tx pgx.Tx, s scene) error {
			return all(tx, discount(s, str("value"), none, str("EUR")), at(s, 1, none, num(100), no, str("2")), at(s, 2, none, num(100), no, str("3")))
		}, "price_scale_by_amount"},
		{"a level numbered twice", func(tx pgx.Tx, s scene) error {
			return all(tx, quantityPrice(s), at(s, 1, str("10"), no, num(70), none), at(s, 1, str("20"), no, num(60), none))
		}, "price_scale_pkey"},
		{"a level changed", func(tx pgx.Tx, s scene) error {
			return all(tx, quantityPrice(s), at(s, 1, str("10"), no, num(70), none), step(`UPDATE sales.price_scale SET unit_price = 1`))
		}, "permission denied"},
		{"a level deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, quantityPrice(s), at(s, 1, str("10"), no, num(70), none), step(`DELETE FROM sales.price_scale`))
		}, "permission denied"},
		{"a level changed by the owner", func(tx pgx.Tx, s scene) error {
			if err := all(tx, quantityPrice(s), at(s, 1, str("10"), no, num(70), none)); err != nil {
				return err
			}
			return asOwner(func(tx pgx.Tx) error { return exec(tx, `UPDATE sales.price_scale SET unit_price = 1`) })(tx)
		}, "insert-only"},
		{"a level deleted by the owner", func(tx pgx.Tx, s scene) error {
			if err := all(tx, quantityPrice(s), at(s, 1, str("10"), no, num(70), none)); err != nil {
				return err
			}
			return asOwner(func(tx pgx.Tx) error { return exec(tx, `DELETE FROM sales.price_scale`) })(tx)
		}, "insert-only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), company: id(), price: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return c.run(tx, s) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}
