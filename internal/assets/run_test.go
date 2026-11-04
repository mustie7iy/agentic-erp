package assets_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// newRuns is newAcquisitions whose people may also start depreciation
// runs (and do what more permits), the tenant's group currency USD, so
// group amounts differ from company 2000's EUR.
func newRuns(t *testing.T, more ...string) assetEnv {
	t.Helper()
	return newRunsWith(t, withLedgers, more...)
}

// newRunsWith is newRuns with the document doc gives.
func newRunsWith(t *testing.T, doc func(*config.Config), more ...string) assetEnv {
	t.Helper()
	e := newAssetsWith(t, doc, append(append(postPermissions, "assets.depreciation.run"), more...)...)
	pgtest.Exec(t, `UPDATE tenancy.tenant SET group_currency = 'USD', version = version + 1 WHERE id = $1`, e.Tenant.ID)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.92, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	return e
}

// start starts a run of company 2000 in 2026 and returns its Operation
// and the run's id.
func (e assetEnv) start(t *testing.T, ledger uuid.UUID, period int) (map[string]any, uuid.UUID) {
	t.Helper()
	op, _ := e.Must(202, "POST", "/v1/depreciation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "fiscal_year": 2026, "period": %d}`,
		e.DE, ledger, period), "")
	return op, uuid.MustParse(strings.TrimPrefix(op["resource"].(string), "/v1/depreciation-runs/"))
}

// step runs one of a run's steps as the run's work does: committed, as
// the person who started it.
func (e assetEnv) step(t *testing.T, fn func(ctx context.Context, h *assets.Endpoints) error) error {
	t.Helper()
	h := app.Assets(app.Config{})
	return apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return fn(ctx, h) })
}

// carry carries the run out as the workflow does, failing it on a
// business problem, and returns the run.
func (e assetEnv) carry(t *testing.T, run uuid.UUID) map[string]any {
	t.Helper()
	var batches []int32
	err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		var err error
		batches, err = h.PlanDepreciationRun(ctx, run)
		return err
	})
	for _, b := range batches {
		if err == nil {
			err = e.step(t, func(ctx context.Context, h *assets.Endpoints) error { return h.PostDepreciationBatch(ctx, run, b) })
		}
	}
	if err == nil {
		err = e.step(t, func(ctx context.Context, h *assets.Endpoints) error { return h.FinishDepreciationRun(ctx, run) })
	}
	if p := problem.From(err); err != nil && p.Status < 500 {
		err = e.step(t, func(ctx context.Context, h *assets.Endpoints) error { return h.FailDepreciationRun(ctx, run, p) })
	}
	if err != nil {
		t.Fatalf("run %s: %v", run, err)
	}
	r, _ := e.Must(200, "GET", "/v1/depreciation-runs/"+run.String(), "", "")
	return r
}

// runOf is a run's status and counts, then each batch as
// "no status assets posted skipped amount".
func runOf(r map[string]any) string {
	out := []string{fmt.Sprint(r["status"], " ", r["assets_planned"], " ", r["assets_posted"], " ", r["assets_skipped"], " ", amountOr(r["amount"]))}
	for _, b := range r["batches"].([]any) {
		b := b.(map[string]any)
		out = append(out, fmt.Sprint(b["batch_no"], " ", b["status"], " ", b["assets"], " ", b["assets_posted"], " ", b["assets_skipped"], " ", amountOr(b["amount"])))
	}
	return strings.Join(out, "; ")
}

func amountOr(v any) string {
	if v == nil {
		return "-"
	}
	return amount(v)
}

