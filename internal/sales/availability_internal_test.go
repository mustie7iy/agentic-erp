package sales

import (
	"math/big"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestConfirm: an item is confirmed as far as its material is available,
// in its own unit rounded down to six decimals, never below what it had
// confirmed (up to its quantity) nor above its quantity, and what it
// promises and has not delivered is counted out of what is available. A
// material of which a promise cannot be valued, its unit no longer one of
// the material's, has nothing available; an item in such a unit keeps
// what it had.
func TestConfirm(t *testing.T) {
	m := materials.Material{ID: uuid.New(), BaseUnit: "EA", Units: []materials.Unit{{Code: "PAK", Numerator: 3, Denominator: 1}}}
	confirmed := func(q string) *string { return &q }
	for _, c := range []struct {
		name                 string
		free                 string
		unknown              bool
		item                 OrderItem
		want, wantLeft       string
		wantUnknownAfterTake bool
	}{
		{"all of it", "10", false, OrderItem{Item: Item{Quantity: "4", Unit: "EA"}, Delivered: "0"}, "4", "6", false},
		{"part, rounded down in the item's unit", "1", false, OrderItem{Item: Item{Quantity: "2", Unit: "PAK"}, Delivered: "0"}, "0.333333", "0.000001", false},
		{"nothing available", "-5", false, OrderItem{Item: Item{Quantity: "2", Unit: "EA"}, Delivered: "0"}, "0", "-5", false},
		{"an increase keeps what was confirmed", "0", false, OrderItem{Item: Item{Quantity: "9", Unit: "EA"}, Delivered: "1", Confirmed: confirmed("3")},
			"3", "-2", false},
		{"a decrease to below what was confirmed", "0", false, OrderItem{Item: Item{Quantity: "2", Unit: "EA"}, Delivered: "1", Confirmed: confirmed("3")},
			"2", "-1", false},
		{"delivered counts as reached", "2", false, OrderItem{Item: Item{Quantity: "9", Unit: "EA"}, Delivered: "4", Confirmed: confirmed("4")}, "6", "0", false},
		{"a promise of the material could not be valued", "10", true, OrderItem{Item: Item{Quantity: "2", Unit: "EA"}, Delivered: "0"}, "0", "10", true},
		{"its own unit no longer the material's", "10", false, OrderItem{Item: Item{Quantity: "5", Unit: "BOX"}, Delivered: "0", Confirmed: confirmed("1")},
			"1", "10", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := stockLeft{left: map[uuid.UUID]*big.Rat{m.ID: money.ParseRat(c.free)}, unknown: map[uuid.UUID]bool{m.ID: c.unknown}}
			c.item.MaterialID = &m.ID
			p := s.confirm(c.item, m)
			if p.confirmed != c.want {
				t.Errorf("confirmed: got %s, want %s", p.confirmed, c.want)
			}
			if left := money.PlainQuantity(s.left[m.ID]); left != c.wantLeft {
				t.Errorf("left: got %s, want %s", left, c.wantLeft)
			}
			if p.unvalued != (c.item.Unit == "BOX") {
				t.Errorf("unvalued: got %v, want it only for a unit the material lacks", p.unvalued)
			}
			if s.unknown[m.ID] != c.wantUnknownAfterTake {
				t.Errorf("unknown: got %v, want %v", s.unknown[m.ID], c.wantUnknownAfterTake)
			}
		})
	}
}
