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

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
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
func skipped(table string) bool { return strings.HasSuffix(table, ".outbox") || Ephemeral(table) }

// Ephemeral reports a tenant table that is neither exported, imported nor
// fingerprinted: people's sessions (ADR-0021 as amended by ADR-0031), which
// end when the tenant moves and change with every request.
func Ephemeral(table string) bool { return table == "identity.session" }

// Import reads the manifest at ref and writes the tenant into the cell, in
// one transaction. The tenant's rows must not be there already (a tenant
// present and not erased is refused); its tombstone row, if any, is brought
// back to the exported state.
func (im *Importer) Import(ctx context.Context, ref string) (Imported, error) {
	if im.Store == nil {
		return Imported{}, errors.New("offboarding: no object store to import from")
	}
	m, err := im.readManifest(ctx, ref)
	if err != nil {
		return Imported{}, err
	}
	parts, order := tableParts(m)
	out := Imported{TenantID: m.TenantID, ExportedAt: m.ExportedAt, Rows: map[string]int64{}, ChainHead: m.ChainHead}
	err = im.inTx(ctx, database.Scope{Role: im.AppRole, TenantID: m.TenantID}, func(ctx context.Context, tx pgx.Tx) error {
		return im.importTenant(ctx, tx, m, order, parts, &out)
	})
	if err != nil {
		return Imported{}, err
	}
	return out, nil
}

