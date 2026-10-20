package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The analytical store's pointer and watermark (ADR-0044, T55b, migration
// 000091): the rules the database keeps for them.

// TestJournalEntriesRecordTheirTransaction: an entry records the transaction
// that posted it, the extract's watermark.
func TestJournalEntriesRecordTheirTransaction(t *testing.T) {
	need(t)
	f := newFixture(t)
	var same bool
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		je, err := f.entry(tx, 3, "2026-03-15", line{account: f.expense, amount: 100}, line{account: f.bank, amount: -100})
		if err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT posted_xid = pg_current_xact_id() FROM ledger.journal_entry WHERE id = $1`, je).Scan(&same)
	})
	if !same {
		t.Error("posted_xid: got another transaction's id, want the posting transaction's")
	}
}

// TestSnapshotPointer: one pointer per tenant, under row-level security,
// naming the manifest of its watermark, moved but never deleted by the
// application.
func TestSnapshotPointer(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	key := func(tenant uuid.UUID, wm string) string {
		return "analytics/v1/tenant=" + tenant.String() + "/manifest-" + wm + ".json"
	}
	insert := `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of) VALUES ($1, $2, $3::text::xid8, now())`
	mustApp(t, a.tenant, func(tx pgx.Tx) error { return exec(tx, insert, a.tenant, key(a.tenant, "00000000000000000007"), "7") })

	for name, c := range map[string]struct {
		as   uuid.UUID
		sql  string
		args []any
		want string
	}{
		"second pointer":          {a.tenant, insert, []any{a.tenant, key(a.tenant, "00000000000000000008"), "8"}, "duplicate key"},
		"other watermark's name":  {b.tenant, insert, []any{b.tenant, key(b.tenant, "00000000000000000009"), "8"}, "snapshot_manifest_key_check"},
		"other tenant's manifest": {b.tenant, insert, []any{b.tenant, key(a.tenant, "00000000000000000008"), "8"}, "snapshot_manifest_key_check"},
		"unpadded name":           {b.tenant, insert, []any{b.tenant, key(b.tenant, "8"), "8"}, "snapshot_manifest_key_check"},
		"short digest":            {b.tenant, insert, []any{b.tenant, key(b.tenant, "00000000000000000008-0123"), "8"}, "snapshot_manifest_key_check"},
		"other suffix":            {b.tenant, insert, []any{b.tenant, key(b.tenant, "00000000000000000008-0123456789abcdeg"), "8"}, "snapshot_manifest_key_check"},
		"another tenant's row":    {b.tenant, insert, []any{a.tenant, key(a.tenant, "00000000000000000008"), "8"}, "row-level security"},
		"delete":                  {a.tenant, `DELETE FROM analytics.snapshot`, nil, "permission denied"},
		"move to another tenant":  {a.tenant, `UPDATE analytics.snapshot SET tenant_id = $1`, []any{b.tenant}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, c.as, func(tx pgx.Tx) error { return exec(tx, c.sql, c.args...) }), c.want)
		})
	}

	t.Run("moved", func(t *testing.T) {
		mustApp(t, a.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE analytics.snapshot SET manifest_key = $1, watermark = '8', as_of = now(), published_at = now(), checked_at = now()
				WHERE manifest_key = $2`, key(a.tenant, "00000000000000000008-0123456789abcdef"), key(a.tenant, "00000000000000000007"))
		})
	})
	t.Run("invisible to another tenant", func(t *testing.T) {
		var n int
		mustApp(t, b.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT count(*) FROM analytics.snapshot`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("pointers seen by tenant B: got %d, want 0", n)
		}
	})
}

// inRole runs fn in a transaction as role, which bypasses row-level
// security: the outbox relay's and the workers'.
func inRole(t *testing.T, role string, fn func(tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
		t.Fatal(err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TestExtractQueue (000092, 000093): the relay and the sweep mark tenants
// changed, the worker claims and finishes their runs, the application only
// reads its own row.
func TestExtractQueue(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	t.Cleanup(func() {
		_, _ = testDB.Exec(context.Background(), `DELETE FROM analytics.extract_due WHERE tenant_id IN ($1, $2)`, a.tenant, b.tenant) // best effort: the rows only queue extracts
	})
	mark := `INSERT INTO analytics.extract_due (tenant_id, changed_at) VALUES ($1, now())`
	if err := inRole(t, "erp_relay", func(tx pgx.Tx) error { return exec(tx, mark, a.tenant) }); err != nil {
		t.Fatalf("relay mark: %v", err)
	}
	if err := inRole(t, "erp_worker", func(tx pgx.Tx) error {
		return exec(tx, `UPDATE analytics.extract_due SET started_at = now(), done_at = now(), claimed_lsn = pg_current_wal_lsn() WHERE tenant_id = $1`, a.tenant)
	}); err != nil {
		t.Fatalf("worker claim: %v", err)
	}
	if err := inRole(t, "erp_worker", func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO analytics.extract_due AS d (tenant_id, changed_at) VALUES ($1, now())
			ON CONFLICT (tenant_id) DO UPDATE SET changed_at = excluded.changed_at`, a.tenant)
	}); err != nil {
		t.Fatalf("worker sweep: %v", err)
	}
	for name, c := range map[string]struct {
		role, sql string
		want      string
	}{
		"application queues":    {"erp_app", mark, "permission denied"},
		"relay claims":          {"erp_relay", `UPDATE analytics.extract_due SET started_at = now()`, "permission denied"},
		"worker deletes":        {"erp_worker", `DELETE FROM analytics.extract_due`, "permission denied"},
		"done before its claim": {"erp_worker", `UPDATE analytics.extract_due SET done_at = started_at + interval '1 second'`, "extract_due_done_at_claim"},
	} {
		t.Run(name, func(t *testing.T) {
			if c.role == "erp_app" {
				failsWith(t, asApp(t, b.tenant, func(tx pgx.Tx) error { return exec(tx, c.sql, b.tenant) }), c.want)
				return
			}
			failsWith(t, inRole(t, c.role, func(tx pgx.Tx) error { return exec(tx, c.sql+` WHERE tenant_id = $1`, a.tenant) }), c.want)
		})
	}
	t.Run("invisible to another tenant", func(t *testing.T) {
		var n int
		mustApp(t, b.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT count(*) FROM analytics.extract_due`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("queue rows seen by tenant B: got %d, want 0", n)
		}
	})
}

// TestSupersededList (000093): the manifests a tenant's publications
// replaced, under row-level security, named for the tenant; the
// application lists its own, the worker forgets them and clears a
// tenant's store for a rebuild.
func TestSupersededList(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	key := func(tenant uuid.UUID, wm string) string {
		return "analytics/v1/tenant=" + tenant.String() + "/manifest-" + wm + ".json"
	}
	list := `INSERT INTO analytics.superseded (tenant_id, manifest_key) VALUES ($1, $2)
		ON CONFLICT (tenant_id, manifest_key) DO UPDATE SET superseded_at = excluded.superseded_at`
	mustApp(t, a.tenant, func(tx pgx.Tx) error { return exec(tx, list, a.tenant, key(a.tenant, "00000000000000000007")) })
	mustApp(t, a.tenant, func(tx pgx.Tx) error { return exec(tx, list, a.tenant, key(a.tenant, "00000000000000000007")) })
	for name, c := range map[string]struct {
		as   uuid.UUID
		args []any
		want string
	}{
		"other tenant's manifest": {b.tenant, []any{b.tenant, key(a.tenant, "00000000000000000008")}, "superseded_manifest_key_check"},
		"not a manifest":          {b.tenant, []any{b.tenant, "analytics/v1/tenant=" + b.tenant.String() + "/gl_lines/x.parquet"}, "superseded_manifest_key_check"},
		"another tenant's row":    {b.tenant, []any{a.tenant, key(a.tenant, "00000000000000000008")}, "row-level security"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, asApp(t, c.as, func(tx pgx.Tx) error { return exec(tx, list, c.args...) }), c.want)
		})
	}
	t.Run("invisible to another tenant", func(t *testing.T) {
		var n int
		mustApp(t, b.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT count(*) FROM analytics.superseded`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("superseded rows seen by tenant B: got %d, want 0", n)
		}
	})
	t.Run("forgotten by the worker alone", func(t *testing.T) {
		failsWith(t, asApp(t, a.tenant, func(tx pgx.Tx) error { return exec(tx, `DELETE FROM analytics.superseded`) }), "permission denied")
		if err := inRole(t, "erp_worker", func(tx pgx.Tx) error {
			return exec(tx, `DELETE FROM analytics.superseded WHERE tenant_id = $1`, a.tenant)
		}); err != nil {
			t.Fatalf("worker forgets: %v", err)
		}
	})
	t.Run("rebuilt by the worker", func(t *testing.T) {
		mustApp(t, a.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of) VALUES ($1, $2, '7', now())`,
				a.tenant, key(a.tenant, "00000000000000000007"))
		})
		if err := inRole(t, "erp_worker", func(tx pgx.Tx) error {
			return exec(tx, `DELETE FROM analytics.snapshot WHERE tenant_id = $1`, a.tenant)
		}); err != nil {
			t.Fatalf("worker rebuild: %v", err)
		}
	})
}
