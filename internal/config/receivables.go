package config

import (
	"math/big"
	"regexp"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Receivables is how a company applies its customers' incoming payments
// (ADR-0063 §6).
type Receivables struct {
	// PaymentTolerance bounds the payment differences an application
	// writes off (T75f). None: a receipt is applied exactly.
	PaymentTolerance PaymentTolerance `yaml:"payment_tolerance,omitempty" json:"payment_tolerance,omitzero"`
	// PaymentDifferenceReasons are the reasons an item is paid in part for
	// (T75o, SAP OBBE): each says whether the difference stays open as a
	// residual item or is written off, and where. None: the four reasons
	// T75f fixed (DefaultDifferenceReasons).
	PaymentDifferenceReasons []DifferenceReason `yaml:"payment_difference_reasons,omitempty" json:"payment_difference_reasons,omitempty"`
}

// A difference reason's treatments: the difference stays open as a
// residual item of the customer's, or is written off to the reason's
// account (SAP's "charge off difference").
const (
	TreatmentResidual = "residual"
	TreatmentWriteOff = "write_off"
)

// DifferenceReason is a reason an item is paid in part (T75o): its code,
// which the residual's reason names, a description, its treatment and,
// for a write-off, the account it is written off to, an expense.
type DifferenceReason struct {
	Code        string `yaml:"code" json:"code"`
	Description string `yaml:"description" json:"description"`
	Treatment   string `yaml:"treatment" json:"treatment"`
	Account     string `yaml:"account,omitempty" json:"account,omitempty"`
}

// DefaultDifferenceReasons are a company's reasons when it configures
// none: T75f's four, each leaving a residual item, so that a company keeps
// the reasons it had until it configures its own.
var DefaultDifferenceReasons = []DifferenceReason{
	{Code: "short_payment", Description: "The customer paid less without saying why", Treatment: TreatmentResidual},
	{Code: "disputed", Description: "The customer disputes the rest", Treatment: TreatmentResidual},
	{Code: "deduction", Description: "The customer deducted something it claims", Treatment: TreatmentResidual},
	{Code: "other", Description: "Anything else, which the free text says", Treatment: TreatmentResidual},
}

// DifferenceReasons are the company's payment-difference reasons: those it
// configures, else DefaultDifferenceReasons, also for a company the
// document does not name (nil).
func (c *Company) DifferenceReasons() []DifferenceReason {
	if c == nil || len(c.Receivables.PaymentDifferenceReasons) == 0 {
		return DefaultDifferenceReasons
	}
	return c.Receivables.PaymentDifferenceReasons
}

// reasonCodeRe is a reason's code: lower case, digits and underscores, up
// to 40 characters, as the payment's item stores it
// (incoming_payment_item_residual_reason_length).
var reasonCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// differenceReasons checks the company's payment-difference reasons: each
// code well formed and once, a description of 1 to 80 characters, a
// treatment the list knows, and the account of a write-off, an expense of
// the chart that reconciles nothing, which a residual reason does not
// have (it stays on the customer's account).
func (v *validator) differenceReasons(p string, co *Company) {
	seen := map[string]bool{}
	for _, r := range co.Receivables.PaymentDifferenceReasons {
		rp := p + ".receivables.payment_difference_reasons[" + r.Code + "]"
		if !reasonCodeRe.MatchString(r.Code) || seen[r.Code] {
			v.err(rp+".code", "a reason's code is lower case letters, digits and underscores, up to 40, once in the company")
		}
		seen[r.Code] = true
		if n := len([]rune(r.Description)); n == 0 || n > 80 {
			v.err(rp+".description", "a description of 1 to 80 characters")
		}
		switch r.Treatment {
		case TreatmentWriteOff:
			v.account(rp+".account", co, r.Account, Expense, false)
		case TreatmentResidual:
			if r.Account != "" {
				v.err(rp+".account", "a residual stays open on the customer's account: a residual reason has no account")
			}
		default:
			v.err(rp+".treatment", "residual or write_off")
		}
	}
}

// PaymentTolerance bounds what a receipt applied to a customer's items may
// differ from them and still clear them, the difference written off (SAP
// OBA3, the customer tolerance group's payment differences; T75f). Each
// direction has an amount in the company's currency and a percent of the
// items applied, both plain decimals; the lower of the two binds, so a
// direction allows a difference only when both are set and above zero.
// An underpayment is posted to the account determination's
// payment_difference_expense, an overpayment to payment_difference_revenue.
type PaymentTolerance struct {
	UnderpaymentAmount  string `yaml:"underpayment_amount,omitempty" json:"underpayment_amount,omitempty"`
	UnderpaymentPercent string `yaml:"underpayment_percent,omitempty" json:"underpayment_percent,omitempty"`
	OverpaymentAmount   string `yaml:"overpayment_amount,omitempty" json:"overpayment_amount,omitempty"`
	OverpaymentPercent  string `yaml:"overpayment_percent,omitempty" json:"overpayment_percent,omitempty"`
}

// Tolerance is the company's payment tolerance; none for a company the
// document does not name (nil).
func (c *Company) Tolerance() PaymentTolerance {
	if c == nil {
		return PaymentTolerance{}
	}
	return c.Receivables.PaymentTolerance
}

// ToleranceLimit is one direction of a payment tolerance as an application
// reads it: Amount in minor units of the company's currency and Percent of
// the items applied; zero for either allows no difference.
type ToleranceLimit struct {
	Amount  money.Minor
	Percent *big.Rat
}

// Limits are the tolerance's underpayment and overpayment limits in minor
// units of a currency with exp digits. A validated document's values
// parse; a value that does not, or is absent, reads as zero, which allows
// no difference.
func (t PaymentTolerance) Limits(exp int) (under, over ToleranceLimit) {
	return limit(t.UnderpaymentAmount, t.UnderpaymentPercent, exp), limit(t.OverpaymentAmount, t.OverpaymentPercent, exp)
}

func limit(amount, percent string, exp int) ToleranceLimit {
	out := ToleranceLimit{Percent: new(big.Rat)}
	if m, err := money.ParseDecimal(amount, exp); err == nil && m > 0 {
		out.Amount = m
	}
	if r, err := money.ParseDecimalRat(percent); err == nil && r.Sign() > 0 {
		out.Percent = r
	}
	return out
}

// receivables checks a company's payment-difference reasons
// (differenceReasons) and its payment tolerance: each direction's
// amount, a plain decimal of at least zero in the company's currency, and
// percent, a plain decimal from 0 to 100, set together or not at all; a
// direction that allows a difference needs the account it is posted to.
func (v *validator) receivables(p string, co *Company, exp int) {
	v.differenceReasons(p, co)
	t, ad := co.Receivables.PaymentTolerance, co.AccountDetermination
	tp := p + ".receivables.payment_tolerance"
	for _, d := range []struct {
		name, amount, percent, key, account string
	}{{"underpayment", t.UnderpaymentAmount, t.UnderpaymentPercent, "payment_difference_expense", ad.PaymentDifferenceExpense},
		{"overpayment", t.OverpaymentAmount, t.OverpaymentPercent, "payment_difference_revenue", ad.PaymentDifferenceRevenue}} {
		if (d.amount == "") != (d.percent == "") {
			v.err(tp+"."+d.name+"_percent", "%s_amount and %s_percent are set together: the lower of the two binds", d.name, d.name)
			continue
		}
		if d.amount == "" {
			continue
		}
		m, ok := v.amount(tp+"."+d.name+"_amount", d.amount, exp)
		r, err := money.ParseDecimalRat(d.percent)
		switch {
		case err != nil || r.Sign() < 0 || r.Cmp(big.NewRat(100, 1)) > 0:
			v.err(tp+"."+d.name+"_percent", "a plain decimal from 0 to 100")
		case ok && m > 0 && r.Sign() > 0 && d.account == "":
			v.err(p+".account_determination."+d.key, "is required: the %s tolerance writes differences off to it", d.name)
		}
	}
}
