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
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
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
	Begin database.BeginFunc
}

func (p *Provisioner) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, p.Pool, p.Begin, s)
}

func (p *Provisioner) now() time.Time {
	return clock.Now(p.Now)
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
	if err := p.createTenant(ctx, tx, req, res); err != nil {
		return Result{}, err
	}
	principal := reqctx.Principal{ID: res.AdministratorID, Kind: identity.Human, TenantID: res.TenantID}
	if err := uow.Act(ctx, tx, principal, p.Now, func(ctx context.Context) error {
		return p.populate(ctx, tx, req, &res)
	}); err != nil {
		return Result{}, err
	}
	if err := p.recordIdempotency(ctx, tx, req.Idempotency, res); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("provisioning: commit: %w", err)
	}
	return res, nil
}

// populate creates, as the administrator, the tenant's keys, administrator,
// roles, legal entities and first configuration, with the events and the
// audit entry of the provisioning.
func (p *Provisioner) populate(ctx context.Context, tx pgx.Tx, req Request, res *Result) error {
	if p.Keys != nil {
		if err := p.Keys.Provision(ctx, tx, res.TenantID); err != nil {
			return err
		}
	}
	if err := createAdministrator(ctx, tx, req, res); err != nil {
		return err
	}
	if err := createRolesAndEntities(ctx, req, res); err != nil {
		return err
	}
	doc, _ := json.Marshal(req.Document) // Config holds only JSON-safe types
	version, err := configuration.Bootstrap(ctx, p.Configuration, doc)
	if err != nil {
		return err
	}
	res.ConfigurationVersion = version
	if _, err := outbox.Add(ctx, outbox.Event{Context: "tenancy", Name: "tenant_status_changed", AggregateType: "tenancy.tenant", AggregateID: res.TenantID,
		Payload: map[string]any{"tenant_id": res.TenantID, "previous_status": "provisioning", "status": "active"}}); err != nil {
		return err
	}
	// Add fails only without an audit buffer, and uow.Act gave ctx one.
	_ = audit.Add(ctx, audit.Entry{Action: "tenancy.tenant.provision", ObjectType: "tenancy.tenant", ObjectID: res.TenantID, Outcome: "succeeded",
		Detail: map[string]any{"name": req.Name, "region": req.Region, "group_currency": req.GroupCurrency, "administrator_id": res.AdministratorID,
			"operator": req.Operator}})
	return nil
}

// createTenant writes the tenant row as the connection's own role, then
// becomes AppRole for the rest (uow.Act makes it act as the administrator).
func (p *Provisioner) createTenant(ctx context.Context, tx pgx.Tx, req Request, res Result) error {
	if err := tenancy.CreateTenant(ctx, tx, tenancy.NewTenant{ID: res.TenantID, Name: strings.TrimSpace(req.Name), Region: req.Region,
		GroupCurrency: strings.ToUpper(req.GroupCurrency)}); err != nil {
		return fmt.Errorf("provisioning: tenant: %w", problem.From(err))
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{p.AppRole}.Sanitize()); err != nil {
		return fmt.Errorf("provisioning: tenant %s: set role %s: %w", res.TenantID, p.AppRole, err)
	}
	return nil
}

// createAdministrator creates the administrator and AdministratorRole with
// every permission, and assigns the role to the administrator.
func createAdministrator(ctx context.Context, tx pgx.Tx, req Request, res *Result) error {
	admin := identity.FirstAdministrator{TenantID: res.TenantID, ID: res.AdministratorID, Subject: req.Administrator.Subject,
		DisplayName: strings.TrimSpace(req.Administrator.DisplayName), Email: req.Administrator.Email}
	if err := identity.CreateFirstAdministrator(ctx, tx, admin); err != nil {
		return fmt.Errorf("provisioning: administrator: %w", problem.From(err))
	}
	description := "The tenant's administrators: every permission, granted at provisioning."
	role, err := identity.CreateRole(ctx, AdministratorRole, "Tenant administrator", &description)
	if err != nil {
		return err
	}
	res.AdministratorRoleID = role.ID
	if err := identity.GrantEveryPermission(ctx, tx, admin, role.ID); err != nil {
		return fmt.Errorf("provisioning: %w", err)
	}
	if _, err := identity.AssignRole(ctx, res.AdministratorID, role.ID, nil, nil); err != nil {
		return err
	}
	return nil
}

// createRolesAndEntities creates the roles the configuration names and an
// active legal entity per company of the configuration.
func createRolesAndEntities(ctx context.Context, req Request, res *Result) error {
	for _, name := range req.Document.RoleNames() {
		if _, err := identity.CreateRole(ctx, name, name, nil); err != nil {
			return err
		}
		res.Roles = append(res.Roles, name)
	}
	res.LegalEntities = map[string]uuid.UUID{}
	for _, co := range req.Document.Companies {
		le, err := tenancy.CreateLegalEntity(ctx, tenancy.NewLegalEntity{Code: co.Code, Name: co.Name, CountryCode: co.Country, LocalCurrency: co.Currency,
			FiscalCalendarID: uuid.Must(uuid.NewV7())})
		if err != nil {
			return err
		}
		if _, err := tenancy.ActivateLegalEntity(ctx, le.ID); err != nil {
			return err
		}
		res.LegalEntities[co.Code] = le.ID
	}
	return nil
}

// recordIdempotency stores the result under the request's Idempotency-Key,
// when it has one, so that a retried request gets the same answer.
func (p *Provisioner) recordIdempotency(ctx context.Context, tx pgx.Tx, k *Idempotency, res Result) error {
	if k != nil {
		body, _ := json.Marshal(res) // ids, strings and a map of ids
		if _, err := tx.Exec(ctx, `INSERT INTO platform.idempotency_key (tenant_id, key, principal_id, request_hash, status, response_code, response_body, expires_at)
			VALUES ($1, $2, $3, $4, 'completed', 201, $5, $6)`, res.TenantID, KeyPrefix+k.Key, k.ActorID, k.RequestHash, body, p.now().Add(uow.Retention)); err != nil {
			return problem.From(err)
		}
	}
	return nil
}
