package procurement_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// TestApproveAndCloseAtOnce (ADR-0034): the decision on an order change's
// request locks the request, then the order; closing the order locks the
// order, then withdraws the request. Run at once they deadlock, which
// PostgreSQL detects and ends by aborting one of them (40P01, a retryable
// 503); the other completes, and the order and the change agree either
// way.
func TestApproveAndCloseAtOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	order := e.big(t, "90.00")
	path := "/v1/purchase-orders/" + order
	e.act(t, path, "release", "")
	ch := e.submitted(t, path, items(item(e.office, "10", "EA", "150.00", "EUR", ""))) // waits for approval
	change, request := uuidOf(ch), uuid.MustParse(ch["approval_request_id"].(string))

	// The decision, as the Decider takes it: the request first.
	decide, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: e.Tenant.ID})
	if err != nil {
		t.Fatalf("begin the decision: %v", err)
	}
	closed := make(chan int, 1)
	drained := false
	defer func() { // release the close, then wait for it, however the test ends
		_ = decide.Rollback(context.WithoutCancel(ctx)) // after a commit there is nothing to roll back
		if !drained {
			<-closed
		}
	}()
	if err := database.SetActor(ctx, decide, e.Approver.ID); err != nil {
		t.Fatalf("decision actor: %v", err)
	}
	dctx := reqctx.WithPrincipal(reqctx.WithTx(ctx, decide), reqctx.Principal{ID: e.Approver.ID, Kind: e.Approver.Kind, TenantID: e.Tenant.ID})
	dctx, _ = audit.WithBuffer(dctx)
	if _, err := decide.Exec(ctx, `SELECT FROM workflow.approval_request WHERE id = $1 FOR UPDATE`, request); err != nil {
		t.Fatalf("lock the request: %v", err)
	}
	// The close, through the API: the order, then the request. The token is
	// minted here, so that nothing on the goroutine reports to t.
	auth := http.Header{"Authorization": {"Bearer " + e.API.Issuer.Token(t, e.Tenant.ID, e.Tenant.Human.Subject, nil)}}
	go func() {
		closed <- e.API.Do(apitest.Req{Method: "POST", Path: path + ":close", Key: apitest.NewKey(), IfMatch: "*", Header: auth}).Code
	}()
	admin := pgtest.Admin(t)
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var waiting bool
		err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query LIKE '%workflow.approval_request%FOR UPDATE%')`).Scan(&waiting)
		return err == nil && waiting
	}, "the close never waited for the request being decided")
	approveErr := procurement.OrderChanges{Endpoints: endpoints()}.Approved(dctx, change, request)
	if approveErr == nil {
		if _, err := decide.Exec(ctx, `UPDATE workflow.approval_request SET status = 'approved', completed_at = now(), version = version + 1 WHERE id = $1`,
			request); err != nil {
			t.Fatalf("complete the request: %v", err)
		}
		if err := decide.Commit(ctx); err != nil {
			t.Fatalf("commit the decision: %v", err)
		}
	}
	closeCode := <-closed
	drained = true
	var pg *pgconn.PgError
	approveLost := errors.As(approveErr, &pg) && pg.Code == "40P01"
	switch {
	case approveLost && closeCode == 200: // the close went on; the decision is refused when retried
		r, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+request.String(), "", "")
		e.As(e.Approver).Problem(409, "workflow.not_pending", "POST", "/v1/approval-requests/"+request.String()+":approve", `{"comment": "ok"}`,
			fmt.Sprintf(`"%v"`, r["version"]))
		agree(t, e, path, change, "cancelled", "cancelled", 1)
	case approveErr == nil && closeCode == 503: // the decision went on; the close succeeds when retried
		e.act(t, path, "close", "")
		agree(t, e, path, change, "applied", "approved", 2)
	default:
		t.Fatalf("approve and close at once: got approve %v and close %d, want one of them aborted by the deadlock (40P01 / 503)", approveErr, closeCode)
	}
}

// agree checks that the order is closed and its change and request agree.
func agree(t *testing.T, e env, path string, change uuid.UUID, changeStatus, requestStatus string, revision float64) {
	t.Helper()
	order, _ := e.Must(200, "GET", path, "", "")
	want(t, "order", order, map[string]any{"status": "closed", "revision": revision})
	ch, _ := e.Must(200, "GET", path+"/changes/"+change.String(), "", "")
	want(t, "change", ch, map[string]any{"status": changeStatus})
	r, _ := e.As(e.Approver).Must(200, "GET", "/v1/approval-requests/"+ch["approval_request_id"].(string), "", "")
	want(t, "request", r, map[string]any{"status": requestStatus})
}
