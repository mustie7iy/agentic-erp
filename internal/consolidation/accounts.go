package consolidation

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

// accountCursor is where a page of group accounts ends.
type accountCursor struct {
	Number string `json:"n"`
}

// ListGroupAccounts implements GET /v1/group-accounts (ADR-0059 §3): the
// active document's group chart in number order, filtered by type, role
// and elimination, each account with its elimination and the number of
// company accounts mapped to it in the companies the caller may act in.
func (e *Endpoints) ListGroupAccounts(ctx context.Context, params gen.ListGroupAccountsParams) (gen.ListGroupAccountsRes, error) {
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("listGroupAccounts|%s|%s|%s", params.Type.Value, params.Role.Value, params.Elimination.Value)
	var after accountCursor
	if params.PageToken.Set {
		if err := e.Pages.Decode(scope, params.PageToken.Value, &after); err != nil {
			return nil, err
		}
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("consolidation: group accounts: %w", err)
	}
	mapped, err := e.mapped(ctx, doc)
	if err != nil {
		return nil, err
	}
	cs := doc.Consolidation
	accounts := slices.Clone(cs.Accounts)
	slices.SortFunc(accounts, func(a, b config.GroupAccount) int { return strings.Compare(a.Number, b.Number) })
	page := gen.GroupAccountPage{Items: []gen.GroupAccount{}}
	for _, a := range accounts {
		elimination := cs.EliminationOf(a.Number)
		if params.PageToken.Set && a.Number <= after.Number || !matches(params, a, elimination) {
			continue
		}
		if len(page.Items) == size {
			tok, _ := e.Pages.Encode(scope, accountCursor{Number: page.Items[size-1].Number}) // a string always encodes
			page.NextPageToken.SetTo(tok)
			break
		}
		page.Items = append(page.Items, accountToAPI(cs, a, elimination, mapped[a.Number]))
	}
	return &page, nil
}

// matches reports whether a group account passes the list's filters.
func matches(params gen.ListGroupAccountsParams, a config.GroupAccount, elimination string) bool {
	return (!params.Type.Set || string(a.Type) == params.Type.Value) && (!params.Role.Set || a.Role == params.Role.Value) &&
		(!params.Elimination.Set || elimination == params.Elimination.Value)
}

// mapped counts the company accounts mapped to each group account, in the
// document's companies whose legal entity the caller may act in, so a
// company outside the caller's access reaches no figure (ADR-0010).
func (e *Endpoints) mapped(ctx context.Context, doc *config.Config) (map[string]int32, error) {
	companies, err := e.companiesByCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("consolidation: group accounts: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("consolidation: group accounts: %w", err)
	}
	out := map[string]int32{}
	for _, co := range doc.Companies {
		if le, ok := companies[co.Code]; !ok || !access.Allows(le.ID) {
			continue
		}
		for _, a := range co.ChartOfAccounts {
			if a.GroupAccount != "" {
				out[a.GroupAccount]++
			}
		}
	}
	return out, nil
}

// accountToAPI is a group account with its role, its elimination by code
// and name, and its mapped accounts.
func accountToAPI(cs config.Consolidation, a config.GroupAccount, elimination string, mapped int32) gen.GroupAccount {
	out := gen.GroupAccount{Number: a.Number, Name: a.Name, Type: string(a.Type), MappedAccounts: mapped}
	if a.Role != "" {
		out.Role.SetTo(a.Role)
	}
	for _, el := range cs.Eliminations {
		if el.Code == elimination && elimination != "" {
			out.Elimination.SetTo(gen.EliminationRef{Code: el.Code, Name: el.Name})
		}
	}
	return out
}
