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
	// One period closed and opened again: its opened count of 2 (ADR-0060 §6) is a value the database keeps,
	// which the import restores as exported.
	// The row is read once by its unique order: each company has a period 1 on the same ledger.
	var period uuid.UUID
	pgtest.Scan(t, `SELECT id FROM ledger.period_status WHERE tenant_id = $1 AND period = 1 ORDER BY fiscal_year, ledger_id, legal_entity_id
		LIMIT 1`, []any{e.Tenant.ID}, &period)
	for _, status := range []string{"closed", "open"} {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = $2, version = version + 1 WHERE id = $1`, period, status)
	}
	if got := count(t, `SELECT count(*) FROM ledger.period_status WHERE tenant_id = $1 AND opened_count = 2`, e.Tenant.ID); got != 1 {
		t.Fatalf("periods opened twice before the export: got %d, want 1", got)
	}
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
	if got := count(t, `SELECT count(*) FROM ledger.period_status WHERE tenant_id = $1 AND opened_count = 2`, tenant); got != 1 {
		t.Errorf("periods opened twice after the import: got %d, want the 1 exported", got)
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
	// Hand-made exports of a tenant that does not exist here.
	tenant := uuid.Must(uuid.NewV7())
	put := func(tb testing.TB, key string, content string) string {
		tb.Helper()
		if err := store.Put(ctx, key, []byte(content), blobstore.PutOptions{}); err != nil {
			tb.Fatal(err)
		}
		sum := sha256.Sum256([]byte(content))
		return hex.EncodeToString(sum[:])
	}
	tenantRow := `{"id":"` + tenant.String() + `","name":"Imported","region":"eu-west","group_currency":"EUR","status":"active"}`
	roleLine := `{"tenant_id":"` + tenant.String() + `","id":"` + uuid.NewString() + `","code":"NOBODY","name":"Nobody","description":null,"status":"active","created_by":"` + uuid.NewString() + `","created_at":"2026-10-12T00:00:00Z","updated_at":"2026-10-12T00:00:00Z","updated_by":"` + uuid.NewString() + `","version":1}` + "\n"
	manifest := func(tb testing.TB, dir string, tables string) string {
		tb.Helper()
		key := dir + "/manifest.json"
		put(tb, key, `{"tenant_id":"`+tenant.String()+`","tenant":`+tenantRow+`,"exported_at":"2026-10-12T00:00:00Z","tables":[`+tables+`]}`)
		return key
	}
	sc := &refusalsScenario{ctx: ctx, pool: pool, store: store, im: im, tenant: tenant, put: put, tenantRow: tenantRow, roleLine: roleLine, manifest: manifest}
	if !t.Run("unreadable manifests", sc.unreadableManifests) {
		t.FailNow()
	}
	if !t.Run("parts that do not add up", sc.partsThatDoNotAddUp) {
		t.FailNow()
	}
	if !t.Run("dangling references and an outbox", sc.danglingReferencesAndAnOutbox) {
		t.FailNow()
	}
	if !t.Run("chain head refused", sc.chainHeadRefused) {
		t.FailNow()
	}
	if !t.Run("rows and tables refused", sc.rowsAndTablesRefused) {
		t.FailNow()
	}
	if !t.Run("savepoints failing", sc.savepointsFailing) {
		t.FailNow()
	}
	if !t.Run("unreadable tenant row", sc.unreadableTenantRow) {
		t.FailNow()
	}
	if !t.Run("each statement failing", sc.eachStatementFailing) {
		t.FailNow()
	}
	t.Run("creating a tenant failing", sc.creatingATenantFailing)
}

// refusalsScenario is the state TestImportRefusals's steps share: a store
// of hand-made exports of a tenant that does not exist here, and the
// importer reading it.
type refusalsScenario struct {
	ctx        context.Context
	pool       *database.Pool
	store      blobstore.Dir
	im         *offboarding.Importer
	tenant     uuid.UUID
	put        func(tb testing.TB, key string, content string) string // stores content, returns its hash
	tenantRow  string
	roleLine   string                                                // a role of the tenant, as exported
	manifest   func(tb testing.TB, dir string, tables string) string // stores a manifest, returns its key
	d          string                                                // an export directory
	key        string                                                // its manifest
	assignment string                                                // a role assignment whose role is not exported
}

// unreadableManifests is the step "unreadable manifests" of TestImportRefusals.
func (sc *refusalsScenario) unreadableManifests(t *testing.T) {
	var err error
	_, err = (&offboarding.Importer{Pool: sc.pool, AppRole: "erp_app"}).Import(sc.ctx, "x")
	pgtest.WantErr(t, err, "no object store")
	_, err = sc.im.Import(sc.ctx, "tenants/none/exports/1/manifest.json")
	pgtest.WantErr(t, err, "reading the manifest")
	if err := sc.store.Put(sc.ctx, "tenants/x/exports/1/manifest.json", []byte(`{"not":"a manifest"}`), blobstore.PutOptions{}); err != nil {
		t.Fatalf("store a garbage manifest: %v", err)
	}
	_, err = sc.im.Import(sc.ctx, "tenants/x/exports/1/manifest.json")
	pgtest.WantErr(t, err, "not an export manifest")
}

// partsThatDoNotAddUp is the step "parts that do not add up" of TestImportRefusals.
func (sc *refusalsScenario) partsThatDoNotAddUp(t *testing.T) {
	var err error
	// A part whose hash does not match.
	sc.d = "tenants/" + sc.tenant.String() + "/exports/hash"
	sc.put(t, sc.d+"/identity.role.00001.jsonl", sc.roleLine)
	_, err = sc.im.Import(sc.ctx, sc.manifest(t, sc.d, `{"name":"identity.role","part":1,"key":"`+sc.d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"00"}`))
	pgtest.WantErr(t, err, "hash")
	// A part with more rows than the manifest says, and a part that is missing.
	sc.d = "tenants/" + sc.tenant.String() + "/exports/rows"
	sum := sc.put(t, sc.d+"/identity.role.00001.jsonl", sc.roleLine)
	_, err = sc.im.Import(sc.ctx, sc.manifest(t, sc.d, `{"name":"identity.role","part":1,"key":"`+sc.d+`/identity.role.00001.jsonl","rows":2,"bytes":1,"sha256":"`+sum+`"}`))
	pgtest.WantErr(t, err, "manifest says 2")
	sc.d = "tenants/" + sc.tenant.String() + "/exports/missing"
	_, err = sc.im.Import(sc.ctx, sc.manifest(t, sc.d, `{"name":"identity.role","part":1,"key":"`+sc.d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sum+`"}`))
	pgtest.WantErr(t, err, "reading")
}

// danglingReferencesAndAnOutbox is the step "dangling references and an outbox" of TestImportRefusals.
func (sc *refusalsScenario) danglingReferencesAndAnOutbox(t *testing.T) {
	var err error
	// Rows that reference rows the export does not hold stay referenced; an outbox is skipped; a chain head the manifest
	// promises but the parts do not deliver is refused.
	sc.d = "tenants/" + sc.tenant.String() + "/exports/fk"
	sc.assignment = `{"tenant_id":"` + sc.tenant.String() + `","id":"` + uuid.NewString() + `","principal_id":"` + uuid.NewString() + `","role_id":"` + uuid.NewString() + `","valid_from":"2026-01-01","valid_to":null,"created_by":"` + uuid.NewString() + `","created_at":"2026-10-12T00:00:00Z","ended_by":null,"ended_at":null}` + "\n"
	sumA := sc.put(t, sc.d+"/identity.principal_role.00001.jsonl", sc.assignment)
	_, err = sc.im.Import(sc.ctx, sc.manifest(t, sc.d, `{"name":"identity.principal_role","part":1,"key":"`+sc.d+`/identity.principal_role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumA+`"}`))
	pgtest.WantErr(t, err, "stay referenced")
	sc.d = "tenants/" + sc.tenant.String() + "/exports/outbox"
	sumR := sc.put(t, sc.d+"/identity.role.00001.jsonl", sc.roleLine)
	sc.put(t, sc.d+"/tenancy.outbox.00001.jsonl", `{"anything":true}`+"\n")
	res, err := sc.im.Import(sc.ctx, sc.manifest(t, sc.d, `{"name":"identity.role","part":1,"key":"`+sc.d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumR+`"},{"name":"tenancy.outbox","part":1,"key":"`+sc.d+`/tenancy.outbox.00001.jsonl","rows":1,"bytes":1,"sha256":"x"}`))
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
	if n := count(t, `SELECT count(*) FROM identity.role WHERE tenant_id = $1`, sc.tenant); n != 1 {
		t.Fatalf("%d roles imported", n)
	}
}

// chainHeadRefused is the step "chain head refused" of TestImportRefusals.
func (sc *refusalsScenario) chainHeadRefused(t *testing.T) {
	var err error
	// The tenant exists now and is live: refused; erased (by the function), a chain head that is not delivered is refused too.
	pgtest.Exec(t, `SELECT platform.erase_tenant('`+sc.tenant.String()+`')`)
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', exported_at = now(), export_ref = 'x', erase_after = now(), erased_at = now(), version = version + 1 WHERE id = $1`, sc.tenant)
	sc.d = "tenants/" + sc.tenant.String() + "/exports/head"
	sumH := sc.put(t, sc.d+"/identity.role.00001.jsonl", sc.roleLine)
	sc.key = sc.d + "/manifest.json"
	sc.put(t, sc.key, `{"tenant_id":"`+sc.tenant.String()+`","tenant":`+sc.tenantRow+`,"exported_at":"2026-10-12T00:00:00Z","audit_chain_head":{"seq":3,"hash":"ab"},"tables":[{"name":"identity.role","part":1,"key":"`+sc.d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumH+`"}]}`)
	_, err = sc.im.Import(sc.ctx, sc.key)
	pgtest.WantErr(t, err, "chain head was not imported")
	// A delivered chain head that differs from the manifest's.
	sc.d = "tenants/" + sc.tenant.String() + "/exports/head2"
	head := `{"tenant_id":"` + sc.tenant.String() + `","seq":1,"hash":"\\x` + strings.Repeat("00", 32) + `","updated_at":"2026-10-12T00:00:00Z"}` + "\n"
	sumHead := sc.put(t, sc.d+"/audit.chain_head.00001.jsonl", head)
	sc.key = sc.d + "/manifest.json"
	sc.put(t, sc.key, `{"tenant_id":"`+sc.tenant.String()+`","tenant":`+sc.tenantRow+`,"exported_at":"2026-10-12T00:00:00Z","audit_chain_head":{"seq":1,"hash":"ab"},"tables":[{"name":"audit.chain_head","part":1,"key":"`+sc.d+`/audit.chain_head.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumHead+`"}]}`)
	_, err = sc.im.Import(sc.ctx, sc.key)
	pgtest.WantErr(t, err, "differs from the export")
}

// rowsAndTablesRefused is the step "rows and tables refused" of TestImportRefusals.
func (sc *refusalsScenario) rowsAndTablesRefused(t *testing.T) {
	var err error
	// A row the table refuses for another reason than a foreign key is reported as such.
	sc.d = "tenants/" + sc.tenant.String() + "/exports/bad"
	sumB := sc.put(t, sc.d+"/identity.role.00001.jsonl", `{"tenant_id":"`+sc.tenant.String()+`","id":"`+uuid.NewString()+`","name":"No code"}`+"\n")
	_, err = sc.im.Import(sc.ctx, sc.manifest(t, sc.d, `{"name":"identity.role","part":1,"key":"`+sc.d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumB+`"}`))
	pgtest.WantErr(t, err, "inserting into identity.role")
	// A table the cell does not have.
	sc.d = "tenants/" + sc.tenant.String() + "/exports/table"
	sumT := sc.put(t, sc.d+"/nowhere.thing.00001.jsonl", `{"tenant_id":"`+sc.tenant.String()+`"}`+"\n")
	_, err = sc.im.Import(sc.ctx, sc.manifest(t, sc.d, `{"name":"nowhere.thing","part":1,"key":"`+sc.d+`/nowhere.thing.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumT+`"}`))
	pgtest.WantErr(t, err, "nowhere.thing")
}

// savepointsFailing is the step "savepoints failing" of TestImportRefusals.
func (sc *refusalsScenario) savepointsFailing(t *testing.T) {
	// Savepoints that cannot start, commit or roll back.
	dOK := "tenants/" + sc.tenant.String() + "/exports/sp"
	sumS := sc.put(t, dOK+"/identity.role.00001.jsonl", sc.roleLine)
	keyOK := sc.manifest(t, dOK, `{"name":"identity.role","part":1,"key":"`+dOK+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumS+`"}`)
	dFK := "tenants/" + sc.tenant.String() + "/exports/spfk"
	sumF := sc.put(t, dFK+"/identity.principal_role.00001.jsonl", sc.assignment)
	keyFK := sc.manifest(t, dFK, `{"name":"identity.principal_role","part":1,"key":"`+dFK+`/identity.principal_role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumF+`"}`)
	for mode, key := range map[string]string{"begin": keyOK, "commit": keyOK, "rollback": keyFK} {
		t.Run("savepoint "+mode+" failing", func(t *testing.T) {
			sc.im.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
				return sc.pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, savepointFails{Tx: tx, mode: mode}) })
			}
			_, err := sc.im.Import(sc.ctx, key)
			pgtest.CheckErr(t, err, "savepoint "+mode)
		})
	}
	sc.im.InTx = nil
}

// unreadableTenantRow is the step "unreadable tenant row" of TestImportRefusals.
func (sc *refusalsScenario) unreadableTenantRow(t *testing.T) {
	var err error
	// A manifest whose tenant row is unreadable.
	sc.d = "tenants/" + sc.tenant.String() + "/exports/row"
	sc.key = sc.d + "/manifest.json"
	sc.put(t, sc.key, `{"tenant_id":"`+sc.tenant.String()+`","tenant":{"name":""},"exported_at":"2026-10-12T00:00:00Z","tables":[]}`)
	_, err = sc.im.Import(sc.ctx, sc.key)
	pgtest.WantErr(t, err, "tenant row is not readable")
}

// eachStatementFailing is the step "each statement failing" of TestImportRefusals.
func (sc *refusalsScenario) eachStatementFailing(t *testing.T) {
	// Database statements failing in turn, on a valid import into the erased tenant.
	sc.d = "tenants/" + sc.tenant.String() + "/exports/db"
	sumD := sc.put(t, sc.d+"/identity.role.00001.jsonl", sc.roleLine)
	sc.key = sc.manifest(t, sc.d, `{"name":"identity.role","part":1,"key":"`+sc.d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sumD+`"}`)
	for n := 1; n < 30; n++ {
		sc.im.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
			return sc.pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}) })
		}
		_, err := sc.im.Import(sc.ctx, sc.key)
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
}

// creatingATenantFailing is the step "creating a tenant failing" of TestImportRefusals.
func (sc *refusalsScenario) creatingATenantFailing(t *testing.T) {
	// Creating a tenant this cell has never held failing: statements fail in
	// turn on fresh tenants until the creation is the one that fails.
	created := false
	for n := 1; n < 10 && !created; n++ {
		fresh := uuid.Must(uuid.NewV7()).String()
		key := "tenants/" + fresh + "/exports/fresh/manifest.json"
		sc.put(t, key, `{"tenant_id":"`+fresh+`","tenant":{"id":"`+fresh+`","name":"Fresh","region":"eu-west","group_currency":"EUR","status":"active"},"exported_at":"2026-10-12T00:00:00Z","tables":[]}`)
		sc.im.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
			return sc.pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}) })
		}
		_, err := sc.im.Import(sc.ctx, key)
		created = errors.Is(err, pgtest.ErrInjected) && strings.Contains(err.Error(), "offboarding: create tenant "+fresh)
	}
	if !created {
		t.Error("no injected failure reached the creation of a new tenant")
	}
	sc.im.InTx = nil
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
