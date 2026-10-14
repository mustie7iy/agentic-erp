package fieldcrypt

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// What other packages may know about tenant keys without their material:
// the control plane shows which key seals a tenant's fields, and the key
// rotation policy finds the keys due. Both read in the caller's
// transaction, within the column grants of its role.

// ActiveKey is a tenant's active key as the control plane shows it: which
// master wraps it and since when.
type ActiveKey struct {
	ID          uuid.UUID
	MasterKeyID string
	CreatedAt   time.Time
}

// ActiveKeys returns the active key of each of the tenants that has one.
func ActiveKeys(ctx context.Context, tx pgx.Tx, tenants []uuid.UUID) (map[uuid.UUID]ActiveKey, error) {
	rows, _ := tx.Query(ctx, `SELECT tenant_id, id, master_key_id, created_at FROM tenancy.tenant_key WHERE tenant_id = ANY($1) AND status = 'active'`, tenants)
	keys := map[uuid.UUID]ActiveKey{}
	var tenant uuid.UUID
	var k ActiveKey
	if _, err := pgx.ForEachRow(rows, []any{&tenant, &k.ID, &k.MasterKeyID, &k.CreatedAt}, func() error {
		keys[tenant] = k
		return nil
	}); err != nil {
		return nil, fmt.Errorf("fieldcrypt: read the active keys of %d tenants: %w", len(tenants), err)
	}
	return keys, nil
}

// KeyedSince returns the tenants whose active key was created at or after
// since.
func KeyedSince(ctx context.Context, tx pgx.Tx, since time.Time) (map[uuid.UUID]bool, error) {
	rows, _ := tx.Query(ctx, `SELECT tenant_id FROM tenancy.tenant_key WHERE status = 'active' AND created_at >= $1`, since)
	keyed := map[uuid.UUID]bool{}
	var tenant uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&tenant}, func() error {
		keyed[tenant] = true
		return nil
	}); err != nil {
		return nil, fmt.Errorf("fieldcrypt: read the keys created since %s: %w", since.Format(time.RFC3339), err)
	}
	return keyed, nil
}
