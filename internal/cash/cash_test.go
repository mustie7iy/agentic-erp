package cash_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// withCash is the prototype's document with a tree of liquidity items, a
// planning group and rules (ADR-0051 §4).
func withCash(t *testing.T) *config.Config {
	t.Helper()
	doc := fixture.Document(t)
	doc.Cash = config.Cash{
		LiquidityItems: []config.LiquidityItem{
			// A child before its parent: the document's order is free.
			{Code: "OP_SUPPLIERS", Name: "Supplier payments", Direction: config.DirectionOutflow, Parent: "OPERATING"},
			{Code: "OPERATING", Name: "Operating", Direction: config.DirectionBoth},
			{Code: "TAX", Name: "Taxes", Direction: config.DirectionOutflow, Parent: "OPERATING"},
			{Code: "REC_CUSTOMERS", Name: "Customer receipts", Direction: config.DirectionInflow, Parent: "OPERATING"},
		},
		PlanningGroups: []config.PlanningGroup{{Code: "TAX_AUTHORITY", Name: "Tax authorities"}, {Code: "DOMESTIC", Name: "Domestic"}},
		Derivation: config.Derivation{PlanningGroups: []config.PlanningGroupDerivation{{Group: "TAX_AUTHORITY", Item: "TAX"}},
			Defaults: config.DerivationDefaults{Outflow: "OP_SUPPLIERS", Inflow: "REC_CUSTOMERS"}},
	}
	return doc
}

