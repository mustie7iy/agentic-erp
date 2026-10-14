// Package controlplane is the operators' API for tenants (T11, 04-api-design
// §4.15): list and read the cell's tenants, provision one, suspend, resume
// and offboard. It is served on the admin listener, not the public API, and
// its callers are platform operators, not principals of any tenant: they
// authenticate with tokens from the operator issuers (ERP_OPERATOR_ISSUERS)
// that name no tenant. Reads across tenants run as the control role
// (erp_control: BYPASSRLS, read-only). A status change runs in the tenant's
// own scope as the application role, so the tenant's audit trail records it
// (actor kind system, an id derived from the operator) and the outbox
// publishes TenantStatusChanged like any other change.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	auditlog "github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Statuses of a tenant (03-data-model §3.3). Provisioning is never seen:
// a tenant is created active in one transaction.
const (
	Provisioning = "provisioning"
	Active       = "active"
	Suspended    = "suspended"
	Offboarded   = "offboarded"
)

// Statuses lists them, in lifecycle order.
var Statuses = []string{Provisioning, Active, Suspended, Offboarded}

// Tenant is a tenant as the control plane shows it: its row (tenancy),
// its active key (fieldcrypt) and its latest audit verification (audit).
type Tenant struct {
	ID            uuid.UUID    `json:"id"`
	Name          string       `json:"name"`
	Region        string       `json:"region"`
	GroupCurrency string       `json:"group_currency"`
	Status        string       `json:"status"`
	Version       int32        `json:"version"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
	Offboarding   *Offboarding `json:"offboarding,omitempty"`        // once an offboarded tenant's data was exported
	Encryption    *Encryption  `json:"encryption,omitempty"`         // the tenant's own key, once it has one (ADR-0018)
	Verification  *Verified    `json:"audit_verification,omitempty"` // the latest scheduled verification of its audit chain (ADR-0025)
}

// Encryption is the key that seals the tenant's sensitive fields: which
// master wraps it and since when.
type Encryption struct {
	KeyID       uuid.UUID `json:"key_id"`
	MasterKeyID string    `json:"master_key_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// Verified is the outcome of the latest scheduled audit verification.
type Verified struct {
	VerifiedAt     time.Time `json:"verified_at"`
	OK             bool      `json:"ok"`
	HeadSeq        int64     `json:"head_seq"`
	Checked        int64     `json:"checked"`
	AnchorsChecked int       `json:"anchors_checked"`
	BrokenAt       *int64    `json:"broken_at"`
}

// Offboarding is what became of an offboarded tenant's data (ADR-0021).
type Offboarding struct {
	ExportedAt time.Time  `json:"exported_at"`
	ExportRef  string     `json:"export_ref"`
	EraseAfter time.Time  `json:"erase_after"`
	ErasedAt   *time.Time `json:"erased_at,omitempty"`
}

// Created is a provisioned tenant with what provisioning made for it.
type Created struct {
	Tenant
	Provisioned provisioning.Result `json:"provisioned"`
}

// Operator is who calls the control plane: the subject of an operator
// token, with a stable id for the audit trail.
type Operator struct {
	ID      uuid.UUID
	Issuer  string
	Subject string
}

// operatorNamespace derives operator ids from issuer and subject, so the
// same operator has the same id in every tenant's audit trail.
var operatorNamespace = uuid.MustParse("6f1d0a5e-3c2b-4a89-9e7d-0b1c2d3e4f50")

// OperatorFrom identifies the operator behind verified claims.
func OperatorFrom(c reqctx.Claims) Operator {
	return Operator{ID: uuid.NewSHA1(operatorNamespace, []byte(c.Issuer+"\n"+c.Subject)), Issuer: c.Issuer, Subject: c.Subject}
}

// Service runs the control plane on one cell's database.
type Service struct {
	Pool        *database.Pool
	AppRole     string // erp_app: status changes run in the tenant's scope
	ControlRole string // erp_control: reads across tenants
	Provisioner *provisioning.Provisioner
	Keys        *fieldcrypt.Keyring // rotates tenant keys; nil: rotation answers 503
	Offboarding *offboarding.Worker // erases exported tenants on request; nil: erasure answers 503
	Directory   *directory.Sync     // records tenants in the global directory after a change; nil: a single cell
	Pages       *pagination.Codec
	Now         func() time.Time
	// Begin starts a transaction; nil means Pool.Begin. Tests replace it to
	// inject database failures.
	Begin database.BeginFunc
}

func (s *Service) begin(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, s.Pool, s.Begin, scope)
}

