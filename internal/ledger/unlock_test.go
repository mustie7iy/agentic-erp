package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// opener opens no approval request: it answers with a fixed request id.
type opener struct {
	id     uuid.UUID
	needed bool
}

func (o opener) Open(context.Context, approvals.NewRequest) (uuid.UUID, bool, error) {
	return o.id, o.needed, nil
}

// firstPeriod returns the path of period 1 of 2026 of legal entity 2000.
func (e env) firstPeriod(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/fiscal-periods?legal_entity_id="+e.de.String()+"&fiscal_year=2026&order_by=period&page_size=1", "", "")
	id := uuid.MustParse(page["items"].([]any)[0].(map[string]any)["id"].(string))
	return id, "/v1/fiscal-periods/" + id.String()
}

// TestRequestUnlock: only a locked period with no pending unlock request
// asks for one, people only; the period then names its request. Approving
// it is tested end to end in internal/app (TestPeriodUnlock).
func TestRequestUnlock(t *testing.T) {
	e := setup(t)
	id, path := e.firstPeriod(t)
	reason := `{"reason": "Audit adjustment"}`
	_, tag := e.Must(200, "GET", path, "", "")
	e.Problem(409, "ledger.invalid_period_status", "POST", path+":unlock", reason, tag)
	_, tag = e.Must(200, "POST", path+":close", "", tag)
	_, tag = e.Must(200, "POST", path+":lock", "", tag)
	agent := e.Client
	agent.As = e.ten.Agent
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+":unlock", reason, tag)
	e.Problem(412, "request.version_mismatch", "POST", path+":unlock", reason, `"1"`)
	e.Problem(400, "request.invalid_body", "POST", path+":unlock", `{"reason": ""}`, tag)
	// The approval policy's role must exist for the request to open.
	e.Problem(422, "workflow.unknown_role", "POST", path+":unlock", reason, tag)
	pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, $2, 'active', $3, $3)`,
		e.ten.ID, approvals.DefaultPeriodUnlockRole, e.ten.Human.ID)
	p, tag := e.Must(200, "POST", path+":unlock", reason, tag)
	if p["status"] != "locked" {
		t.Errorf("status: got %v, want locked", p["status"])
	}
	if _, ok := p["unlock_request_id"].(string); !ok {
		t.Fatalf("unlock_request_id: got %v, want the pending request", p["unlock_request_id"])
	}
	e.Problem(409, "ledger.unlock_pending", "POST", path+":unlock", reason, tag)
	for _, op := range []string{":open", ":close"} {
		e.Problem(409, "ledger.invalid_period_status", "POST", path+op, "", tag)
	}
	// A policy that needs no approval would unlock without a decision.
	pgtest.Exec(t, `UPDATE ledger.period_status SET unlock_request_id = NULL, version = version + 1 WHERE id = $1`, id)
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := ledger.RequestUnlock(ctx, opener{uuid.New(), false}, id, func(int32) error { return nil }, "r")
		return err
	})
	if err == nil {
		t.Error("an unlock that needs no approval: got no error, want one")
	}
}

// TestUnlockCallbacks: the approvals callbacks act only on the request the
// period names, and only while it is locked.
func TestUnlockCallbacks(t *testing.T) {
	e := setup(t)
	id, _ := e.firstPeriod(t)
	request := uuid.New()
	name := func() {
		t.Helper()
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE id = $1 AND status = 'open'`, id)
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'locked', version = version + 1 WHERE id = $1 AND status = 'closed'`, id)
		pgtest.Exec(t, `UPDATE ledger.period_status SET unlock_request_id = $2, version = version + 1 WHERE id = $1`, id, request)
	}
	h := ledger.PeriodUnlocks{}
	call := func(fn func(ctx context.Context) error) error {
		t.Helper()
		return apitest.Committed(t, e.ten, e.ten.Human, fn)
	}
	// is checks the period's status and that it names no request.
	is := func(when, want string) {
		t.Helper()
		var status string
		var named *uuid.UUID
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT status, unlock_request_id FROM ledger.period_status WHERE id = $1`, id).Scan(&status, &named); err != nil {
			t.Fatal(err)
		}
		if status != want {
			t.Errorf("%s: status got %s, want %s", when, status, want)
		}
		if named != nil {
			t.Errorf("%s: unlock_request_id got %v, want none", when, named)
		}
	}
	name()
	for what, fn := range map[string]func(ctx context.Context) error{
		"approving another request":  func(ctx context.Context) error { return h.Approved(ctx, id, uuid.New()) },
		"rejecting another request":  func(ctx context.Context) error { return h.Rejected(ctx, id, uuid.New(), "c") },
		"cancelling another request": func(ctx context.Context) error { return h.Cancelled(ctx, id, uuid.New(), "c") },
		"approving another period":   func(ctx context.Context) error { return h.Approved(ctx, uuid.New(), request) },
	} {
		t.Run(what, func(t *testing.T) {
			if err := call(fn); err == nil {
				t.Error("got no error, want one")
			}
		})
	}
	if err := call(func(ctx context.Context) error { return h.Cancelled(ctx, id, request, "withdrawn") }); err != nil {
		t.Fatalf("cancelled: %v", err)
	}
	is("cancelled", "locked")
	name()
	if err := call(func(ctx context.Context) error { return h.Approved(ctx, id, request) }); err != nil {
		t.Fatalf("approved: %v", err)
	}
	is("approved", "closed")
	// A second callback for the same request finds the period closed.
	if err := call(func(ctx context.Context) error { return h.Approved(ctx, id, request) }); err == nil {
		t.Error("approved twice: got no error, want one")
	}
	if got := events(t, e.ten, "period_unlocked"); len(got) != 1 {
		t.Errorf("period_unlocked events: got %d, want 1", len(got))
	}
}
