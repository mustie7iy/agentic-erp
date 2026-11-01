package payables

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
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
	// Unassessed says that the invoice's total could not be converted into
	// the company's currency on the tax date (no rate of the tax rate
	// type), so the tier may not apply: the requirement is reported for a
	// person to check rather than the posting refused.
	Unassessed bool `json:"unassessed,omitempty"`
}

// warningCodes are the warning codes by requirement.
var warningCodes = map[string]string{
	config.RequiresSupplierTaxID:    "supplier_tax_id_missing",
	config.RequiresItemDescriptions: "item_description_missing",
}

// Message is the warning for people: what is missing, from which total.
func (w DocumentaryWarning) Message() string {
	what := map[string]string{
		config.RequiresSupplierTaxID:    "The supplier has no tax identifier of the company's country; its registration number must be on the document",
		config.RequiresItemDescriptions: "An item has no description; the document must describe each supply",
	}[w.Requirement]
	total := "of " + w.Threshold.Plain(w.Exponent) + " " + w.Currency + " or more"
	if !w.Inclusive {
		total = "above " + w.Threshold.Plain(w.Exponent) + " " + w.Currency
	}
	msg := what + " for the input tax of a document " + total + " to be recovered (" + w.Source + ")."
	if w.Unassessed {
		msg += " The invoice's total could not be converted into " + w.Currency + " on its tax date, so check whether the tier applies."
	}
	return msg
}

// documentaryWarnings are an invoice's warnings as it posts: of the
// company's documentary tiers (its tax settings) that the invoice's total,
// converted into the company's currency at the tax date's rate of the tax
// rate type, reaches, each requirement the invoice or its supplier does
// not meet, once, with the first tier naming it. Only an invoice or
// subsequent debit claiming input tax is checked: a credit takes tax back.
// A total that cannot be converted (no rate of the type on the tax date)
// never refuses the posting (ADR-0049 §6): every tier is then taken as
// reached and its warnings marked unassessed.
func (e *Endpoints) documentaryWarnings(ctx context.Context, tx pgx.Tx, v Invoice, sup partner.Supplier, taxDate time.Time) ([]DocumentaryWarning, error) {
	if v.Tax == 0 || (v.Kind != KindInvoice && v.Kind != KindSubsequentDebit) {
		return []DocumentaryWarning{}, nil
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
		return []DocumentaryWarning{}, nil
	}
	total, exp, err := e.localTotal(ctx, tx, v, le.LocalCurrency, taxDate, settings.RateTypeOrAverage())
	if err != nil {
		return nil, err
	}
	return unmet(settings.DocumentaryRequirements, total, exp, le.LocalCurrency, v, sup, le.CountryCode)
}

// localTotal is the invoice's gross amount in the company's currency at
// the tax date's rate of the rate type, with that currency's digits; nil
// when no rate converts it.
func (e *Endpoints) localTotal(ctx context.Context, tx pgx.Tx, v Invoice, local string, taxDate time.Time, rateType string) (*big.Rat, int, error) {
	conv, err := e.taxConversion(ctx, v.Currency, local, taxDate, rateType)
	if p := problem.From(err); p != nil && p.Code == "ledger.no_exchange_rate" {
		exp, err := currencyExponent(ctx, tx, local, "currency")
		return nil, exp, err
	}
	if err != nil {
		return nil, 0, err
	}
	total, err := conv.Exact(v.Gross)
	if err != nil {
		return nil, conv.ToExp, nil //nolint:nilerr // a rate that cannot convert leaves the tiers unassessed, never refuses (ADR-0049 §6)
	}
	return total, conv.ToExp, nil
}

// unmet are the warnings of the tiers an invoice's total in the company's
// currency (exp digits) reaches, every tier when the total is nil
// (unassessed): each requirement not met, once, with the first tier
// naming it.
func unmet(tiers []config.DocumentaryRequirement, total *big.Rat, exp int, currency string, v Invoice, sup partner.Supplier,
	country string) ([]DocumentaryWarning, error) {
	out := []DocumentaryWarning{}
	for _, r := range tiers {
		amount, inclusive := r.Threshold()
		threshold, err := money.ParseDecimal(amount, exp) // validated with the company's currency's digits
		if err != nil {
			return nil, fmt.Errorf("payables: documentary threshold %q in %s: %w", amount, currency, err)
		}
		if total != nil {
			if c := total.Cmp(money.Rat(threshold, exp)); c < 0 || (c == 0 && !inclusive) {
				continue
			}
		}
		for _, q := range r.Requires {
			if met(q, v, sup, country) || slices.ContainsFunc(out, func(w DocumentaryWarning) bool { return w.Requirement == q }) {
				continue
			}
			out = append(out, DocumentaryWarning{Code: warningCodes[q], Requirement: q, Threshold: threshold, Inclusive: inclusive,
				Currency: currency, Exponent: exp, Source: r.Source, PendingSignOff: r.PendingSignOff, Unassessed: total == nil})
		}
	}
	return out, nil
}

// met says whether the invoice meets a documentary requirement as far as
// the ERP holds it: the supplier has a tax identifier of the company's
// country (a scheme named after it, CA_..., as the SAP load names them
// CC_TAX), not one of another country's (a US vendor's EIN is no GST/HST
// number); each item is described, by its own description or its order
// item's.
func met(requirement string, v Invoice, sup partner.Supplier, country string) bool {
	if requirement == config.RequiresSupplierTaxID {
		return slices.ContainsFunc(sup.TaxSchemes, func(s string) bool { return strings.HasPrefix(s, country+"_") })
	}
	return !slices.ContainsFunc(v.Items, func(it Item) bool {
		return it.PurchaseOrderID == nil && (it.Description == nil || strings.TrimSpace(*it.Description) == "")
	})
}
