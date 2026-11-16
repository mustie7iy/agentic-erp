package audit_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	trail "github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// evidenceEnv is a tenant whose company 2000 has two entries posted
// today, the first's source document with an approval request a second
// person approved, and an audit event with a client address, with an API
// that writes evidence exports to a directory of its own.
type evidenceEnv struct {
	fixture.Env
	api            *apitest.API
	objects        blobstore.Dir
	first, second  uuid.UUID // the entries
	request        uuid.UUID // the approval request about the first's source document
	from, to, body string    // the period around today, and the request for both companies
}

func setupEvidence(t *testing.T) evidenceEnv {
	t.Helper()
	e := evidenceEnv{Env: fixture.New(t, "audit.evidence.export", "audit.event.read"), objects: blobstore.Dir{Root: t.TempDir()}}
	today := time.Now().UTC()
	e.first = e.PostedBy(t, "ledger", today.Format(time.DateOnly), 10000)
	e.second = e.PostedBy(t, "ledger", today.Format(time.DateOnly), 2550)
	e.request = uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO workflow.approval_request (tenant_id, id, object_type, object_id, title, requester_id, status, created_by)
		SELECT tenant_id, $2, 'manual_entry', source_document_id, 'Manual entry SA', $3, 'pending', $3 FROM ledger.journal_entry WHERE tenant_id = $1 AND id = $4`,
		e.Tenant.ID, e.request, e.Tenant.Human.ID, e.first)
	pgtest.Exec(t, `INSERT INTO workflow.approval_step (tenant_id, request_id, step_no, role_id, required)
		SELECT $1, $2, 1, id, 1 FROM identity.role WHERE tenant_id = $1 AND code = 'all'`, e.Tenant.ID, e.request)
	pgtest.Exec(t, `INSERT INTO workflow.decision (tenant_id, id, request_id, step_no, actor_id, actor_kind, decision, comment)
		VALUES ($1, gen_random_uuid(), $2, 1, $3, 'human', 'approve', 'Checked against the contract')`, e.Tenant.ID, e.request, e.Approver.ID)
	ctx := reqctx.WithClientAddress(context.Background(), netip.MustParseAddr("2001:db8::24"))
	if err := pgtest.Pool(t).InTx(ctx, database.Scope{Role: "erp_app", TenantID: e.Tenant.ID}, func(ctx context.Context, tx pgx.Tx) error {
		return trail.Write(ctx, tx, reqctx.Principal{ID: e.Tenant.Human.ID, Kind: "human", TenantID: e.Tenant.ID}, "", time.Now(),
			trail.Entry{Action: "test.thing.do", ObjectType: "test.thing", Outcome: "succeeded"})
	}); err != nil {
		t.Fatal(err)
	}
	e.from, e.to = today.AddDate(0, 0, -1).Format(time.DateOnly), today.AddDate(0, 0, 1).Format(time.DateOnly)
	e.body = exportBody([]uuid.UUID{e.DE, e.US}, e.from, e.to)
	e.api = apitest.NewWith(t, func(c *app.Config) { c.Objects = e.objects })
	return e
}

// exportBody is an export's request body.
func exportBody(companies []uuid.UUID, from, to string) string {
	ids, _ := json.Marshal(companies) // ids always marshal
	return fmt.Sprintf(`{"legal_entity_ids": %s, "from": %q, "to": %q}`, ids, from, to)
}

// as is the API as the tenant's person.
func (e evidenceEnv) as(t *testing.T) apitest.Client {
	t.Helper()
	return apitest.Client{T: t, API: e.api, Tenant: e.Tenant, As: e.Tenant.Human}
}

// queue asks for the export of both companies and returns its id.
func (e evidenceEnv) queue(t *testing.T) string {
	t.Helper()
	got, _ := e.as(t).Must(201, "POST", "/v1/audit-evidence-exports", e.body, "")
	return got["id"].(string)
}

// exporter is the worker as the server wires it.
func (e evidenceEnv) exporter(t *testing.T) *audit.EvidenceExporter {
	t.Helper()
	return &audit.EvidenceExporter{Endpoints: app.Audit(app.Config{Objects: e.objects}), Requesters: identity.Actors{}, Pool: pgtest.Pool(t),
		AppRole: "erp_app", WorkerRole: "erp_worker"}
}

// round runs the exporter once.
func (e evidenceEnv) round(t *testing.T) {
	t.Helper()
	if _, err := e.exporter(t).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// export reads an export through the API.
func (e evidenceEnv) export(t *testing.T, id string) map[string]any {
	t.Helper()
	got, _ := e.as(t).Must(200, "GET", "/v1/audit-evidence-exports/"+id, "", "")
	return got
}

// file downloads a file of an export through the API as the person.
func (e evidenceEnv) file(t *testing.T, id, name string) (int, []byte) {
	t.Helper()
	r := e.api.Do(apitest.Req{Method: http.MethodGet, Path: "/v1/audit-evidence-exports/" + id + "/files/" + name, Tenant: e.Tenant.ID, As: e.Tenant.Human})
	return r.Code, r.Body.Bytes()
}

// objectsIn are the store's objects of the export.
func (e evidenceEnv) objectsIn(t *testing.T, id string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.objects.Root, blobstore.Prefix, e.Tenant.ID.String(), blobstore.KindEvidence, id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, x := range entries {
		if !strings.HasSuffix(x.Name(), ".meta") { // the directory store's content types
			out = append(out, x.Name())
		}
	}
	return out
}

// settledExport is the export's status and failure code as the database
// holds them.
func settledExport(t *testing.T, id string) string {
	t.Helper()
	var status string
	var code *string
	pgtest.Scan(t, `SELECT status, failure_code FROM audit.evidence_export WHERE id = $1`, []any{id}, &status, &code)
	if code != nil {
		return status + " " + *code
	}
	return status
}

// jsonLines are the records of a JSON Lines file.
func jsonLines(t *testing.T, content []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// TestEvidenceExport: an export of the companies is queued; the
// exporter's round writes the period's audit events up to the chain's
// head, the companies' journal entries with their lines and the decisions
// on their source documents with who decided, and a manifest with each
// file's records and SHA-256 and the chain verified; the export completes
// with the manifest's figures, each file downloads as the manifest names
// it, and the completion is audited and published once (T101a
// acceptance).
func TestEvidenceExport(t *testing.T) {
	e := setupEvidence(t)
	r := e.api.Do(apitest.Req{Method: http.MethodPost, Path: "/v1/audit-evidence-exports", Body: e.body, Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: "evidence-1"})
	var created map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &created); err != nil || r.Code != http.StatusCreated {
		t.Fatalf("the request: got %d %s, want 201", r.Code, r.Body)
	}
	id := created["id"].(string)
	if got := r.Header().Get("Location"); got != "/v1/audit-evidence-exports/"+id {
		t.Errorf("Location: got %q, want the export's path", got)
	}
	if got := fmt.Sprint(created["status"], " ", created["companies"], " ", created["with_client_addresses"], " ", created["files"], " ", created["chain"]); got !=
		fmt.Sprintf("queued [map[company_code:1000 legal_entity_id:%s] map[company_code:2000 legal_entity_id:%s]] true [] <nil>", e.US, e.DE) {
		t.Errorf("queued: got %s", got)
	}
	e.round(t)
	got := e.export(t, id)
	chain, _ := got["chain"].(map[string]any)
	if got["status"] != "completed" || chain == nil {
		t.Fatalf("after the round: got %v, want completed with the chain's verification", got)
	}
	var events float64
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND occurred_at >= $2::date AND occurred_at < $3::date + 1 AND seq <= $4`,
		[]any{e.Tenant.ID, e.from, e.to, int64(chain["last_seq"].(float64))}, &events)
	if s := fmt.Sprint(got["audit_event_count"] == events, " ", got["journal_entry_count"], " ", got["approval_decision_count"], " ", chain["verified"], " ",
		chain["checked"] == chain["last_seq"]); s != "true 2 1 true true" {
		t.Errorf("the figures: got %s (%v audit events counted), want the period's events, 2 entries, 1 decision, the chain verified to its head", s, events)
	}
	if got["requested_by"] != e.Tenant.Human.ID.String() || got["requested_by_name"] == "" {
		t.Errorf("the requester: got %v %v, want the person and their name", got["requested_by"], got["requested_by_name"])
	}
	files := checkFiles(t, e, id, got)
	checkRecords(t, e, files)
	e.round(t) // run again: nothing queued, nothing written
	if objects := e.objectsIn(t, id); len(objects) != 4 {
		t.Errorf("objects after a second round: got %v, want the three files and the manifest", objects)
	}
	if evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "platform", Name: "audit_evidence_exported"}); len(evs) != 1 || evs[0]["manifest_sha256"] != got["manifest_sha256"] {
		t.Errorf("events: got %v, want one naming the manifest's hash", evs)
	}
	var audited []string
	rows, _ := pgtest.Admin(t).Query(context.Background(), `SELECT action FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 ORDER BY seq`, e.Tenant.ID, id)
	audited, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || strings.Join(audited, " ") != "audit.evidence_export.request audit.evidence_export.complete" {
		t.Errorf("audit records: got %v, %v, want the request and the completion", audited, err)
	}
}

