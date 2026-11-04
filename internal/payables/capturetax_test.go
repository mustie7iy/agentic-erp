package payables_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestCapturedTax: the capture proposes tax codes from the rates a document
// prints and flags its stated tax (ADR-0049 §6, T61d): a rate the order
// item's code charges keeps it; a rate one input code charges proposes it,
// with a warning; a rate no code or several codes charge, or one that is
// no number, holds the invoice for a person to choose; a stated tax the
// codes' rates do not bear out holds it with the figures.
func TestCapturedTax(t *testing.T) {
	e := setup(t)
	inputCodes(t, e)
	e.order(t, e.Supplier, "PO-1")
	for name, c := range map[string]struct {
		toner, tax, gross string // the toner line's printed rate, the stated tax and gross
		status, codes     string // the invoice's status and its items' codes
		reason, warning   string
	}{
		"the order's code":    {"", "22.80", "157.80", "parked", "V1 ", "", ""},
		"a proposed code":     {"10", "24.30", "159.30", "parked", "V1 R9", "", "tax code R9 proposed for the printed rate of 10%"},
		"two codes charging":  {"7", "22.80", "157.80", "on_hold", "V1 ", "which codes P7, V7 charge", ""},
		"no code charging":    {"13", "22.80", "157.80", "on_hold", "V1 ", "which no input tax code charges", ""},
		"no number":           {"x", "22.80", "157.80", "on_hold", "V1 ", "which is not a number", ""},
		"a stated tax beyond": {"", "30.00", "165.00", "on_hold", "V1 ", "beyond the company's tolerance", ""},
	} {
		t.Run(name, func(t *testing.T) {
			x := extraction()
			x.InvoiceNumber, x.TaxAmount, x.GrossAmount = "CAP-"+strings.ReplaceAll(name, " ", "-"), c.tax, c.gross
			x.Lines = []capture.Line{{Description: "Paper", Quantity: "10", Amount: "120.00", TaxRate: "19"}, {Description: "Toner", Quantity: "5",
				Amount: "15.00", TaxRate: c.toner}}
			var res gen.CaptureSupplierInvoiceRes
			if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
				res, err = endpoints(extractor{e: x}).CaptureSupplierInvoice(ctx, &gen.SupplierInvoiceCaptureMultipart{LegalEntityID: e.DE,
					Document: gen.NewOptString("%PDF-1 " + name)}, gen.CaptureSupplierInvoiceParams{})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			op := res.(*gen.OperationHeaders).Response
			inv, _ := e.in(t).Must(200, "GET", op.Resource.Value, "", "")
			codes := ""
			for _, it := range inv["items"].([]any) {
				code, _ := it.(map[string]any)["tax_code"].(string)
				codes += code + " "
			}
			if inv["status"] != c.status || strings.TrimSpace(codes) != strings.TrimSpace(c.codes) {
				t.Errorf("status and codes: got %v %q, want %s %q", inv["status"], codes, c.status, c.codes)
			}
			if r, _ := inv["hold_reason"].(string); !strings.Contains(r, c.reason) {
				t.Errorf("hold reason: got %q, want %q in it", r, c.reason)
			}
			if w := fmt.Sprint(op.Result); !strings.Contains(w, c.warning) {
				t.Errorf("warnings: got %s, want %q among them", w, c.warning)
			}
		})
	}
}

// TestCapturedAdjustmentCodeNotProposed: an adjustment code charges 0%
// like an exempt one, but it is a manual entry's tax alone (ADR-0049 §11,
// T61j), so a line printing 0% is held naming the codes a document's line
// may carry at that rate (the exempt V0 and the reverse charge codes,
// which the supplier does not charge), not the adjustment code A0.
func TestCapturedAdjustmentCodeNotProposed(t *testing.T) {
	e := setup(t)
	fixture.TaxCodes(t, e.Tenant, inputCodes(t, e), config.TaxCode{Code: "A0", Description: "Adjustment", Kind: config.TaxAdjustment,
		Components: []config.TaxComponent{{Key: "tax", AccountKey: config.KeyInput, Account: "140000", Rates: []config.TaxRate{rate("0", "2026-01-01", "")}}}})
	e.order(t, e.Supplier, "PO-1")
	x := extraction()
	x.Lines[1].TaxRate = "0"
	var res gen.CaptureSupplierInvoiceRes
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		res, err = endpoints(extractor{e: x}).CaptureSupplierInvoice(ctx, &gen.SupplierInvoiceCaptureMultipart{LegalEntityID: e.DE,
			Document: gen.NewOptString("%PDF-1 adjustment")}, gen.CaptureSupplierInvoiceParams{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	inv, _ := e.Must(200, "GET", res.(*gen.OperationHeaders).Response.Resource.Value, "", "")
	if got, want := inv["hold_reason"], "Line 2 prints a tax rate of 0%, which codes RC, RH, V0 charge; choose its tax code."; got != want {
		t.Errorf("hold reason: got %v, want %s", got, want)
	}
}

// TestCapturedCodeNotCalculable: a code the capture proposes that the
// calculator cannot calculate on the tax date (half a reverse charge pair
// then, the only input code charging the printed 0%) refuses the capture
// with 422 payables.invalid_tax_code, as a code that is no input tax code
// then does (payables.unknown_tax_code), not a 500.
func TestCapturedCodeNotCalculable(t *testing.T) {
	e := setup(t)
	doc := fixture.Document(t)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "240000", Name: "Output tax", Type: config.Liability})
	fixture.TaxCodes(t, e.Tenant, doc, halfPair("RX"))
	// V0, exempt, charges 0% too: withdrawn, RX is the one code that does.
	pgtest.Exec(t, `UPDATE ledger.tax_rate SET withdrawn_at = now(), withdrawn_by = $2 WHERE tenant_id = $1 AND code = 'V0'`, e.Tenant.ID, e.Tenant.Human.ID)
	e.order(t, e.Supplier, "PO-1")
	x := extraction()
	x.Lines[1].TaxRate = "0"
	err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints(extractor{e: x}).CaptureSupplierInvoice(ctx, &gen.SupplierInvoiceCaptureMultipart{LegalEntityID: e.DE,
			Document: gen.NewOptString("%PDF-1 half")}, gen.CaptureSupplierInvoiceParams{})
		return err
	})
	if p := problem.From(err); p == nil || p.Code != "payables.invalid_tax_code" || !strings.Contains(p.Detail, "RX") {
		t.Errorf("got %v, want 422 payables.invalid_tax_code naming RX", err)
	}
}
