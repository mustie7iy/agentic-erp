package payments

import "testing"

// TestCounterpartyAccount: the other side's account is its IBAN, or its
// account number where the bank gives no IBAN (camt.053 Othr/Id).
func TestCounterpartyAccount(t *testing.T) {
	for name, c := range map[string]struct {
		d    LineDetail
		want string
	}{
		"an IBAN":           {LineDetail{CounterpartyIBAN: "GB33BUKB20201555555555"}, "GB33BUKB20201555555555"},
		"an account number": {LineDetail{CounterpartyAccount: "12345678"}, "12345678"},
		"none":              {LineDetail{}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := c.d.counterpartyAccount(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
