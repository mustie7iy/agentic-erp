package partner

import (
	"context"
	"errors"
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
		CustomerCompanies: []gen.CustomerCompany{}, Addresses: []gen.Address{}, TaxIdentifiers: []gen.TaxIdentifier{}, Extensions: apitypes.EncodeObject(p.Extensions),
	}
	for _, s := range p.SupplierCompanies {
		out.SupplierCompanies = append(out.SupplierCompanies, gen.SupplierCompany{LegalEntityID: s.LegalEntityID,
			ReconciliationAccountID: s.ReconciliationAccountID, PaymentTermCode: s.PaymentTermCode, IsPostingBlocked: s.IsPostingBlocked,
			IsPaymentBlocked: s.IsPaymentBlocked, WithholdingTaxCodes: s.WithholdingTaxCodes, PaymentMethods: s.PaymentMethods,
			PlanningGroup: apitypes.OptString(s.PlanningGroup)})
	}
	for _, c := range p.CustomerCompanies {
		out.CustomerCompanies = append(out.CustomerCompanies, gen.CustomerCompany{LegalEntityID: c.LegalEntityID,
			ReconciliationAccountID: c.ReconciliationAccountID, PaymentTermCode: c.PaymentTermCode, IsPostingBlocked: c.IsPostingBlocked})
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
	if p.GroupCompanyID != nil {
		out.GroupCompanyID = gen.NewOptUUID(*p.GroupCompanyID)
		out.GroupCompany = gen.NewOptGroupCompany(gen.GroupCompany{ID: *p.GroupCompanyID})
	}
	out.PersonalDataBlockedAt = apitypes.OptDateTime(p.PersonalDataBlockedAt)
	out.PersonalDataDeletedAt = apitypes.OptDateTime(p.PersonalDataDeletedAt)
	return out
}

// nameGroupCompanies gives each partner's group company the caller may
// act in (access, ADR-0010) its company code and its name as tenancy has
// them now, reading each legal entity once; one outside access keeps its
// id alone (product owner 2026-11-05), as a journal line's trading partner
// (ADR-0059 As built, T68b). The ETag is the partner's version, so neither
// renaming a company nor who reads it changes it. A group company was a
// legal entity of the tenant when it was set, and legal entities are
// never deleted, so one tenancy does not know is an error.
func nameGroupCompanies(ctx context.Context, les LegalEntities, access authz.Access, reps ...*gen.BusinessPartner) error {
	names := map[uuid.UUID]gen.GroupCompany{}
	for _, rep := range reps {
		gc, ok := rep.GroupCompany.Get()
		if !ok || !access.Allows(gc.ID) {
			continue
		}
		named, seen := names[gc.ID]
		if !seen {
			le, found, err := les.Entity(ctx, gc.ID)
			if err != nil {
				return fmt.Errorf("partner: name group company %s: %w", gc.ID, err)
			}
			if !found {
				return fmt.Errorf("partner: name group company %s: no legal entity of the tenant has the id", gc.ID)
			}
			named = gen.GroupCompany{ID: gc.ID, Code: gen.NewOptString(le.Code), Name: gen.NewOptString(le.Name)}
			names[gc.ID] = named
		}
		rep.GroupCompany.SetTo(named)
	}
	return nil
}

