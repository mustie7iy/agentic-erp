package payables_test

import (
	"context"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCapturedQuantity: a captured line's quantity comes from the model
// unchecked, so it is held to the column's grammar: a positive plain
// decimal of at most 6 decimals and 14 integer digits, numeric(20,6). A
// fraction or an exponent is refused with 422 rather than reaching the
// database (22P02) or being read as a number it does not print, and a
// seventh decimal is refused rather than rounded away.
func TestCapturedQuantity(t *testing.T) {
	e := setup(t)
	e.order(t, e.Supplier, "PO-1")
	capture := func(t *testing.T, quantity, ref string) error {
		t.Helper()
		x := extraction()
		x.InvoiceNumber, x.Lines[0].Quantity = ref, quantity
		return apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := endpoints(extractor{e: x}).CaptureInvoice(ctx, e.DE, []byte("%PDF-1 "+ref))
			return err
		})
	}
	for _, q := range []string{"1/3", "1e3", "0.1234567", "123456789012345", "0.000000", "-2"} {
		t.Run(q, func(t *testing.T) {
			pgtest.WantErr(t, capture(t, q, "CAP-Q "+q), "payables.invalid_quantity")
		})
	}
	for _, q := range []string{"99999999999999.999999", "0.000001", "2.5000000"} {
		t.Run(q, func(t *testing.T) {
			if err := capture(t, q, "CAP-Q "+q); err != nil {
				t.Errorf("got %v, want the invoice captured", err)
			}
		})
	}
}
