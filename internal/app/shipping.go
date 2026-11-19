package app

import (
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/shipping"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Shipping is the shipping context as the API wires it (ADR-0083 §1): it
// reads sales orders through sales' published read and has sales hold
// what its deliveries take, reads materials through materials' published
// API and stock through inventory's, and tenancy's companies and org
// units, the ledger's fiscal calendar and the configuration's number
// ranges and company dates through its ports.
func Shipping(cfg Config) *shipping.Endpoints {
	return &shipping.Endpoints{Pages: cfg.Pages, Orders: sales.Orders{}, Shipments: Sales(cfg), Stock: Inventory(cfg), Materials: materials.Materials{},
		Partners: partner.Payees{}, LegalEntities: tenancy.Directory{}, Numbers: configuration.Numbers{}, Calendar: calendar(), Settings: configuration.Settings{}, Now: cfg.Now}
}