// nameAccounts gives each customer company of the partners its
// reconciliation account's number as the ledger has it now, reading the
// accounts of a page once (ADR-0053: an id comes with the reference its
// consumer needs). The account was the ledger's when the data was
// requested, and accounts are never deleted, so one the ledger does not
// know is an error.
func nameAccounts(ctx context.Context, accounts Accounts, reps ...*gen.BusinessPartner) error {
	var ids []uuid.UUID
	for _, rep := range reps {
		for _, c := range rep.CustomerCompanies {
			ids = append(ids, c.ReconciliationAccountID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	numbers, err := accounts.AccountNumbers(ctx, ids)
	if err != nil {
		return fmt.Errorf("partner: name the reconciliation accounts of customers: %w", err)
	}
	for _, rep := range reps {
		for i := range rep.CustomerCompanies {
			c := &rep.CustomerCompanies[i]
			number, ok := numbers[c.ReconciliationAccountID]
			if !ok {
				return fmt.Errorf("partner: name reconciliation account %s of business partner %s: the ledger has no account with the id",
					c.ReconciliationAccountID, rep.ID)
			}
			c.ReconciliationAccountNumber = number
		}
	}
	return nil
}

// partnerOut is the partner as the caller sees it: the company data of
// the legal entities the caller may act in, the group company and the
// customer data's reconciliation accounts named, and its personal data
// masked while it is blocked, unless the caller reads it in full.
func (e *Endpoints) partnerOut(ctx context.Context, p Partner) (gen.BusinessPartner, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return gen.BusinessPartner{}, err
	}
	full, err := readsBlocked(ctx, p.maskable())
	if err != nil {
		return gen.BusinessPartner{}, err
	}
	if !full {
		p = masked(p)
	}
	out := partnerToAPI(visible(p, access))
	if err := nameGroupCompanies(ctx, e.LegalEntities, access, &out); err != nil {
		return out, err
	}
	return out, nameAccounts(ctx, e.Accounts, &out)
}

func changeToAPI(c Change) gen.PartnerChange {
	out := gen.PartnerChange{
		ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt, UpdatedBy: gen.NewOptUUID(c.UpdatedBy),
		Number: c.Number, Kind: c.Kind, PartnerID: c.PartnerID, Payload: apitypes.EncodeObject(c.Masked()), Reason: c.Reason,
		Warnings: []gen.PartnerChangeWarning{}, PossibleDuplicates: []gen.PossibleDuplicate{}, Status: c.Status, RequestedBy: c.RequestedBy,
	}
	for _, w := range c.Warnings {
		out.Warnings = append(out.Warnings, gen.PartnerChangeWarning(w))
	}
	for _, d := range c.PossibleDuplicates {
		out.PossibleDuplicates = append(out.PossibleDuplicates, gen.PossibleDuplicate{PartnerID: d.PartnerID, Number: d.Number, Name: d.Name, Matched: d.Matched})
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
		BankCountryCode: b.BankCountryCode, HolderName: b.HolderName, PartnerChangeID: b.PartnerChangeID,
		InstitutionNumber: apitypes.OptString(b.InstitutionNumber), TransitNumber: apitypes.OptString(b.TransitNumber)}
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
		out = append(out, SupplierCompanyInput{LegalEntityID: s.LegalEntityID, ReconciliationAccountID: s.ReconciliationAccountID,
			PaymentTermCode: s.PaymentTermCode, WithholdingTaxCodes: s.WithholdingTaxCodes, PaymentMethods: s.PaymentMethods,
			PlanningGroup: s.PlanningGroup.Or("")})
	}
	return out
}

func customerCompaniesIn(in []gen.CustomerCompanyInput) []CustomerCompanyInput {
	out := []CustomerCompanyInput{}
	for _, c := range in {
		out = append(out, CustomerCompanyInput{LegalEntityID: c.LegalEntityID, ReconciliationAccountID: c.ReconciliationAccountID,
			PaymentTermCode: c.PaymentTermCode, IsPostingBlocked: c.IsPostingBlocked.Or(false)})
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
		CustomerCompanies: customerCompaniesIn(req.CustomerCompanies), Addresses: addressesIn(req.Addresses), TaxIdentifiers: taxesIn(req.TaxIdentifiers), Extensions: apitypes.DecodeObject(req.Extensions.Or(nil)), Reason: req.Reason,
		GroupCompanyID: apitypes.Ptr[uuid.UUID](req.GroupCompanyID), ConfirmDuplicate: req.ConfirmDuplicate.Value}
	for _, b := range req.BankAccounts {
		in.BankAccounts = append(in.BankAccounts, BankInput{Purpose: b.Purpose.Value, AccountNumber: b.AccountNumber, AccountScheme: b.AccountScheme,
			BIC: apitypes.Ptr[string](b.Bic), BankCountryCode: b.BankCountryCode, HolderName: b.HolderName, ValidFrom: b.ValidFrom,
			InstitutionNumber: b.InstitutionNumber.Value, TransitNumber: b.TransitNumber.Value})
	}
	p, c, err := CreatePartner(ctx, e.ports(), in)
	if err != nil {
		return nil, err
	}
	if _, err := RequestApproval(ctx, e.Approvals, e.Schemas, e.Now, c); err != nil {
		return nil, err
	}
	if p, err = getPartner(ctx, tx, p.ID, false); err != nil {
		return nil, err
	}
	out, err := e.partnerOut(ctx, p)
	if err != nil {
		return nil, err
	}
	return &gen.BusinessPartnerHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Location: gen.NewOptString("/v1/business-partners/" + p.ID.String()),
		Response: out}, nil
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
	out, err := e.partnerOut(ctx, p)
	if err != nil {
		return nil, err
	}
	return &gen.GetBusinessPartnerOKHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: out}, nil
}

