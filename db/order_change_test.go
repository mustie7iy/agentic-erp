package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Purchase order changes (T47e, migration 000065): a released order
// changes only as an order change is applied, never its identity, never
// below what was received; a change moves along its lifecycle, and is
// applied exactly as the order moves to the revision it makes.

// released is a released order of f, 10 EA at 2.50 with 10% tolerance and
// 4 received, and an open change of it (draft) based on its revision 1.
func (f fixture) released(t *testing.T) (po, change uuid.UUID) {
	t.Helper()
	price := int64(250)
	var receipt uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
		if po, err = f.order(tx, &price); err != nil {
			return err
		}
		if err := exec(tx, `UPDATE procurement.purchase_order_item SET over_delivery_pct = 10 WHERE purchase_order_id = $1`, po); err != nil {
			return err
		}
		return exec(tx, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, po)
	})
	mustApp(t, f.tenant, f.move(&receipt, po, "101", nil, nil, "4"))
	change = f.orderChange(t, po, 1)
	return po, change
}

// orderChange inserts a draft change of po based on revision base.
func (f fixture) orderChange(t *testing.T, po uuid.UUID, base int) uuid.UUID {
	t.Helper()
	change := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO procurement.purchase_order_change (tenant_id, id, purchase_order_id, reason, base_revision, proposed,
			total_before, total_after, created_by, updated_by) VALUES ($1, $2, $3, 'More paper', $4, '{}', 2500, 3000, $5, $5)`,
			f.tenant, change, po, base, f.actor)
	})
	return change
}

// applying runs sql (with $1 the order) inside the application of change,
// as T47f applies one: the order names it, the items and header are
// written, the change is applied, and the order moves on clearing the name.
func applying(po, change uuid.UUID, sql string) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		if err := exec(tx, `UPDATE procurement.purchase_order SET applying_change_id = $2, version = version + 1 WHERE id = $1`, po, change); err != nil {
			return err
		}
		if err := exec(tx, sql, po); err != nil {
			return err
		}
		if err := exec(tx, `UPDATE procurement.purchase_order_change SET status = 'applied', revision = 2, previous = '{}', version = version + 1
			WHERE id = $1`, change); err != nil {
			return err
		}
		return exec(tx, `UPDATE procurement.purchase_order SET applying_change_id = NULL, revision = revision + 1, version = version + 1 WHERE id = $1`, po)
	}
}

const addItem = `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
	account_id, valuation_price, valuation_price_unit) SELECT tenant_id, $1, 2, 'Toner', 1, 'EA', 100, account_id, %s FROM procurement.purchase_order_item
	WHERE purchase_order_id = $1`

func TestOrderChangeApplication(t *testing.T) {
	need(t)
	header := func(set string) string {
		return `UPDATE procurement.purchase_order SET ` + set + `, version = version + 1 WHERE id = $1`
	}
	item := func(set string) string {
		return `UPDATE procurement.purchase_order_item SET ` + set + ` WHERE purchase_order_id = $1`
	}
	for _, c := range []struct {
		name  string
		apply bool // inside the change's application
		sql   string
		want  string
	}{
		{"an item changed outside a change", false, item("quantity = 12"), "cannot change"},
		{"an item added outside a change", false, fmtAdd("100, 1"), "cannot change"},
		{"the header changed outside a change", false, header("payment_term_code = 'NT60'"), "changes only through an order change"},
		{"the revision moved outside a change", false, header("revision = 2"), "changes only through an order change"},
		{"the supplier changed", false, header("supplier_id = gen_random_uuid()"), "keeps its supplier"},
		{"the currency changed", false, header("currency = 'EUR'"), "keeps its supplier"},
		{"the legal entity changed", false, header("legal_entity_id = gen_random_uuid()"), "keeps its supplier"},
		{"the contract changed", false, header("contract_id = gen_random_uuid()"), "keeps its supplier"},
		{"the requisition changed", false, header("requisition_id = gen_random_uuid()"), "keeps its supplier"},
		{"the purchasing org changed", false, header("purchasing_org_id = gen_random_uuid()"), "keeps its supplier"},
		{"the number changed", false, header("document_number = 'X1'"), "keeps its supplier"},
		{"quantity and price changed", true, item("quantity = 12, unit_price = 300"), ""},
		{"the header changed", true, header("payment_term_code = 'NT60'"), ""},
		{"an item added valued", true, fmtAdd("100, 1"), ""},
		{"an item added unvalued", true, fmtAdd("NULL, NULL"), "with its valuation price"},
		{"below what was received", true, item("quantity = 3"), "below the 4 received"},
		{"within the tolerance of what was received", true, item("quantity = 3.7"), ""},
		{"the tolerance lowered below what was received", true, item("quantity = 3.7, over_delivery_pct = 0"), "below the 4 received"},
		{"an item with receipts removed", true, item("is_removed = true"), "below the 4 received"},
		{"an item's account changed", true, item("account_id = gen_random_uuid()"), "keeps its account"},
		{"an item's unit changed", true, item("uom_code = 'KG'"), "keeps its account"},
		{"an item's material changed", true, item("material_id = gen_random_uuid()"), "keeps its account"},
		{"an item's contract item changed", true, item("contract_item_no = 1"), "keeps its account"},
		{"an item's valuation price changed", true, item("valuation_price = 300"), "valued at the price it was released at"},
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
}

// fmtAdd is addItem with the valuation price and unit given.
func fmtAdd(valuation string) string { return strings.Replace(addItem, "%s", valuation, 1) }

// TestOrderChangeNaming: an order names only an open change of its own
// revision, and clears the name only as it moves to the revision the
// change made.
func TestOrderChangeNaming(t *testing.T) {
	need(t)
	name := func(po, change uuid.UUID, extra string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order SET applying_change_id = $2`+extra+`, version = version + 1 WHERE id = $1`, po, change)
		}
	}
	t.Run("another order's change", func(t *testing.T) {
		f := newFixture(t)
		po, _ := f.released(t)
		_, other := f.released(t)
		failsWith(t, asApp(t, f.tenant, name(po, other, "")), "applies an open change of its own revision")
	})
	t.Run("a change of another revision", func(t *testing.T) {
		f := newFixture(t)
		po, change := f.released(t)
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order_change SET status = 'cancelled', version = version + 1 WHERE id = $1`, change)
		})
		failsWith(t, asApp(t, f.tenant, name(po, f.orderChange(t, po, 2), "")), "applies an open change of its own revision")
	})
	t.Run("a cancelled change", func(t *testing.T) {
		f := newFixture(t)
		po, change := f.released(t)
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order_change SET status = 'cancelled', version = version + 1 WHERE id = $1`, change)
		})
		failsWith(t, asApp(t, f.tenant, name(po, change, "")), "applies an open change of its own revision")
	})
	t.Run("named as the revision moves", func(t *testing.T) {
		f := newFixture(t)
		po, change := f.released(t)
		failsWith(t, asApp(t, f.tenant, name(po, change, ", revision = 2")), "applies an open change of its own revision")
	})
	t.Run("cleared without the change applied", func(t *testing.T) {
		f := newFixture(t)
		po, change := f.released(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := name(po, change, "")(tx); err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.purchase_order SET applying_change_id = NULL, revision = 2, version = version + 1 WHERE id = $1`, po)
		}), "clears the change it applied")
	})
	t.Run("cleared without its revision", func(t *testing.T) {
		f := newFixture(t)
		po, change := f.released(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := name(po, change, "")(tx); err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.purchase_order SET applying_change_id = NULL, version = version + 1 WHERE id = $1`, po)
		}), "changes only through an order change")
	})
	t.Run("a draft's revision moved", func(t *testing.T) {
		f := newFixture(t)
		price := int64(250)
		var po uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) { po, err = f.order(tx, &price); return err })
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order SET revision = 7, version = version + 1 WHERE id = $1`, po)
		}), "changes only through an order change")
	})
	t.Run("a draft naming a change", func(t *testing.T) {
		f := newFixture(t)
		price := int64(250)
		var po uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) { po, err = f.order(tx, &price); return err })
		failsWith(t, asApp(t, f.tenant, name(po, f.orderChange(t, po, 1), "")), "purchase_order_applying_released")
	})
	t.Run("an item added to a pending order", func(t *testing.T) {
		f := newFixture(t)
		price := int64(250)
		var po uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
			if po, err = f.order(tx, &price); err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.purchase_order SET status = 'pending_approval', release_request_id = gen_random_uuid(),
				version = version + 1 WHERE id = $1`, po)
		})
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, fmtAdd("100, 1"), po) }), "cannot change")
	})
	t.Run("an item returned in full removed", func(t *testing.T) {
		f := newFixture(t)
		po, change := f.released(t)
		reason := "Faulty"
		var ret uuid.UUID
		mustApp(t, f.tenant, f.move(&ret, po, "122", nil, &reason, "-4"))
		mustApp(t, f.tenant, applying(po, change, `UPDATE procurement.purchase_order_item SET is_removed = true WHERE purchase_order_id = $1`))
	})
	t.Run("an unvalued item restored", func(t *testing.T) {
		f := newFixture(t)
		price := int64(250)
		var po uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
			if po, err = f.order(tx, &price); err != nil {
				return err
			}
			if err := exec(tx, fmtAdd("NULL, NULL"), po); err != nil {
				return err
			}
			if err := exec(tx, `UPDATE procurement.purchase_order_item SET is_removed = true WHERE purchase_order_id = $1 AND item_no = 2`, po); err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, po)
		})
		change := f.orderChange(t, po, 1)
		failsWith(t, asApp(t, f.tenant, applying(po, change,
			`UPDATE procurement.purchase_order_item SET is_removed = false WHERE purchase_order_id = $1 AND item_no = 2`)), "with its valuation price")
	})
	t.Run("a released order cancelled", func(t *testing.T) {
		f := newFixture(t)
		price := int64(250)
		var po uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
			if po, err = f.order(tx, &price); err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, po)
		})
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order SET status = 'cancelled', version = version + 1 WHERE id = $1`, po)
		})
	})
}

func TestOrderChangeLifecycle(t *testing.T) {
	need(t)
	const submit = "status = 'pending_approval', approval_request_id = gen_random_uuid()"
	const applied = "status = 'applied', revision = 2, previous = '{}'"
	for _, c := range []struct {
		name   string
		after  []string
		set    string
		naming bool // the order names the change, as it is applied
		want   string
	}{
		{"edited as a draft", nil, "reason = 'More toner'", false, ""},
		{"submitted", nil, submit, false, ""},
		{"applied at once", nil, applied, true, ""},
		{"applied without the order applying it", nil, applied, false, "is applied only as its order applies it"},
		{"cancelled", nil, "status = 'cancelled'", false, ""},
		{"applied after approval", []string{submit}, applied, true, ""},
		{"rejected", []string{submit}, "status = 'rejected'", false, ""},
		{"rejected on another request", []string{submit}, "status = 'rejected', approval_request_id = gen_random_uuid()", false, "decided only on the request it names"},
		{"withdrawn", []string{submit}, "status = 'draft', approval_request_id = NULL", false, ""},
		{"edited while pending", []string{submit}, "reason = 'More toner'", false, "cannot go from pending_approval to pending_approval"},
		{"changed as it is submitted", nil, submit + ", total_after = 1", false, "changes only while it is a draft"},
		{"applied on another request", []string{submit}, applied + ", approval_request_id = gen_random_uuid()", true, "decided only on the request it names"},
		{"applied as another revision", nil, "status = 'applied', revision = 3, previous = '{}'", true, "purchase_order_change_revision"},
		{"applied without what it replaced", nil, "status = 'applied', revision = 2", true, "purchase_order_change_applied"},
		{"pending without a request", nil, "status = 'pending_approval'", false, "purchase_order_change_request"},
		{"rejected reopened", []string{submit, "status = 'rejected'"}, "status = 'draft'", false, "cannot go from rejected to draft"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			po, change := f.released(t)
			set := func(sql string, naming bool) error {
				return asApp(t, f.tenant, func(tx pgx.Tx) error {
					if naming {
						if err := exec(tx, `UPDATE procurement.purchase_order SET applying_change_id = $2, version = version + 1 WHERE id = $1`, po, change); err != nil {
							return err
						}
					}
					return exec(tx, `UPDATE procurement.purchase_order_change SET `+sql+`, version = version + 1 WHERE id = $1`, change)
				})
			}
			for _, sql := range c.after {
				if err := set(sql, false); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}
			err := set(c.set, c.naming)
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("an applied change reopened", func(t *testing.T) {
		f := newFixture(t)
		po, change := f.released(t)
		mustApp(t, f.tenant, applying(po, change, `UPDATE procurement.purchase_order SET payment_term_code = 'NT60', version = version + 1 WHERE id = $1`))
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order_change SET status = 'draft', revision = NULL, previous = NULL, version = version + 1 WHERE id = $1`, change)
		}), "cannot go from applied to draft")
	})
	t.Run("one open change per order", func(t *testing.T) {
		f := newFixture(t)
		po, _ := f.released(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO procurement.purchase_order_change (tenant_id, id, purchase_order_id, reason, base_revision, proposed,
				total_before, total_after, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, 'r', 1, '{}', 1, 1, $3, $3)`, f.tenant, po, f.actor)
		}), "purchase_order_one_open_change")
	})
	t.Run("inserted as anything but a draft", func(t *testing.T) {
		f := newFixture(t)
		po, change := f.released(t)
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order_change SET status = 'cancelled', version = version + 1 WHERE id = $1`, change)
		})
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO procurement.purchase_order_change (tenant_id, id, purchase_order_id, status, reason, base_revision, revision,
				previous, proposed, total_before, total_after, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, 'applied', 'r', 1, 2, '{}', '{}',
				1, 1, $3, $3)`, f.tenant, po, f.actor)
		}), "an order change starts as a draft")
	})
	t.Run("a reason too long", func(t *testing.T) {
		f := newFixture(t)
		_, change := f.released(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order_change SET reason = repeat('r', 2001), version = version + 1 WHERE id = $1`, change)
		}), "purchase_order_change_reason_check")
	})
}
