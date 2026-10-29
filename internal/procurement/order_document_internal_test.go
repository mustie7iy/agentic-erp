package procurement

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestOrderDocumentRender covers what the API tests reach only through
// order changes or stored data the API refuses: a later revision, and
// items whose amounts are out of range.
func TestOrderDocumentRender(t *testing.T) {
	d := orderDocument{order: PurchaseOrder{DocumentNumber: "4500000001", Revision: 3, OrderDate: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),
		Currency: "JPY", Exponent: 0, Status: Released, Items: []Item{{No: 1, Description: "Pens", Quantity: "7", Unit: "EA", UnitPrice: 120, PriceUnit: 1}}}}
	got, err := d.render()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Revision 3 (replaces revision 2), order date 2026-03-10, currency JPY\n", "    7 EA at 120 JPY per 1 EA\n", "\nTotal 840 JPY\n"} {
		if !strings.Contains(got, s) {
			t.Errorf("document: got\n%s\nwant it to contain %q", got, s)
		}
	}
	d.order.Items = append(d.order.Items, Item{No: 2, Description: "Gold", Quantity: "99999999999999", Unit: "EA", UnitPrice: math.MaxInt64 / 2, PriceUnit: 1})
	_, err = d.render()
	pgtest.WantErr(t, err, "procurement.amount_too_large")
}
