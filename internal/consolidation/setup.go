package consolidation

import (
	"context"
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Setup is the consolidation context's section of the configuration
// (ADR-0059 §3): it keeps nothing of its own, and checks against the
// tenant's group currency what the document's own validation cannot know:
// each group's parent keeps its books in the group currency, since a run
// eliminates the investment at its booked group amount, which equals its
// translated amount only then (§3; a run refuses another parent with 422
// consolidation.parent_currency), and each elimination's tolerance, an
// amount in the group currency, has at most that currency's decimals.
type Setup struct {
	Companies Companies
}

// Check reports each group whose parent is in another currency than the
// group currency and each tolerance with more decimals than it has. A
// document without groups or eliminations reads nothing.
func (s Setup) Check(ctx context.Context, _, proposed *config.Config) ([]config.Finding, error) {
	cs := proposed.Consolidation
	if len(cs.Groups) == 0 && len(cs.Eliminations) == 0 {
		return nil, nil
	}
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	group, err := s.Companies.GroupCurrency(ctx)
	if err != nil {
		return nil, fmt.Errorf("consolidation: check the configuration: %w", err)
	}
	exponents, err := currency.LoadCodes(ctx, tx, group)
	if err != nil {
		return nil, fmt.Errorf("consolidation: check the configuration: %w", err)
	}
	var out []config.Finding
	for _, g := range cs.Groups {
		if co := proposed.Company(g.Parent); co != nil && co.Currency != group {
			out = append(out, config.Finding{Severity: config.SevError, Path: "consolidation.groups[" + g.Code + "].parent",
				Message: fmt.Sprintf("the parent %s keeps its books in %s, not the group currency %s; a group's parent is in the group currency",
					g.Parent, co.Currency, group)})
		}
	}
	for _, e := range cs.Eliminations {
		_, plain := money.ParseDecimalRat(e.Tolerance)
		if _, err := money.ParseDecimal(e.Tolerance, exponents[group]); err != nil && plain == nil { // a malformed one is validation's
			out = append(out, config.Finding{Severity: config.SevError, Path: "consolidation.eliminations[" + e.Code + "].tolerance",
				Message: fmt.Sprintf("an amount in the group currency %s, with at most %d decimals", group, exponents[group])})
		}
	}
	return out, nil
}

// Apply keeps nothing: the consolidation context reads its settings from
// the active document.
func (Setup) Apply(context.Context, *config.Config) error { return nil }
