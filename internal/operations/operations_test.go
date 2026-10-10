package operations_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
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
	if got["id"] != o.ID.String() || got["status"] != "queued" || got["kind"] != "payments.propose" || got["resource"] != nil ||
		got["progress"].(map[string]any)["total"] != nil || got["created_by"] != ten.Human.ID.String() {
		t.Fatalf("%v", got)
	}
	// A finished operation shows its result and error.
	failed := start(t, ten, "integration.migrate")
	pgtest.Exec(t, `UPDATE platform.operation SET status = 'failed', progress_done = 5, progress_total = 10, resource = '/v1/migration-runs/x',
		result = '{"loaded": 5}', error = '{"type":"t","title":"Failed","status":422,"code":"integration.reconciliation_failed"}', version = version + 1
		WHERE id = $1`, failed.ID)
	got = apitest.JSON(t, api.Do(apitest.Req{Method: "GET", Path: "/v1/operations/" + failed.ID.String(), Tenant: ten.ID, As: ten.Human}))
	if got["resource"] != "/v1/migration-runs/x" || got["progress"].(map[string]any)["total"] != float64(10) ||
		got["result"].(map[string]any)["loaded"] != float64(5) || got["error"].(map[string]any)["code"] != "integration.reconciliation_failed" {
		t.Fatalf("%v", got)
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
		r := api.Do(c.req)
		if r.Code != c.code || apitest.JSON(t, r)["code"] != c.pc || r.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s: %d %s", c.name, r.Code, r.Body)
		}
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
	if got := itemIDs(first); len(got) != 2 || got[0] != ids[2] || got[1] != ids[1] {
		t.Fatalf("newest first: %v (ids %v)", got, ids)
	}
	tok := first["next_page_token"].(string)
	second := list("?kind=payments.propose&page_size=2&page_token=" + tok)
	if got := itemIDs(second); len(got) != 1 || got[0] != ids[0] || second["next_page_token"] != nil {
		t.Fatalf("second page: %v", second)
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
		r := api.Do(apitest.Req{Method: "GET", Path: "/v1/operations" + query, Tenant: ten.ID, As: ten.Human})
		if r.Code != 400 || apitest.JSON(t, r)["code"] != code {
			t.Errorf("%s: %d %s", query, r.Code, r.Body)
		}
	}
}

func TestCancelOperation(t *testing.T) {
	api := apitest.New(t)
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
	if r.Code != 200 || r.Header().Get("ETag") != `"2"` || got["status"] != "cancelled" || got["updated_by"] != ten.Agent.ID.String() {
		t.Fatalf("cancel: %d %v", r.Code, got)
	}
	// A retry with the same key returns the same answer and changes nothing.
	replay := api.Do(apitest.Req{Method: "POST", Path: path, Tenant: ten.ID, As: ten.Agent, Key: key, IfMatch: `"1"`})
	if replay.Code != 200 || replay.Header().Get("Idempotent-Replayed") != "true" || replay.Header().Get("ETag") != `"2"` {
		t.Fatalf("replay: %d %v", replay.Code, replay.Header())
	}
	if code, body := cancel(apitest.NewKey(), `"2"`); code != 409 || body["code"] != "platform.operation_not_cancellable" {
		t.Fatalf("cancel twice: %d %v", code, body)
	}
	// The change was audited once and published once, as the agent.
	var audits, events int
	var payload []byte
	admin := pgtest.Admin(t)
	admin.QueryRow(context.Background(), `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'platform.operation.cancel'
		AND object_id = $2 AND actor_id = $3 AND actor_kind = 'agent'`, ten.ID, o.ID, ten.Agent.ID).Scan(&audits)
	admin.QueryRow(context.Background(), `SELECT count(*), max(payload::text) FROM platform.outbox WHERE tenant_id = $1 AND aggregate_id = $2
		AND event_type = 'com.example.erp.platform.operation_completed.v1' AND actor_id = $3`, ten.ID, o.ID, ten.Agent.ID).Scan(&events, &payload)
	if audits != 1 || events != 1 {
		t.Fatalf("audits %d events %d", audits, events)
	}
	var ev map[string]any
	json.Unmarshal(payload, &ev)
	if ev["status"] != "cancelled" || ev["operation_id"] != o.ID.String() || ev["error"] != nil {
		t.Fatalf("event %v", ev)
	}
	// A running operation that recorded an error carries it in its completion event.
	running := start(t, ten, "integration.migrate")
	pgtest.Exec(t, `UPDATE platform.operation SET status = 'running', version = version + 1,
		error = '{"code":"integration.source_unavailable","detail":"SAP not reachable"}' WHERE id = $1`, running.ID)
	r = api.Do(apitest.Req{Method: "POST", Path: "/v1/operations/" + running.ID.String() + ":cancel", Tenant: ten.ID, As: ten.Human, Key: apitest.NewKey(), IfMatch: `"2"`})
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	admin.QueryRow(context.Background(), `SELECT payload::text FROM platform.outbox WHERE aggregate_id = $1`, running.ID).Scan(&payload)
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
		o := start(t, ten, kind)
		err := inRequest(t, ten, ten.Human, withAudit, func(ctx context.Context) error {
			_, err := e.CancelOperation(ctx, gen.CancelOperationParams{ID: o.ID, IfMatch: `"1"`})
			return err
		})
		if err == nil {
			t.Errorf("%s: failure not reported", kind)
		}
	}
}

func TestSucceed(t *testing.T) {
	ten := pgtest.NewTenant(t, perms...)
	if _, err := operations.Succeed(context.Background(), uuid.New(), "/v1/x", nil); err == nil {
		t.Error("Succeed outside a request")
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
		if err == nil && (done.Status != operations.Succeeded || *done.Resource != "/v1/supplier-invoices/1" || string(done.Result) != `{"fields": 3}`) {
			t.Errorf("succeeded: %+v %s", done, done.Result)
		}
		return err
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
