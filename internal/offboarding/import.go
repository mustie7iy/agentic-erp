package offboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// Re-onboarding (T24, ADR-0029): an export (ADR-0021) is also a tenant's
// portable form. Import reads a manifest and its parts from the store and
// writes the tenant back into a cell: the tenant row, then every table's
// rows in passes over foreign keys, each part checked against its hash,
// each table's count against the manifest, and the audit chain head
// against the one the export recorded. A tenant erased from this cell comes
// back; a tenant moving between cells is copied this way.

// Importer brings exported tenants into the cell.
type Importer struct {
	Pool    *database.Pool
	Store   blobstore.Store
	AppRole string // erp_app: writes in the tenant's scope
	// InTx runs the import's transaction; nil: Pool.InTx. Tests inject failures.
	InTx func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error
}

func (im *Importer) inTx(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
	if im.InTx != nil {
		return im.InTx(ctx, scope, fn)
	}
	return im.Pool.InTx(ctx, scope, fn)
}

// Imported is what an import did.
type Imported struct {
	TenantID   uuid.UUID        `json:"tenant_id"`
	ExportedAt time.Time        `json:"exported_at"`
	Tables     int              `json:"tables"`
	Rows       map[string]int64 `json:"rows"` // inserted, per table
	ChainHead  *ChainHead       `json:"audit_chain_head,omitempty"`
}

// skipped tables: an outbox's events were delivered in the cell they
// happened in; importing them would publish them again.
func skipped(table string) bool { return strings.HasSuffix(table, ".outbox") }

// Import reads the manifest at ref and writes the tenant into the cell, in
// one transaction. The tenant's rows must not be there already (a tenant
// present and not erased is refused); its tombstone row, if any, is brought
// back to the exported state.
func (im *Importer) Import(ctx context.Context, ref string) (Imported, error) {
	if im.Store == nil {
		return Imported{}, errors.New("offboarding: no object store to import from")
	}
	body, _, err := im.Store.Get(ctx, ref)
	if err != nil {
		return Imported{}, fmt.Errorf("offboarding: reading the manifest %s: %w", ref, err)
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil || m.TenantID == uuid.Nil {
		return Imported{}, fmt.Errorf("offboarding: %s is not an export manifest", ref)
	}
	parts := map[string][]exportedTable{}
	order := []string{}
	for _, p := range m.Tables {
		if skipped(p.Name) {
			continue
		}
		if _, seen := parts[p.Name]; !seen {
			order = append(order, p.Name)
		}
		parts[p.Name] = append(parts[p.Name], p)
	}
	out := Imported{TenantID: m.TenantID, ExportedAt: m.ExportedAt, Rows: map[string]int64{}, ChainHead: m.ChainHead}
	err = im.inTx(ctx, database.Scope{Role: im.AppRole, TenantID: m.TenantID}, func(ctx context.Context, tx pgx.Tx) error {
		if err := im.tenantRow(ctx, tx, m); err != nil {
			return err
		}
		for _, name := range order {
			var n int64
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+name+` WHERE tenant_id = $1`, m.TenantID).Scan(&n); err != nil {
				return fmt.Errorf("offboarding: count rows of %s: %w", name, err)
			}
			if n > 0 {
				return fmt.Errorf("offboarding: %s already holds %d rows of the tenant; import into an empty or erased tenant", name, n)
			}
		}
		rows := map[string][]json.RawMessage{}
		cols := map[string][]string{}
		for _, name := range order {
			var err error
			if cols[name], err = columns(ctx, tx, name); err != nil {
				return fmt.Errorf("offboarding: columns of %s: %w", name, err)
			}
			for _, p := range parts[name] {
				content, _, err := im.Store.Get(ctx, p.Key)
				if err != nil {
					return fmt.Errorf("offboarding: reading %s: %w", p.Key, err)
				}
				if sum := sha256.Sum256(content); hex.EncodeToString(sum[:]) != p.SHA256 {
					return fmt.Errorf("offboarding: %s does not match the manifest's hash", p.Key)
				}
				var count int64
				for _, line := range strings.Split(strings.TrimRight(string(content), "\n"), "\n") {
					rows[name] = append(rows[name], json.RawMessage(line)) // one JSON object per line, as the export writes them
					count++
				}
				if count != p.Rows {
					return fmt.Errorf("offboarding: %s holds %d rows, the manifest says %d", p.Key, count, p.Rows)
				}
			}
		}
		// Tables whose rows still reference rows not yet inserted wait for the
		// next pass; each table's insert runs under a savepoint so a foreign
		// key violation does not end the transaction.
		todo := order
		for pass := 1; len(todo) > 0; pass++ {
			var next []string
			for _, name := range todo {
				sp, err := tx.Begin(ctx)
				if err != nil {
					return fmt.Errorf("offboarding: open savepoint for %s: %w", name, err)
				}
				n, err := im.insert(ctx, sp, name, cols[name], rows[name])
				var pg *pgconn.PgError
				if errors.As(err, &pg) && pg.Code == "23503" { // foreign_key_violation
					if err := sp.Rollback(ctx); err != nil {
						return fmt.Errorf("offboarding: roll back savepoint for %s: %w", name, err)
					}
					next = append(next, name)
					continue
				}
				if err != nil {
					return fmt.Errorf("offboarding: inserting into %s: %w", name, err)
				}
				if err := sp.Commit(ctx); err != nil {
					return fmt.Errorf("offboarding: release savepoint for %s: %w", name, err)
				}
				out.Rows[name] = n
				out.Tables++
			}
			if len(next) == len(todo) {
				return fmt.Errorf("offboarding: rows of %s stay referenced after %d passes", strings.Join(next, ", "), pass)
			}
			todo = next
		}
		if m.ChainHead != nil {
			var seq int64
			var hash []byte
			if err := tx.QueryRow(ctx, `SELECT seq, hash FROM audit.chain_head WHERE tenant_id = $1`, m.TenantID).Scan(&seq, &hash); err != nil {
				return fmt.Errorf("offboarding: the audit chain head was not imported: %w", err)
			}
			if seq != m.ChainHead.Seq || hex.EncodeToString(hash) != m.ChainHead.Hash {
				return fmt.Errorf("offboarding: the audit chain head (%d) differs from the export's (%d)", seq, m.ChainHead.Seq)
			}
		}
		return nil
	})
	if err != nil {
		return Imported{}, err
	}
	return out, nil
}

