package config

import (
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
