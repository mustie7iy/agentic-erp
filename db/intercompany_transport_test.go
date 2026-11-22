package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// icOrderSQL is a draft purchase order of the plant (nil: none supplies
// it) with the goods issue given (all nil: none).
const icOrderSQL = `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, supplying_plant_id, issue_document_id, issue_document_number, issue_date, created_by, updated_by)
	VALUES ($1, $2, 'PO1', $3, $4, 'EUR', '2026-11-21', 'NT30', 'draft', $5, $6, $7, $8::date, $1, $1)`

// icScene is an intercompany order's tenant, order, company and
// supplying plant.
type icScene struct{ tenant, order, company, plant uuid.UUID }

func newICScene() icScene { return icScene{tenant: id(), order: id(), company: id(), plant: id()} }

func (s icScene) row(plant *uuid.UUID, doc *uuid.UUID, number, day any) stmt {
	return step(icOrderSQL, s.tenant, s.order, s.company, id(), plant, doc, number, day)
}

// TestIntercompanyTransportRules (S2i1, migrations 000437 and 000438;
// ADR-0082 As built S2i): an order's goods issue is all of its document,
// number and day or none, only an order with a supplying plant has one,
// its number is 1 to 40 characters, the supplying plant never changes and
// a goods issue once set stays; a 643 names its reference.
func TestIntercompanyTransportRules(t *testing.T) {
	need(t)
	doc := id()
	cases := []struct {
		name string
		run  func(s icScene) []stmt
		want string // empty: allowed
	}{
		{"an intercompany order", func(s icScene) []stmt { return []stmt{s.row(&s.plant, nil, nil, nil)} }, ""},
		{"an issued intercompany order", func(s icScene) []stmt { return []stmt{s.row(&s.plant, &doc, "MD1", "2026-11-21")} }, ""},
		{"an issue of no supplying plant", func(s icScene) []stmt { return []stmt{s.row(nil, &doc, "MD1", "2026-11-21")} }, "purchase_order_issue"},
		{"an issue without its number", func(s icScene) []stmt { return []stmt{s.row(&s.plant, &doc, nil, "2026-11-21")} }, "purchase_order_issue"},
		{"an issue without its day", func(s icScene) []stmt { return []stmt{s.row(&s.plant, &doc, "MD1", nil)} }, "purchase_order_issue"},
		{"an issue number of 41 characters", func(s icScene) []stmt {
			return []stmt{s.row(&s.plant, &doc, strings.Repeat("M", 41), "2026-11-21")}
		}, "purchase_order_issue_document_number_check"},
		{"an issue posted", func(s icScene) []stmt {
			return []stmt{s.row(&s.plant, nil, nil, nil), step(`UPDATE procurement.purchase_order SET issue_document_id = $2, issue_document_number = 'MD1',
				issue_date = '2026-11-21', version = 2 WHERE id = $1`, s.order, doc)}
		}, ""},
		{"the supplying plant changed", func(s icScene) []stmt {
			return []stmt{s.row(&s.plant, nil, nil, nil), step(`UPDATE procurement.purchase_order SET supplying_plant_id = $2 WHERE id = $1`, s.order, id())}
		}, "purchase_order_supply_fixed"},
		{"a supplying plant added", func(s icScene) []stmt {
			return []stmt{s.row(nil, nil, nil, nil), step(`UPDATE procurement.purchase_order SET supplying_plant_id = $2 WHERE id = $1`, s.order, s.plant)}
		}, "purchase_order_supply_fixed"},
		{"an issue moved", func(s icScene) []stmt {
			return []stmt{s.row(&s.plant, &doc, "MD1", "2026-11-21"), step(`UPDATE procurement.purchase_order SET issue_document_number = 'MD9' WHERE id = $1`,
				s.order)}
		}, "purchase_order_supply_fixed"},
		{"an issue removed", func(s icScene) []stmt {
			return []stmt{s.row(&s.plant, &doc, "MD1", "2026-11-21"), step(`UPDATE procurement.purchase_order SET issue_document_id = NULL,
				issue_document_number = NULL, issue_date = NULL WHERE id = $1`, s.order)}
		}, "purchase_order_supply_fixed"},
		{"a 643", func(s icScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
				reference, created_by) VALUES ($1, $2, $3, 2026, 'MD1', '643', '2026-11-21', 'PO1', $1)`, s.tenant, id(), s.company)}
		}, ""},
		{"a 643 without its reference", func(s icScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
				created_by) VALUES ($1, $2, $3, 2026, 'MD1', '643', '2026-11-21', $1)`, s.tenant, id(), s.company)}
		}, "material_document_reference"},
		{"a 644", func(s icScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
				reference, created_by) VALUES ($1, $2, $3, 2026, 'MD1', '644', '2026-11-21', 'PO1', $1)`, s.tenant, id(), s.company)}
		}, "material_document_movement_type_check"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newICScene()
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want == "" {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			failsOn(t, err, c.want)
		})
	}
}

// TestIntercompanyTransportTriggerAsOwner (S2i1, migration 000437): past
// the grants, as the owner, the supplying plant and a goods issue stay as
// set.
func TestIntercompanyTransportTriggerAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	doc := id()
	for name, sql := range map[string]string{
		"the supplying plant changed": `UPDATE procurement.purchase_order SET supplying_plant_id = NULL WHERE id = $1`,
		"an issue moved":              `UPDATE procurement.purchase_order SET issue_date = '2026-11-22' WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newICScene()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String()), s.row(&s.plant, &doc, "MD1", "2026-11-21")); err != nil {
				t.Fatal(err)
			}
			failsOn(t, exec(tx, sql, s.order), "purchase_order_supply_fixed")
		})
	}
}

// TestIntercompanyTransportDownRefused: 000437's down migration refuses
// to drop intercompany orders and their goods issues (06 §6.4: a down
// migration loses no data).
func TestIntercompanyTransportDownRefused(t *testing.T) {
	need(t)
	ctx := context.Background()
	for name, st := range map[string]func(s icScene) stmt{
		"an intercompany order": func(s icScene) stmt { return s.row(&s.plant, nil, nil, nil) },
		"a 643": func(s icScene) stmt {
			return step(`INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
				reference, created_by) VALUES ($1, $2, $3, 2026, 'MD1', '643', '2026-11-21', 'PO1', $1)`, s.tenant, id(), s.company)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newICScene()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String()), st(s),
				step(`SELECT set_config('app.tenant_id', '', true)`)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, migrationNamed(t, "000437_intercompany_transport").Down), "intercompany stock transport orders exist")
		})
	}
}
