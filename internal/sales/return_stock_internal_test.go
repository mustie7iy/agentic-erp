package sales

import (
	"errors"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestAsReceipt (S2b4): a refusal of inventory's, which numbers the stock
// lines alone, names the receipt's own item and field: its quantity's
// value, its item for a material or an amount, its storage location for
// a plant; a field of no line is kept, and an error that is no problem is
// passed on.
func TestAsReceipt(t *testing.T) {
	lines := []stockLine{{at: 1}, {at: 2}}
	for _, c := range []struct{ in, want string }{
		{"items[0].quantity", "items[1].quantity.value"},
		{"items[1].quantity", "items[2].quantity.value"},
		{"items[1].material_id", "items[2].item_no"},
		{"items[0].amount", "items[1].item_no"},
		{"items[1].plant_id", "items[2].storage_location_id"},
		{"items[1].batch_number", "items[2].batch_number"},
		{"items", "items"},
	} {
		t.Run(c.in, func(t *testing.T) {
			got := problem.From(asReceipt(problem.Invalid("inventory.x", c.in, "refused"), lines))
			if len(got.Errors) != 1 || got.Errors[0].Field != c.want {
				t.Errorf("got %v, want the field %s", got.Errors, c.want)
			}
		})
	}
	plain := errors.New("down")
	if err := asReceipt(plain, lines); !errors.Is(err, plain) {
		t.Errorf("an error that is no problem: got %v, want it as it was", err)
	}
}
