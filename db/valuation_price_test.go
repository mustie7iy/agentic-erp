package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Valuation prices (T47a, migration 000061): an order item's goods
// movements are valued through GR/IR at the price it was released at, so
// a later change of its price cannot revalue its open GR/IR position.

// order inserts a draft order of f with one item of 10 EA at 2.50, valued
// at valued (none when nil), and returns its id.
func (f fixture) order(tx pgx.Tx, valued *int64) (uuid.UUID, error) {
	po := id()
	if err := exec(tx, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1,$2,$3,$4,$5,'USD','2026-08-01','NT30','draft',$6,$6)`,
		f.tenant, po, po.String()[24:], f.entity, f.supplier, f.actor); err != nil {
		return po, err
	}
	var unit *int32
	if valued != nil {
		one := int32(1)
		unit = &one
	}
	return po, exec(tx, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		account_id, valuation_price, valuation_price_unit) VALUES ($1,$2,1,'Paper',10,'EA',250,$3,$4,$5)`, f.tenant, po, f.expense, valued, unit)
}

func TestValuationPrices(t *testing.T) {
	need(t)
	price := int64(250)
	release := func(po uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, po)
		}
	}
	t.Run("released with its items valued", func(t *testing.T) {
		f := newFixture(t)
		var po uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) { po, err = f.order(tx, &price); return err })
		mustApp(t, f.tenant, release(po))
	})
	// Whatever else lets an item change: today only a draft's items do,
	// an order change will (T47e).
	t.Run("a valued item's price changed", func(t *testing.T) {
		f := newFixture(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			po, err := f.order(tx, &price)
			if err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.purchase_order_item SET valuation_price = 300 WHERE purchase_order_id = $1`, po)
		}), "valued at the price it was released at")
	})
	// An application that does not value the items yet (one released
	// before T47a, during a rolling upgrade) still releases: the release
	// values them at their price.
	t.Run("released with an item not valued", func(t *testing.T) {
		f := newFixture(t)
		var po uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) { po, err = f.order(tx, nil); return err })
		mustApp(t, f.tenant, release(po))
		var valued, unit int64
		if err := testDB.QueryRow(context.Background(), `SELECT valuation_price, valuation_price_unit FROM procurement.purchase_order_item
			WHERE purchase_order_id = $1`, po).Scan(&valued, &unit); err != nil {
			t.Fatal(err)
		}
		if valued != 250 {
			t.Errorf("valuation price on release: got %d, want 250", valued)
		}
		if unit != 1 {
			t.Errorf("valuation price unit on release: got %d, want 1", unit)
		}
	})
	t.Run("an item added to a released order", func(t *testing.T) {
		f := newFixture(t)
		var po uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) { po, err = f.order(tx, &price); return err })
		mustApp(t, f.tenant, release(po))
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code,
				unit_price, account_id) VALUES ($1,$2,2,'Toner',1,'EA',100,$3)`, f.tenant, po, f.expense)
		}), "cannot change") // outside an order change; inside one, an item comes valued (TestOrderChangeApplication)
	})
	for _, c := range []struct {
		name   string
		valued *int64 // the item's valuation price before the change
		set    string
		want   string // empty: allowed
	}{
		{"a negative price", nil, "valuation_price = -1, valuation_price_unit = 1", "purchase_order_item_valuation_price_check"},
		{"a zero price unit", nil, "valuation_price = 1, valuation_price_unit = 0", "purchase_order_item_valuation_price_unit_check"},
		{"the unit of a valued price", &price, "valuation_price_unit = 10", "valued at the price it was released at"},
		{"a valued price cleared", &price, "valuation_price = NULL, valuation_price_unit = NULL", "valued at the price it was released at"},
		{"a valued price set as it is", &price, "valuation_price = 250, valuation_price_unit = 1", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				po, err := f.order(tx, c.valued)
				if err != nil {
					return err
				}
				return exec(tx, `UPDATE procurement.purchase_order_item SET `+c.set+` WHERE purchase_order_id = $1`, po)
			})
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Errorf("got %v, want no error", err)
			}
		})
	}
	t.Run("a price without its unit", func(t *testing.T) {
		f := newFixture(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			po, err := f.order(tx, nil)
			if err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.purchase_order_item SET valuation_price = 250 WHERE purchase_order_id = $1`, po)
		}), "purchase_order_item_valuation_pair")
	})
}

// TestValuationPricesBackfilled: the items of orders released before
// 000061 are valued at their price; a draft's are not yet.
func TestValuationPricesBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, 61)
	f := fixtureIDs()
	var draft, released uuid.UUID
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) error {
		if err := f.seed(tx); err != nil {
			return err
		}
		var err error
		for _, po := range []*uuid.UUID{&draft, &released} {
			*po = id()
			if err = exec(tx, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency,
				order_date, payment_term_code, status, created_by, updated_by) VALUES ($1,$2,$3,$4,$5,'USD','2026-08-01','NT30','draft',$6,$6)`,
				f.tenant, *po, po.String()[24:], f.entity, f.supplier, f.actor); err != nil {
				return err
			}
			if err = exec(tx, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code,
				unit_price, price_unit, account_id) VALUES ($1,$2,1,'Paper',10,'EA',250,10,$3)`, f.tenant, *po, f.expense); err != nil {
				return err
			}
		}
		return exec(tx, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, released)
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		po        uuid.UUID
		price     *int64
		priceUnit *int32
	}{{"draft", draft, nil, nil}, {"released", released, new(int64(250)), new(int32(10))}} {
		t.Run(c.name, func(t *testing.T) {
			var price *int64
			var unit *int32
			if err := conn.QueryRow(context.Background(), `SELECT valuation_price, valuation_price_unit FROM procurement.purchase_order_item
				WHERE purchase_order_id = $1`, c.po).Scan(&price, &unit); err != nil {
				t.Fatal(err)
			}
			if !samePtr(price, c.price) {
				t.Errorf("valuation price: got %v, want %v", deref(price), deref(c.price))
			}
			if !samePtr(unit, c.priceUnit) {
				t.Errorf("valuation price unit: got %v, want %v", deref(unit), deref(c.priceUnit))
			}
		})
	}
}

func samePtr[T comparable](a, b *T) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func deref[T any](v *T) any {
	if v == nil {
		return nil
	}
	return *v
}
