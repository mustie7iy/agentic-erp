package payables

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// TestStatedTaxRefusals: a difference that would recover more than a
// component's tax, or of the other sign, is a mismatch; one whose local
// amount is out of range is too large.
func TestStatedTaxRefusals(t *testing.T) {
	one := ledger.Conversion{Rate: "1", FromExp: 2, ToExp: 2}
	half := []tax.ComponentTax{{Code: "M5", Key: "tax", AccountKey: tax.KeyInput, Amounts: tax.Amounts{Base: 10, Tax: 2, Recoverable: 1, NonRecoverable: 1}}}
	_, _, err := statedTax(Invoice{Currency: "EUR", Exponent: 2}, half, 2, one)
	if got := problem.From(err); got == nil || got.Code != "payables.tax_mismatch" {
		t.Errorf("recovering more than the tax: got %v, want payables.tax_mismatch", err)
	}
	large := money.Minor(1) << 62
	apart := []tax.ComponentTax{{Code: "V1", Key: "tax", AccountKey: tax.KeyInput, Amounts: tax.Amounts{Tax: large, Recoverable: large}},
		{Code: "V7", Key: "tax", AccountKey: tax.KeyInput, Amounts: tax.Amounts{Tax: large, Recoverable: large}}}
	_, _, err = statedTax(Invoice{Currency: "EUR", Exponent: 2}, apart, 1, one)
	if got := problem.From(err); got == nil || got.Code != "payables.amount_too_large" {
		t.Errorf("a charged total out of range: got %v, want payables.amount_too_large", err)
	}
	huge := money.Minor(1) << 60
	whole := []tax.ComponentTax{{Code: "V1", Key: "tax", AccountKey: tax.KeyInput, Amounts: tax.Amounts{Base: huge, Tax: huge, Recoverable: huge}}}
	_, _, err = statedTax(Invoice{Currency: "EUR", Exponent: 2, Tax: huge + 1}, whole, 1, ledger.Conversion{Rate: "100", FromExp: 2, ToExp: 2})
	if got := problem.From(err); got == nil || got.Code != "payables.amount_too_large" {
		t.Errorf("a local amount out of range: got %v, want payables.amount_too_large", err)
	}
}

// TestCalculationFailed: amounts out of range are the invoice's 422; the
// calculator's other errors are wrapped.
func TestCalculationFailed(t *testing.T) {
	if got := problem.From(calculationFailed(uuid.Nil, fmt.Errorf("tax: line 1: %w", money.ErrOverflow))); got == nil || got.Code != "payables.amount_too_large" {
		t.Errorf("overflow: got %v, want payables.amount_too_large", got)
	}
	if err := calculationFailed(uuid.Nil, tax.ErrInvalidCode); !errors.Is(err, tax.ErrInvalidCode) {
		t.Errorf("an invalid code: got %v, want it wrapped", err)
	}
}

// TestToleranceOf: a company the configuration does not list has the
// default tolerance, one minor unit per code.
func TestToleranceOf(t *testing.T) {
	if got := toleranceOf(nil); got != 1 {
		t.Errorf("without the company: got %d, want 1", got)
	}
	units := 3
	if got := toleranceOf(&config.Company{InvoiceTolerance: config.Tolerance{TaxUnitsPerCode: &units}}); got != 3 {
		t.Errorf("the company's: got %d, want 3", got)
	}
}