// checkFiles downloads the manifest and every file it names: each is the
// bytes its SHA-256 names, with a line per record; it returns them by
// name.
func checkFiles(t *testing.T, e evidenceEnv, id string, got map[string]any) map[string][]byte {
	t.Helper()
	code, manifest := e.file(t, id, "manifest.json")
	sum := sha256.Sum256(manifest)
	if code != 200 || hex.EncodeToString(sum[:]) != got["manifest_sha256"] {
		t.Fatalf("the manifest: got %d %x, want 200 and the export's %v", code, sum, got["manifest_sha256"])
	}
	var m map[string]any
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if s := fmt.Sprint(m["format"], " ", m["evidence_export_id"], " ", m["from"], " ", m["to"], " ", m["with_client_addresses"], " ", m["chain"].(map[string]any)["verified"]); s !=
		audit.EvidenceFormat+" "+id+" "+e.from+" "+e.to+" true true" {
		t.Errorf("the manifest: got %s", s)
	}
	out := map[string][]byte{}
	listed := apitest.List(t, got, "files")
	for i, name := range []string{"audit_events.jsonl", "journal_entries.jsonl", "approval_decisions.jsonl"} {
		f := listed[i].(map[string]any)
		code, content := e.file(t, id, name)
		sum := sha256.Sum256(content)
		if code != 200 || f["name"] != name || hex.EncodeToString(sum[:]) != f["sha256"] || float64(len(content)) != f["size_bytes"] ||
			float64(len(jsonLines(t, content))) != f["records"] {
			t.Errorf("%s: got %d, %v, %x of %d bytes, want 200 and the export's %v", name, code, f["name"], sum, len(content), f)
		}
		out[name] = content
	}
	return out
}

