package ledger_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/identity"
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
	r.h.People = identity.Actors{}
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
	outcome, err := r.tryPark(t, template, day)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

// tryPark runs the activity's work for the template's date as the
// recurrer and returns what it did.
func (r *recurring) tryPark(t *testing.T, template uuid.UUID, day string) (string, error) {
	t.Helper()
	var outcome string
	err := apitest.Committed(t, r.ten, recurrer, func(ctx context.Context) (err error) {
		outcome, err = r.h.ParkOccurrence(ctx, template, date(day))
		return err
	})
	return outcome, err
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
	if got := r.due(t); len(got) != 1 || got[0] != short+" 2026-01-01" {
		t.Fatalf("due: got %v, want January, the next date, alone", got)
	}
	tx, err := pgtest.Pool(t).Begin(context.Background(), database.Scope{Role: "erp_worker", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := ledger.RecurringTenants(context.Background(), tx, r.now)
	_ = tx.Rollback(context.Background()) // a read-only transaction: nothing to keep or lose
	if err != nil || !slices.Contains(tenants, r.ten.ID) {
		t.Fatalf("tenants with templates due: got %v %v, want %s among them", tenants, err, r.ten.ID)
	}
	t.Run("dates in order", func(t *testing.T) {
		for name, c := range map[string]struct {
			day  string
			want error
		}{
			"a later date while January waits": {"2026-02-01", ledger.ErrEarlierDatePending},
			"a date off the schedule":          {"2026-02-02", ledger.ErrEarlierDatePending},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := r.tryPark(t, r.template, c.day); !errors.Is(err, c.want) {
					t.Errorf("got %v, want %v", err, c.want)
				}
			})
		}
	})
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
			"template_id": r.template.String(), "template_name": "Office rent"})
		published := events(t, r.ten, "parked_journal_entry_changed")
		if len(published) != 1 {
			t.Fatalf("parked events: got %d, want 1", len(published))
		}
		want(t, "parked event", published[0], map[string]any{"template_id": r.template.String(), "template_name": "Office rent",
			"occurrence_date": "2026-01-01", "owner_id": r.ten.Human.ID.String(), "created_by_kind": "system"})
		tpl, _ := r.Must(200, "GET", "/v1/entry-templates/"+r.template.String(), "", "")
		want(t, "template", tpl, map[string]any{"last_occurrence_on": "2026-01-01", "next_due_on": "2026-02-01"})
		mine, _ := r.Must(200, "GET", "/v1/parked-journal-entries?owned_by=me&status=parked", "", "")
		if n := len(mine["items"].([]any)); n != 1 {
			t.Errorf("the owner's entries: got %d, want 1", n)
		}
		if got := r.due(t); len(got) != 1 || got[0] != short+" 2026-02-01" {
			t.Errorf("due: got %v, want February", got)
		}
	})
	t.Run("skipped", func(t *testing.T) {
		for name, c := range map[string]struct {
			template uuid.UUID
			date     string
		}{
			"a date before the next": {r.template, "2025-12-01"},
			"an unknown template":    {uuid.New(), "2026-02-01"},
		} {
			t.Run(name, func(t *testing.T) {
				if got := r.park(t, c.template, c.date); got != ledger.OccurrenceSkipped {
					t.Errorf("got %s, want skipped", got)
				}
			})
		}
	})
	t.Run("not due yet on the company's day", func(t *testing.T) {
		r.now = time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC)
		defer func() { r.now = time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC) }()
		if _, err := r.tryPark(t, r.template, "2026-02-01"); !errors.Is(err, ledger.ErrNotDueYet) {
			t.Errorf("got %v, want %v", err, ledger.ErrNotDueYet)
		}
		if got := r.due(t); len(got) != 0 {
			t.Errorf("due: got %v, want none", got)
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

// TestRecurringRefusals: a date whose entry is refused is recorded on the
// template, published once however often the activity runs, and passed
// over; the next date goes on. A retired template, or one no person owns,
// parks nothing.
func TestRecurringRefusals(t *testing.T) {
	r := recurringSetup(t)
	r.now = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	rent := r.account(t, "2000", "400000")
	refuse := func(t *testing.T, day, code string) {
		t.Helper()
		if got := r.park(t, r.template, day); got != ledger.OccurrenceRefused {
			t.Fatalf("%s: got %s, want refused", day, got)
		}
		var on, got string
		pgtest.Scan(t, `SELECT refused_on::text, refusal_code FROM ledger.entry_template WHERE id = $1`, []any{r.template}, &on, &got)
		if on != day || got != code {
			t.Errorf("refusal: got %s %s, want %s %s", on, got, day, code)
		}
	}
	t.Run("an inactive account, run twice", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE ledger.gl_account SET status = 'inactive', version = version + 1 WHERE id = $1`, rent)
		refuse(t, "2026-01-01", "ledger.account_not_postable")
		if got := r.park(t, r.template, "2026-01-01"); got != ledger.OccurrenceSkipped {
			t.Errorf("second run: got %s, want skipped", got)
		}
		refusals := events(t, r.ten, "entry_template_changed")
		want(t, "refusal event", refusals[len(refusals)-1], map[string]any{"refused_on": "2026-01-01", "refusal_code": "ledger.account_not_postable"})
		var audited int
		pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'ledger.entry_template.refuse'`, []any{r.ten.ID}, &audited)
		if n := len(refusals); n != 2 || audited != 1 {
			t.Errorf("template events and refusal records: got %d and %d, want 2 (saved, refused) and 1", n, audited)
		}
		pgtest.Exec(t, `UPDATE ledger.gl_account SET status = 'active', version = version + 1 WHERE id = $1`, rent)
		if got := r.park(t, r.template, "2026-01-01"); got != ledger.OccurrenceSkipped {
			t.Errorf("after the account is active again: got %s, want skipped, the date passed over", got)
		}
		tpl, _ := r.Must(200, "GET", "/v1/entry-templates/"+r.template.String(), "", "")
		want(t, "template", tpl, map[string]any{"next_due_on": "2026-02-01", "last_occurrence_on": nil})
		tpl, _ = r.Must(200, "PATCH", "/v1/entry-templates/"+r.template.String(), `{"header_text": "Rent, lease 2"}`, etagOf(tpl))
		want(t, "changed after a refusal", tpl, map[string]any{"next_due_on": "2026-02-01"})
	})
	t.Run("a company no longer active", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE tenancy.legal_entity SET status = 'blocked', version = version + 1 WHERE id = $1`, r.de)
		defer pgtest.Exec(t, `UPDATE tenancy.legal_entity SET status = 'active', version = version + 1 WHERE id = $1`, r.de)
		refuse(t, "2026-02-01", "ledger.legal_entity_not_active")
	})
	t.Run("an account only goods receipts post", func(t *testing.T) {
		pgtest.Exec(t, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $2, 'gr_ir', $3, $4)`,
			r.ten.ID, r.de, rent, r.ten.Human.ID)
		defer pgtest.Exec(t, `DELETE FROM ledger.account_determination WHERE tenant_id = $1 AND key = 'gr_ir'`, r.ten.ID)
		refuse(t, "2026-03-01", "ledger.automatic_account")
	})
	t.Run("an owner no longer active", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, r.ten.Human.ID)
		defer pgtest.Exec(t, `UPDATE identity.principal SET status = 'active', version = version + 1 WHERE id = $1`, r.ten.Human.ID)
		refuse(t, "2026-04-01", "ledger.owner_not_permitted")
	})
	t.Run("an owner without access to the company", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE identity.principal SET all_companies = false, version = version + 1 WHERE id = $1`, r.ten.Human.ID)
		defer pgtest.Exec(t, `UPDATE identity.principal SET all_companies = true, version = version + 1 WHERE id = $1`, r.ten.Human.ID)
		refuse(t, "2026-05-01", "ledger.owner_not_permitted")
	})
	t.Run("an owner without ledger.entry.park", func(t *testing.T) {
		var roles []uuid.UUID
		pgtest.Scan(t, `SELECT array_agg(rp.role_id) FROM identity.role_permission rp JOIN identity.principal_role pr
			ON pr.tenant_id = rp.tenant_id AND pr.role_id = rp.role_id WHERE pr.principal_id = $1 AND rp.permission_code = 'ledger.entry.park'`,
			[]any{r.ten.Human.ID}, &roles)
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND role_id = ANY($2) AND permission_code = 'ledger.entry.park'`,
			r.ten.ID, roles)
		defer pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
			SELECT $1, gen_random_uuid(), x, 'ledger.entry.park', '2000-01-01', $3 FROM unnest($2::uuid[]) AS x`, r.ten.ID, roles, r.ten.Human.ID)
		refuse(t, "2026-06-01", "ledger.owner_not_permitted")
	})
	t.Run("an owner barred from ledger.entry.park", func(t *testing.T) {
		pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), 'ledger.entry.park', 'ledger.entry.post', 'Park or post', '2000-01-01', $2)`, r.ten.ID, r.ten.Human.ID)
		defer pgtest.Exec(t, `DELETE FROM identity.sod_rule WHERE tenant_id = $1 AND description = 'Park or post'`, r.ten.ID)
		refuse(t, "2026-07-01", "ledger.owner_not_permitted")
	})
	if n := len(r.parked(t)); n != 0 {
		t.Errorf("entries: got %d, want none", n)
	}
	r.now = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	if got := r.park(t, r.template, "2026-08-01"); got != ledger.OccurrenceParked {
		t.Errorf("the next date, nothing refusing it: got %s, want parked", got)
	}
	t.Run("retired or ownerless", func(t *testing.T) {
		tpl, _ := r.Must(200, "GET", "/v1/entry-templates/"+r.template.String(), "", "")
		r.Must(200, "POST", "/v1/entry-templates/"+r.template.String()+":retire", "", etagOf(tpl))
		if got := r.park(t, r.template, "2026-09-01"); got != ledger.OccurrenceSkipped {
			t.Errorf("retired: got %s, want skipped", got)
		}
		bank := r.account(t, "2000", "100000")
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
	t.Run("without a directory of people", func(t *testing.T) {
		h := *r.h
		h.People = nil
		err := apitest.Committed(t, r.ten, recurrer, func(ctx context.Context) error {
			_, err := h.ParkOccurrence(ctx, r.template, date("2026-01-01"))
			return err
		})
		pgtest.CheckErr(t, err, "no directory of people")
	})
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
			_, err := r.h.ParkOccurrence(ctx, r.template, date("2026-01-01"))
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

// TestParkPermission: the permission the recurrer checks the owner for is
// the one parking an entry takes.
func TestParkPermission(t *testing.T) {
	if got := api.MustOperations()["parkJournalEntry"].Permission; got != ledger.ParkPermission {
		t.Errorf("got %s, want parkJournalEntry's x-permission %s", ledger.ParkPermission, got)
	}
}
