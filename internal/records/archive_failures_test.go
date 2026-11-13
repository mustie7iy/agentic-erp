package records_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/records"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// queue asks for the run of 2026 of company 2000 and returns its id.
func (e archiveEnv) queue(t *testing.T) string {
	t.Helper()
	got, _ := e.as().Must(201, "POST", "/v1/archiving-runs", body(e.DE, "journal_entry", 2026), "")
	return got["id"].(string)
}

// settled is the run's status and failure code as the database holds them.
func settled(t *testing.T, id string) string {
	t.Helper()
	var status string
	var code *string
	pgtest.Scan(t, `SELECT status, failure_code FROM records.archiving_run WHERE id = $1`, []any{id}, &status, &code)
	if code != nil {
		return status + " " + *code
	}
	return status
}

// store is an object store whose writes or reads fail.
type store struct {
	blobstore.Store
	put, get error
	suffix   string // the keys whose writes fail; "": every key
}

func (s store) Put(ctx context.Context, key string, content []byte, opts blobstore.PutOptions) error {
	if s.put != nil && strings.HasSuffix(key, s.suffix) {
		return s.put
	}
	return s.Store.Put(ctx, key, content, opts)
}

func (s store) Get(ctx context.Context, key string) ([]byte, string, error) {
	if s.get != nil {
		return nil, "", s.get
	}
	return s.Store.Get(ctx, key)
}

// TestArchiverRefuses: a requester blocked since the request, a year
// opened since, other bytes under the run's key and debits too large to
// add up fail the run with their code, audited; nothing is published.
func TestArchiverRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		before func(t *testing.T, e archiveEnv, id string)
		change func(w *records.Archiver)
		want   string
	}{
		"the requester blocked": {before: func(t *testing.T, e archiveEnv, _ string) {
			t.Helper()
			pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, e.Tenant.Human.ID)
		}, want: "failed identity.principal_blocked"},
		"the year opened": {before: func(t *testing.T, e archiveEnv, _ string) {
			t.Helper()
			pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
				AND fiscal_year = 2026 AND period = 12`, e.Tenant.ID, e.DE)
		}, want: "failed records.year_not_closed"},
		"another file under its key": {before: func(t *testing.T, e archiveEnv, id string) {
			t.Helper()
			if err := e.objects.Put(context.Background(), blobstore.Key(e.Tenant.ID, blobstore.KindArchives, id+".jsonl"), []byte("{}\n"),
				blobstore.PutOptions{}); err != nil {
				t.Fatal(err)
			}
		}, want: "failed records.archive_changed"},
		"debits too large": {change: func(w *records.Archiver) { w.Endpoints.Journal = huge{} }, want: "failed records.amount_too_large"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setupArchive(t, false)
			id := e.queue(t)
			if c.before != nil {
				c.before(t, e, id)
			}
			w := e.archiver()
			w.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
			if c.change != nil {
				c.change(w)
			}
			if _, err := w.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := settled(t, id); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
			var audited int
			pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'records.archiving_run.fail' AND object_id = $2`,
				[]any{e.Tenant.ID, id}, &audited)
			if audited != 1 {
				t.Errorf("audit records: got %d, want 1", audited)
			}
			if evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "records", Name: "archiving_run_completed"}); len(evs) != 0 {
				t.Errorf("events: got %v, want none", evs)
			}
		})
	}
}

// huge is a journal whose one entry's debits add up to more than an
// amount holds.
type huge struct{ ledger.Archive }

func (huge) Entries(_ context.Context, _ uuid.UUID, _ int16, fn func(ledger.ArchivedEntry) error) error {
	return fn(ledger.ArchivedEntry{ID: uuid.Must(uuid.NewV7()), Currency: "EUR", LocalCurrency: "EUR", GroupCurrency: "EUR",
		Lines: []ledger.ArchivedLine{{LineNo: 1, AmountLocal: math.MaxInt64}, {LineNo: 2, AmountLocal: 1}}})
}