// checkRecords reads the files: the audit events by sequence number with
// their client addresses, the entries with their lines, the decision with
// who decided and the entry it is about.
func checkRecords(t *testing.T, e evidenceEnv, files map[string][]byte) {
	t.Helper()
	events := jsonLines(t, files["audit_events.jsonl"])
	addressed := 0
	for i, ev := range events {
		if address, present := ev["client_address"]; !present {
			t.Errorf("event %v: no client_address, want it present", ev["seq"])
		} else if address == "2001:db8::24" {
			addressed++
		}
		if i > 0 && ev["seq"].(float64) <= events[i-1]["seq"].(float64) {
			t.Errorf("events out of order: %v after %v", ev["seq"], events[i-1]["seq"])
		}
	}
	if addressed != 1 {
		t.Errorf("events with the client address: got %d, want 1", addressed)
	}
	entries := jsonLines(t, files["journal_entries.jsonl"])
	if len(entries) != 2 || len(apitest.List(t, entries[0], "lines")) != 2 {
		t.Errorf("entries: got %v, want two with their lines", entries)
	}
	var name string
	pgtest.Scan(t, `SELECT display_name FROM identity.principal WHERE id = $1`, []any{e.Approver.ID}, &name)
	decisions := jsonLines(t, files["approval_decisions.jsonl"])
	if len(decisions) != 1 {
		t.Fatalf("decisions: got %v, want one", decisions)
	}
	d := decisions[0]
	if s := fmt.Sprint(d["approval_request_id"], " ", d["decision"], " ", d["decided_by"], " ", d["decided_by_name"], " ", d["journal_entry_ids"], " ", d["comment"]); s !=
		fmt.Sprintf("%s approve %s %s [%s] Checked against the contract", e.request, e.Approver.ID, name, e.first) {
		t.Errorf("the decision: got %s", s)
	}
}

