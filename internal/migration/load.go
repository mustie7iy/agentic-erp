package migration

import "github.com/mustie7iy/agentic-erp/internal/core"

// Load writes a dataset into the store. Journal entries go through the same
// ledger validation as native postings, so a migrated entry that would not
// be accepted from a user is not accepted from SAP either.
func Load(s *core.Store, ds *Dataset) []Issue {
	var issues []Issue
	for _, c := range ds.Companies {
		s.Companies[c.Code] = c
	}
	for _, a := range ds.Accounts {
		s.PutAccount(a)
	}
	for _, v := range ds.Vendors {
		if existing, ok := s.Vendors[v.ID]; ok {
			// Vendors are shared across company codes: merge, don't overwrite.
			for cc, acct := range v.APAccount {
				existing.APAccount[cc] = acct
			}
			continue
		}
		s.Vendors[v.ID] = v
	}
	for _, po := range ds.POs {
		s.PurchaseOrders[po.ID] = po
	}
	for _, g := range ds.Receipts {
		s.GoodsReceipts[g.ID] = g
	}
	for _, inv := range ds.Invoices {
		s.Invoices[inv.ID] = inv
	}
	for _, je := range ds.Entries {
		if err := s.Post(je); err != nil {
			issues = append(issues, Issue{Severity: SevError, Table: "JOURNAL", Key: je.ID, Message: err.Error()})
		}
	}
	return issues
}
