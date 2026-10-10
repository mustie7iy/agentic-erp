package tenancy

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/go-faster/jx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Endpoints implements the tenancy part of the generated API.
type Endpoints struct {
	Pages *pagination.Codec
}

func ifMatch(header string) func(int32) error {
	return func(v int32) error { return etag.Check(header, v) }
}

func fromExtensions(x gen.Extensions) map[string]any {
	if x == nil {
		return nil
	}
	out := make(map[string]any, len(x))
	for k, raw := range x {
		var v any
		_ = json.Unmarshal(raw, &v) // the generated decoder only passes valid JSON
		out[k] = v
	}
	return out
}

func toExtensions(m map[string]any) gen.Extensions {
	out := make(gen.Extensions, len(m))
	for k, v := range m {
		b, _ := json.Marshal(v) // values decoded from JSON always encode
		out[k] = jx.Raw(b)
	}
	return out
}

func leToAPI(e LegalEntity) gen.LegalEntity {
	return gen.LegalEntity{
		ID: e.ID, Version: e.Version, CreatedAt: e.CreatedAt, CreatedBy: e.CreatedBy, UpdatedAt: e.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(e.UpdatedBy), Code: e.Code, Name: e.Name, CountryCode: e.CountryCode,
		LocalCurrency: e.LocalCurrency, FiscalCalendarID: e.FiscalCalendarID, Status: e.Status, Extensions: toExtensions(e.Extensions),
	}
}

func ouToAPI(u OrgUnit) gen.OrgUnit {
	out := gen.OrgUnit{
		ID: u.ID, Version: u.Version, CreatedAt: u.CreatedAt, CreatedBy: u.CreatedBy, UpdatedAt: u.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(u.UpdatedBy), LegalEntityID: u.LegalEntityID, Kind: u.Kind, Code: u.Code, Name: u.Name, Status: u.Status,
	}
	if u.ParentID != nil {
		out.ParentID.SetTo(*u.ParentID)
	}
	return out
}

func leOK(e LegalEntity) *gen.UpdateLegalEntityOKHeaders {
	return &gen.UpdateLegalEntityOKHeaders{ETag: gen.NewOptString(etag.Format(e.Version)), Response: leToAPI(e)}
}

func ouOK(u OrgUnit) *gen.UpdateOrgUnitOKHeaders {
	return &gen.UpdateOrgUnitOKHeaders{ETag: gen.NewOptString(etag.Format(u.Version)), Response: ouToAPI(u)}
}

// CreateLegalEntity implements POST /v1/legal-entities.
func (e *Endpoints) CreateLegalEntity(ctx context.Context, req *gen.LegalEntityCreate, _ gen.CreateLegalEntityParams) (gen.CreateLegalEntityRes, error) {
	le, err := CreateLegalEntity(ctx, NewLegalEntity{
		Code: req.Code, Name: req.Name, CountryCode: req.CountryCode, LocalCurrency: req.LocalCurrency,
		FiscalCalendarID: req.FiscalCalendarID, Extensions: fromExtensions(req.Extensions.Value),
	})
	if err != nil {
		return nil, err
	}
	return &gen.LegalEntityHeaders{ETag: gen.NewOptString(etag.Format(le.Version)), Location: gen.NewOptString("/v1/legal-entities/" + le.ID.String()),
		Response: leToAPI(le)}, nil
}

