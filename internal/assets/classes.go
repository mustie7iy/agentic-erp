package assets

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

// classCursor is where a page of asset classes ends.
type classCursor struct {
	Code string `json:"c"`
}

// ListAssetClasses implements GET /v1/asset-classes (ADR-0057 §3, §10):
// the active document's classes in code order, each with its terms per
// ledger and the accounts of each company that uses it and that the caller
// may act in. Classes are configuration, so a page is cut from the
// document rather than read from a table.
func (e *Endpoints) ListAssetClasses(ctx context.Context, params gen.ListAssetClassesParams) (gen.ListAssetClassesRes, error) {
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	entity := apitypes.Ptr[uuid.UUID](params.LegalEntityID)
	scope := fmt.Sprintf("listAssetClasses|%s", entity)
	var after string
	if params.PageToken.Set {
		var c classCursor
		if err := e.Pages.Decode(scope, params.PageToken.Value, &c); err != nil {
			return nil, err
		}
		after = c.Code
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("assets: list asset classes: %w", err)
	}
	companies, err := e.users(ctx, doc, entity)
	if err != nil {
		return nil, err
	}
	page := gen.AssetClassPage{Items: []gen.AssetClass{}}
	for _, cls := range sortedClasses(doc) {
		users := usersOf(cls.Code, companies)
		if params.PageToken.Set && cls.Code <= after || entity != nil && len(users) == 0 {
			continue
		}
		if len(page.Items) == size {
			tok, _ := e.Pages.Encode(scope, classCursor{Code: page.Items[size-1].Code}) // a string always encodes
			page.NextPageToken.SetTo(tok)
			break
		}
		page.Items = append(page.Items, classToAPI(doc, cls, users))
	}
	return &page, nil
}

// user is a company of the document and its legal entity.
type user struct {
	company Company
	co      *config.Company
}

// users is the document's companies that the caller may act in, entity's
// alone when it is given, each with its legal entity; a company without
// one is left out (a document is activated only for companies with one).
func (e *Endpoints) users(ctx context.Context, doc *config.Config, entity *uuid.UUID) ([]user, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("assets: list asset classes: %w", err)
	}
	var out []user
	for i := range doc.Companies {
		co := &doc.Companies[i]
		le, ok, err := e.Companies.ByCode(ctx, co.Code)
		if err != nil {
			return nil, fmt.Errorf("assets: legal entity of company %s: %w", co.Code, err)
		}
		if ok && access.Allows(le.ID) && (entity == nil || *entity == le.ID) {
			out = append(out, user{company: le, co: co})
		}
	}
	return out, nil
}

// usersOf is the class's accounts in each of the users that names them.
func usersOf(class string, users []user) []gen.AssetClassCompany {
	var out []gen.AssetClassCompany
	for _, u := range users {
		if a := u.co.AssetAccountsOf(class); a != nil {
			out = append(out, gen.AssetClassCompany{LegalEntityID: u.company.ID, CompanyCode: u.company.Code, CompanyName: u.company.Name,
				Accounts: accountsToAPI(u.co, a)})
		}
	}
	return out
}

// sortedClasses is the document's classes in code order.
func sortedClasses(doc *config.Config) []config.AssetClass {
	out := slices.Clone(doc.AssetClasses)
	slices.SortFunc(out, func(a, b config.AssetClass) int { return strings.Compare(a.Code, b.Code) })
	return out
}

// classToAPI is a class with its terms, by the ledgers' names, and its
// companies' accounts.
func classToAPI(doc *config.Config, cls config.AssetClass, companies []gen.AssetClassCompany) gen.AssetClass {
	names := map[string]string{}
	for _, l := range doc.LedgerList() {
		names[l.Code] = l.Name
	}
	out := gen.AssetClass{Code: cls.Code, Name: cls.Name, Terms: make([]gen.AssetClassTerms, len(cls.Terms)), Companies: companies}
	if out.Companies == nil {
		out.Companies = []gen.AssetClassCompany{}
	}
	for i, t := range cls.Terms {
		out.Terms[i] = gen.AssetClassTerms{LedgerCode: t.Ledger, LedgerName: names[t.Ledger], Method: t.Method,
			UsefulLifeMonths: int32(t.UsefulLifeMonths)}
		if t.DecliningFactor != "" {
			out.Terms[i].DecliningFactor.SetTo(t.DecliningFactor)
		}
	}
	return out
}

// accountsToAPI is the company's accounts of a class by number and name;
// validation keeps each in the company's chart.
func accountsToAPI(co *config.Company, a *config.AssetAccounts) gen.AssetClassAccounts {
	names := map[string]string{}
	for _, acc := range co.ChartOfAccounts {
		names[acc.Number] = acc.Name
	}
	ref := func(number string) gen.AssetAccountRef {
		return gen.AssetAccountRef{Number: number, Name: names[number]}
	}
	return gen.AssetClassAccounts{AcquisitionCost: ref(a.AcquisitionCost), AccumulatedDepreciation: ref(a.AccumulatedDepreciation),
		Depreciation: ref(a.Depreciation), Clearing: ref(a.Clearing), Gain: ref(a.Gain), Loss: ref(a.Loss), SaleClearing: ref(a.SaleClearing)}
}
