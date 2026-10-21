package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// taxBase is a line's declared base (NewLine.TaxBase), parsed: a tax line
// of an import code posted from a customs declaration names the base its
// tax is on, which no line of the entry carries (T61e, ADR-0049 §9).
func taxBase(l NewLine, i, expTC int) (*money.Minor, error) {
	if l.TaxBase == nil {
		return nil, nil
	}
	field := fmt.Sprintf("lines[%d].tax_base", i)
	if l.TaxBaseCurrency != l.Currency {
		return nil, problem.Unprocessable("ledger.currency_mismatch", "Every line amount must be in the entry's currency "+l.Currency+".",
			problem.FieldError{Field: field + ".currency", Code: "mismatch", Message: l.Currency})
	}
	base, err := money.ParseAmount(*l.TaxBase, expTC)
	if err != nil {
		return nil, problem.BadRequest("request.invalid_amount", fmt.Sprintf("Amounts in %s have exactly %d decimals.", l.Currency, expTC),
			problem.FieldError{Field: field + ".amount", Code: "invalid", Message: err.Error()})
	}
	if base == 0 {
		return nil, problem.Unprocessable("ledger.zero_tax_base", "A declared tax base is never zero.",
			problem.FieldError{Field: field, Code: "zero", Message: "the declared value the tax is on"})
	}
	return &base, nil
}

// checkTaxBases holds each line declaring a tax base to being a tax line
// of an import code (one on a component's account; 422
// ledger.tax_base_not_import) whose amount is the tax on that base at the
// component's rate on the tax date, as the calculator gives it, within one
// minor unit (422 ledger.tax_base_mismatch): the input part recovered, or
// the self-assessed output tax.
func checkTaxBases(ctx context.Context, le uuid.UUID, date time.Time, lines []Line, expTC int) error {
	for i, l := range lines {
		if l.TaxBase == nil {
			continue
		}
		t, k, err := importComponent(ctx, le, date, l)
		if err != nil {
			return err
		}
		if k < 0 {
			return problem.Unprocessable("ledger.tax_base_not_import", fmt.Sprintf("Line %d declares a tax base, which only a tax line of an import "+
				"code (one on a component's account) does: an import's tax posted from the customs declaration.", i+1),
				problem.FieldError{Field: fmt.Sprintf("lines[%d].tax_base", i), Code: "not_import", Message: "a tax line of an import code"})
		}
		want, err := onAccount(t, k, *l.TaxBase, date)
		if err != nil {
			return err
		}
		if d := l.TC - want; d > 1 || d < -1 {
			return problem.Unprocessable("ledger.tax_base_mismatch", fmt.Sprintf("Line %d posts %s of %s tax on its declared base; the rate gives %s.",
				i+1, l.TC.Plain(expTC), t.Code, want.Plain(expTC)),
				problem.FieldError{Field: fmt.Sprintf("lines[%d].amount", i), Code: "tax_base_mismatch", Message: "the tax on the declared base"})
		}
	}
	return nil
}

// importComponent is the import code a line carries, with the index of
// its component posting to the line's account; -1 when the line is no tax
// line of an import code.
func importComponent(ctx context.Context, le uuid.UUID, date time.Time, l Line) (TaxCode, int, error) {
	if l.TaxCode == nil {
		return TaxCode{}, -1, nil
	}
	t, _, err := Accounts{}.TaxCode(ctx, le, *l.TaxCode, date) // checkTaxCodes found it valid on the date
	if err != nil || t.Kind != "import" {
		return t, -1, err
	}
	for k, c := range t.Components {
		if c.AccountID != nil && *c.AccountID == l.AccountID {
			return t, k, nil
		}
	}
	return t, -1, nil
}

// onAccount is what the code's component k puts on its account for a
// base: the self-assessed output tax, or the input part recovered. The
// calculator's items are the code's components in order.
func onAccount(t TaxCode, k int, base money.Minor, date time.Time) (money.Minor, error) {
	res, err := tax.Calculate(tax.Document{TaxDate: date, Codes: map[string]tax.Code{t.Code: t.Calculable()}, Lines: []tax.Line{{Amount: base, Code: t.Code}}})
	if err != nil {
		return 0, fmt.Errorf("ledger: tax on a declared base of %s: %w", t.Code, err)
	}
	if it := res.Items[k]; it.AccountKey == tax.KeySelfOutput {
		return it.Tax, nil
	}
	return res.Items[k].Recoverable, nil
}
