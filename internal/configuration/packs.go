package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// packCursor is where a page of country packs ends.
type packCursor struct {
	Country string `json:"c"`
	Version string `json:"v"`
}

// ListCountryPacks implements GET /v1/country-packs. The packs are the
// image's and the same for every tenant, so a page is cut from the library
// in its order (country, then version) rather than read from a table.
func (e *Endpoints) ListCountryPacks(ctx context.Context, params gen.ListCountryPacksParams) (gen.ListCountryPacksRes, error) {
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	scope := "listCountryPacks|" + params.Country.Value
	var after *countrypack.Pack
	if params.PageToken.Set {
		var c packCursor
		if err := e.Pages.Decode(scope, params.PageToken.Value, &c); err != nil {
			return nil, err
		}
		after = &countrypack.Pack{Manifest: countrypack.Manifest{Country: c.Country, Version: c.Version}}
	}
	page := gen.CountryPackPage{Items: []gen.CountryPack{}}
	for _, p := range e.Packs.All() {
		if (params.Country.Set && p.Manifest.Country != params.Country.Value) || (after != nil && countrypack.Order(p, after) <= 0) {
			continue
		}
		if len(page.Items) == size {
			last := page.Items[size-1]
			tok, _ := e.Pages.Encode(scope, packCursor{Country: last.Country, Version: last.Version}) // strings always encode
			page.NextPageToken.SetTo(tok)
			break
		}
		page.Items = append(page.Items, packToAPI(e.Packs, p))
	}
	return &gen.CountryPackPageHeaders{Response: page}, nil
}

func packToAPI(lib *countrypack.Library, p *countrypack.Pack) gen.CountryPack {
	m := p.Manifest
	from, _ := time.Parse(time.DateOnly, m.ValidFrom) // a loaded pack's dates are valid
	out := gen.CountryPack{Country: m.Country, Version: m.Version, Name: m.Name, ValidFrom: from, Hash: lib.Hash(p), SignedOff: p.SignedOff(),
		Sources: make([]gen.CountryPackSource, 0, len(m.Sources)),
		Contents: gen.CountryPackContents{Accounts: int32(len(p.Accounts.Accounts)), TaxCodes: int32(len(p.Tax.TaxCodes)),
			Holidays: int32(len(p.Calendar.Holidays)), NumberRanges: int32(len(p.Numbering.NumberRanges)), PaymentTerms: int32(len(p.Payments.PaymentTerms))}}
	if p.SignedOff() {
		out.SignOff.SetTo(gen.CountryPackSignOff{TaxSpecialist: m.SignOff.TaxSpecialist, Date: m.SignOff.Date})
	}
	for _, s := range m.Sources {
		out.Sources = append(out.Sources, gen.CountryPackSource{Title: s.Title, URL: s.URL, Retrieved: s.Retrieved})
	}
	return out
}

// ApplyCountryPack implements POST /v1/country-pack-applications.
func (e *Endpoints) ApplyCountryPack(ctx context.Context, req *gen.CountryPackApplication, _ gen.ApplyCountryPackParams) (gen.ApplyCountryPackRes, error) {
	c, err := ApplyCountryPack(ctx, e.Dependencies, e.Packs, PackApplication{Country: req.Country, Version: req.Version, Company: req.Company,
		On: req.AppliedOn, FiscalYear: int(req.FiscalYear), Title: req.Title.Value, Rationale: req.Rationale.Value})
	if err != nil {
		return nil, err
	}
	return crCreated(c), nil
}

// PackApplication is the input of ApplyCountryPack.
type PackApplication struct {
	Country, Version string
	Company          string
	On               time.Time // the date of application
	FiscalYear       int       // the year the pack's number ranges are created for
	Title, Rationale string    // an empty title names the pack and the company
}

// ApplyCountryPack merges a pack version into the active document for one
// company (countrypack.Apply: three-way, from the version the company
// adopted) and stores the draft as a change request based on the active
// version, with the merge's notes for the reviewers. It activates nothing:
// the request is submitted, evidenced, approved and activated like any
// other, so its tier is the draft's (ADR-0048). The active version stays
// locked until the request is stored, as for every new request.
func ApplyCountryPack(ctx context.Context, deps Dependencies, lib *countrypack.Library, in PackApplication) (ChangeRequest, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return ChangeRequest{}, err
	}
	pack := lib.Get(in.Country, in.Version)
	if pack == nil {
		return ChangeRequest{}, problem.NotFoundOf("country pack")
	}
	if err := accessible(ctx, deps.LegalEntities, in.Company); err != nil {
		return ChangeRequest{}, err
	}
	active, err := activeVersion(ctx, tx)
	if err != nil {
		return ChangeRequest{}, err
	}
	// Read strictly: a draft written back without keys this replica does
	// not know would remove them.
	cur, err := assessableVersion(ctx, tx, active)
	if err != nil {
		return ChangeRequest{}, err
	}
	draft, notes, err := countrypack.Apply(cur, pack, lib, countrypack.Options{Company: in.Company, On: in.On, FiscalYear: in.FiscalYear})
	if err != nil {
		return ChangeRequest{}, packProblem(err)
	}
	proposed, _ := json.Marshal(draft) // Config holds only JSON-safe types
	title := in.Title
	if title == "" {
		title = fmt.Sprintf("Apply the %s country pack %s to company %s", pack.Manifest.Country, pack.Manifest.Version, in.Company)
	}
	return create(ctx, NewChangeRequest{Title: title, Rationale: in.Rationale, BaseVersion: active, Proposed: proposed}, notes,
		map[string]any{"notes": len(notes), "country_pack": map[string]any{"country": pack.Manifest.Country, "version": pack.Manifest.Version,
			"hash": lib.Hash(pack), "company": in.Company, "applied_on": in.On.Format(time.DateOnly), "fiscal_year": in.FiscalYear}})
}

// accessible refuses a company code of no legal entity and one of a legal
// entity outside the caller's company access alike, so codes cannot be
// probed (04-api-design §4.9).
func accessible(ctx context.Context, les LegalEntities, company string) error {
	access, err := authz.Companies(ctx)
	if err != nil {
		return err
	}
	id, _, ok, err := les.LegalEntityByCode(ctx, company)
	if err != nil {
		return err
	}
	if !ok || !access.Allows(id) {
		return problem.Unprocessable("config.unknown_company", "There is no legal entity with this company code that you have access to.",
			problem.FieldError{Field: "company", Code: "not_found", Message: "the company code of a legal entity you have access to"})
	}
	return nil
}

// packProblem answers what the caller can correct; anything else (an
// adopted version missing from the image, which the hash list prevents) is
// the server's.
func packProblem(err error) error {
	switch {
	case errors.Is(err, countrypack.ErrNotApplicable):
		return problem.Unprocessable("config.pack_not_applicable", "The country pack does not apply to this company.",
			problem.FieldError{Field: "company", Code: "not_applicable", Message: strings.TrimPrefix(err.Error(), countrypack.ErrNotApplicable.Error()+": ")})
	case errors.Is(err, countrypack.ErrDowngrade):
		return problem.Conflict("config.pack_downgrade", "The company adopted a newer version of this country pack; a pack is never downgraded.")
	}
	return fmt.Errorf("configuration: apply country pack: %w", err)
}
