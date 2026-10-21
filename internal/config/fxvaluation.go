package config

import (
	"fmt"
	"slices"
)

// rateTypes are the exchange rate types a company may value at (SAP M,
// G, B, P), those of ledger.exchange_rate.
var rateTypes = []string{"average", "bank_buying", "bank_selling", "planning"}

// fxValuation checks the company's foreign-currency valuation (ADR-0042):
// a known rate type; flagged accounts that are assets or liabilities
// (equity is non-monetary, IAS 21.16), reconcile no supplier and are not
// the GR/IR account, each with a valuation; and one valuation per account, of a vendor reconciliation account other than
// the down payment account (special G/L items are not revalued) or of a
// flagged account, with a balance-sheet adjustment account exactly for a
// reconciliation account, a revenue gain and an expense loss.
func (v *validator) fxValuation(p string, co *Company) {
	fp := p + ".fx_valuation"
	if !slices.Contains(rateTypes, co.FXValuation.RateTypeOrAverage()) {
		v.err(fp+".rate_type", "rate type %q must be average, bank_buying, bank_selling or planning", co.FXValuation.RateType)
	}
	valued := map[string]bool{}
	for _, f := range co.FXValuation.Accounts {
		ap := fmt.Sprintf("%s.accounts[%s]", fp, f.Account)
		if valued[f.Account] {
			v.err(ap, "duplicate account")
		}
		valued[f.Account] = true
		v.fxValuationAccount(ap, co, f)
	}
	for _, a := range co.ChartOfAccounts {
		if !a.RevalueForeignCurrency {
			continue
		}
		ap := fmt.Sprintf("%s.chart_of_accounts[%s].revalue_foreign_currency", p, a.Number)
		switch {
		case (a.Type != Asset && a.Type != Liability) || a.VendorReconciliation || a.Number == co.AccountDetermination.GRIR:
			v.err(ap, "only an asset or liability that reconciles no supplier and is not the GR/IR account is revalued as a balance")
		case !valued[a.Number]:
			v.err(ap, "account %s is revalued: add it to fx_valuation.accounts with its unrealised gain and loss", a.Number)
		}
	}
}

// fxValuationAccount checks one account's valuation accounts.
func (v *validator) fxValuationAccount(ap string, co *Company, f FXValuationAccount) {
	a := co.Account(f.Account)
	recon := a != nil && a.VendorReconciliation
	switch {
	case a == nil:
		v.err(ap, "account %s is not in the chart of accounts", f.Account)
		return
	case f.Account == co.AccountDetermination.DownPayment:
		v.err(ap, "account %s is the down payment account; special G/L items are not revalued", f.Account)
	case !recon && !a.RevalueForeignCurrency:
		v.err(ap, "account %s is neither a vendor reconciliation account nor revalue_foreign_currency", f.Account)
	}
	adj := co.Account(f.Adjustment)
	switch {
	case recon && (adj == nil || !adj.Type.BalanceSheet() || adj.VendorReconciliation):
		v.err(ap+".adjustment", "a reconciliation account's difference goes to a balance-sheet account of the chart that reconciles no supplier")
	case !recon && f.Adjustment != "":
		v.err(ap+".adjustment", "a revalued G/L account takes its difference itself; leave adjustment out")
	}
	if g := co.Account(f.UnrealisedGain); g == nil || g.Type != Revenue {
		v.err(ap+".unrealised_gain", "account %s: the unrealised gain is a revenue account of the chart", f.UnrealisedGain)
	}
	if l := co.Account(f.UnrealisedLoss); l == nil || l.Type != Expense {
		v.err(ap+".unrealised_loss", "account %s: the unrealised loss is an expense account of the chart", f.UnrealisedLoss)
	}
}

// BalanceSheet says whether accounts of the type are on the balance sheet.
func (t AccountType) BalanceSheet() bool { return t == Asset || t == Liability || t == Equity }
