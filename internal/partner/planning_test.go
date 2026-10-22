package partner_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// planningGroups gives the tenant the planning groups TAX and DOMESTIC,
// and GONE, which the configuration no longer has (cash.Setup keeps them).
func (e env) planningGroups(t *testing.T) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO cash.planning_group (tenant_id, code, name, is_active, created_by, updated_by)
		VALUES ($1, 'TAX', 'Tax authorities', true, $2, $2), ($1, 'DOMESTIC', 'Domestic', true, $2, $2), ($1, 'GONE', 'Gone', false, $2, $2)`,
		e.ten.ID, e.ten.Human.ID)
}

// withGroup is a supplier body whose company data has the planning group.
func (e env) withGroup(name, group string) string {
	return strings.Replace(e.supplier(name, 0), `"payment_term_code": "NT30"}`, `"payment_term_code": "NT30", "planning_group": "`+group+`"}`, 1)
}

// groupChange is a partner change giving the supplier the planning group,
// or none.
func (e env) groupChange(partnerID, group string) string {
	field := ""
	if group != "" {
		field = fmt.Sprintf(`, "planning_group": %q`, group)
	}
	return e.update(partnerID, fmt.Sprintf(`{"supplier_companies": [{"legal_entity_id": %q,
		"reconciliation_account_id": %q, "payment_term_code": "NT30"%s}]}`, e.le, e.recon, field))
}

// TestSupplierPlanningGroup: a supplier's planning group per company (SAP
// LFB1-FDGRV; ADR-0051 §4) is an active planning group of the tenant; a
// change replaces it, leaving it out takes it off, and the supplier port
// reads it.
func TestSupplierPlanningGroup(t *testing.T) {
	e := setup(t)
	e.planningGroups(t)
	p, c := e.create(t, e.withGroup("Muster", "TAX"))
	e.mustApply(t, id(c))
	group := func() any {
		t.Helper()
		got, _ := e.Must(200, "GET", "/v1/business-partners/"+id(p), "", "")
		return got["supplier_companies"].([]any)[0].(map[string]any)["planning_group"]
	}
	if got := group(); got != "TAX" {
		t.Errorf("planning group: got %v, want TAX", got)
	}
	supplierGroup := func() string {
		t.Helper()
		var g string
		if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			s, _, err := partner.Suppliers{}.Supplier(ctx, uuid.MustParse(id(p)), e.le)
			g = s.PlanningGroup
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return g
	}
	if got := supplierGroup(); got != "TAX" {
		t.Errorf("supplier's planning group: got %q, want TAX", got)
	}
	ch, _ := e.Must(201, "POST", "/v1/partner-changes", e.groupChange(id(p), "DOMESTIC"), "")
	e.mustApply(t, id(ch))
	if got := group(); got != "DOMESTIC" {
		t.Errorf("planning group after the change: got %v, want DOMESTIC", got)
	}
	ch, _ = e.Must(201, "POST", "/v1/partner-changes", e.groupChange(id(p), ""), "")
	e.mustApply(t, id(ch))
	if got := group(); got != nil {
		t.Errorf("planning group taken off: got %v, want none", got)
	}
	if got := supplierGroup(); got != "" {
		t.Errorf("supplier's planning group taken off: got %q, want none", got)
	}
	for name, g := range map[string]string{"unknown group": "FOREIGN", "inactive group": "GONE"} {
		t.Run(name, func(t *testing.T) {
			e.in(t).Problem(422, "partner.unknown_planning_group", "POST", "/v1/business-partners", e.withGroup("Other "+name, g), "")
		})
	}
	t.Run("a code of another form", func(t *testing.T) {
		e.in(t).Must(400, "POST", "/v1/business-partners", e.withGroup("Lower", "tax"), "")
	})
}

// TestPlanningGroupCodesSection: a configuration that takes a planning
// group away while a supplier has it, or a pending partner change gives
// it, is refused when it is submitted and when it is activated, once per
// group however many companies use it (ADR-0051 §4).
func TestPlanningGroupCodesSection(t *testing.T) {
	e := setup(t)
	e.planningGroups(t)
	p, c := e.create(t, e.withGroup("Muster", "TAX"))
	e.mustApply(t, id(c))
	doc := func(groups ...string) *config.Config {
		// Company 1000 twice: the group is the tenant's, used in each.
		d := &config.Config{Companies: []config.Company{{Code: "1000"}, {Code: "1000"}, {Code: "9999"}}}
		for _, g := range groups {
			d.Cash.PlanningGroups = append(d.Cash.PlanningGroups, config.PlanningGroup{Code: g})
		}
		return d
	}
	section := partner.PlanningGroupCodes{LegalEntities: tenancy.Directory{}}
	findings := func(proposed *config.Config) string {
		t.Helper()
		var paths []string
		if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			fs, err := section.Check(ctx, &config.Config{}, proposed)
			for _, f := range fs {
				paths = append(paths, f.Path)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(paths)
	}
	if got := findings(doc()); got != "[cash.planning_groups[TAX]]" {
		t.Errorf("findings with TAX on a supplier: got %s, want [cash.planning_groups[TAX]]", got)
	}
	ch, _ := e.Must(201, "POST", "/v1/partner-changes", e.groupChange(id(p), "DOMESTIC"), "")
	if got := findings(doc("TAX")); got != "[cash.planning_groups[DOMESTIC]]" {
		t.Errorf("findings with DOMESTIC in a pending change: got %s, want [cash.planning_groups[DOMESTIC]]", got)
	}
	if got := findings(doc("TAX", "DOMESTIC")); got != "[]" {
		t.Errorf("findings keeping both groups: got %s, want none", got)
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error { return section.Apply(ctx, doc("TAX")) })
	pgtest.WantErr(t, err, "partner.planning_group_in_use")
	e.mustApply(t, id(ch))
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error { return section.Apply(ctx, doc("DOMESTIC")) }); err != nil {
		t.Errorf("apply keeping DOMESTIC: got %v, want nil", err)
	}
	t.Run("nothing to check on the first version", func(t *testing.T) {
		var fs []config.Finding
		if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			var err error
			fs, err = section.Check(ctx, nil, doc())
			return err
		}); err != nil || len(fs) != 0 {
			t.Errorf("Check(nil, ...): got %v, %v, want none", fs, err)
		}
	})
}
