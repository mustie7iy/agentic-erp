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

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// exportThenErase offboards, exports and erases a fresh tenant and returns
// its id, the manifest key and the rows past their first state it held.
func exportThenErase(t *testing.T, pool *database.Pool, store blobstore.Dir) (uuid.UUID, string, map[string]int64) {
	t.Helper()
	ctx := context.Background()
	e := fixture.New(t, "payables.invoice.read", "ledger.entry.park", "ledger.entry.post", "ledger.entry.read")
	pastFirstStates(t, e)
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
	held := pastFirstStatesHeld(t, e.Tenant.ID)
	for name, n := range held {
		if n == 0 {
			t.Fatalf("%s before the export: got none, want some", name)
		}
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
	return e.Tenant.ID, ex.Ref, held
}

// pastFirstStates gives the tenant of e rows past the state the database
// inserts them in, each reached through its transitions: a period close
// signed off with its task done, a posted parked entry with its lines, a
// final consolidation run with its members and a balance, and a
// carry-forward run superseded by a second (ADR-0029, migration 000230).
func pastFirstStates(t *testing.T, e fixture.Env) {
	t.Helper()
	ten, who, closeID := e.Tenant.ID, e.Tenant.Human.ID, uuid.New()
	pgtest.Exec(t, `INSERT INTO closing.period_close (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, period_end, config_version, created_by,
		updated_by) VALUES ($1, $2, $3, 2026, 1, '2026-01-31', 1, $4, $4)`, ten, closeID, e.DE, who)
	pgtest.Exec(t, `INSERT INTO closing.close_task (tenant_id, close_id, code, seq, name, kind, due_on, created_by, updated_by)
		VALUES ($1, $2, 'AP', 1, 'Payables', 'manual', '2026-02-03', $3, $3)`, ten, closeID, who)
	pgtest.Exec(t, `UPDATE closing.close_task SET status = 'done', completed_by = $2, completed_at = now(), version = version + 1 WHERE close_id = $1`,
		closeID, who)
	pgtest.Exec(t, `UPDATE closing.period_close SET status = 'signed_off', version = version + 1 WHERE id = $1`, closeID)
	pgtest.Exec(t, `INSERT INTO closing.sign_off (tenant_id, id, close_id, signed_by, locked) VALUES ($1, gen_random_uuid(), $2, $3, '[{"locked": true}]')`,
		ten, closeID, who)
	parked := e.Park(t, "2026-03-15", "Imported", [2]string{"520000", "10.00"}, [2]string{"100000", "-10.00"})
	_, tag := e.Must(200, "GET", "/v1/parked-journal-entries/"+parked.String(), "", "")
	e.Must(200, "POST", "/v1/parked-journal-entries/"+parked.String()+":post", "", tag)
	finalRun(t, e)
	supersededCarryForward(t, e)
}

// finalRun gives the tenant of e a final consolidation run of companies
// 2000 (the parent) and 1000, with 1000's translation difference.
func finalRun(t *testing.T, e fixture.Env) {
	t.Helper()
	ten, who, run := e.Tenant.ID, e.Tenant.Human.ID, uuid.New()
	pgtest.Exec(t, `INSERT INTO consolidation.run (tenant_id, id, group_code, parent_id, companies, fiscal_year, fiscal_period, period_start, period_end,
		config_version, group_currency, rate_type, status, operation_id, created_by, updated_by) VALUES ($1, $2, 'GROUP', $3, ARRAY[$3, $4]::uuid[], 2026, 1,
		'2026-01-01', '2026-01-31', 1, 'EUR', 'closing', 'queued', gen_random_uuid(), $5, $5)`, ten, run, e.DE, e.US, who)
	pgtest.Exec(t, `UPDATE consolidation.run SET status = 'running', version = version + 1 WHERE id = $1`, run)
	pgtest.Exec(t, `INSERT INTO consolidation.run_member (tenant_id, run_id, legal_entity_id, company_code, role, ownership_percent, first_fiscal_year,
		first_fiscal_period, local_currency, closing_rate, rate_valid_from) VALUES ($1, $2, $3, '2000', 'parent', NULL, NULL, NULL, 'EUR', 1, NULL),
		($1, $2, $4, '1000', 'member', 100, 2026, 1, 'USD', 0.9, '2026-01-31')`, ten, run, e.DE, e.US)
	pgtest.Exec(t, `UPDATE consolidation.run_member SET period_status = 'open', period_version = 1, translated_at = now() WHERE run_id = $1`, run)
	pgtest.Exec(t, `INSERT INTO consolidation.run_balance (tenant_id, run_id, legal_entity_id, line_no, group_account, kind, amount_gc)
		VALUES ($1, $2, $3, 1, 'G3900', 'cta', 5)`, ten, run, e.US)
	pgtest.Exec(t, `UPDATE consolidation.run SET status = 'computed', computed_at = now(), version = version + 1 WHERE id = $1`, run)
	pgtest.Exec(t, `UPDATE consolidation.run SET status = 'final', finalized_by = updated_by, finalized_at = now(), version = version + 1 WHERE id = $1`, run)
}

// supersededCarryForward gives company 2000 of e a retained earnings
// account and a carry-forward of 2025 into 2026 of its leading ledger, run
// twice: the first run superseded by the second, which names it.
func supersededCarryForward(t *testing.T, e fixture.Env) {
	t.Helper()
	ten, who, equity, first, second := e.Tenant.ID, e.Tenant.Human.ID, uuid.New(), uuid.New(), uuid.New()
	pgtest.Exec(t, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, status, created_by, updated_by)
		SELECT $1, $2, c.id, '330000', 'Retained earnings', 'equity', 'active', $3, $3 FROM ledger.chart_of_accounts c WHERE c.tenant_id = $1 AND c.code = '2000'`,
		ten, equity, who)
	pgtest.Exec(t, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1, $2, $3, $4, $4)`,
		ten, e.DE, equity, who)
	run := `INSERT INTO ledger.carry_forward_run (tenant_id, id, legal_entity_id, ledger_id, from_fiscal_year, to_fiscal_year, basis,
		retained_earnings_account_id, row_count, created_by) SELECT $1, $2, $3, l.id, 2025, 2026, 'all_years', $4, 0, $5 FROM ledger.ledger l
		WHERE l.tenant_id = $1 AND l.code = '` + config.LeadingLedger + `'`
	pgtest.Exec(t, run, ten, first, e.DE, equity, who)
	// The successor names the run it supersedes in the same transaction (superseded_by_id is checked at commit).
	ctx := context.Background()
	tx, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // after the commit a no-op; before it, the failure already reported
	if _, err := tx.Exec(ctx, `UPDATE ledger.carry_forward_run SET status = 'superseded', superseded_by_id = $2, superseded_at = now(),
		version = version + 1 WHERE id = $1`, first, second); err != nil {
		t.Fatalf("supersede the first run: %v", err)
	}
	if _, err := tx.Exec(ctx, run, ten, second, e.DE, equity, who); err != nil {
		t.Fatalf("insert the second run: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the second run: %v", err)
	}
}