var partnerSort = pagination.Sort{Default: "number", Fields: []pagination.Field{
	{Name: "number", Column: "number", Type: "text"}, {Name: "name", Column: "name", Type: "text"},
}}

// maskedSort is partnerSort for a caller who reads blocked personal data
// masked: by name as it is answered, so neither the order nor the page
// token carries a blocked partner's name (ADR-0079 §11).
var maskedSort = pagination.Sort{Default: "number", Fields: []pagination.Field{
	{Name: "number", Column: "number", Type: "text"},
	{Name: "name", Column: "(CASE WHEN personal_data_blocked_at IS NOT NULL AND personal_data_deleted_at IS NULL THEN '" + MaskedText + "' ELSE name END)", Type: "text"},
}}

// partnerKeyset is the listing's keyset and whether the caller reads
// blocked personal data in full: asked, once, when the listing searches or
// orders by name; otherwise it is answered later, for the page.
func (e *Endpoints) partnerKeyset(ctx context.Context, scope string, byName bool, page pagination.Request) (*pagination.Keyset, bool, error) {
	full, err := readsBlocked(ctx, byName)
	if err != nil {
		return nil, false, err
	}
	sort := partnerSort
	if !full {
		sort, scope = maskedSort, scope+"|masked"
	}
	ks, err := e.Pages.Keyset(sort, scope, page)
	return ks, full, err
}

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
	ids := make([]string, len(params.Ids))
	for i, id := range params.Ids {
		ids[i] = id.String()
	}
	scope := "listBusinessPartners|" + params.Number.Value + "|" + params.NamePrefix.Value + "|" + params.Role.Value + "|" + params.Status.Value +
		"|" + strings.Join(ids, ",")
	prefix := ""
	if params.NamePrefix.Set {
		prefix = likePrefix(strings.ToUpper(strings.Join(strings.Fields(params.NamePrefix.Value), " ")))
	}
	// A search by name finds a partner whose personal data is blocked only
	// for a caller who reads it in full: its name is not to be confirmed.
	byName := prefix != "" || strings.HasPrefix(strings.TrimSpace(params.OrderBy.Value), "name")
	ks, full, err := e.partnerKeyset(ctx, scope, byName, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(partnerSelect+` WHERE ($1 = '' OR p.number = $1) AND ($2 = '' OR (p.name_norm LIKE $2 AND ($6 OR p.personal_data_blocked_at IS NULL OR p.personal_data_deleted_at IS NOT NULL)))
		AND ($3 = '' OR EXISTS (SELECT FROM partner.partner_role r WHERE r.tenant_id = p.tenant_id AND r.partner_id = p.id AND r.role = $3))
		AND ($4 = '' OR p.status = $4) AND ($5::uuid[] IS NULL OR p.id = ANY($5::uuid[]))`,
		[]any{params.Number.Value, prefix, params.Role.Value, params.Status.Value, params.Ids, full})
	rows, _ := tx.Query(ctx, q, args...)
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Partner, error) { return scanPartner(r) })
	if err != nil {
		return nil, fmt.Errorf("partner: list business partners: %w", err)
	}
	if full, err = pageReadsBlocked(ctx, ps, byName, full); err != nil {
		return nil, err
	}
	items, next := apitypes.PageOf(ks, ps, func(p Partner) (string, uuid.UUID) {
		switch {
		case ks.Field() != "name":
			return p.Number, p.ID
		case !full && p.maskable():
			return MaskedText, p.ID // the masked sort's key
		}
		return p.Name, p.ID
	}, func(p Partner) gen.BusinessPartner {
		if !full && p.maskable() {
			p = masked(p)
		}
		return partnerToAPI(visible(p, access))
	})
	reps := make([]*gen.BusinessPartner, len(items))
	for i := range items {
		reps[i] = &items[i]
	}
	if err := nameGroupCompanies(ctx, e.LegalEntities, access, reps...); err != nil { // each company once for the page
		return nil, err
	}
	if err := nameAccounts(ctx, e.Accounts, reps...); err != nil {
		return nil, err
	}
	return &gen.BusinessPartnerPage{Items: items, NextPageToken: next}, nil
}

