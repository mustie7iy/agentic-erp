package fixture

import "github.com/mustie7iy/agentic-erp/internal/config"

// AssetClasses gives the prototype's document three asset classes: company
// 1000 uses MACHINES and VEHICLES, company 2000 MACHINES, and no company
// BUILDINGS; each company has the accounts and number ranges they need
// (ADR-0057 §3).
func AssetClasses(doc *config.Config) {
	doc.AssetClasses = []config.AssetClass{
		{Code: "VEHICLES", Name: "Vehicles", Terms: []config.AssetTerms{{Ledger: config.LeadingLedger, Method: config.DecliningBalance, UsefulLifeMonths: 60,
			DecliningFactor: "2.5"}}},
		{Code: "MACHINES", Name: "Machinery and equipment", Terms: []config.AssetTerms{{Ledger: config.LeadingLedger, Method: config.StraightLine,
			UsefulLifeMonths: 120}}},
		{Code: "BUILDINGS", Name: "Buildings", Terms: []config.AssetTerms{{Ledger: config.LeadingLedger, Method: config.StraightLine, UsefulLifeMonths: 600}}},
	}
	for i := range doc.Companies {
		co := &doc.Companies[i]
		co.ChartOfAccounts = append(co.ChartOfAccounts,
			config.Account{Number: "160000", Name: "Machinery", Type: config.Asset, AssetReconciliation: true},
			config.Account{Number: "160900", Name: "Machinery, accumulated depreciation", Type: config.Asset, AssetReconciliation: true},
			config.Account{Number: "620000", Name: "Depreciation", Type: config.Expense},
			config.Account{Number: "169000", Name: "Asset clearing", Type: config.Asset},
			config.Account{Number: "490000", Name: "Gain on asset retirements", Type: config.Revenue},
			config.Account{Number: "690000", Name: "Loss on asset retirements", Type: config.Expense},
			config.Account{Number: "169500", Name: "Asset sale clearing", Type: config.Asset})
		accounts := config.AssetAccounts{Class: "MACHINES", AcquisitionCost: "160000", AccumulatedDepreciation: "160900", Depreciation: "620000",
			Clearing: "169000", Gain: "490000", Loss: "690000", SaleClearing: "169500"}
		co.AssetAccounts = []config.AssetAccounts{accounts}
		if co.Code == "1000" {
			accounts.Class = "VEHICLES"
			co.AssetAccounts = append(co.AssetAccounts, accounts)
		}
		for _, y := range Years() {
			first, last := int64(1), int64(999999)
			if y != 2026 { // asset numbers may not repeat another year's (config.NumberedPerYear)
				first, last = 1000000, 1999999
			}
			for _, kind := range config.AssetRanges {
				doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: co.Code, DocumentType: kind, FiscalYear: y, First: first, Last: last})
			}
		}
	}
}
