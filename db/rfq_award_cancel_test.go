package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// An award's purchase order cancelled (T82h, migration 000371): an awarded
// RFQ becomes order_cancelled, keeping its order and reason, only once
// that order is cancelled, and stays so.

const (
	orderCancelledSQL = `UPDATE procurement.rfq SET status = 'order_cancelled', version = version + 1 WHERE id = $1`
	cancelOrderSQL    = `UPDATE procurement.purchase_order SET status = 'cancelled', version = version + 1 WHERE id = $1`
)

// TestRFQOrderCancelledRules: what the database refuses of an RFQ's award
// order cancelled, as the application role and, for the trigger's rules,
// as the owner.
func TestRFQOrderCancelledRules(t *testing.T) {
	need(t)
	f := newOrderAwards(t)
	mustApp(t, f.tenant, orderAwarding(f.quotation, f.published, f.order, "Delivers in time"))
	mustApp(t, f.tenant, awarding(f.third, f.other, f.contract))
	one := func(sql string, args ...any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error { return exec(tx, sql, args...) }
	}
	const notCancelled = "only once that purchase order is cancelled"
	for name, c := range map[string]struct {
		do      func(tx pgx.Tx) error
		want    string
		trigger bool
	}{
		"its order not cancelled": {one(orderCancelledSQL, f.published), notCancelled, true},
		"a contract's RFQ":        {one(orderCancelledSQL, f.other), notCancelled, true},
		"a published RFQ":         {one(orderCancelledSQL, f.sourced), "stays published", true},
		"the award changed": {func(tx pgx.Tx) error {
			if err := exec(tx, cancelOrderSQL, f.order); err != nil {
				return err
			}
			return exec(tx, `UPDATE procurement.rfq SET status = 'order_cancelled', award_reason = NULL, version = version + 1 WHERE id = $1`, f.published)
		}, "stays awarded to its contract or purchase order", true},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, c.do), c.want)
			if c.trigger {
				failsWith(t, asApp(t, f.tenant, asOwner(c.do)), c.want)
			}
		})
	}
	t.Run("a draft", func(t *testing.T) {
		failsWith(t, asApp(t, f.tenant, one(orderCancelledSQL, f.draft)), "rfq_awarded_to_one_document")
	})
}

// TestRFQOrderCancelled: the move the database accepts, and what it keeps.
func TestRFQOrderCancelled(t *testing.T) {
	need(t)
	f := newOrderAwards(t)
	mustApp(t, f.tenant, orderAwarding(f.quotation, f.published, f.order, "Delivers in time"))
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, cancelOrderSQL, f.order); err != nil {
			return err
		}
		return exec(tx, orderCancelledSQL, f.published)
	})
	one := func(sql string, args ...any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error { return exec(tx, sql, args...) }
	}
	const stays = "with its reason or award"
	for name, do := range map[string]func(tx pgx.Tx) error{
		"awarded again":      one(`UPDATE procurement.rfq SET status = 'awarded', version = version + 1 WHERE id = $1`, f.published),
		"the order changed":  one(`UPDATE procurement.rfq SET purchase_order_id = $2, version = version + 1 WHERE id = $1`, f.published, f.usdOrder),
		"the reason dropped": one(`UPDATE procurement.rfq SET award_reason = NULL, version = version + 1 WHERE id = $1`, f.published),
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, f.tenant, do), stays)
			failsWith(t, asApp(t, f.tenant, asOwner(do)), stays)
		})
	}
}

// TestRFQOrderCancelledRevert (000371's down): the revert is refused while
// an RFQ's award's order was cancelled, which the previous version cannot
// show.
func TestRFQOrderCancelledRevert(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000371_rfq_award_order_cancelled").Down
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // the test only reads the refusal; nothing is kept
	tenant, rfq := id(), id()
	// The scene without the triggers (the foreign key's among them): an
	// RFQ already in the status.
	if err := all(tx, step(`SET LOCAL session_replication_role = replica`), step(rfqSQL, tenant, rfq, "RQ1", tenant, "EUR", "draft", tenant),
		step(`UPDATE procurement.rfq SET status = 'order_cancelled', purchase_order_id = $2 WHERE id = $1`, rfq, id()), step(`SET LOCAL session_replication_role = DEFAULT`)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "000371 cannot be reverted")
}