// entryLines are a journal entry's lines, one each: "ledger account cost
// center profit center local group".
func entryLines(t *testing.T, entry any) string {
	t.Helper()
	rows, err := pgtest.Admin(t).Query(context.Background(), `SELECT g.code, a.number, coalesce(cc.code, '-'), coalesce(pc.code, '-'), l.amount_lc, l.amount_gc
		FROM ledger.journal_line l JOIN ledger.ledger g ON g.id = l.ledger_id JOIN ledger.gl_account a ON a.id = l.account_id
		LEFT JOIN ledger.cost_center cc ON cc.id = l.cost_center_id LEFT JOIN ledger.profit_center pc ON pc.id = l.profit_center_id
		WHERE l.journal_entry_id = $1 ORDER BY l.line_no`, entry)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ledger, account, cc, pc string
		var lc, gc int64
		if err := rows.Scan(&ledger, &account, &cc, &pc, &lc, &gc); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(ledger, " ", account, " ", cc, " ", pc, " ", lc, " ", gc))
	}
	return strings.Join(out, "; ")
}

// TestDepreciationRun: June's run in the leading ledger posts one entry
// there alone, the depreciation of two assets on their class's accounts
// and centers, with group amounts per asset summed into each line; their
// movements name the entry, they are booked through June, take a seq each
// and their terms are fixed; a repeat run posts nothing; July's run posts
// back the depreciation of an asset whose acquisition was reversed, on
// lines of its own; refusals.
func TestDepreciationRun(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	lathe, lathed := e.depreciable(t, "Lathe")
	op, run := e.start(t, e.l0, 6)
	want(t, "the operation", fmt.Sprint(op["kind"], " ", op["status"]), "assets.depreciate queued")
	r, _ := e.Must(200, "GET", "/v1/depreciation-runs/"+run.String(), "", "")
	want(t, "the queued run", fmt.Sprint(r["company_code"], " ", r["ledger_code"], " ", r["fiscal_year"], "/", r["period"], " ", r["posting_date"], " ",
		r["currency"], " ", runOf(r), " ", r["operation_id"] == op["id"]), "2000 0L 2026/6 2026-06-30 EUR queued <nil> <nil> <nil> - true")
	e.Problem(409, "assets.run_in_progress", "POST", "/v1/depreciation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026, "period": 6}`, e.DE), "")

	r = e.carry(t, run)
	want(t, "June's run", runOf(r), "posted 2 2 0 20.00; 1 posted 2 2 0 20.00")
	batch := r["batches"].([]any)[0].(map[string]any)
	want(t, "the batch's entry, in 0L alone", entryLines(t, batch["journal_entry_id"]), "0L 620000 1000 PC1 2000 2174; 0L 160900 - PC1 -2000 -2174")
	var entries int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE source_document_id = $1`, []any{run}, &entries)
	want(t, "the run's entries", entries, 1)
	m, _ := e.Must(200, "GET", "/v1/assets/"+mill+"/movements?kind=depreciation", "", "")
	mv := m["items"].([]any)[0].(map[string]any)
	want(t, "the mill's depreciation", fmt.Sprint(mv["seq"], " ", mv["ledger_code"], " ", mv["posting_date"], " ", amount(mv["accumulated"]), " ",
		amount(mv["accumulated_gc"]), " ", mv["document_kind"], " ", mv["document_id"] == run.String(), " ", mv["document_number"] == batch["document_number"]),
		"3 0L 2026-06-30 -10.00 -10.87 depreciation_run true true")
	var booked string
	pgtest.Scan(t, `SELECT string_agg(g.code || ' ' || coalesce(t.booked_through_year || '/' || t.booked_through_period, '-'), ', ' ORDER BY g.code)
		FROM assets.asset_terms t JOIN ledger.ledger g ON g.id = t.ledger_id WHERE t.asset_id = $1`, []any{mill}, &booked)
	want(t, "booked through", booked, "0L 2026/6, 2L -")
	_, tag := e.Must(200, "GET", "/v1/assets/"+mill, "", "")
	e.Problem(409, "assets.terms_in_use", "PATCH", "/v1/assets/"+mill, fmt.Sprintf(`{"terms": [{"ledger_id": %q, "useful_life_months": 60}]}`, e.l0), tag)
	evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "depreciation_run_completed"})
	want(t, "DepreciationRunCompleted", fmt.Sprint(evs[0]["status"], " ", evs[0]["assets_posted"], " ", evs[0]["amount"], " ", len(evs[0]["entries"].([]any))),
		"posted 2 map[amount:20.00 currency:EUR] 1")

	_, again := e.start(t, e.l0, 6)
	want(t, "a repeat run", runOf(e.carry(t, again)), "posted 2 0 0 0.00; 1 posted 2 0 0 0.00")
	var seq int
	pgtest.Scan(t, `SELECT last_seq FROM assets.asset WHERE id = $1`, []any{mill}, &seq)
	want(t, "the mill's seq after two runs", seq, 4)

	_, tag = e.Must(200, "GET", "/v1/asset-acquisitions/"+lathed, "", "")
	e.clocked.Must(200, "POST", "/v1/asset-acquisitions/"+lathed+":reverse", `{"reason": "Not ours"}`, tag)
	_, july := e.start(t, e.l0, 7)
	r = e.carry(t, july)
	want(t, "July's run, the lathe's depreciation posted back", runOf(r), "posted 2 2 0 0.00; 1 posted 2 2 0 0.00")
	want(t, "July's entry", entryLines(t, r["batches"].([]any)[0].(map[string]any)["journal_entry_id"]),
		"0L 620000 1000 PC1 1000 1087; 0L 620000 1000 PC1 -1000 -1087; 0L 160900 - PC1 1000 1087; 0L 160900 - PC1 -1000 -1087")
	s, _ := e.Must(200, "GET", "/v1/assets/"+lathe+"/schedule?fiscal_year=2026", "", "")
	want(t, "the lathe's schedule", schedule(s, 6), "6 0.00 10.00 0.00; 7 0.00 -10.00 0.00; 8 0.00 0.00 0.00; 9 0.00 0.00 0.00; 10 0.00 0.00 0.00; "+
		"11 0.00 0.00 0.00; 12 0.00 0.00 0.00 | 0.00 0.00")

	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year = 2026 AND period = 9`, e.DE, e.l0)
	body := func(ledger uuid.UUID, year, period int) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "fiscal_year": %d, "period": %d}`, e.DE, ledger, year, period)
	}
	for name, c := range map[string]struct {
		status     int
		code, body string
	}{
		"a special period":                   {422, "assets.special_period", body(e.l0, 2026, 13)},
		"a ledger the company does not keep": {422, "ledger.ledger_not_kept", body(e.l3, 2026, 8)},
		"a year not set up":                  {422, "ledger.no_fiscal_period", body(e.l0, 2031, 1)},
		"a period not open":                  {409, "ledger.period_not_open", body(e.l0, 2026, 9)},
		"a period before the latest run's":   {409, "assets.run_out_of_order", body(e.l0, 2026, 6)},
		"a company the caller cannot act in": {404, "request.not_found", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026, "period": 8}`,
			uuid.Must(uuid.NewV7()))},
	} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(c.status, c.code, "POST", "/v1/depreciation-runs", c.body, "")
		})
	}
	e.Problem(404, "request.not_found", "GET", "/v1/depreciation-runs/"+uuid.Must(uuid.NewV7()).String(), "", "")
}