// GetLegalEntity implements GET /v1/legal-entities/{id}.
func (e *Endpoints) GetLegalEntity(ctx context.Context, params gen.GetLegalEntityParams) (gen.GetLegalEntityRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, err := getLE(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetLegalEntityOKHeaders{ETag: gen.NewOptString(etag.Format(le.Version)), Response: leToAPI(le)}, nil
}

var leSort = pagination.Sort{Default: "code", Fields: []pagination.Field{
	{Name: "code", Column: "code", Type: "text"}, {Name: "name", Column: "name", Type: "text"},
}}

// ListLegalEntities implements GET /v1/legal-entities.
func (e *Endpoints) ListLegalEntities(ctx context.Context, params gen.ListLegalEntitiesParams) (gen.ListLegalEntitiesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(leSort, "listLegalEntities|"+params.Status.Value, pagination.Request{
		OrderBy: params.OrderBy.Value, PageSize: params.PageSize.Value, PageSizeSet: params.PageSize.Set,
		PageToken: params.PageToken.Value, PageTokenSet: params.PageToken.Set,
	})
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+leColumns+` FROM tenancy.legal_entity WHERE ($1 OR id = ANY($2)) AND ($3 = '' OR status = $3)`,
		[]any{access.All, access.IDs, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	les, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LegalEntity, error) { return scanLE(r) })
	if err != nil {
		return nil, err
	}
	les, next := pagination.Page(ks, les, func(l LegalEntity) (string, uuid.UUID) {
		if ks.Field() == "name" {
			return l.Name, l.ID
		}
		return l.Code, l.ID
	})
	page := gen.LegalEntityPage{Items: make([]gen.LegalEntity, 0, len(les))}
	for _, l := range les {
		page.Items = append(page.Items, leToAPI(l))
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.LegalEntityPageHeaders{Response: page}, nil
}

// UpdateLegalEntity implements PATCH /v1/legal-entities/{id}.
func (e *Endpoints) UpdateLegalEntity(ctx context.Context, req *gen.LegalEntityUpdate, params gen.UpdateLegalEntityParams) (gen.UpdateLegalEntityRes, error) {
	ch := LegalEntityChange{Extensions: fromExtensions(req.Extensions.Value)}
	if req.Name.Set {
		ch.Name = &req.Name.Value
	}
	le, err := UpdateLegalEntity(ctx, params.ID, ifMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return leOK(le), nil
}

// ActivateLegalEntity implements POST /v1/legal-entities/{id}:activate.
func (e *Endpoints) ActivateLegalEntity(ctx context.Context, params gen.ActivateLegalEntityParams) (gen.ActivateLegalEntityRes, error) {
	le, err := changeLegalEntityStatus(ctx, params.ID, ifMatch(params.IfMatch), activateLE, "")
	if err != nil {
		return nil, err
	}
	return leOK(le), nil
}

// BlockLegalEntity implements POST /v1/legal-entities/{id}:block.
func (e *Endpoints) BlockLegalEntity(ctx context.Context, req *gen.ReasonInput, params gen.BlockLegalEntityParams) (gen.BlockLegalEntityRes, error) {
	le, err := changeLegalEntityStatus(ctx, params.ID, ifMatch(params.IfMatch), blockLE, req.Reason)
	if err != nil {
		return nil, err
	}
	return leOK(le), nil
}

// UnblockLegalEntity implements POST /v1/legal-entities/{id}:unblock.
func (e *Endpoints) UnblockLegalEntity(ctx context.Context, params gen.UnblockLegalEntityParams) (gen.UnblockLegalEntityRes, error) {
	le, err := changeLegalEntityStatus(ctx, params.ID, ifMatch(params.IfMatch), unblockLE, "")
	if err != nil {
		return nil, err
	}
	return leOK(le), nil
}

// CreateOrgUnit implements POST /v1/org-units.
func (e *Endpoints) CreateOrgUnit(ctx context.Context, req *gen.OrgUnitCreate, _ gen.CreateOrgUnitParams) (gen.CreateOrgUnitRes, error) {
	in := NewOrgUnit{LegalEntityID: req.LegalEntityID, Kind: req.Kind, Code: req.Code, Name: req.Name}
	if req.ParentID.Set {
		in.ParentID = &req.ParentID.Value
	}
	u, err := CreateOrgUnit(ctx, in)
	if err != nil {
		return nil, err
	}
	return &gen.OrgUnitHeaders{ETag: gen.NewOptString(etag.Format(u.Version)), Location: gen.NewOptString("/v1/org-units/" + u.ID.String()),
		Response: ouToAPI(u)}, nil
}

// GetOrgUnit implements GET /v1/org-units/{id}.
func (e *Endpoints) GetOrgUnit(ctx context.Context, params gen.GetOrgUnitParams) (gen.GetOrgUnitRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	u, err := getOU(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetOrgUnitOKHeaders{ETag: gen.NewOptString(etag.Format(u.Version)), Response: ouToAPI(u)}, nil
}

var ouSort = pagination.Sort{Default: "code", Fields: []pagination.Field{
	{Name: "code", Column: "code", Type: "text"}, {Name: "name", Column: "name", Type: "text"},
}}

// ListOrgUnits implements GET /v1/org-units.
func (e *Endpoints) ListOrgUnits(ctx context.Context, params gen.ListOrgUnitsParams) (gen.ListOrgUnitsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	var le *uuid.UUID
	if params.LegalEntityID.Set {
		le = &params.LegalEntityID.Value
	}
	scope := "listOrgUnits|" + params.LegalEntityID.Value.String() + "|" + params.Kind.Value + "|" + params.Status.Value
	ks, err := e.Pages.Keyset(ouSort, scope, pagination.Request{
		OrderBy: params.OrderBy.Value, PageSize: params.PageSize.Value, PageSizeSet: params.PageSize.Set,
		PageToken: params.PageToken.Value, PageTokenSet: params.PageToken.Set,
	})
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+ouColumns+` FROM tenancy.org_unit WHERE ($1 OR legal_entity_id = ANY($2))
		AND ($3::uuid IS NULL OR legal_entity_id = $3) AND ($4 = '' OR kind = $4) AND ($5 = '' OR status = $5)`,
		[]any{access.All, access.IDs, le, params.Kind.Value, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	units, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (OrgUnit, error) { return scanOU(r) })
	if err != nil {
		return nil, err
	}
	units, next := pagination.Page(ks, units, func(u OrgUnit) (string, uuid.UUID) {
		if ks.Field() == "name" {
			return u.Name, u.ID
		}
		return u.Code, u.ID
	})
	page := gen.OrgUnitPage{Items: make([]gen.OrgUnit, 0, len(units))}
	for _, u := range units {
		page.Items = append(page.Items, ouToAPI(u))
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.OrgUnitPageHeaders{Response: page}, nil
}

// UpdateOrgUnit implements PATCH /v1/org-units/{id}.
func (e *Endpoints) UpdateOrgUnit(ctx context.Context, req *gen.OrgUnitUpdate, params gen.UpdateOrgUnitParams) (gen.UpdateOrgUnitRes, error) {
	var ch OrgUnitChange
	if req.Name.Set {
		ch.Name = &req.Name.Value
	}
	if req.ParentID.Set {
		ch.Parent = true
		if !req.ParentID.Null {
			ch.ParentID = &req.ParentID.Value
		}
	}
	u, err := UpdateOrgUnit(ctx, params.ID, ifMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return ouOK(u), nil
}

// ActivateOrgUnit implements POST /v1/org-units/{id}:activate.
func (e *Endpoints) ActivateOrgUnit(ctx context.Context, params gen.ActivateOrgUnitParams) (gen.ActivateOrgUnitRes, error) {
	u, err := changeOrgUnitStatus(ctx, params.ID, ifMatch(params.IfMatch), activateOU, "")
	if err != nil {
		return nil, err
	}
	return ouOK(u), nil
}

// BlockOrgUnit implements POST /v1/org-units/{id}:block.
func (e *Endpoints) BlockOrgUnit(ctx context.Context, req *gen.ReasonInput, params gen.BlockOrgUnitParams) (gen.BlockOrgUnitRes, error) {
	u, err := changeOrgUnitStatus(ctx, params.ID, ifMatch(params.IfMatch), blockOU, req.Reason)
	if err != nil {
		return nil, err
	}
	return ouOK(u), nil
}

// UnblockOrgUnit implements POST /v1/org-units/{id}:unblock.
func (e *Endpoints) UnblockOrgUnit(ctx context.Context, params gen.UnblockOrgUnitParams) (gen.UnblockOrgUnitRes, error) {
	u, err := changeOrgUnitStatus(ctx, params.ID, ifMatch(params.IfMatch), unblockOU, "")
	if err != nil {
		return nil, err
	}
	return ouOK(u), nil
}

// Directory answers other contexts' questions about legal entities in the
// request's transaction (a port of the configuration context).
type Directory struct{}

// LegalEntityByCode returns the id and status of the legal entity with the
// code, or ok = false.
func (Directory) LegalEntityByCode(ctx context.Context, code string) (id uuid.UUID, status string, ok bool, err error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, "", false, err
	}
	err = tx.QueryRow(ctx, `SELECT id, status FROM tenancy.legal_entity WHERE code = $1`, code).Scan(&id, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", false, nil
	}
	return id, status, err == nil, err
}

// LegalEntityStatus returns the status of the legal entity, or ok = false
// when there is none with the id. Company access is not applied: callers
// administer access itself.
func (Directory) LegalEntityStatus(ctx context.Context, id uuid.UUID) (status string, ok bool, err error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return "", false, err
	}
	err = tx.QueryRow(ctx, `SELECT status FROM tenancy.legal_entity WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return status, err == nil, err
}

// Entity is what other contexts read about a legal entity.
type Entity struct {
	ID               uuid.UUID
	Code             string
	Status           string
	LocalCurrency    string
	FiscalCalendarID uuid.UUID
}

// Entity returns the legal entity with the id, or ok = false. Company
// access is not applied: callers apply it where it matters.
func (Directory) Entity(ctx context.Context, id uuid.UUID) (Entity, bool, error) {
	return entity(ctx, `id = $1`, id)
}

// EntityByCode returns the legal entity with the company code, or ok = false.
func (Directory) EntityByCode(ctx context.Context, code string) (Entity, bool, error) {
	return entity(ctx, `code = $1`, code)
}

func entity(ctx context.Context, where string, arg any) (Entity, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Entity{}, false, err
	}
	var e Entity
	err = tx.QueryRow(ctx, `SELECT id, code, status, local_currency, fiscal_calendar_id FROM tenancy.legal_entity WHERE `+where, arg).
		Scan(&e.ID, &e.Code, &e.Status, &e.LocalCurrency, &e.FiscalCalendarID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entity{}, false, nil
	}
	return e, err == nil, err
}

// GroupCurrency is the tenant's group currency, the third currency of every
// journal line.
func (Directory) GroupCurrency(ctx context.Context) (string, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return "", err
	}
	var c string
	err = tx.QueryRow(ctx, `SELECT group_currency FROM tenancy.tenant WHERE id = $1`, p.TenantID).Scan(&c)
	return c, err
}

// Unit is what other contexts read about an org unit.
type Unit struct {
	ID            uuid.UUID
	LegalEntityID uuid.UUID
	Kind          string
	Status        string
}

// OrgUnit returns the org unit with the id, or ok = false.
func (Directory) OrgUnit(ctx context.Context, id uuid.UUID) (Unit, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Unit{}, false, err
	}
	u := Unit{ID: id}
	err = tx.QueryRow(ctx, `SELECT legal_entity_id, kind, status FROM tenancy.org_unit WHERE id = $1`, id).Scan(&u.LegalEntityID, &u.Kind, &u.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Unit{}, false, nil
	}
	return u, err == nil, err
}