// readManifest reads and decodes the export manifest at ref.
func (im *Importer) readManifest(ctx context.Context, ref string) (manifest, error) {
	body, _, err := im.Store.Get(ctx, ref)
	if err != nil {
		return manifest{}, fmt.Errorf("offboarding: reading the manifest %s: %w", ref, err)
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil || m.TenantID == uuid.Nil {
		return manifest{}, fmt.Errorf("offboarding: %s is not an export manifest", ref)
	}
	return m, nil
}

// tableParts groups the manifest's parts by table, in the order the
// tables first appear, without the skipped tables.
func tableParts(m manifest) (map[string][]exportedTable, []string) {
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
	return parts, order
}

// importTenant writes the tenant into the cell within tx: its row, then
// its tables' rows once each table is found empty, then checks the audit
// chain head. The manifest's table names are resolved first: no statement
// uses a name the cell does not export.
func (im *Importer) importTenant(ctx context.Context, tx pgx.Tx, m manifest, order []string, parts map[string][]exportedTable, out *Imported) error {
	idents, err := resolveTables(ctx, tx, order)
	if err != nil {
		return err
	}
	if err := im.tenantRow(ctx, tx, m); err != nil {
		return err
	}
	if err := checkEmpty(ctx, tx, m.TenantID, order, idents); err != nil {
		return err
	}
	rows, cols, err := im.readTables(ctx, tx, order, idents, parts)
	if err != nil {
		return err
	}
	if err := im.insertInPasses(ctx, tx, order, idents, cols, rows, out); err != nil {
		return err
	}
	return checkChainHead(ctx, tx, m)
}

// resolveTables maps each table the manifest names to its quoted
// identifier. The manifest itself is not hash-checked, so its names are
// untrusted: only the tenant tables this cell exports are accepted, and
// any other name is refused before a statement uses it.
func resolveTables(ctx context.Context, tx pgx.Tx, order []string) (map[string]string, error) {
	list, err := tenantTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	known := make(map[string]string, len(list))
	for _, t := range list {
		known[t.Name] = t.ident()
	}
	idents := make(map[string]string, len(order))
	for _, name := range order {
		ident, ok := known[name]
		if !ok {
			return nil, fmt.Errorf("offboarding: the manifest names %q, which is not a table this cell exports", name)
		}
		idents[name] = ident
	}
	return idents, nil
}

// checkEmpty refuses the import when a table already holds rows of the
// tenant.
func checkEmpty(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, order []string, idents map[string]string) error {
	for _, name := range order {
		var n int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+idents[name]+` WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
			return fmt.Errorf("offboarding: count rows of %s: %w", name, err)
		}
		if n > 0 {
			return fmt.Errorf("offboarding: %s already holds %d rows of the tenant; import into an empty or erased tenant", name, n)
		}
	}
	return nil
}

// readTables reads every table's insertable columns and its rows from the
// store, each part checked against the manifest.
func (im *Importer) readTables(ctx context.Context, tx pgx.Tx, order []string, idents map[string]string, parts map[string][]exportedTable) (map[string][]json.RawMessage, map[string][]string, error) {
	rows := map[string][]json.RawMessage{}
	cols := map[string][]string{}
	for _, name := range order {
		var err error
		if cols[name], err = columns(ctx, tx, idents[name]); err != nil {
			return nil, nil, fmt.Errorf("offboarding: columns of %s: %w", name, err)
		}
		for _, p := range parts[name] {
			if err := im.readPart(ctx, name, p, rows); err != nil {
				return nil, nil, err
			}
		}
	}
	return rows, cols, nil
}

// readPart appends a part's rows to the table's in rows, after checking
// the part's hash and, once read, its row count.
func (im *Importer) readPart(ctx context.Context, name string, p exportedTable, rows map[string][]json.RawMessage) error {
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
	return nil
}

// insertInPasses inserts the tables' rows. Tables whose rows still
// reference rows not yet inserted wait for the next pass; each table's
// insert runs under a savepoint so a foreign key violation does not end
// the transaction.
func (im *Importer) insertInPasses(ctx context.Context, tx pgx.Tx, order []string, idents map[string]string, cols map[string][]string, rows map[string][]json.RawMessage, out *Imported) error {
	todo := order
	for pass := 1; len(todo) > 0; pass++ {
		var next []string
		for _, name := range todo {
			n, wait, err := im.insertTable(ctx, tx, name, idents[name], cols[name], rows[name])
			if err != nil {
				return err
			}
			if wait {
				next = append(next, name)
				continue
			}
			out.Rows[name] = n
			out.Tables++
		}
		if len(next) == len(todo) {
			return fmt.Errorf("offboarding: rows of %s stay referenced after %d passes", strings.Join(next, ", "), pass)
		}
		todo = next
	}
	return nil
}

// insertTable inserts a table's rows under a savepoint. When they
// reference rows not inserted yet, the savepoint is rolled back and wait
// is set: the table waits for the next pass.
func (im *Importer) insertTable(ctx context.Context, tx pgx.Tx, name, ident string, cols []string, rows []json.RawMessage) (n int64, wait bool, err error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("offboarding: open savepoint for %s: %w", name, err)
	}
	n, err = im.insert(ctx, sp, ident, cols, rows)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23503" { // foreign_key_violation
		if err := sp.Rollback(ctx); err != nil {
			return 0, false, fmt.Errorf("offboarding: roll back savepoint for %s: %w", name, err)
		}
		return 0, true, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("offboarding: inserting into %s: %w", name, err)
	}
	if err := sp.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("offboarding: release savepoint for %s: %w", name, err)
	}
	return n, false, nil
}

// checkChainHead compares the imported audit chain head with the one the
// export recorded, when it recorded one.
func checkChainHead(ctx context.Context, tx pgx.Tx, m manifest) error {
	if m.ChainHead != nil {
		head, ok, err := audit.ReadHead(ctx, tx, m.TenantID)
		if err == nil && !ok {
			err = pgx.ErrNoRows
		}
		if err != nil {
			return fmt.Errorf("offboarding: the audit chain head was not imported: %w", err)
		}
		if head.Seq != m.ChainHead.Seq || hex.EncodeToString(head.Hash) != m.ChainHead.Hash {
			return fmt.Errorf("offboarding: the audit chain head (%d) differs from the export's (%d)", head.Seq, m.ChainHead.Seq)
		}
	}
	return nil
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
	row := tenancy.NewTenant{ID: m.TenantID, Name: t.Name, Region: t.Region, GroupCurrency: t.GroupCurrency}
	existing, err := tenancy.GetTenant(ctx, tx, m.TenantID, false)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := tenancy.CreateTenant(ctx, tx, row); err != nil {
			return fmt.Errorf("offboarding: create tenant %s: %w", m.TenantID, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("offboarding: %w", err)
	case existing.ErasedAt == nil:
		return fmt.Errorf("offboarding: tenant %s is %s in this cell and not erased; import into an empty or erased tenant", m.TenantID, existing.Status)
	}
	if err := tenancy.RestoreTenant(ctx, tx, row); err != nil {
		return fmt.Errorf("offboarding: %w", err)
	}
	return nil
}

// insert writes a table's rows from their JSON into the columns given (the
// table's, without the generated ones the database computes), and lets the
// database check the rest. ident is the table's quoted identifier.
func (im *Importer) insert(ctx context.Context, tx pgx.Tx, ident string, cols []string, rows []json.RawMessage) (int64, error) {
	list := make([]string, len(cols))
	for i, c := range cols {
		list[i] = pgx.Identifier{c}.Sanitize()
	}
	stmt := fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM json_populate_record(NULL::%s, $1::json)`, ident, strings.Join(list, ", "), strings.Join(list, ", "), ident)
	for i, row := range rows {
		if _, err := tx.Exec(ctx, stmt, string(row)); err != nil {
			return 0, fmt.Errorf("insert row %d: %w", i+1, err)
		}
	}
	return int64(len(rows)), nil
}

// columns lists a table's columns that can be inserted: not generated, not
// dropped. ident is the table's quoted identifier.
func columns(ctx context.Context, tx pgx.Tx, ident string) ([]string, error) {
	rows, _ := tx.Query(ctx, `SELECT a.attname FROM pg_attribute a WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = '' ORDER BY a.attnum`, ident)
	cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return cols, fmt.Errorf("read catalog: %w", err)
	}
	return cols, nil
}
