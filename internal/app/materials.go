package app

import (
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Materials is the material master context as the API wires it (ADR-0082
// §1): it reads tenancy's companies and org units, the configuration's
// number ranges and company dates and the ledger's fiscal calendar
// through its ports.
func Materials(cfg Config) *materials.Endpoints {
	return &materials.Endpoints{Pages: cfg.Pages, LegalEntities: tenancy.Directory{}, Numbers: configuration.Numbers{}, Calendar: calendar(),
		Settings: configuration.Settings{}, Now: cfg.Now}
}