// pageReadsBlocked reports whether the caller reads the page's blocked
// personal data in full: asked is whether full is the answer read
// already; otherwise the permission is read once for the page, when a
// partner of it has its personal data blocked.
func pageReadsBlocked(ctx context.Context, ps []Partner, asked, full bool) (bool, error) {
	if asked {
		return full, nil
	}
	blocked := false
	for _, p := range ps {
		blocked = blocked || p.maskable()
	}
	return readsBlocked(ctx, blocked)
}

// BlockPersonalData implements POST /v1/business-partners/{id}:blockPersonalData.
func (e *Endpoints) BlockPersonalData(ctx context.Context, req *gen.ReasonInput, params gen.BlockPersonalDataParams) (gen.BlockPersonalDataRes, error) {
	p, err := BlockPersonalData(ctx, e.OpenItems, e.LegalEntities, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	out, err := e.partnerOut(ctx, p)
	if err != nil {
		return nil, err
	}
	return &gen.BlockPersonalDataOKHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: out}, nil
}

// UnblockPersonalData implements POST /v1/business-partners/{id}:unblockPersonalData.
func (e *Endpoints) UnblockPersonalData(ctx context.Context, req *gen.ReasonInput, params gen.UnblockPersonalDataParams) (gen.UnblockPersonalDataRes, error) {
	p, err := UnblockPersonalData(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	out, err := e.partnerOut(ctx, p)
	if err != nil {
		return nil, err
	}
	return &gen.BlockPersonalDataOKHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: out}, nil
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
		c, err = RequestApproval(ctx, e.Approvals, e.Schemas, e.Now, c)
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
		SupplierCompanies: companiesIn(pl.SupplierCompanies), CustomerCompanies: customerCompaniesIn(pl.CustomerCompanies), Addresses: addressesIn(pl.Addresses), TaxIdentifiers: taxesIn(pl.TaxIdentifiers),
		Extensions: apitypes.DecodeObject(pl.Extensions.Or(nil)), GroupCompanyID: apitypes.OptNilUUID(pl.GroupCompanyID)}
	c, err := RequestUpdate(ctx, e.ports(), req.PartnerID, ch, req.Reason)
	if err == nil {
		c, err = RequestApproval(ctx, e.Approvals, e.Schemas, e.Now, c)
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
	cs := []Change{c}
	if err := maskChanges(ctx, tx, cs); err != nil {
		return nil, err
	}
	return &gen.GetPartnerChangeOKHeaders{ETag: changeTag(c), Response: changeToAPI(cs[0])}, nil
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
	if err := maskChanges(ctx, tx, cs); err != nil {
		return nil, err
	}
	items, next := apitypes.PageOf(ks, cs, func(c Change) (string, uuid.UUID) { return pagination.Instant(c.CreatedAt), c.ID }, changeToAPI)
	return &gen.PartnerChangePage{Items: items, NextPageToken: next}, nil
}

