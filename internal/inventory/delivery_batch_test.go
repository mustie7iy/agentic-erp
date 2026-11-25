package inventory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestDeliveryBatchRefusals (S2f2, S2g2): a delivery's goods issue names
// the batch of a batch-managed material's item by a number of the
// material's, and none for another material, and the serial numbers of a
// serial-managed material's units; each refusal names the item's field,
// and nothing is posted. Shipping's tests post them.
func TestDeliveryBatchRefusals(t *testing.T) {
	e := batchSetup(t)
	other := e.batchMaterial(t, "Other sealant")
	e.batch(t, other, "L9", "2026-11-01", "")
	gauge := e.serialMaterial(t, "Pressure gauge")
	h := endpoints(t)
	item := func(material string, batch string, serials ...string) inventory.DeliveryItem {
		return inventory.DeliveryItem{MaterialID: uuid.MustParse(material), PlantID: e.p1, StorageLocationID: e.s1, Quantity: "1", Unit: "EA", Batch: batch,
			SerialNumbers: serials}
	}
	for name, c := range map[string]struct {
		items       []inventory.DeliveryItem
		code, field string
	}{
		"no batch":              {[]inventory.DeliveryItem{item(e.sealant, "")}, "inventory.batch_required", "items[0].batch_number"},
		"no such batch":         {[]inventory.DeliveryItem{item(e.sealant, "L1"), item(e.sealant, "L7")}, "inventory.batch_invalid", "items[1].batch_number"},
		"another's batch":       {[]inventory.DeliveryItem{item(e.sealant, "L9")}, "inventory.batch_invalid", "items[0].batch_number"},
		"a batch of no batch":   {[]inventory.DeliveryItem{item(e.sealant, "L1"), item(e.bracket, "L1")}, "inventory.batch_not_allowed", "items[1].batch_number"},
		"an unknown material":   {[]inventory.DeliveryItem{item(uuid.NewString(), "L1")}, "inventory.material_invalid", "items[0].material_id"},
		"no serial numbers":     {[]inventory.DeliveryItem{item(gauge, "")}, "inventory.serial_required", "items[0].serial_numbers"},
		"serials of no profile": {[]inventory.DeliveryItem{item(e.sealant, "L1", "G1")}, "inventory.serial_not_allowed", "items[0].serial_numbers"},
	} {
		t.Run(name, func(t *testing.T) {
			var p *problem.Problem
			err := e.inRequest(t, func(ctx context.Context) error {
				_, err := h.IssueForDelivery(ctx, "OD1", c.items)
				return err
			})
			if !errors.As(err, &p) || p.Code != c.code || len(p.Errors) != 1 || p.Errors[0].Field != c.field {
				t.Errorf("got %v, want %s naming %s", err, c.code, c.field)
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM inventory.material_document WHERE tenant_id = $1 AND movement_type = '601'`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("goods issues posted: got %d, want none", n)
	}
}
