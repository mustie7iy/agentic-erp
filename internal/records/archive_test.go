package records_test

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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/records"
)

// archiveDay is the day the archiving tests run on: 2026 has ended, so a
// rule of zero years puts it past retention.
const archiveDay = "2027-01-15"

// archiveEnv is a tenant whose company 2000 keeps journal entries for
// zero years and has two entries posted in 2026, with an API that writes
// archive files to a directory of its own.
type archiveEnv struct {
	fixture.Env
	api     *apitest.API
	objects blobstore.Dir
	now     *time.Time // the day the API and the archiver run on
}

// setupArchive posts the entries and, unless open, closes 2026 in every
// ledger of company 2000.
func setupArchive(t *testing.T, open bool) archiveEnv {
	t.Helper()
	e := archiveEnv{Env: setup(t, years(0), "records.archive.run", "ledger.entry.read"), objects: blobstore.Dir{Root: t.TempDir()}}
	day, _ := time.Parse(time.DateOnly, archiveDay) // the constant parses
	day = day.Add(12 * time.Hour)
	e.now = &day
	e.PostedBy(t, "ledger", "2026-03-02", 10000)
	e.PostedBy(t, "ledger", "2026-04-01", 2550)
	if !open {
		closeYear(t, e.Env, 2026)
	}
	e.api = apitest.NewWith(t, func(c *app.Config) { c.Objects = e.objects; c.Now = func() time.Time { return *e.now } })
	return e
}

// closeYear closes every period of the company 2000's fiscal year.
func closeYear(t *testing.T, e fixture.Env, year int) {
	t.Helper()
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1, updated_by = $3
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_year = $4 AND status = 'open'`, e.Tenant.ID, e.DE, e.Tenant.Human.ID, year)
}

// as is the API as the tenant's person.
func (e archiveEnv) as() apitest.Client {
	return apitest.Client{T: e.T, API: e.api, Tenant: e.Tenant, As: e.Tenant.Human}
}

// body is a run's request body.
func body(le uuid.UUID, kind string, year int) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "document_kind": %q, "fiscal_year": %d}`, le, kind, year)
}

// archiver is the worker as the server wires it, on the test's day.
func (e archiveEnv) archiver() *records.Archiver {
	ep := app.Records(app.Config{Now: func() time.Time { return *e.now }, Objects: e.objects})
	return &records.Archiver{Endpoints: ep, Requesters: identity.Actors{}, Pool: pgtest.Pool(e.T), AppRole: "erp_app", WorkerRole: "erp_worker",
		Now: func() time.Time { return *e.now }}
}

// round runs the archiver once.
func (e archiveEnv) round(t *testing.T) {
	t.Helper()
	if _, err := e.archiver().RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// run reads a run through the API.
func (e archiveEnv) run(t *testing.T, id string) map[string]any {
	t.Helper()
	got, _ := e.as().Must(200, "GET", "/v1/archiving-runs/"+id, "", "")
	return got
}

// file downloads a run's file through the API.
func (e archiveEnv) file(t *testing.T, id string) (int, []byte) {
	t.Helper()
	r := e.api.Do(apitest.Req{Method: http.MethodGet, Path: "/v1/archiving-runs/" + id + "/file", Tenant: e.Tenant.ID, As: e.Tenant.Human})
	return r.Code, r.Body.Bytes()
}

// objectsIn are the store's objects of the tenant's archives.
func (e archiveEnv) objectsIn(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.objects.Root, blobstore.Prefix, e.Tenant.ID.String(), blobstore.KindArchives))
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

// TestArchivingRun: a run of a year past retention and closed is queued,
// a second request answers it, and the archiver's round writes every
// entry of the year with its lines to the file and the manifest beside
// it; the run completes with the file's hash and the manifest's figures,
// the year reads as archived and nothing in the ledger changes (T99b
// acceptance).
func TestArchivingRun(t *testing.T) {
	e := setupArchive(t, false)
	var before int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`, []any{e.Tenant.ID}, &before)
	created, _ := e.as().Must(201, "POST", "/v1/archiving-runs", body(e.DE, "journal_entry", 2026), "")
	id := created["id"].(string)
	if got := fmt.Sprint(created["status"], " ", created["company_code"], " ", created["retention_years"], " ", created["fiscal_year"], " ", created["file"]); got !=
		"queued 2000 0 2026 <nil>" {
		t.Errorf("queued: got %s", got)
	}
	again, _ := e.as().Must(200, "POST", "/v1/archiving-runs", body(e.DE, "journal_entry", 2026), "")
	if again["id"] != id {
		t.Errorf("a second request: got run %v, want %s", again["id"], id)
	}
	e.round(t)
	got := e.run(t, id)
	f, _ := got["file"].(map[string]any)
	if got["status"] != "completed" || f == nil {
		t.Fatalf("after the round: got %v, want completed with its file", got)
	}
	if s := fmt.Sprint(f["name"], " ", f["entry_count"], " ", f["line_count"], " ", f["debit_total"]); s != id+".jsonl 2 4 map[amount:125.50 currency:EUR]" {
		t.Errorf("the file's figures: got %s", s)
	}
	code, content := e.file(t, id)
	sum := sha256.Sum256(content)
	if code != 200 || hex.EncodeToString(sum[:]) != f["sha256"] || float64(len(content)) != f["size_bytes"] {
		t.Errorf("the file: got %d, %x of %d bytes, want 200 and the run's %v of %v", code, sum, len(content), f["sha256"], f["size_bytes"])
	}
	checkLines(t, content)
	checkManifest(t, e, id, f)
	e.round(t) // run again: nothing queued, nothing written
	if got := e.objectsIn(t); len(got) != 2 {
		t.Errorf("objects after a second round: got %v, want the file and its manifest", got)
	}
	if evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "records", Name: "archiving_run_completed"}); len(evs) != 1 || evs[0]["sha256"] != f["sha256"] {
		t.Errorf("events: got %v, want one naming the file's hash", evs)
	}
	var after int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`, []any{e.Tenant.ID}, &after)
	if after != before {
		t.Errorf("journal entries: got %d after, want the %d before", after, before)
	}
	st, _ := e.as().Must(200, "GET", "/v1/retention-status?document_kind=journal_entry&legal_entity_id="+e.DE.String(), "", "")
	if y := st["years"].([]any)[0].(map[string]any); y["fiscal_year"] != float64(2026) || y["archived"] != true {
		t.Errorf("the status: got %v, want 2026 archived", y)
	}
	done, _ := e.as().Must(200, "POST", "/v1/archiving-runs", body(e.DE, "journal_entry", 2026), "")
	if done["id"] != id || done["status"] != "completed" {
		t.Errorf("a request after completion: got %v, want the completed run", done)
	}
}

