package partner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func partnerToAPI(p Partner) gen.BusinessPartner {
	out := gen.BusinessPartner{
		ID: p.ID, Version: p.Version, CreatedAt: p.CreatedAt, CreatedBy: p.CreatedBy, UpdatedAt: p.UpdatedAt, UpdatedBy: gen.NewOptUUID(p.UpdatedBy),
		Number: p.Number, Kind: p.Kind, Name: p.Name, LegalName: apitypes.OptString(p.LegalName), CountryCode: p.CountryCode,
		Language: apitypes.OptString(p.Language), Status: p.Status, Roles: p.Roles, SupplierCompanies: []gen.SupplierCompany{},
		Addresses: []gen.Address{}, TaxIdentifiers: []gen.TaxIdentifier{}, Extensions: apitypes.EncodeObject(p.Extensions),
	}
	for _, s := range p.SupplierCompanies {
		out.SupplierCompanies = append(out.SupplierCompanies, gen.SupplierCompany(s))
	}
	for _, a := range p.Addresses {
		ga := gen.Address{ID: gen.NewOptUUID(a.ID), Purpose: a.Purpose, Line1: a.Line1, Line2: apitypes.OptString(a.Line2), City: a.City,
			PostalCode: apitypes.OptString(a.PostalCode), Region: apitypes.OptString(a.Region), CountryCode: a.CountryCode, ValidFrom: a.ValidFrom.Time}
		if a.ValidTo != nil {
			ga.ValidTo.SetTo(a.ValidTo.Time)
		}
		out.Addresses = append(out.Addresses, ga)
	}
	for _, t := range p.TaxIdentifiers {
		out.TaxIdentifiers = append(out.TaxIdentifiers, gen.TaxIdentifier(t))
	}
	return out
}

func changeToAPI(c Change) gen.PartnerChange {
	out := gen.PartnerChange{
		ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt, UpdatedBy: gen.NewOptUUID(c.UpdatedBy),
		Number: c.Number, Kind: c.Kind, PartnerID: c.PartnerID, Payload: apitypes.EncodeObject(c.Masked()), Reason: c.Reason,
		Warnings: []gen.PartnerChangeWarning{}, Status: c.Status, RequestedBy: c.RequestedBy,
	}
	for _, w := range c.Warnings {
		out.Warnings = append(out.Warnings, gen.PartnerChangeWarning(w))
	}
	if c.WorkflowRequestID != nil {
		out.WorkflowRequestID.SetTo(*c.WorkflowRequestID)
	}
	if c.AppliedAt != nil {
		out.AppliedAt.SetTo(*c.AppliedAt)
	}
	return out
}

func bankToAPI(b BankAccount) gen.BankAccount {
	out := gen.BankAccount{ID: b.ID, ValidFrom: b.ValidFrom, CreatedAt: b.CreatedAt, CreatedBy: b.CreatedBy, PartnerID: b.PartnerID,
		Purpose: b.Purpose, AccountLast4: b.AccountLast4, AccountScheme: b.AccountScheme, Bic: apitypes.OptString(b.BIC),
		BankCountryCode: b.BankCountryCode, HolderName: b.HolderName, PartnerChangeID: b.PartnerChangeID}
	if b.ValidTo != nil {
		out.ValidTo.SetTo(*b.ValidTo)
	}
	if b.EndedAt != nil {
		out.EndedAt.SetTo(*b.EndedAt)
	}
	if b.EndedBy != nil {
		out.EndedBy.SetTo(*b.EndedBy)
	}
	return out
}

func bankTag(b BankAccount) string {
	v := bankToAPI(b)
	return etag.Of(&v)
}

func changeTag(c Change) gen.OptString { return gen.NewOptString(etag.Format(c.Version)) }

func companiesIn(in []gen.SupplierCompanyInput) []SupplierCompanyInput {
	out := []SupplierCompanyInput{}
	for _, s := range in {
		out = append(out, SupplierCompanyInput(s))
	}
	return out
}

