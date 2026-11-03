package operations_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"platform.operation.read", "platform.operation.cancel"}

// inRequest runs fn as a principal inside a transaction, the way the unit of
// work does; withAudit adds an audit buffer.
func inRequest(t *testing.T, ten pgtest.Tenant, a pgtest.Actor, withAudit bool, fn func(ctx context.Context) error) error {
	t.Helper()
	return pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID}, func(ctx context.Context, tx pgx.Tx) error {
		ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), reqctx.Principal{ID: a.ID, Kind: a.Kind, TenantID: ten.ID})
		if withAudit {
			ctx, _ = audit.WithBuffer(ctx)
		}
		return fn(ctx)
	})
}

func start(t *testing.T, ten pgtest.Tenant, kind string) operations.Operation {
	t.Helper()
	var o operations.Operation
	if err := inRequest(t, ten, ten.Human, false, func(ctx context.Context) (err error) {
		o, err = operations.Start(ctx, kind, map[string]any{"run_date": "2026-10-09"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestGetOperation(t *testing.T) {
	api := apitest.New(t)
	ten := pgtest.NewTenant(t, perms...)
	o := start(t, ten, "payments.propose")
	r := api.Do(apitest.Req{Method: "GET", Path: "/v1/operations/" + o.ID.String(), Tenant: ten.ID, As: ten.Agent})
	if r.Code != 200 || r.Header().Get("ETag") != `"1"` {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	got := apitest.JSON(t, r)
	progress, ok := got["progress"].(map[string]any)
	if !ok {
		t.Fatalf("progress: got %v, want an object (a required field)", got["progress"])
	}
	for field, c := range map[string]struct{ got, want any }{
		"id":             {got["id"], o.ID.String()},
		"status":         {got["status"], "queued"},
		"kind":           {got["kind"], "payments.propose"},
		"resource":       {got["resource"], nil},
		"progress.total": {progress["total"], nil},
		"created_by":     {got["created_by"], ten.Human.ID.String()},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", field, c.got, c.want)
		}
	}
	// A finished operation shows its result and error.
	failed := start(t, ten, "integration.migrate")
	pgtest.Exec(t, `UPDATE platform.operation SET status = 'failed', progress_done = 5, progress_total = 10, resource = '/v1/migration-runs/x',
		result = '{"loaded": 5}', error = '{"type":"t","title":"Failed","status":422,"code":"integration.reconciliation_failed"}', version = version + 1
		WHERE id = $1`, failed.ID)
	got = apitest.JSON(t, api.Do(apitest.Req{Method: "GET", Path: "/v1/operations/" + failed.ID.String(), Tenant: ten.ID, As: ten.Human}))
	progress, _ = got["progress"].(map[string]any)
	result, _ := got["result"].(map[string]any)
	problemBody, _ := got["error"].(map[string]any)
	for field, c := range map[string]struct{ got, want any }{
		"resource":       {got["resource"], "/v1/migration-runs/x"},
		"progress.total": {progress["total"], float64(10)},
		"result.loaded":  {result["loaded"], float64(5)},
		"error.code":     {problemBody["code"], "integration.reconciliation_failed"},
	} {
		if c.got != c.want {
			t.Errorf("failed operation %s: got %v, want %v", field, c.got, c.want)
		}
	}
}

func TestOperationAccess(t *testing.T) {
	api := apitest.New(t)
	ten := pgtest.NewTenant(t, perms...)
	other := pgtest.NewTenant(t, perms...)
	noPerms := pgtest.NewTenant(t)
	o := start(t, ten, "payments.propose")
	path := "/v1/operations/" + o.ID.String()
	cases := []struct {
		name string
		req  apitest.Req
		code int
		pc   string
	}{
		{"no token", apitest.Req{Method: "GET", Path: path}, 401, "authn.invalid_token"},
		{"other tenant", apitest.Req{Method: "GET", Path: path, Tenant: other.ID, As: other.Human}, 404, "request.not_found"},
		{"no permission", apitest.Req{Method: "GET", Path: path, Tenant: noPerms.ID, As: noPerms.Human}, 403, "identity.permission_missing"},
		{"unknown id", apitest.Req{Method: "GET", Path: "/v1/operations/" + uuid.NewString(), Tenant: ten.ID, As: ten.Human}, 404, "request.not_found"},
		{"bad id", apitest.Req{Method: "GET", Path: "/v1/operations/not-a-uuid", Tenant: ten.ID, As: ten.Human}, 400, "request.invalid_parameter"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := api.Do(c.req)
			if r.Code != c.code {
				t.Errorf("got status %d, want %d: %s", r.Code, c.code, r.Body)
			}
			if ct := r.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("got content type %q, want application/problem+json", ct)
			}
			if pc := apitest.JSON(t, r)["code"]; pc != c.pc {
				t.Errorf("got problem %v, want %s", pc, c.pc)
			}
		})
	}
}

func TestListOperations(t *testing.T) {
	api := apitest.New(t)
	ten := pgtest.NewTenant(t, perms...)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, start(t, ten, "payments.propose").ID.String())
	}
	start(t, ten, "integration.migrate")
	list := func(query string) map[string]any {
		r := api.Do(apitest.Req{Method: "GET", Path: "/v1/operations" + query, Tenant: ten.ID, As: ten.Human})
		if r.Code != 200 {
			t.Fatalf("%s: %d %s", query, r.Code, r.Body)
		}
		return apitest.JSON(t, r)
	}
	itemIDs := func(page map[string]any) (out []string) {
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any)["id"].(string))
		}
		return out
	}
	first := list("?kind=payments.propose&page_size=2")
	if got, want := strings.Join(itemIDs(first), ","), ids[2]+","+ids[1]; got != want {
		t.Fatalf("newest first: got %s, want %s", got, want)
	}
	tok := first["next_page_token"].(string)
	second := list("?kind=payments.propose&page_size=2&page_token=" + tok)
	if got := strings.Join(itemIDs(second), ","); got != ids[0] {
		t.Fatalf("second page: got %s, want %s", got, ids[0])
	}
	if second["next_page_token"] != nil {
		t.Fatalf("second page next_page_token: got %v, want none", second["next_page_token"])
	}
	asc := list("?kind=payments.propose&order_by=created_at%20asc")
	if got := itemIDs(asc); len(got) != 3 || got[0] != ids[0] {
		t.Fatalf("ascending: %v", got)
	}
	if got := itemIDs(list("?status=running")); len(got) != 0 {
		t.Fatalf("status filter: %v", got)
	}
	if got := list("?kind=test.none")["items"].([]any); len(got) != 0 {
		t.Fatal("empty page must be an empty array")
	}
	for query, code := range map[string]string{
		"?order_by=kind":      "request.invalid_order_by",
		"?page_size=0":        "request.invalid_parameter", // the contract enforces the minimum
		"?page_token=garbage": "request.invalid_page_token",
		"?kind=integration.migrate&page_token=" + tok: "request.invalid_page_token", // issued for other filters
	} {
		t.Run(query, func(t *testing.T) {
			r := api.Do(apitest.Req{Method: "GET", Path: "/v1/operations" + query, Tenant: ten.ID, As: ten.Human})
			if r.Code != 400 || apitest.JSON(t, r)["code"] != code {
				t.Errorf("got %d %s, want 400 %s", r.Code, r.Body, code)
			}
		})
	}
}

