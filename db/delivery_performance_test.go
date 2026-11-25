package db

import (
	"context"
	"testing"
)

// TestDeliveryPerformanceIndexes (S2l1, migrations 000469-000470;
// ADR-0082 As built S2l1): the suppliers' delivery performance finds a
// material's order items and a receipt's reversal by index, each partial
// to the rows that have one, and both are valid (built concurrently, a
// failed build would leave one invalid).
func TestDeliveryPerformanceIndexes(t *testing.T) {
	need(t)
	ctx := context.Background()
	for name, want := range map[string]string{
		"purchase_order_item_by_material": "CREATE INDEX purchase_order_item_by_material ON procurement.purchase_order_item USING btree (tenant_id, material_id) " +
			"WHERE (material_id IS NOT NULL)",
		"goods_movement_by_reversal": "CREATE INDEX goods_movement_by_reversal ON procurement.goods_movement USING btree (tenant_id, reversal_of_id) " +
			"WHERE (reversal_of_id IS NOT NULL)",
	} {
		t.Run(name, func(t *testing.T) {
			var def string
			var valid bool
			if err := testDB.QueryRow(ctx, `SELECT i.indexdef, x.indisvalid FROM pg_indexes i JOIN pg_class c ON c.relname = i.indexname AND c.relnamespace = 'procurement'::regnamespace
				JOIN pg_index x ON x.indexrelid = c.oid WHERE i.schemaname = 'procurement' AND i.indexname = $1`, name).Scan(&def, &valid); err != nil {
				t.Fatalf("got %v, want the index", err)
			}
			if def != want {
				t.Errorf("definition: got %q, want %q", def, want)
			}
			if !valid {
				t.Error("valid: got false, want true")
			}
		})
	}
}
