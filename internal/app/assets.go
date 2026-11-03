package app

import (
	"context"

	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Assets is the assets context as the API wires it (ADR-0057 §2): a leaf
// that reads the configuration and tenancy contexts through the adapters
// below, so neither imports it and it imports neither.
func Assets(cfg Config) *assets.Endpoints {
	return &assets.Endpoints{Settings: configuration.Settings{}, Companies: assetCompanies{}, Pages: cfg.Pages}
}

// assetCompanies gives the assets context the legal entity of a company
// code.
type assetCompanies struct{}

func (assetCompanies) ByCode(ctx context.Context, code string) (assets.Company, bool, error) {
	le, ok, err := tenancy.Directory{}.EntityByCode(ctx, code)
	return assets.Company{ID: le.ID, Code: le.Code, Name: le.Name}, ok, err
}
