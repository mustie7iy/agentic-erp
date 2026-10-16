package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The release strategy (T47d, migration 000063): an order moves only along
// its lifecycle, names the request it waits for exactly while it waits,
// and is valued as it leaves draft.
func TestOrderTransitions(t *testing.T) {
	need(t)
	const submit = "status = 'pending_approval', release_request_id = gen_random_uuid()"
	for _, c := range []struct {
		name  string
		after []string
		set   string
		want  string // empty: allowed
	}{
		{"submitted", nil, submit, ""},
		{"released at once", nil, "status = 'released'", ""},
		{"approved", []string{submit}, "status = 'released', release_request_id = NULL", ""},
		{"approved through GR/IR", []string{submit}, "status = 'released', release_request_id = NULL, valuation = 'gr_ir'", ""},
		{"rejected or withdrawn", []string{submit}, "status = 'draft', release_request_id = NULL", ""},
		{"cancelled while pending", []string{submit}, "status = 'cancelled', release_request_id = NULL", ""},
		{"released naming its request", []string{submit}, "status = 'released'", "purchase_order_release_request_pending"},
		{"pending without a request", nil, "status = 'pending_approval'", "purchase_order_release_request_pending"},
		{"a draft naming a request", nil, "release_request_id = gen_random_uuid()", "purchase_order_release_request_pending"},
		{"released back to draft", []string{"status = 'released'"}, "status = 'draft'", "cannot go from released to draft"},
		{"closed released again", []string{"status = 'released'", "status = 'closed'"}, "status = 'released'", "cannot go from closed to released"},
		{"cancelled submitted", []string{"status = 'cancelled'"}, submit, "cannot go from cancelled to pending_approval"},
		{"pending closed", []string{submit}, "status = 'closed', release_request_id = NULL", "cannot go from pending_approval to closed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			var po uuid.UUID
			mustApp(t, f.tenant, func(tx pgx.Tx) (err error) { po, err = f.order(tx, nil); return err })
			change := func(set string) error {
				return asApp(t, f.tenant, func(tx pgx.Tx) error {
					return exec(tx, `UPDATE procurement.purchase_order SET `+set+`, version = version + 1 WHERE id = $1`, po)
				})
			}
			for _, set := range c.after {
				if err := change(set); err != nil {
					t.Fatalf("%s: %v", set, err)
				}
			}
			err := change(c.set)
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	// Items are valued as the order is released, not as it is submitted: a
	// release rejected and the order changed is valued at its new price.
	t.Run("valued as it is released", func(t *testing.T) {
		f := newFixture(t)
		var po uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) { po, err = f.order(tx, nil); return err })
		valuation := func() *int64 {
			var valued *int64
			if err := testDB.QueryRow(context.Background(), `SELECT valuation_price FROM procurement.purchase_order_item WHERE purchase_order_id = $1`, po).
				Scan(&valued); err != nil {
				t.Fatal(err)
			}
			return valued
		}
		set := func(sql string) {
			mustApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, sql, po) })
		}
		set(`UPDATE procurement.purchase_order SET ` + submit + `, version = version + 1 WHERE id = $1`)
		if v := valuation(); v != nil {
			t.Errorf("valuation price once submitted: got %v, want none", *v)
		}
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order_item SET quantity = 5 WHERE purchase_order_id = $1`, po)
		}), "cannot change")
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.purchase_order_item SET valuation_price = 250, valuation_price_unit = 1, quantity = 5
				WHERE purchase_order_id = $1`, po)
		}), "cannot change")
		set(`UPDATE procurement.purchase_order SET status = 'draft', release_request_id = NULL, version = version + 1 WHERE id = $1`)
		set(`UPDATE procurement.purchase_order_item SET unit_price = 100 WHERE purchase_order_id = $1`)
		set(`UPDATE procurement.purchase_order SET ` + submit + `, version = version + 1 WHERE id = $1`)
		set(`UPDATE procurement.purchase_order SET status = 'released', release_request_id = NULL, version = version + 1 WHERE id = $1`)
		if v := valuation(); v == nil || *v != 100 {
			t.Errorf("valuation price once released: got %v, want 100", deref(v))
		}
	})
	t.Run("pending valued through GR/IR", func(t *testing.T) {
		f := newFixture(t)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
				payment_term_code, status, valuation, release_request_id, created_by, updated_by)
				VALUES ($1, gen_random_uuid(), 'P1', $2, $3, 'USD', '2026-08-01', 'NT30', 'pending_approval', 'gr_ir', gen_random_uuid(), $4, $4)`,
				f.tenant, f.entity, f.supplier, f.actor)
		}), "purchase_order_draft_direct")
	})
}
