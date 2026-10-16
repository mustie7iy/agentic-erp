package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Purchase order changes (T47e, migration 000065): a released order
// changes only as an order change is applied, never its identity, never
// below what was received; a change moves along its lifecycle.

// released is a released order of f, 10 EA at 2.50 with 4 received, and
// an open change of it (draft) based on its revision 1.
func (f fixture) released(t *testing.T) (po, change uuid.UUID) {
	t.Helper()
	price := int64(250)
	var receipt uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
		if po, err = f.order(tx, &price); err != nil {
			return err
		}
		return exec(tx, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, po)
	})
	mustApp(t, f.tenant, f.move(&receipt, po, "101", nil, nil, "4"))
	change = id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO procurement.purchase_order_change (tenant_id, id, purchase_order_id, reason, base_revision, proposed,
			total_before, total_after, created_by, updated_by) VALUES ($1, $2, $3, 'More paper', 1, '{}', 2500, 3000, $4, $4)`, f.tenant, change, po, f.actor)
	})
	return po, change
}

// applying runs sql (with $1 the order) inside the application of change.
func applying(po, change uuid.UUID, sql string) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		if err := exec(tx, `UPDATE procurement.purchase_order SET applying_change_id = $2, version = version + 1 WHERE id = $1`, po, change); err != nil {
			return err
		}
		if err := exec(tx, sql, po); err != nil {
			return err
		}
		return exec(tx, `UPDATE procurement.purchase_order SET applying_change_id = NULL, revision = revision + 1, version = version + 1 WHERE id = $1`, po)
	}
}

func TestOrderChangeApplication(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name  string
		apply bool // inside the change's application
		sql   string
		want  string
	}{
		{"an item changed outside a change", false, `UPDATE procurement.purchase_order_item SET quantity = 12 WHERE purchase_order_id = $1`, "cannot change"},
		{"an item added outside a change", false, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity,
			uom_code, unit_price, account_id, valuation_price, valuation_price_unit) SELECT tenant_id, $1, 2, 'Toner', 1, 'EA', 100, account_id, 100, 1
			FROM procurement.purchase_order_item WHERE purchase_order_id = $1`, "cannot change"},
		{"the header changed outside a change", false, `UPDATE procurement.purchase_order SET payment_term_code = 'NT60', version = version + 1 WHERE id = $1`,
			"changes only through an order change"},
		{"the revision moved outside a change", false, `UPDATE procurement.purchase_order SET revision = 2, version = version + 1 WHERE id = $1`,
			"changes only through an order change"},
		{"the supplier changed", false, `UPDATE procurement.purchase_order SET supplier_id = gen_random_uuid(), version = version + 1 WHERE id = $1`,
			"keeps its supplier"},
		{"quantity and price changed", true, `UPDATE procurement.purchase_order_item SET quantity = 12, unit_price = 300 WHERE purchase_order_id = $1`, ""},
		{"the header changed", true, `UPDATE procurement.purchase_order SET payment_term_code = 'NT60', version = version + 1 WHERE id = $1`, ""},
		{"an item added valued", true, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code,
			unit_price, account_id, valuation_price, valuation_price_unit) SELECT tenant_id, $1, 2, 'Toner', 1, 'EA', 100, account_id, 100, 1
			FROM procurement.purchase_order_item WHERE purchase_order_id = $1`, ""},
		{"an item added unvalued", true, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code,
			unit_price, account_id) SELECT tenant_id, $1, 2, 'Toner', 1, 'EA', 100, account_id FROM procurement.purchase_order_item WHERE purchase_order_id = $1`,
			"with its valuation price"},
		{"below what was received", true, `UPDATE procurement.purchase_order_item SET quantity = 3 WHERE purchase_order_id = $1`, "below the 4 received"},
		{"an item with receipts removed", true, `UPDATE procurement.purchase_order_item SET is_removed = true WHERE purchase_order_id = $1`, "below the 4 received"},
		{"an item's account changed", true, `UPDATE procurement.purchase_order_item SET account_id = gen_random_uuid() WHERE purchase_order_id = $1`,
			"keeps its account"},
		{"an item's valuation price changed", true, `UPDATE procurement.purchase_order_item SET valuation_price = 300 WHERE purchase_order_id = $1`,
			"valued at the price it was released at"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			po, change := f.released(t)
			fn := func(tx pgx.Tx) error { return exec(tx, c.sql, po) }
			if c.apply {
				fn = applying(po, change, c.sql)
			}
			err := asApp(t, f.tenant, fn)
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("the change cleared without its revision", func(t *testing.T) {
		f := newFixture(t)
		po, change := f.released(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, `UPDATE procurement.purchase_order SET applying_change_id = $2, version = version + 1 WHERE id = $1`, po, change); err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.purchase_order SET applying_change_id = NULL, version = version + 1 WHERE id = $1`, po)
		}), "changes only through an order change")
	})
	t.Run("applying another order's change", func(t *testing.T) {
		f := newFixture(t)
		po, _ := f.released(t)
		_, other := f.released(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order SET applying_change_id = $2, version = version + 1 WHERE id = $1`, po, other)
		}), "applies an open change of its own revision")
	})
	t.Run("applying a change of a draft", func(t *testing.T) {
		f := newFixture(t)
		price := int64(250)
		var po uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) { po, err = f.order(tx, &price); return err })
		change := id()
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, `INSERT INTO procurement.purchase_order_change (tenant_id, id, purchase_order_id, reason, base_revision, proposed,
				total_before, total_after, created_by, updated_by) VALUES ($1, $2, $3, 'r', 1, '{}', 1, 1, $4, $4)`, f.tenant, change, po, f.actor); err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.purchase_order SET applying_change_id = $2, version = version + 1 WHERE id = $1`, po, change)
		}), "purchase_order_applying_released")
	})
}

func TestOrderChangeLifecycle(t *testing.T) {
	need(t)
	const submit = "status = 'pending_approval', approval_request_id = gen_random_uuid()"
	const applied = "status = 'applied', revision = 2, previous = '{}'"
	for _, c := range []struct {
		name  string
		after []string
		set   string
		want  string
	}{
		{"edited as a draft", nil, "reason = 'More toner'", ""},
		{"submitted", nil, submit, ""},
		{"applied at once", nil, applied, ""},
		{"cancelled", nil, "status = 'cancelled'", ""},
		{"applied after approval", []string{submit}, applied, ""},
		{"rejected", []string{submit}, "status = 'rejected'", ""},
		{"withdrawn", []string{submit}, "status = 'draft'", ""},
		{"edited while pending", []string{submit}, "reason = 'More toner'", "cannot go from pending_approval to pending_approval"},
		{"changed as it is submitted", nil, submit + ", total_after = 1", "changes only while it is a draft"},
		{"applied on another request", []string{submit}, applied + ", approval_request_id = gen_random_uuid()", "decided only on the request it names"},
		{"applied as another revision", nil, "status = 'applied', revision = 3, previous = '{}'", "makes revision 2"},
		{"applied without what it replaced", nil, "status = 'applied', revision = 2", "purchase_order_change_applied"},
		{"pending without a request", nil, "status = 'pending_approval'", "purchase_order_change_request"},
		{"applied reopened", []string{applied}, "status = 'draft', revision = NULL, previous = NULL", "cannot go from applied to draft"},
		{"rejected reopened", []string{submit, "status = 'rejected'"}, "status = 'draft'", "cannot go from rejected to draft"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			_, change := f.released(t)
			set := func(sql string) error {
				return asApp(t, f.tenant, func(tx pgx.Tx) error {
					return exec(tx, `UPDATE procurement.purchase_order_change SET `+sql+`, version = version + 1 WHERE id = $1`, change)
				})
			}
			for _, sql := range c.after {
				if err := set(sql); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}
			err := set(c.set)
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("one open change per order", func(t *testing.T) {
		f := newFixture(t)
		po, _ := f.released(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO procurement.purchase_order_change (tenant_id, id, purchase_order_id, reason, base_revision, proposed,
				total_before, total_after, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, 'r', 1, '{}', 1, 1, $3, $3)`, f.tenant, po, f.actor)
		}), "purchase_order_one_open_change")
	})
	t.Run("a reason too long", func(t *testing.T) {
		f := newFixture(t)
		_, change := f.released(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order_change SET reason = repeat('r', 2001), version = version + 1 WHERE id = $1`, change)
		}), "purchase_order_change_reason_check")
	})
}