// RequestBankAccountChange implements POST /v1/business-partners/{id}/bank-accounts.
func (e *Endpoints) RequestBankAccountChange(ctx context.Context, req *gen.BankChangeCreate, params gen.RequestBankAccountChangeParams) (gen.RequestBankAccountChangeRes, error) {
	c, err := RequestBankAccount(ctx, e.ports(), params.ID, BankInput{Purpose: req.Purpose.Value, AccountNumber: req.AccountNumber,
		AccountScheme: req.AccountScheme, BIC: apitypes.Ptr[string](req.Bic), BankCountryCode: req.BankCountryCode, HolderName: req.HolderName,
		ValidFrom: req.ValidFrom, InstitutionNumber: req.InstitutionNumber.Value, TransitNumber: req.TransitNumber.Value}, req.Reason)
	if err == nil {
		c, err = RequestApproval(ctx, e.Approvals, e.Schemas, e.Now, c)
	}
	if err != nil {
		return nil, err
	}
	return &gen.PartnerChangeHeaders{ETag: changeTag(c), Location: gen.NewOptString("/v1/partner-changes/" + c.ID.String()), Response: changeToAPI(c)}, nil
}

// RequestBankAccountEnd implements POST /v1/business-partners/{id}/bank-accounts/{bank_account_id}:end.
func (e *Endpoints) RequestBankAccountEnd(ctx context.Context, req *gen.BankAccountEnd, params gen.RequestBankAccountEndParams) (gen.RequestBankAccountEndRes, error) {
	c, err := RequestBankAccountEnd(ctx, e.ports(), params.ID, params.BankAccountID, func(b BankAccount) error { return etag.Match(params.IfMatch, bankTag(b)) },
		req.ValidTo, req.Reason)
	if err == nil {
		c, err = RequestApproval(ctx, e.Approvals, e.Schemas, e.Now, c)
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
	blocked, err := blockedAmong(ctx, tx, []uuid.UUID{params.ID})
	if err != nil {
		return nil, err
	}
	full, err := readsBlocked(ctx, blocked[params.ID])
	if err != nil {
		return nil, err
	}
	// The ETag is the account's as stored, whoever reads it, as the end of
	// the account matches it.
	tag, out := bankTag(b), b
	if !full {
		out = maskedBank(b)
	}
	return &gen.BankAccountHeaders{ETag: gen.NewOptString(tag), Response: bankToAPI(out)}, nil
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
	var blocked bool
	err = tx.QueryRow(ctx, `SELECT personal_data_blocked_at IS NOT NULL AND personal_data_deleted_at IS NULL FROM partner.business_partner WHERE id = $1`,
		params.ID).Scan(&blocked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, problem.NotFoundOf("business partner")
	}
	if err != nil {
		return nil, fmt.Errorf("partner: find business partner %s: %w", params.ID, err)
	}
	full, err := readsBlocked(ctx, blocked)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+bankColumns+` FROM partner.bank_account WHERE partner_id = $1
		AND ($2::date IS NULL OR (valid_from <= $2 AND (valid_to IS NULL OR valid_to > $2)))`, []any{params.ID, validOn})
	rows, _ := tx.Query(ctx, q, args...)
	bs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BankAccount, error) { return scanBank(r) })
	if err != nil {
		return nil, fmt.Errorf("partner: list bank accounts of business partner %s: %w", params.ID, err)
	}
	items, next := apitypes.PageOf(ks, bs, func(b BankAccount) (string, uuid.UUID) { return b.ValidFrom.Format(time.DateOnly), b.ID }, func(b BankAccount) gen.BankAccount {
		if !full {
			b = maskedBank(b)
		}
		return bankToAPI(b)
	})
	return &gen.BankAccountPage{Items: items, NextPageToken: next}, nil
}