// pastFirstStatesHeld counts the rows pastFirstStates leaves in the
// tenant, by table and state.
func pastFirstStatesHeld(t *testing.T, tenant uuid.UUID) map[string]int64 {
	t.Helper()
	held := map[string]int64{}
	for name, sql := range map[string]string{
		"signed-off closes":     `SELECT count(*) FROM closing.period_close WHERE tenant_id = $1 AND status = 'signed_off'`,
		"done close tasks":      `SELECT count(*) FROM closing.close_task WHERE tenant_id = $1 AND status = 'done'`,
		"posted parked entries": `SELECT count(*) FROM ledger.parked_entry WHERE tenant_id = $1 AND status = 'posted' AND journal_entry_id IS NOT NULL`,
		"posted parked entries' lines": `SELECT count(*) FROM ledger.parked_entry_line l JOIN ledger.parked_entry p ON p.tenant_id = l.tenant_id
			AND p.id = l.parked_entry_id WHERE l.tenant_id = $1 AND p.status = 'posted'`,
		"final runs":           `SELECT count(*) FROM consolidation.run WHERE tenant_id = $1 AND status = 'final'`,
		"translated members":   `SELECT count(*) FROM consolidation.run_member WHERE tenant_id = $1 AND translated_at IS NOT NULL`,
		"final runs' balances": `SELECT count(*) FROM consolidation.run_balance WHERE tenant_id = $1`,
		"superseded carry-forwards": `SELECT count(*) FROM ledger.carry_forward_run WHERE tenant_id = $1 AND status = 'superseded'
			AND superseded_by_id IS NOT NULL`,
		"current carry-forwards": `SELECT count(*) FROM ledger.carry_forward_run WHERE tenant_id = $1 AND status = 'current'`,
	} {
		held[name] = count(t, sql, tenant)
	}
	return held
}