// TestEvidenceExportAddresses: the audit events carry client addresses
// only in an export a holder of audit.event.read asked for, and only such
// a holder downloads them; the manifest stays the export's to download.
func TestEvidenceExportAddresses(t *testing.T) {
	e := setupEvidence(t)
	with := e.queue(t)
	e.Tenant.Revoke(t, "audit.event.read", e.Tenant.Human)
	without := e.queue(t)
	e.round(t)
	if got := e.export(t, without); got["with_client_addresses"] != false {
		t.Errorf("asked without audit.event.read: got %v, want no addresses", got["with_client_addresses"])
	}
	_, content := e.file(t, without, "audit_events.jsonl")
	for _, ev := range jsonLines(t, content) {
		if address, present := ev["client_address"]; present {
			t.Errorf("event %v: got client_address %v, want it absent", ev["seq"], address)
		}
	}
	e.as(t).Problem(403, "audit.client_addresses_withheld", "GET", "/v1/audit-evidence-exports/"+with+"/files/audit_events.jsonl", "", "")
	if code, _ := e.file(t, with, "manifest.json"); code != 200 {
		t.Errorf("the manifest without audit.event.read: got %d, want 200", code)
	}
	agent := apitest.Client{T: t, API: e.api, Tenant: e.Tenant, As: e.Tenant.Agent}
	agent.Problem(403, "identity.agent_not_allowed", "GET", "/v1/audit-evidence-exports/"+with, "", "")
}

// TestEvidenceExportRefused: a period that ends before it starts or is
// longer than a fiscal year, companies none of which the caller may act
// in, and a server without object storage are refused; an export covers
// the companies asked for that the caller may act in.
func TestEvidenceExportRefused(t *testing.T) {
	e := setupEvidence(t)
	c := e.as(t)
	for name, body := range map[string]string{
		"ending before it starts": exportBody([]uuid.UUID{e.DE}, e.to, e.from),
		"longer than 371 days":    exportBody([]uuid.UUID{e.DE}, "2026-01-01", "2027-01-07"),
	} {
		t.Run(name, func(t *testing.T) {
			if p := c.In(t).Problem(422, "request.validation_failed", "POST", "/v1/audit-evidence-exports", body, ""); !strings.Contains(fmt.Sprint(p["errors"]), "to") {
				t.Errorf("got %v, want to named", p)
			}
		})
	}
	if got, _ := c.Must(201, "POST", "/v1/audit-evidence-exports", exportBody([]uuid.UUID{e.DE}, "2026-01-01", "2027-01-06"), ""); got["to"] != "2027-01-06" {
		t.Errorf("371 days: got %v, want it queued", got)
	}
	unknown := exportBody([]uuid.UUID{uuid.Must(uuid.NewV7())}, e.from, e.to)
	if p := c.Problem(422, "request.validation_failed", "POST", "/v1/audit-evidence-exports", unknown, ""); !strings.Contains(fmt.Sprint(p["errors"]), "legal_entity_ids") {
		t.Errorf("an unknown company: got %v, want legal_entity_ids named", p)
	}
	reader := e.Reader(t, "Rita Reader", e.US)
	other := apitest.Client{T: t, API: e.api, Tenant: e.Tenant, As: reader}
	other.Problem(422, "request.validation_failed", "POST", "/v1/audit-evidence-exports", exportBody([]uuid.UUID{e.DE}, e.from, e.to), "")
	if got, _ := other.Must(201, "POST", "/v1/audit-evidence-exports", e.body, ""); fmt.Sprint(got["companies"]) !=
		fmt.Sprintf("[map[company_code:1000 legal_entity_id:%s]]", e.US) {
		t.Errorf("both asked by a caller of 1000: got %v, want 1000 alone", got["companies"])
	}
	plain := apitest.Client{T: t, API: apitest.NewWith(t, nil), Tenant: e.Tenant, As: e.Tenant.Human}
	plain.Problem(503, "platform.unavailable", "POST", "/v1/audit-evidence-exports", e.body, "")
}

