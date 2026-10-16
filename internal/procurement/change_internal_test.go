package procurement

import "testing"

// TestReceivedFailsClosed: the received checks of an order change read
// quantities the order select and orderItems produce; one that does not
// parse counts as received, so a change is refused rather than passed.
func TestReceivedFailsClosed(t *testing.T) {
	ok := Item{Quantity: "10", OverDelivery: "10"}
	for name, c := range map[string]struct {
		it       Item
		received string
		want     bool
	}{
		"within the tolerance": {ok, "11", false},
		"above the tolerance":  {ok, "11.000001", true},
		"quantity unparsed":    {Item{Quantity: "x", OverDelivery: "0"}, "0", true},
		"tolerance unparsed":   {Item{Quantity: "1", OverDelivery: "x"}, "0", true},
		"received unparsed":    {ok, "x", true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := belowReceived(c.it, c.received); got != c.want {
				t.Errorf("belowReceived: got %v, want %v", got, c.want)
			}
		})
	}
	for received, want := range map[string]bool{"0": false, "0.5": true, "": true} {
		t.Run("receipts "+received, func(t *testing.T) {
			if got := hasReceipts(Item{Received: received}); got != want {
				t.Errorf("hasReceipts(%q): got %v, want %v", received, got, want)
			}
		})
	}
}
