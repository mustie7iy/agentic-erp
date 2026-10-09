package capture

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/testkit"
)

// Model output is untrusted: any field values are either rejected or
// produce a parked/held invoice, never a panic or a posting.
func FuzzCapture(f *testing.F) {
	f.Add("Initech Components", "10-0000000", "INV-1", "2026-10-01", "", "USD", "x", "1", "10.00", "0.80", "10.80", "high")
	f.Add("", "", "", "", "4599999999", "EUR", "", "-1", "1e9", "", "", "low")
	s := testkit.Store(f)
	cfg := testkit.Config(f)
	journal := len(s.Journal)
	f.Fuzz(func(t *testing.T, name, tax, number, date, po, cur, desc, qty, amt, taxAmt, gross, conf string) {
		e := &Extraction{VendorName: name, VendorTaxID: tax, InvoiceNumber: number, InvoiceDate: date, PurchaseOrder: po, Currency: cur,
			Lines: []Line{{Description: desc, Quantity: qty, Amount: amt}}, TaxAmount: taxAmt, GrossAmount: gross, Confidence: conf}
		Capture(s, cfg, []byte(name+number+date+qty+amt), e, "fuzz", testkit.Now)
		if len(s.Journal) != journal {
			t.Fatal("capture must never post")
		}
	})
}
