package payments_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// bankDetailFields are a payment's fields that describe its supplier's
// bank account; bank_account_id, the payment's own reference, is not one.
var bankDetailFields = []string{"bank_account_last4", "bank_account_scheme", "bank_account_holder_name", "bank_account_bic",
	"bank_account_institution_number", "bank_account_transit_number"}

// TestBankDetailsNeedPartnerRead: a run's payments give their supplier's
// bank account details only to a caller who may use partner.supplier.read,
// person or agent; a caller who does not hold it, or whom a
// segregation-of-duties rule bars from it, gets the payments with
// bank_account_id alone, never a refusal (product owner 2026-11-02, T45e).
// The statement's payment search, a person's, answers the same.
func TestBankDetailsNeedPartnerRead(t *testing.T) {
	sc := matchStatementSetup(t)
	e := sc.e
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	var run string
	if err := pgtest.Admin(t).QueryRow(t.Context(), `SELECT payment_run_id::text FROM payments.payment WHERE id = $1`, uuidOf(sc.pays[0].payment)).Scan(&run); err != nil {
		t.Fatal(err)
	}
	// The supplier's account becomes a Canadian one, so its bank and branch
	// numbers are there to be shown or left out; bank accounts are
	// insert-only but for their end, so the trigger is off for this row.
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE partner.bank_account SET account_scheme = 'ca', institution_number = '004', transit_number = '12345', bic = NULL,
		bank_country_code = 'CA' WHERE partner_id = '%s'; END $$`, e.Supplier))
	paths := map[string]string{"run's payments": "/v1/payment-runs/" + run + "/payments", "statement's search": "/v1/bank-statements/" + sc.statement + "/payments"}
	check := func(t *testing.T, as pgtest.Actor, path string, shown bool) {
		t.Helper()
		page, _ := e.As(as).In(t).Must(200, "GET", path, "", "")
		items := page["items"].([]any)
		if len(items) == 0 {
			t.Fatal("payments: got none, want some")
		}
		for _, it := range items {
			p := it.(map[string]any)
			if p["bank_account_id"] == nil {
				t.Errorf("payment %v: got no bank_account_id, want it", p["id"])
			}
			if shown {
				wantBankAccount(t, p)
				continue
			}
			for _, f := range bankDetailFields {
				if v, ok := p[f]; ok {
					t.Errorf("payment %v: got %s %v, want it absent", p["id"], f, v)
				}
			}
		}
	}
	cases := []struct {
		name  string
		shown bool
		setup func()
	}{
		{"held", true, func() {}},
		{"barred by a segregation-of-duties rule", false, func() {
			pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
				VALUES ($1, gen_random_uuid(), $2, 'payments.run.propose', 'd', current_date, $3)`, e.Tenant.ID, partner.ReadPermission, e.Tenant.Human.ID)
		}},
		{"not held", false, func() {
			// The rule is gone, so only the permission's absence hides the
			// details; rules are never deleted but here, with the trigger off.
			pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
				DELETE FROM identity.sod_rule WHERE tenant_id = '%s'; END $$`, e.Tenant.ID))
			e.Tenant.Revoke(t, partner.ReadPermission, e.Tenant.Human, e.Tenant.Agent)
		}},
	}
	for _, c := range cases {
		c.setup()
		for what, path := range paths {
			t.Run(fmt.Sprintf("%s, %s, person", c.name, what), func(t *testing.T) { check(t, e.Tenant.Human, path, c.shown) })
		}
		t.Run(c.name+", run's payments, agent", func(t *testing.T) { check(t, e.Tenant.Agent, paths["run's payments"], c.shown) })
	}
}
