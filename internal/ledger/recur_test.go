package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// Recurring entries (ADR-0056): the ERP parks one entry per due date of a
// template's schedule for its owner, once however often it runs, and a
// person posts it.

// recurrer is the ERP's recurrer as a test actor.
var recurrer = pgtest.Actor{ID: ledger.RecurrerActor, Kind: reqctx.KindSystem}

type recurring struct {
	env
	h        *ledger.Endpoints
	now      time.Time
	template uuid.UUID
}

// recurringSetup saves a monthly template on day 1 from January 2026 and
// sets the clock to 15 February 2026.
func recurringSetup(t *testing.T) *recurring {
	t.Helper()
	e := setup(t)
	rent, bank := e.account(t, "2000", "400000"), e.account(t, "2000", "100000")
	tpl, _ := e.Must(201, "POST", "/v1/entry-templates", templateBody(e.de, "Office rent", `{"frequency": "monthly", "day_of_month": 1, "starts_on": "2026-01-01"}`,
		line{rent, "4500.00", ""}, line{bank, "-4500.00", ""}), "")
	r := &recurring{env: e, h: endpoints(), now: time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC), template: uuid.MustParse(tpl["id"].(string))}
	r.h.Now = func() time.Time { return r.now }
	return r
}

func (r *recurring) due(t *testing.T) []string {
	t.Helper()
	var out []ledger.Occurrence
	if err := apitest.Committed(t, r.ten, recurrer, func(ctx context.Context) (err error) {
		out, err = r.h.DueOccurrences(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dates := make([]string, len(out))
	for i, o := range out {
		dates[i] = o.TemplateID.String()[:4] + " " + o.Date.Format(time.DateOnly)
	}
	return dates
}

func (r *recurring) park(t *testing.T, template uuid.UUID, day string) string {
	t.Helper()
	var outcome string
	if err := apitest.Committed(t, r.ten, recurrer, func(ctx context.Context) (err error) {
		outcome, err = r.h.ParkOccurrence(ctx, template, date(day))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return outcome
}

func (r *recurring) parked(t *testing.T) []map[string]any {
	t.Helper()
	page, _ := r.Must(200, "GET", "/v1/parked-journal-entries?template_id="+r.template.String(), "", "")
	var out []map[string]any
	for _, it := range page["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func TestRecurringEntries(t *testing.T) {
	r := recurringSetup(t)
	if p := ledger.Recurrer(r.ten.ID); p.ID != ledger.RecurrerActor || p.Kind != reqctx.KindSystem || p.TenantID != r.ten.ID {
		t.Errorf("the recurrer: got %+v, want the system actor in the tenant", p)
	}
	short := r.template.String()[:4]
	if got := r.due(t); len(got) != 2 || got[0] != short+" 2026-01-01" || got[1] != short+" 2026-02-01" {
		t.Fatalf("due: got %v, want January and February", got)
	}
	tx, err := pgtest.Pool(t).Begin(context.Background(), database.Scope{Role: "erp_worker", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := ledger.RecurringTenants(context.Background(), tx, r.now)
	_ = tx.Rollback(context.Background()) // a read-only transaction: nothing to keep or lose
	if err != nil || !containsTenant(tenants, r.ten.ID) {
		t.Fatalf("tenants with templates due: got %v %v, want %s among them", tenants, err, r.ten.ID)
	}
	t.Run("parked once however often it runs", func(t *testing.T) {
		if got := r.park(t, r.template, "2026-01-01"); got != ledger.OccurrenceParked {
			t.Fatalf("first run: got %s, want parked", got)
		}
		if got := r.park(t, r.template, "2026-01-01"); got != ledger.OccurrenceSkipped {
			t.Errorf("second run: got %s, want skipped", got)
		}
		entries := r.parked(t)
		if len(entries) != 1 {
			t.Fatalf("entries: got %d, want 1", len(entries))
		}
		want(t, "recurring entry", entries[0], map[string]any{"created_by_kind": "system", "owner_id": r.ten.Human.ID.String(),
			"created_by": ledger.RecurrerActor.String(), "occurrence_date": "2026-01-01", "posting_date": "2026-01-01", "status": "parked",
			"template_id": r.template.String()})
		published := events(t, r.ten, "parked_journal_entry_changed")
		if len(published) != 1 {
			t.Fatalf("parked events: got %d, want 1", len(published))
		}
		want(t, "parked event", published[0], map[string]any{"template_id": r.template.String(), "occurrence_date": "2026-01-01",
			"owner_id": r.ten.Human.ID.String(), "created_by_kind": "system"})
		tpl, _ := r.Must(200, "GET", "/v1/entry-templates/"+r.template.String(), "", "")
		want(t, "template", tpl, map[string]any{"last_occurrence_on": "2026-01-01", "next_due_on": "2026-02-01"})
		mine, _ := r.Must(200, "GET", "/v1/parked-journal-entries?owned_by=me&status=parked", "", "")
		if n := len(mine["items"].([]any)); n != 1 {
			t.Errorf("the owner's entries: got %d, want 1", n)
		}
	})
	t.Run("skipped", func(t *testing.T) {
		for name, c := range map[string]struct {
			template uuid.UUID
			date     string
		}{
			"not due yet":         {r.template, "2026-03-01"},
			"not a date of it":    {r.template, "2026-02-02"},
			"an unknown template": {uuid.New(), "2026-02-01"},
		} {
			if got := r.park(t, c.template, c.date); got != ledger.OccurrenceSkipped {
				t.Errorf("%s: got %s, want skipped", name, got)
			}
		}
	})
	t.Run("posted by the owner in an open period, refused in a closed one", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = 2`, r.ten.ID, r.de)
		if got := r.park(t, r.template, "2026-02-01"); got != ledger.OccurrenceParked {
			t.Fatalf("a date in a closed period: got %s, want parked", got)
		}
		for _, pe := range r.parked(t) {
			path := "/v1/parked-journal-entries/" + pe["id"].(string) + ":post"
			if pe["posting_date"] == "2026-02-01" {
				r.Problem(409, "ledger.period_not_open", "POST", path, "", etagOf(pe))
				continue
			}
			posted, _ := r.Must(200, "POST", path, "", etagOf(pe))
			want(t, "posted", posted, map[string]any{"status": "posted"})
		}
	})
}

func containsTenant(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// TestRecurringRefusals: a date whose entry parking refuses is recorded on
// the template and passed over; the template keeps its latest refused
// date. A retired template, or one no person owns, parks nothing.
func TestRecurringRefusals(t *testing.T) {
	r := recurringSetup(t)
	r.now = time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	rent := r.account(t, "2000", "400000")
	pgtest.Exec(t, `UPDATE ledger.gl_account SET status = 'inactive', version = version + 1 WHERE id = $1`, rent)
	if got := r.park(t, r.template, "2026-03-01"); got != ledger.OccurrenceRefused {
		t.Fatalf("an inactive account: got %s, want refused", got)
	}
	if got := r.park(t, r.template, "2026-02-01"); got != ledger.OccurrenceRefused {
		t.Fatalf("an earlier date: got %s, want refused", got)
	}
	tpl, _ := r.Must(200, "GET", "/v1/entry-templates/"+r.template.String(), "", "")
	want(t, "template", tpl, map[string]any{"next_due_on": "2026-04-01", "last_occurrence_on": nil})
	want(t, "refusal", tpl["last_refusal"].(map[string]any), map[string]any{"occurrence_date": "2026-03-01", "code": "ledger.account_not_postable"})
	refusals := events(t, r.ten, "entry_template_changed")
	want(t, "refusal event", refusals[len(refusals)-1], map[string]any{"refused_on": "2026-03-01", "refusal_code": "ledger.account_not_postable"})
	if n := len(r.parked(t)); n != 0 {
		t.Errorf("entries: got %d, want none", n)
	}
	if got := r.due(t); len(got) != 1 || got[0][5:] != "2026-04-01" {
		t.Errorf("due after the refusals: got %v, want April only", got)
	}
	t.Run("a company no longer active", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.gl_account SET status = 'active', version = version + 1 WHERE id = $1`, rent)
		pgtest.Exec(t, `UPDATE tenancy.legal_entity SET status = 'blocked', version = version + 1 WHERE id = $1`, r.de)
		defer pgtest.Exec(t, `UPDATE tenancy.legal_entity SET status = 'active', version = version + 1 WHERE id = $1`, r.de)
		if got := r.park(t, r.template, "2026-04-01"); got != ledger.OccurrenceRefused {
			t.Errorf("got %s, want refused", got)
		}
	})
	t.Run("an account only goods receipts post", func(t *testing.T) {
		r.now = time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
		pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'gr_ir', $3, $4)`,
			r.ten.ID, r.de, rent, r.ten.Human.ID)
		defer pgtest.Exec(t, `DELETE FROM ledger.account_determination WHERE tenant_id = $1 AND key = 'gr_ir'`, r.ten.ID)
		if got := r.park(t, r.template, "2026-05-01"); got != ledger.OccurrenceRefused {
			t.Fatalf("got %s, want refused", got)
		}
		tpl, _ := r.Must(200, "GET", "/v1/entry-templates/"+r.template.String(), "", "")
		want(t, "refusal", tpl["last_refusal"].(map[string]any), map[string]any{"occurrence_date": "2026-05-01", "code": "ledger.automatic_account"})
	})
	t.Run("retired or ownerless", func(t *testing.T) {
		tpl, _ := r.Must(200, "GET", "/v1/entry-templates/"+r.template.String(), "", "")
		r.Must(200, "POST", "/v1/entry-templates/"+r.template.String()+":retire", "", etagOf(tpl))
		if got := r.park(t, r.template, "2026-06-01"); got != ledger.OccurrenceSkipped {
			t.Errorf("retired: got %s, want skipped", got)
		}
		bank, rent := r.account(t, "2000", "100000"), r.account(t, "2000", "400000")
		agent := r.asAgent(t)
		agent.Must(201, "POST", "/v1/entry-templates", templateBody(r.de, "Proposed", `{"frequency": "monthly", "day_of_month": 1, "starts_on": "2026-01-01"}`,
			line{rent, "1.00", ""}, line{bank, "-1.00", ""}), "")
		if got := r.due(t); len(got) != 0 {
			t.Errorf("due: got %v, want none", got)
		}
	})
}

// TestParkOccurrenceFailures: every statement of the activity failing in
// turn, outside a unit of work, and its savepoint failing to start or to
// be released.
func TestParkOccurrenceFailures(t *testing.T) {
	r := recurringSetup(t)
	call := func(ctx context.Context) error {
		_, err := r.h.ParkOccurrence(ctx, r.template, date("2026-01-01"))
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	if _, err := r.h.DueOccurrences(context.Background()); err == nil {
		t.Error("due outside a unit of work: got no error, want one")
	}
	for _, mode := range []string{"begin", "commit"} {
		t.Run("savepoint "+mode+" failing", func(t *testing.T) {
			ctx := context.Background()
			tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: r.ten.ID})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			p := reqctx.Principal{ID: ledger.RecurrerActor, Kind: reqctx.KindSystem, TenantID: r.ten.ID}
			err = uow.Act(ctx, savepointFails{Tx: tx, mode: mode}, p, nil, call)
			pgtest.CheckErr(t, err, "savepoint "+mode)
		})
	}
	apitest.EveryFailure(t, r.ten, recurrer, "park occurrence", call)
	apitest.EveryFailure(t, r.ten, recurrer, "due occurrences", func(ctx context.Context) error {
		_, err := r.h.DueOccurrences(ctx)
		return err
	})
	t.Run("recording a refusal", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.gl_account SET status = 'inactive', version = version + 1 WHERE id = $1`, r.account(t, "2000", "400000"))
		apitest.EveryFailure(t, r.ten, recurrer, "refuse occurrence", func(ctx context.Context) error {
			_, err := r.h.ParkOccurrence(ctx, r.template, date("2026-02-01"))
			return err
		})
	})
	t.Run("the worker's question", func(t *testing.T) {
		ctx := context.Background()
		tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_worker", ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := ledger.RecurringTenants(ctx, &pgtest.FailingTx{Tx: tx, FailAt: 1}, r.now); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("got %v, want the injected failure", err)
		}
	})
}

// savepointFails is a transaction whose savepoints fail to begin or to be
// released.
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
	return brokenRelease{Tx: sp}, nil
}

type brokenRelease struct{ pgx.Tx }

func (b brokenRelease) Commit(ctx context.Context) error {
	_ = b.Rollback(ctx) // the double fails the release; what rolling back says is beside the point
	return errors.New("savepoint commit failed")
}
