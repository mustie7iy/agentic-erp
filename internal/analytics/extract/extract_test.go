package extract_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/parquet-go/parquet-go"

	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// row is what a reader sees of a gl_lines file.
type row struct {
	TenantID     uuid.UUID `parquet:"tenant_id,uuid"`
	DocumentID   uuid.UUID `parquet:"document_id,uuid"`
	LineNo       int32     `parquet:"line_no"`
	Company      string    `parquet:"company"`
	Account      string    `parquet:"account"`
	DocumentType string    `parquet:"document_type"`
	Vendor       *string   `parquet:"vendor,optional"`
	AmountMinor  int64     `parquet:"amount_minor"`
	Currency     string    `parquet:"currency"`
	Exponent     int32     `parquet:"exponent"`
	PostedXID    uint64    `parquet:"posted_xid"`
}

// env is a configured tenant whose journal the tests post to.
type env struct {
	fixture.Env
	server *blobstoretest.Server
	x      extract.Extractor
}

func setup(t *testing.T) env {
	t.Helper()
	return setupWith(t, nil)
}

// setupWith is setup with the tenant's configuration document changed by
// extend before it is bootstrapped. The extractor reads the cash actuals
// through the cash context, as erp-server wires it.
func setupWith(t *testing.T, extend func(*config.Config)) env {
	t.Helper()
	pool := pgtest.Pool(t)
	srv := blobstoretest.New(t)
	return env{Env: fixture.NewWith(t, nil, extend, "ledger.entry.post"), server: srv,
		x: extract.Extractor{Replica: pool.Begin, Primary: pool.Begin, Role: "erp_app", WorkerRole: "erp_worker", Store: srv.Store(),
			Cash: app.NewAnalyticsCash(app.Config{Pool: pool, Consistent: pool, AppRole: "erp_app"})}}
}

// post posts an entry of company 2000 on date: the expense debited, the
// supplier credited on its payables account.
func (e env) post(t *testing.T, date, amount string) string {
	t.Helper()
	id := e.postOnly(t, date, amount)
	e.settled(t)
	return id
}