// rows reads the tenant's items or groups as "code name direction parent
// active" lines, in code order.
func rows(t *testing.T, ten pgtest.Tenant, query string) []string {
	t.Helper()
	var out []string
	if err := apitest.InRequest(t, ten, ten.Human, func(ctx context.Context) error {
		tx, _, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		r, _ := tx.Query(ctx, query)
		defer r.Close()
		for r.Next() {
			var code, name, direction, parent string
			var active bool
			if err := r.Scan(&code, &name, &direction, &parent, &active); err != nil {
				return err
			}
			out = append(out, fmt.Sprintf("%s %s %s %s %v", code, name, direction, parent, active))
		}
		return r.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

const (
	itemsQuery  = `SELECT code, name, direction, coalesce(parent_code, '-'), is_active FROM cash.liquidity_item ORDER BY code`
	groupsQuery = `SELECT code, name, '-', '-', is_active FROM cash.planning_group ORDER BY code`
)

func wantRows(t *testing.T, what string, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

// TestSetup: activating a document keeps its liquidity items and planning
// groups; one it no longer has becomes inactive, and comes back when a
// later document has it again (ADR-0051 §4).
func TestSetup(t *testing.T) {
	f := fixture.NewWith(t, withCash(t))
	wantRows(t, "items after bootstrap", rows(t, f.Tenant, itemsQuery), []string{
		"OPERATING Operating both - true", "OP_SUPPLIERS Supplier payments outflow OPERATING true",
		"REC_CUSTOMERS Customer receipts inflow OPERATING true", "TAX Taxes outflow OPERATING true"})
	wantRows(t, "groups after bootstrap", rows(t, f.Tenant, groupsQuery), []string{"DOMESTIC Domestic - - true", "TAX_AUTHORITY Tax authorities - - true"})

	apply := func(doc *config.Config) {
		t.Helper()
		if err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error { return cash.Setup{}.Apply(ctx, doc) }); err != nil {
			t.Fatal(err)
		}
	}
	next := withCash(t)
	next.Cash.LiquidityItems = append(next.Cash.LiquidityItems[1:3], config.LiquidityItem{Code: "SUPPLIERS", Name: "Suppliers",
		Direction: config.DirectionOutflow})
	next.Cash.LiquidityItems[1].Name, next.Cash.LiquidityItems[1].Parent = "Tax payments", ""
	next.Cash.PlanningGroups = next.Cash.PlanningGroups[:1]
	apply(next)
	wantRows(t, "items after a change", rows(t, f.Tenant, itemsQuery), []string{
		"OPERATING Operating both - true", "OP_SUPPLIERS Supplier payments outflow OPERATING false",
		"REC_CUSTOMERS Customer receipts inflow OPERATING false", "SUPPLIERS Suppliers outflow - true", "TAX Tax payments outflow - true"})
	wantRows(t, "groups after a change", rows(t, f.Tenant, groupsQuery), []string{"DOMESTIC Domestic - - false", "TAX_AUTHORITY Tax authorities - - true"})

	apply(withCash(t))
	wantRows(t, "items back", rows(t, f.Tenant, itemsQuery), []string{
		"OPERATING Operating both - true", "OP_SUPPLIERS Supplier payments outflow OPERATING true",
		"REC_CUSTOMERS Customer receipts inflow OPERATING true", "SUPPLIERS Suppliers outflow - false", "TAX Taxes outflow OPERATING true"})
	var versions int
	pgtest.Scan(t, `SELECT sum(version) FROM cash.liquidity_item WHERE tenant_id = $1`, []any{f.Tenant.ID}, &versions)
	apply(withCash(t))
	var again int
	pgtest.Scan(t, `SELECT sum(version) FROM cash.liquidity_item WHERE tenant_id = $1`, []any{f.Tenant.ID}, &again)
	if again != versions {
		t.Errorf("versions after the same document again: got %d, want %d (nothing changed)", again, versions)
	}

	t.Run("nothing to check against the tenant's data", func(t *testing.T) {
		fs, err := cash.Setup{}.Check(context.Background(), nil, withCash(t))
		if err != nil || fs != nil {
			t.Errorf("Check: got %v, %v, want none", fs, err)
		}
	})
}

// TestPlanningGroups: the partner context's check is of the tenant's
// active planning groups.
func TestPlanningGroups(t *testing.T) {
	f := fixture.NewWith(t, withCash(t))
	next := withCash(t)
	next.Cash.PlanningGroups = next.Cash.PlanningGroups[:1]
	if err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error { return cash.Setup{}.Apply(ctx, next) }); err != nil {
		t.Fatal(err)
	}
	other := fixture.NewWith(t, fixture.Document(t))
	for _, c := range []struct {
		name string
		ten  pgtest.Tenant
		code string
		want bool
	}{
		{"active", f.Tenant, "TAX_AUTHORITY", true},
		{"inactive", f.Tenant, "DOMESTIC", false},
		{"unknown", f.Tenant, "FOREIGN", false},
		{"another tenant's", other.Tenant, "TAX_AUTHORITY", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got bool
			if err := apitest.InRequest(t, c.ten, c.ten.Human, func(ctx context.Context) error {
				var err error
				got, err = cash.PlanningGroups{}.IsPlanningGroup(ctx, c.code)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("IsPlanningGroup(%s): got %v, want %v", c.code, got, c.want)
			}
		})
	}
}

// TestDatabaseFailures calls every function outside a unit of work and with
// each of its database statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	f := fixture.NewWith(t, withCash(t))
	for name, call := range map[string]func(ctx context.Context) error{
		"apply": func(ctx context.Context) error { return cash.Setup{}.Apply(ctx, withCash(t)) },
		"planning group": func(ctx context.Context) error {
			_, err := cash.PlanningGroups{}.IsPlanningGroup(ctx, "TAX_AUTHORITY")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
				t.Errorf("outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
			}
			apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, name, call)
		})
	}
}

// TestServerWiring: the configuration as the server wires it keeps the
// document's cash settings and refuses taking away a planning group a
// supplier has (ADR-0051 §4).
func TestServerWiring(t *testing.T) {
	f := fixture.NewWith(t, withCash(t))
	pgtest.Exec(t, `UPDATE partner.supplier_company SET planning_group = 'DOMESTIC', version = version + 1 WHERE tenant_id = $1 AND partner_id = $2`,
		f.Tenant.ID, f.Supplier)
	activate := func(doc *config.Config) error {
		t.Helper()
		return apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			for _, s := range app.Configuration(app.Config{FieldKeys: apitest.FieldKeys()}).Sections {
				if err := s.Apply(ctx, doc); err != nil {
					return err
				}
			}
			return nil
		})
	}
	without := withCash(t)
	without.Cash.PlanningGroups = without.Cash.PlanningGroups[:1]
	pgtest.WantErr(t, activate(without), "partner.planning_group_in_use")
	renamed := withCash(t)
	renamed.Cash.PlanningGroups[1].Name = "Domestic suppliers"
	if err := activate(renamed); err != nil {
		t.Fatalf("activate with the group renamed: %v", err)
	}
	wantRows(t, "groups", rows(t, f.Tenant, groupsQuery), []string{"DOMESTIC Domestic suppliers - - true", "TAX_AUTHORITY Tax authorities - - true"})
}
