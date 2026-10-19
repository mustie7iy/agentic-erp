package drverify_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/drverify"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// cell is a migrated database of its own: Verify reads every tenant, so a
// test's corruption must not reach another test's cell.
type cell struct {
	pool *database.Pool
	url  string
}

func newCell(t *testing.T) cell {
	t.Helper()
	ctx := context.Background()
	u := pgtest.EmptyDatabase(t)
	c, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }() // migrated or failed; closing loses nothing
	if _, err := db.Migrate(ctx, c); err != nil {
		t.Fatal(err)
	}
	p, err := database.Open(ctx, u, database.DefaultSettings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return cell{pool: p, url: u}
}

// exec runs fixture SQL as superuser in the cell; unguarded turns the
// triggers (foreign keys, the balance check) off, for a state a restore
// that lost rows leaves behind.
func (c cell) exec(t *testing.T, unguarded bool, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, c.url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }() // the statement's outcome is checked; closing loses nothing
	if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if unguarded {
			if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

func (c cell) tenant(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	c.exec(t, false, `INSERT INTO tenancy.tenant (id, name, region, group_currency, status) VALUES ($1, 'Drill', 'eu-west', 'EUR', 'active')`, id)
	return id
}

// entry posts a balanced two-line entry; the lines' ids are returned so a
// test can tear one away.
func (c cell) entry(t *testing.T, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	c.exec(t, true, `WITH e AS (INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
			document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by)
		VALUES ($1, 2026, $2, $3, $4, 10, 'SA', $5, '2026-10-01', '2026-10-01', 'EUR', 'EUR', 'EUR', 1, 1, 'ledger', $1) RETURNING *)
		INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, amount_tc, amount_lc, amount_gc)
		SELECT tenant_id, fiscal_year, id, l.n, legal_entity_id, ledger_id, fiscal_period, posting_date, gen_random_uuid(), l.a, l.a, l.a
		FROM e, (VALUES (1, 12345), (2, -12345)) l(n, a)`, tenant, id, uuid.New(), uuid.New(), id.String()[:8])
	return id
}

func (c cell) verifier(want int) *drverify.Verifier {
	return &drverify.Verifier{Pool: c.pool, AppRole: "erp_app", ControlRole: "erp_control", Want: want}
}

func wantProblems(t *testing.T, r drverify.Report, want ...string) {
	t.Helper()
	if len(r.Problems) != len(want) {
		t.Fatalf("problems: got %q, want %d containing %q", r.Problems, len(want), want)
	}
	for i, w := range want {
		if !strings.Contains(r.Problems[i], w) {
			t.Errorf("problem %d: got %q, want it to contain %q", i, r.Problems[i], w)
		}
	}
}

func TestVerifyRestoredCell(t *testing.T) {
	ctx := context.Background()
	c := newCell(t)
	v := c.verifier(db.LatestVersion())
	t.Run("an empty cell at the expected version passes", func(t *testing.T) {
		r, err := v.Verify(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Passed() || r.SchemaVersion != db.LatestVersion() || r.Tenants != 0 {
			t.Fatalf("got %+v, want a pass at version %d with no tenants", r, db.LatestVersion())
		}
	})
	t.Run("a schema at another version fails", func(t *testing.T) {
		r, err := c.verifier(db.LatestVersion() + 1).Verify(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wantProblems(t, r, "schema version")
	})
	ten := c.tenant(t)
	c.entry(t, ten)
	before, err := v.Mark(ctx, ten, "before")
	if err != nil {
		t.Fatal(err)
	}
	v.Restored, v.Lost = &before, &drverify.Mark{TenantID: ten, Name: "after"}
	t.Run("a restore holding the marker before its point passes", func(t *testing.T) {
		r, err := v.Verify(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wantProblems(t, r)
		if r.Restored == nil || !r.Restored.At.Equal(before.At) {
			t.Errorf("restored marker: got %+v, want written at %s", r.Restored, before.At)
		}
		if r.JournalEntries != 1 || r.Tenants != 1 || len(r.Fingerprints) != 1 {
			t.Errorf("got %d entries, %d tenants, %d fingerprints, want 1 each", r.JournalEntries, r.Tenants, len(r.Fingerprints))
		}
	})
	t.Run("the old database agrees with itself and not with an empty one", func(t *testing.T) {
		old := *v
		old.Old = c.pool
		r, err := old.Verify(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wantProblems(t, r)
		old.Old = newCell(t).pool
		if r, err = old.Verify(ctx); err != nil {
			t.Fatal(err)
		}
		wantProblems(t, r, "the old database has no tenant")
	})
	t.Run("a restore past its point fails", func(t *testing.T) {
		if _, err := v.Mark(ctx, ten, "after"); err != nil {
			t.Fatal(err)
		}
		r, err := v.Verify(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wantProblems(t, r, "audit.audit_event: 2 rows in the restored database, 1 in marker before",
			"audit chain head: seq 2", "marker \"after\", written after the restore point, is present")
	})
	t.Run("a restore without the marker before its point fails", func(t *testing.T) {
		missing := *v
		missing.Restored, missing.Lost = &drverify.Mark{TenantID: ten, Name: "never"}, nil
		r, err := missing.Verify(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wantProblems(t, r, "marker \"never\", written before the restore point, is missing")
	})
}

func TestVerifyFindsTornData(t *testing.T) {
	ctx := context.Background()
	c := newCell(t)
	ten := c.tenant(t)
	entry := c.entry(t, ten)
	v := c.verifier(db.LatestVersion())
	if _, err := v.Mark(ctx, ten, "chain"); err != nil {
		t.Fatal(err)
	}
	c.exec(t, true, `DELETE FROM ledger.journal_line WHERE journal_entry_id = $1 AND line_no = 2`, entry)
	c.exec(t, true, `UPDATE audit.audit_event SET detail = '{"name": "forged"}' WHERE tenant_id = $1`, ten)
	r, err := v.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantProblems(t, r, "audit chain broken at seq 1", "journal entry "+entry.String()+" (2026) unbalanced: tc 12345, lc 12345, gc 12345",
		"trial balance lc 12345, gc 12345, want 0")
}

func TestMarkNeedsAName(t *testing.T) {
	_, err := (&drverify.Verifier{}).Mark(context.Background(), uuid.New(), "")
	pgtest.WantErr(t, err, "a marker needs a name")
}

// TestDatabaseFailures fails each statement of a verification with an old
// database and a drill's markers, and of a mark, in turn: each failure is
// reported, never a pass.
func TestDatabaseFailures(t *testing.T) {
	ctx := context.Background()
	c := newCell(t)
	ten := c.tenant(t)
	c.entry(t, ten)
	v := c.verifier(db.LatestVersion())
	before, err := v.Mark(ctx, ten, "before")
	if err != nil {
		t.Fatal(err)
	}
	failing := func(p *database.Pool, n int, count *int) database.BeginFunc {
		return func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := p.Begin(ctx, s)
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: count}, err
		}
	}
	for n := 1; ; n++ {
		count := 0
		f := *v
		f.Old, f.Restored, f.Lost = c.pool, &before, &drverify.Mark{TenantID: ten, Name: "after"}
		f.Begin, f.OldBegin = failing(c.pool, n, &count), failing(c.pool, n, &count)
		_, err := f.Verify(ctx)
		if count < n {
			if err != nil {
				t.Fatalf("all %d statements ran: %v", count, err)
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: got %v, want the injected failure", n, err)
		}
	}
	for n := 1; ; n++ {
		count := 0
		f := *v
		f.Begin = failing(c.pool, n, &count)
		_, err := f.Mark(ctx, ten, "mark")
		if count < n {
			if err != nil {
				t.Fatalf("all %d statements ran: %v", count, err)
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("mark statement %d: got %v, want the injected failure", n, err)
		}
	}
	t.Run("commit", func(t *testing.T) {
		f := *v
		f.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := c.pool.Begin(ctx, s)
			return &pgtest.FailingTx{Tx: tx, FailCommit: true}, err
		}
		_, err := f.Mark(ctx, ten, "uncommitted")
		pgtest.WantErr(t, err, "commit")
	})
	t.Run("begin", func(t *testing.T) {
		refused := func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected }
		for name, f := range map[string]drverify.Verifier{
			"restored": {Begin: refused, Want: 1},
			"old":      {Pool: c.pool, OldBegin: refused, Old: c.pool, AppRole: "erp_app", ControlRole: "erp_control"},
		} {
			if _, err := f.Verify(ctx); !errors.Is(err, pgtest.ErrInjected) {
				t.Errorf("%s: got %v, want the refused begin", name, err)
			}
		}
		if _, err := (&drverify.Verifier{Begin: refused}).Mark(ctx, ten, "x"); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("mark: got %v, want the refused begin", err)
		}
	})
}
