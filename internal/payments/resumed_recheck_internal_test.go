package payments

import (
	"context"
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

// TestResumedReleaseRechecksNothing: a releasing run being resumed is
// not refused for its items or requests (dropPayment drops what cannot
// post); the checks read nothing, which a context without ledger or
// payables proves.
func TestResumedReleaseRechecksNothing(t *testing.T) {
	e := &Endpoints{}
	r := Run{Status: Releasing}
	pays := []Payment{{Kind: KindDownPayment, RequestID: new(uuid.UUID)}}
	if err := e.recheckItems(context.Background(), r, pays); err != nil {
		t.Errorf("recheckItems: got %v, want nil", err)
	}
	if err := e.recheckRequests(context.Background(), r, pays); err != nil {
		t.Errorf("recheckRequests: got %v, want nil", err)
	}
}
