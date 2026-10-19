package payables

import (
	"context"
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// withholding are the withholding taxes of the supplier's codes in the
// invoice's legal entity (T46f, ADR-0037), each the code's rate of the
// invoice's net (its items) or gross, in the invoice's direction, and the
// lines crediting what codes deducted at invoice withhold to their
// accounts; withheld is their total, which the supplier is owed less of.
// What all the codes withhold, at invoice and at payment, is less than the
// gross, so the supplier is always paid something (422
// payables.withholding_exceeds_invoice); each code's amount is less than
// the gross (its rate is below 100%), so the running rest never wraps.
func (e *Endpoints) withholding(ctx context.Context, v Invoice, sup partner.Supplier) (ws []ledger.Withholding, lines []ledger.PostingLine, withheld money.Minor,
	err error) {
	left := v.Gross // what the codes leave the supplier, at invoice and at payment
	for _, code := range sup.WithholdingTaxCodes {
		c, ok, err := e.Accounts.WithholdingTaxCode(ctx, v.LegalEntityID, code)
		if err != nil {
			return nil, nil, 0, err
		}
		if !ok {
			return nil, nil, 0, problem.Unprocessable("payables.withholding_tax_code_unknown",
				fmt.Sprintf("The supplier's withholding tax code %s is not an active code of the legal entity.", code))
		}
		base := v.Gross
		if c.Base != config.WithholdingGross {
			base = v.Gross - v.Tax
		}
		amount, err := money.Scale(base, c.RatePct, 100)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("payables: withholding tax %s of invoice %s: %w", code, v.ID, err)
		}
		if left -= amount; amount != 0 && left <= 0 {
			return nil, nil, 0, problem.Unprocessable("payables.withholding_exceeds_invoice",
				"The supplier's withholding tax codes would withhold the whole invoice or more.")
		}
		w := ledger.Withholding{Code: code, Posting: c.Posting, RatePct: c.RatePct, AccountID: c.AccountID, Base: signed(v, base)}
		if c.Posting == config.WithholdingAtInvoice {
			w.Amount, withheld = signed(v, amount), withheld+amount
			if amount != 0 {
				text := "Withholding tax " + code
				lines = append(lines, ledger.PostingLine{AccountID: c.AccountID, Amount: -signed(v, amount), Text: &text})
			}
		}
		ws = append(ws, w)
	}
	return ws, lines, withheld, nil
}
