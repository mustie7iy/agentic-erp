package db

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// Account assignment on requisition and order items (T56e, migrations
// 000161-000162): an item assigned to a cost center names it, and only
// such an item names one; the assignment is none or a cost center. A
// released order's item keeps it (TestOrderChangeApplication).
func TestItemAccountAssignment(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name, set, want string
	}{
		{"a cost center assignment with its cost center", `account_assignment = 'cost_center', cost_center_id = gen_random_uuid()`, ""},
		{"none without one", `account_assignment = 'none', cost_center_id = NULL`, ""},
		{"a cost center assignment without one", `account_assignment = 'cost_center'`, "account_assignment"},
		{"none with a cost center", `cost_center_id = gen_random_uuid()`, "account_assignment"},
		{"another assignment", `account_assignment = 'order'`, "account_assignment"},
	} {
		for table, item := range map[string]func(f fixture, tx pgx.Tx) error{
			"requisition_item": func(f fixture, tx pgx.Tx) error {
				r := id()
				if err := exec(tx, `INSERT INTO procurement.requisition (tenant_id, id, document_number, legal_entity_id, requester_id, currency, status,
					created_by, updated_by) VALUES ($1, $2, $3, $4, $5, 'USD', 'draft', $5, $5)`, f.tenant, r, r.String()[24:], f.entity, f.actor); err != nil {
					return err
				}
				if err := exec(tx, `INSERT INTO procurement.requisition_item (tenant_id, requisition_id, item_no, description, quantity, uom_code, unit_price,
					account_id) VALUES ($1, $2, 1, 'Paper', 1, 'EA', 100, gen_random_uuid())`, f.tenant, r); err != nil {
					return err
				}
				return exec(tx, `UPDATE procurement.requisition_item SET `+c.set+` WHERE requisition_id = $1`, r)
			},
			"purchase_order_item": func(f fixture, tx pgx.Tx) error {
				po, err := f.order(tx, nil)
				if err != nil {
					return err
				}
				return exec(tx, `UPDATE procurement.purchase_order_item SET `+c.set+` WHERE purchase_order_id = $1`, po)
			},
		} {
			t.Run(table+": "+c.name, func(t *testing.T) {
				f := newFixture(t)
				err := asApp(t, f.tenant, func(tx pgx.Tx) error { return item(f, tx) })
				if c.want != "" {
					failsWith(t, err, table+"_"+c.want)
					return
				}
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
			})
		}
	}
}