// TestArchiverWaits: the store not taking or not returning the file, and
// a company the tenancy context no longer answers for, leave the run
// queued for the next round, which completes it once they answer.
func TestArchiverWaits(t *testing.T) {
	e := setupArchive(t, false)
	id := e.queue(t)
	down := errors.New("store down")
	for name, change := range map[string]func(w *records.Archiver){
		"the store refusing the file": func(w *records.Archiver) { w.Endpoints.Objects = store{Store: e.objects, put: down} },
		"the store not returning a file": func(w *records.Archiver) {
			w.Endpoints.Objects = store{Store: e.objects, put: blobstore.ErrExists, get: down}
		},
		"the store refusing the manifest": func(w *records.Archiver) {
			w.Endpoints.Objects = store{Store: e.objects, put: down, suffix: ".manifest.json"}
		},
		"no company":           func(w *records.Archiver) { w.Endpoints.LegalEntities = entities{} },
		"the company not read": func(w *records.Archiver) { w.Endpoints.LegalEntities = entities{err: down} },
	} {
		t.Run(name, func(t *testing.T) {
			w := e.archiver()
			change(w)
			if _, err := w.RunOnce(context.Background()); err == nil {
				t.Error("got no error, want the round to fail")
			}
			if got := settled(t, id); got != records.RunQueued {
				t.Errorf("got %s, want queued", got)
			}
		})
	}
	e.round(t)
	if got := settled(t, id); got != records.RunCompleted {
		t.Errorf("once they answer: got %s, want completed", got)
	}
}

// entities is a tenancy context that does not hold the company, or
// cannot be read.
type entities struct{ err error }

func (x entities) Entity(context.Context, uuid.UUID) (tenancy.Entity, bool, error) {
	return tenancy.Entity{}, false, x.err
}

// loseCommit begins the tenant's transactions so that their commit is
// lost, as when the acknowledgement of a commit never came.
func loseCommit(t *testing.T) database.BeginFunc {
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

// TestArchiverRunTwice: a round whose commit is lost after it wrote the
// file leaves the run queued; the next round finds the same bytes under
// its keys and completes the run, with one file and one event (ADR-0012:
// idempotent activities).
func TestArchiverRunTwice(t *testing.T) {
	e := setupArchive(t, false)
	id := e.queue(t)
	lost := e.archiver()
	lost.Begin = loseCommit(t)
	if _, err := lost.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("the lost commit: got %v, want the injected failure", err)
	}
	if got, objects := settled(t, id), e.objectsIn(t); got != records.RunQueued || len(objects) != 2 {
		t.Fatalf("after the lost commit: got %s with %v, want queued with the file and manifest written", got, objects)
	}
	e.round(t)
	e.round(t)
	if got, objects := settled(t, id), e.objectsIn(t); got != records.RunCompleted || len(objects) != 2 {
		t.Errorf("after two more rounds: got %s with %v, want completed with one file and its manifest", got, objects)
	}
	if evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "records", Name: "archiving_run_completed"}); len(evs) != 1 {
		t.Errorf("events: got %d, want 1", len(evs))
	}
}

// TestArchiverDatabaseFailures: each of the round's statements failing in
// turn fails the round and leaves the run queued, until a round that
// fails none completes it; the tenants not listed, or not begun, fail the
// round too; a stopped worker returns.
func TestArchiverDatabaseFailures(t *testing.T) {
	e := setupArchive(t, false)
	id := e.queue(t)
	pool := pgtest.Pool(t)
	done := false
	for failAt := 1; failAt <= 200 && !done; failAt++ {
		count := 0
		w := e.archiver()
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
		done = err == nil && count < failAt && settled(t, id) == records.RunCompleted
	}
	if !done {
		t.Fatal("still failing after 200 statements")
	}
	refused := setupArchive(t, false) // a run the archiver fails: its failure's statements fail in turn too
	failing := refused.queue(t)
	done = false
	for failAt := 1; failAt <= 200 && !done; failAt++ {
		count := 0
		w := refused.archiver()
		w.Endpoints.Journal = huge{}
		w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: failAt, Count: &count}, nil
		}
		_, err := w.RunOnce(context.Background())
		if err != nil && !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("refused, statement %d: got %v, want the injected failure", failAt, err)
		}
		done = err == nil && count < failAt && settled(t, failing) == "failed records.amount_too_large"
	}
	if !done {
		t.Fatal("refused: still failing after 200 statements")
	}
	w := e.archiver()
	w.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected }
	if _, err := w.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("no transaction: got %v, want the injected failure", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.archiver().Run(ctx) // returns once the context has ended
}

// TestArchivingRunFile: a run not completed has no file; a file the store
// no longer holds as written is refused; a server without object storage,
// or whose store does not answer, is unavailable; the file is the
// ledger's to read too.
func TestArchivingRunFile(t *testing.T) {
	e := setupArchive(t, false)
	id := e.queue(t)
	if code, _ := e.file(t, id); code != http.StatusConflict {
		t.Errorf("a queued run's file: got %d, want 409", code)
	}
	e.round(t)
	plain := apitest.Client{T: t, API: apitest.NewWith(t, nil), Tenant: e.Tenant, As: e.Tenant.Human}
	plain.Problem(503, "platform.unavailable", "GET", "/v1/archiving-runs/"+id+"/file", "", "")
	gone := apitest.Client{T: t, API: apitest.NewWith(t, func(c *app.Config) { c.Objects = store{Store: e.objects, get: errors.New("down")} }),
		Tenant: e.Tenant, As: e.Tenant.Human}
	gone.Problem(503, "platform.unavailable", "GET", "/v1/archiving-runs/"+id+"/file", "", "")
	path := filepath.Join(e.objects.Root, blobstore.Key(e.Tenant.ID, blobstore.KindArchives, id+".jsonl"))
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.as().Problem(409, "records.archive_changed", "GET", "/v1/archiving-runs/"+id+"/file", "", "")
	e.as().Problem(404, "request.not_found", "GET", "/v1/archiving-runs/"+uuid.Must(uuid.NewV7()).String()+"/file", "", "")
}

