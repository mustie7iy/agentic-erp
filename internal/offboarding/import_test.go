package offboarding_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// exportThenErase offboards, exports and erases a fresh tenant and returns
// its id and the manifest key.
func exportThenErase(t *testing.T, pool *database.Pool, store blobstore.Dir) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	e := fixture.New(t, "payables.invoice.read")
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', version = version + 1 WHERE id = $1`, e.Tenant.ID)
	clock := time.Now().UTC()
	w := worker(t, pool, store, &clock)
	w.ChunkRows = 2
	ex, err := w.Export(ctx, e.Tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Erase(ctx, e.Tenant.ID, offboarding.Actor, nil); err != nil {
		t.Fatal(err)
	}
	return e.Tenant.ID, ex.Ref
}

// TestReonboarding: an erased tenant comes back from its export, row for
// row, chain head and all; the import refuses what does not match.
func TestReonboarding(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t)
	store := blobstore.Dir{Root: t.TempDir()}
	tenant, ref := exportThenErase(t, pool, store)
	var manifest struct {
		Tables []struct {
			Name string
			Rows int64
		}
		ChainHead struct {
			Seq  int64
			Hash string
		} `json:"audit_chain_head"`
	}
	body, _, _ := store.Get(ctx, ref)
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	want := map[string]int64{}
	outboxes := 0
	for _, tb := range manifest.Tables {
		if strings.HasSuffix(tb.Name, ".outbox") { // delivered where they happened, never imported
			outboxes++
			continue
		}
		want[tb.Name] += tb.Rows
	}
	if outboxes == 0 {
		t.Fatal("the export holds no outbox rows to skip")
	}
	if len(want) < 5 {
		t.Fatalf("exported tables: got %d, want at least 5: %v", len(want), want)
	}
	for _, table := range []string{"identity.principal", "audit.audit_event"} {
		if want[table] == 0 {
			t.Fatalf("exported %s rows: got 0, want some: %v", table, want)
		}
	}
	// Gone: principals erased, the tenant a tombstone, the audit trail kept (with the export and erasure entries).
	if n := count(t, `SELECT count(*) FROM identity.principal WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("%d principals before the import", n)
	}
	im := &offboarding.Importer{Pool: pool, Store: store, AppRole: "erp_app"}
	// The audit trail of this cell kept its rows: the import refuses to double them. Erase the trail here as the
	// move of a tenant between cells would find it in the destination: absent.
	wipeTrail(t, tenant)
	res, err := im.Import(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if res.TenantID != tenant {
		t.Errorf("imported tenant: got %v, want %v", res.TenantID, tenant)
	}
	if res.Tables != len(want) {
		t.Errorf("imported tables: got %d, want %d", res.Tables, len(want))
	}
	if res.ChainHead == nil {
		t.Fatalf("imported chain head: got nil, want seq %d", manifest.ChainHead.Seq)
	}
	if res.ChainHead.Seq != manifest.ChainHead.Seq {
		t.Errorf("imported chain head seq: got %d, want %d", res.ChainHead.Seq, manifest.ChainHead.Seq)
	}
	for name, n := range want {
		if res.Rows[name] != n {
			t.Errorf("%s imported: got %d rows, want the %d exported", name, res.Rows[name], n)
		}
		if got := count(t, `SELECT count(*) FROM `+name+` WHERE tenant_id = $1`, tenant); got != n {
			t.Errorf("%s in the cell: got %d rows, want the %d exported", name, got, n)
		}
	}
	var status string
	var erased *time.Time
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT status, erased_at FROM tenancy.tenant WHERE id = $1`, tenant).Scan(&status, &erased); err != nil {
		t.Fatalf("read the tenant: %v", err)
	}
	if status != "active" || erased != nil {
		t.Fatalf("tenant after the import: %s %v", status, erased)
	}
	var hash []byte
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT hash FROM audit.chain_head WHERE tenant_id = $1`, tenant).Scan(&hash); err != nil {
		t.Fatalf("read the chain head: %v", err)
	}
	if hex.EncodeToString(hash) != manifest.ChainHead.Hash {
		t.Fatal("chain head differs after the import")
	}
	// A live tenant is refused; so is one whose rows are present.
	_, err = im.Import(ctx, ref)
	pgtest.WantErr(t, err, "not erased")
	pgtest.Exec(t, `UPDATE tenancy.tenant SET exported_at = now(), export_ref = 'x', erase_after = now(), erased_at = now(), version = version + 1 WHERE id = $1`, tenant)
	_, err = im.Import(ctx, ref)
	pgtest.WantErr(t, err, "already holds")
	// Into a cell that never had the tenant: the tenant row is created.
	pgtest.Exec(t, `SELECT platform.erase_tenant('`+tenant.String()+`')`)
	wipeTrail(t, tenant)
	pgtest.Exec(t, `DELETE FROM tenancy.tenant WHERE id = $1`, tenant)
	if res, err := im.Import(ctx, ref); err != nil || res.Rows["identity.principal"] != want["identity.principal"] {
		t.Fatalf("fresh cell: %+v %v", res, err)
	}
}

// TestImportRefusals: manifests and parts that do not add up, stores and
// databases that fail.
func TestImportRefusals(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t)
	store := blobstore.Dir{Root: t.TempDir()}
	im := &offboarding.Importer{Pool: pool, Store: store, AppRole: "erp_app"}
	_, err := (&offboarding.Importer{Pool: pool, AppRole: "erp_app"}).Import(ctx, "x")
	pgtest.WantErr(t, err, "no object store")
	_, err = im.Import(ctx, "tenants/none/exports/1/manifest.json")
	pgtest.WantErr(t, err, "reading the manifest")
	if err := store.Put(ctx, "tenants/x/exports/1/manifest.json", []byte(`{"not":"a manifest"}`), blobstore.PutOptions{}); err != nil {
		t.Fatalf("store a garbage manifest: %v", err)
	}
	_, err = im.Import(ctx, "tenants/x/exports/1/manifest.json")
	pgtest.WantErr(t, err, "not an export manifest")
	// Hand-made exports of a tenant that does not exist here.
	tenant := uuid.Must(uuid.NewV7())
	put := func(key string, content string) string {
		if err := store.Put(ctx, key, []byte(content), blobstore.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(content))
		return hex.EncodeToString(sum[:])
	}
	tenantRow := `{"id":"` + tenant.String() + `","name":"Imported","region":"eu-west","group_currency":"EUR","status":"active"}`
	roleLine := `{"tenant_id":"` + tenant.String() + `","id":"` + uuid.NewString() + `","code":"NOBODY","name":"Nobody","description":null,"status":"active","created_by":"` + uuid.NewString() + `","created_at":"2026-10-12T00:00:00Z","updated_at":"2026-10-12T00:00:00Z","updated_by":"` + uuid.NewString() + `","version":1}` + "\n"
	manifest := func(dir string, tables string) string {
		key := dir + "/manifest.json"
		put(key, `{"tenant_id":"`+tenant.String()+`","tenant":`+tenantRow+`,"exported_at":"2026-10-12T00:00:00Z","tables":[`+tables+`]}`)
		return key
	}
	// A part whose hash does not match.
	d := "tenants/" + tenant.String() + "/exports/hash"
	put(d+"/identity.role.00001.jsonl", roleLine)
	_, err = im.Import(ctx, manifest(d, `{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"00"}`))
	pgtest.WantErr(t, err, "hash")
	// A part with more rows than the manifest says, and a part that is missing.
	d = "tenants/" + tenant.String() + "/exports/rows"
	sum := put(d+"/identity.role.00001.jsonl", roleLine)
	_, err = im.Import(ctx, manifest(d, `{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":2,"bytes":1,"sha256":"`+sum+`"}`))
	pgtest.WantErr(t, err, "manifest says 2")
	d = "tenants/" + tenant.String() + "/exports/missing"
	_, err = im.Import(ctx, manifest(d, `{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sum+`"}`))
	pgtest.WantErr(t, err, "reading")
	// Rows that reference rows the export does not hold stay referenced; an outbox is skipped; a chain head the manifest
	// promises but the parts do not deliver is refused.
	d = "tenants/" + tenant.String() + "/exports/fk"
	assignment := `{"tenant_id":"` + tenant.String() + `","id":"` + uuid.NewString() + `","principal_id":"` + uuid.NewString() + `","role_id":"` + uuid.NewString() + `","valid_from":"2026-01-01","valid_to":null,"created_by":"` + uuid.NewString() + `","created_at":"2026-10-12T00:00:00Z","ended_by":null,"ended_at":null}` + "\n"
	sumA := put(d+"/identity.principal_role.00001.jsonl", assignment)
	_, err = im.Import(ctx, manifest(d, `{"name":"identity.principal_role","part":1,"key":"`+d+`/identity.principal_role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumA+`"}`))
	pgtest.WantErr(t, err, "stay referenced")
	d = "tenants/" + tenant.String() + "/exports/outbox"
	sumR := put(d+"/identity.role.00001.jsonl", roleLine)
	put(d+"/tenancy.outbox.00001.jsonl", `{"anything":true}`+"\n")
	res, err := im.Import(ctx, manifest(d, `{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumR+`"},{"name":"tenancy.outbox","part":1,"key":"`+d+`/tenancy.outbox.00001.jsonl","rows":1,"bytes":1,"sha256":"x"}`))
	if err != nil {
		t.Fatalf("outbox skipped: %v", err)
	}
	if res.Tables != 1 {
		t.Errorf("outbox skipped: got %d tables, want 1", res.Tables)
	}
	if res.Rows["identity.role"] != 1 {
		t.Errorf("outbox skipped: got %d roles, want 1", res.Rows["identity.role"])
	}
	if res.ChainHead != nil {
		t.Errorf("outbox skipped: got chain head %+v, want nil", res.ChainHead)
	}
	if n := count(t, `SELECT count(*) FROM identity.role WHERE tenant_id = $1`, tenant); n != 1 {
		t.Fatalf("%d roles imported", n)
	}
	// The tenant exists now and is live: refused; erased (by the function), a chain head that is not delivered is refused too.
	pgtest.Exec(t, `SELECT platform.erase_tenant('`+tenant.String()+`')`)
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', exported_at = now(), export_ref = 'x', erase_after = now(), erased_at = now(), version = version + 1 WHERE id = $1`, tenant)
	d = "tenants/" + tenant.String() + "/exports/head"
	sumH := put(d+"/identity.role.00001.jsonl", roleLine)
	key := d + "/manifest.json"
	put(key, `{"tenant_id":"`+tenant.String()+`","tenant":`+tenantRow+`,"exported_at":"2026-10-12T00:00:00Z","audit_chain_head":{"seq":3,"hash":"ab"},"tables":[{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumH+`"}]}`)
	_, err = im.Import(ctx, key)
	pgtest.WantErr(t, err, "chain head was not imported")
	// A delivered chain head that differs from the manifest's.
	d = "tenants/" + tenant.String() + "/exports/head2"
	head := `{"tenant_id":"` + tenant.String() + `","seq":1,"hash":"\\x` + strings.Repeat("00", 32) + `","updated_at":"2026-10-12T00:00:00Z"}` + "\n"
	sumHead := put(d+"/audit.chain_head.00001.jsonl", head)
	key = d + "/manifest.json"
	put(key, `{"tenant_id":"`+tenant.String()+`","tenant":`+tenantRow+`,"exported_at":"2026-10-12T00:00:00Z","audit_chain_head":{"seq":1,"hash":"ab"},"tables":[{"name":"audit.chain_head","part":1,"key":"`+d+`/audit.chain_head.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumHead+`"}]}`)
	_, err = im.Import(ctx, key)
	pgtest.WantErr(t, err, "differs from the export")
	// A row the table refuses for another reason than a foreign key is reported as such.
	d = "tenants/" + tenant.String() + "/exports/bad"
	sumB := put(d+"/identity.role.00001.jsonl", `{"tenant_id":"`+tenant.String()+`","id":"`+uuid.NewString()+`","name":"No code"}`+"\n")
	_, err = im.Import(ctx, manifest(d, `{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumB+`"}`))
	pgtest.WantErr(t, err, "inserting into identity.role")
	// A table the cell does not have.
	d = "tenants/" + tenant.String() + "/exports/table"
	sumT := put(d+"/nowhere.thing.00001.jsonl", `{"tenant_id":"`+tenant.String()+`"}`+"\n")
	_, err = im.Import(ctx, manifest(d, `{"name":"nowhere.thing","part":1,"key":"`+d+`/nowhere.thing.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumT+`"}`))
	pgtest.WantErr(t, err, "nowhere.thing")
	// Savepoints that cannot start, commit or roll back.
	dOK := "tenants/" + tenant.String() + "/exports/sp"
	sumS := put(dOK+"/identity.role.00001.jsonl", roleLine)
	keyOK := manifest(dOK, `{"name":"identity.role","part":1,"key":"`+dOK+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumS+`"}`)
	dFK := "tenants/" + tenant.String() + "/exports/spfk"
	sumF := put(dFK+"/identity.principal_role.00001.jsonl", assignment)
	keyFK := manifest(dFK, `{"name":"identity.principal_role","part":1,"key":"`+dFK+`/identity.principal_role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumF+`"}`)
	for mode, key := range map[string]string{"begin": keyOK, "commit": keyOK, "rollback": keyFK} {
		t.Run("savepoint "+mode+" failing", func(t *testing.T) {
			im.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
				return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, savepointFails{Tx: tx, mode: mode}) })
			}
			_, err := im.Import(ctx, key)
			pgtest.CheckErr(t, err, "savepoint "+mode)
		})
	}
	im.InTx = nil
	// A manifest whose tenant row is unreadable.
	d = "tenants/" + tenant.String() + "/exports/row"
	key = d + "/manifest.json"
	put(key, `{"tenant_id":"`+tenant.String()+`","tenant":{"name":""},"exported_at":"2026-10-12T00:00:00Z","tables":[]}`)
	_, err = im.Import(ctx, key)
	pgtest.WantErr(t, err, "tenant row is not readable")
	// Database statements failing in turn, on a valid import into the erased tenant.
	d = "tenants/" + tenant.String() + "/exports/db"
	sumD := put(d+"/identity.role.00001.jsonl", roleLine)
	key = manifest(d, `{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumD+`"}`)
	for n := 1; n < 30; n++ {
		im.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
			return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}) })
		}
		_, err := im.Import(ctx, key)
		if err == nil {
			if n == 1 {
				t.Fatal("imported without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
	// Creating a tenant this cell has never held failing: statements fail in
	// turn on fresh tenants until the creation is the one that fails.
	created := false
	for n := 1; n < 10 && !created; n++ {
		fresh := uuid.Must(uuid.NewV7()).String()
		key := "tenants/" + fresh + "/exports/fresh/manifest.json"
		put(key, `{"tenant_id":"`+fresh+`","tenant":{"id":"`+fresh+`","name":"Fresh","region":"eu-west","group_currency":"EUR","status":"active"},"exported_at":"2026-10-12T00:00:00Z","tables":[]}`)
		im.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
			return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}) })
		}
		_, err := im.Import(ctx, key)
		created = errors.Is(err, pgtest.ErrInjected) && strings.Contains(err.Error(), "offboarding: create tenant "+fresh)
	}
	if !created {
		t.Error("no injected failure reached the creation of a new tenant")
	}
	im.InTx = nil
}

// wipeTrail removes a tenant's audit trail the way a destination cell
// would lack it: the erasure keeps it, an import brings it back.
func wipeTrail(t *testing.T, tenant uuid.UUID) {
	t.Helper()
	id := tenant.String()
	pgtest.Exec(t, `SELECT set_config('app.erasing', '`+id+`', true); DELETE FROM audit.verification WHERE tenant_id = '`+id+`'; DELETE FROM audit.anchor WHERE tenant_id = '`+id+`';
		DELETE FROM audit.audit_event WHERE tenant_id = '`+id+`'; DELETE FROM audit.chain_head WHERE tenant_id = '`+id+`'`)
}

// savepointFails is a transaction whose savepoints fail to begin, commit or
// roll back.
type savepointFails struct {
	pgx.Tx
	mode string
}

func (f savepointFails) Begin(ctx context.Context) (pgx.Tx, error) {
	if f.mode == "begin" {
		return nil, errors.New("savepoint begin failed")
	}
	sp, err := f.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return brokenSavepoint{Tx: sp, mode: f.mode}, nil
}

type brokenSavepoint struct {
	pgx.Tx
	mode string
}

func (b brokenSavepoint) Commit(ctx context.Context) error {
	if b.mode == "commit" {
		b.Tx.Rollback(ctx)
		return errors.New("savepoint commit failed")
	}
	return b.Tx.Commit(ctx)
}

func (b brokenSavepoint) Rollback(ctx context.Context) error {
	if b.mode == "rollback" {
		b.Tx.Rollback(ctx)
		return errors.New("savepoint rollback failed")
	}
	return b.Tx.Rollback(ctx)
}

// TestImportRefusesUnexportedTables: the manifest is not hash-checked, so
// a table name in it is untrusted. A name the cell does not export, or one
// carrying SQL, is refused before any statement uses it: a sentinel
// sequence the payload would advance (nextval survives the rollback) is
// left as it was.
func TestImportRefusesUnexportedTables(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t)
	store := blobstore.Dir{Root: t.TempDir()}
	im := &offboarding.Importer{Pool: pool, Store: store, AppRole: "erp_app"}
	sentinel := "public.b1_sentinel_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	pgtest.Exec(t, `CREATE SEQUENCE `+sentinel+`; GRANT USAGE, SELECT, UPDATE ON SEQUENCE `+sentinel+` TO erp_app`)
	t.Cleanup(func() { pgtest.Exec(t, `DROP SEQUENCE IF EXISTS `+sentinel) })
	tenant := uuid.Must(uuid.NewV7())
	tenantRow := `{"id":"` + tenant.String() + `","name":"Imported","region":"eu-west","group_currency":"EUR","status":"active"}`
	cases := map[string]string{
		"unknown table": sentinel,
		"injection":     `(SELECT nextval('` + sentinel + `') AS n, $1::uuid AS tenant_id) s`,
	}
	for name, table := range cases {
		t.Run(name, func(t *testing.T) {
			d := "tenants/" + tenant.String() + "/exports/" + strings.ReplaceAll(name, " ", "-")
			tables, err := json.Marshal([]map[string]any{{"name": table, "part": 1, "key": d + "/part.jsonl", "rows": 1, "bytes": 1, "sha256": "00"}})
			if err != nil {
				t.Fatal(err)
			}
			key := d + "/manifest.json"
			body := `{"tenant_id":"` + tenant.String() + `","tenant":` + tenantRow + `,"exported_at":"2026-10-12T00:00:00Z","tables":` + string(tables) + `}`
			if err := store.Put(ctx, key, []byte(body), blobstore.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			_, err = im.Import(ctx, key)
			pgtest.CheckErr(t, err, "not a table this cell exports")
			if called := count(t, `SELECT count(*) FROM `+sentinel+` WHERE is_called`); called != 0 {
				t.Error("got the sentinel sequence advanced, want the payload never run")
			}
			if n := count(t, `SELECT count(*) FROM tenancy.tenant WHERE id = $1`, tenant); n != 0 {
				t.Errorf("got %d tenant rows, want the import rolled back", n)
			}
		})
	}
}
