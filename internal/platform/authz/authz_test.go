package authz_test

import (
	"context"
	"errors"
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
		if !set["platform.operation.read"] || set["platform.operation.cancel"] || set["audit.event.read"] {
			t.Errorf("%v", set)
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
		if err != nil || a.All || !a.Allows(le) || a.Allows(ended) || a.Allows(uuid.New()) {
			t.Errorf("agent: %+v %v", a, err)
		}
	})
	as(t, ten, ten.Service, func(ctx context.Context) {
		if a, err := authz.Companies(ctx); err != nil || a.All || len(a.IDs) != 0 || a.Allows(le) {
			t.Errorf("service: %+v %v", a, err)
		}
	})
	as(t, ten, ten.Human, func(ctx context.Context) {
		if a, err := authz.Companies(ctx); err != nil || !a.All || !a.Allows(uuid.New()) {
			t.Errorf("human: %+v %v", a, err)
		}
	})
	if _, err := authz.Companies(context.Background()); err == nil {
		t.Error("company access outside a request")
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
			if p := problem.From(a.Authorize(ctx, op)); p.Code != "identity.sod_conflict" {
				t.Errorf("%s: %v", op, p)
			}
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
		err := pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: ten.ID, ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
			ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}), reqctx.Principal{ID: ten.Human.ID, Kind: "human", TenantID: ten.ID})
			return a.Authorize(ctx, "getOperation")
		})
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("statement %d: %v", n, err)
		}
	}
}
