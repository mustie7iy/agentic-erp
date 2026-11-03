package closing

import (
	"context"
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// Setup is the closing context's section of the configuration (ADR-0058
// §3): it keeps nothing of its own, and checks what the document's own
// validation cannot, that each company's plan basis names a plan category
// the controlling context holds.
type Setup struct {
	Plans Plans
}

// Check reports each company whose variance settings name a plan category
// the tenant does not have.
func (s Setup) Check(ctx context.Context, _, proposed *config.Config) ([]config.Finding, error) {
	var out []config.Finding
	for _, co := range proposed.Companies {
		v := co.Variance(config.BasisPlan)
		if v == nil {
			continue
		}
		_, ok, err := s.Plans.Category(ctx, v.PlanCategory)
		if err != nil {
			return nil, fmt.Errorf("closing: plan category of company %s: %w", co.Code, err)
		}
		if !ok {
			out = append(out, config.Finding{Severity: config.SevError, Path: "companies[" + co.Code + "].close.variance.plan_category",
				Message: fmt.Sprintf("there is no plan category %s; create it (POST /v1/plan-categories) first", v.PlanCategory)})
		}
	}
	return out, nil
}

// Apply keeps nothing: the closing context reads its settings from the
// active document.
func (Setup) Apply(context.Context, *config.Config) error { return nil }
