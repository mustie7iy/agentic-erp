package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withTolerance gives company 1000 the payment difference accounts and a
// payment tolerance (T75f).
func withTolerance(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	co := &c.Companies[0]
	co.ChartOfAccounts = append(co.ChartOfAccounts, Account{Number: "680000", Name: "Payment differences", Type: Expense},
		Account{Number: "480000", Name: "Payment differences kept", Type: Revenue})
	co.AccountDetermination.PaymentDifferenceExpense, co.AccountDetermination.PaymentDifferenceRevenue = "680000", "480000"
	co.Receivables.PaymentTolerance = PaymentTolerance{UnderpaymentAmount: "5.00", UnderpaymentPercent: "1", OverpaymentAmount: "2.00", OverpaymentPercent: "0.5"}
	return c
}

// TestPaymentToleranceValid (T75f; ADR-0063 §6): a company's payment
// tolerance gives each direction an amount in its currency and a percent
// from 0 to 100, together; a direction that allows a difference needs its
// account, an expense for underpayments and a revenue for overpayments,
// both among the accounts the system posts to.
func TestPaymentToleranceValid(t *testing.T) {
	if fs := Validate(withTolerance(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("a payment tolerance: got %v, want no errors", fs)
	}
	var automatic []string
	for _, a := range withTolerance(t).Companies[0].AutomaticAccounts() {
		if strings.HasSuffix(a.Account, "80000") && a.Account != "180000" {
			automatic = append(automatic, a.Path+" "+a.Account)
		}
	}
	if got, want := strings.Join(automatic, ", "), "account_determination.payment_difference_expense 680000, account_determination.payment_difference_revenue 480000"; got != want {
		t.Errorf("automatic accounts: got %s, want %s", got, want)
	}
	const p = "companies[1000]."
	for _, c := range []struct {
		name   string
		change func(co *Company)
		path   string
	}{
		{"an amount without a percent", func(co *Company) { co.Receivables.PaymentTolerance.UnderpaymentPercent = "" }, p + "receivables.payment_tolerance.underpayment_percent"},
		{"a percent without an amount", func(co *Company) { co.Receivables.PaymentTolerance.OverpaymentAmount = "" }, p + "receivables.payment_tolerance.overpayment_percent"},
		{"a negative amount", func(co *Company) { co.Receivables.PaymentTolerance.UnderpaymentAmount = "-1.00" }, p + "receivables.payment_tolerance.underpayment_amount"},
		{"an amount with three decimals", func(co *Company) { co.Receivables.PaymentTolerance.OverpaymentAmount = "1.001" }, p + "receivables.payment_tolerance.overpayment_amount"},
		{"a percent over 100", func(co *Company) { co.Receivables.PaymentTolerance.UnderpaymentPercent = "100.5" }, p + "receivables.payment_tolerance.underpayment_percent"},
		{"a negative percent", func(co *Company) { co.Receivables.PaymentTolerance.OverpaymentPercent = "-1" }, p + "receivables.payment_tolerance.overpayment_percent"},
		{"a percent in words", func(co *Company) { co.Receivables.PaymentTolerance.OverpaymentPercent = "half" }, p + "receivables.payment_tolerance.overpayment_percent"},
		{"no expense account", func(co *Company) { co.AccountDetermination.PaymentDifferenceExpense = "" }, p + "account_determination.payment_difference_expense"},
		{"no revenue account", func(co *Company) { co.AccountDetermination.PaymentDifferenceRevenue = "" }, p + "account_determination.payment_difference_revenue"},
		{"an expense account that is a revenue", func(co *Company) { co.AccountDetermination.PaymentDifferenceExpense = "480000" },
			p + "account_determination.payment_difference_expense"},
		{"a revenue account that is an expense", func(co *Company) { co.AccountDetermination.PaymentDifferenceRevenue = "680000" },
			p + "account_determination.payment_difference_revenue"},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := withTolerance(t)
			c.change(&doc.Companies[0])
			fs := Validate(doc, currency.Seeded())
			var paths []string
			for _, f := range fs {
				if f.Severity == SevError {
					paths = append(paths, f.Path)
				}
			}
			if strings.Join(paths, " ") != c.path {
				t.Errorf("errors: got %v, want one at %s", fs, c.path)
			}
		})
	}
	t.Run("a zero tolerance needs no account", func(t *testing.T) {
		doc := withTolerance(t)
		co := &doc.Companies[0]
		co.Receivables.PaymentTolerance = PaymentTolerance{UnderpaymentAmount: "0.00", UnderpaymentPercent: "0"}
		co.AccountDetermination.PaymentDifferenceExpense, co.AccountDetermination.PaymentDifferenceRevenue = "", ""
		if fs := Validate(doc, currency.Seeded()); HasErrors(fs) {
			t.Errorf("got %v, want no errors", fs)
		}
	})
}