// postOnly posts the entry without waiting for the snapshot horizon.
func (e env) postOnly(t *testing.T, date, amount string) string {
	t.Helper()
	je, _ := e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": "EUR",
		"lines": [{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}},
		{"account_id": %q, "amount": {"amount": "-%s", "currency": "EUR"}, "partner_id": %q}]}`,
		e.DE, date, date, e.Account(t, "2000", "610000"), amount, e.Account(t, "2000", "211000"), amount, e.Supplier), "")
	id, _ := je["id"].(string)
	return id
}

// settled waits until every transaction up to the tenant's last posting has
// finished, so a run's cutoff (its snapshot's xmin) lies above it: another
// test's open transaction holds xmin back, which the watermark rightly
// waits for.
func (e env) settled(t *testing.T) {
	t.Helper()
	pgtest.Eventually(t, 5*time.Minute, func() bool { // the horizon waits for every open transaction of the cluster, long ones of parallel packages too
		var done bool
		pgtest.Scan(t, `SELECT coalesce(max(posted_xid) < pg_snapshot_xmin(pg_current_snapshot()), true) FROM ledger.journal_entry WHERE tenant_id = $1`,
			[]any{e.Tenant.ID}, &done)
		return done
	}, "postings of tenant %s still below the snapshot horizon", e.Tenant.ID)
}

func (e env) run(t *testing.T) extract.Result {
	t.Helper()
	res, err := e.x.Run(context.Background(), e.Tenant.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

// checkedAt is the time the pointer records the tenant's store current.
func (e env) checkedAt(t *testing.T) time.Time {
	t.Helper()
	var at *time.Time
	pgtest.Scan(t, `SELECT checked_at FROM analytics.snapshot WHERE tenant_id = $1`, []any{e.Tenant.ID}, &at)
	if at == nil {
		return time.Time{}
	}
	return *at
}

func (e env) manifest(t *testing.T, key string) extract.Manifest {
	t.Helper()
	o, ok := e.server.Object(key)
	if !ok {
		t.Fatalf("manifest %s not stored", key)
	}
	var m extract.Manifest
	if err := json.Unmarshal(o.Content, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (e env) rows(t *testing.T, key string) []row {
	t.Helper()
	o, ok := e.server.Object(key)
	if !ok {
		t.Fatalf("file %s not stored", key)
	}
	rows, err := parquet.Read[row](bytes.NewReader(o.Content), int64(len(o.Content)))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestRunAppendsByWatermark(t *testing.T) {
	e := setup(t)
	other := setup(t) // another tenant's lines, which no extract of e may read
	other.post(t, "2026-03-05", "7.00")
	march := e.post(t, "2026-03-02", "100.00")

	first := e.run(t)
	if first.Lines != 2 {
		t.Fatalf("lines: got %d, want 2", first.Lines)
	}
	if want := strings.TrimSuffix(extract.ManifestKey(e.Tenant.ID, first.Watermark, ""), ".json") + "-"; !strings.HasPrefix(first.ManifestKey, want) {
		t.Errorf("manifest key: got %s, want under %s", first.ManifestKey, want)
	}
	if !first.Published || first.Pending {
		t.Errorf("first run: got %+v, want published with nothing pending", first)
	}
	var key, wm string
	pgtest.Scan(t, `SELECT manifest_key, watermark::text FROM analytics.snapshot WHERE tenant_id = $1`, []any{e.Tenant.ID}, &key, &wm)
	if key != first.ManifestKey || wm != fmt.Sprint(first.Watermark) {
		t.Errorf("pointer: got %s at %s, want %s at %d", key, wm, first.ManifestKey, first.Watermark)
	}
	ds := e.manifest(t, first.ManifestKey).Datasets["gl_lines"]
	if len(ds.Files) != 1 || ds.Rows != 2 || ds.Watermark != first.Watermark {
		t.Fatalf("manifest: got %+v, want one file of 2 rows at %d", ds, first.Watermark)
	}
	prefix := "analytics/v1/tenant=" + e.Tenant.ID.String() + "/gl_lines/month=2026-03/part-"
	if !strings.HasPrefix(ds.Files[0].Key, prefix) {
		t.Errorf("file key: got %s, want under %s", ds.Files[0].Key, prefix)
	}
	checkMarch(t, e, e.rows(t, ds.Files[0].Key), march, first.Watermark)

	t.Run("run twice", func(t *testing.T) {
		keys := slices.Sorted(slices.Values(e.server.Keys()))
		checked := e.checkedAt(t)
		again := e.run(t)
		if again.ManifestKey != first.ManifestKey || again.Lines != 0 || again.Published {
			t.Errorf("second run: got %+v, want manifest %s unpublished and no lines", again, first.ManifestKey)
		}
		if got := e.checkedAt(t); !got.After(checked) {
			t.Errorf("checked_at: got %v, want after %v", got, checked)
		}
		if got := slices.Sorted(slices.Values(e.server.Keys())); !slices.Equal(got, keys) {
			t.Errorf("second run wrote files: got %v, want %v", got, keys)
		}
	})

	t.Run("next month appended", func(t *testing.T) {
		april := e.post(t, "2026-04-01", "5.00")
		next := e.run(t)
		if next.Lines != 2 || next.Watermark <= first.Watermark {
			t.Fatalf("next run: got %+v, want 2 lines above %d", next, first.Watermark)
		}
		m := e.manifest(t, next.ManifestKey)
		for name, d := range m.Datasets {
			if !d.AsOf.Equal(m.Datasets["gl_lines"].AsOf) || d.Watermark != next.Watermark {
				t.Errorf("dataset %s: got as of %v at %d, want the run's", name, d.AsOf, d.Watermark)
			}
		}
		ds := m.Datasets["gl_lines"]
		if len(ds.Files) != 2 || ds.Rows != 4 {
			t.Fatalf("manifest: got %+v, want two files of 4 rows", ds)
		}
		rows := e.rows(t, ds.Files[1].Key)
		if len(rows) != 2 || rows[0].DocumentID.String() != april || rows[0].PostedXID < first.Watermark {
			t.Errorf("april file: got %+v, want entry %s's lines only", rows, april)
		}
	})
}

// checkMarch checks the lines of the March entry.
func checkMarch(t *testing.T, e env, rows []row, entry string, cutoff uint64) {
	t.Helper()
	if len(rows) != 2 {
		t.Fatalf("rows: got %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.TenantID != e.Tenant.ID || r.DocumentID.String() != entry || r.Company != "2000" || r.DocumentType != "SA" {
			t.Errorf("row: got %+v, want entry %s of company 2000 in tenant %s", r, entry, e.Tenant.ID)
		}
		if r.Currency != "EUR" || r.Exponent != 2 {
			t.Errorf("money: got %s exponent %d, want EUR exponent 2", r.Currency, r.Exponent)
		}
		if r.PostedXID == 0 || r.PostedXID >= cutoff {
			t.Errorf("posted_xid: got %d, want in (0, %d)", r.PostedXID, cutoff)
		}
	}
	expense, supplier := rows[0], rows[1]
	if expense.Account != "610000" || expense.AmountMinor != 10000 || expense.Vendor != nil {
		t.Errorf("expense line: got %+v, want 610000 debited 10000 without vendor", expense)
	}
	if supplier.Account != "211000" || supplier.AmountMinor != -10000 || supplier.Vendor == nil || *supplier.Vendor != "BP90000001" {
		t.Errorf("supplier line: got %+v, want 211000 credited 10000 for BP90000001", supplier)
	}
}

// TestRunBehindTheWatermark: a cutoff below the stored watermark (the
// database restored or moved under the store) stops the run, which would
// otherwise skip the entries posted with the transaction ids in between.
func TestRunBehindTheWatermark(t *testing.T) {
	e := setup(t)
	e.post(t, "2026-03-02", "1.00")
	ahead := uint64(1) << 62
	key := extract.ManifestKey(e.Tenant.ID, ahead, "")
	pgtest.Exec(t, `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of) VALUES ($1, $2, $3::text::xid8, now())`,
		e.Tenant.ID, key, fmt.Sprint(ahead))
	// A pointer ahead of the database is rebuilt by the scheduler's tests.
	t.Cleanup(func() { pgtest.Exec(t, `DELETE FROM analytics.snapshot WHERE tenant_id = $1`, e.Tenant.ID) })
	if _, err := e.x.Run(context.Background(), e.Tenant.ID); !errors.Is(err, extract.ErrBehind) {
		t.Errorf("run: got %v, want ErrBehind", err)
	}
	if keys := e.server.Keys(); len(keys) != 0 {
		t.Errorf("files: got %v, want none", keys)
	}
}

// TestRunLosesTheRace: a pointer moved between a run's read and its
// publication is left as the other run set it.
func TestRunLosesTheRace(t *testing.T) {
	move := `UPDATE analytics.snapshot SET manifest_key = $2, watermark = '1' WHERE tenant_id = $1`
	for name, c := range map[string]struct {
		move      string
		prior, ok bool // a run before the race; a new entry for the racing run
	}{
		"first publication": {`INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of) VALUES ($1, $2, '1', now())`, false, true},
		"later publication": {move, true, true},
		"check":             {move, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			if c.prior {
				e.post(t, "2026-03-02", "1.00")
				e.run(t)
			}
			if c.ok {
				e.post(t, "2026-03-03", "2.00")
			}
			moved := extract.ManifestKey(e.Tenant.ID, 1, "")
			primary := e.x.Primary
			var once sync.Once
			e.x.Primary = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
				once.Do(func() { pgtest.Exec(t, c.move, e.Tenant.ID, moved) })
				return primary(ctx, s)
			}
			_, err := e.x.Run(context.Background(), e.Tenant.ID)
			if !errors.Is(err, extract.ErrMoved) {
				t.Fatalf("run: got %v, want ErrMoved", err)
			}
			var key string
			pgtest.Scan(t, `SELECT manifest_key FROM analytics.snapshot WHERE tenant_id = $1`, []any{e.Tenant.ID}, &key)
			if key != moved {
				t.Errorf("pointer: got %s, want %s", key, moved)
			}
			// The lost run's manifest is listed for collection.
			var lost int
			pgtest.Scan(t, `SELECT count(*) FROM analytics.superseded WHERE tenant_id = $1 AND manifest_key <> $2`, []any{e.Tenant.ID, moved}, &lost)
			if want := map[bool]int{true: 1, false: 0}[c.ok]; lost != want {
				t.Errorf("superseded lost manifests: got %d, want %d", lost, want)
			}
		})
	}
}

// TestLostRunNotListed: a lost run whose manifest cannot be listed for
// collection reports both.
func TestLostRunNotListed(t *testing.T) {
	e := setup(t)
	e.post(t, "2026-03-02", "1.00")
	pool := pgtest.Pool(t)
	begins := 0
	e.x.Primary = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		begins++
		tx, err := pool.Begin(ctx, s)
		if begins == 1 {
			pgtest.Exec(t, `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of) VALUES ($1, $2, '1', now())`,
				e.Tenant.ID, extract.ManifestKey(e.Tenant.ID, 1, ""))
			return tx, err
		}
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
	}
	_, err := e.x.Run(context.Background(), e.Tenant.ID)
	if !errors.Is(err, extract.ErrMoved) || !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("run: got %v, want ErrMoved and the listing's failure", err)
	}
}

// failingStore fails the named method on the first key holding part.
type failingStore struct {
	blobstore.Store
	method, part string
}

func (f failingStore) fail(method, key string) bool {
	return method == f.method && strings.Contains(key, f.part)
}

func (f failingStore) Put(ctx context.Context, key string, b []byte, opts blobstore.PutOptions) error {
	if f.fail("put", key) {
		return blobstore.ErrLocked
	}
	return f.Store.Put(ctx, key, b, opts)
}

func (f failingStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if f.fail("get", key) {
		return nil, "", blobstore.ErrNotFound
	}
	return f.Store.Get(ctx, key)
}

func TestStoreFailures(t *testing.T) {
	e := setup(t)
	e.post(t, "2026-03-02", "1.00")
	e.run(t)
	e.post(t, "2026-04-02", "2.00")
	e.post(t, "2026-05-02", "3.00")
	for name, f := range map[string]failingStore{
		"previous manifest": {method: "get", part: "/manifest-"},
		"journal part":      {method: "put", part: "/gl_lines/month=2026-04/"}, // flushed when May begins
		"snapshot file":     {method: "put", part: "/open_items/"},
		"manifest":          {method: "put", part: "/manifest-"},
	} {
		t.Run(name, func(t *testing.T) {
			f.Store = e.x.Store
			x := e.x
			x.Store = f
			if _, err := x.Run(context.Background(), e.Tenant.ID); err == nil {
				t.Fatal("run: got nil error, want the store's")
			}
		})
	}
	t.Run("previous manifest unreadable", func(t *testing.T) {
		key := extract.ManifestKey(e.Tenant.ID, 2, "")
		if err := e.x.Store.Put(context.Background(), key, []byte("{"), blobstore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		pgtest.Exec(t, `UPDATE analytics.snapshot SET manifest_key = $2, watermark = '2' WHERE tenant_id = $1`, e.Tenant.ID, key)
		_, err := e.x.Run(context.Background(), e.Tenant.ID)
		pgtest.WantErr(t, err, "previous manifest")
	})
}

// failEach runs the extract failing at each statement of its transactions
// in turn until a run succeeds, and returns that run.
func failEach(t *testing.T, e env) extract.Result {
	t.Helper()
	pool := pgtest.Pool(t)
	for n := 1; ; n++ {
		count := 0
		begin := func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, err
		}
		x := e.x
		x.Replica, x.Primary = begin, begin
		res, err := x.Run(context.Background(), e.Tenant.ID)
		if err == nil {
			if n < 4 {
				t.Fatalf("statement %d: run succeeded, want every statement covered", n)
			}
			return res
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: got %v, want the injected failure", n, err)
		}
	}
}

func TestDatabaseFailures(t *testing.T) {
	e := setup(t)
	e.post(t, "2026-03-02", "1.00")
	if res := failEach(t, e); !res.Published {
		t.Errorf("first run: got %+v, want published", res)
	}
	if res := failEach(t, e); res.Published {
		t.Errorf("run with nothing new: got %+v, want a check only", res)
	}
	e.post(t, "2026-03-03", "1.00")
	if res := failEach(t, e); !res.Published { // over a pointer: the superseded list too
		t.Errorf("second run: got %+v, want published", res)
	}
	e.post(t, "2026-03-04", "1.00")
	pool := pgtest.Pool(t)
	x := e.x
	x.Primary = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, err
	}
	_, err := x.Run(context.Background(), e.Tenant.ID)
	pgtest.WantErr(t, err, "commit")
	// The next run publishes what the failed one could not.
	res := e.run(t)
	var key string
	pgtest.Scan(t, `SELECT manifest_key FROM analytics.snapshot WHERE tenant_id = $1`, []any{e.Tenant.ID}, &key)
	if key != res.ManifestKey || res.Lines != 2 {
		t.Errorf("rerun: got pointer %s and %+v, want the rerun's manifest with the 2 new lines", key, res)
	}
}
