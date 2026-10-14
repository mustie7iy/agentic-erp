package tenancy

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

// The tenant row (03-data-model §3.3): the tenant itself, its lifecycle
// status (ADR-0015) and what became of an offboarded tenant's data
// (ADR-0021). Provisioning creates it, the control plane changes its
// status, offboarding records the export and the erasure and the directory
// lists it, each through the functions below in the caller's transaction,
// whose role and scope decide what it may see: the control role reads
// across tenants, the application role only its tenant's row. A missing
// tenant is an error wrapping pgx.ErrNoRows, which the callers answer each
// in their own way.

// Lifecycle is the part of a tenant row the worker role may read (its
// column grant): the status and what became of an offboarded tenant's data.
type Lifecycle struct {
	ID         uuid.UUID
	Status     string
	UpdatedAt  time.Time
	ExportedAt *time.Time // once an offboarded tenant's data was exported
	ExportRef  *string
	EraseAfter *time.Time
	ErasedAt   *time.Time
}

// Tenant is a row of tenancy.tenant.
type Tenant struct {
	Lifecycle
	Name          string
	Region        string
	GroupCurrency string
	Version       int32
	CreatedAt     time.Time
}

// NewTenant is what a tenant row is created or restored with.
type NewTenant struct {
	ID            uuid.UUID
	Name          string
	Region        string
	GroupCurrency string
}

const tenantColumns = lifecycleColumns + `, name, region, group_currency, version, created_at`

const lifecycleColumns = `id, status, updated_at, exported_at, export_ref, erase_after, erased_at`

// fields are the destinations of lifecycleColumns.
func (l *Lifecycle) fields() []any {
	return []any{&l.ID, &l.Status, &l.UpdatedAt, &l.ExportedAt, &l.ExportRef, &l.EraseAfter, &l.ErasedAt}
}

// fields are the destinations of tenantColumns.
func (t *Tenant) fields() []any {
	return append(t.Lifecycle.fields(), &t.Name, &t.Region, &t.GroupCurrency, &t.Version, &t.CreatedAt)
}

func scanLifecycle(row pgx.Row) (Lifecycle, error) {
	var l Lifecycle
	err := row.Scan(l.fields()...)
	return l, errs.Wrapf(err, "scan tenant lifecycle")
}

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	err := row.Scan(t.fields()...)
	return t, errs.Wrapf(err, "scan tenant")
}

func collectTenants(rows pgx.Rows, what string) ([]Tenant, error) {
	tenants, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tenant, error) { return scanTenant(r) })
	if err != nil {
		return nil, fmt.Errorf("tenancy: list %s: %w", what, err)
	}
	return tenants, nil
}

// GetTenant reads the tenant, locked FOR UPDATE when lock is set.
func GetTenant(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Tenant, error) {
	q := `SELECT ` + tenantColumns + ` FROM tenancy.tenant WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	t, err := scanTenant(tx.QueryRow(ctx, q, id))
	if err != nil {
		return Tenant{}, fmt.Errorf("tenancy: read tenant %s: %w", id, err)
	}
	return t, nil
}

// GetLifecycle reads the tenant's lifecycle, within the worker role's grant.
func GetLifecycle(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Lifecycle, error) {
	l, err := scanLifecycle(tx.QueryRow(ctx, `SELECT `+lifecycleColumns+` FROM tenancy.tenant WHERE id = $1`, id))
	if err != nil {
		return Lifecycle{}, fmt.Errorf("tenancy: read tenant %s: %w", id, err)
	}
	return l, nil
}

// Lifecycles returns every tenant's lifecycle, in id order, within the
// worker role's grant.
func Lifecycles(ctx context.Context, tx pgx.Tx) ([]Lifecycle, error) {
	rows, _ := tx.Query(ctx, `SELECT `+lifecycleColumns+` FROM tenancy.tenant ORDER BY id`)
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Lifecycle, error) { return scanLifecycle(r) })
	if err != nil {
		return nil, fmt.Errorf("tenancy: list every tenant's lifecycle: %w", err)
	}
	return all, nil
}

// TenantJSON reads the tenant and its row as JSON, as an export records it.
func TenantJSON(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Tenant, json.RawMessage, error) {
	var t Tenant
	var row json.RawMessage
	err := tx.QueryRow(ctx, `SELECT `+tenantColumns+`, row_to_json(t) FROM tenancy.tenant t WHERE id = $1`, id).Scan(append(t.fields(), &row)...)
	if err != nil {
		return Tenant{}, nil, fmt.Errorf("tenancy: read tenant %s: %w", id, err)
	}
	return t, row, nil
}

// ListTenants returns one page of the tenants, those in one status unless
// status is empty, ordered and limited by ks (sorting by created_at or
// name).
func ListTenants(ctx context.Context, tx pgx.Tx, status string, ks *pagination.Keyset) ([]Tenant, error) {
	q, args := ks.SQL(`SELECT `+tenantColumns+` FROM tenancy.tenant WHERE ($1 = '' OR status = $1)`, []any{status})
	rows, _ := tx.Query(ctx, q, args...)
	return collectTenants(rows, "tenants")
}

// AllTenants returns every tenant, in id order.
func AllTenants(ctx context.Context, tx pgx.Tx) ([]Tenant, error) {
	rows, _ := tx.Query(ctx, `SELECT `+tenantColumns+` FROM tenancy.tenant ORDER BY id`)
	return collectTenants(rows, "every tenant")
}

// TenantsToExport lists the offboarded tenants not yet exported, in id
// order.
func TenantsToExport(ctx context.Context, tx pgx.Tx) ([]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT id FROM tenancy.tenant WHERE status = 'offboarded' AND exported_at IS NULL ORDER BY id`)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("tenancy: list tenants to export: %w", err)
	}
	return ids, nil
}