// TestPaymentToleranceLimits: the limits are read in minor units of the
// company's currency, a direction not set or zero allowing nothing, and a
// company the document does not name has none.
func TestPaymentToleranceLimits(t *testing.T) {
	under, over := withTolerance(t).Companies[0].Tolerance().Limits(2)
	if under.Amount != 500 || under.Percent.RatString() != "1" || over.Amount != 200 || over.Percent.RatString() != "1/2" {
		t.Errorf("limits: got %d %s and %d %s, want 500 1 and 200 1/2", under.Amount, under.Percent.RatString(), over.Amount, over.Percent.RatString())
	}
	var none *Company
	under, over = none.Tolerance().Limits(2)
	if under.Amount != 0 || under.Percent.Sign() != 0 || over.Amount != 0 || over.Percent.Sign() != 0 {
		t.Errorf("no company's limits: got %+v and %+v, want zero", under, over)
	}
}

// withReasons gives company 1000 the payment differences of withTolerance
// and three reasons (T75o): two residuals and bank charges written off to
// 690000.
func withReasons(t *testing.T) *Config {
	t.Helper()
	c := withTolerance(t)
	co := &c.Companies[0]
	co.ChartOfAccounts = append(co.ChartOfAccounts, Account{Number: "690000", Name: "Bank charges borne", Type: Expense})
	co.Receivables.PaymentDifferenceReasons = []DifferenceReason{{Code: "short_payment", Description: "Paid less", Treatment: TreatmentResidual},
		{Code: "disputed", Description: "Disputed", Treatment: TreatmentResidual},
		{Code: "bank_charges", Description: "The customer's bank charged", Treatment: TreatmentWriteOff, Account: "690000"}}
	return c
}

// TestDifferenceReasonsValid (T75o; ADR-0063 §6): each reason has a code
// of lower case letters, digits and underscores, once, a description, a
// treatment, residual or write_off, and a write-off its account, an
// expense of the chart that reconciles nothing, which is then an account
// the system posts to; a residual reason has none.
func TestDifferenceReasonsValid(t *testing.T) {
	if fs := Validate(withReasons(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("reasons: got %v, want no errors", fs)
	}
	if !slices.Contains(withReasons(t).Companies[0].AutomaticAccounts(), AutomaticAccount{Path: "receivables.payment_difference_reasons[bank_charges].account",
		Account: "690000"}) {
		t.Errorf("automatic accounts: got %v, want the write-off account among them", withReasons(t).Companies[0].AutomaticAccounts())
	}
	const p = "companies[1000].receivables.payment_difference_reasons"
	reason := func(i int, change func(r *DifferenceReason)) func(co *Company) {
		return func(co *Company) { change(&co.Receivables.PaymentDifferenceReasons[i]) }
	}
	for _, c := range []struct {
		name   string
		change func(co *Company)
		path   string
	}{
		{"a code in capitals", reason(0, func(r *DifferenceReason) { r.Code = "SHORT" }), p + "[SHORT].code"},
		{"a code of 41 characters", reason(0, func(r *DifferenceReason) { r.Code = strings.Repeat("a", 41) }), p + "[" + strings.Repeat("a", 41) + "].code"},
		{"a code twice", reason(1, func(r *DifferenceReason) { r.Code = "short_payment" }), p + "[short_payment].code"},
		{"no description", reason(0, func(r *DifferenceReason) { r.Description = "" }), p + "[short_payment].description"},
		{"a description of 81 characters", reason(0, func(r *DifferenceReason) { r.Description = strings.Repeat("d", 81) }), p + "[short_payment].description"},
		{"a treatment not listed", reason(0, func(r *DifferenceReason) { r.Treatment = "ignore" }), p + "[short_payment].treatment"},
		{"a write-off without its account", reason(2, func(r *DifferenceReason) { r.Account = "" }), p + "[bank_charges].account"},
		{"a write-off to a revenue", reason(2, func(r *DifferenceReason) { r.Account = "480000" }), p + "[bank_charges].account"},
		{"a write-off to an account not in the chart", reason(2, func(r *DifferenceReason) { r.Account = "699999" }), p + "[bank_charges].account"},
		{"a residual reason with an account", reason(1, func(r *DifferenceReason) { r.Account = "690000" }), p + "[disputed].account"},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := withReasons(t)
			c.change(&doc.Companies[0])
			var paths []string
			for _, f := range Validate(doc, currency.Seeded()) {
				if f.Severity == SevError {
					paths = append(paths, f.Path)
				}
			}
			if got := strings.Join(paths, " "); got != c.path {
				t.Errorf("errors: got %s, want one at %s", got, c.path)
			}
		})
	}
}