// tenantRow writes the tenant's own row: created when the cell has none,
// brought back from its tombstone when the tenant was erased here, refused
// when the tenant is live.
func (im *Importer) tenantRow(ctx context.Context, tx pgx.Tx, m manifest) error {
	var t struct {
		Name          string `json:"name"`
		Region        string `json:"region"`
		GroupCurrency string `json:"group_currency"`
		Status        string `json:"status"`
	}
	if err := json.Unmarshal(m.Tenant, &t); err != nil || t.Name == "" {
		return fmt.Errorf("offboarding: the manifest's tenant row is not readable")
	}
	var status string
	var erased *time.Time
	switch err := tx.QueryRow(ctx, `SELECT status, erased_at FROM tenancy.tenant WHERE id = $1`, m.TenantID).Scan(&status, &erased); {
	case errors.Is(err, pgx.ErrNoRows):
		_, err := tx.Exec(ctx, `INSERT INTO tenancy.tenant (id, name, region, group_currency, status) VALUES ($1, $2, $3, $4, 'active')`, m.TenantID, t.Name, t.Region, t.GroupCurrency)
		if err != nil {
			return fmt.Errorf("offboarding: create tenant %s: %w", m.TenantID, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("offboarding: read tenant %s: %w", m.TenantID, err)
	case erased == nil:
		return fmt.Errorf("offboarding: tenant %s is %s in this cell and not erased; import into an empty or erased tenant", m.TenantID, status)
	}
	_, err := tx.Exec(ctx, `UPDATE tenancy.tenant SET status = 'active', name = $2, region = $3, group_currency = $4,
		exported_at = NULL, export_ref = NULL, erase_after = NULL, erased_at = NULL, version = version + 1 WHERE id = $1`, m.TenantID, t.Name, t.Region, t.GroupCurrency)
	if err != nil {
		return fmt.Errorf("offboarding: restore tenant %s: %w", m.TenantID, err)
	}
	return nil
}

// insert writes a table's rows from their JSON into the columns given (the
// table's, without the generated ones the database computes), and lets the
// database check the rest.
func (im *Importer) insert(ctx context.Context, tx pgx.Tx, table string, cols []string, rows []json.RawMessage) (int64, error) {
	list := make([]string, len(cols))
	for i, c := range cols {
		list[i] = pgx.Identifier{c}.Sanitize()
	}
	stmt := fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM json_populate_record(NULL::%s, $1::json)`, table, strings.Join(list, ", "), strings.Join(list, ", "), table)
	for i, row := range rows {
		if _, err := tx.Exec(ctx, stmt, string(row)); err != nil {
			return 0, fmt.Errorf("insert row %d: %w", i+1, err)
		}
	}
	return int64(len(rows)), nil
}

// columns lists a table's columns that can be inserted: not generated, not
// dropped.
func columns(ctx context.Context, tx pgx.Tx, table string) ([]string, error) {
	rows, _ := tx.Query(ctx, `SELECT a.attname FROM pg_attribute a WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = '' ORDER BY a.attnum`, table)
	cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return cols, fmt.Errorf("read catalog: %w", err)
	}
	return cols, nil
}