// TestArchivingRunsListed: the runs of the companies the caller may act
// in, newest first, filtered and paged; another company's are neither
// listed nor found.
func TestArchivingRunsListed(t *testing.T) {
	e := setupArchive(t, false)
	first := e.queue(t)
	pgtest.Exec(t, `UPDATE records.archiving_run SET status = 'failed', failure_code = 'records.year_not_closed', version = version + 1 WHERE id = $1`, first)
	second := e.queue(t)
	e.round(t)
	c := e.as()
	for _, q := range []struct{ query, want string }{
		{"", second + " " + first},
		{"?status=failed", first},
		{"?status=completed&document_kind=journal_entry&fiscal_year=2026&legal_entity_id=" + e.DE.String(), second},
		{"?fiscal_year=2025", ""},
		{"?legal_entity_id=" + e.US.String(), ""},
	} {
		t.Run(q.query, func(t *testing.T) {
			got, _ := c.In(t).Must(200, "GET", "/v1/archiving-runs"+q.query, "", "")
			if ids := idsOf(got); ids != q.want {
				t.Errorf("got %s, want %s", ids, q.want)
			}
		})
	}
	c.Problem(400, "request.invalid_page_token", "GET", "/v1/archiving-runs?page_token=nonsense", "", "")
	page, _ := c.Must(200, "GET", "/v1/archiving-runs?page_size=1", "", "")
	next, _ := c.Must(200, "GET", "/v1/archiving-runs?page_size=1&page_token="+page["next_page_token"].(string), "", "")
	if got := idsOf(page) + " " + idsOf(next); got != second+" "+first {
		t.Errorf("pages: got %s, want %s %s", got, second, first)
	}
	reader := e.Reader(t, "Rita Reader", e.US)
	other := apitest.Client{T: t, API: e.api, Tenant: e.Tenant, As: reader}
	if got, _ := other.Must(200, "GET", "/v1/archiving-runs", "", ""); idsOf(got) != "" {
		t.Errorf("another company's caller: got %s, want none", idsOf(got))
	}
	other.Problem(404, "request.not_found", "GET", "/v1/archiving-runs/"+second, "", "")
}

// idsOf are a page's run ids in order.
func idsOf(page map[string]any) string {
	var ids []string
	for _, it := range page["items"].([]any) {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	return strings.Join(ids, " ")
}

// TestArchivingDatabaseFailures calls each read and write outside a unit
// of work and with each of its database statements failing in turn.
func TestArchivingDatabaseFailures(t *testing.T) {
	e := setupArchive(t, false)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	ep := app.Records(app.Config{Now: func() time.Time { return *e.now }, Objects: e.objects, Pages: pages})
	create := func(ctx context.Context) error {
		_, err := ep.CreateArchivingRun(ctx, &gen.ArchivingRunCreate{LegalEntityID: e.DE, DocumentKind: "journal_entry", FiscalYear: 2026}, gen.CreateArchivingRunParams{})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "a run queued", create)
	id := e.queue(t) // the earlier calls' runs were rolled back
	e.round(t)
	run := uuid.MustParse(id)
	calls := map[string]func(ctx context.Context) error{
		"a run answered": create,
		"a run read": func(ctx context.Context) error {
			_, err := ep.GetArchivingRun(ctx, gen.GetArchivingRunParams{ID: run})
			return err
		},
		"runs listed": func(ctx context.Context) error {
			_, err := ep.ListArchivingRuns(ctx, gen.ListArchivingRunsParams{})
			return err
		},
		"a file read": func(ctx context.Context) error {
			_, err := ep.GetArchivingRunFile(ctx, gen.GetArchivingRunFileParams{ID: run})
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
	bad := app.Records(app.Config{Pages: pages})
	bad.LegalEntities = entities{}
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := bad.Run(ctx, run)
		return err
	}); err == nil || !strings.Contains(err.Error(), "not held by the tenancy context") {
		t.Errorf("a company the tenancy context does not hold: got %v", err)
	}
	bad.People = people{}
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := bad.Run(ctx, run)
		return err
	}); err == nil || !strings.Contains(err.Error(), "requesters") {
		t.Errorf("requesters not named: got %v", err)
	}
	bad.LegalEntities = entities{err: errors.New("down")}
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := bad.Run(ctx, run)
		return err
	}); err == nil || !strings.Contains(err.Error(), "down") {
		t.Errorf("a company not read: got %v", err)
	}
}

