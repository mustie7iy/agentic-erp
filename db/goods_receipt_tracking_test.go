package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestGoodsReceiptTracking (S2f1, S2g1; migrations 000473, 000474): a
// goods receipt's item names a batch by id and number together, or
// neither, and one to 1000 serial numbers, or none.
func TestGoodsReceiptTracking(t *testing.T) {
	need(t)
	price := int64(250)
	serials := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "G" + strings.Repeat("1", 1+i%5)
		}
		return out
	}
	batch, number := uuid.New(), "B1"
	for _, c := range []struct {
		name    string
		batch   *uuid.UUID
		number  *string
		serials []string
		want    string // empty: allowed
	}{
		{"neither", nil, nil, nil, ""},
		{"a batch by id and number", &batch, &number, nil, ""},
		{"serial numbers", nil, nil, []string{"G1", "G2"}, ""},
		{"1000 serial numbers", nil, nil, serials(1000), ""},
		{"a batch without its number", &batch, nil, nil, "goods_movement_item_batch"},
		{"a batch number without its batch", nil, &number, nil, "goods_movement_item_batch"},
		{"no serial numbers", nil, nil, []string{}, "goods_movement_item_serial_numbers"},
		{"1001 serial numbers", nil, nil, serials(1001), "goods_movement_item_serial_numbers"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				po, err := f.order(tx, &price)
				if err != nil {
					return err
				}
				if err := exec(tx, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, po); err != nil {
					return err
				}
				gm := uuid.Must(uuid.NewV7())
				if err := exec(tx, `INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, movement_type, document_date,
					posting_date, posted_by) VALUES ($1, $2, 'GR1', $3, '101', '2026-08-05', '2026-08-05', $4)`, f.tenant, gm, f.entity, f.actor); err != nil {
					return err
				}
				return exec(tx, `INSERT INTO procurement.goods_movement_item (tenant_id, goods_movement_id, item_no, purchase_order_id, po_item_no, quantity,
					uom_code, batch_id, batch_number, serial_numbers) VALUES ($1, $2, 1, $3, 1, 2, 'EA', $4, $5, $6)`, f.tenant, gm, po, c.batch, c.number, c.serials)
			})
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

// TestGoodsReceiptTrackingDownRefused: 000473's down migration refuses to
// drop goods receipt items naming a batch or serial numbers (06 §6.4: a
// down migration loses no data).
func TestGoodsReceiptTrackingDownRefused(t *testing.T) {
	need(t)
	ctx := context.Background()
	batch, number := uuid.New(), "B1"
	for name, c := range map[string]struct {
		batch   *uuid.UUID
		number  *string
		serials []string
	}{"a batch": {&batch, &number, nil}, "serial numbers": {nil, nil, []string{"G1"}}} {
		t.Run(name, func(t *testing.T) {
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant, gm := id(), id()
			// The rows are written with the triggers and foreign keys off
			// (its order item would be a fixture's), as the guard reads
			// only the item's columns.
			if err := all(tx, step(`SET LOCAL session_replication_role = replica`),
				step(`INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, movement_type, document_date,
					posting_date, posted_by) VALUES ($1, $2, 'GR1', $1, '101', '2026-08-05', '2026-08-05', $1)`, tenant, gm),
				step(`INSERT INTO procurement.goods_movement_item (tenant_id, goods_movement_id, item_no, purchase_order_id, po_item_no, quantity, uom_code,
					batch_id, batch_number, serial_numbers) VALUES ($1, $2, 1, $1, 1, 1, 'EA', $3, $4, $5)`, tenant, gm, c.batch, c.number, c.serials),
				step(`SET LOCAL session_replication_role = origin`)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, migrationNamed(t, "000473_goods_receipt_tracking").Down), "goods receipt items name batches or serial numbers")
		})
	}
}
