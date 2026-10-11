// Package provisioning creates tenants: the one step the API cannot do for
// itself, because every request runs as a principal of an existing tenant
// (ADR-0003, ADR-0014). An operator provisions a tenant with its first
// administrator and its first configuration in one transaction; from then
// on the administrator manages the tenant through the API.
package provisioning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// AdministratorRole is the code of the role the first administrator holds:
// every permission, so that the tenant can be set up through the API.
const AdministratorRole = "tenant_admin"

// Administrator is the tenant's first person.
type Administrator struct {
	Subject     string // subject at the identity provider
	DisplayName string
	Email       *string
}

// Request describes the tenant to create.
type Request struct {
	Name          string
	Region        string // e.g. eu-west
	GroupCurrency string // ISO 4217
	Administrator Administrator
	Document      *config.Config // the first configuration, activated as version 1
	Operator      string         // who asked (the command's user, an operator token's subject), for the audit entry
	Idempotency   *Idempotency   // the request's Idempotency-Key, when it came through the control plane
}

// Idempotency is the Idempotency-Key of a provisioning request (04 §4.3).
// There is no tenant to store it under before the request runs, so the
// provisioner stores it with the tenant it creates, in the same
// transaction, under the control-plane prefix: a retry finds the tenant
// instead of creating a second one, and two concurrent requests with the
// same key create one.
type Idempotency struct {
	Key         string    // the client's key, without the prefix
	RequestHash []byte    // sha256 of method, URL and body, as the unit of work hashes them
	ActorID     uuid.UUID // the operator
}

// KeyPrefix marks control-plane idempotency keys in platform.idempotency_key.
const KeyPrefix = "control|"

// Result is what was created. It is the stored response of an idempotent
// provisioning request, so its fields are named for JSON.
type Result struct {
	TenantID             uuid.UUID            `json:"tenant_id"`
	AdministratorID      uuid.UUID            `json:"administrator_id"`
	AdministratorRoleID  uuid.UUID            `json:"administrator_role_id"`
	Roles                []string             `json:"roles"`          // roles the configuration's approval policies name, created empty
	LegalEntities        map[string]uuid.UUID `json:"legal_entities"` // the configuration's companies, by code, created active
	ConfigurationVersion int32                `json:"configuration_version"`
}

func init() {
	problem.RegisterConstraint("idempotency_key_control", func() *problem.Problem {
		p := problem.Conflict("request.idempotency_key_in_use", "A request with this Idempotency-Key is running or just completed; retry to get its result.")
		p.RetryAfter = 1
		return p
	})
}

// Provisioner provisions tenants in one database.
type Provisioner struct {
	Pool          *database.Pool
	AppRole       string              // erp_app: the role the tenant's own rows are written as
	Keys          *fieldcrypt.Keyring // gives the tenant its own data key; nil leaves it on the legacy key
	Configuration configuration.Dependencies
	Now           func() time.Time
	// Begin starts the transaction; nil means Pool.Begin. Tests replace it
	// to inject database failures.
	Begin func(context.Context, database.Scope) (pgx.Tx, error)
}

func (p *Provisioner) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	if p.Begin != nil {
		return p.Begin(ctx, s)
	}
	return p.Pool.Begin(ctx, s)
}