// TenantsToErase lists the offboarded tenants not yet erased whose grace
// period ended by now, in id order.
func TenantsToErase(ctx context.Context, tx pgx.Tx, now time.Time) ([]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT id FROM tenancy.tenant WHERE status = 'offboarded' AND erased_at IS NULL AND erase_after <= $1 ORDER BY id`, now)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("tenancy: list tenants to erase: %w", err)
	}
	return ids, nil
}

// CreateTenant creates the tenant row, active. A constraint violation is
// returned wrapped, for the caller's problem mapping.
func CreateTenant(ctx context.Context, tx pgx.Tx, t NewTenant) error {
	if _, err := tx.Exec(ctx, `INSERT INTO tenancy.tenant (id, name, region, group_currency, status) VALUES ($1, $2, $3, $4, 'active')`,
		t.ID, t.Name, t.Region, t.GroupCurrency); err != nil {
		return fmt.Errorf("tenancy: create tenant %s: %w", t.ID, err)
	}
	return nil
}

// RestoreTenant brings an erased tenant's row back, active, with no
// offboarding recorded: an import into the cell that erased it.
func RestoreTenant(ctx context.Context, tx pgx.Tx, t NewTenant) error {
	if _, err := tx.Exec(ctx, `UPDATE tenancy.tenant SET status = 'active', name = $2, region = $3, group_currency = $4,
		exported_at = NULL, export_ref = NULL, erase_after = NULL, erased_at = NULL, version = version + 1 WHERE id = $1`, t.ID, t.Name, t.Region, t.GroupCurrency); err != nil {
		return fmt.Errorf("tenancy: restore tenant %s: %w", t.ID, err)
	}
	return nil
}

// SetTenantStatus sets the tenant's status and returns the row; the
// version moves on.
func SetTenantStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID, status string) (Tenant, error) {
	t, err := scanTenant(tx.QueryRow(ctx, `UPDATE tenancy.tenant SET status = $2, version = version + 1 WHERE id = $1 RETURNING `+tenantColumns, id, status))
	if err != nil {
		return Tenant{}, fmt.Errorf("tenancy: set tenant %s %s: %w", id, status, err)
	}
	return t, nil
}

// TouchTenant moves the tenant's version on, for a change recorded
// elsewhere (a new key), and returns the row.
func TouchTenant(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Tenant, error) {
	t, err := scanTenant(tx.QueryRow(ctx, `UPDATE tenancy.tenant SET version = version + 1 WHERE id = $1 RETURNING `+tenantColumns, id))
	if err != nil {
		return Tenant{}, fmt.Errorf("tenancy: update tenant %s: %w", id, err)
	}
	return t, nil
}

// RecordExport records the tenant's export and reports whether it did:
// false when another round recorded one first.
func RecordExport(ctx context.Context, tx pgx.Tx, id uuid.UUID, at time.Time, ref string, eraseAfter time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE tenancy.tenant SET exported_at = $2, export_ref = $3, erase_after = $4, version = version + 1 WHERE id = $1 AND exported_at IS NULL`,
		id, at, ref, eraseAfter)
	if err != nil {
		return false, fmt.Errorf("tenancy: record export of tenant %s: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordErasure records the tenant's erasure and reports whether it did:
// false when another round recorded one first.
func RecordErasure(ctx context.Context, tx pgx.Tx, id uuid.UUID, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE tenancy.tenant SET erased_at = $2, version = version + 1 WHERE id = $1 AND erased_at IS NULL`, id, at)
	if err != nil {
		return false, fmt.Errorf("tenancy: record erasure of tenant %s: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}
