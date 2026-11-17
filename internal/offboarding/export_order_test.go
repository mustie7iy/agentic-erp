package offboarding_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// partFails refuses to store the object whose key ends in suffix.
type partFails struct {
	blobstore.Store
	suffix string
}

func (f partFails) Put(ctx context.Context, key string, content []byte, opts blobstore.PutOptions) error {
	if strings.HasSuffix(key, f.suffix) {
		return errors.New("store full")
	}
	return f.Store.Put(ctx, key, content, opts)
}

// planned begins the worker's transactions with the planner's settings
// given, as a different plan for the same query would be chosen after the
// table's statistics changed.
func planned(pool *database.Pool, settings string) database.BeginFunc {
	return func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, settings); err != nil {
			_ = tx.Rollback(ctx) // the setting's error is the one that matters
			return nil, err
		}
		return tx, nil
	}
}

// TestResumedExportReadsInKeyOrder: an export that stopped part-way through
// a table and resumes keeps the parts already stored, so it must read the
// table's rows in the same order as before, whatever plan the database
// picks now. Here the first round reads the principals through their index
// and stores the first one; an update then moves that row to the end of
// the table, and the export resumes reading the table in physical order.
// Every row is in the export exactly once.
func TestResumedExportReadsInKeyOrder(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t)
	e := fixture.New(t, "payables.invoice.read")
	tenant := e.Tenant.ID
	clock := time.Now().UTC().Truncate(time.Second)
	store := blobstore.Dir{Root: t.TempDir(), Now: func() time.Time { return clock }}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', version = version + 1 WHERE id = $1`, tenant)
	t.Cleanup(func() { // the package's other tests run worker rounds: they find nothing of this tenant's to do
		pgtest.Exec(t, `UPDATE tenancy.tenant SET erased_at = now(), version = version + 1 WHERE id = $1`, tenant)
	})

	first := worker(t, pool, partFails{Store: store, suffix: "/identity.principal.00002.jsonl"}, &clock)
	first.ChunkRows = 1
	first.Begin = planned(pool, `SET LOCAL enable_seqscan = off`)
	if _, err := first.Export(ctx, tenant); err == nil {
		t.Fatal("first round: got nil error, want the second principal part refused")
	}
	var moved uuid.UUID
	pgtest.Scan(t, `SELECT id FROM identity.principal WHERE tenant_id = $1 ORDER BY id LIMIT 1`, []any{tenant}, &moved)
	pgtest.Exec(t, `UPDATE identity.principal SET display_name = display_name || '', version = version + 1 WHERE tenant_id = $1 AND id = $2`, tenant, moved)

	resumed := worker(t, pool, store, &clock)
	resumed.ChunkRows = 1
	resumed.Begin = planned(pool, `SET LOCAL enable_indexscan = off; SET LOCAL enable_bitmapscan = off`)
	ex, err := resumed.Export(ctx, tenant)
	if err != nil {
		t.Fatalf("resumed round: %v", err)
	}
	got := exportedPrincipals(t, store, ex.Ref)
	var want []uuid.UUID
	rows := pgtest.Admin(t)
	r, err := rows.Query(ctx, `SELECT id FROM identity.principal WHERE tenant_id = $1`, tenant)
	if err != nil {
		t.Fatalf("read the principals: %v", err)
	}
	for r.Next() {
		var id uuid.UUID
		if err := r.Scan(&id); err != nil {
			t.Fatalf("scan a principal: %v", err)
		}
		want = append(want, id)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("read the principals: %v", err)
	}
	if len(want) < 2 {
		t.Fatalf("fixture: got %d principals, want at least 2", len(want))
	}
	for _, id := range want {
		if got[id] != 1 {
			t.Errorf("principal %s: exported %d times, want once", id, got[id])
		}
	}
	if len(got) != len(want) {
		t.Errorf("exported principals: got %d distinct, want %d", len(got), len(want))
	}
}

// exportedPrincipals reads the manifest at ref and counts how often each
// principal's row is in the export's principal parts.
func exportedPrincipals(t *testing.T, store blobstore.Dir, ref string) map[uuid.UUID]int {
	t.Helper()
	ctx := context.Background()
	body, _, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	var m struct{ Tables []struct{ Name, Key string } }
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	seen := map[uuid.UUID]int{}
	for _, tb := range m.Tables {
		if tb.Name != "identity.principal" {
			continue
		}
		lines, _, err := store.Get(ctx, tb.Key)
		if err != nil {
			t.Fatalf("read %s: %v", tb.Key, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(lines)), "\n") {
			var row struct{ ID uuid.UUID }
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("decode a principal row: %v", err)
			}
			seen[row.ID]++
		}
	}
	return seen
}
