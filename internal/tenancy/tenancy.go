// Package tenancy is the tenancy context (03-data-model §3.3): the legal
// entities (company codes) of a tenant and their organizational units.
// Both start inactive, are activated once and can then be blocked and
// unblocked; nothing is deleted. Every read and write is limited to the
// legal entities the caller may act in, and anything outside them is
// answered as not found.
package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Statuses of legal entities and org units.
const (
	Inactive = "inactive"
	Active   = "active"
	Blocked  = "blocked"
)

func init() {
	problem.RegisterConstraint("legal_entity_tenant_id_code_key", func() *problem.Problem {
		return problem.Conflict("tenancy.legal_entity_code_taken", "Another legal entity already has this code.")
	})
	problem.RegisterConstraint("legal_entity_country_code_fkey", func() *problem.Problem {
		return problem.Unprocessable("tenancy.unknown_country", "The country code is not known.",
			problem.FieldError{Field: "country_code", Code: "unknown", Message: "not an ISO 3166 country in the reference data"})
	})
	problem.RegisterConstraint("legal_entity_local_currency_fkey", func() *problem.Problem {
		return problem.Unprocessable("tenancy.unknown_currency", "The currency code is not known.",
			problem.FieldError{Field: "local_currency", Code: "unknown", Message: "not an ISO 4217 currency in the reference data"})
	})
	problem.RegisterConstraint("org_unit_tenant_id_legal_entity_id_kind_code_key", func() *problem.Problem {
		return problem.Conflict("tenancy.org_unit_code_taken", "The legal entity already has an org unit of this kind with this code.")
	})
}

// LegalEntity is a row of tenancy.legal_entity.
type LegalEntity struct {
	ID               uuid.UUID
	Code             string
	Name             string
	CountryCode      string
	LocalCurrency    string
	FiscalCalendarID uuid.UUID
	Status           string
	Extensions       map[string]any
	Version          int32
	CreatedAt        time.Time
	CreatedBy        uuid.UUID
	UpdatedAt        time.Time
	UpdatedBy        uuid.UUID
}

const leColumns = `id, code, name, country_code, local_currency, fiscal_calendar_id, status, extensions, version,
	created_at, created_by, updated_at, updated_by`

func scanLE(row pgx.Row) (LegalEntity, error) {
	var e LegalEntity
	err := row.Scan(&e.ID, &e.Code, &e.Name, &e.CountryCode, &e.LocalCurrency, &e.FiscalCalendarID, &e.Status, &e.Extensions,
		&e.Version, &e.CreatedAt, &e.CreatedBy, &e.UpdatedAt, &e.UpdatedBy)
	return e, err
}

// OrgUnit is a row of tenancy.org_unit.
type OrgUnit struct {
	ID            uuid.UUID
	LegalEntityID uuid.UUID
	Kind          string
	Code          string
	Name          string
	ParentID      *uuid.UUID
	Status        string
	Version       int32
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
	UpdatedAt     time.Time
	UpdatedBy     uuid.UUID
}

const ouColumns = `id, legal_entity_id, kind, code, name, parent_id, status, version, created_at, created_by, updated_at, updated_by`

func scanOU(row pgx.Row) (OrgUnit, error) {
	var u OrgUnit
	err := row.Scan(&u.ID, &u.LegalEntityID, &u.Kind, &u.Code, &u.Name, &u.ParentID, &u.Status, &u.Version,
		&u.CreatedAt, &u.CreatedBy, &u.UpdatedAt, &u.UpdatedBy)
	return u, err
}

// OrgUnitKinds are the kinds the database accepts.
var OrgUnitKinds = map[string]bool{"purchasing_org": true, "plant": true, "storage_location": true, "sales_org": true}

var extensionKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// checkExtensions enforces snake_case keys (ADR-0009). Validation against
// the tenant's published custom field schema comes with the configuration
// context's custom field schemas.
func checkExtensions(ext map[string]any) error {
	for k := range ext {
		if !extensionKey.MatchString(k) {
			return problem.Unprocessable("request.invalid_extension", "Custom field keys are snake_case.",
				problem.FieldError{Field: "extensions." + k, Code: "invalid_key", Message: "snake_case, starting with a letter"})
		}
	}
	return nil
}

// mergePatch applies an RFC 7396 merge patch to a JSON object: null removes
// a member, objects merge recursively, anything else replaces.
func mergePatch(target, patch map[string]any) map[string]any {
	out := make(map[string]any, len(target))
	for k, v := range target {
		out[k] = v
	}
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(out, k)
		case map[string]any:
			tv, _ := out[k].(map[string]any)
			out[k] = mergePatch(tv, pv)
		default:
			out[k] = v
		}
	}
	return out
}

func notFound(what string) error { return problem.NotFound("No " + what + " with this id.") }

