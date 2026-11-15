package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPartnerChangeDuplicates (T89a, migration 000330): a change's
// possible duplicates are an array, empty by default; a partner is found
// by partner.normalized_name, which the name's index is built on, white
// space and case aside.
func TestPartnerChangeDuplicates(t *testing.T) {
	need(t)
	const change = `INSERT INTO partner.partner_change (tenant_id, id, number, kind, partner_id, payload, reason, status, requested_by, created_by,
		updated_by, possible_duplicates) VALUES ($1, gen_random_uuid(), $2, 'create', $3, '{}', 'r', 'pending', $1, $1, $1, $4::jsonb)`
	f := newFixture(t)
	for _, c := range []struct {
		name, duplicates, want string
	}{
		{"an array", `[{"partner_id": "x", "number": "BP1", "name": "N", "matched": ["name_and_country"]}]`, ""},
		{"an object", `{}`, "partner_change_possible_duplicates_array"},
		{"null", `null`, "partner_change_possible_duplicates_array"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, change, f.tenant, "PC-"+c.name, f.supplier, c.duplicates) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("a change without them lists none", func(t *testing.T) {
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			var got string
			if err := tx.QueryRow(context.Background(), `INSERT INTO partner.partner_change (tenant_id, id, number, kind, partner_id, payload, reason,
					status, requested_by, created_by, updated_by) VALUES ($1, gen_random_uuid(), 'PC-1', 'update', $2, '{}', 'r', 'pending', $1, $1, $1)
				RETURNING possible_duplicates::text`, f.tenant, f.supplier).Scan(&got); err != nil {
				return err
			}
			if got != "[]" {
				t.Errorf("possible duplicates: got %s, want []", got)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("partner.normalized_name finds a partner, white space and case aside", func(t *testing.T) {
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM partner.business_partner
				WHERE country_code = (SELECT country_code FROM partner.business_partner WHERE id = $1)
					AND partner.normalized_name(name) = partner.normalized_name(' ' || lower((SELECT name FROM partner.business_partner WHERE id = $1)) || E'\t')`,
				f.supplier).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				t.Errorf("partners found: got %d, want 1", n)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
