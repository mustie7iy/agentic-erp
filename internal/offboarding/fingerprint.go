package offboarding

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Cross-cell verification (T25, ADR-0030): a tenant's fingerprint is what a
// move between cells compares, the import's checks taken without writing:
// every table's row count and the audit chain head. Two cells holding the
// same tenant give the same fingerprint; the global control plane asks both
// and names every difference.

// ChainHead is the audit chain's last entry: its sequence number and hash.
type ChainHead struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

// TableCount is one table's rows of the tenant.
type TableCount struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
}

// Fingerprint is the tenant's data in numbers: the rows of every table
// carrying a tenant_id (outboxes left out: their events stay in the cell
// they happened in) and the audit chain head, read in one snapshot.
type Fingerprint struct {
	TenantID   uuid.UUID    `json:"tenant_id"`
	ComputedAt time.Time    `json:"computed_at"`
	Tables     []TableCount `json:"tables"`
	ChainHead  *ChainHead   `json:"audit_chain_head"` // null while the tenant has no audit trail
}

// TakeFingerprint reads the tenant's fingerprint in tx, which runs in the
// tenant's scope (the counts stay within row-level security either way).
func TakeFingerprint(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, now time.Time) (Fingerprint, error) {
	list, err := tenantTables(ctx, tx)
	if err != nil {
		return Fingerprint{}, err
	}
	fp := Fingerprint{TenantID: tenant, ComputedAt: now.UTC(), Tables: []TableCount{}}
	for _, t := range list {
		if skipped(t.Name) {
			continue
		}
		var n int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+t.ident()+` WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
			return Fingerprint{}, fmt.Errorf("offboarding: count rows of %s: %w", t.Name, err)
		}
		fp.Tables = append(fp.Tables, TableCount{Name: t.Name, Rows: n})
	}
	if fp.ChainHead, err = readHead(ctx, tx, tenant); err != nil {
		return Fingerprint{}, err
	}
	return fp, nil
}

// readHead reads the tenant's audit chain head; nil when it has none.
func readHead(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) (*ChainHead, error) {
	var head ChainHead
	var hash []byte
	switch err := tx.QueryRow(ctx, `SELECT seq, hash FROM audit.chain_head WHERE tenant_id = $1`, tenant).Scan(&head.Seq, &hash); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("offboarding: read audit chain head of tenant %s: %w", tenant, err)
	}
	head.Hash = hex.EncodeToString(hash)
	return &head, nil
}