// fakeEngine records the workflows it is asked to cancel; ids containing
// "fail" are refused.
type fakeEngine struct{ cancelled []string }

func (f *fakeEngine) Start(context.Context, workflow.Start) (bool, error) { return true, nil }
func (f *fakeEngine) Cancel(_ context.Context, id string) error {
	f.cancelled = append(f.cancelled, id)
	if strings.Contains(id, "fail") {
		return errors.New("engine down")
	}
	return nil
}

func TestCancelOperation(t *testing.T) {
	engine := &fakeEngine{}
	var logs bytes.Buffer
	api := apitest.NewWith(t, func(cfg *app.Config) { cfg.Workflows = engine; cfg.Log = slog.New(slog.NewTextHandler(&logs, nil)) })
	ten := pgtest.NewTenant(t, perms...)
	o := start(t, ten, "payments.propose")
	path := "/v1/operations/" + o.ID.String() + ":cancel"
	key := apitest.NewKey()
	cancel := func(k, ifMatch string) (int, map[string]any) {
		r := api.Do(apitest.Req{Method: "POST", Path: path, Tenant: ten.ID, As: ten.Agent, Key: k, IfMatch: ifMatch})
		return r.Code, apitest.JSON(t, r)
	}
	if code, body := cancel(key, ""); code != 428 || body["code"] != "request.precondition_required" {
		t.Fatalf("missing If-Match: %d %v", code, body)
	}
	if code, _ := cancel("", `"1"`); code != 428 {
		t.Fatalf("missing Idempotency-Key: %d", code)
	}
	if code, body := cancel(apitest.NewKey(), `"7"`); code != 412 || body["code"] != "request.version_mismatch" {
		t.Fatalf("stale If-Match: %d %v", code, body)
	}
	r := api.Do(apitest.Req{Method: "POST", Path: path, Tenant: ten.ID, As: ten.Agent, Key: key, IfMatch: `"1"`})
	got := apitest.JSON(t, r)
	if r.Code != 200 {
		t.Fatalf("cancel: got status %d, want 200: %v", r.Code, got)
	}
	if tag := r.Header().Get("ETag"); tag != `"2"` {
		t.Errorf("cancel ETag: got %s, want %s", tag, `"2"`)
	}
	if got["status"] != "cancelled" {
		t.Errorf("cancel status: got %v, want cancelled", got["status"])
	}
	if want := ten.Agent.ID.String(); got["updated_by"] != want {
		t.Errorf("cancel updated_by: got %v, want %s", got["updated_by"], want)
	}
	// An operation without a workflow asks the engine nothing; one with a
	// workflow has it cancelled; an engine that does not take the
	// cancellation is logged, the database row being the truth.
	if len(engine.cancelled) != 0 {
		t.Fatalf("engine asked for %v", engine.cancelled)
	}
	for _, wf := range []string{"wf-ok", "wf-fail"} {
		if !t.Run(wf, func(t *testing.T) {
			ox := start(t, ten, "payments.propose")
			pgtest.Exec(t, `UPDATE platform.operation SET workflow_id = $2, version = version + 1 WHERE id = $1`, ox.ID, wf)
			if r := api.Do(apitest.Req{Method: "POST", Path: "/v1/operations/" + ox.ID.String() + ":cancel", Tenant: ten.ID, As: ten.Agent, Key: apitest.NewKey(), IfMatch: "*"}); r.Code != 200 {
				t.Fatalf("got %d %s, want 200", r.Code, r.Body)
			}
		}) {
			t.FailNow()
		}
	}
	if got := strings.Join(engine.cancelled, " "); got != "wf-ok wf-fail" {
		t.Fatalf("engine cancellations: got %s, want wf-ok wf-fail", got)
	}
	if !strings.Contains(logs.String(), "did not take the cancellation") {
		t.Fatalf("logs: got %s, want the refused cancellation logged", logs.String())
	}
	// A retry with the same key returns the same answer and changes nothing.
	replay := api.Do(apitest.Req{Method: "POST", Path: path, Tenant: ten.ID, As: ten.Agent, Key: key, IfMatch: `"1"`})
	if replay.Code != 200 {
		t.Fatalf("replay: got status %d, want 200", replay.Code)
	}
	if got := replay.Header().Get("Idempotent-Replayed"); got != "true" {
		t.Errorf("replay Idempotent-Replayed: got %q, want true", got)
	}
	if got := replay.Header().Get("ETag"); got != `"2"` {
		t.Errorf("replay ETag: got %s, want %s", got, `"2"`)
	}
	if code, body := cancel(apitest.NewKey(), `"2"`); code != 409 || body["code"] != "platform.operation_not_cancellable" {
		t.Fatalf("cancel twice: %d %v", code, body)
	}
	// The change was audited once and published once, as the agent.
	var audits, events int
	var payload []byte
	admin := pgtest.Admin(t)
	if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'platform.operation.cancel'
		AND object_id = $2 AND actor_id = $3 AND actor_kind = 'agent'`, ten.ID, o.ID, ten.Agent.ID).Scan(&audits); err != nil {
		t.Fatalf("count cancel audits: %v", err)
	}
	if err := admin.QueryRow(context.Background(), `SELECT count(*), max(payload::text) FROM platform.outbox WHERE tenant_id = $1 AND aggregate_id = $2
		AND event_type = 'com.example.erp.platform.operation_completed.v1' AND actor_id = $3`, ten.ID, o.ID, ten.Agent.ID).Scan(&events, &payload); err != nil {
		t.Fatalf("read completion events: %v", err)
	}
	if audits != 1 || events != 1 {
		t.Fatalf("audits %d events %d", audits, events)
	}
	var ev map[string]any
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("decode the event: %v", err)
	}
	if ev["status"] != "cancelled" {
		t.Errorf("event status: got %v, want cancelled", ev["status"])
	}
	if ev["operation_id"] != o.ID.String() {
		t.Errorf("event operation_id: got %v, want %s", ev["operation_id"], o.ID)
	}
	if ev["error"] != nil {
		t.Errorf("event error: got %v, want nil", ev["error"])
	}
	// A running operation that recorded an error carries it in its completion event.
	running := start(t, ten, "integration.migrate")
	pgtest.Exec(t, `UPDATE platform.operation SET status = 'running', version = version + 1,
		error = '{"code":"integration.source_unavailable","detail":"SAP not reachable"}' WHERE id = $1`, running.ID)
	r = api.Do(apitest.Req{Method: "POST", Path: "/v1/operations/" + running.ID.String() + ":cancel", Tenant: ten.ID, As: ten.Human, Key: apitest.NewKey(), IfMatch: `"2"`})
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if err := admin.QueryRow(context.Background(), `SELECT payload::text FROM platform.outbox WHERE aggregate_id = $1`, running.ID).Scan(&payload); err != nil {
		t.Fatalf("read the failure event: %v", err)
	}
	if !strings.Contains(string(payload), `"message": "SAP not reachable"`) {
		t.Fatalf("event %s", payload)
	}
}

func TestOutsideAUnitOfWork(t *testing.T) {
	ctx := context.Background()
	e := &operations.Endpoints{}
	if _, err := operations.Start(ctx, "x.y", nil); err == nil {
		t.Error("Start outside a request")
	}
	if _, err := e.GetOperation(ctx, gen.GetOperationParams{}); err == nil {
		t.Error("get outside a request")
	}
	if _, err := e.ListOperations(ctx, gen.ListOperationsParams{}); err == nil {
		t.Error("list outside a request")
	}
	if _, err := e.CancelOperation(ctx, gen.CancelOperationParams{}); err == nil {
		t.Error("cancel outside a request")
	}
	if _, err := operations.IsCancelled(ctx, uuid.New(), true); err == nil {
		t.Error("IsCancelled outside a request")
	}
}

// TestIsCancelled: an operation reads as cancelled once it is, and not
// while queued, under its lock and without; an unknown one is not found.
func TestIsCancelled(t *testing.T) {
	ten := pgtest.NewTenant(t, perms...)
	for name, lock := range map[string]bool{"under its lock": true, "as it stands": false} {
		t.Run(name, func(t *testing.T) {
			o := start(t, ten, "test.cancelled")
			read := func(id uuid.UUID) (bool, error) {
				var got bool
				err := inRequest(t, ten, ten.Human, false, func(ctx context.Context) error {
					var err error
					got, err = operations.IsCancelled(ctx, id, lock)
					return err
				})
				return got, err
			}
			if got, err := read(o.ID); err != nil || got {
				t.Errorf("a queued operation: got %v %v, want false", got, err)
			}
			pgtest.Exec(t, `UPDATE platform.operation SET status = 'cancelled', version = version + 1 WHERE id = $1`, o.ID)
			if got, err := read(o.ID); err != nil || !got {
				t.Errorf("a cancelled operation: got %v %v, want true", got, err)
			}
			if _, err := read(uuid.New()); problem.From(err).Code != "request.not_found" {
				t.Errorf("an unknown operation: got %v, want request.not_found", err)
			}
		})
	}
}

func TestDatabaseFailures(t *testing.T) {
	ten := pgtest.NewTenant(t, perms...)
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	e := &operations.Endpoints{Pages: pages}
	if err := inRequest(t, ten, ten.Human, false, func(ctx context.Context) error {
		_, err := operations.Start(ctx, "x.y", make(chan int))
		return err
	}); err == nil {
		t.Error("unencodable request accepted")
	}
	// The handler checks page_size itself as well, for callers other than the generated router.
	if err := inRequest(t, ten, ten.Human, false, func(ctx context.Context) error {
		_, err := e.ListOperations(ctx, gen.ListOperationsParams{PageSize: gen.NewOptInt32(0)})
		return err
	}); err == nil {
		t.Error("page_size 0 accepted")
	}
	// Queries on a transaction that has ended fail.
	pool := pgtest.Pool(t)
	tx, _ := pool.Begin(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID})
	tx.Rollback(context.Background())
	dead := reqctx.WithPrincipal(reqctx.WithTx(context.Background(), tx), reqctx.Principal{ID: ten.Human.ID, Kind: "human", TenantID: ten.ID})
	if _, err := e.GetOperation(dead, gen.GetOperationParams{ID: uuid.New()}); err == nil {
		t.Error("get on a dead transaction")
	}
	if _, err := e.ListOperations(dead, gen.ListOperationsParams{}); err == nil {
		t.Error("list on a dead transaction")
	}
	if _, err := e.CancelOperation(dead, gen.CancelOperationParams{ID: uuid.New(), IfMatch: `"1"`}); err == nil {
		t.Error("cancel on a dead transaction")
	}
	if err := operations.Report(dead, uuid.New(), 1, 2); err == nil {
		t.Error("report on a dead transaction")
	}
	// Failures injected with triggers: the update, the outbox write, and a missing audit buffer.
	pgtest.Exec(t, `CREATE OR REPLACE FUNCTION public.fail_kind() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.kind = 'test.fail_update' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END $$`)
	pgtest.Exec(t, `CREATE TRIGGER fail_kind BEFORE UPDATE ON platform.operation FOR EACH ROW EXECUTE FUNCTION public.fail_kind()`)
	pgtest.Exec(t, `CREATE OR REPLACE FUNCTION public.fail_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.payload->>'kind' = 'test.fail_outbox' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END $$`)
	pgtest.Exec(t, `CREATE TRIGGER fail_event BEFORE INSERT ON platform.outbox FOR EACH ROW EXECUTE FUNCTION public.fail_event()`)
	t.Cleanup(func() {
		pgtest.Exec(t, `DROP TRIGGER fail_kind ON platform.operation`)
		pgtest.Exec(t, `DROP TRIGGER fail_event ON platform.outbox`)
	})
	for kind, withAudit := range map[string]bool{"test.fail_update": true, "test.fail_outbox": true, "test.no_audit": false} {
		t.Run(kind, func(t *testing.T) {
			o := start(t, ten, kind)
			err := inRequest(t, ten, ten.Human, withAudit, func(ctx context.Context) error {
				_, err := e.CancelOperation(ctx, gen.CancelOperationParams{ID: o.ID, IfMatch: `"1"`})
				return err
			})
			if err == nil {
				t.Errorf("got nil error, want the failure reported")
			}
		})
	}
}

func TestSucceed(t *testing.T) {
	ten := pgtest.NewTenant(t, perms...)
	if _, err := operations.Succeed(context.Background(), uuid.New(), "/v1/x", nil); err == nil {
		t.Error("Succeed outside a request")
	}
	if err := operations.Report(context.Background(), uuid.New(), 1, 2); err == nil {
		t.Error("Report outside a request")
	}
	if err := inRequest(t, ten, ten.Human, false, func(ctx context.Context) error {
		o, err := operations.Start(ctx, "payables.capture", nil)
		if err != nil {
			return err
		}
		if _, err := operations.Succeed(ctx, o.ID, "/v1/x", make(chan int)); err == nil {
			t.Error("unencodable result accepted")
		}
		done, err := operations.Succeed(ctx, o.ID, "/v1/supplier-invoices/1", map[string]any{"fields": 3})
		if err != nil {
			return err
		}
		if done.Status != operations.Succeeded {
			t.Errorf("succeeded status: got %v, want %v", done.Status, operations.Succeeded)
		}
		if *done.Resource != "/v1/supplier-invoices/1" {
			t.Errorf("succeeded resource: got %s, want /v1/supplier-invoices/1", *done.Resource)
		}
		if string(done.Result) != `{"fields": 3}` {
			t.Errorf("succeeded result: got %s, want %s", done.Result, `{"fields": 3}`)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	apitest.EveryFailure(t, ten, ten.Human, "succeed", func(ctx context.Context) error {
		o, err := operations.Start(ctx, "payables.capture", nil)
		if err != nil {
			return err
		}
		_, err = operations.Succeed(ctx, o.ID, "/v1/x", nil)
		return err
	})
}

// TestFinishOnce: an operation finishes once. Succeeding or failing one that
// has already succeeded, failed or been cancelled is a conflict, and
// progress reported on it is ignored: neither changes it or publishes a
// second completion.
func TestFinishOnce(t *testing.T) {
	ten := pgtest.NewTenant(t, perms...)
	why := problem.Unprocessable("payments.bank_file_invalid", "test")
	type finisher func(ctx context.Context, id uuid.UUID) error
	succeed := func(ctx context.Context, id uuid.UUID) error {
		_, err := operations.Succeed(ctx, id, "/v1/x", nil)
		return err
	}
	fail := func(ctx context.Context, id uuid.UUID) error {
		_, err := operations.Fail(ctx, id, why)
		return err
	}
	cancel := func(_ context.Context, id uuid.UUID) error {
		pgtest.Exec(t, `UPDATE platform.operation SET status = 'cancelled', version = version + 1 WHERE id = $1`, id)
		return nil
	}
	state := func(t *testing.T, id uuid.UUID) (status string, version int32, events int) {
		t.Helper()
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT status, version, (SELECT count(*) FROM platform.outbox x
			WHERE x.tenant_id = o.tenant_id AND x.aggregate_id = o.id) FROM platform.operation o WHERE id = $1`, id).Scan(&status, &version, &events); err != nil {
			t.Fatalf("read the operation: %v", err)
		}
		return status, version, events
	}
	for first, finish := range map[string]finisher{"succeeded": succeed, "failed": fail, "cancelled": cancel} {
		for again, finishAgain := range map[string]finisher{"succeed": succeed, "fail": fail} {
			t.Run(first+" then "+again, func(t *testing.T) {
				o := start(t, ten, "payables.capture")
				if err := inRequest(t, ten, ten.Human, false, func(ctx context.Context) error { return finish(ctx, o.ID) }); err != nil {
					t.Fatal(err)
				}
				status, version, events := state(t, o.ID)
				err := inRequest(t, ten, ten.Human, false, func(ctx context.Context) error { return finishAgain(ctx, o.ID) })
				var p *problem.Problem
				if !errors.As(err, &p) || p.Code != "platform.operation_finished" || !strings.Contains(p.Detail, first) {
					t.Fatalf("finish again: got %v, want platform.operation_finished naming %s", err, first)
				}
				if err := inRequest(t, ten, ten.Human, false, func(ctx context.Context) error { return operations.Report(ctx, o.ID, 1, 2) }); err != nil {
					t.Fatalf("report on a finished operation: got %v, want nil", err)
				}
				gotStatus, gotVersion, gotEvents := state(t, o.ID)
				if gotStatus != status {
					t.Errorf("status: got %s, want %s", gotStatus, status)
				}
				if gotVersion != version {
					t.Errorf("version: got %d, want %d", gotVersion, version)
				}
				if gotEvents != events {
					t.Errorf("completion events: got %d, want %d", gotEvents, events)
				}
			})
		}
	}
	pgtest.WantErr(t, inRequest(t, ten, ten.Human, false, func(ctx context.Context) error { return succeed(ctx, uuid.New()) }), "No operation")
}