// TestDifferenceReasons: a company's reasons are those it configures, else
// the four T75f fixed, all residuals, as for a company the document does
// not name.
func TestDifferenceReasons(t *testing.T) {
	codes := func(rs []DifferenceReason) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Code+" "+r.Treatment)
		}
		return strings.Join(out, ", ")
	}
	var none *Company
	for name, c := range map[string]struct {
		co   *Company
		want string
	}{
		"configured":      {&withReasons(t).Companies[0], "short_payment residual, disputed residual, bank_charges write_off"},
		"none":            {&withTolerance(t).Companies[0], "short_payment residual, disputed residual, deduction residual, other residual"},
		"no such company": {none, "short_payment residual, disputed residual, deduction residual, other residual"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := codes(c.co.DifferenceReasons()); got != c.want {
				t.Errorf("reasons: got %s, want %s", got, c.want)
			}
		})
	}
}

// TestDifferenceReasonsTier: a reason decides what is written off to
// profit and loss and where, so adding one and changing its treatment or
// account are posting-related (tier 3), each named by its code.
func TestDifferenceReasonsTier(t *testing.T) {
	moved := withReasons(t)
	moved.Companies[0].Receivables.PaymentDifferenceReasons[1].Treatment = TreatmentWriteOff
	diffs, _ := Diff(withReasons(t), moved) // Diff never fails
	if len(diffs) != 1 || diffs[0].Path != "companies[1000].receivables.payment_difference_reasons[disputed].treatment" || diffs[0].Tier != TierPostingRelated {
		t.Errorf("a treatment changed: got %v, want one change of tier %v", diffs, TierPostingRelated)
	}
	diffs, _ = Diff(withTolerance(t), withReasons(t)) // Diff never fails
	var reasons []string
	for _, d := range diffs {
		if strings.Contains(d.Path, ".payment_difference_reasons[") {
			reasons = append(reasons, d.Path)
			if d.Tier != TierPostingRelated {
				t.Errorf("tier of %s: got %v, want %v", d.Path, d.Tier, TierPostingRelated)
			}
		}
	}
	if len(reasons) != 3 {
		t.Errorf("reasons set: got %v, want one change per reason", reasons)
	}
}

// TestDiscountKeysValid (ADR-0063 and ADR-0061 As built batch 92): a
// company may name its cash discount granted account, an expense, and its
// supplier rebate account, a revenue; both are accounts the system posts
// to.
func TestDiscountKeysValid(t *testing.T) {
	c := fixture(t)
	ad := &c.Companies[0].AccountDetermination
	ad.CashDiscountGranted, ad.SupplierRebate = "520000", "400000"
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("both keys: got %v, want no errors", fs)
	}
	var automatic []string
	for _, a := range c.Companies[0].AutomaticAccounts() {
		if strings.HasPrefix(a.Path, "account_determination.") && (a.Account == "520000" || a.Account == "400000") {
			automatic = append(automatic, a.Path+" "+a.Account)
		}
	}
	if got, want := strings.Join(automatic, ", "), "account_determination.cash_discount_granted 520000, account_determination.supplier_rebate 400000"; got != want {
		t.Errorf("automatic accounts: got %s, want %s", got, want)
	}
}
