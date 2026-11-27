package integration

import "testing"

// TestQuantity (S1a3): SAP's quantities are written as the platform keeps
// a measure, zero and empty as none; what is no decimal, has more than
// three decimals or ten whole digits is refused.
func TestQuantity(t *testing.T) {
	for in, want := range map[string]struct {
		out string
		ok  bool
	}{
		"": {"", true}, " ": {"", true}, "0": {"", true}, "0.000": {"", true}, "007.50": {"7.5", true}, "1.2340": {"1.234", true},
		"0.5": {"0.5", true}, "18": {"18", true}, "1000.000": {"1000", true}, "9999999999.999": {"9999999999.999", true},
		"12345678901": {"", false}, "1.2345": {"", false}, "1e3": {"", false}, "-1": {"", false}, "1,5": {"", false}, ".5": {"", false},
	} {
		t.Run(in, func(t *testing.T) {
			got, ok := quantity(in)
			if got != want.out || ok != want.ok {
				t.Errorf("got %q %v, want %q %v", got, ok, want.out, want.ok)
			}
		})
	}
}
