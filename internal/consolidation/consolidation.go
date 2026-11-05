// Package consolidation is the group reporting and consolidation context
// (ADR-0059; SAP's Group Reporting): consolidation groups, their runs per
// regular period, the members' translated balances, intercompany matching
// and eliminations, the investment's elimination with goodwill and
// non-controlling interests, and the consolidated statements, full
// consolidation only. It is a leaf (ADR-0059 §2): it reads the other
// contexts only through ports it declares, which the composition root
// (internal/app) adapts and which run in the request's transaction under
// row-level security (ADR-0003), and no context imports it
// (internal/layout).
//
// T68a builds its configuration and reads: the group chart, the
// eliminations, the groups and each company account's group account are
// configuration (ADR-0059 §3, internal/config), validated there and, for
// what needs the tenant's data (the group currency), by Setup, the
// context's section of the configuration. ListConsolidationGroups,
// GetConsolidationGroup and ListGroupAccounts answer them from the active
// document through the Settings and Companies ports. A group is visible
// only to a caller who may act in its parent and every member (ADR-0059
// §12): its figures are only meaningful whole, so a group outside that is
// unknown, never shown in part. Runs, translation, the trading partner and
// eliminations follow in T68b-T68h.
package consolidation

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Settings reads the active configuration (configuration context), whose
// group chart and groups the context reads.
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Company is a legal entity as the consolidation context reads it: its
// code, name, local currency and fiscal calendar.
type Company struct {
	ID               uuid.UUID
	Code             string
	Name             string
	LocalCurrency    string
	FiscalCalendarID uuid.UUID
}

// Companies reads the tenant's legal entities and its group currency
// (tenancy context, ADR-0059 §2). Company access is not applied: the
// context applies it, to a group as a whole.
type Companies interface {
	Entities(ctx context.Context) ([]Company, error)
	GroupCurrency(ctx context.Context) (string, error)
}

// Endpoints are the consolidation context's operations.
type Endpoints struct {
	Settings  Settings
	Companies Companies
	Pages     *pagination.Codec
}

// groupUnknown is the answer for a group that does not exist or that the
// caller may not see whole (ADR-0059 §12), so neither its existence nor
// its companies leak.
func groupUnknown(code string) *problem.Problem {
	return problem.New(http.StatusNotFound, "consolidation.group_unknown", "Not found",
		"There is no consolidation group "+code+" whose parent and members you may all act in.")
}

// companiesByCode are the tenant's legal entities by company code.
func (e *Endpoints) companiesByCode(ctx context.Context) (map[string]Company, error) {
	all, err := e.Companies.Entities(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Company, len(all))
	for _, c := range all {
		out[c.Code] = c
	}
	return out, nil
}

// visible reports whether the caller may see the group: its parent and
// every member a legal entity the caller may act in. A company of the
// document without a legal entity cannot be in an activated document
// (ledger.Setup), and would hide the group rather than show it in part.
func visible(g config.ConsolidationGroup, companies map[string]Company, access authz.Access) bool {
	codes := []string{g.Parent}
	for _, m := range g.Members {
		codes = append(codes, m.Company)
	}
	for _, code := range codes {
		le, ok := companies[code]
		if !ok || !access.Allows(le.ID) {
			return false
		}
	}
	return true
}