// getLE loads a legal entity the caller may act in; lock takes the row lock
// for a change.
func getLE(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (LegalEntity, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return LegalEntity{}, err
	}
	q := `SELECT ` + leColumns + ` FROM tenancy.legal_entity WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	e, err := scanLE(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(e.ID)) {
		return LegalEntity{}, notFound("legal entity")
	}
	return e, err
}

func getOU(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (OrgUnit, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return OrgUnit{}, err
	}
	q := `SELECT ` + ouColumns + ` FROM tenancy.org_unit WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	u, err := scanOU(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(u.LegalEntityID)) {
		return OrgUnit{}, notFound("org unit")
	}
	return u, err
}

// NewLegalEntity is the input of CreateLegalEntity.
type NewLegalEntity struct {
	Code, Name, CountryCode, LocalCurrency string
	FiscalCalendarID                       uuid.UUID
	Extensions                             map[string]any
}

// CreateLegalEntity inserts an inactive legal entity. Code, country and
// currency are checked by the database (pattern, reference data, unique).
func CreateLegalEntity(ctx context.Context, in NewLegalEntity) (LegalEntity, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return LegalEntity{}, err
	}
	if in.Extensions == nil {
		in.Extensions = map[string]any{}
	}
	if err := checkExtensions(in.Extensions); err != nil {
		return LegalEntity{}, err
	}
	e, err := scanLE(tx.QueryRow(ctx, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency,
		fiscal_calendar_id, status, extensions, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'inactive', $8, $9, $9) RETURNING `+leColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), in.Code, in.Name, in.CountryCode, in.LocalCurrency, in.FiscalCalendarID, in.Extensions, p.ID))
	if err != nil {
		return LegalEntity{}, err
	}
	return e, record(ctx, "tenancy.legal_entity.create", "tenancy.legal_entity", e.ID, map[string]any{"code": e.Code},
		outbox.Event{Context: "tenancy", Name: "legal_entity_created", AggregateType: "tenancy.legal_entity", AggregateID: e.ID,
			Payload: map[string]any{
				"legal_entity_id": e.ID, "code": e.Code, "name": e.Name, "country_code": e.CountryCode,
				"local_currency": e.LocalCurrency, "fiscal_calendar_id": e.FiscalCalendarID, "status": e.Status,
			}})
}

// LegalEntityChange is a merge patch of a legal entity.
type LegalEntityChange struct {
	Name       *string
	Extensions map[string]any // merge patch of the custom fields; nil: unchanged
}

// UpdateLegalEntity changes the name and custom fields.
func UpdateLegalEntity(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch LegalEntityChange) (LegalEntity, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return LegalEntity{}, err
	}
	e, err := getLE(ctx, tx, id, true)
	if err != nil {
		return LegalEntity{}, err
	}
	if err := ifMatch(e.Version); err != nil {
		return LegalEntity{}, err
	}
	name, ext := e.Name, e.Extensions
	if ch.Name != nil {
		name = *ch.Name
	}
	if ch.Extensions != nil {
		ext = mergePatch(ext, ch.Extensions)
		if err := checkExtensions(ext); err != nil {
			return LegalEntity{}, err
		}
	}
	updated, err := scanLE(tx.QueryRow(ctx, `UPDATE tenancy.legal_entity SET name = $2, extensions = $3, version = version + 1, updated_by = $4
		WHERE id = $1 RETURNING `+leColumns, id, name, ext, p.ID))
	if err != nil {
		return LegalEntity{}, err
	}
	return updated, record(ctx, "tenancy.legal_entity.update", "tenancy.legal_entity", id, changed(e.Name != name, "name", jsonDiffers(e.Extensions, ext), "extensions"))
}

// transition is a status change: allowed from one status, to another, with
// the event it publishes ("" for none).
type transition struct {
	action, from, to, event string
}

// Activation publishes nothing: the contract has no "activated" event yet
// (LegalEntityCreated carries status inactive), see docs/design/07-transition.md.
var (
	activateLE = transition{"activate", Inactive, Active, ""}
	blockLE    = transition{"block", Active, Blocked, "legal_entity_blocked"}
	unblockLE  = transition{"unblock", Blocked, Active, "legal_entity_unblocked"}
	activateOU = transition{"activate", Inactive, Active, ""}
	blockOU    = transition{"block", Active, Blocked, "org_unit_blocked"}
	unblockOU  = transition{"unblock", Blocked, Active, "org_unit_unblocked"}
)

// ActivateLegalEntity activates an inactive legal entity regardless of its
// version, for provisioning, which creates the configuration's companies
// ready to post.
func ActivateLegalEntity(ctx context.Context, id uuid.UUID) (LegalEntity, error) {
	return changeLegalEntityStatus(ctx, id, func(int32) error { return nil }, activateLE, "")
}

func (t transition) check(what, current string) error {
	if current != t.from {
		return problem.Conflict("tenancy.invalid_status", "The "+what+" is "+current+"; "+t.action+" requires status "+t.from+".")
	}
	return nil
}

// changeLegalEntityStatus applies a status transition; reason is recorded
// for blocks.
func changeLegalEntityStatus(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, t transition, reason string) (LegalEntity, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return LegalEntity{}, err
	}
	e, err := getLE(ctx, tx, id, true)
	if err != nil {
		return LegalEntity{}, err
	}
	if err := ifMatch(e.Version); err != nil {
		return LegalEntity{}, err
	}
	if err := t.check("legal entity", e.Status); err != nil {
		return LegalEntity{}, err
	}
	updated, err := scanLE(tx.QueryRow(ctx, `UPDATE tenancy.legal_entity SET status = $2, version = version + 1, updated_by = $3
		WHERE id = $1 RETURNING `+leColumns, id, t.to, p.ID))
	if err != nil {
		return LegalEntity{}, err
	}
	var events []outbox.Event
	if t.event != "" {
		events = append(events, outbox.Event{Context: "tenancy", Name: t.event, AggregateType: "tenancy.legal_entity", AggregateID: id,
			Payload: map[string]any{"legal_entity_id": id, "code": e.Code, "previous_status": e.Status, "status": t.to}})
	}
	return updated, record(ctx, "tenancy.legal_entity."+t.action, "tenancy.legal_entity", id,
		withReason(map[string]any{"from_status": e.Status}, reason), events...)
}

// NewOrgUnit is the input of CreateOrgUnit.
type NewOrgUnit struct {
	LegalEntityID uuid.UUID
	Kind, Code    string
	Name          string
	ParentID      *uuid.UUID
}

// CreateOrgUnit inserts an inactive org unit under a legal entity the
// caller may act in. A parent must belong to the same legal entity.
func CreateOrgUnit(ctx context.Context, in NewOrgUnit) (OrgUnit, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return OrgUnit{}, err
	}
	if !OrgUnitKinds[in.Kind] {
		return OrgUnit{}, problem.Unprocessable("tenancy.unknown_org_unit_kind", "Unknown org unit kind.",
			problem.FieldError{Field: "kind", Code: "unknown", Message: "purchasing_org, plant, storage_location or sales_org"})
	}
	if _, err := getLE(ctx, tx, in.LegalEntityID, false); err != nil {
		return OrgUnit{}, referenceError(err, "legal_entity_id", "legal entity")
	}
	if err := checkParent(ctx, tx, in.LegalEntityID, uuid.Nil, in.ParentID); err != nil {
		return OrgUnit{}, err
	}
	u, err := scanOU(tx.QueryRow(ctx, `INSERT INTO tenancy.org_unit (tenant_id, id, legal_entity_id, kind, code, name, parent_id, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'inactive', $8, $8) RETURNING `+ouColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), in.LegalEntityID, in.Kind, in.Code, in.Name, in.ParentID, p.ID))
	if err != nil {
		return OrgUnit{}, err
	}
	return u, record(ctx, "tenancy.org_unit.create", "tenancy.org_unit", u.ID, map[string]any{"code": u.Code, "kind": u.Kind},
		outbox.Event{Context: "tenancy", Name: "org_unit_created", AggregateType: "tenancy.org_unit", AggregateID: u.ID,
			Payload: map[string]any{
				"org_unit_id": u.ID, "legal_entity_id": u.LegalEntityID, "kind": u.Kind, "code": u.Code, "name": u.Name,
				"parent_id": u.ParentID, "status": u.Status,
			}})
}

// referenceError turns "not found" for a referenced resource into 422 on
// the referencing field.
func referenceError(err error, field, what string) error {
	if problem.From(err).Status == 404 {
		return problem.Unprocessable("request.reference_not_found", "No "+what+" with this id.",
			problem.FieldError{Field: field, Code: "not_found", Message: "no " + what + " with this id that you can access"})
	}
	return err
}

// checkParent requires a parent in the same legal entity and no cycle: the
// unit itself (self, nil for a new unit) must not be among the parent's
// ancestors.
func checkParent(ctx context.Context, tx pgx.Tx, legalEntity, self uuid.UUID, parent *uuid.UUID) error {
	if parent == nil {
		return nil
	}
	var sameEntity, cycle bool
	err := tx.QueryRow(ctx, `WITH RECURSIVE up AS (
			SELECT id, parent_id, legal_entity_id FROM tenancy.org_unit WHERE id = $1
			UNION
			SELECT o.id, o.parent_id, o.legal_entity_id FROM tenancy.org_unit o JOIN up ON o.id = up.parent_id
		)
		SELECT coalesce(bool_or(id = $1 AND legal_entity_id = $2), false), coalesce(bool_or(id = $3), false) FROM up`, *parent, legalEntity, self).Scan(&sameEntity, &cycle)
	switch {
	case err != nil:
		return err
	case !sameEntity:
		return problem.Unprocessable("tenancy.invalid_parent", "The parent must be an org unit of the same legal entity.",
			problem.FieldError{Field: "parent_id", Code: "invalid", Message: "no org unit with this id in the legal entity"})
	case cycle:
		return problem.Unprocessable("tenancy.invalid_parent", "An org unit cannot be its own ancestor.",
			problem.FieldError{Field: "parent_id", Code: "cycle", Message: "the parent is the unit or one of its descendants"})
	}
	return nil
}

// OrgUnitChange is a merge patch of an org unit.
type OrgUnitChange struct {
	Name     *string
	Parent   bool // ParentID is set (nil clears the parent)
	ParentID *uuid.UUID
}

// UpdateOrgUnit changes the name and parent.
func UpdateOrgUnit(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch OrgUnitChange) (OrgUnit, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return OrgUnit{}, err
	}
	u, err := getOU(ctx, tx, id, true)
	if err != nil {
		return OrgUnit{}, err
	}
	if err := ifMatch(u.Version); err != nil {
		return OrgUnit{}, err
	}
	name, parent := u.Name, u.ParentID
	if ch.Name != nil {
		name = *ch.Name
	}
	if ch.Parent {
		parent = ch.ParentID
		if err := checkParent(ctx, tx, u.LegalEntityID, u.ID, parent); err != nil {
			return OrgUnit{}, err
		}
	}
	updated, err := scanOU(tx.QueryRow(ctx, `UPDATE tenancy.org_unit SET name = $2, parent_id = $3, version = version + 1, updated_by = $4
		WHERE id = $1 RETURNING `+ouColumns, id, name, parent, p.ID))
	if err != nil {
		return OrgUnit{}, err
	}
	return updated, record(ctx, "tenancy.org_unit.update", "tenancy.org_unit", id,
		changed(u.Name != name, "name", !sameParent(u.ParentID, parent), "parent_id"))
}

func sameParent(a, b *uuid.UUID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// changeOrgUnitStatus applies a status transition.
func changeOrgUnitStatus(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, t transition, reason string) (OrgUnit, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return OrgUnit{}, err
	}
	u, err := getOU(ctx, tx, id, true)
	if err != nil {
		return OrgUnit{}, err
	}
	if err := ifMatch(u.Version); err != nil {
		return OrgUnit{}, err
	}
	if err := t.check("org unit", u.Status); err != nil {
		return OrgUnit{}, err
	}
	updated, err := scanOU(tx.QueryRow(ctx, `UPDATE tenancy.org_unit SET status = $2, version = version + 1, updated_by = $3
		WHERE id = $1 RETURNING `+ouColumns, id, t.to, p.ID))
	if err != nil {
		return OrgUnit{}, err
	}
	var events []outbox.Event
	if t.event != "" {
		events = append(events, outbox.Event{Context: "tenancy", Name: t.event, AggregateType: "tenancy.org_unit", AggregateID: id,
			Payload: map[string]any{"org_unit_id": id, "legal_entity_id": u.LegalEntityID, "kind": u.Kind, "code": u.Code,
				"previous_status": u.Status, "status": t.to}})
	}
	return updated, record(ctx, "tenancy.org_unit."+t.action, "tenancy.org_unit", id,
		withReason(map[string]any{"from_status": u.Status}, reason), events...)
}

// record publishes the events and adds the audit entry of a change.
func record(ctx context.Context, action, objectType string, id uuid.UUID, detail map[string]any, events ...outbox.Event) error {
	for _, e := range events {
		if _, err := outbox.Add(ctx, e); err != nil {
			return err
		}
	}
	return audit.Add(ctx, audit.Entry{Action: action, ObjectType: objectType, ObjectID: id, Outcome: "succeeded", Detail: detail})
}

func withReason(detail map[string]any, reason string) map[string]any {
	if reason != "" {
		detail["reason"] = reason
	}
	return detail
}

// changed lists the fields a patch actually changed, for the audit detail.
func changed(pairs ...any) map[string]any {
	fields := []string{}
	for i := 0; i < len(pairs); i += 2 {
		if pairs[i].(bool) {
			fields = append(fields, pairs[i+1].(string)) // #nosec G602 -- pairs come in twos by construction
		}
	}
	return map[string]any{"changed_fields": fields}
}

func jsonDiffers(a, b map[string]any) bool {
	x, _ := json.Marshal(a) // decoded JSON always encodes
	y, _ := json.Marshal(b)
	return string(x) != string(y)
}
