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
			return exec(tx, `UPDATE analytics.snapshot SET manifest_key = $1, watermark = '8', as_of = now(), published_at = now() WHERE manifest_key = $2`,
				key(a.tenant, "00000000000000000008"), key(a.tenant, "00000000000000000007"))
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
