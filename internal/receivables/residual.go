package receivables

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Partial payments (ADR-0063 §6 As built, T75f, T75o): an item an
// incoming payment pays in part leaves a residual item for what stays
// open, or has it written off, as the company's reason for it says; a
// difference within the company's payment tolerance is written off. The
// ledger posts them (ledger.Postings.ApplyReceipt).

// ResidualInput is what of an item paid in part stays open, or is written
// off, as a client sends it: the amount as a decimal in a currency, the
// reason, whose treatment decides which (T75o), and an optional free
// text.
type ResidualInput struct {
	Value, Currency string
	Reason          string
	Text            *string
}

// reasonRule is a payment-difference reason as an application reads it
// (T75o): written off or left open, and the id of the account a write-off
// goes to, uuid.Nil when the company's chart has no account of the number
// the configuration names.
type reasonRule struct {
	writeOff bool
	account  uuid.UUID
}

// differenceRules are what decides an application's payment differences:
// the company's tolerance (companies[].receivables.payment_tolerance, SAP
// OBA3), its amounts in minor units of the company's currency, none
// allowing no difference, and its reasons by code
// (companies[].receivables.payment_difference_reasons, SAP OBBE), with
// their codes in the configuration's order for the refusal that lists them.
type differenceRules struct {
	under, over config.ToleranceLimit
	reasons     map[string]reasonRule
	codes       []string
}

// residualsOf reads the residuals of n items in the receipt's currency
// with its exp minor-unit digits: nil when no item has one, else one per
// item, zero for an item paid in full. A residual is more than zero (422
// receivables.amount_not_positive; the ledger holds it under the item),
// in the receipt's currency (422 receivables.currency_mismatch), with one
// of the company's reasons (422 receivables.unknown_residual_reason) and a
// free text that is not white space alone. A reason that writes off (T75o)
// writes the residual off to its account, which the company's chart must
// still have (422 receivables.no_write_off_account). Its line's text says
// what it is and why.
func residualsOf(in []*ResidualInput, n int, code string, exp int, rules differenceRules) ([]ledger.Residual, error) {
	var out []ledger.Residual
	for i, r := range in {
		if r == nil {
			continue
		}
		residual, err := residualOf(fmt.Sprintf("items[%d].residual", i), r, code, exp, rules)
		if err != nil {
			return nil, fmt.Errorf("receivables: residual of item %d: %w", i+1, err)
		}
		if out == nil {
			out = make([]ledger.Residual, n)
		}
		out[i] = residual
	}
	return out, nil
}

// residualOf reads one item's residual, its refusals naming field.
func residualOf(field string, r *ResidualInput, code string, exp int, rules differenceRules) (ledger.Residual, error) {
	if r.Currency != code {
		return ledger.Residual{}, problem.Invalid("receivables.currency_mismatch", field+".amount.currency", "A residual item is in the receipt's currency, "+code+".")
	}
	amount, err := currency.ParseAmount(r.Value, code, exp, field+".amount.amount")
	if err != nil {
		return ledger.Residual{}, err
	}
	if amount <= 0 {
		return ledger.Residual{}, problem.Invalid("receivables.amount_not_positive", field+".amount.amount", "A residual item is more than zero.")
	}
	rule, ok := rules.reasons[r.Reason]
	switch {
	case !ok:
		return ledger.Residual{}, problem.Invalid("receivables.unknown_residual_reason", field+".reason",
			"A residual's reason is one of the company's payment-difference reasons: "+strings.Join(rules.codes, ", ")+".")
	case rule.writeOff && rule.account == uuid.Nil:
		return ledger.Residual{}, problem.Invalid("receivables.no_write_off_account", field+".reason",
			"The reason writes the difference off to an account the company's chart of accounts does not have.")
	}
	text := "Residual item, " + r.Reason
	if rule.writeOff {
		text = "Written off, " + r.Reason
	}
	if r.Text != nil {
		if err := problem.RequireText(field+".reason_text", *r.Text); err != nil {
			return ledger.Residual{}, err
		}
		text += ": " + *r.Text
	}
	return ledger.Residual{Amount: amount, Text: &text, WriteOff: rule.account}, nil
}

// differenceRules reads the company's tolerance, in minor units of its
// currency, and its payment-difference reasons, each write-off's account
// found by its number in the company.
func (e *Endpoints) differenceRules(ctx context.Context, tx pgx.Tx, le tenancy.Entity) (differenceRules, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return differenceRules{}, fmt.Errorf("receivables: payment differences of company %s: %w", le.Code, err)
	}
	exp, err := currency.Exponent(ctx, tx, le.LocalCurrency, nil)
	if err != nil {
		return differenceRules{}, fmt.Errorf("receivables: payment differences of company %s: %w", le.Code, err)
	}
	co := doc.Company(le.Code)
	rules := differenceRules{reasons: map[string]reasonRule{}}
	rules.under, rules.over = co.Tolerance().Limits(exp)
	var numbers []string
	for _, r := range co.DifferenceReasons() {
		if r.Account != "" {
			numbers = append(numbers, r.Account)
		}
	}
	ids, err := e.References.Accounts(ctx, le.ID, numbers)
	if err != nil {
		return differenceRules{}, fmt.Errorf("receivables: write-off accounts of company %s: %w", le.Code, err)
	}
	for _, r := range co.DifferenceReasons() {
		rules.codes = append(rules.codes, r.Code)
		rules.reasons[r.Code] = reasonRule{writeOff: r.Treatment == config.TreatmentWriteOff, account: ids[r.Account]}
	}
	return rules, nil
}
