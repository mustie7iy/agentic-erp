// Package offboarding is the data lifecycle of an offboarded tenant
// (ADR-0021): its data is exported to locked object storage, kept in the
// cell for a grace period, then erased. A worker does both in rounds; an
// operator may erase earlier through the control plane. What stays after
// the erasure is the tenant row (that the tenant existed, and what became
// of its data) and the audit schema (what happened, hash-chained and
// anchored); everything else of the tenant is gone from the cell and lives
// on in the export only.
package offboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Actor is the worker's id in audit trails: a system actor, never a token
// holder.
var Actor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:offboarding"))

// Worker exports and erases offboarded tenants.
type Worker struct {
	Pool       *database.Pool
	Store      blobstore.Store // where exports go and the tenant's objects are; nil: the worker does nothing
	AppRole    string          // erp_app: reads and records in the tenant's scope
	WorkerRole string          // erp_worker: finds tenants across the cell, runs the erasure
	Interval   time.Duration   // between rounds; default 1 minute
	Grace      time.Duration   // between export and erasure
	Retention  time.Duration   // how long exports are locked; 0 leaves them unlocked
	ChunkRows  int             // rows per export object; default 10000 (bounds the memory a table takes)
	Log        *slog.Logger
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests replace it to
	// inject database failures.
	Begin func(context.Context, database.Scope) (pgx.Tx, error)
}

func (w *Worker) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	if w.Begin != nil {
		return w.Begin(ctx, s)
	}
	return w.Pool.Begin(ctx, s)
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

// Export is what an export did.
type Export struct {
	Ref        string    `json:"export_ref"` // the manifest's key
	Tables     int       `json:"tables"`
	Rows       int64     `json:"rows"`
	ExportedAt time.Time `json:"exported_at"`
	EraseAfter time.Time `json:"erase_after"`
}

// Erasure is what an erasure did.
type Erasure struct {
	Rows     map[string]int64 `json:"rows"`    // deleted, per table
	Objects  int              `json:"objects"` // deleted from the tenant's prefix
	Kept     int              `json:"kept"`    // objects whose retention holds (anchors, exports)
	ErasedAt time.Time        `json:"erased_at"`
}

// manifest describes an export: the tenant, when, every table with its
// rows and the hash of its file, and the audit chain head at the time.
type manifest struct {
	TenantID   uuid.UUID       `json:"tenant_id"`
	Tenant     json.RawMessage `json:"tenant"`
	ExportedAt time.Time       `json:"exported_at"`
	ChainHead  *chainHead      `json:"audit_chain_head,omitempty"`
	Tables     []exportedTable `json:"tables"`
}

type chainHead struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

type exportedTable struct {
	Name   string `json:"name"`
	Part   int    `json:"part"` // 1, 2, ... within the table
	Key    string `json:"key"`
	Rows   int64  `json:"rows"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// RunOnce does one round: exports the tenants offboarded and not yet
// exported, then erases those whose grace period has ended.
func (w *Worker) RunOnce(ctx context.Context) (exported, erased int, err error) {
	toExport, toErase, err := w.pending(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, t := range toExport {
		if _, err := w.Export(ctx, t); err != nil {
			return exported, erased, fmt.Errorf("exporting tenant %s: %w", t, err)
		}
		exported++
	}
	for _, t := range toErase {
		if _, err := w.Erase(ctx, t, Actor, nil); err != nil {
			return exported, erased, fmt.Errorf("erasing tenant %s: %w", t, err)
		}
		erased++
	}
	return exported, erased, nil
}

// Run rounds until ctx ends, logging failures.
func (w *Worker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	for {
		if exported, erased, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			w.Log.ErrorContext(ctx, "offboarding round failed", "error", err)
		} else if exported+erased > 0 {
			w.Log.InfoContext(ctx, "offboarding round", "exported", exported, "erased", erased)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// pending finds the tenants to export and to erase, as the worker role.
func (w *Worker) pending(ctx context.Context) (toExport, toErase []uuid.UUID, err error) {
	tx, err := w.begin(ctx, database.Scope{Role: w.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT id FROM tenancy.tenant WHERE status = 'offboarded' AND exported_at IS NULL ORDER BY id`)
	if toExport, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
		return nil, nil, err
	}
	rows, _ = tx.Query(ctx, `SELECT id FROM tenancy.tenant WHERE status = 'offboarded' AND erased_at IS NULL AND erase_after <= $1 ORDER BY id`, w.now())
	if toErase, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
		return nil, nil, err
	}
	return toExport, toErase, nil
}

