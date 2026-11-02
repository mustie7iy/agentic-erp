// Package tax calculates sales tax (VAT, GST/HST, QST, PST) on a
// document's lines (T61b, ADR-0049 §2, §8, §9): the built-in calculator
// behind the Calculator port, pure Go with no database, like
// internal/money. Amounts are money.Minor and rates exact decimals, never
// floats (ADR-0005).
//
// Tax is rounded once per document, tax code and component, half away
// from zero, and allocated to the lines by largest remainder
// (money.Allocate), so the lines' tax adds up to the document's; a
// country whose law or practice wants it rounds per line instead
// (RoundLine). A tax-inclusive line (a
// captured gross, a down payment) is split as amount × rate / (100 +
// rate). Built from an exclusive document's bases and charged tax, an
// inclusive one gives back each base within two minor units and, rounded
// per document, a one-component code's tax exactly; otherwise each item's
// tax within one unit per line. Each component's tax is split into a
// recoverable and a non-recoverable part by its fixed recoverable share
// (decision P8). A
// reverse charge or import code's self_output and self_input components
// self-assess the tax: the input side is the tax at the rate, the output
// side its negation, so the pair nets to zero and the partner is owed the
// base only.
//
// Which code a line carries is the caller's (a person, a rule or the
// capture agent proposes it); the calculator determines nothing from the
// partner or the places of supply, which an external engine behind the
// same port would (T61m).
package tax

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Calculator is the port a document's tax is calculated through (ADR-0049
// §2): BuiltIn for the pilot countries, an external engine's connector
// later.
type Calculator interface {
	Calculate(ctx context.Context, doc Document) (Result, error)
}

// BuiltIn is the in-house calculator (decision P1).
type BuiltIn struct{}

// Calculate implements Calculator with Calculate; it does no I/O.
func (BuiltIn) Calculate(_ context.Context, doc Document) (Result, error) {
	return Calculate(doc)
}

// Rounding modes: once per document, code and component (the default), or
// per line.
type Rounding string

const (
	RoundDocument Rounding = "document"
	RoundLine     Rounding = "line"
)

// Account keys, the values of config's and the ledger's (SAP MWS, VST,
// NVV, ESA/ESE): where a component's tax posts.
const (
	KeyInput         = "input"
	KeyOutput        = "output"
	KeyNonDeductible = "non_deductible"
	KeySelfOutput    = "self_output"
	KeySelfInput     = "self_input"
)

// Kinds of code whose rate is always 0: the base is recorded for the
// return, no tax is charged.
const (
	KindZero       = "zero"
	KindExempt     = "exempt"
	KindOutOfScope = "out_of_scope"
)

// Errors the calculator reports, wrapped with the line or code; amounts
// out of range are money.ErrOverflow.
var (
	ErrUnknownCode = errors.New("tax: unknown tax code")
	ErrNoRate      = errors.New("tax: no rate in force on the tax date")
	ErrInvalidCode = errors.New("tax: invalid tax code")
	ErrInvalid     = errors.New("tax: invalid document")
)

// Reason is the calculator's error in words, for a problem's detail: its
// message without the package's prefix, naming the code and, for a
// missing rate, the component and the date.
func Reason(err error) string {
	return strings.TrimPrefix(err.Error(), "tax: ")
}

// Document is what tax is calculated on. The partner and place fields are
// for an engine that determines tax (T61m); the built-in calculator reads
// the codes the lines carry.
type Document struct {
	Company        string
	PartnerCountry string // ISO 3166-1
	PartnerRegion  string // ISO 3166-2 subdivision, "" when none
	ShipFrom       string // ISO 3166-2 or -1
	ShipTo         string
	// TaxDate decides each component's rate (ADR-0049 §4).
	TaxDate  time.Time
	Currency string
	// Inclusive says the lines' amounts include the tax charged (a gross);
	// otherwise they are the base.
	Inclusive bool
	Rounding  Rounding // "" is RoundDocument
	// Codes are the tax codes the lines name, by code.
	Codes map[string]Code
	Lines []Line
	// Local, when given, converts the results to the local currency.
	Local *Conversion
}

