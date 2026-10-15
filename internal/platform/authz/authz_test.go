package authz_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func as(t *testing.T, ten pgtest.Tenant, a pgtest.Actor, fn func(ctx context.Context)) {
	t.Helper()
	pool := pgtest.Pool(t)
	err := pool.InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID, ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		ctx = reqctx.WithTx(ctx, tx)
		ctx = reqctx.WithPrincipal(ctx, reqctx.Principal{ID: a.ID, Kind: a.Kind, TenantID: ten.ID})
		fn(ctx)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func status(err error) int {
	if err == nil {
		return 0
	}
	return problem.From(err).Status
}

func TestAuthorizeAgainstTheContract(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	a := authz.New(ops)
	ten := pgtest.NewTenant(t, "platform.operation.read", "workflow.request.decide")
	as(t, ten, ten.Human, func(ctx context.Context) {
		if err := a.Authorize(ctx, "getOperation"); err != nil {
			t.Error(err)
		}
		if err := a.Authorize(ctx, "approveApprovalRequest"); err != nil {
			t.Error(err)
		}
		if s := status(a.Authorize(ctx, "cancelOperation")); s != 403 {
			t.Errorf("cancel without permission: %d", s)
		}
		if err := a.Authorize(ctx, "noSuchOperation"); err == nil {
			t.Error("unknown operation authorized")
		}
	})
	as(t, ten, ten.Agent, func(ctx context.Context) {
		if err := a.Authorize(ctx, "getOperation"); err != nil {
			t.Error(err)
		}
		p := problem.From(a.Authorize(ctx, "approveApprovalRequest"))
		if p.Code != "identity.agent_not_allowed" {
			t.Errorf("agent decision: %s", p.Code)
		}
	})
}

func TestOnlyCurrentGrantsCount(t *testing.T) {
	ten := pgtest.NewTenant(t, "platform.operation.read", "platform.operation.cancel")
	// End the cancel grant today, put a future-dated grant in place, and deactivate nothing else.
	pgtest.Exec(t, `UPDATE identity.role_permission SET valid_to = current_date, ended_by = created_by
		WHERE tenant_id = $1 AND permission_code = 'platform.operation.cancel'`, ten.ID)
	pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
		SELECT tenant_id, gen_random_uuid(), role_id, 'audit.event.read', current_date + 5, created_by FROM identity.role_permission
		WHERE tenant_id = $1 AND permission_code = 'platform.operation.read' LIMIT 1`, ten.ID)
	as(t, ten, ten.Human, func(ctx context.Context) {
		set, err := authz.Permissions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for code, want := range map[string]bool{
			"platform.operation.read":   true,  // current
			"platform.operation.cancel": false, // ended today
			"audit.event.read":          false, // not in force yet
		} {
			if got := set[code]; got != want {
				t.Errorf("%s granted: got %v, want %v", code, got, want)
			}
		}
		if s := status(authz.Require(ctx, "platform.operation.cancel")); s != 403 {
			t.Errorf("ended grant used: %d", s)
		}
	})
	// An inactive role grants nothing.
	pgtest.Exec(t, `UPDATE identity.role SET status = 'inactive', version = version + 1 WHERE tenant_id = $1`, ten.ID)
	as(t, ten, ten.Human, func(ctx context.Context) {
		if set, _ := authz.Permissions(ctx); len(set) != 0 {
			t.Errorf("inactive role grants %v", set)
		}
	})
}

func TestOutsideARequest(t *testing.T) {
	a := authz.New(map[string]api.Operation{"x": {ID: "x", Permission: "a.b.c", AgentAllowed: true}})
	if err := a.Authorize(context.Background(), "x"); err == nil {
		t.Fatal("authorized without a principal")
	}
	if _, err := authz.Permissions(context.Background()); err == nil {
		t.Fatal("permissions without a transaction")
	}
	if err := authz.Require(context.Background(), "a.b.c"); err == nil {
		t.Fatal("required without a transaction")
	}
	// A failed query is reported.
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	tx, _ := pool.Begin(context.Background(), database.Scope{})
	tx.Rollback(context.Background())
	ctx := reqctx.WithPrincipal(reqctx.WithTx(context.Background(), tx), reqctx.Principal{ID: ten.Human.ID, Kind: "human", TenantID: ten.ID})
	if _, err := authz.Permissions(ctx); err == nil {
		t.Fatal("closed transaction accepted")
	}
}

func TestRequireGrantedPermission(t *testing.T) {
	ten := pgtest.NewTenant(t, "platform.operation.read")
	as(t, ten, ten.Human, func(ctx context.Context) {
		if err := authz.Require(ctx, "platform.operation.read"); err != nil {
			t.Error(err)
		}
	})
}

func TestCompanies(t *testing.T) {
	ten := pgtest.NewTenant(t)
	le, ended := uuid.New(), uuid.New()
	pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, current_date, $2)`, ten.ID, ten.Agent.ID, le)
	pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, valid_to, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, current_date - 5, current_date, $2)`, ten.ID, ten.Agent.ID, ended)
	ten.AllCompanies(t, ten.Human)
	as(t, ten, ten.Agent, func(ctx context.Context) {
		a, err := authz.Companies(ctx)
		if err != nil {
			t.Errorf("agent: %v", err)
			return
		}
		if a.All {
			t.Error("agent all companies: got true, want false")
		}
		if !a.Allows(le) {
			t.Error("agent granted company: got denied, want allowed")
		}
		if a.Allows(ended) {
			t.Error("agent ended company: got allowed, want denied")
		}
		if a.Allows(uuid.New()) {
			t.Error("agent unknown company: got allowed, want denied")
		}
	})
	as(t, ten, ten.Service, func(ctx context.Context) {
		a, err := authz.Companies(ctx)
		if err != nil {
			t.Errorf("service: %v", err)
			return
		}
		if a.All {
			t.Error("service all companies: got true, want false")
		}
		if len(a.IDs) != 0 {
			t.Errorf("service companies: got %v, want none", a.IDs)
		}
		if a.Allows(le) {
			t.Error("service company of another principal: got allowed, want denied")
		}
	})
	as(t, ten, ten.Human, func(ctx context.Context) {
		a, err := authz.Companies(ctx)
		if err != nil {
			t.Errorf("human: %v", err)
			return
		}
		if !a.All {
			t.Error("human all companies: got false, want true")
		}
		if !a.Allows(uuid.New()) {
			t.Error("human any company: got denied, want allowed")
		}
	})
	if _, err := authz.Companies(context.Background()); err == nil {
		t.Error("company access outside a request")
	}
}

// TestVisible: a missing row and a row in a company outside the caller's
// access are the same notFound; other read errors are wrapped with context;
// an accessible row is no error.
func TestVisible(t *testing.T) {
	mine, other, id := uuid.New(), uuid.New(), uuid.New()
	access := authz.Access{IDs: []uuid.UUID{mine}}
	notFound := problem.NotFoundOf("thing")
	boom := errors.New("boom")
	cases := []struct {
		name string
		err  error
		le   uuid.UUID
		want error
	}{
		{"accessible", nil, mine, nil},
		{"other company", nil, other, notFound},
		{"no row", fmt.Errorf("scan: %w", pgx.ErrNoRows), mine, notFound},
		{"read error", boom, mine, boom},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := authz.Visible(c.err, access, c.le, notFound, "read thing %s", id)
			if !errors.Is(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	got := authz.Visible(boom, access, mine, notFound, "read thing %s", id)
	if want := "read thing " + id.String() + ": boom"; got.Error() != want {
		t.Errorf("read error: got %q, want %q", got, want)
	}
}

// TestSegregationOfDutiesAtRuntime: a principal holding both permissions of
// a rule in force may use neither; rules not in force today do not count.
func TestSegregationOfDutiesAtRuntime(t *testing.T) {
	a := authz.New(api.MustOperations())
	ten := pgtest.NewTenant(t, "platform.operation.read", "platform.operation.cancel")
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, valid_to, created_by)
		VALUES ($1, gen_random_uuid(), 'platform.operation.cancel', 'platform.operation.read', 'future', current_date + 3, NULL, $2)`, ten.ID, ten.Human.ID)
	as(t, ten, ten.Human, func(ctx context.Context) {
		if err := a.Authorize(ctx, "getOperation"); err != nil {
			t.Errorf("future rule applied: %v", err)
		}
	})
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, valid_to, created_by)
		VALUES ($1, gen_random_uuid(), 'platform.operation.cancel', 'platform.operation.read', 'now', current_date, current_date + 3, $2)`, ten.ID, ten.Human.ID)
	as(t, ten, ten.Human, func(ctx context.Context) {
		for _, op := range []string{"getOperation", "cancelOperation"} {
			t.Run(op, func(t *testing.T) {
				if p := problem.From(a.Authorize(ctx, op)); p.Code != "identity.sod_conflict" {
					t.Errorf("got %v, want identity.sod_conflict", p)
				}
			})
		}
	})
	// Holding only one side of the rule is fine.
	only := pgtest.NewTenant(t, "platform.operation.read")
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), 'platform.operation.cancel', 'platform.operation.read', 'd', current_date, $2)`, only.ID, only.Human.ID)
	as(t, only, only.Human, func(ctx context.Context) {
		if err := a.Authorize(ctx, "getOperation"); err != nil {
			t.Error(err)
		}
	})
	// Database failures while authorizing are reported.
	for n := 1; n <= 2; n++ {
		t.Run(fmt.Sprintf("statement %d fails", n), func(t *testing.T) {
			err := pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID, ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
				ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}), reqctx.Principal{ID: ten.Human.ID, Kind: "human", TenantID: ten.ID})
				return a.Authorize(ctx, "getOperation")
			})
			if !errors.Is(err, pgtest.ErrInjected) {
				t.Errorf("got %v, want %v", err, pgtest.ErrInjected)
			}
		})
	}
}
