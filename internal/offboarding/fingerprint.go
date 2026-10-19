package offboarding

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
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
	head, ok, err := audit.ReadHead(ctx, tx, tenant)
	if err != nil || !ok {
		return nil, err
	}
	return &ChainHead{Seq: head.Seq, Hash: hex.EncodeToString(head.Hash)}, nil
}

// Differences names what differs between two fingerprints of one tenant,
// a and b, taken in the places aName and bName (two cells, or a restored
// database and the old one): a missing tenant, a table's rows, the audit
// chain head. Empty when they agree.
func Differences(aName string, a *Fingerprint, bName string, b *Fingerprint) []string {
	switch {
	case a == nil && b == nil:
		return []string{"neither " + aName + " nor " + bName + " has the tenant"}
	case a == nil:
		return []string{aName + " has no tenant with this id"}
	case b == nil:
		return []string{bName + " has no tenant with this id"}
	}
	out := tableDifferences(aName, a, bName, b)
	if h := headDifference(aName, a.ChainHead, bName, b.ChainHead); h != "" {
		out = append(out, h)
	}
	return out
}

func tableDifferences(aName string, a *Fingerprint, bName string, b *Fingerprint) []string {
	out := []string{}
	rows := map[string][2]*int64{}
	var names []string
	for _, t := range a.Tables {
		n := t.Rows
		rows[t.Name] = [2]*int64{&n, nil}
		names = append(names, t.Name)
	}
	for _, t := range b.Tables {
		n := t.Rows
		r, seen := rows[t.Name]
		if !seen {
			names = append(names, t.Name)
		}
		r[1] = &n
		rows[t.Name] = r
	}
	for _, name := range names {
		r := rows[name]
		switch {
		case r[0] == nil:
			out = append(out, fmt.Sprintf("%s: no table in %s, %d rows in %s", name, aName, *r[1], bName))
		case r[1] == nil:
			out = append(out, fmt.Sprintf("%s: %d rows in %s, no table in %s", name, *r[0], aName, bName))
		case *r[0] != *r[1]:
			out = append(out, fmt.Sprintf("%s: %d rows in %s, %d in %s", name, *r[0], aName, *r[1], bName))
		}
	}
	return out
}

func headDifference(aName string, x *ChainHead, bName string, y *ChainHead) string {
	switch {
	case x == nil && y == nil:
		return ""
	case x == nil:
		return fmt.Sprintf("audit chain head: none in %s, seq %d in %s", aName, y.Seq, bName)
	case y == nil:
		return fmt.Sprintf("audit chain head: seq %d in %s, none in %s", x.Seq, aName, bName)
	case x.Seq != y.Seq || x.Hash != y.Hash:
		return fmt.Sprintf("audit chain head: seq %d (%s) in %s, seq %d (%s) in %s", x.Seq, short(x.Hash), aName, y.Seq, short(y.Hash), bName)
	}
	return ""
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12] + "…"
	}
	return hash
}
