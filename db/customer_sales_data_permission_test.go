package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestCustomerSalesDataPermission (migration 000390; ADR-0067 §7 and As
// built; product owner, 2026-11-18): customers' sales data have their own
// read and write permissions, neither an approval one, and on a fresh
// database no role holds them.
func TestCustomerSalesDataPermission(t *testing.T) {
	need(t)
	for _, code := range []string{"sales.customer_sales_data.read", "sales.customer_sales_data.write"} {
		t.Run(code, func(t *testing.T) {
			var approval bool
			var roles int
			if err := testDB.QueryRow(context.Background(), `SELECT is_approval, (SELECT count(*) FROM identity.role_permission WHERE permission_code = p.code)
				FROM ref.permission p WHERE code = $1`, code).Scan(&approval, &roles); err != nil {
				t.Fatalf("%s: %v", code, err)
			}
			if approval {
				t.Errorf("%s is an approval permission, want one that is not", code)
			}
			if roles != 0 {
				t.Errorf("roles holding %s on a fresh database: got %d, want 0", code, roles)
			}
		})
	}
}

// TestCustomerSalesDataPermissionExpand (migration 000390): the expand
// step copies every grant of sales.price.read or .write in force today or
// from a later day to the new permission, in every tenant, for the same
// days from today at the earliest and in the name of whoever made the
// grant, each permission from its own grant; a grant ended today or
// before and other permissions are not copied. Its down
// removes the new permissions, their grants and the rules naming them.
func TestCustomerSalesDataPermissionExpand(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := t.Context()
	migrateBefore(t, conn, "000390_customer_sales_data_permission")
	tenants, actor := []uuid.UUID{id(), id()}, id()
	const role = `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, $2, $3, $3, 'active', $4, $4)`
	const grant = `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, valid_to, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, current_date + $4::int, current_date + $5::int, $6)`
	never := (*int)(nil)
	days := func(n int) *int { return &n }
	roles := map[string]uuid.UUID{}
	for _, g := range []struct {
		tenant         uuid.UUID
		code, perm     string
		from           int
		to             *int
		createdByActor bool
	}{
		{tenants[0], "pricing", "sales.price.read", -30, never, true},
		{tenants[0], "pricing", "sales.price.write", -30, never, true},
		{tenants[0], "sales_admin", "sales.price.write", -30, days(20), false},
		{tenants[0], "former", "sales.price.write", -30, days(0), true},
		{tenants[0], "next_month", "sales.price.read", 10, never, true},
		{tenants[0], "orders", "sales.order.write", -30, never, true},
		{tenants[0], "mixed", "sales.price.read", -30, never, true},
		{tenants[0], "mixed", "sales.price.write", 5, days(40), true},
		{tenants[0], "long_gone", "sales.price.read", -30, days(-1), true},
		{tenants[1], "pricing", "sales.price.read", -5, never, true},
	} {
		key := g.tenant.String() + "/" + g.code
		roleID, ok := roles[key]
		if !ok {
			roleID = id()
			roles[key] = roleID
			if _, err := conn.Exec(ctx, role, g.tenant, roleID, g.code, actor); err != nil {
				t.Fatalf("role %s: %v", g.code, err)
			}
		}
		by := actor
		if !g.createdByActor {
			by = g.tenant
		}
		if _, err := conn.Exec(ctx, grant, g.tenant, roleID, g.perm, g.from, g.to, by); err != nil {
			t.Fatalf("grant %s to %s: %v", g.perm, g.code, err)
		}
	}
	if _, err := migrate(ctx, conn, Migrations()); err != nil {
		t.Fatal(err)
	}
	const copied = `SELECT coalesce(string_agg(t.n || '/' || r.code || ':' || substr(rp.permission_code, 27) || ' ' || (rp.valid_from - current_date)
		|| '..' || coalesce((rp.valid_to - current_date)::text, '') || ' ' || CASE WHEN rp.created_by = $2 THEN 'actor' ELSE 'tenant' END, ', '
		ORDER BY t.n, r.code, rp.permission_code), '')
		FROM identity.role_permission rp JOIN identity.role r ON r.tenant_id = rp.tenant_id AND r.id = rp.role_id
		JOIN unnest($1::uuid[]) WITH ORDINALITY AS t(id, n) ON t.id = rp.tenant_id
		WHERE rp.permission_code LIKE 'sales.customer_sales_data.%'`
	var got string
	if err := conn.QueryRow(ctx, copied, tenants, actor).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "1/mixed:read 0.. actor, 1/mixed:write 5..40 actor, 1/next_month:read 10.. actor, 1/pricing:read 0.. actor, 1/pricing:write 0.. actor, 1/sales_admin:write 0..20 tenant, 2/pricing:read 0.. actor"; got != want {
		t.Errorf("the grants copied: got %q, want %q", got, want)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// The new permissions on either side of a rule (permission_a < permission_b).
	if _, err := tx.Exec(ctx, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), 'sales.customer_sales_data.write', 'sales.price.write', 'Prices apart from price groups', current_date, $2),
		($1, gen_random_uuid(), 'sales.credit_memo_request.read', 'sales.customer_sales_data.read', 'Credits apart from price groups', current_date, $2)`,
		tenants[0], actor); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, migrationNamed(t, "000390_customer_sales_data_permission").Down); err != nil {
		t.Fatalf("down: %v", err)
	}
	for what, sql := range map[string]string{
		"grants":      `SELECT count(*) FROM identity.role_permission WHERE permission_code LIKE 'sales.customer_sales_data.%'`,
		"rules":       `SELECT count(*) FROM identity.sod_rule WHERE permission_a LIKE 'sales.customer_sales_data.%' OR permission_b LIKE 'sales.customer_sales_data.%'`,
		"permissions": `SELECT count(*) FROM ref.permission WHERE code LIKE 'sales.customer_sales_data.%'`,
	} {
		var n int
		if err := tx.QueryRow(ctx, sql).Scan(&n); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if n != 0 {
			t.Errorf("%s after the down: got %d, want 0", what, n)
		}
	}
}