// Line is one taxable line: its amount in minor units (the base, or the
// gross when the document is inclusive; negative for a credit) and its
// tax code. TaxClass and Quantity are for an engine.
type Line struct {
	Amount   money.Minor
	Code     string
	TaxClass string
	Quantity string
}

// Conversion converts transaction-currency amounts to the local currency
// at Rate (a plain decimal, local per transaction unit, of the rate type
// and date the country names), from FromExp to ToExp minor-unit digits.
type Conversion struct {
	Rate    string
	FromExp int
	ToExp   int
}

// Code is a tax code with its components and their dated rates, as the
// ledger keeps them (ledger.tax_code_definition, tax_component, tax_rate).
type Code struct {
	Code       string
	Kind       string // standard, zero, exempt, out_of_scope, reverse_charge or import
	Components []Component
}

// Component is one tax a code levies (GST, BC PST).
type Component struct {
	Key          string
	Jurisdiction string
	AccountKey   string // one of the Key constants
	// RecoverablePct is the recoverable share, a plain decimal from 0 to 100.
	RecoverablePct string
	Rates          []Rate
}

// Rate is a percentage (a plain decimal such as "9.975") in force from
// From (zero: always) to before Until (zero: open), the ledger's
// tax_rate dates. A caller that resolved a code on the tax date passes
// that one rate undated.
type Rate struct {
	From  time.Time
	Until time.Time
	Pct   string
}

// Result is a document's tax: per line, and per code and component (the
// tax items, ADR-0049 §5), whose amounts are the sums of the lines'.
type Result struct {
	Lines []LineTax
	Items []ComponentTax
}

// LineTax is one line's tax: its base (an inclusive line's gross less the
// tax charged in it) and each component of its code.
type LineTax struct {
	Base       money.Minor
	Components []ComponentTax
}

// ComponentTax is a component's tax on a line or on the document.
type ComponentTax struct {
	Code         string
	Key          string
	Jurisdiction string
	AccountKey   string
	RatePct      string // the rate applied, as the code gives it
	Amounts
	// Local is Amounts in the local currency when Document.Local is
	// given: an item's base and tax converted once, its lines adding up to
	// them; recoverable parts the lines' shares of their local tax.
	Local Amounts
}

// Amounts are a base and its tax, the tax split into recoverable and
// non-recoverable parts (Recoverable + NonRecoverable = Tax).
type Amounts struct {
	Base           money.Minor
	Tax            money.Minor
	Recoverable    money.Minor
	NonRecoverable money.Minor
}

// Charged is the tax the partner charges on the line: every component but
// the self-assessed ones, which the partner does not charge.
func (l LineTax) Charged() (money.Minor, error) {
	var taxes []money.Minor
	for _, c := range l.Components {
		if !selfAssessed(c.AccountKey) {
			taxes = append(taxes, c.Tax)
		}
	}
	return money.Sum(taxes...)
}

func selfAssessed(key string) bool { return key == KeySelfOutput || key == KeySelfInput }

// Included is the tax an amount that includes it at one rate in percent
// holds, amount × rate / (100 + rate) rounded once, half away from zero,
// in minor units of exp digits (ADR-0049 §8): a down payment request's
// tax, calculated when it is requested and checked again at the rate of
// the day it is paid (§4). A rate that is not a decimal of 0 to below 100
// is an error.
func Included(amount money.Minor, exp int, ratePct string) (money.Minor, error) {
	rate := money.ParseRat(ratePct)
	if rate == nil || rate.Sign() < 0 || rate.Cmp(big.NewRat(100, 1)) >= 0 {
		return 0, fmt.Errorf("tax: rate %q is not a percentage of 0 to below 100", ratePct)
	}
	share := new(big.Rat).Quo(rate, new(big.Rat).Add(rate, big.NewRat(100, 1)))
	return money.Round(new(big.Rat).Mul(money.Rat(amount, exp), share), exp)
}