// people is an identity context that cannot be read.
type people struct{}

func (people) PersonNames(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
	return nil, errors.New("identity down")
}

// TestArchiveYearLocked: the check that a year is closed holds its
// periods FOR SHARE until the transaction ends, so a reopening, which
// takes a period FOR UPDATE, cannot come between the check and the read
// of the year's entries (ADR-0008).
func TestArchiveYearLocked(t *testing.T) {
	e := setupArchive(t, false)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		if closed, err := (ledger.Archive{}).YearClosed(ctx, e.DE, 2026); err != nil || !closed {
			t.Fatalf("got closed %v, %v, want closed", closed, err)
		}
		other, err := pgtest.Pool(t).Begin(context.Background(), database.Scope{Role: "erp_app", TenantID: e.Tenant.ID})
		if err != nil {
			return err
		}
		defer func() { _ = other.Rollback(context.Background()) }() // the probe changes nothing
		_, err = other.Exec(context.Background(), `SELECT 1 FROM ledger.period_status WHERE legal_entity_id = $1 AND fiscal_year = 2026 AND period = 12
			FOR UPDATE NOWAIT`, e.DE)
		if err == nil || !strings.Contains(err.Error(), "could not obtain lock") {
			t.Errorf("a reopening's lock while the year is checked: got %v, want it refused", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestArchivingRunRace: two requests for a year at once queue one run:
// the second waits for the first's insert and answers its run.
func TestArchivingRunRace(t *testing.T) {
	e := setupArchive(t, false)
	ep := e.archiver().Endpoints
	request := func(ctx context.Context) (records.Run, bool, error) {
		return ep.RequestRun(ctx, e.DE, "journal_entry", 2026)
	}
	held, release := make(chan struct{}), make(chan struct{})
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	letGo := sync.OnceFunc(func() { close(release) })
	defer letGo()
	var first, second records.Run
	var created bool
	hold := sync.OnceFunc(func() { close(held) })
	go func() {
		err := errors.New("transaction setup failed")
		defer func() { hold(); firstDone <- err }() // the main goroutine never waits on a body that did not run
		err = apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			var err error
			first, _, err = request(ctx)
			hold()
			<-release
			return err
		})
	}()
	<-held
	go func() {
		err := errors.New("transaction setup failed")
		defer func() { secondDone <- err }()
		err = apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			var err error
			second, created, err = request(ctx)
			return err
		})
	}()
	admin := pgtest.Admin(t)
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var n int
		if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.query LIKE '%INSERT INTO records.archiving_run%'`).Scan(&n); err != nil {
			t.Fatalf("read the lock waits: %v", err)
		}
		return n > 0
	}, "the second request did not wait for the first's insert")
	letGo()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil || created || second.ID != first.ID {
		t.Errorf("the second request: got %v, created %v, run %s, want the first's run %s", err, created, second.ID, first.ID)
	}
}

// TestArchiveJournal: the ledger's read of a year for an archive: a year
// without periods is not closed, an error of the caller's stops the read
// at any entry, and both need a unit of work.
func TestArchiveJournal(t *testing.T) {
	e := setupArchive(t, false)
	stop := errors.New("stop")
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		if closed, err := (ledger.Archive{}).YearClosed(ctx, e.DE, 1999); err != nil || closed {
			t.Errorf("a year without periods: got closed %v, %v, want not closed", closed, err)
		}
		for _, at := range []int{1, 2} {
			n := 0
			err := ledger.Archive{}.Entries(ctx, e.DE, 2026, func(ledger.ArchivedEntry) error {
				if n++; n == at {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Errorf("stopped at entry %d: got %v, want the caller's error", at, err)
			}
		}
		return ledger.Archive{}.Entries(ctx, e.DE, 1999, func(ledger.ArchivedEntry) error { return stop })
	})
	if err != nil {
		t.Errorf("a year without entries: got %v, want no call", err)
	}
	if _, err := (ledger.Archive{}).YearClosed(context.Background(), e.DE, 2026); err == nil {
		t.Error("closed outside a unit of work: got nil error")
	}
	if err := (ledger.Archive{}).Entries(context.Background(), e.DE, 2026, nil); err == nil {
		t.Error("entries outside a unit of work: got nil error")
	}
}
