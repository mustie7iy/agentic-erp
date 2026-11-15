package records_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/records"
)

// blockedOn is the moment the deletion tests block a partner's personal
// data: noon UTC, the same day in every company of the fixture.
const blockedOn = "2026-01-10T12:00:00Z"

// deletionEnv is a tenant whose companies keep personal data for the
// years given (absent: no rule), and the day its endpoints and deleter
// run on.
type deletionEnv struct {
	fixture.Env
	now *time.Time
}

func setupDeletion(t *testing.T, rules map[string]int) deletionEnv {
	t.Helper()
	env := fixture.NewWith(t, nil, func(c *config.Config) {
		for code, y := range rules {
			c.Company(code).Retention = []config.RetentionRule{{Kind: config.RetentionPersonalData, Years: years(y)}}
		}
	}, "records.privacy.delete", "partner.supplier.read")
	now, _ := time.Parse(time.RFC3339, blockedOn) // the constant parses
	return deletionEnv{Env: env, now: &now}
}

// on sets the day the endpoints and the deleter run on, at noon UTC.
func (e deletionEnv) on(day string) {
	d, _ := time.Parse(time.DateOnly, day) // the tests' days parse
	*e.now = d.Add(12 * time.Hour)
}

// block blocks the partner's personal data at blockedOn, as
// partner.BlockPersonalData leaves it.
func (e deletionEnv) block(t *testing.T, id uuid.UUID) {
	t.Helper()
	pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'blocked', personal_data_blocked_at = $2, version = version + 1 WHERE id = $1`, id, blockedOn)
}

// endpoints is the context as the API wires it, on the test's day.
func (e deletionEnv) endpoints() *records.Endpoints {
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32))) // a 32-byte key is valid
	return app.Records(app.Config{Now: func() time.Time { return *e.now }, FieldKeys: apitest.FieldKeys(), Pages: pages})
}

// deleter is the worker as the server wires it, on the test's day.
func (e deletionEnv) deleter() *records.Deleter {
	return &records.Deleter{Endpoints: e.endpoints(), Requesters: identity.Actors{}, Pool: pgtest.Pool(e.T), AppRole: "erp_app", WorkerRole: "erp_worker",
		Now: func() time.Time { return *e.now }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// eligible are the numbers of the subjects a run started now blanks.
func (e deletionEnv) eligible(t *testing.T) string {
	t.Helper()
	var got []records.Subject
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		got, _, err = e.endpoints().Eligible(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(got))
	for i, s := range got {
		out[i] = fmt.Sprintf("%s %d %s", s.Number, s.Years, s.Ends.Format(time.DateOnly))
	}
	return strings.Join(out, ", ")
}

// request starts a run, committed, and returns it and whether it is new.
func (e deletionEnv) request(t *testing.T) (records.DeletionRun, bool, error) {
	t.Helper()
	var r records.DeletionRun
	var created bool
	err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		r, created, err = e.endpoints().RequestDeletion(ctx, "Retention for personal data ended")
		return err
	})
	return r, created, err
}

// read reads a run.
func (e deletionEnv) read(t *testing.T, id uuid.UUID) records.DeletionRun {
	t.Helper()
	var r records.DeletionRun
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		r, err = e.endpoints().DeletionRun(ctx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

// subjects are a run's subjects as "number status", read a page of one
// at a time, and its counts as "pending blanked skipped".
func (e deletionEnv) subjects(t *testing.T, r records.DeletionRun) string {
	t.Helper()
	var out []string
	token := ""
	for {
		var page []records.Subject
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
			page, token, err = e.endpoints().Subjects(ctx, r.ID, "", pagination.Request{PageSize: 1, PageSizeSet: true, PageToken: token, PageTokenSet: token != ""})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, s := range page {
			out = append(out, s.Number+" "+s.Status)
		}
		if token == "" {
			break
		}
	}
	return fmt.Sprintf("%s (%d %d %d)", strings.Join(out, ", "), r.Pending, r.Blanked, r.Skipped)
}

// TestDeletionEligible (T100c, ADR-0079 §12): a partner is eligible once
// its block has lasted the whole years of the personal data rule of every
// company it has data in, counted from the block's day; a company without
// a rule keeps it, as does a partner in no company; one blanked already
// is not listed again.
func TestDeletionEligible(t *testing.T) {
	for _, c := range []struct {
		name  string
		rules map[string]int
		day   string
		want  string
	}{
		{"no rule", nil, "2040-01-01", ""},
		{"a rule in one of its companies only", map[string]int{"2000": 0}, "2040-01-01", ""},
		{"zero years, the day of the block", map[string]int{"1000": 0, "2000": 0}, "2026-01-10", "BP90000001 0 2026-01-10"},
		{"a year, the day before it ends", map[string]int{"1000": 1, "2000": 0}, "2027-01-09", ""},
		{"a year, the day it ends", map[string]int{"1000": 1, "2000": 0}, "2027-01-10", "BP90000001 1 2027-01-10"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setupDeletion(t, c.rules)
			e.block(t, e.Supplier)
			e.on(c.day)
			if got := e.eligible(t); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	t.Run("no company, blanked already", func(t *testing.T) {
		e := setupDeletion(t, map[string]int{"1000": 0, "2000": 0})
		e.block(t, e.Supplier)
		e.block(t, e.Vendor)
		pgtest.Exec(t, `DELETE FROM partner.supplier_company WHERE partner_id = $1`, e.Vendor)
		pgtest.Exec(t, `UPDATE partner.business_partner SET personal_data_deleted_at = now(), version = version + 1 WHERE id = $1`, e.Supplier)
		if got := e.eligible(t); got != "" {
			t.Errorf("got %q, want none", got)
		}
	})
}

// TestDeletionRun (T100c acceptance): the run lists the preview's
// subjects; the deleter blanks each in a transaction of its own, audited
// and published by ids only, skips one whose block was lifted since, and
// completes the run; a second request while it is queued answers it, and
// once it completed there is nothing left to delete. A second round
// changes nothing (run twice). Ledger documents and audit events stay.
func TestDeletionRun(t *testing.T) {
	e := setupDeletion(t, map[string]int{"1000": 0, "2000": 0})
	e.block(t, e.Supplier)
	e.block(t, e.Vendor)
	entry := e.PostedBy(t, "ledger", "2026-01-05", 10000)
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1`, []any{e.Tenant.ID}, &audits)
	preview := e.eligible(t)
	r, created, err := e.request(t)
	if err != nil || !created {
		t.Fatalf("request: got %v, created %v", err, created)
	}
	if got := fmt.Sprint(r.Status, " ", r.Count, " ", e.subjects(t, r), " ", r.RequestedByName != ""); got != "queued 2 BP90000001 pending, BP90000002 pending (2 0 0) true" {
		t.Errorf("queued: got %s", got)
	}
	if want := "BP90000001 0 2026-01-10, BP90000002 0 2026-01-10"; preview != want {
		t.Errorf("preview: got %s, want %s", preview, want)
	}
	again, created, err := e.request(t)
	if err != nil || created || again.ID != r.ID {
		t.Errorf("a second request: got %s created %v (%v), want the run queued", again.ID, created, err)
	}
	pgtest.Exec(t, `UPDATE partner.business_partner SET personal_data_blocked_at = NULL, version = version + 1 WHERE id = $1`, e.Vendor) // lifted since
	for range 2 {
		if _, err := e.deleter().RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	done := e.read(t, r.ID)
	if got := fmt.Sprint(done.Status, " ", e.subjects(t, done), " ", done.CompletedAt != nil); got != "completed BP90000001 blanked, BP90000002 skipped (0 1 1) true" {
		t.Errorf("settled: got %s", got)
	}
	var name string
	var deleted bool
	pgtest.Scan(t, `SELECT name, personal_data_deleted_at IS NOT NULL FROM partner.business_partner WHERE id = $1`, []any{e.Supplier}, &name, &deleted)
	if name != partner.DeletedText || !deleted {
		t.Errorf("the supplier: got %s, deleted %v; want %s, deleted", name, deleted, partner.DeletedText)
	}
	evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "records", Name: "personal_data_deleted"})
	if len(evs) != 1 || evs[0]["partner_id"] != e.Supplier.String() || evs[0]["deletion_run_id"] != r.ID.String() || len(evs[0]) != 3 {
		t.Errorf("PersonalDataDeleted: got %v, want one for the supplier by ids", evs)
	}
	var after, entries int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action NOT LIKE 'records.deletion_run.%'`, []any{e.Tenant.ID}, &after)
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1 AND id = $2`, []any{e.Tenant.ID, entry}, &entries)
	if after < audits || entries != 1 {
		t.Errorf("audit events %d (before %d), the entry %d: want every audit event and the entry kept", after, audits, entries)
	}
	var steps int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action IN ('records.deletion_run.blank', 'records.deletion_run.skip',
		'records.deletion_run.complete')`, []any{e.Tenant.ID}, &steps)
	if steps != 3 {
		t.Errorf("audit records of the run's steps: got %d, want 3", steps)
	}
	if _, _, err := e.request(t); problem.From(err).Code != "records.nothing_to_delete" {
		t.Errorf("after the run: got %v, want records.nothing_to_delete", err)
	}
}

// TestDeleterRunTwice: a step whose commit is lost leaves its subject
// pending; the next round blanks it once, with one event (ADR-0012).
func TestDeleterRunTwice(t *testing.T) {
	e := setupDeletion(t, map[string]int{"1000": 0, "2000": 0})
	e.block(t, e.Supplier)
	r, _, err := e.request(t)
	if err != nil {
		t.Fatal(err)
	}
	lost := e.deleter()
	lost.Begin = loseCommit(t)
	if _, err := lost.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("the lost commit: got %v, want the injected failure", err)
	}
	if got := e.subjects(t, e.read(t, r.ID)); got != "BP90000001 pending (1 0 0)" {
		t.Fatalf("after the lost commit: got %s, want pending", got)
	}
	for range 2 {
		if _, err := e.deleter().RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := fmt.Sprint(e.read(t, r.ID).Status, " ", e.subjects(t, e.read(t, r.ID))); got != "completed BP90000001 blanked (0 1 0)" {
		t.Errorf("after two rounds: got %s", got)
	}
	if evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "records", Name: "personal_data_deleted"}); len(evs) != 1 {
		t.Errorf("events: got %d, want 1", len(evs))
	}
}

// TestDeleterRefuses: a requester blocked since the request fails the
// run, audited; nothing is blanked.
func TestDeleterRefuses(t *testing.T) {
	e := setupDeletion(t, map[string]int{"1000": 0, "2000": 0})
	e.block(t, e.Supplier)
	r, _, err := e.request(t)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, e.Tenant.Human.ID)
	if _, err := e.deleter().RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status, code, name string
	pgtest.Scan(t, `SELECT status, coalesce(failure_code, '') FROM records.deletion_run WHERE id = $1`, []any{r.ID}, &status, &code)
	pgtest.Scan(t, `SELECT name FROM partner.business_partner WHERE id = $1`, []any{e.Supplier}, &name)
	if status+" "+code != "failed identity.principal_blocked" || name == partner.DeletedText {
		t.Errorf("got %s %s, the supplier %s; want failed identity.principal_blocked, not blanked", status, code, name)
	}
}

// TestDeletionAPI: the preview, the run and its read through the API, to
// the person who may act in every company and to no one else; agents are
// refused; a run is not found once it is another tenant's.
func TestDeletionAPI(t *testing.T) {
	e := setupDeletion(t, map[string]int{"1000": 0, "2000": 0})
	pgtest.Exec(t, `UPDATE partner.business_partner SET status = 'blocked', personal_data_blocked_at = now(), version = version + 1 WHERE id = ANY($1)`,
		[]uuid.UUID{e.Supplier, e.Vendor})
	preview, _ := e.Must(200, "GET", "/v1/personal-data-deletion-preview?page_size=1", "", "")
	if items := preview["items"].([]any); preview["count"] != float64(2) || len(items) != 1 || items[0].(map[string]any)["partner_number"] != "BP90000001" {
		t.Errorf("preview: got %v, want the first of two", preview)
	}
	rest, _ := e.Must(200, "GET", "/v1/personal-data-deletion-preview?page_size=1&page_token="+preview["next_page_token"].(string), "", "")
	if items := rest["items"].([]any); len(items) != 1 || items[0].(map[string]any)["partner_number"] != "BP90000002" || rest["next_page_token"] != nil {
		t.Errorf("preview's second page: got %v, want the second and last", rest)
	}
	created, _ := e.Must(201, "POST", "/v1/personal-data-deletion-runs", `{"reason": "Retention ended"}`, "")
	again, _ := e.Must(200, "POST", "/v1/personal-data-deletion-runs", `{"reason": "Retention ended"}`, "")
	got, _ := e.Must(200, "GET", "/v1/personal-data-deletion-runs/"+created["id"].(string), "", "")
	if again["id"] != created["id"] || got["status"] != "queued" || got["subject_count"] != float64(2) || got["pending_count"] != float64(2) {
		t.Errorf("run: created %v, again %v, read %v", created, again, got)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/personal-data-deletion-runs/"+uuid.NewString(), "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/personal-data-deletion-runs/"+uuid.NewString()+"/subjects", "", "")
	page, _ := e.Must(200, "GET", "/v1/personal-data-deletion-runs/"+created["id"].(string)+"/subjects?status=pending&page_size=2", "", "")
	if items := page["items"].([]any); len(items) != 2 || items[0].(map[string]any)["status"] != "pending" || page["next_page_token"] != nil {
		t.Errorf("subjects: got %v", page)
	}
	none, _ := e.Must(200, "GET", "/v1/personal-data-deletion-runs/"+created["id"].(string)+"/subjects?status=blanked", "", "")
	if items := none["items"].([]any); len(items) != 0 {
		t.Errorf("blanked subjects: got %v, want none", items)
	}
	// Page sizes are checked by the handlers too, for callers other than the generated router.
	ep, zero, run := e.endpoints(), gen.NewOptInt32(0), uuid.MustParse(created["id"].(string))
	for name, call := range map[string]func(ctx context.Context) error{
		"preview": func(ctx context.Context) error {
			_, err := ep.GetPersonalDataDeletionPreview(ctx, gen.GetPersonalDataDeletionPreviewParams{PageSize: zero})
			return err
		},
		"subjects": func(ctx context.Context) error {
			_, err := ep.ListPersonalDataDeletionSubjects(ctx, gen.ListPersonalDataDeletionSubjectsParams{ID: run, PageSize: zero})
			return err
		},
	} {
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call); problem.From(err).Code != "request.invalid_page_size" {
			t.Errorf("%s, page_size 0: got %v, want request.invalid_page_size", name, err)
		}
	}
	pgtest.Exec(t, `UPDATE identity.principal SET all_companies = false, version = version + 1 WHERE id = $1`, e.Tenant.Human.ID)
	e.Problem(403, "records.all_companies_required", "GET", "/v1/personal-data-deletion-preview", "", "")
	e.Problem(403, "records.all_companies_required", "GET", "/v1/personal-data-deletion-runs/"+created["id"].(string), "", "")
	e.Problem(403, "records.all_companies_required", "POST", "/v1/personal-data-deletion-runs", `{"reason": "r"}`, "")
	e.Problem(403, "records.all_companies_required", "GET", "/v1/personal-data-deletion-runs/"+created["id"].(string)+"/subjects", "", "")
	agent := e.As(e.Tenant.Agent)
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/personal-data-deletion-runs", `{"reason": "r"}`, "")
}

// TestDeletionDatabaseFailures: every database statement of the preview,
// the request and the read failing in turn fails the call, as does each
// port not answering; the deleter's statements failing in turn leave the
// run queued until a round that fails none settles it.
func TestDeletionDatabaseFailures(t *testing.T) {
	e := setupDeletion(t, map[string]int{"1000": 0, "2000": 0})
	e.block(t, e.Supplier)
	ep := e.endpoints()
	calls := map[string]func(ctx context.Context) error{
		"preview": func(ctx context.Context) error {
			_, err := ep.GetPersonalDataDeletionPreview(ctx, gen.GetPersonalDataDeletionPreviewParams{})
			return err
		},
		"request": func(ctx context.Context) error {
			_, err := ep.CreatePersonalDataDeletionRun(ctx, &gen.ReasonInput{Reason: "r"}, gen.CreatePersonalDataDeletionRunParams{})
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
	r, _, err := e.request(t)
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"answered": calls["request"],
		"read": func(ctx context.Context) error {
			_, err := ep.GetPersonalDataDeletionRun(ctx, gen.GetPersonalDataDeletionRunParams{ID: r.ID})
			return err
		},
		"subjects listed": func(ctx context.Context) error {
			_, err := ep.ListPersonalDataDeletionSubjects(ctx, gen.ListPersonalDataDeletionSubjectsParams{ID: r.ID, Status: gen.NewOptString("pending")})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	t.Run("ports", func(t *testing.T) { deletionPorts(t, e, r.ID) })
	pool := pgtest.Pool(t)
	done := false
	for failAt := 1; failAt <= 200 && !done; failAt++ {
		count := 0
		w := e.deleter()
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
		done = err == nil && count < failAt && e.read(t, r.ID).Status == records.RunCompleted
	}
	if !done {
		t.Fatal("still failing after 200 statements")
	}
	w := e.deleter()
	w.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected }
	if _, err := w.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("no transaction: got %v, want the injected failure", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.deleter().Run(ctx) // returns once the context has ended
}

// privacyDown is a partner context that does not answer.
type privacyDown struct{}

func (privacyDown) Blocked(context.Context) ([]partner.BlockedSubject, error) {
	return nil, errors.New("partner down")
}

func (privacyDown) Blank(context.Context, uuid.UUID, time.Time) (bool, error) {
	return false, errors.New("partner down")
}

// deletionPorts: the partner, tenancy and identity contexts not answering
// fail the preview, the read and the deleter's step, as does a company
// tenancy does not hold.
func deletionPorts(t *testing.T, e deletionEnv, run uuid.UUID) {
	t.Helper()
	call := func(ep *records.Endpoints, fn func(ctx context.Context, ep *records.Endpoints) error) error {
		return apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return fn(ctx, ep) })
	}
	eligible := func(ctx context.Context, ep *records.Endpoints) error { _, _, err := ep.Eligible(ctx); return err }
	for name, c := range map[string]struct {
		change func(ep *records.Endpoints)
		fn     func(ctx context.Context, ep *records.Endpoints) error
		want   string
	}{
		"the partner context":      {func(ep *records.Endpoints) { ep.Privacy = privacyDown{} }, eligible, "partner down"},
		"tenancy":                  {func(ep *records.Endpoints) { ep.LegalEntities = entities{err: errors.New("tenancy down")} }, eligible, "tenancy down"},
		"a company tenancy lacks":  {func(ep *records.Endpoints) { ep.LegalEntities = entities{} }, eligible, "not held by the tenancy context"},
		"the requester's name":     {func(ep *records.Endpoints) { ep.People = people{} }, func(ctx context.Context, ep *records.Endpoints) error { _, err := ep.DeletionRun(ctx, run); return err }, "identity down"},
		"the configuration":        {func(ep *records.Endpoints) { ep.Settings = noSettings{} }, eligible, "no configuration"},
		"a company without a zone": {func(ep *records.Endpoints) { ep.Settings = zoneless{ep.Settings} }, eligible, "date of company"},
	} {
		t.Run(name, func(t *testing.T) {
			ep := e.endpoints()
			c.change(ep)
			if err := call(ep, c.fn); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %s", err, c.want)
			}
		})
	}
	w := e.deleter()
	w.Endpoints.Privacy = privacyDown{}
	if _, err := w.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "partner down") {
		t.Errorf("blanking not answered: got %v", err)
	}
}

// noSettings is a configuration context that does not answer.
type noSettings struct{}

func (noSettings) Document(context.Context) (*config.Config, int32, error) {
	return nil, 0, errors.New("no configuration")
}

// zoneless is the configuration with company 1000's time zone one that
// does not load.
type zoneless struct{ records.Settings }

func (z zoneless) Document(ctx context.Context) (*config.Config, int32, error) {
	doc, v, err := z.Settings.Document(ctx)
	if err == nil {
		doc.Company("1000").TimeZone = "Local"
	}
	return doc, v, err
}

// failEachStatement runs the deleter with each of its statements failing
// in turn until a round that fails none leaves done true.
func failEachStatement(t *testing.T, e deletionEnv, done func() bool) {
	t.Helper()
	pool := pgtest.Pool(t)
	for failAt := 1; failAt <= 200; failAt++ {
		count := 0
		w := e.deleter()
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
		if err == nil && count < failAt && done() {
			return
		}
	}
	t.Fatal("still failing after 200 statements")
}

// TestDeleterFailures: a run's completion and its failure, each with its
// statements failing in turn, leave the run queued until a round that
// fails none settles it.
func TestDeleterFailures(t *testing.T) {
	t.Run("completion", func(t *testing.T) {
		e := setupDeletion(t, map[string]int{"1000": 0, "2000": 0})
		e.block(t, e.Supplier)
		r, _, err := e.request(t)
		if err != nil {
			t.Fatal(err)
		}
		pgtest.Exec(t, `UPDATE records.deletion_run_subject SET status = 'skipped', settled_at = now() WHERE run_id = $1`, r.ID)
		failEachStatement(t, e, func() bool { return e.read(t, r.ID).Status == records.RunCompleted })
	})
	t.Run("failure", func(t *testing.T) {
		e := setupDeletion(t, map[string]int{"1000": 0, "2000": 0})
		e.block(t, e.Supplier)
		r, _, err := e.request(t)
		if err != nil {
			t.Fatal(err)
		}
		pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, e.Tenant.Human.ID)
		failEachStatement(t, e, func() bool {
			var status string
			pgtest.Scan(t, `SELECT status FROM records.deletion_run WHERE id = $1`, []any{r.ID}, &status)
			return status == records.RunFailed
		})
	})
}

// TestDeletionRunRace: two requests at once queue one run: the second
// waits for the first's insert and answers its run.
func TestDeletionRunRace(t *testing.T) {
	e := setupDeletion(t, map[string]int{"1000": 0, "2000": 0})
	e.block(t, e.Supplier)
	ep := e.endpoints()
	held, release := make(chan struct{}), make(chan struct{})
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	letGo := sync.OnceFunc(func() { close(release) })
	defer letGo()
	var first, second records.DeletionRun
	var created bool
	hold := sync.OnceFunc(func() { close(held) })
	go func() {
		err := errors.New("transaction setup failed")
		defer func() { hold(); firstDone <- err }() // the main goroutine never waits on a body that did not run
		err = apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			var err error
			first, _, err = ep.RequestDeletion(ctx, "first")
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
			second, created, err = ep.RequestDeletion(ctx, "second")
			return err
		})
	}()
	admin := pgtest.Admin(t)
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var n int
		if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.query LIKE '%INSERT INTO records.deletion_run %'`).Scan(&n); err != nil {
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
