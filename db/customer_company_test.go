package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// customerCompany inserts the partner's customer data in the fixture's
// legal entity with the columns of set changed.
func customerCompany(f fixture, partner uuid.UUID, set map[string]any) stmt {
	row := map[string]any{"tenant_id": f.tenant, "legal_entity_id": f.entity, "payment_term_code": "NT30", "role_customer": "customer", "version": 1}
	for k, v := range set {
		row[k] = v
	}
	return step(`INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code,
			role_customer, version, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
		row["tenant_id"], partner, row["legal_entity_id"], f.bank, row["payment_term_code"], row["role_customer"], row["version"], f.actor)
}

// TestCustomerCompanyRules: every rule the database enforces on a
// customer's company data (T74a, 000266): a partner with the customer
// role, a payment term of the tenant, one row per customer and legal
// entity, a version from 1, no posting block until one is set; a row
// changed but never deleted; and a tenant's rows are its own.
func TestCustomerCompanyRules(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name string
		run  func(f fixture, customer uuid.UUID) []stmt
		want string // empty: allowed
	}{
		{"a customer's data", func(f fixture, customer uuid.UUID) []stmt { return []stmt{customerCompany(f, customer, nil)} }, ""},
		{"in a second legal entity", func(f fixture, customer uuid.UUID) []stmt {
			return []stmt{customerCompany(f, customer, nil), customerCompany(f, customer, map[string]any{"legal_entity_id": id()})}
		}, ""},
		{"blocked for posting, the version raised", func(f fixture, customer uuid.UUID) []stmt {
			return []stmt{customerCompany(f, customer, nil),
				step(`UPDATE partner.customer_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, customer)}
		}, ""},
		{"a partner without the customer role", func(f fixture, _ uuid.UUID) []stmt { return []stmt{customerCompany(f, f.supplier, nil)} },
			"customer_company_tenant_id_partner_id_role_customer_fkey"},
		{"another role in the role column", func(f fixture, _ uuid.UUID) []stmt {
			return []stmt{customerCompany(f, f.supplier, map[string]any{"role_customer": "supplier"})}
		}, "customer_company_role_customer_check"},
		{"an unknown payment term", func(f fixture, customer uuid.UUID) []stmt {
			return []stmt{customerCompany(f, customer, map[string]any{"payment_term_code": "NT99"})}
		}, "customer_company_tenant_id_payment_term_code_fkey"},
		{"a legal entity twice", func(f fixture, customer uuid.UUID) []stmt {
			return []stmt{customerCompany(f, customer, nil), customerCompany(f, customer, nil)}
		}, "customer_company_pkey"},
		{"a version of 0", func(f fixture, customer uuid.UUID) []stmt {
			return []stmt{customerCompany(f, customer, map[string]any{"version": 0})}
		}, "customer_company_version_check"},
		{"another tenant's row", func(f fixture, customer uuid.UUID) []stmt {
			return []stmt{customerCompany(f, customer, map[string]any{"tenant_id": id()})}
		}, "row-level security"},
		{"a row deleted", func(f fixture, customer uuid.UUID) []stmt {
			return []stmt{customerCompany(f, customer, nil), step(`DELETE FROM partner.customer_company WHERE partner_id = $1`, customer)}
		}, "permission denied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			customer := id()
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return all(tx, append([]stmt{
					step(`INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
						VALUES ($1, $2, 'C0001', 'organization', 'Customer', 'US', 'active', $3, $3)`, f.tenant, customer, f.actor),
					step(`INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'customer', $3)`, f.tenant, customer, f.actor),
				}, c.run(f, customer)...)...)
			})
			if c.want == "" {
				if err != nil {
					t.Fatalf("got %v, want it allowed", err)
				}
				return
			}
			failsWith(t, err, c.want)
		})
	}
}

// TestCustomerCompanyIsolation: a tenant reads no other tenant's customer
// data, and none without a tenant; a new row is not blocked for posting.
func TestCustomerCompanyIsolation(t *testing.T) {
	need(t)
	a, b := newFixture(t), newFixture(t)
	customer := id()
	mustApp(t, a.tenant, func(tx pgx.Tx) error {
		return all(tx,
			step(`INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
				VALUES ($1, $2, 'C0001', 'organization', 'Customer', 'US', 'active', $3, $3)`, a.tenant, customer, a.actor),
			step(`INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'customer', $3)`, a.tenant, customer, a.actor),
			customerCompany(a, customer, nil))
	})
	count := func(tenant uuid.UUID) (n int) {
		mustApp(t, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT count(*) FROM partner.customer_company WHERE partner_id = $1 AND NOT is_posting_blocked`, customer).Scan(&n)
		})
		return n
	}
	for name, tc := range map[string]struct {
		tenant uuid.UUID
		want   int
	}{"its own tenant": {a.tenant, 1}, "another tenant": {b.tenant, 0}, "no tenant": {uuid.Nil, 0}} {
		t.Run(name, func(t *testing.T) {
			if got := count(tc.tenant); got != tc.want {
				t.Errorf("unblocked customer company rows visible: got %d, want %d", got, tc.want)
			}
		})
	}
}
