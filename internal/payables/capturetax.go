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
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// capturedTax proposes the tax codes of a captured document and flags its
// stated tax (ADR-0049 §6, T61d): a line printing a tax rate its order
// item's code does not charge gets the one input code of the legal entity
// that charges it on the tax date, a warning saying so, or, with none or
// several, a reason to hold the invoice for a person to choose; and a
// stated tax the codes' rates do not bear out within the tolerance is a
// reason to hold it (payables.tax_mismatch's figures). It posts nothing:
// the invoice is entered parked or held as ever (ADR-0036).
func (e *Endpoints) capturedTax(ctx context.Context, tx pgx.Tx, in *Input, ext *capture.Extraction) ([]string, string, error) {
	order, _, err := e.Orders.Order(ctx, *in.PurchaseOrderID) // capturedInput read it
	if err != nil {
		return nil, "", err
	}
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
		code := orderItemCode(order.Items, *it.POItemNo)
		if printed := ext.Lines[i].TaxRate; printed != "" {
			proposed, warning, reason := proposeCode(codes, code, printed, i+1)
			if proposed != nil {
				code, in.Items[i].TaxCode = proposed, proposed
			}
			warnings, held = appendNonEmpty(warnings, warning), appendNonEmpty(held, reason)
		}
		// Captured amounts were read with the currency's digits (printed).
		amount, _ := money.ParseDecimal(it.Amount.Value, exp)
		v.Items = append(v.Items, Item{Amount: amount, TaxCode: code})
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

// orderItemCode is the tax code of an order item, which an invoice item on
// it inherits.
func orderItemCode(items []procurement.OrderItem, no int32) *string {
	for _, oi := range items {
		if oi.ItemNo == no {
			return oi.TaxCode
		}
	}
	return nil
}

// proposeCode is the code a line printing a tax rate gets: none when its
// order item's code charges that rate; else the one input code that does,
// with a warning; else a reason to hold the invoice.
func proposeCode(codes []ledger.TaxCode, current *string, printed string, line int) (code *string, warning, reason string) {
	rate := money.ParseRat(printed)
	if rate == nil {
		return nil, "", fmt.Sprintf("Line %d prints the tax rate %q, which is not a number; choose its tax code.", line, printed)
	}
	var matching []string
	for _, c := range codes {
		if c.Direction != config.TaxInput || charged(c).Cmp(rate) != 0 {
			continue
		}
		if current != nil && c.Code == *current {
			return nil, "", ""
		}
		matching = append(matching, c.Code)
	}
	switch len(matching) {
	case 1:
		return &matching[0], fmt.Sprintf("line %d: tax code %s proposed for the printed rate of %s%%", line, matching[0], printed), ""
	case 0:
		return nil, "", fmt.Sprintf("Line %d prints a tax rate of %s%%, which no input tax code charges; choose its tax code.", line, printed)
	}
	return nil, "", fmt.Sprintf("Line %d prints a tax rate of %s%%, which codes %s charge; choose its tax code.", line, printed, strings.Join(matching, ", "))
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