func (s *Service) now() time.Time {
	return clock.Now(s.Now)
}

// shown is the tenant as the control plane shows it, without its key and
// verification, which views adds.
func shown(r tenancy.Tenant) Tenant {
	t := Tenant{ID: r.ID, Name: r.Name, Region: r.Region, GroupCurrency: r.GroupCurrency, Status: r.Status, Version: r.Version,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if r.ExportedAt != nil {
		t.Offboarding = &Offboarding{ExportedAt: *r.ExportedAt, ExportRef: *r.ExportRef, EraseAfter: *r.EraseAfter, ErasedAt: r.ErasedAt}
	}
	return t
}

// views shows the tenants with their active keys and latest verifications,
// two reads in tx however many tenants there are. The key's and the
// verification's times are shown in UTC. The reads are separate statements
// under READ COMMITTED, so a key rotated between them can show next to the
// version before it; accepted for an operator's read, since the ETag guards
// any change made from it (a stale If-Match is refused, and reading again
// shows both).
func views(ctx context.Context, tx pgx.Tx, rows []tenancy.Tenant) ([]Tenant, error) {
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	keys, err := fieldcrypt.ActiveKeys(ctx, tx, ids)
	if err != nil {
		return nil, fmt.Errorf("controlplane: %w", err)
	}
	verified, err := auditlog.LatestVerifications(ctx, tx, ids)
	if err != nil {
		return nil, fmt.Errorf("controlplane: %w", err)
	}
	out := make([]Tenant, len(rows))
	for i, r := range rows {
		out[i] = shown(r)
		if k, ok := keys[r.ID]; ok {
			out[i].Encryption = &Encryption{KeyID: k.ID, MasterKeyID: k.MasterKeyID, CreatedAt: k.CreatedAt.UTC()}
		}
		if v, ok := verified[r.ID]; ok {
			out[i].Verification = &Verified{VerifiedAt: v.VerifiedAt.UTC(), OK: v.OK, HeadSeq: v.HeadSeq, Checked: v.Checked,
				AnchorsChecked: v.AnchorsChecked, BrokenAt: brokenAt(v.BrokenAt)}
		}
	}
	return out, nil
}

// brokenAt is a verification's first broken sequence number; nil when the
// chain is intact (0).
func brokenAt(seq int64) *int64 {
	if seq == 0 {
		return nil
	}
	return &seq
}

// view shows one tenant as views does.
func view(ctx context.Context, tx pgx.Tx, r tenancy.Tenant) (Tenant, error) {
	ts, err := views(ctx, tx, []tenancy.Tenant{r})
	if err != nil {
		return Tenant{}, err
	}
	return ts[0], nil
}

// read reads the tenant's row in tx, locked when lock is set; a tenant this
// cell does not have is 404.
func read(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (tenancy.Tenant, error) {
	t, err := tenancy.GetTenant(ctx, tx, id, lock)
	if errors.Is(err, pgx.ErrNoRows) {
		return tenancy.Tenant{}, problem.NotFound("No tenant with this id.")
	}
	if err != nil {
		return tenancy.Tenant{}, fmt.Errorf("controlplane: %w", err)
	}
	return t, nil
}

// Sort is the order_by a tenant listing accepts.
var Sort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "created_at", Type: "timestamptz"},
	{Name: "name", Column: "name", Type: "text"},
}}

