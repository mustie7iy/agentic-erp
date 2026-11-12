package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestSalesPermissions (T78a, migration 000276; ADR-0067 §7): the sales
// permissions are seeded and are no approval permissions.
func TestSalesPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'sales.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "sales.quotation.read sales.quotation.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestQuotationRules (T78a, migration 000276; ADR-0067 §2, §3): a
// quotation is created open, numbered once per company, valid until a day
// not before its date, with a net value above zero; the customer's answer
// is its one change, once; an item is a positive quantity in a known unit
// at a positive price per a positive price unit with a positive net
// value; what was offered is fixed and never deleted.
func TestQuotationRules(t *testing.T) {
	need(t)
	const header = `INSERT INTO sales.quotation (tenant_id, id, document_number, legal_entity_id, customer_id, quotation_date, valid_until,
		currency, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), '2026-03-10', $5, 'EUR', $6, $7, $8, $8)`
	const item = `INSERT INTO sales.quotation_item (tenant_id, quotation_id, item_no, description, quantity, uom_code, unit_price, price_unit,
		net_amount, account_id) VALUES ($1, $2, $3, 'Bracket', $4, $5, $6, $7, $8, gen_random_uuid())`
	const answer = `UPDATE sales.quotation SET status = $1, version = version + 1, updated_by = tenant_id`
	type scene struct{ tenant, actor, company, doc uuid.UUID }
	quotation := func(s scene, doc uuid.UUID, number, validUntil string, net int64, status string) stmt {
		return step(header, s.tenant, doc, number, s.company, validUntil, net, status, s.actor)
	}
	made := func(s scene) stmt { return quotation(s, s.doc, "QT1", "2026-04-10", 10000, "open") }
	line := func(s scene, no int, quantity, unit string, price int64, priceUnit int, net int64) stmt {
		return step(item, s.tenant, s.doc, no, quantity, unit, price, priceUnit, net)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"an open quotation with its items", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "2.5", "KG", 1001, 1, 2503), line(s, 2, "3", "EA", 9999, 100, 300))
		}, ""},
		{"valid on its own day alone", func(tx pgx.Tx, s scene) error { return all(tx, quotation(s, s.doc, "QT1", "2026-03-10", 1, "open")) }, ""},
		{"valid until the day before it is made", func(tx pgx.Tx, s scene) error {
			return all(tx, quotation(s, s.doc, "QT1", "2026-03-09", 1, "open"))
		}, "quotation_validity"},
		{"created in an unknown status", func(tx pgx.Tx, s scene) error {
			return all(tx, quotation(s, s.doc, "QT1", "2026-04-10", 1, "expired"))
		}, "quotation_status_check"},
		{"a net value of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, quotation(s, s.doc, "QT1", "2026-04-10", 0, "open"))
		}, "quotation_net_positive"},
		{"a number issued twice in the company", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), quotation(s, id(), "QT1", "2026-04-10", 1, "open"))
		}, "quotation_number_once"},
		{"the same number in another company", func(tx pgx.Tx, s scene) error {
			other := s
			other.company = id()
			return all(tx, made(s), quotation(other, id(), "QT1", "2026-04-10", 1, "open"))
		}, ""},
		{"an unknown currency", func(tx pgx.Tx, s scene) error {
			return all(tx, step(strings.Replace(header, "'EUR'", "'XXA'", 1), s.tenant, s.doc, "QT1", s.company, "2026-04-10", 1, "open", s.actor))
		}, "quotation_currency_fkey"},
		{"accepted", func(tx pgx.Tx, s scene) error { return all(tx, made(s), step(answer, "accepted")) }, ""},
		{"rejected", func(tx pgx.Tx, s scene) error { return all(tx, made(s), step(answer, "rejected")) }, ""},
		{"accepted, then rejected", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(answer, "accepted"), step(answer, "rejected"))
		}, "only an open quotation changes"},
		{"rejected, then accepted", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(answer, "rejected"), step(answer, "accepted"))
		}, "only an open quotation changes"},
		{"an open quotation changed without an answer", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(answer, "open"))
		}, "only an open quotation changes"},
		{"what was offered changed with the answer", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(`UPDATE sales.quotation SET status = 'accepted', net_amount = 1, valid_until = '2027-01-01'`))
		}, "permission denied"},
		{"a quotation deleted", func(tx pgx.Tx, s scene) error { return all(tx, made(s), step(`DELETE FROM sales.quotation`)) }, "permission denied"},
		{"a quantity of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "0", "EA", 100, 1, 100))
		}, "quotation_item_quantity_positive"},
		{"an unknown unit", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "XX", 100, 1, 100))
		}, "quotation_item_uom_code_fkey"},
		{"a price of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 0, 1, 100))
		}, "quotation_item_price_positive"},
		{"a price unit of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 0, 100))
		}, "quotation_item_price_unit_positive"},
		{"an item's value of zero", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 0))
		}, "quotation_item_net_positive"},
		{"a description of white space", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), step(strings.Replace(item, "'Bracket'", "'  '", 1), s.tenant, s.doc, 1, "1", "EA", 100, 1, 100))
		}, "quotation_item_description_check"},
		{"an item key of white space", func(tx pgx.Tx, s scene) error {
			keyed := strings.Replace(strings.Replace(item, "description,", "description, item_key,", 1), "'Bracket',", "'Bracket', ' ',", 1)
			return all(tx, made(s), step(keyed, s.tenant, s.doc, 1, "1", "EA", 100, 1, 100))
		}, "quotation_item_item_key_check"},
		{"an item numbered twice", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 100), line(s, 1, "1", "EA", 100, 1, 100))
		}, "quotation_item_pkey"},
		{"an item numbered 0", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 0, "1", "EA", 100, 1, 100))
		}, "quotation_item_item_no_check"},
		{"an item of no quotation", func(tx pgx.Tx, s scene) error {
			return all(tx, line(s, 1, "1", "EA", 100, 1, 100))
		}, "quotation_item_tenant_id_quotation_id_fkey"},
		{"an item changed", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 100), step(`UPDATE sales.quotation_item SET net_amount = 200`))
		}, "permission denied"},
		{"an item deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, made(s), line(s, 1, "1", "EA", 100, 1, 100), step(`DELETE FROM sales.quotation_item`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), company: id(), doc: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return c.run(tx, s) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}
