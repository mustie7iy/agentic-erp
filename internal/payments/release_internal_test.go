package payments

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// TestItemReason: a payment with an item reversed is excluded as
// item_reversed, one whose items are only no longer open as item_not_open.
func TestItemReason(t *testing.T) {
	a, b := ledger.ItemRef{EntryID: uuid.New()}, ledger.ItemRef{EntryID: uuid.New()}
	for name, c := range map[string]struct {
		unpayable map[ledger.ItemRef]string
		want      string
	}{
		"reversed":         {map[ledger.ItemRef]string{a: ledger.ItemReversed}, ItemReversed},
		"not open":         {map[ledger.ItemRef]string{a: ledger.ItemNotOpen}, ItemNotOpen},
		"one of each":      {map[ledger.ItemRef]string{a: ledger.ItemNotOpen, b: ledger.ItemReversed}, ItemReversed},
		"none (defensive)": {nil, ItemNotOpen},
	} {
		t.Run(name, func(t *testing.T) {
			if got := itemReason(c.unpayable); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