func addressesIn(in []gen.Address) []AddressInput {
	out := []AddressInput{}
	for _, a := range in {
		ai := AddressInput{Purpose: a.Purpose, Line1: a.Line1, Line2: apitypes.Ptr[string](a.Line2), City: a.City, PostalCode: apitypes.Ptr[string](a.PostalCode),
			Region: apitypes.Ptr[string](a.Region), CountryCode: a.CountryCode, ValidFrom: Date{a.ValidFrom}}
		if a.ValidTo.Set {
			ai.ValidTo = &Date{a.ValidTo.Value}
		}
		out = append(out, ai)
	}
	return out
}

func taxesIn(in []gen.TaxIdentifierInput) []TaxInput {
	out := []TaxInput{}
	for _, t := range in {
		out = append(out, TaxInput(t))
	}
	return out
}

// CreateBusinessPartner implements POST /v1/business-partners.
func (e *Endpoints) CreateBusinessPartner(ctx context.Context, req *gen.BusinessPartnerCreate, _ gen.CreateBusinessPartnerParams) (gen.CreateBusinessPartnerRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	in := NewPartner{Kind: req.Kind, Name: req.Name, LegalName: apitypes.Ptr[string](req.LegalName), CountryCode: req.CountryCode,
		Language: apitypes.Ptr[string](req.Language), Roles: req.Roles, SupplierCompanies: companiesIn(req.SupplierCompanies),
		Addresses: addressesIn(req.Addresses), TaxIdentifiers: taxesIn(req.TaxIdentifiers), Extensions: apitypes.DecodeObject(req.Extensions.Or(nil)), Reason: req.Reason}
	for _, b := range req.BankAccounts {
		in.BankAccounts = append(in.BankAccounts, BankInput{Purpose: b.Purpose.Value, AccountNumber: b.AccountNumber, AccountScheme: b.AccountScheme,
			BIC: apitypes.Ptr[string](b.Bic), BankCountryCode: b.BankCountryCode, HolderName: b.HolderName, ValidFrom: b.ValidFrom})
	}
	p, c, err := CreatePartner(ctx, e.ports(), in)
	if err != nil {
		return nil, err
	}
	if _, err := RequestApproval(ctx, e.Approvals, e.Schemas, c); err != nil {
		return nil, err
	}
	if p, err = getPartner(ctx, tx, p.ID, false); err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	return &gen.BusinessPartnerHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Location: gen.NewOptString("/v1/business-partners/" + p.ID.String()),
		Response: partnerToAPI(visible(p, access))}, nil
}

// GetBusinessPartner implements GET /v1/business-partners/{id}.
func (e *Endpoints) GetBusinessPartner(ctx context.Context, params gen.GetBusinessPartnerParams) (gen.GetBusinessPartnerRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	p, err := getPartner(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	return &gen.GetBusinessPartnerOKHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: partnerToAPI(visible(p, access))}, nil
}

var partnerSort = pagination.Sort{Default: "number", Fields: []pagination.Field{
	{Name: "number", Column: "number", Type: "text"}, {Name: "name", Column: "name", Type: "text"},
}}

// likePrefix escapes LIKE wildcards in a prefix.
func likePrefix(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
}