// tables lists the tenant tables: every table with a tenant_id column,
// partitioned parents rather than their partitions.
func tables(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, _ := tx.Query(ctx, `SELECT format('%I.%I', ns.nspname, c.relname) FROM pg_class c
		JOIN pg_namespace ns ON ns.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
		WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition AND ns.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY 1`)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Export writes the tenant's data to the store, one JSON Lines object per
// table and a manifest, all locked for Retention, and records the export
// on the tenant with the time its data may be erased. An export keyed by
// the tenant's offboarding time is written once: objects already there
// (an earlier round that did not get to record itself) are kept and
// described as stored.
func (w *Worker) Export(ctx context.Context, tenant uuid.UUID) (Export, error) {
	if w.Store == nil {
		return Export{}, problem.Unavailable("This server has no object storage; offboarded tenants are not exported.", 0)
	}
	tx, err := w.begin(ctx, database.Scope{Role: w.AppRole, TenantID: tenant, ReadOnly: true})
	if err != nil {
		return Export{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var row json.RawMessage
	var status string
	var offboardedAt time.Time
	var exportedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT row_to_json(t), status, updated_at, exported_at FROM tenancy.tenant t WHERE id = $1`, tenant).Scan(&row, &status, &offboardedAt, &exportedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Export{}, problem.NotFound("No tenant with this id.")
	}
	if err != nil {
		return Export{}, err
	}
	if status != "offboarded" || exportedAt != nil {
		return Export{}, problem.Conflict("tenancy.invalid_transition", "Only an offboarded tenant that has not been exported is exported.")
	}
	names, err := tables(ctx, tx)
	if err != nil {
		return Export{}, err
	}
	now := w.now()
	dir := blobstore.Key(tenant, blobstore.KindExports, strconv.FormatInt(offboardedAt.Unix(), 10))
	opts := blobstore.PutOptions{ContentType: "application/x-ndjson"}
	if w.Retention > 0 {
		opts.RetainUntil = now.Add(w.Retention)
	}
	m := manifest{TenantID: tenant, Tenant: row, ExportedAt: now}
	var ex Export
	for _, name := range names {
		parts, rows, err := w.exportTable(ctx, tx, tenant, name, dir, opts)
		if err != nil {
			return Export{}, err
		}
		m.Tables = append(m.Tables, parts...)
		ex.Rows += rows
	}
	var head chainHead
	var hash []byte
	switch err := tx.QueryRow(ctx, `SELECT seq, hash FROM audit.chain_head WHERE tenant_id = $1`, tenant).Scan(&head.Seq, &hash); {
	case err == nil:
		head.Hash = hex.EncodeToString(hash)
		m.ChainHead = &head
	case !errors.Is(err, pgx.ErrNoRows):
		return Export{}, err
	}
	body, _ := json.Marshal(m) // plain fields and raw JSON always encode
	opts.ContentType = "application/json"
	if _, err := w.put(ctx, dir+"/manifest.json", body, opts); err != nil {
		return Export{}, err
	}
	tx.Rollback(context.WithoutCancel(ctx)) // the read-only transaction is done
	ex.Ref, ex.Tables, ex.ExportedAt, ex.EraseAfter = dir+"/manifest.json", len(m.Tables), now, now.Add(w.Grace)
	// Record the export on the tenant, in its audit trail and its events.
	tx, err = w.begin(ctx, database.Scope{Role: w.AppRole, TenantID: tenant})
	if err != nil {
		return Export{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, err := tx.Exec(ctx, `UPDATE tenancy.tenant SET exported_at = $2, export_ref = $3, erase_after = $4, version = version + 1 WHERE id = $1 AND exported_at IS NULL`,
		tenant, now, ex.Ref, ex.EraseAfter)
	if err != nil {
		return Export{}, err
	}
	if tag.RowsAffected() == 0 {
		return Export{}, problem.Conflict("tenancy.invalid_transition", "The tenant was exported by another round.")
	}
	if err := database.SetActor(ctx, tx, Actor); err != nil {
		return Export{}, err
	}
	payload := map[string]any{"tenant_id": tenant, "export_ref": ex.Ref, "tables": ex.Tables, "rows": ex.Rows, "erase_after": ex.EraseAfter}
	if _, err := outbox.Write(ctx, tx, tenant, Actor, "", outbox.Event{Context: "tenancy", Name: "tenant_exported", AggregateType: "tenancy.tenant", AggregateID: tenant, Payload: payload}); err != nil {
		return Export{}, err
	}
	actor := reqctx.Principal{ID: Actor, Kind: reqctx.KindSystem, TenantID: tenant}
	if err := audit.Write(ctx, tx, actor, "", now, audit.Entry{Action: "tenancy.tenant.export", ObjectType: "tenancy.tenant", ObjectID: tenant, Outcome: "succeeded", Detail: payload}); err != nil {
		return Export{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Export{}, err
	}
	return ex, nil
}

// exportTable streams a table's rows into objects of ChunkRows each, so a
// table takes a bounded amount of memory whatever its size, and returns the
// parts written and the rows they hold.
func (w *Worker) exportTable(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, name, dir string, opts blobstore.PutOptions) ([]exportedTable, int64, error) {
	chunk := w.ChunkRows
	if chunk <= 0 {
		chunk = 10000
	}
	rows, _ := tx.Query(ctx, `SELECT row_to_json(t)::text FROM `+name+` t WHERE tenant_id = $1`, tenant) // errors surface from ForEachRow
	var parts []exportedTable
	var total int64
	var buf strings.Builder
	n := 0
	flush := func() error {
		if n == 0 {
			return nil
		}
		part := len(parts) + 1
		key := fmt.Sprintf("%s/%s.%05d.jsonl", dir, strings.ReplaceAll(name, `"`, ""), part)
		content, err := w.put(ctx, key, []byte(buf.String()), opts)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		parts = append(parts, exportedTable{Name: name, Part: part, Key: key, Rows: int64(n), Bytes: len(content), SHA256: hex.EncodeToString(sum[:])})
		total += int64(n)
		buf.Reset()
		n = 0
		return nil
	}
	var line string
	var stored error // a part that could not be stored, reported as such rather than as a read failure
	if _, err := pgx.ForEachRow(rows, []any{&line}, func() error {
		buf.WriteString(line)
		buf.WriteByte('\n')
		if n++; n == chunk {
			stored = flush()
		}
		return stored
	}); err != nil {
		if stored != nil {
			return nil, 0, stored
		}
		return nil, 0, fmt.Errorf("reading %s: %w", name, err)
	}
	if err := flush(); err != nil {
		return nil, 0, err
	}
	return parts, total, nil
}

// put stores an object of the export, or reads back the one already there
// so that the manifest describes what is stored.
func (w *Worker) put(ctx context.Context, key string, content []byte, opts blobstore.PutOptions) ([]byte, error) {
	err := w.Store.Put(ctx, key, content, opts)
	if errors.Is(err, blobstore.ErrExists) {
		stored, _, gerr := w.Store.Get(ctx, key)
		if gerr != nil {
			return nil, fmt.Errorf("reading the stored %s: %w", key, gerr)
		}
		return stored, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storing %s: %w", key, err)
	}
	return content, nil
}

// Erase deletes the tenant's rows from every table but the audit schema
// and its objects from the store but the locked ones, then records the
// erasure on the tenant row, in its audit trail (which starts over after
// the erasure's own entry) and its events. Only an exported tenant is
// erased; the actor is the worker or the operator who asked, with detail
// for the audit entry.
func (w *Worker) Erase(ctx context.Context, tenant, actor uuid.UUID, detail map[string]any) (Erasure, error) {
	tx, err := w.begin(ctx, database.Scope{Role: w.WorkerRole})
	if err != nil {
		return Erasure{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var status, ref string
	var exportedAt, erasedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT status, coalesce(export_ref, ''), exported_at, erased_at FROM tenancy.tenant WHERE id = $1`, tenant).Scan(&status, &ref, &exportedAt, &erasedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Erasure{}, problem.NotFound("No tenant with this id.")
	}
	if err != nil {
		return Erasure{}, err
	}
	switch {
	case status != "offboarded" || exportedAt == nil:
		return Erasure{}, problem.Conflict("tenancy.invalid_transition", "Only an offboarded tenant whose export is complete is erased.")
	case erasedAt != nil:
		return Erasure{}, problem.Conflict("tenancy.invalid_transition", "The tenant has already been erased.")
	}
	var counts map[string]int64
	if err := tx.QueryRow(ctx, `SELECT platform.erase_tenant($1)`, tenant).Scan(&counts); err != nil {
		return Erasure{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Erasure{}, err
	}
	er := Erasure{Rows: counts, ErasedAt: w.now()}
	if er.Objects, er.Kept, err = w.eraseObjects(ctx, tenant); err != nil {
		return Erasure{}, err
	}
	tx, err = w.begin(ctx, database.Scope{Role: w.AppRole, TenantID: tenant})
	if err != nil {
		return Erasure{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, err := tx.Exec(ctx, `UPDATE tenancy.tenant SET erased_at = $2, version = version + 1 WHERE id = $1 AND erased_at IS NULL`, tenant, er.ErasedAt)
	if err != nil {
		return Erasure{}, err
	}
	if tag.RowsAffected() == 0 {
		return Erasure{}, problem.Conflict("tenancy.invalid_transition", "The tenant was erased by another round.")
	}
	if err := database.SetActor(ctx, tx, actor); err != nil {
		return Erasure{}, err
	}
	var rows int64
	for _, n := range counts {
		rows += n
	}
	payload := map[string]any{"tenant_id": tenant, "export_ref": ref, "rows": rows, "objects": er.Objects}
	if _, err := outbox.Write(ctx, tx, tenant, actor, "", outbox.Event{Context: "tenancy", Name: "tenant_erased", AggregateType: "tenancy.tenant", AggregateID: tenant, Payload: payload}); err != nil {
		return Erasure{}, err
	}
	entry := map[string]any{"export_ref": ref, "rows": counts, "objects": er.Objects, "objects_kept": er.Kept}
	for k, v := range detail {
		entry[k] = v
	}
	principal := reqctx.Principal{ID: actor, Kind: reqctx.KindSystem, TenantID: tenant}
	if err := audit.Write(ctx, tx, principal, "", er.ErasedAt, audit.Entry{Action: "tenancy.tenant.erase", ObjectType: "tenancy.tenant", ObjectID: tenant, Outcome: "succeeded", Detail: entry}); err != nil {
		return Erasure{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Erasure{}, err
	}
	return er, nil
}

// eraseObjects deletes the tenant's objects, keeping those whose retention
// holds (anchors, exports) and counting them.
func (w *Worker) eraseObjects(ctx context.Context, tenant uuid.UUID) (deleted, kept int, err error) {
	eraser, ok := w.Store.(blobstore.Eraser)
	if !ok {
		return 0, 0, nil
	}
	keys, err := eraser.List(ctx, blobstore.Prefix+"/"+tenant.String()+"/")
	if err != nil {
		return 0, 0, err
	}
	for _, key := range keys {
		switch err := eraser.Delete(ctx, key); {
		case errors.Is(err, blobstore.ErrLocked):
			kept++
		case err != nil:
			return deleted, kept, err
		default:
			deleted++
		}
	}
	return deleted, kept, nil
}
