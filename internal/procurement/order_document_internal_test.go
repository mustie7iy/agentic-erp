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

// TestOrderDocumentDeliveryDates: an item prints its own delivery date;
// the requisition's needed-by date is printed for the items that have
// none, qualified when others have one, and not at all when every item
// has its own (ADR-0054 As built).
func TestOrderDocumentDeliveryDates(t *testing.T) {
	day := func(month time.Month, d int) *time.Time {
		t := time.Date(2026, month, d, 0, 0, 0, 0, time.UTC)
		return &t
	}
	pens := func(no int32, delivery *time.Time) Item {
		return Item{No: no, Description: "Pens", Quantity: "1", Unit: "EA", UnitPrice: 100, PriceUnit: 1, DeliveryDate: delivery}
	}
	const (
		item1 = "  Item 1: Pens\n    1 EA at 1.00 EUR per 1 EA\n    Amount 1.00 EUR\n"
		item2 = "  Item 2: Pens\n    1 EA at 1.00 EUR per 1 EA\n    Amount 1.00 EUR\n"
	)
	for _, c := range []struct {
		name     string
		neededBy *time.Time
		items    []Item
		terms    string // the line after the payment terms' line
		section  string // the items section
	}{
		{"every item dated", day(4, 1), []Item{pens(1, day(4, 10)), pens(2, day(4, 20))}, "\nItems\n",
			item1 + "    Delivery date 2026-04-10\n" + item2 + "    Delivery date 2026-04-20\n"},
		{"no item dated", day(4, 1), []Item{pens(1, nil), pens(2, nil)}, "Needed by: 2026-04-01\n", item1 + item2},
		{"mixed", day(4, 1), []Item{pens(1, day(4, 10)), pens(2, nil)},
			"Needed by: 2026-04-01, for the items without a delivery date of their own\n", item1 + "    Delivery date 2026-04-10\n" + item2},
		{"no requisition date", nil, []Item{pens(1, day(4, 10)), pens(2, nil)}, "\nItems\n", item1 + "    Delivery date 2026-04-10\n" + item2},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := orderDocument{order: PurchaseOrder{DocumentNumber: "4500000001", Revision: 1, OrderDate: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),
				Currency: "EUR", Exponent: 2, Status: Released, Items: c.items}, neededBy: c.neededBy}
			got, err := d.render()
			if err != nil {
				t.Fatal(err)
			}
			if want := "Payment terms:  \n" + c.terms; !strings.Contains(got, want) {
				t.Errorf("document: got\n%s\nwant it to contain %q", got, want)
			}
			if want := "\nItems\n" + c.section + "\nTotal "; !strings.Contains(got, want) {
				t.Errorf("document: got\n%s\nwant it to contain %q", got, want)
			}
		})
	}
}
