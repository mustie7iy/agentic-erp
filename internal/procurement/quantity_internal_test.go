package procurement

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestQuantity: quantities reach procurement through the API, whose
// contract pattern bounds them, and from migrated SAP documents, which do
// not pass that pattern. Both are held to the column's grammar: a positive
// plain decimal that numeric(20,6) stores exactly. A fraction, an exponent
// or a seventh decimal is 422, not a database error or a silent rounding.
func TestQuantity(t *testing.T) {
	for _, q := range []string{"1/3", "1e3", "0.1234567", "123456789012345", "0", "-1", "abc"} {
		t.Run(q, func(t *testing.T) {
			pgtest.WantErr(t, quantity(q, "items[0].quantity"), "procurement.invalid_quantity")
		})
	}
	for _, q := range []string{"12.5", "0.000001", "99999999999999.999999", "1.0000000"} {
		t.Run(q, func(t *testing.T) {
			if err := quantity(q, "items[0].quantity"); err != nil {
				t.Errorf("got %v, want accepted", err)
			}
		})
	}
}
