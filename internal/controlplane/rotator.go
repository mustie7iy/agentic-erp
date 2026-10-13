package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Key rotation policy (T23, ADR-0028): a worker rotates the field
// encryption key of every active tenant whose current key is older than
// ERP_KEY_ROTATION_AGE, the way an operator's :rotateKey does, as a system
// actor. Operators keep rotating on request; the policy makes sure no key
// outlives the age set.

// RotatorActor is the worker's id in audit trails: a system actor.
var RotatorActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:key-rotator"))

// KeyRotator rotates tenant keys older than Age in rounds.
type KeyRotator struct {
	Pool       *database.Pool
	Keys       *fieldcrypt.Keyring
	AppRole    string        // erp_app: rotates in the tenant's scope
	WorkerRole string        // erp_worker: finds the tenants due across the cell
	Age        time.Duration // keys older than this are rotated
	Interval   time.Duration // between rounds; default 1 hour
	Log        *slog.Logger
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (k *KeyRotator) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, k.Pool, k.Begin, s)
}

// due lists the active tenants whose active key is older than Age (or
// that have no key of their own yet), as the worker role.
func (k *KeyRotator) due(ctx context.Context) ([]uuid.UUID, error) {
	tx, err := k.begin(ctx, database.Scope{Role: k.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT t.id FROM tenancy.tenant t
		LEFT JOIN tenancy.tenant_key k ON k.tenant_id = t.id AND k.status = 'active'
		WHERE t.status = 'active' AND (k.created_at IS NULL OR k.created_at < $1) ORDER BY t.id`, time.Now().Add(-k.Age))
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("controlplane: list tenants due for key rotation: %w", err)
	}
	return tenants, nil
}

// Rotate gives one tenant a new key and re-seals its fields, recorded in
// its audit trail as the worker.
func (k *KeyRotator) Rotate(ctx context.Context, tenant uuid.UUID) (fieldcrypt.Rotation, error) {
	tx, err := k.begin(ctx, database.Scope{Role: k.AppRole, TenantID: tenant})
	if err != nil {
		return fieldcrypt.Rotation{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM tenancy.tenant WHERE id = $1 FOR UPDATE`, tenant).Scan(&status); err != nil {
		return fieldcrypt.Rotation{}, fmt.Errorf("controlplane: lock tenant %s: %w", tenant, err)
	}
	if status != Active {
		return fieldcrypt.Rotation{}, fmt.Errorf("tenant %s is %s", tenant, status)
	}
	if err := database.SetActor(ctx, tx, RotatorActor); err != nil {
		return fieldcrypt.Rotation{}, err
	}
	rot, err := k.Keys.Rotate(ctx, tx, tenant)
	if err != nil {
		return fieldcrypt.Rotation{}, err
	}
	actor := reqctx.Principal{ID: RotatorActor, Kind: reqctx.KindSystem, TenantID: tenant}
	if err := audit.Write(ctx, tx, actor, "", time.Now().UTC(), audit.Entry{Action: "tenancy.tenant.rotate_key", ObjectType: "tenancy.tenant", ObjectID: tenant, Outcome: "succeeded",
		Detail: map[string]any{"key_id": rot.KeyID, "previous_key_id": rot.Previous, "master_key_id": rot.Master, "resealed": rot.Resealed, "policy": k.Age.String()}}); err != nil {
		return fieldcrypt.Rotation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return fieldcrypt.Rotation{}, fmt.Errorf("controlplane: commit key rotation of tenant %s: %w", tenant, err)
	}
	return rot, nil
}

// RunOnce rotates every tenant due and returns how many. A tenant whose
// rotation fails is reported and left for the next round; the tenants after
// it go on, or one key that cannot be rotated (its master key gone) would
// stop every later tenant's rotation for good.
func (k *KeyRotator) RunOnce(ctx context.Context) (int, error) {
	tenants, err := k.due(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	var failures []error
	for _, t := range tenants {
		if _, err := k.Rotate(ctx, t); err != nil {
			failures = append(failures, fmt.Errorf("rotating the key of tenant %s: %w", t, err))
			continue
		}
		n++
	}
	return n, errors.Join(failures...)
}

// Run rounds until ctx ends, logging failures and work done.
func (k *KeyRotator) Run(ctx context.Context) {
	interval := k.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	for {
		if n, err := k.RunOnce(ctx); err != nil && ctx.Err() == nil {
			k.Log.ErrorContext(ctx, "key rotation round failed", "error", err)
		} else if n > 0 {
			k.Log.InfoContext(ctx, "key rotation round", "rotated", n, "older_than", k.Age.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
