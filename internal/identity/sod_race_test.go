package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestConcurrentAssignmentsKeepSegregation: two assignments that conflict
// only together, made at the same time to one principal, cannot both
// commit. The second waits for the first's segregation-of-duties lock and
// its check then sees the first's assignment, so it is refused.
func TestConcurrentAssignmentsKeepSegregation(t *testing.T) {
	c, ten := admin(t)
	capture := uuid.MustParse(role(t, c, "CAPTURE", "payables.invoice.capture"))
	post := uuid.MustParse(role(t, c, "POST", "payables.invoice.post"))
	c.Must(201, "POST", "/v1/sod-rules", `{"permission_a": "payables.invoice.post", "permission_b": "payables.invoice.capture",
		"description": "Who captures an invoice does not post it"}`, "")
	pool := pgtest.Pool(t)
	bg := context.Background()
	begin := func() (context.Context, pgx.Tx, error) {
		tx, err := pool.Begin(bg, database.Scope{Role: "erp_app", TenantID: ten.ID})
		if err != nil {
			return nil, nil, err
		}
		ctx, _ := audit.WithBuffer(reqctx.WithPrincipal(reqctx.WithTx(bg, tx), reqctx.Principal{ID: ten.Human.ID, Kind: "human", TenantID: ten.ID}))
		return ctx, tx, nil
	}
	first, tx1, err := begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx1.Rollback(bg)
	if _, err := identity.AssignRole(first, ten.Agent.ID, capture, nil, nil); err != nil {
		t.Fatalf("first assignment: %v", err)
	}
	second := make(chan error, 1)
	go func() {
		ctx, tx2, err := begin()
		if err != nil {
			second <- err
			return
		}
		defer tx2.Rollback(bg)
		_, err = identity.AssignRole(ctx, ten.Agent.ID, post, nil, nil)
		second <- err
	}()
	pgtest.Eventually(t, 30*time.Second, func() bool {
		var waiting int
		pgtest.Scan(t, `SELECT count(*) FROM pg_locks l, (SELECT hashtextextended('identity.sod:' || $1::text, 0) AS k) h
			WHERE l.locktype = 'advisory' AND NOT l.granted AND l.classid::bigint = (h.k >> 32) & 4294967295 AND l.objid::bigint = h.k & 4294967295`,
			[]any{ten.ID}, &waiting)
		return waiting > 0
	}, "the second assignment never waited for the first's lock")
	if err := tx1.Commit(bg); err != nil {
		t.Fatalf("commit the first assignment: %v", err)
	}
	if err := <-second; problem.From(err).Code != "identity.sod_conflict" {
		t.Fatalf("second assignment: got %v, want identity.sod_conflict", err)
	}
}
