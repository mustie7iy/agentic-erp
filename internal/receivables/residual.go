package receivables

import (
	"context"
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Partial payments (ADR-0063 §6 As built, T75f): an item an incoming
// payment pays in part leaves a residual item for what stays open, with a
// reason; a difference within the company's payment tolerance is written
// off. The ledger posts both (ledger.Postings.ApplyReceipt).

// ResidualInput is what stays open of an item paid in part, as a client
// sends it: the amount as a decimal in a currency, the reason and an
// optional free text.
type ResidualInput struct {
	Value, Currency string
	Reason          string
	Text            *string
}

// residualReasons are the reasons a residual item is left for (an open
// enumeration of the contract).
var residualReasons = map[string]bool{"short_payment": true, "disputed": true, "deduction": true, "other": true}

// residualsOf reads the residuals of n items in the receipt's currency
// with its exp minor-unit digits: nil when no item has one, else one per
// item, zero for an item paid in full. A residual is more than zero (422
// receivables.amount_not_positive; the ledger holds it under the item),
// in the receipt's currency (422 receivables.currency_mismatch), with a
// known reason (422 receivables.unknown_residual_reason) and a free text
// that is not white space alone. Its line's text says it is a residual and
// why.
func residualsOf(in []*ResidualInput, n int, code string, exp int) ([]ledger.Residual, error) {
	var out []ledger.Residual
	for i, r := range in {
		if r == nil {
			continue
		}
		field := fmt.Sprintf("items[%d].residual", i)
		if r.Currency != code {
			return nil, problem.Invalid("receivables.currency_mismatch", field+".amount.currency", "A residual item is in the receipt's currency, "+code+".")
		}
		amount, err := currency.ParseAmount(r.Value, code, exp, field+".amount.amount")
		if err != nil {
			return nil, fmt.Errorf("receivables: residual of item %d: %w", i+1, err)
		}
		if amount <= 0 {
			return nil, problem.Invalid("receivables.amount_not_positive", field+".amount.amount", "A residual item is more than zero.")
		}
		if !residualReasons[r.Reason] {
			return nil, problem.Invalid("receivables.unknown_residual_reason", field+".reason",
				"A residual item's reason is one of short_payment, disputed, deduction and other.")
		}
		text := "Residual item, " + r.Reason
		if r.Text != nil {
			if err := problem.RequireText(field+".reason_text", *r.Text); err != nil {
				return nil, err
			}
			text += ": " + *r.Text
		}
		if out == nil {
			out = make([]ledger.Residual, n)
		}
		out[i] = ledger.Residual{Amount: amount, Text: &text}
	}
	return out, nil
}

// tolerance is the company's payment tolerance (companies[].receivables.
// payment_tolerance, SAP OBA3), its amounts in minor units of the
// company's currency; a company without one allows no difference.
func (e *Endpoints) tolerance(ctx context.Context, le tenancy.Entity) (under, over config.ToleranceLimit, err error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return under, over, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return under, over, fmt.Errorf("receivables: payment tolerance of company %s: %w", le.Code, err)
	}
	exp, err := currency.Exponent(ctx, tx, le.LocalCurrency, nil)
	if err != nil {
		return under, over, fmt.Errorf("receivables: payment tolerance of company %s: %w", le.Code, err)
	}
	under, over = doc.Company(le.Code).Tolerance().Limits(exp)
	return under, over, nil
}
