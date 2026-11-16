package db

import (
	"context"
	"testing"
)

// TestProcessMiningSchema (T103a, migrations 000339-000341; ADR-0080):
// reading event logs is a permission of its own and no approval
// permission; the context has no schema, and the orders and the supplier
// documents on no order of a company are indexed by date and id, the
// documents' index holding only those without an order on their header.
func TestProcessMiningSchema(t *testing.T) {
	need(t)
	ctx := context.Background()
	var approval bool
	if err := testDB.QueryRow(ctx, `SELECT is_approval FROM ref.permission WHERE code = 'processmining.log.read'`).Scan(&approval); err != nil || approval {
		t.Errorf("the permission: got approval %v, %v, want it seeded and no approval permission", approval, err)
	}
	var schemas int
	if err := testDB.QueryRow(ctx, `SELECT count(*) FROM pg_namespace WHERE nspname = 'processmining'`).Scan(&schemas); err != nil || schemas != 0 {
		t.Errorf("the schema: got %d, %v, want none", schemas, err)
	}
	for name, want := range map[string]string{
		"purchase_order_by_date":         "CREATE INDEX purchase_order_by_date ON procurement.purchase_order USING btree (tenant_id, legal_entity_id, order_date, id)",
		"supplier_invoice_without_order": "CREATE INDEX supplier_invoice_without_order ON payables.supplier_invoice USING btree (tenant_id, legal_entity_id, invoice_date, id) WHERE (purchase_order_id IS NULL)",
	} {
		var def string
		if err := testDB.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = $1`, name).Scan(&def); err != nil || def != want {
			t.Errorf("index %s: got %q, %v, want %q", name, def, err, want)
		}
	}
}
