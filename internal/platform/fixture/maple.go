package fixture

import (
	"slices"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// MapleCompany is Maple Inc's legal entity, for NewWithCompanies with
// Maple.
var MapleCompany = Company{Code: "3000", Name: "Maple Inc", Country: "CA", Currency: "CAD"}

// Maple is company 3000, Maple Inc, a Canadian company in CAD (ADR-0051
// §3, T53a): company 2000's chart and settings, a CAD house bank MAIN and a
// USD house bank USD, both at a Canadian bank, the USD bank's clearing
// (102000), main (103000) and incoming (104000) accounts revalued at period
// end; payment method T ranks MAIN in CAD and USD in USD; its statements
// match by end-to-end id and post without approval.
func Maple(doc *config.Config) {
	co := *doc.Company("2000")
	co.Code, co.Name, co.Currency, co.Country = "3000", "Maple Inc", "CAD", "CA"
	co.ChartOfAccounts = append(slices.Clone(co.ChartOfAccounts),
		config.Account{Number: "102000", Name: "Bank clearing - USD", Type: config.Asset, OpenItems: true, RevalueForeignCurrency: true},
		config.Account{Number: "103000", Name: "Bank - USD", Type: config.Asset, RevalueForeignCurrency: true},
		config.Account{Number: "104000", Name: "Bank incoming - USD", Type: config.Asset, OpenItems: true, RevalueForeignCurrency: true},
		config.Account{Number: "420000", Name: "Exchange gains", Type: config.Revenue},
		config.Account{Number: "630000", Name: "Exchange losses", Type: config.Expense})
	co.TaxCodes = slices.Clone(co.TaxCodes)
	co.AccountDetermination.FXGain, co.AccountDetermination.FXLoss = "420000", "630000"
	co.HouseBank = config.HouseBank{Name: "MAIN", BIC: "ROYCCAT2", InstitutionNumber: "003", TransitNumber: "00002", AccountNumber: "1000001"}
	co.HouseBanks = []config.HouseBank{{Name: "USD", BIC: "ROYCCAT2", InstitutionNumber: "003", TransitNumber: "00002", AccountNumber: "4000001",
		GLAccount: "102000", MainGLAccount: "103000", IncomingGLAccount: "104000", Currency: "USD"}}
	co.PaymentMethods = []config.PaymentMethod{{Code: "T", Description: "Bank transfer",
		Ranking: []config.BankRanking{{Currency: "CAD", HouseBanks: []string{"MAIN"}}, {Currency: "USD", HouseBanks: []string{"USD"}}}}}
	co.FXValuation = config.FXValuation{}
	for _, n := range []string{"102000", "103000", "104000"} {
		co.FXValuation.Accounts = append(co.FXValuation.Accounts, config.FXValuationAccount{Account: n, UnrealisedGain: "420000", UnrealisedLoss: "630000"})
	}
	co.BankStatements = config.BankStatements{MatchingRules: []config.MatchingRule{{Kind: config.MatchEndToEndID}}, ApprovalTiers: []config.ApprovalTier{{}}}
	doc.Companies = append(doc.Companies, co)
}