// TestEvidenceExportsListed: the exports whose every company the caller
// may act in, newest first, filtered and paged; an export of a company
// the caller cannot act in is neither listed nor found.
func TestEvidenceExportsListed(t *testing.T) {
	e := setupEvidence(t)
	both := e.queue(t)
	pgtest.Exec(t, `UPDATE audit.evidence_export SET status = 'failed', failure_code = 'identity.principal_blocked', version = version + 1 WHERE id = $1`, both)
	us, _ := e.as(t).Must(201, "POST", "/v1/audit-evidence-exports", exportBody([]uuid.UUID{e.US}, e.from, e.to), "")
	e.round(t)
	c := e.as(t)
	for _, q := range []struct{ query, want string }{
		{"", us["id"].(string) + " " + both},
		{"?status=failed", both},
		{"?status=completed&legal_entity_id=" + e.US.String(), us["id"].(string)},
		{"?legal_entity_id=" + e.DE.String(), both},
	} {
		t.Run(q.query, func(t *testing.T) {
			got, _ := c.In(t).Must(200, "GET", "/v1/audit-evidence-exports"+q.query, "", "")
			if ids := exportIDs(got); ids != q.want {
				t.Errorf("got %s, want %s", ids, q.want)
			}
		})
	}
	c.Problem(400, "request.invalid_page_token", "GET", "/v1/audit-evidence-exports?page_token=nonsense", "", "")
	page, _ := c.Must(200, "GET", "/v1/audit-evidence-exports?page_size=1", "", "")
	next, _ := c.Must(200, "GET", "/v1/audit-evidence-exports?page_size=1&page_token="+page["next_page_token"].(string), "", "")
	if got := exportIDs(page) + " " + exportIDs(next); got != us["id"].(string)+" "+both {
		t.Errorf("pages: got %s", got)
	}
	reader := apitest.Client{T: t, API: e.api, Tenant: e.Tenant, As: e.Reader(t, "Rita Reader", e.US)}
	if got, _ := reader.Must(200, "GET", "/v1/audit-evidence-exports", "", ""); exportIDs(got) != us["id"] {
		t.Errorf("a caller of 1000: got %s, want the export of 1000 alone", exportIDs(got))
	}
	reader.Problem(404, "request.not_found", "GET", "/v1/audit-evidence-exports/"+both, "", "")
	reader.Problem(404, "request.not_found", "GET", "/v1/audit-evidence-exports/"+both+"/files/manifest.json", "", "")
}

// exportIDs are a page's export ids in order.
func exportIDs(page map[string]any) string {
	var ids []string
	for _, it := range page["items"].([]any) {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	return strings.Join(ids, " ")
}

// TestEvidenceExportFile: an export not completed has no files; another
// name is not found; a file or manifest the store no longer holds as
// written is refused; a server without object storage, or whose store
// does not answer, is unavailable.
func TestEvidenceExportFile(t *testing.T) {
	e := setupEvidence(t)
	id := e.queue(t)
	e.as(t).Problem(409, "audit.evidence_not_completed", "GET", "/v1/audit-evidence-exports/"+id+"/files/manifest.json", "", "")
	e.round(t)
	e.as(t).Problem(404, "request.not_found", "GET", "/v1/audit-evidence-exports/"+id+"/files/other.jsonl", "", "")
	e.as(t).Problem(404, "request.not_found", "GET", "/v1/audit-evidence-exports/"+uuid.Must(uuid.NewV7()).String()+"/files/manifest.json", "", "")
	plain := apitest.Client{T: t, API: apitest.NewWith(t, nil), Tenant: e.Tenant, As: e.Tenant.Human}
	plain.Problem(503, "platform.unavailable", "GET", "/v1/audit-evidence-exports/"+id+"/files/manifest.json", "", "")
	gone := apitest.Client{T: t, API: apitest.NewWith(t, func(c *app.Config) { c.Objects = evidenceStore{Store: e.objects, get: errors.New("down")} }),
		Tenant: e.Tenant, As: e.Tenant.Human}
	gone.Problem(503, "platform.unavailable", "GET", "/v1/audit-evidence-exports/"+id+"/files/manifest.json", "", "")
	dir := filepath.Join(e.objects.Root, blobstore.Prefix, e.Tenant.ID.String(), blobstore.KindEvidence, id)
	for _, name := range e.objectsIn(t, id) {
		if strings.HasPrefix(name, "journal_entries.") {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.as(t).Problem(409, "audit.evidence_changed", "GET", "/v1/audit-evidence-exports/"+id+"/files/journal_entries.jsonl", "", "")
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.as(t).Problem(409, "audit.evidence_changed", "GET", "/v1/audit-evidence-exports/"+id+"/files/manifest.json", "", "")
}

// evidenceStore is an object store whose writes or reads fail.
type evidenceStore struct {
	blobstore.Store
	put, get error
	suffix   string // the keys whose writes fail; "": every key
}

func (s evidenceStore) Put(ctx context.Context, key string, content []byte, opts blobstore.PutOptions) error {
	if s.put != nil && strings.HasSuffix(key, s.suffix) {
		return s.put
	}
	return s.Store.Put(ctx, key, content, opts)
}

func (s evidenceStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if s.get != nil {
		return nil, "", s.get
	}
	return s.Store.Get(ctx, key)
}
