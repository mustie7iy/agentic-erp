package db

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPaymentMethodRunOrderBackfilled: 000247 gives each payment method
// of an existing tenant its place in the order a payment run tries them
// (ADR-0038; SAP specialist 2026-11-09, A4(c)): its position among its
// company's payment_methods in the active configuration, the order a later
// activation writes for the same document, each company on its own. An
// earlier version's order counts nothing, and a method the active
// document no longer has, or of a company it does not have, keeps none.
func TestPaymentMethodRunOrderBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, "000247_payment_method_run_order")
	f := fixtureIDs()
	second := id()
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) error {
		if err := f.seed(tx); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
			VALUES ($1, $2, '2000', 'Acme DE', 'DE', 'EUR', $3, 'active', $4, $4)`, f.tenant, second, f.calendar, f.actor); err != nil {
			return err
		}
		for n, document := range []string{
			`{"companies": [{"code": "1000", "payment_methods": [{"code": "C"}, {"code": "T"}, {"code": "S"}]}]}`,
			`{"companies": [{"code": "1000", "payment_methods": [{"code": "T"}, {"code": "S"}, {"code": "C"}]},
				{"code": "2000", "payment_methods": [{"code": "S"}, {"code": "T"}]}, {"code": "3000"}, {"code": "4000", "payment_methods": [{"code": "T"}]}]}`,
		} {
			if err := exec(tx, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, activated_by)
				VALUES ($1, gen_random_uuid(), $2, $3::jsonb, sha256(($3::jsonb)::text::bytea), 'bootstrap', $4)`, f.tenant, n+1, document, f.actor); err != nil {
				return err
			}
		}
		if err := exec(tx, `INSERT INTO config.active_version (tenant_id, number) VALUES ($1, 2)`, f.tenant); err != nil {
			return err
		}
		for _, m := range []struct {
			entity any
			code   string
		}{{f.entity, "C"}, {f.entity, "S"}, {f.entity, "T"}, {f.entity, "X"}, {second, "S"}, {second, "T"}} {
			if err := exec(tx, `INSERT INTO payments.payment_method (tenant_id, legal_entity_id, code, description, is_active, created_by, updated_by)
				VALUES ($1, $2, $3, 'M', $3 <> 'X', $4, $4)`, f.tenant, m.entity, m.code, f.actor); err != nil {
				return err
			}
		}
		return nil
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	var got []string
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) error {
		rows, _ := tx.Query(context.Background(), `SELECT e.code, m.code, coalesce(m.run_order::text, 'none'), m.version FROM payments.payment_method m
			JOIN tenancy.legal_entity e ON e.tenant_id = m.tenant_id AND e.id = m.legal_entity_id`)
		var company, code, order string
		var version int
		_, err := pgx.ForEachRow(rows, []any{&company, &code, &order, &version}, func() error {
			got = append(got, fmt.Sprintf("%s %s: %s v%d", company, code, order, version))
			return nil
		})
		return err
	})
	slices.Sort(got)
	want := "1000 C: 3 v2; 1000 S: 2 v2; 1000 T: 1 v2; 1000 X: none v1; 2000 S: 1 v2; 2000 T: 2 v2"
	if strings.Join(got, "; ") != want {
		t.Errorf("the methods' run order: got %s, want %s", strings.Join(got, "; "), want)
	}
}
