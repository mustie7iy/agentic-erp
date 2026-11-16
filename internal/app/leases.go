package app

import (
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/leases"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Leases is the lease accounting context as the API wires it (ADR-0073
// §1): a leaf that reads tenancy, the partner context's suppliers and
// names, the ledger's cost centers and calendar and the configuration's
// number ranges and company dates through its ports, and posts through the
// ledger's posting port with the account determination's accounts.
func Leases(cfg Config) *leases.Endpoints {
	return &leases.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Lessors: partner.Suppliers{Settings: configuration.Settings{}}, Partners: partner.Payees{},
		CostCenters: ledger.AssignmentBalances{}, Numbers: configuration.Numbers{}, Calendar: calendar(), Settings: configuration.Settings{},
		Postings: postings(), Accounts: ledger.Accounts{}, Now: cfg.Now}
}