// checkLines reads the file as JSON Lines: one entry per line, by posting
// date, each with its lines and their amounts.
func checkLines(t *testing.T, content []byte) {
	t.Helper()
	var dates, amounts []string
	sc := bufio.NewScanner(bytes.NewReader(content))
	for sc.Scan() {
		var x struct {
			PostingDate string           `json:"posting_date"`
			Ledger      string           `json:"ledger"`
			Raw         []map[string]any `json:"lines"`
		}
		if err := json.Unmarshal(sc.Bytes(), &x); err != nil {
			t.Fatal(err)
		}
		dates = append(dates, x.PostingDate+" "+x.Ledger)
		for _, l := range x.Raw {
			amounts = append(amounts, fmt.Sprint(l["account"], " ", l["amount"], " ", l["amount_local"]))
		}
	}
	if got := strings.Join(dates, ", "); got != "2026-03-02 0L, 2026-04-01 0L" {
		t.Errorf("entries: got %s", got)
	}
	if got := strings.Join(amounts, ", "); got != "520000 100.00 100.00, 100000 -100.00 -100.00, 520000 25.50 25.50, 100000 -25.50 -25.50" {
		t.Errorf("lines: got %s", got)
	}
}

// checkManifest reads the manifest the store holds beside the file.
func checkManifest(t *testing.T, e archiveEnv, id string, f map[string]any) {
	t.Helper()
	raw, _, err := e.objects.Get(context.Background(), blobstore.Key(e.Tenant.ID, blobstore.KindArchives, id+".manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(m["format"], " ", m["file"], " ", m["entry_count"], " ", m["line_count"], " ", m["debit_total"], " ", m["currency"], " ", m["sha256"] == f["sha256"],
		" ", hex.EncodeToString(sum[:]) == f["manifest_sha256"]); got != records.ArchiveFormat+" "+id+".jsonl 2 4 125.50 EUR true true" {
		t.Errorf("the manifest: got %s", got)
	}
}

// TestArchivingRunRefused: only journal entries are archived, of a year
// past retention and closed, of a company the caller may act in, by a
// server with object storage.
func TestArchivingRunRefused(t *testing.T) {
	e := setupArchive(t, true)
	c := e.as()
	if p := c.Problem(422, "request.validation_failed", "POST", "/v1/archiving-runs", body(e.DE, "personal_data", 2026), ""); !strings.Contains(fmt.Sprint(p["errors"]), "document_kind") {
		t.Errorf("personal data: got %v, want document_kind named", p)
	}
	c.Problem(422, "records.year_not_closed", "POST", "/v1/archiving-runs", body(e.DE, "journal_entry", 2026), "")
	c.Problem(422, "records.year_not_past_retention", "POST", "/v1/archiving-runs", body(e.DE, "journal_entry", 2031), "")
	c.Problem(404, "request.not_found", "POST", "/v1/archiving-runs", body(uuid.Must(uuid.NewV7()), "journal_entry", 2026), "")
	c.Problem(422, "records.year_not_past_retention", "POST", "/v1/archiving-runs", body(e.US, "journal_entry", 2026), "") // no rule for 1000
	*e.now = e.now.AddDate(-1, 0, 0)                                                                                        // 2026 not ended
	closeYear(t, e.Env, 2026)
	c.Problem(422, "records.year_not_past_retention", "POST", "/v1/archiving-runs", body(e.DE, "journal_entry", 2026), "")
	plain := apitest.NewWith(t, nil) // no object storage
	apitest.Client{T: t, API: plain, Tenant: e.Tenant, As: e.Tenant.Human}.Problem(503, "platform.unavailable", "POST", "/v1/archiving-runs", body(e.DE, "journal_entry", 2026), "")
}
