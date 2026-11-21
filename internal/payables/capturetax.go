package payables

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// printedTax is the tax a document's line states: its rate, a percent as
// a decimal ("" when it prints none), and, on an e-invoice, its VAT
// category (UN/CEFACT 5305, IBT-151; "" on a captured document, which
// does not read one).
type printedTax struct{ rate, category string }

// printedRates are the rates a captured document's lines print, without
// a category.
func printedRates(ext *capture.Extraction) []printedTax {
	out := make([]printedTax, len(ext.Lines))
	for i, l := range ext.Lines {
		out[i] = printedTax{rate: l.TaxRate}
	}
	return out
}

// capturedTax proposes the tax codes of a captured document and flags its
// stated tax (ADR-0049 §6, T61d): a line printing a tax rate its order
// item's code does not charge gets the one input code of the legal entity
// that charges it on the tax date, a warning saying so, or, with none or
// several, a reason to hold the invoice for a person to choose; and a
// stated tax the codes' rates do not bear out within the tolerance is a
// reason to hold it (payables.tax_mismatch's figures). A line stating its
// category (an e-invoice's, ADR-0061 §6) is matched among the codes of
// that category alone, so the order item's code stands only when it has
// the line's category and charges its rate. It posts nothing: the invoice
// is entered parked or held as ever (ADR-0036).
func (e *Endpoints) capturedTax(ctx context.Context, tx pgx.Tx, in *Input, lines []printedTax) ([]string, string, error) {
	taxDate, err := taxDateOf(ctx, tx, nil, in.InvoiceDate, in.ReferenceInvoiceID)
	if err != nil {
		return nil, "", err
	}
	codes, err := e.Accounts.TaxCodes(ctx, in.LegalEntityID, taxDate)
	if err != nil {
		return nil, "", err
	}
	exp, err := currency.Exponent(ctx, tx, in.Gross.Currency, nil) // the order's currency, which capturedInput read
	if err != nil {
		return nil, "", fmt.Errorf("payables: capture: %w", err)
	}
	v := Invoice{LegalEntityID: in.LegalEntityID, Currency: in.Gross.Currency, Exponent: exp, Kind: in.Kind, InvoiceDate: in.InvoiceDate,
		ReferenceInvoiceID: in.ReferenceInvoiceID}
	var warnings, held []string
	for i, it := range in.Items {
		if printed := lines[i]; printed.rate != "" {
			proposed, warning, reason := proposeCode(codes, it.TaxCode, printed, i+1)
			if proposed != nil {
				in.Items[i].TaxCode = proposed
			}
			warnings, held = appendNonEmpty(warnings, warning), appendNonEmpty(held, reason)
		}
		// Captured amounts were read with the currency's digits (printed).
		amount, _ := money.ParseDecimal(it.Amount.Value, exp)
		v.Items = append(v.Items, Item{Amount: amount, TaxCode: in.Items[i].TaxCode})
	}
	v.Tax, _ = money.ParseDecimal(in.Tax.Value, exp) // as the items'
	if _, err := e.calculateTax(ctx, v, taxDate); err != nil {
		p := problem.From(err)
		if p == nil || p.Code != "payables.tax_mismatch" {
			return nil, "", err
		}
		held = append(held, p.Detail)
	}
	return warnings, strings.Join(held, " "), nil
}

// proposeCode is the code a line printing a tax rate gets: none when its
// order item's code charges that rate; else the one input code that does,
// with a warning; else a reason to hold the invoice. A line stating its
// category considers the codes of that category only. An adjustment code
// is a manual entry's tax alone (ADR-0049 §11), never a document's line, so
// it is not proposed for a printed 0%.
func proposeCode(codes []ledger.TaxCode, current *string, line printedTax, no int) (code *string, warning, reason string) {
	printed := line.rate
	rate := money.ParseRat(printed)
	if rate == nil {
		return nil, "", fmt.Sprintf("Line %d prints the tax rate %q, which is not a number; choose its tax code.", no, printed)
	}
	var matching []string
	for _, c := range codes {
		if c.Direction != config.TaxInput || c.Kind == config.TaxAdjustment || (line.category != "" && c.Category != line.category) ||
			charged(c).Cmp(rate) != 0 {
			continue
		}
		if current != nil && c.Code == *current {
			return nil, "", ""
		}
		matching = append(matching, c.Code)
	}
	rated := printed + "%"
	if line.category != "" {
		rated += " in VAT category " + line.category
	}
	switch len(matching) {
	case 1:
		return &matching[0], fmt.Sprintf("line %d: tax code %s proposed for the printed rate of %s", no, matching[0], rated), ""
	case 0:
		return nil, "", fmt.Sprintf("Line %d prints a tax rate of %s, which no input tax code charges; choose its tax code.", no, rated)
	}
	return nil, "", fmt.Sprintf("Line %d prints a tax rate of %s, which codes %s charge; choose its tax code.", no, rated, strings.Join(matching, ", "))
}

// charged is the rate a code charges: its components' rates but the
// self-assessed ones, which the supplier does not charge.
func charged(c ledger.TaxCode) *big.Rat {
	sum := new(big.Rat)
	for _, k := range c.Components {
		if k.AccountKey == tax.KeySelfInput || k.AccountKey == tax.KeySelfOutput {
			continue
		}
		if r := money.ParseRat(k.RatePct); r != nil { // the ledger's numeric(9,6): a decimal
			sum.Add(sum, r)
		}
	}
	return sum
}

// appendNonEmpty appends s when it is not empty.
func appendNonEmpty(list []string, s string) []string {
	if s == "" {
		return list
	}
	return append(list, s)
}
