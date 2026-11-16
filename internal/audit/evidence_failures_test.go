package audit_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	trail "github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestEvidenceExporterRefuses: a requester blocked since the request, and
// another manifest under the export's key, fail the export with their
// code, audited; nothing is published.
func TestEvidenceExporterRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		before func(t *testing.T, e evidenceEnv, id string)
		change func(w *audit.EvidenceExporter)
		want   string
	}{
		"more journal lines than an export holds": {before: func(*testing.T, evidenceEnv, string) {},
			change: func(w *audit.EvidenceExporter) { w.Endpoints.Journal = journal{lines: audit.MaxEvidenceRecords + 1} }, want: "failed audit.evidence_too_large"},
		"the requester blocked": {before: func(t *testing.T, e evidenceEnv, _ string) {
			t.Helper()
			pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, e.Tenant.Human.ID)
		}, want: "failed identity.principal_blocked"},
		"another manifest under its key": {before: func(t *testing.T, e evidenceEnv, id string) {
			t.Helper()
			key := blobstore.Key(e.Tenant.ID, blobstore.KindEvidence, id+"/manifest.json")
			if err := e.objects.Put(context.Background(), key, []byte(`{"evidence_export_id": "`+uuid.Must(uuid.NewV7()).String()+`"}`), blobstore.PutOptions{}); err != nil {
				t.Fatal(err)
			}
		}, want: "failed audit.evidence_changed"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setupEvidence(t)
			id := e.queue(t)
			c.before(t, e, id)
			w := e.exporter(t)
			w.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
			if c.change != nil {
				c.change(w)
			}
			if _, err := w.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := settledExport(t, id); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
			var audited int
			pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'audit.evidence_export.fail' AND object_id = $2`,
				[]any{e.Tenant.ID, id}, &audited)
			if audited != 1 {
				t.Errorf("audit records: got %d, want 1", audited)
			}
			if evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "platform", Name: "audit_evidence_exported"}); len(evs) != 0 {
				t.Errorf("events: got %v, want none", evs)
			}
		})
	}
}

// TestEvidenceExporterWaits: the store not taking a file or the manifest,
// or not returning a manifest it holds, and a company, the journal, the
// approvals or the people not read leave the export queued for the next
// round, which completes it once they answer.
func TestEvidenceExporterWaits(t *testing.T) {
	e := setupEvidence(t)
	id := e.queue(t)
	down := errors.New("down")
	for name, change := range map[string]func(w *audit.EvidenceExporter){
		"the store refusing a file": func(w *audit.EvidenceExporter) { w.Endpoints.Objects = evidenceStore{Store: e.objects, put: down} },
		"the store refusing the manifest": func(w *audit.EvidenceExporter) {
			w.Endpoints.Objects = evidenceStore{Store: e.objects, put: down, suffix: "manifest.json"}
		},
		"the store not returning the manifest it holds": func(w *audit.EvidenceExporter) {
			w.Endpoints.Objects = evidenceStore{Store: e.objects, put: blobstore.ErrExists, suffix: "manifest.json", get: down}
		},
		"no company":                    func(w *audit.EvidenceExporter) { w.Endpoints.LegalEntities = companies{} },
		"the company not read":          func(w *audit.EvidenceExporter) { w.Endpoints.LegalEntities = companies{err: down} },
		"the journal not read":          func(w *audit.EvidenceExporter) { w.Endpoints.Journal = journal{err: down} },
		"the journal lines not counted": func(w *audit.EvidenceExporter) { w.Endpoints.Journal = journal{countErr: down} },
		"the approvals not read": func(w *audit.EvidenceExporter) {
			w.Endpoints.Approvals = approvalsDown{}
		},
		"the deciders not named": func(w *audit.EvidenceExporter) { w.Endpoints.People = names{fail: 2, n: new(int)} },
	} {
		t.Run(name, func(t *testing.T) {
			w := e.exporter(t)
			change(w)
			if _, err := w.RunOnce(context.Background()); err == nil {
				t.Errorf("got %v, want the round to fail", err)
			}
			if got := settledExport(t, id); got != audit.ExportQueued {
				t.Errorf("got %s, want queued", got)
			}
		})
	}
	e.round(t)
	if got := settledExport(t, id); got != audit.ExportCompleted {
		t.Errorf("once they answer: got %s, want completed", got)
	}
}

// companies is a tenancy context that does not hold the company, or
// cannot be read.
type companies struct{ err error }

func (x companies) Entity(context.Context, uuid.UUID) (tenancy.Entity, bool, error) {
	return tenancy.Entity{}, false, x.err
}

// journal is a ledger whose journal cannot be read, or whose lines are
// counted as lines, or not counted.
type journal struct {
	err, countErr error
	lines         int
}

func (x journal) EntriesBetween(context.Context, uuid.UUID, time.Time, time.Time, func(ledger.ArchivedEntry) error) error {
	return x.err
}

func (x journal) LinesBetween(context.Context, uuid.UUID, time.Time, time.Time) (int, error) {
	return x.lines, x.countErr
}

// approvalsDown is an approvals context that cannot be read.
type approvalsDown struct{}

func (approvalsDown) About(context.Context, []uuid.UUID) ([]approvals.Request, error) {
	return nil, errors.New("approvals down")
}

// names is an identity context whose fail-th read and those after it
// fail; n counts the reads.
type names struct {
	fail int
	n    *int
}

func (x names) PersonNames(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
	if *x.n++; *x.n >= x.fail {
		return nil, errors.New("identity down")
	}
	return map[uuid.UUID]string{}, nil
}

// TestEvidenceExporterRunTwice: a round whose commit is lost after it
// wrote the files and the manifest leaves the export queued; the next
// round, which reads a period still open and so writes another audit
// events file, completes the export with the manifest kept, whose files
// download as written, with one event (ADR-0012: idempotent activities).
func TestEvidenceExporterRunTwice(t *testing.T) {
	e := setupEvidence(t)
	id := e.queue(t)
	lost := e.exporter(t)
	lost.Begin = loseCommitOf(t)
	if _, err := lost.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("the lost commit: got %v, want the injected failure", err)
	}
	if got, objects := settledExport(t, id), e.objectsIn(t, id); got != audit.ExportQueued || len(objects) != 4 {
		t.Fatalf("after the lost commit: got %s with %v, want queued with the files and manifest written", got, objects)
	}
	kept, _, err := e.objects.Get(context.Background(), blobstore.Key(e.Tenant.ID, blobstore.KindEvidence, id+"/manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return trail.Add(ctx, trail.Entry{Action: "test.thing.do", ObjectType: "test.thing", Outcome: "succeeded"})
	}); err != nil {
		t.Fatal(err)
	}
	e.round(t)
	e.round(t)
	if got, objects := settledExport(t, id), e.objectsIn(t, id); got != audit.ExportCompleted || len(objects) != 5 {
		t.Errorf("after two more rounds: got %s with %v, want completed, a second audit events file beside the first", got, objects)
	}
	got := e.export(t, id)
	files := checkFiles(t, e, id, got)
	if _, manifest := e.file(t, id, "manifest.json"); string(manifest) != string(kept) || len(files) != 3 {
		t.Errorf("the manifest: got %s, want the one the lost round wrote", manifest)
	}
	if evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "platform", Name: "audit_evidence_exported"}); len(evs) != 1 {
		t.Errorf("events: got %d, want 1", len(evs))
	}
}

// loseCommitOf begins the tenant's transactions so that their commit is
// lost, as when the acknowledgement of a commit never came.
func loseCommitOf(t *testing.T) database.BeginFunc {
	t.Helper()
	pool := pgtest.Pool(t)
	return func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil || s.TenantID == uuid.Nil {
			return tx, err
		}
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
	}
}

// failingRounds runs the exporter with each of its statements failing in
// turn until a round fails none, and reports whether the export then
// settled as want.
func failingRounds(t *testing.T, e evidenceEnv, id, want string, change func(w *audit.EvidenceExporter)) bool {
	t.Helper()
	pool := pgtest.Pool(t)
	for failAt := 1; failAt <= 300; failAt++ {
		count := 0
		w := e.exporter(t)
		change(w)
		w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: failAt, Count: &count}, nil
		}
		_, err := w.RunOnce(context.Background())
		if err != nil && !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: got %v, want the injected failure", failAt, err)
		}
		if err == nil && count < failAt && settledExport(t, id) == want {
			return true
		}
	}
	return false
}

// TestEvidenceExporterDatabaseFailures: each of the round's statements
// failing in turn fails the round and leaves the export queued, until a
// round that fails none settles it, completed or, for a requester blocked
// since, failed; no transaction fails the round; a stopped worker
// returns.
func TestEvidenceExporterDatabaseFailures(t *testing.T) {
	e := setupEvidence(t)
	id := e.queue(t)
	if !failingRounds(t, e, id, audit.ExportCompleted, func(*audit.EvidenceExporter) {}) {
		t.Fatal("still failing after 300 statements")
	}
	refused := setupEvidence(t)
	failing := refused.queue(t)
	pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, refused.Tenant.Human.ID)
	if !failingRounds(t, refused, failing, "failed identity.principal_blocked", func(*audit.EvidenceExporter) {}) {
		t.Fatal("refused: still failing after 300 statements")
	}
	w := e.exporter(t)
	w.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected }
	if _, err := w.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("no transaction: got %v, want the injected failure", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.exporter(t).Run(ctx) // returns once the context has ended
}

// TestEvidenceDatabaseFailures calls each read and write outside a unit
// of work and with each of its database statements failing in turn, and
// with a company or a requester its ports cannot give.
func TestEvidenceDatabaseFailures(t *testing.T) {
	e := setupEvidence(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	ep := app.Audit(app.Config{Objects: e.objects, Pages: pages})
	from, _ := time.Parse(time.DateOnly, e.from) // the setup's dates parse
	to, _ := time.Parse(time.DateOnly, e.to)
	create := func(ctx context.Context) error {
		_, err := ep.CreateAuditEvidenceExport(ctx, &gen.AuditEvidenceExportCreate{LegalEntityIds: []uuid.UUID{e.DE}, From: from, To: to},
			gen.CreateAuditEvidenceExportParams{})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "an export queued", create)
	id := e.queue(t)
	e.round(t)
	export := uuid.MustParse(id)
	calls := map[string]func(ctx context.Context) error{
		"an export queued": create,
		"an export read": func(ctx context.Context) error {
			_, err := ep.GetAuditEvidenceExport(ctx, gen.GetAuditEvidenceExportParams{ID: export})
			return err
		},
		"exports listed": func(ctx context.Context) error {
			_, err := ep.ListAuditEvidenceExports(ctx, gen.ListAuditEvidenceExportsParams{})
			return err
		},
		"a file read": func(ctx context.Context) error {
			_, err := ep.GetAuditEvidenceExportFile(ctx, gen.GetAuditEvidenceExportFileParams{ID: export, Name: "audit_events.jsonl"})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	down := errors.New("down")
	for name, c := range map[string]struct {
		change func(ep *audit.Endpoints)
		call   func(ctx context.Context, ep *audit.Endpoints) error
		want   string
	}{
		"a company the tenancy context does not hold": {func(ep *audit.Endpoints) { ep.LegalEntities = companies{} }, read, "not held by the tenancy context"},
		"a company not read":                          {func(ep *audit.Endpoints) { ep.LegalEntities = companies{err: down} }, read, "down"},
		"requesters not named":                        {func(ep *audit.Endpoints) { ep.People = names{fail: 1, n: new(int)} }, read, "requesters"},
		"a company asked for not read": {func(ep *audit.Endpoints) { ep.LegalEntities = companies{err: down} }, func(ctx context.Context, ep *audit.Endpoints) error {
			_, err := ep.RequestExport(ctx, []uuid.UUID{e.DE}, from, to)
			return err
		}, "down"},
	} {
		t.Run(name, func(t *testing.T) {
			bad := app.Audit(app.Config{Objects: e.objects, Pages: pages})
			c.change(bad)
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return c.call(ctx, bad) }); err == nil ||
				!strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
}

// read lists the setup's exports.
func read(ctx context.Context, ep *audit.Endpoints) error {
	_, _, err := ep.Exports(ctx, audit.ExportsQuery{})
	return err
}

// TestEvidenceExportBrokenChain: an export of a chain that does not
// verify completes and names where it broke, in its figures, manifest and
// event; an entry without a source document has no approval evidence.
func TestEvidenceExportBrokenChain(t *testing.T) {
	e := setupEvidence(t)
	id := e.queue(t)
	pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE audit.audit_event SET hash = '\x`+strings.Repeat("ab", 32)+`' WHERE tenant_id = '`+e.Tenant.ID.String()+`' AND seq = 1; END $$`)
	w := e.exporter(t)
	w.Endpoints.Journal = unsourced{}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := e.export(t, id)
	chain, _ := got["chain"].(map[string]any)
	if s := fmt.Sprint(got["status"], " ", chain["verified"], " ", chain["first_broken_seq"], " ", got["journal_entry_count"], " ", got["approval_decision_count"]); s !=
		"completed false 1 2 0" {
		t.Errorf("got %s, want completed, broken at 1, two entries without decisions", s)
	}
	if evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "platform", Name: "audit_evidence_exported"}); len(evs) != 1 || evs[0]["chain_verified"] != false {
		t.Errorf("events: got %v, want one naming the chain unverified", evs)
	}
}

