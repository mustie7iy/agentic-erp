package fixture

import (
	"github.com/mustie7iy/agentic-erp/internal/config"
)

// GroupAccounts is the group chart Consolidation configures (ADR-0059
// §3): one account per type for what no elimination holds, intercompany
// receivables and payables, investments, the four role accounts and the
// account intercompany differences go to.
var GroupAccounts = []config.GroupAccount{
	{Number: "G1000", Name: "Other assets", Type: config.Asset},
	{Number: "G1250", Name: "Intercompany receivables", Type: config.Asset},
	{Number: "G1500", Name: "Investments in subsidiaries", Type: config.Asset},
	{Number: "G1900", Name: "Goodwill", Type: config.Asset, Role: config.RoleGoodwill},
	{Number: "G2000", Name: "Liabilities", Type: config.Liability},
	{Number: "G2250", Name: "Intercompany payables", Type: config.Liability},
	{Number: "G3000", Name: "Share capital", Type: config.Equity},
	{Number: "G3800", Name: "Retained earnings", Type: config.Equity, Role: config.RoleRetainedEarnings},
	{Number: "G3900", Name: "Currency translation reserve", Type: config.Equity, Role: config.RoleCTA},
	{Number: "G3950", Name: "Non-controlling interests", Type: config.Equity, Role: config.RoleNCI},
	{Number: "G4000", Name: "Revenue", Type: config.Revenue},
	{Number: "G5000", Name: "Expenses", Type: config.Expense},
	{Number: "G6950", Name: "Intercompany differences", Type: config.Expense},
}

// Consolidation gives the prototype's document group reporting (ADR-0059
// §3, T68a): the group chart GroupAccounts, the elimination IC_BALANCES of
// intercompany receivables and payables (tolerance 10.00), and the group
// ACME: company 2000, in EUR, the fixture tenant's group currency, holding
// 80% of company 1000 from 2026-11, its investment on a new asset account
// 150000. Every other account of every company maps to the group account
// of its type.
func Consolidation(doc *config.Config) {
	doc.Consolidation = config.Consolidation{
		Accounts: append([]config.GroupAccount(nil), GroupAccounts...),
		Eliminations: []config.Elimination{{Code: "IC_BALANCES", Name: "Intercompany receivables and payables",
			Accounts: []string{"G1250", "G2250"}, DifferenceAccount: "G6950", Tolerance: "10.00"}},
		Groups: []config.ConsolidationGroup{{Code: "ACME", Name: "Acme group", Parent: "2000",
			Members: []config.GroupMember{{Company: "1000", OwnershipPercent: "80", FirstPeriod: "2026-11", InvestmentAccount: "150000"}}}},
	}
	parent := doc.Company("2000")
	parent.ChartOfAccounts = append(parent.ChartOfAccounts,
		config.Account{Number: "150000", Name: "Investment in Acme Corp US", Type: config.Asset, GroupAccount: "G1500"})
	byType := map[config.AccountType]string{config.Asset: "G1000", config.Liability: "G2000", config.Equity: "G3000",
		config.Revenue: "G4000", config.Expense: "G5000"}
	for i := range doc.Companies {
		for j := range doc.Companies[i].ChartOfAccounts {
			if a := &doc.Companies[i].ChartOfAccounts[j]; a.GroupAccount == "" {
				a.GroupAccount = byType[a.Type]
			}
		}
	}
}