func (p *Provisioner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func invalid(field, msg string) error {
	return problem.Unprocessable("tenancy.invalid_tenant", "The tenant request is invalid.", problem.FieldError{Field: field, Code: "invalid", Message: msg})
}

func (r Request) check() error {
	switch {
	case strings.TrimSpace(r.Name) == "":
		return invalid("name", "required")
	case r.Region == "":
		return invalid("region", "required, e.g. eu-west")
	case len(r.GroupCurrency) != 3:
		return invalid("group_currency", "an ISO 4217 code")
	case r.Administrator.Subject == "":
		return invalid("administrator.subject", "the administrator's subject at the identity provider is required")
	case strings.TrimSpace(r.Administrator.DisplayName) == "":
		return invalid("administrator.display_name", "required")
	case r.Document == nil:
		return invalid("document", "the first configuration is required")
	}
	return nil
}

// Provision creates the tenant, its administrator holding AdministratorRole
// with every permission, the roles the configuration names, an active legal
// entity per company of the configuration, and the configuration as
// version 1, all in one transaction. The tenant row is
// written as the connection's own role, the rest as AppRole acting as the
// administrator, so the audit trail starts with the administrator.
func (p *Provisioner) Provision(ctx context.Context, req Request) (Result, error) {
	if err := req.check(); err != nil {
		return Result{}, err
	}
	res := Result{TenantID: uuid.Must(uuid.NewV7()), AdministratorID: uuid.Must(uuid.NewV7())}
	tx, err := p.begin(ctx, database.Scope{TenantID: res.TenantID})
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `INSERT INTO tenancy.tenant (id, name, region, group_currency, status) VALUES ($1, $2, $3, $4, 'active')`,
		res.TenantID, strings.TrimSpace(req.Name), req.Region, strings.ToUpper(req.GroupCurrency)); err != nil {
		return Result{}, fmt.Errorf("provisioning: tenant: %w", problem.From(err))
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{p.AppRole}.Sanitize()); err != nil {
		return Result{}, err
	}
	if err := database.SetActor(ctx, tx, res.AdministratorID); err != nil {
		return Result{}, err
	}
	principal := reqctx.Principal{ID: res.AdministratorID, Kind: identity.Human, TenantID: res.TenantID}
	ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), principal)
	ctx, buf := audit.WithBuffer(ctx)
	if p.Keys != nil {
		if err := p.Keys.Provision(ctx, tx, res.TenantID); err != nil {
			return Result{}, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity.principal (tenant_id, id, kind, external_subject, display_name, email, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, $5, true, 'active', $2, $2)`,
		res.TenantID, res.AdministratorID, req.Administrator.Subject, strings.TrimSpace(req.Administrator.DisplayName), req.Administrator.Email); err != nil {
		return Result{}, fmt.Errorf("provisioning: administrator: %w", problem.From(err))
	}
	description := "The tenant's administrators: every permission, granted at provisioning."
	role, err := identity.CreateRole(ctx, AdministratorRole, "Tenant administrator", &description)
	if err != nil {
		return Result{}, err
	}
	res.AdministratorRoleID = role.ID
	// Every permission in one grant: the role is the tenant's root, and the
	// audit entry below records it (no per-permission events, unlike grants
	// made through the API later).
	tag, err := tx.Exec(ctx, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, code, current_date, $3 FROM ref.permission`, res.TenantID, role.ID, res.AdministratorID)
	if err != nil {
		return Result{}, err
	}
	// The audit buffer is in ctx, so Add cannot fail.
	_ = audit.Add(ctx, audit.Entry{Action: "identity.role_permission.grant", ObjectType: "identity.role", ObjectID: role.ID, Outcome: "succeeded",
		Detail: map[string]any{"role_id": role.ID, "permissions": "all", "count": tag.RowsAffected()}})
	if _, err := identity.AssignRole(ctx, res.AdministratorID, role.ID, nil, nil); err != nil {
		return Result{}, err
	}
	for _, name := range req.Document.RoleNames() {
		if _, err := identity.CreateRole(ctx, name, name, nil); err != nil {
			return Result{}, err
		}
		res.Roles = append(res.Roles, name)
	}
	res.LegalEntities = map[string]uuid.UUID{}
	for _, co := range req.Document.Companies {
		le, err := tenancy.CreateLegalEntity(ctx, tenancy.NewLegalEntity{Code: co.Code, Name: co.Name, CountryCode: co.Country, LocalCurrency: co.Currency,
			FiscalCalendarID: uuid.Must(uuid.NewV7())})
		if err != nil {
			return Result{}, err
		}
		if _, err := tenancy.ActivateLegalEntity(ctx, le.ID); err != nil {
			return Result{}, err
		}
		res.LegalEntities[co.Code] = le.ID
	}
	doc, _ := json.Marshal(req.Document) // Config holds only JSON-safe types
	if res.ConfigurationVersion, err = configuration.Bootstrap(ctx, p.Configuration, doc); err != nil {
		return Result{}, err
	}
	if _, err := outbox.Add(ctx, outbox.Event{Context: "tenancy", Name: "tenant_status_changed", AggregateType: "tenancy.tenant", AggregateID: res.TenantID,
		Payload: map[string]any{"tenant_id": res.TenantID, "previous_status": "provisioning", "status": "active"}}); err != nil {
		return Result{}, err
	}
	_ = audit.Add(ctx, audit.Entry{Action: "tenancy.tenant.provision", ObjectType: "tenancy.tenant", ObjectID: res.TenantID, Outcome: "succeeded",
		Detail: map[string]any{"name": req.Name, "region": req.Region, "group_currency": req.GroupCurrency, "administrator_id": res.AdministratorID,
			"operator": req.Operator}})
	if err := buf.Flush(ctx, tx, principal, reqctx.TraceID(ctx), p.now()); err != nil {
		return Result{}, err
	}
	if k := req.Idempotency; k != nil {
		body, _ := json.Marshal(res) // ids, strings and a map of ids
		if _, err := tx.Exec(ctx, `INSERT INTO platform.idempotency_key (tenant_id, key, principal_id, request_hash, status, response_code, response_body, expires_at)
			VALUES ($1, $2, $3, $4, 'completed', 201, $5, $6)`, res.TenantID, KeyPrefix+k.Key, k.ActorID, k.RequestHash, body, p.now().Add(uow.Retention)); err != nil {
			return Result{}, problem.From(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("provisioning: commit: %w", err)
	}
	return res, nil
}
