package payables

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
)

// DocumentaryWarning says that a posted invoice's document may not show
// what its input tax's recovery requires (ADR-0049 §6): a warning, never a
// refusal, since the business may post and decide not to claim. It is
// recorded when the invoice posts, with the tier that asked for it.
type DocumentaryWarning struct {
	// Code is the requirement unmet: supplier_tax_id_missing or
	// item_description_missing.
	Code string `json:"code"`
	// Requirement is the tier's requirement (config.RequiresSupplierTaxID,
	// config.RequiresItemDescriptions).
	Requirement string `json:"requirement"`
	// Threshold is the tier's amount in minor units of Currency, the
	// company's; Inclusive whether a total equal to it is in the tier.
	Threshold money.Minor `json:"threshold"`
	Inclusive bool        `json:"inclusive"`
	Currency  string      `json:"currency"`
	Exponent  int         `json:"exponent"`
	// Source is the law the tier rests on; PendingSignOff that the tax
	// specialist has not yet confirmed the tier.
	Source         string `json:"source"`
	PendingSignOff bool   `json:"pending_sign_off,omitempty"`
}

// warningCodes are the warning codes by requirement.
var warningCodes = map[string]string{
	config.RequiresSupplierTaxID:    "supplier_tax_id_missing",
	config.RequiresItemDescriptions: "item_description_missing",
}

// Message is the warning for people: what is missing, from which total.
func (w DocumentaryWarning) Message() string {
	what := map[string]string{
		config.RequiresSupplierTaxID:    "The supplier has no tax identifier; its registration number must be on the document",
		config.RequiresItemDescriptions: "An item has no description; the document must describe each supply",
	}[w.Requirement]
	total := "of " + w.Threshold.Plain(w.Exponent) + " " + w.Currency + " or more"
	if !w.Inclusive {
		total = "above " + w.Threshold.Plain(w.Exponent) + " " + w.Currency
	}
	return what + " for the input tax of a document " + total + " to be recovered (" + w.Source + ")."
}

// documentaryWarnings are an invoice's warnings as it posts: of the
// company's documentary tiers (its tax settings) that the invoice's total,
// converted into the company's currency at the tax date's rate of the tax
// rate type, reaches, each requirement the invoice or its supplier does
// not meet, once, with the first tier naming it. Only an invoice or
// subsequent debit claiming input tax is checked: a credit takes tax back.
func (e *Endpoints) documentaryWarnings(ctx context.Context, v Invoice, sup partner.Supplier, taxDate time.Time) ([]DocumentaryWarning, error) {
	out := []DocumentaryWarning{}
	if v.Tax == 0 || (v.Kind != KindInvoice && v.Kind != KindSubsequentDebit) {
		return out, nil
	}
	le, _, err := e.LegalEntities.Entity(ctx, v.LegalEntityID) // a stored invoice's legal entity exists
	if err != nil {
		return nil, err
	}
	co, err := e.company(ctx, le.Code)
	if err != nil {
		return nil, err
	}
	settings := settingsOf(co)
	if len(settings.DocumentaryRequirements) == 0 {
		return out, nil
	}
	conv, err := e.taxConversion(ctx, v.Currency, le.LocalCurrency, taxDate, settings.RateTypeOrAverage())
	if err != nil {
		return nil, err
	}
	total, err := conv.Exact(v.Gross)
	if err != nil {
		return nil, errTaxTooLarge
	}
	return unmet(settings.DocumentaryRequirements, total, conv.ToExp, le.LocalCurrency, v, sup)
}

// unmet are the warnings of the tiers an invoice's total in the company's
// currency (exp digits) reaches: each requirement not met, once, with the
// first tier naming it.
func unmet(tiers []config.DocumentaryRequirement, total *big.Rat, exp int, currency string, v Invoice, sup partner.Supplier) ([]DocumentaryWarning, error) {
	out := []DocumentaryWarning{}
	for _, r := range tiers {
		amount, inclusive := r.Threshold()
		threshold, err := money.ParseDecimal(amount, exp) // validated with the company's currency's digits
		if err != nil {
			return nil, fmt.Errorf("payables: documentary threshold %q in %s: %w", amount, currency, err)
		}
		if c := total.Cmp(money.Rat(threshold, exp)); c < 0 || (c == 0 && !inclusive) {
			continue
		}
		for _, q := range r.Requires {
			if met(q, v, sup) || slices.ContainsFunc(out, func(w DocumentaryWarning) bool { return w.Requirement == q }) {
				continue
			}
			out = append(out, DocumentaryWarning{Code: warningCodes[q], Requirement: q, Threshold: threshold, Inclusive: inclusive,
				Currency: currency, Exponent: exp, Source: r.Source, PendingSignOff: r.PendingSignOff})
		}
	}
	return out, nil
}

// met says whether the invoice meets a documentary requirement as far as
// the ERP holds it: the supplier has a tax identifier; each item is
// described, by its own description or its order item's.
func met(requirement string, v Invoice, sup partner.Supplier) bool {
	if requirement == config.RequiresSupplierTaxID {
		return sup.HasTaxIdentifier
	}
	return !slices.ContainsFunc(v.Items, func(it Item) bool {
		return it.PurchaseOrderID == nil && (it.Description == nil || strings.TrimSpace(*it.Description) == "")
	})
}