// List returns one page of the cell's tenants, optionally those in one
// status.
func (s *Service) List(ctx context.Context, status string, page pagination.Request) ([]Tenant, string, error) {
	if status != "" && !slices.Contains(Statuses, status) {
		return nil, "", problem.BadRequest("request.invalid_parameter", "Parameter status is invalid.",
			problem.FieldError{Field: "status", Code: "invalid", Message: "one of " + strings.Join(Statuses, ", ")})
	}
	ks, err := s.Pages.Keyset(Sort, "listTenants|"+status, page)
	if err != nil {
		return nil, "", err
	}
	tx, err := s.begin(ctx, database.Scope{Role: s.ControlRole, ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, err := tenancy.ListTenants(ctx, tx, status, ks)
	if err != nil {
		return nil, "", fmt.Errorf("controlplane: %w", err)
	}
	tenants, err := views(ctx, tx, rows)
	if err != nil {
		return nil, "", err
	}
	tenants, next := pagination.Page(ks, tenants, func(t Tenant) (string, uuid.UUID) {
		if ks.Field() == "name" {
			return t.Name, t.ID
		}
		return t.CreatedAt.UTC().Format(time.RFC3339Nano), t.ID
	})
	return tenants, next, nil
}

// Get returns one tenant of the cell.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Tenant, error) {
	tx, err := s.begin(ctx, database.Scope{Role: s.ControlRole, ReadOnly: true})
	if err != nil {
		return Tenant{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	r, err := read(ctx, tx, id, false)
	if err != nil {
		return Tenant{}, err
	}
	return view(ctx, tx, r)
}

// Fingerprint reads the tenant and its fingerprint (ADR-0030): every
// table's rows and the audit chain head, in the tenant's scope, in one
// snapshot. The global control plane compares two cells' fingerprints
// before a move switches the directory.
func (s *Service) Fingerprint(ctx context.Context, id uuid.UUID) (Tenant, offboarding.Fingerprint, error) {
	tx, err := s.begin(ctx, database.Scope{Role: s.AppRole, TenantID: id, ReadOnly: true})
	if err != nil {
		return Tenant{}, offboarding.Fingerprint{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	r, err := read(ctx, tx, id, false)
	if err != nil {
		return Tenant{}, offboarding.Fingerprint{}, err
	}
	t, err := view(ctx, tx, r)
	if err != nil {
		return Tenant{}, offboarding.Fingerprint{}, err
	}
	fp, err := offboarding.TakeFingerprint(ctx, tx, id, s.now())
	return t, fp, err
}

// Create provisions a tenant for the operator. With req.Idempotency set, a
// retry of an earlier request answers the tenant that request created
// (replayed = true) instead of creating a second one.
func (s *Service) Create(ctx context.Context, op Operator, req provisioning.Request) (Created, bool, error) {
	req.Operator = op.Subject
	if k := req.Idempotency; k != nil {
		k.ActorID = op.ID
		res, found, err := s.replay(ctx, k)
		if err != nil {
			return Created{}, false, err
		}
		if found {
			t, err := s.Get(ctx, res.TenantID)
			return Created{Tenant: t, Provisioned: res}, true, err
		}
	}
	res, err := s.Provisioner.Provision(ctx, req)
	if err != nil {
		return Created{}, false, err
	}
	t, err := s.Get(ctx, res.TenantID)
	if err == nil {
		s.register(ctx, t)
	}
	return Created{Tenant: t, Provisioned: res}, false, err
}

// register tells the directory about the tenant's current state (ADR-0022).
func (s *Service) register(ctx context.Context, t Tenant) {
	if s.Directory != nil {
		s.Directory.Register(ctx, t.ID, t.Region, t.Status)
	}
}

// replay finds the result of an earlier provisioning request with the same
// key: the same operator and request get it back, anyone else 422.
func (s *Service) replay(ctx context.Context, k *provisioning.Idempotency) (provisioning.Result, bool, error) {
	tx, err := s.begin(ctx, database.Scope{Role: s.ControlRole, ReadOnly: true})
	if err != nil {
		return provisioning.Result{}, false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var owner uuid.UUID
	var hash, body []byte
	// The predicate repeats the partial index's, so the lookup uses it.
	err = tx.QueryRow(ctx, `SELECT principal_id, request_hash, response_body FROM platform.idempotency_key
		WHERE key = $1 AND key LIKE 'control|%' AND expires_at > now()`, provisioning.KeyPrefix+k.Key).Scan(&owner, &hash, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return provisioning.Result{}, false, nil
	}
	if err != nil {
		return provisioning.Result{}, false, fmt.Errorf("controlplane: read idempotency key of operator %s: %w", k.ActorID, err)
	}
	if owner != k.ActorID || !bytes.Equal(hash, k.RequestHash) {
		return provisioning.Result{}, false, problem.Unprocessable("request.idempotency_key_reused", "This Idempotency-Key was already used for a different request.")
	}
	var res provisioning.Result
	_ = json.Unmarshal(body, &res) // written by the provisioner from a Result
	return res, true, nil
}

// transition is what a custom method does to the status.
type transition struct {
	from []string
	to   string
}

// Transitions are the custom methods on a tenant: suspend (active →
// suspended), resume (suspended → active) and offboard (active or suspended
// → offboarded, final). A suspended or offboarded tenant's requests are
// refused at the door (403 tenancy.tenant_inactive).
var Transitions = map[string]transition{
	"suspend":  {from: []string{Active}, to: Suspended},
	"resume":   {from: []string{Suspended}, to: Active},
	"offboard": {from: []string{Active, Suspended}, to: Offboarded},
}

// Transition applies the custom method verb to the tenant, guarded by
// If-Match, and records it in the tenant's audit trail and outbox.
func (s *Service) Transition(ctx context.Context, op Operator, id uuid.UUID, verb, ifMatch string) (Tenant, error) {
	tr, ok := Transitions[verb]
	if !ok {
		return Tenant{}, problem.NotFound("No such method on tenants: " + verb + ".")
	}
	tx, err := s.begin(ctx, database.Scope{Role: s.AppRole, TenantID: id})
	if err != nil {
		return Tenant{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	r, err := read(ctx, tx, id, true)
	if err != nil {
		return Tenant{}, err
	}
	if err := etag.Check(ifMatch, r.Version); err != nil {
		return Tenant{}, err
	}
	if !slices.Contains(tr.from, r.Status) {
		return Tenant{}, problem.Conflict("tenancy.invalid_transition",
			fmt.Sprintf("The tenant is %s; %s applies to %s tenants.", r.Status, verb, strings.Join(tr.from, " or ")))
	}
	previous := r.Status
	if err := database.SetActor(ctx, tx, op.ID); err != nil {
		return Tenant{}, err
	}
	if r, err = tenancy.SetTenantStatus(ctx, tx, id, tr.to); err != nil {
		return Tenant{}, fmt.Errorf("controlplane: %s: %w", verb, err)
	}
	t, err := view(ctx, tx, r)
	if err != nil {
		return Tenant{}, err
	}
	trace := reqctx.TraceID(ctx)
	if _, err := outbox.Write(ctx, tx, id, op.ID, trace, outbox.Event{Context: "tenancy", Name: "tenant_status_changed", AggregateType: "tenancy.tenant", AggregateID: id,
		Payload: map[string]any{"tenant_id": id, "previous_status": previous, "status": t.Status}}); err != nil {
		return Tenant{}, err
	}
	actor := reqctx.Principal{ID: op.ID, Kind: reqctx.KindSystem, TenantID: id}
	if err := audit.Write(ctx, tx, actor, trace, s.now(), audit.Entry{Action: "tenancy.tenant." + verb, ObjectType: "tenancy.tenant", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"previous_status": previous, "status": t.Status, "operator": op.Subject, "issuer": op.Issuer}}); err != nil {
		return Tenant{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Tenant{}, fmt.Errorf("controlplane: commit %s of tenant %s: %w", verb, id, err)
	}
	s.register(ctx, t)
	return t, nil
}

// RotateKey gives the tenant a new field-encryption key and re-seals its
// sensitive fields (ADR-0018), guarded by If-Match like a status change,
// and records it in the tenant's audit trail. The tenant's version moves on.
func (s *Service) RotateKey(ctx context.Context, op Operator, id uuid.UUID, ifMatch string) (Tenant, fieldcrypt.Rotation, error) {
	if s.Keys == nil {
		return Tenant{}, fieldcrypt.Rotation{}, problem.Unavailable("Key rotation is not configured on this server.", 0)
	}
	tx, err := s.begin(ctx, database.Scope{Role: s.AppRole, TenantID: id})
	if err != nil {
		return Tenant{}, fieldcrypt.Rotation{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	r, err := read(ctx, tx, id, true)
	if err != nil {
		return Tenant{}, fieldcrypt.Rotation{}, err
	}
	if err := etag.Check(ifMatch, r.Version); err != nil {
		return Tenant{}, fieldcrypt.Rotation{}, err
	}
	if r.Status == Offboarded {
		return Tenant{}, fieldcrypt.Rotation{}, problem.Conflict("tenancy.invalid_transition", "The tenant is offboarded; its keys are not rotated.")
	}
	if err := database.SetActor(ctx, tx, op.ID); err != nil {
		return Tenant{}, fieldcrypt.Rotation{}, err
	}
	rot, err := s.Keys.Rotate(ctx, tx, id)
	if err != nil {
		return Tenant{}, fieldcrypt.Rotation{}, err
	}
	if r, err = tenancy.TouchTenant(ctx, tx, id); err != nil {
		return Tenant{}, fieldcrypt.Rotation{}, fmt.Errorf("controlplane: %w", err)
	}
	t, err := view(ctx, tx, r)
	if err != nil {
		return Tenant{}, fieldcrypt.Rotation{}, err
	}
	actor := reqctx.Principal{ID: op.ID, Kind: reqctx.KindSystem, TenantID: id}
	if err := audit.Write(ctx, tx, actor, reqctx.TraceID(ctx), s.now(), audit.Entry{Action: "tenancy.tenant.rotate_key", ObjectType: "tenancy.tenant", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"key_id": rot.KeyID, "previous_key_id": rot.Previous, "master_key_id": rot.Master, "resealed": rot.Resealed, "operator": op.Subject, "issuer": op.Issuer}}); err != nil {
		return Tenant{}, fieldcrypt.Rotation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Tenant{}, fieldcrypt.Rotation{}, fmt.Errorf("controlplane: commit key rotation of tenant %s: %w", id, err)
	}
	return t, rot, nil
}

// Erase erases an offboarded tenant's data before its grace period ends
// (ADR-0021), guarded by If-Match: the export must be complete. The
// erasure is recorded in the tenant's audit trail as the operator.
func (s *Service) Erase(ctx context.Context, op Operator, id uuid.UUID, ifMatch string) (Tenant, offboarding.Erasure, error) {
	if s.Offboarding == nil {
		return Tenant{}, offboarding.Erasure{}, problem.Unavailable("This server has no object storage; offboarded tenants are not exported or erased.", 0)
	}
	tx, err := s.begin(ctx, database.Scope{Role: s.AppRole, TenantID: id, ReadOnly: true})
	if err != nil {
		return Tenant{}, offboarding.Erasure{}, err
	}
	r, err := read(ctx, tx, id, false)
	tx.Rollback(context.WithoutCancel(ctx))
	if err != nil {
		return Tenant{}, offboarding.Erasure{}, err
	}
	if err := etag.Check(ifMatch, r.Version); err != nil {
		return Tenant{}, offboarding.Erasure{}, err
	}
	switch {
	case r.Status != Offboarded || r.ExportedAt == nil:
		return Tenant{}, offboarding.Erasure{}, problem.Conflict("tenancy.invalid_transition", "The tenant is "+r.Status+" and not yet exported; erase applies to offboarded tenants whose export is complete.")
	case r.ErasedAt != nil:
		return Tenant{}, offboarding.Erasure{}, problem.Conflict("tenancy.invalid_transition", "The tenant has already been erased.")
	}
	er, err := s.Offboarding.Erase(ctx, id, op.ID, map[string]any{"operator": op.Subject, "issuer": op.Issuer})
	if err != nil {
		return Tenant{}, offboarding.Erasure{}, err
	}
	t, err := s.Get(ctx, id)
	return t, er, err
}
