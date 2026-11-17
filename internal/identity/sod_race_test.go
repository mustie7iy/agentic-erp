package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
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

// TestConcurrentApprovalGrantKeepsAgentsOut: an approval permission added
// to a role while the role is given to an agent cannot both commit. The
// agent-approval check of the second runs under the first's lock and
// sees its row, so the second is refused.
func TestConcurrentApprovalGrantKeepsAgentsOut(t *testing.T) {
	c, ten := admin(t)
	helper := uuid.MustParse(role(t, c, "HELPER", "payables.invoice.read"))
	first, second := apitest.Race(t, ten, ten.Human, ten.Human,
		func(ctx context.Context) error {
			_, err := identity.GrantPermission(ctx, helper, "payables.invoice.approve", nil, nil)
			return err
		},
		func(ctx context.Context) error {
			_, err := identity.AssignRole(ctx, ten.Agent.ID, helper, nil, nil)
			return err
		})
	if first != nil {
		t.Fatalf("approval permission: got %v, want it granted", first)
	}
	if got := problem.From(second).Code; got != "identity.agent_approval_permission" {
		t.Fatalf("agent's assignment: got %v (%s), want identity.agent_approval_permission", second, got)
	}
}

// TestConcurrentBlocksLeaveOneActive: two administrators blocking each
// other at once cannot both succeed. The second waits for the first's
// lock on both principals and then finds itself blocked.
func TestConcurrentBlocksLeaveOneActive(t *testing.T) {
	c, ten := admin(t)
	other, _ := c.Must(201, "POST", "/v1/principals", `{"kind": "human", "external_subject": "auth0|second-admin", "display_name": "Second admin"}`, "")
	second := pgtest.Actor{ID: uuid.MustParse(id(other)), Kind: "human"}
	block := func(target uuid.UUID) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := identity.BlockPrincipal(ctx, target, func(int32) error { return nil }, "r")
			return err
		}
	}
	firstErr, secondErr := apitest.Race(t, ten, ten.Human, second, block(second.ID), block(ten.Human.ID))
	if firstErr != nil {
		t.Fatalf("first block: got %v, want it made", firstErr)
	}
	if got := problem.From(secondErr).Code; got != "identity.principal_blocked" {
		t.Fatalf("second block: got %v (%s), want identity.principal_blocked", secondErr, got)
	}
	var active int
	pgtest.Scan(t, `SELECT count(*) FROM identity.principal WHERE id IN ($1, $2) AND status = 'active'`, []any{ten.Human.ID, second.ID}, &active)
	if active != 1 {
		t.Errorf("got %d of the two administrators active, want 1", active)
	}
}

// TestConcurrentSelfBlocksDoNotDeadlock: an administrator blocking
// themselves twice at once gets the 409 both times. The request takes its
// row FOR UPDATE at once; holding it FOR SHARE first and then upgrading,
// as the other request (played here by tx) does, deadlocked.
func TestConcurrentSelfBlocksDoNotDeadlock(t *testing.T) {
	_, ten := admin(t)
	pool, bg := pgtest.Pool(t), context.Background()
	tx, err := pool.Begin(bg, database.Scope{Role: "erp_app", TenantID: ten.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(bg)
	if _, err := tx.Exec(bg, `SELECT 1 FROM identity.principal WHERE id = $1 FOR SHARE`, ten.Human.ID); err != nil {
		t.Fatalf("hold the row FOR SHARE: %v", err)
	}
	tx2, err := pool.Begin(bg, database.Scope{Role: "erp_app", TenantID: ten.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(bg)
	var pid int32
	if err := tx2.QueryRow(bg, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- uow.Act(bg, tx2, reqctx.Principal{ID: ten.Human.ID, Kind: "human", TenantID: ten.ID}, nil, func(ctx context.Context) error {
			_, err := identity.BlockPrincipal(ctx, ten.Human.ID, func(int32) error { return nil }, "r")
			return err
		})
	}()
	pgtest.Eventually(t, 30*time.Second, func() bool {
		var waiting bool
		pgtest.Scan(t, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock')`, []any{pid}, &waiting)
		return waiting
	}, "the self-block did not wait for the row")
	if _, err := tx.Exec(bg, `SELECT 1 FROM identity.principal WHERE id = $1 FOR UPDATE`, ten.Human.ID); err != nil {
		t.Fatalf("got %v upgrading the other request's lock, want it granted (no deadlock)", err)
	}
	if err := tx.Rollback(bg); err != nil {
		t.Fatal(err)
	}
	if got := problem.From(<-done).Code; got != "identity.cannot_block_self" {
		t.Errorf("self-block: got %s, want identity.cannot_block_self", got)
	}
}