// TestDepreciationRunSkipsAndCatchesUp: an asset no longer selectable
// under its lock when its batch posts is skipped and takes no seq; a run
// whose period was closed after it was planned fails with the ledger's
// problem, its Operation too, and the next period's run catches up.
func TestDepreciationRunSkipsAndCatchesUp(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	press, pressed := e.depreciable(t, "Press")
	_, run := e.start(t, e.l0, 6)
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		_, err := h.PlanDepreciationRun(ctx, run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, tag := e.Must(200, "GET", "/v1/asset-acquisitions/"+pressed, "", "")
	e.clocked.Must(200, "POST", "/v1/asset-acquisitions/"+pressed+":reverse", `{"reason": "Not ours"}`, tag)
	var seq int
	pgtest.Scan(t, `SELECT last_seq FROM assets.asset WHERE id = $1`, []any{press}, &seq)
	want(t, "the run, the press skipped", runOf(e.carry(t, run)), "posted 2 1 1 10.00; 1 posted 2 1 1 10.00")
	var after int
	pgtest.Scan(t, `SELECT last_seq FROM assets.asset WHERE id = $1`, []any{press}, &after)
	want(t, "the skipped press's seq", after, seq)

	op, july := e.start(t, e.l0, 7)
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		_, err := h.PlanDepreciationRun(ctx, july)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year = 2026 AND period = 7`, e.DE, e.l0)
	r := e.carry(t, july)
	want(t, "the run of a period closed meanwhile", fmt.Sprint(runOf(r), " ", r["error"].(map[string]any)["code"]),
		"failed 1 0 0 0.00; 1 pending 1 <nil> <nil> - ledger.period_not_open")
	var operation string
	pgtest.Scan(t, `SELECT status || ' ' || (error->>'code') FROM platform.operation WHERE id = $1`, []any{op["id"]}, &operation)
	want(t, "its operation", operation, "failed ledger.period_not_open")
	evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "depreciation_run_completed"})
	want(t, "the failed run's event", fmt.Sprint(evs[1]["status"], " ", evs[1]["assets_planned"], " ", evs[1]["error"].(map[string]any)["code"]),
		"failed 1 ledger.period_not_open")

	_, august := e.start(t, e.l0, 8)
	want(t, "August's run, July's caught up", runOf(e.carry(t, august)), "posted 1 1 0 20.00; 1 posted 1 1 0 20.00")
	s, _ := e.Must(200, "GET", "/v1/assets/"+mill+"/schedule?fiscal_year=2026", "", "")
	want(t, "the mill's schedule", schedule(s, 6), "6 10.00 10.00 1190.00; 7 10.00 0.00 1180.00; 8 10.00 20.00 1170.00; 9 10.00 0.00 1160.00; "+
		"10 10.00 0.00 1150.00; 11 10.00 0.00 1140.00; 12 10.00 0.00 1130.00 | 70.00 30.00")
}

// TestListDepreciationRuns: the runs of the caller's companies by
// creation, filtered by company, ledger, fiscal year and status, paged.
func TestListDepreciationRuns(t *testing.T) {
	e := newRuns(t)
	e.depreciable(t, "Mill")
	_, june := e.start(t, e.l0, 6)
	e.carry(t, june)
	_, l2 := e.start(t, e.l2, 6)
	list := func(query string) string {
		page, _ := e.Must(200, "GET", "/v1/depreciation-runs?"+query, "", "")
		var out []string
		for _, r := range page["items"].([]any) {
			r := r.(map[string]any)
			out = append(out, fmt.Sprint(r["ledger_code"], " ", r["status"], " ", len(r["batches"].([]any))))
		}
		return strings.Join(out, ", ")
	}
	want(t, "every run", list(""), "0L posted 1, 2L queued 0")
	want(t, "the company's in 2L", list(fmt.Sprintf("legal_entity_id=%s&ledger_id=%s", e.DE, e.l2)), "2L queued 0")
	want(t, "posted ones of 2026", list("fiscal_year=2026&status=posted"), "0L posted 1")
	want(t, "another year", list("fiscal_year=2027"), "")
	page, _ := e.Must(200, "GET", "/v1/depreciation-runs?page_size=1&order_by=created_at%20desc", "", "")
	want(t, "the latest first", page["items"].([]any)[0].(map[string]any)["id"], l2.String())
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/depreciation-runs?order_by=period", "", "")
}

// TestDepreciationRunSteps: each step called again after it took effect
// does nothing (ADR-0012), a step of a run in another state leaves it
// alone, and one outside a unit of work is refused.
func TestDepreciationRunSteps(t *testing.T) {
	e := newRuns(t)
	e.depreciable(t, "Mill")
	_, run := e.start(t, e.l0, 6)
	e.carry(t, run)
	r := e.carry(t, run) // every step again on a posted run
	want(t, "the run carried out again", runOf(r), "posted 1 1 0 10.00; 1 posted 1 1 0 10.00")
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		return h.FailDepreciationRun(ctx, run, problem.Conflict("ledger.period_not_open", "closed"))
	}); err != nil {
		t.Fatal(err)
	}
	r, _ = e.Must(200, "GET", "/v1/depreciation-runs/"+run.String(), "", "")
	want(t, "a posted run failed again", r["status"], "posted")
	h := app.Assets(app.Config{})
	for name, fn := range map[string]func(ctx context.Context) error{
		"plan":   func(ctx context.Context) error { _, err := h.PlanDepreciationRun(ctx, run); return err },
		"post":   func(ctx context.Context) error { return h.PostDepreciationBatch(ctx, run, 1) },
		"finish": func(ctx context.Context) error { return h.FinishDepreciationRun(ctx, run) },
		"fail":   func(ctx context.Context) error { return h.FailDepreciationRun(ctx, run, problem.Conflict("x.y", "z")) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); err == nil {
				t.Errorf("outside a unit of work: got nil, want an error")
			}
		})
	}
	err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		_, err := h.PlanDepreciationRun(ctx, uuid.Must(uuid.NewV7()))
		return err
	})
	if p := problem.From(err); p.Code != "request.not_found" {
		t.Errorf("an unknown run: got %v, want request.not_found", err)
	}
	var unfinished map[uuid.UUID]uuid.UUID
	_, queued := e.start(t, e.l2, 6)
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		tx, _ := reqctx.TxFrom(ctx) // step runs it in a unit of work
		var err error
		unfinished, err = h.UnfinishedDepreciationRuns(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := unfinished[queued]; !ok || len(unfinished) != 1 {
		t.Errorf("unfinished runs: got %v, want %s alone", unfinished, queued)
	}
}

// TestDepreciationRunFinishesAfterItsBatches: a run is not finished while
// a batch is pending.
func TestDepreciationRunFinishesAfterItsBatches(t *testing.T) {
	e := newRuns(t)
	e.depreciable(t, "Mill")
	_, run := e.start(t, e.l0, 6)
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		_, err := h.PlanDepreciationRun(ctx, run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error { return h.FinishDepreciationRun(ctx, run) })
	if err == nil || !strings.Contains(err.Error(), "1 batches not posted") {
		t.Errorf("finish before the batch: got %v, want the pending batch named", err)
	}
}

// TestDepreciationRunBatches: a run's assets are planned in batches by
// asset number (two a batch here), the last one partial, each posted as
// its own entry with the progress reported on the Operation and the plan
// audited; a running run, and one failing after its first batch, shows
// what that batch posted, which stays posted.
func TestDepreciationRunBatches(t *testing.T) {
	e := newRuns(t)
	for _, d := range []string{"Mill", "Lathe", "Press"} {
		e.depreciable(t, d)
	}
	h := app.Assets(app.Config{})
	h.BatchSize = 2
	op, run := e.start(t, e.l0, 6)
	committed := func(fn func(ctx context.Context) error) {
		t.Helper()
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, fn); err != nil {
			t.Fatal(err)
		}
	}
	var batches []int32
	committed(func(ctx context.Context) error {
		var err error
		batches, err = h.PlanDepreciationRun(ctx, run)
		return err
	})
	want(t, "the batches", fmt.Sprint(batches), "[1 2]")
	var numbers string
	pgtest.Scan(t, `SELECT string_agg(x.n, ' | ' ORDER BY b.batch_no) FROM assets.depreciation_batch b,
			LATERAL (SELECT string_agg(a.asset_number, ' ' ORDER BY a.asset_number) AS n FROM assets.asset a WHERE a.id = ANY(b.asset_ids)) x
		WHERE b.run_id = $1`, []any{run}, &numbers)
	want(t, "the batches' assets by number", numbers, "1 2 | 3")
	var planned int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'assets.depreciation_run.plan' AND object_id = $2`,
		[]any{e.Tenant.ID, run}, &planned)
	want(t, "the plan's audit record", planned, 1)
	committed(func(ctx context.Context) error { return h.PostDepreciationBatch(ctx, run, 1) })
	var progress string
	pgtest.Scan(t, `SELECT progress_done || '/' || progress_total FROM platform.operation WHERE id = $1`, []any{op["id"]}, &progress)
	want(t, "the progress", progress, "1/2")
	r, _ := e.Must(200, "GET", "/v1/depreciation-runs/"+run.String(), "", "")
	want(t, "a running run after its first batch", runOf(r), "running 3 2 0 20.00; 1 posted 2 2 0 20.00; 2 pending 1 <nil> <nil> -")
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year = 2026 AND period = 6`, e.DE, e.l0)
	r = e.carry(t, run)
	want(t, "a run failed after its first batch", runOf(r), "failed 3 2 0 20.00; 1 posted 2 2 0 20.00; 2 pending 1 <nil> <nil> -")
}

// heldRuns is the operations port whose report, the last thing the plan
// and a batch do before they commit, waits inside the step's transaction
// until the test lets it go: a step under way, held open.
type heldRuns struct {
	assets.Runs
	inside  chan<- struct{}
	release <-chan struct{}
}

func (h heldRuns) Report(ctx context.Context, id uuid.UUID, done, total int64) error {
	h.inside <- struct{}{}
	<-h.release
	return h.Runs.Report(ctx, id, done, total)
}

// TestDepreciationRunCancelDuringAStep: a :cancel while the plan's or a
// batch's transaction is open answers 200 at once, rather than waiting
// for the step and meeting the lock timeout (409 request.in_progress,
// which the Idempotency-Key would replay); the step commits as a whole,
// leaving the cancelled Operation alone, and the next step fails the run
// with assets.run_cancelled.
func TestDepreciationRunCancelDuringAStep(t *testing.T) {
	e := newRuns(t, "platform.operation.read", "platform.operation.cancel")
	e.depreciable(t, "Mill")
	plan := func(ctx context.Context, h *assets.Endpoints, run uuid.UUID) error {
		_, err := h.PlanDepreciationRun(ctx, run)
		return err
	}
	for _, c := range []struct {
		name    string
		planned bool
		step    func(ctx context.Context, h *assets.Endpoints, run uuid.UUID) error
		want    string
	}{
		{"the plan", false, plan, "failed 1 0 0 0.00; 1 pending 1 <nil> <nil> -"},
		{"a batch", true, func(ctx context.Context, h *assets.Endpoints, run uuid.UUID) error {
			return h.PostDepreciationBatch(ctx, run, 1)
		},
			"failed 1 1 0 10.00; 1 posted 1 1 0 10.00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			op, run := e.start(t, e.l0, 6)
			if c.planned {
				if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error { return plan(ctx, h, run) }); err != nil {
					t.Fatal(err)
				}
			}
			path := "/v1/operations/" + op["id"].(string)
			_, tag := e.In(t).Must(200, "GET", path, "", "")
			inside, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			h := app.Assets(app.Config{})
			h.Runs = heldRuns{Runs: h.Runs, inside: inside, release: release}
			go func() {
				done <- apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return c.step(ctx, h, run) })
			}()
			select {
			case <-inside:
			case err := <-done:
				t.Fatalf("the step ended before its report: %v", err)
			}
			code, body, _ := e.In(t).Do("POST", path+":cancel", "", tag)
			close(release)
			if err := <-done; err != nil {
				t.Errorf("the step held open: got %v, want it committed", err)
			}
			if code != 200 || body["status"] != "cancelled" {
				t.Errorf("the cancel during %s: got %d %v, want 200 cancelled at once", c.name, code, body)
			}
			r := e.carry(t, run)
			want(t, "the run after the cancel", runOf(r), c.want)
			problem, _ := r["error"].(map[string]any) // none on a run that went on to be posted
			want(t, "its problem", problem["code"], "assets.run_cancelled")
			o, _ := e.In(t).Must(200, "GET", path, "", "")
			want(t, "its Operation", o["status"], "cancelled")
		})
	}
}