// TestReonboarding: an erased tenant comes back from its export, row for
// row, chain head and all; the import refuses what does not match.
func TestReonboarding(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t)
	store := blobstore.Dir{Root: t.TempDir()}
	tenant, ref, held := exportThenErase(t, pool, store)
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
	for name, n := range pastFirstStatesHeld(t, tenant) {
		if n != held[name] {
			t.Errorf("%s after the import: got %d, want the %d exported", name, n, held[name])
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
	if !t.Run("creating a tenant failing", sc.creatingATenantFailing) {
		t.FailNow()
	}
	t.Run("an older export's defaults", sc.olderExportsDefaults)
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
	// Rows that are no JSON object or name none of the table's columns; a row a check refuses, which waits for rows a later
	// pass might insert until a pass inserts nothing, then reported as such.
	for _, tc := range []struct{ name, line, want string }{
		{"no object", `[1]`, "not a JSON object"},
		{"no column", `{"nothing":1}`, "names none of the table's columns"},
		{"a check refused", strings.Replace(sc.roleLine, `"code":"NOBODY"`, `"code":"no body"`, 1), "stay referenced or refused after 1 passes: inserting into identity.role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := "tenants/" + sc.tenant.String() + "/exports/" + strings.ReplaceAll(tc.name, " ", "-")
			sum := sc.put(t, d+"/identity.role.00001.jsonl", strings.TrimSuffix(tc.line, "\n")+"\n")
			_, err := sc.im.Import(sc.ctx, sc.manifest(t, d, `{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":1,"bytes":1,"sha256":"`+sum+`"}`))
			pgtest.WantErr(t, err, tc.want)
		})
	}
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

// olderExportsDefaults is the step "an older export's defaults" of
// TestImportRefusals: a column an export written before it was added does
// not name takes its default, not NULL (ADR-0029).
func (sc *refusalsScenario) olderExportsDefaults(t *testing.T) {
	fresh := uuid.Must(uuid.NewV7()).String()
	d := "tenants/" + fresh + "/exports/older"
	line := `{"tenant_id":"` + fresh + `","id":"` + uuid.NewString() + `","code":"OLDER","name":"Older","status":"active","created_by":"` +
		uuid.NewString() + `","updated_by":"` + uuid.NewString() + `"}` + "\n" // no created_at, updated_at or version
	sum := sc.put(t, d+"/identity.role.00001.jsonl", line)
	key := d + "/manifest.json"
	sc.put(t, key, `{"tenant_id":"`+fresh+`","tenant":{"id":"`+fresh+`","name":"Older","region":"eu-west","group_currency":"EUR","status":"active"},
		"exported_at":"2026-10-12T00:00:00Z","tables":[{"name":"identity.role","part":1,"key":"`+d+`/identity.role.00001.jsonl","rows":1,"bytes":1,
		"sha256":"`+sum+`"}]}`)
	if _, err := sc.im.Import(sc.ctx, key); err != nil {
		t.Fatalf("import an older export: %v", err)
	}
	if n := count(t, `SELECT count(*) FROM identity.role WHERE tenant_id = $1 AND version = 1 AND created_at IS NOT NULL AND updated_at IS NOT NULL`,
		fresh); n != 1 {
		t.Errorf("roles imported with their defaults: got %d, want 1", n)
	}
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
