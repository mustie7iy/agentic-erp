package consolidation

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

// groupCursor is where a page of groups ends.
type groupCursor struct {
	Code string `json:"c"`
}

// groupReader is what a read of groups starts from: the active document,
// the tenant's legal entities by code, the caller's company access and the
// group currency.
type groupReader struct {
	doc       *config.Config
	companies map[string]Company
	access    authz.Access
	currency  string
}

// reader reads what every group read needs, once.
func (e *Endpoints) reader(ctx context.Context) (groupReader, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return groupReader{}, fmt.Errorf("consolidation: groups: %w", err)
	}
	companies, err := e.companiesByCode(ctx)
	if err != nil {
		return groupReader{}, fmt.Errorf("consolidation: groups: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return groupReader{}, fmt.Errorf("consolidation: groups: %w", err)
	}
	currency, err := e.Companies.GroupCurrency(ctx)
	if err != nil {
		return groupReader{}, fmt.Errorf("consolidation: groups: %w", err)
	}
	return groupReader{doc: doc, companies: companies, access: access, currency: currency}, nil
}

// ListConsolidationGroups implements GET /v1/consolidation-groups
// (ADR-0059 §3): the active document's groups in code order that the
// caller may see whole (visible). Groups are configuration, so a page is
// cut from the document rather than read from a table.
func (e *Endpoints) ListConsolidationGroups(ctx context.Context, params gen.ListConsolidationGroupsParams) (gen.ListConsolidationGroupsRes, error) {
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	const scope = "listConsolidationGroups"
	var after groupCursor
	if params.PageToken.Set {
		if err := e.Pages.Decode(scope, params.PageToken.Value, &after); err != nil {
			return nil, err
		}
	}
	r, err := e.reader(ctx)
	if err != nil {
		return nil, err
	}
	groups := slices.Clone(r.doc.Consolidation.Groups)
	slices.SortFunc(groups, func(a, b config.ConsolidationGroup) int { return strings.Compare(a.Code, b.Code) })
	page := gen.ConsolidationGroupPage{Items: []gen.ConsolidationGroup{}}
	for _, g := range groups {
		if params.PageToken.Set && g.Code <= after.Code || !visible(g, r.companies, r.access) {
			continue
		}
		if len(page.Items) == size {
			tok, _ := e.Pages.Encode(scope, groupCursor{Code: page.Items[size-1].Code}) // a string always encodes
			page.NextPageToken.SetTo(tok)
			break
		}
		page.Items = append(page.Items, r.toAPI(g))
	}
	return &page, nil
}

// GetConsolidationGroup implements GET /v1/consolidation-groups/{code}
// (ADR-0059 §3): one group of the active document, unknown to a caller who
// may not see it whole (404 consolidation.group_unknown, §12).
func (e *Endpoints) GetConsolidationGroup(ctx context.Context, params gen.GetConsolidationGroupParams) (gen.GetConsolidationGroupRes, error) {
	r, err := e.reader(ctx)
	if err != nil {
		return nil, err
	}
	g := r.doc.Consolidation.Group(params.Code)
	if g == nil || !visible(*g, r.companies, r.access) {
		return nil, groupUnknown(params.Code)
	}
	out := r.toAPI(*g)
	return &out, nil
}

// toAPI is a visible group with its companies named, its members in
// company code order and its net investment designations by company,
// account and trading partner; visible has found each company's legal
// entity, and validation keeps each investment account in the parent's
// chart, each first period well formed and each designation's companies
// in the group and its account in its company's chart.
func (r groupReader) toAPI(g config.ConsolidationGroup) gen.ConsolidationGroup {
	company := func(code string) gen.ConsolidationCompany {
		le := r.companies[code]
		return gen.ConsolidationCompany{LegalEntityID: le.ID, CompanyCode: le.Code, CompanyName: le.Name, LocalCurrency: le.LocalCurrency}
	}
	names := map[string]string{}
	for _, a := range r.doc.Company(g.Parent).ChartOfAccounts {
		names[a.Number] = a.Name
	}
	investments := slices.Clone(g.NetInvestments)
	slices.SortFunc(investments, func(a, b config.NetInvestment) int {
		return cmp.Or(strings.Compare(a.Company, b.Company), strings.Compare(a.Account, b.Account), strings.Compare(a.Partner, b.Partner))
	})
	members := slices.Clone(g.Members)
	slices.SortFunc(members, func(a, b config.GroupMember) int { return strings.Compare(a.Company, b.Company) })
	out := gen.ConsolidationGroup{Code: g.Code, Name: g.Name, GroupCurrency: r.currency, Parent: company(g.Parent),
		Members: make([]gen.ConsolidationMember, len(members)), NetInvestments: make([]gen.ConsolidationNetInvestment, len(investments))}
	for i, n := range investments {
		// Validated: the account is in the company's chart.
		out.NetInvestments[i] = gen.ConsolidationNetInvestment{Company: company(n.Company), Partner: company(n.Partner),
			Account: gen.AssetAccountRef{Number: n.Account, Name: r.doc.Company(n.Company).Account(n.Account).Name}}
	}
	for i, m := range members {
		c := company(m.Company)
		year, period, _ := m.First() // validated: well formed
		investment := gen.AssetAccountRef{Number: m.InvestmentAccount, Name: names[m.InvestmentAccount]}
		out.Members[i] = gen.ConsolidationMember{LegalEntityID: c.LegalEntityID, CompanyCode: c.CompanyCode, CompanyName: c.CompanyName,
			LocalCurrency: c.LocalCurrency, OwnershipPercent: m.OwnershipPercent, Origin: m.Origin, FirstFiscalYear: int32(year), FirstFiscalPeriod: int32(period),
			InvestmentAccount: investment}
	}
	return out
}