// unsourced is a journal whose entries name no source document, once per
// company asked for.
type unsourced struct{}

func (unsourced) LinesBetween(context.Context, uuid.UUID, time.Time, time.Time) (int, error) {
	return 2, nil
}

func (unsourced) EntriesBetween(_ context.Context, _ uuid.UUID, _, _ time.Time, fn func(ledger.ArchivedEntry) error) error {
	return fn(ledger.ArchivedEntry{ID: uuid.Must(uuid.NewV7()), Currency: "EUR", LocalCurrency: "EUR", GroupCurrency: "EUR"})
}

// TestEvidenceManifestWithoutAFile: a manifest kept from an earlier round
// that names a file without its SHA-256 completes the export, and the
// file is refused, not handed out unchecked.
func TestEvidenceManifestWithoutAFile(t *testing.T) {
	e := setupEvidence(t)
	id := e.queue(t)
	manifest := `{"evidence_export_id": "` + id + `", "files": [{"name": "audit_events.jsonl", "sha256": "zz"}, {"name": "journal_entries.jsonl"},
		{"name": "approval_decisions.jsonl"}], "chain": {"verified": true}}`
	if err := e.objects.Put(context.Background(), blobstore.Key(e.Tenant.ID, blobstore.KindEvidence, id+"/manifest.json"), []byte(manifest),
		blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	e.round(t)
	if got := settledExport(t, id); got != audit.ExportCompleted {
		t.Fatalf("got %s, want completed", got)
	}
	for _, name := range []string{"audit_events.jsonl", "journal_entries.jsonl"} {
		e.as(t).Problem(409, "audit.evidence_changed", "GET", "/v1/audit-evidence-exports/"+id+"/files/"+name, "", "")
	}
	if _, err := (ledger.Archive{}).LinesBetween(context.Background(), e.DE, time.Now(), time.Now()); err == nil {
		t.Error("journal lines outside a unit of work: got nil error")
	}
	if _, err := (approvals.Evidence{}).About(context.Background(), nil); err == nil {
		t.Error("approval requests outside a unit of work: got nil error")
	}
}
