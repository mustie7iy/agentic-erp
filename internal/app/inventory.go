package app

import (
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Inventory is the inventory management context as the API wires it
// (ADR-0082 §1-§3): it reads the materials context's published API,
// tenancy's companies and org units, the ledger's cost centers and fiscal
// calendar and the configuration's number ranges and company dates
// through its ports.
func Inventory(cfg Config) *inventory.Endpoints {
	return &inventory.Endpoints{Pages: cfg.Pages, Materials: materials.Materials{}, LegalEntities: tenancy.Directory{}, CostCenters: ledger.AssignmentBalances{},
		Numbers: configuration.Numbers{}, Calendar: calendar(), Settings: configuration.Settings{}, Now: cfg.Now}
}