// ListBusinessPartners implements GET /v1/business-partners.
func (e *Endpoints) ListBusinessPartners(ctx context.Context, params gen.ListBusinessPartnersParams) (gen.ListBusinessPartnersRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	scope := "listBusinessPartners|" + params.Number.Value + "|" + params.NamePrefix.Value + "|" + params.Role.Value + "|" + params.Status.Value
	ks, err := e.Pages.Keyset(partnerSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if params.NamePrefix.Set {
		prefix = likePrefix(strings.ToUpper(strings.Join(strings.Fields(params.NamePrefix.Value), " ")))
	}
	q, args := ks.SQL(partnerSelect+` WHERE ($1 = '' OR p.number = $1) AND ($2 = '' OR p.name_norm LIKE $2)
		AND ($3 = '' OR EXISTS (SELECT FROM partner.partner_role r WHERE r.tenant_id = p.tenant_id AND r.partner_id = p.id AND r.role = $3))
		AND ($4 = '' OR p.status = $4)`, []any{params.Number.Value, prefix, params.Role.Value, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Partner, error) { return scanPartner(r) })
	if err != nil {
		return nil, fmt.Errorf("partner: list business partners: %w", err)
	}
	items, next := apitypes.PageOf(ks, ps, func(p Partner) (string, uuid.UUID) {
		if ks.Field() == "name" {
			return p.Name, p.ID
		}
		return p.Number, p.ID
	}, func(p Partner) gen.BusinessPartner { return partnerToAPI(visible(p, access)) })
	return &gen.BusinessPartnerPageHeaders{Response: gen.BusinessPartnerPage{Items: items, NextPageToken: next}}, nil
}

// BlockBusinessPartner implements POST /v1/business-partners/{id}:block.
func (e *Endpoints) BlockBusinessPartner(ctx context.Context, req *gen.ReasonInput, params gen.BlockBusinessPartnerParams) (gen.BlockBusinessPartnerRes, error) {
	c, err := Block(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return &gen.RequestBankAccountEndOKHeaders{ETag: changeTag(c), Response: changeToAPI(c)}, nil
}

// UnblockBusinessPartner implements POST /v1/business-partners/{id}:unblock.
func (e *Endpoints) UnblockBusinessPartner(ctx context.Context, req *gen.ReasonInput, params gen.UnblockBusinessPartnerParams) (gen.UnblockBusinessPartnerRes, error) {
	c, err := Unblock(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err == nil {
		c, err = RequestApproval(ctx, e.Approvals, e.Schemas, c)
	}
	if err != nil {
		return nil, err
	}
	return &gen.RequestBankAccountEndOKHeaders{ETag: changeTag(c), Response: changeToAPI(c)}, nil
}

// CreatePartnerChange implements POST /v1/partner-changes.
func (e *Endpoints) CreatePartnerChange(ctx context.Context, req *gen.PartnerChangeCreate, _ gen.CreatePartnerChangeParams) (gen.CreatePartnerChangeRes, error) {
	if req.Kind != KindUpdate {
		return nil, invalid("partner.unsupported_kind", "kind", "only update; other kinds have their own operations")
	}
	pl := req.Payload
	ch := Changes{Name: apitypes.Ptr[string](pl.Name), LegalName: apitypes.OptNilString(pl.LegalName), Language: apitypes.OptNilString(pl.Language), Roles: pl.Roles,
		SupplierCompanies: companiesIn(pl.SupplierCompanies), Addresses: addressesIn(pl.Addresses), TaxIdentifiers: taxesIn(pl.TaxIdentifiers),
		Extensions: apitypes.DecodeObject(pl.Extensions.Or(nil))}
	c, err := RequestUpdate(ctx, e.ports(), req.PartnerID, ch, req.Reason)
	if err == nil {
		c, err = RequestApproval(ctx, e.Approvals, e.Schemas, c)
	}
	if err != nil {
		return nil, err
	}
	return &gen.PartnerChangeHeaders{ETag: changeTag(c), Location: gen.NewOptString("/v1/partner-changes/" + c.ID.String()), Response: changeToAPI(c)}, nil
}

// GetPartnerChange implements GET /v1/partner-changes/{id}.
func (e *Endpoints) GetPartnerChange(ctx context.Context, params gen.GetPartnerChangeParams) (gen.GetPartnerChangeRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := getChange(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetPartnerChangeOKHeaders{ETag: changeTag(c), Response: changeToAPI(c)}, nil
}

var changeSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListPartnerChanges implements GET /v1/partner-changes.
func (e *Endpoints) ListPartnerChanges(ctx context.Context, params gen.ListPartnerChangesParams) (gen.ListPartnerChangesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	var partnerID *uuid.UUID
	if params.PartnerID.Set {
		partnerID = &params.PartnerID.Value
	}
	scope := "listPartnerChanges|" + params.PartnerID.Value.String() + "|" + params.Kind.Value + "|" + params.Status.Value
	ks, err := e.Pages.Keyset(changeSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+changeColumns+` FROM partner.partner_change WHERE ($1::uuid IS NULL OR partner_id = $1)
		AND ($2 = '' OR kind = $2) AND ($3 = '' OR status = $3)`, []any{partnerID, params.Kind.Value, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Change, error) { return scanChange(r) })
	if err != nil {
		return nil, fmt.Errorf("partner: list partner changes: %w", err)
	}
	items, next := apitypes.PageOf(ks, cs, func(c Change) (string, uuid.UUID) { return c.CreatedAt.Format(time.RFC3339Nano), c.ID }, changeToAPI)
	return &gen.PartnerChangePageHeaders{Response: gen.PartnerChangePage{Items: items, NextPageToken: next}}, nil
}

// RequestBankAccountChange implements POST /v1/business-partners/{id}/bank-accounts.
func (e *Endpoints) RequestBankAccountChange(ctx context.Context, req *gen.BankChangeCreate, params gen.RequestBankAccountChangeParams) (gen.RequestBankAccountChangeRes, error) {
	c, err := RequestBankAccount(ctx, e.ports(), params.ID, BankInput{Purpose: req.Purpose.Value, AccountNumber: req.AccountNumber,
		AccountScheme: req.AccountScheme, BIC: apitypes.Ptr[string](req.Bic), BankCountryCode: req.BankCountryCode, HolderName: req.HolderName,
		ValidFrom: req.ValidFrom}, req.Reason)
	if err == nil {
		c, err = RequestApproval(ctx, e.Approvals, e.Schemas, c)
	}
	if err != nil {
		return nil, err
	}
	return &gen.PartnerChangeHeaders{ETag: changeTag(c), Location: gen.NewOptString("/v1/partner-changes/" + c.ID.String()), Response: changeToAPI(c)}, nil
}

// RequestBankAccountEnd implements POST /v1/business-partners/{id}/bank-accounts/{bank_account_id}:end.
func (e *Endpoints) RequestBankAccountEnd(ctx context.Context, req *gen.BankAccountEnd, params gen.RequestBankAccountEndParams) (gen.RequestBankAccountEndRes, error) {
	c, err := RequestBankAccountEnd(ctx, params.ID, params.BankAccountID, func(b BankAccount) error { return etag.Match(params.IfMatch, bankTag(b)) },
		req.ValidTo, req.Reason)
	if err == nil {
		c, err = RequestApproval(ctx, e.Approvals, e.Schemas, c)
	}
	if err != nil {
		return nil, err
	}
	return &gen.RequestBankAccountEndOKHeaders{ETag: changeTag(c), Response: changeToAPI(c)}, nil
}

// GetBankAccount implements GET /v1/business-partners/{id}/bank-accounts/{bank_account_id}.
func (e *Endpoints) GetBankAccount(ctx context.Context, params gen.GetBankAccountParams) (gen.GetBankAccountRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	b, err := getBank(ctx, tx, params.ID, params.BankAccountID)
	if err != nil {
		return nil, err
	}
	return &gen.BankAccountHeaders{ETag: gen.NewOptString(bankTag(b)), Response: bankToAPI(b)}, nil
}

var bankSort = pagination.Sort{Default: "valid_from", Fields: []pagination.Field{{Name: "valid_from", Column: "valid_from", Type: "date"}}}

// ListBankAccounts implements GET /v1/business-partners/{id}/bank-accounts.
func (e *Endpoints) ListBankAccounts(ctx context.Context, params gen.ListBankAccountsParams) (gen.ListBankAccountsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	var validOn *time.Time
	scope := "listBankAccounts|" + params.ID.String() + "|"
	if params.ValidOn.Set {
		validOn = &params.ValidOn.Value
		scope += params.ValidOn.Value.Format(time.DateOnly)
	}
	ks, err := e.Pages.Keyset(bankSort, scope, apitypes.Page(gen.OptString{}, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM partner.business_partner WHERE id = $1)`, params.ID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("partner: find business partner %s: %w", params.ID, err)
	}
	if !exists {
		return nil, problem.NotFoundOf("business partner")
	}
	q, args := ks.SQL(`SELECT `+bankColumns+` FROM partner.bank_account WHERE partner_id = $1
		AND ($2::date IS NULL OR (valid_from <= $2 AND (valid_to IS NULL OR valid_to > $2)))`, []any{params.ID, validOn})
	rows, _ := tx.Query(ctx, q, args...)
	bs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BankAccount, error) { return scanBank(r) })
	if err != nil {
		return nil, fmt.Errorf("partner: list bank accounts of business partner %s: %w", params.ID, err)
	}
	items, next := apitypes.PageOf(ks, bs, func(b BankAccount) (string, uuid.UUID) { return b.ValidFrom.Format(time.DateOnly), b.ID }, bankToAPI)
	return &gen.BankAccountPageHeaders{Response: gen.BankAccountPage{Items: items, NextPageToken: next}}, nil
}
