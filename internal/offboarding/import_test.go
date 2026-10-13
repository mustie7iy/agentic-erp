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
	json.Unmarshal(body, &manifest)
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
	if len(want) < 5 || want["identity.principal"] == 0 || want["audit.audit_event"] == 0 {
		t.Fatalf("export: %v", want)
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
	if res.TenantID != tenant || res.Tables != len(want) || res.ChainHead == nil || res.ChainHead.Seq != manifest.ChainHead.Seq {
		t.Fatalf("%+v", res)
	}
	for name, n := range want {
		if res.Rows[name] != n {
			t.Errorf("%s: %d imported, %d exported", name, res.Rows[name], n)
		}
		if got := count(t, `SELECT count(*) FROM `+name+` WHERE tenant_id = $1`, tenant); got != n {
			t.Errorf("%s: %d rows in the cell, %d exported", name, got, n)
		}
	}
	var status string
	var erased *time.Time
	pgtest.Admin(t).QueryRow(ctx, `SELECT status, erased_at FROM tenancy.tenant WHERE id = $1`, tenant).Scan(&status, &erased)
	if status != "active" || erased != nil {
		t.Fatalf("tenant after the import: %s %v", status, erased)
	}
	var hash []byte
	pgtest.Admin(t).QueryRow(ctx, `SELECT hash FROM audit.chain_head WHERE tenant_id = $1`, tenant).Scan(&hash)
	if hex.EncodeToString(hash) != manifest.ChainHead.Hash {
		t.Fatal("chain head differs after the import")
	}
	// A live tenant is refused; so is one whose rows are present.
	if _, err := im.Import(ctx, ref); err == nil || !strings.Contains(err.Error(), "not erased") {
		t.Fatalf("import over a live tenant: %v", err)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET exported_at = now(), export_ref = 'x', erase_after = now(), erased_at = now(), version = version + 1 WHERE id = $1`, tenant)
	if _, err := im.Import(ctx, ref); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("import over present rows: %v", err)
	}
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
	if _, err := (&offboarding.Importer{Pool: pool, AppRole: "erp_app"}).Import(ctx, "x"); err == nil || !strings.Contains(err.Error(), "no object store") {
		t.Fatalf("without a store: %v", err)
	}
	if _, err := im.Import(ctx, "tenants/none/exports/1/manifest.json"); err == nil || !strings.Contains(err.Error(), "reading the manifest") {
		t.Fatalf("missing manifest: %v", err)
	}
	store.Put(ctx, "tenants/x/exports/1/manifest.json", []byte(`{"not":"a manifest"}`), blobstore.PutOptions{})
	if _, err := im.Import(ctx, "tenants/x/exports/1/manifest.json"); err == nil || !strings.Contains(err.Error(), "not an export manifest") {
		t.Fatalf("garbage manifest: %v", err)
	}
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
	if _, err := im.Import(ctx, manifest(d, `{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"00"}`)); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("hash mismatch: %v", err)
	}
	// A part with more rows than the manifest says, and a part that is missing.
	d = "tenants/" + tenant.String() + "/exports/rows"
	sum := put(d+"/identity.role.00001.jsonl", roleLine)
	if _, err := im.Import(ctx, manifest(d, `{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":2,"bytes":1,"sha256":"`+sum+`"}`)); err == nil || !strings.Contains(err.Error(), "manifest says 2") {
		t.Fatalf("row count: %v", err)
	}
	d = "tenants/" + tenant.String() + "/exports/missing"
	if _, err := im.Import(ctx, manifest(d, `{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sum+`"}`)); err == nil || !strings.Contains(err.Error(), "reading") {
		t.Fatalf("missing part: %v", err)
	}
	// Rows that reference rows the export does not hold stay referenced; an outbox is skipped; a chain head the manifest
	// promises but the parts do not deliver is refused.
	d = "tenants/" + tenant.String() + "/exports/fk"
	assignment := `{"tenant_id":"` + tenant.String() + `","id":"` + uuid.NewString() + `","principal_id":"` + uuid.NewString() + `","role_id":"` + uuid.NewString() + `","valid_from":"2026-01-01","valid_to":null,"created_by":"` + uuid.NewString() + `","created_at":"2026-10-12T00:00:00Z","ended_by":null,"ended_at":null}` + "\n"
	sumA := put(d+"/identity.principal_role.00001.jsonl", assignment)
	if _, err := im.Import(ctx, manifest(d, `{"name":"identity.principal_role","part":1,"key":"`+d+`/identity.principal_role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumA+`"}`)); err == nil || !strings.Contains(err.Error(), "stay referenced") {
		t.Fatalf("foreign keys: %v", err)
	}
	d = "tenants/" + tenant.String() + "/exports/outbox"
	sumR := put(d+"/identity.role.00001.jsonl", roleLine)
	put(d+"/tenancy.outbox.00001.jsonl", `{"anything":true}`+"\n")
	res, err := im.Import(ctx, manifest(d, `{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumR+`"},{"name":"tenancy.outbox","part":1,"key":"`+d+`/tenancy.outbox.00001.jsonl","rows":1,"bytes":1,"sha256":"x"}`))
	if err != nil || res.Tables != 1 || res.Rows["identity.role"] != 1 || res.ChainHead != nil {
		t.Fatalf("outbox skipped: %+v %v", res, err)
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
	if _, err := im.Import(ctx, key); err == nil || !strings.Contains(err.Error(), "chain head was not imported") {
		t.Fatalf("missing chain head: %v", err)
	}
	// A delivered chain head that differs from the manifest's.
	d = "tenants/" + tenant.String() + "/exports/head2"
	head := `{"tenant_id":"` + tenant.String() + `","seq":1,"hash":"\\x` + strings.Repeat("00", 32) + `","updated_at":"2026-10-12T00:00:00Z"}` + "\n"
	sumHead := put(d+"/audit.chain_head.00001.jsonl", head)
	key = d + "/manifest.json"
	put(key, `{"tenant_id":"`+tenant.String()+`","tenant":`+tenantRow+`,"exported_at":"2026-10-12T00:00:00Z","audit_chain_head":{"seq":1,"hash":"ab"},"tables":[{"name":"audit.chain_head","part":1,"key":"`+d+`/audit.chain_head.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumHead+`"}]}`)
	if _, err := im.Import(ctx, key); err == nil || !strings.Contains(err.Error(), "differs from the export") {
		t.Fatalf("chain head mismatch: %v", err)
	}
	// A row the table refuses for another reason than a foreign key is reported as such.
	d = "tenants/" + tenant.String() + "/exports/bad"
	sumB := put(d+"/identity.role.00001.jsonl", `{"tenant_id":"`+tenant.String()+`","id":"`+uuid.NewString()+`","name":"No code"}`+"\n")
	if _, err := im.Import(ctx, manifest(d, `{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumB+`"}`)); err == nil || !strings.Contains(err.Error(), "inserting into identity.role") {
		t.Fatalf("bad row: %v", err)
	}
	// A table the cell does not have.
	d = "tenants/" + tenant.String() + "/exports/table"
	sumT := put(d+"/nowhere.thing.00001.jsonl", `{"tenant_id":"`+tenant.String()+`"}`+"\n")
	if _, err := im.Import(ctx, manifest(d, `{"name":"nowhere.thing","part":1,"key":"`+d+`/nowhere.thing.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumT+`"}`)); err == nil || !strings.Contains(err.Error(), "nowhere.thing") {
		t.Fatalf("unknown table: %v", err)
	}
	// Savepoints that cannot start, commit or roll back.
	dOK := "tenants/" + tenant.String() + "/exports/sp"
	sumS := put(dOK+"/identity.role.00001.jsonl", roleLine)
	keyOK := manifest(dOK, `{"name":"identity.role","part":1,"key":"`+dOK+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumS+`"}`)
	dFK := "tenants/" + tenant.String() + "/exports/spfk"
	sumF := put(dFK+"/identity.principal_role.00001.jsonl", assignment)
	keyFK := manifest(dFK, `{"name":"identity.principal_role","part":1,"key":"`+dFK+`/identity.principal_role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumF+`"}`)
	for mode, key := range map[string]string{"begin": keyOK, "commit": keyOK, "rollback": keyFK} {
		im.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
			return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, savepointFails{Tx: tx, mode: mode}) })
		}
		if _, err := im.Import(ctx, key); err == nil || !strings.Contains(err.Error(), "savepoint "+mode) {
			t.Errorf("savepoint %s failing: %v", mode, err)
		}
	}
	im.InTx = nil
	// A manifest whose tenant row is unreadable.
	d = "tenants/" + tenant.String() + "/exports/row"
	key = d + "/manifest.json"
	put(key, `{"tenant_id":"`+tenant.String()+`","tenant":{"name":""},"exported_at":"2026-10-12T00:00:00Z","tables":[]}`)
	if _, err := im.Import(ctx, key); err == nil || !strings.Contains(err.Error(), "tenant row is not readable") {
		t.Fatalf("tenant row: %v", err)
	}
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
