package app

import (
	"context"

	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/consolidation"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Consolidation is the consolidation context as the API wires it
// (ADR-0059 §2): a leaf that reads the configuration and tenancy contexts
// through the adapters below, so none of them imports it and it imports
// none of them.
func Consolidation(cfg Config) *consolidation.Endpoints {
	return &consolidation.Endpoints{Settings: configuration.Settings{}, Companies: consolidationCompanies{}, Pages: cfg.Pages}
}

// consolidationCompanies gives the consolidation context the tenant's
// legal entities and its group currency.
type consolidationCompanies struct{}

func (consolidationCompanies) Entities(ctx context.Context) ([]consolidation.Company, error) {
	es, err := tenancy.Directory{}.Entities(ctx)
	out := make([]consolidation.Company, len(es))
	for i, e := range es {
		out[i] = consolidation.Company{ID: e.ID, Code: e.Code, Name: e.Name, LocalCurrency: e.LocalCurrency, FiscalCalendarID: e.FiscalCalendarID}
	}
	return out, err
}

func (consolidationCompanies) GroupCurrency(ctx context.Context) (string, error) {
	return tenancy.Directory{}.GroupCurrency(ctx)
}
