package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// transportScene is inventoryScene with stock in transit to its location
// and a stock transport order from another plant.
type transportScene struct {
	inventoryScene
	transit, order, from uuid.UUID
}

func newTransportScene() transportScene {
	return transportScene{inventoryScene: inventoryScene{tenant: id(), company: id(), doc: id(), stock: id(), material: id(), plant: id(), location: id()},
		transit: id(), order: id(), from: id()}
}

func (s transportScene) transitRow() stmt {
	return step(`INSERT INTO inventory.transit_stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id)
		VALUES ($1, $2, $3, $4, $5, $6)`, s.tenant, s.transit, s.company, s.material, s.plant, s.location)
}

// transitMovement is a line of the document in transit to the scene's
// location.
func (s transportScene) transitMovement(doc uuid.UUID, line int, qty string) stmt {
	return step(`INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id, storage_location_id,
			quantity, entry_quantity, entry_uom_code, in_transit) VALUES ($1, $2, $3, 1, $4, $5, $6, $7::numeric, 1, 'EA', true)`,
		s.tenant, doc, line, s.material, s.plant, s.location, qty)
}

// issued is a 351 putting 10 EA in transit to the scene's location.
func (s transportScene) issued() []stmt {
	return []stmt{s.valuation(), step(`INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type,
			posting_date, reference, created_by) VALUES ($1, $2, $3, 2026, 'MD1', '351', '2026-11-20', 'TO1', $1)`, s.tenant, s.doc, s.company),
		s.transitRow(), s.transitMovement(s.doc, 1, "10")}
}

func (s transportScene) orderRow(status string, issue, receipt *uuid.UUID) stmt {
	var issueNo, receiptNo, issueDay, receiptDay any
	if issue != nil {
		issueNo, issueDay = "MD1", "2026-11-20"
	}
	if receipt != nil {
		receiptNo, receiptDay = "MD2", "2026-11-21"
	}
	return step(`INSERT INTO procurement.stock_transport_order (tenant_id, id, legal_entity_id, fiscal_year, number, supplying_plant_id, receiving_plant_id,
			order_date, status, issue_document_id, issue_document_number, issue_date, receipt_document_id, receipt_document_number, receipt_date,
			created_by, updated_by)
		VALUES ($1, $2, $3, 2026, 'TO1', $4, $5, '2026-11-20', $6, $7, $8, $9::date, $10, $11, $12::date, $1, $1)`,
		s.tenant, s.order, s.company, s.from, s.plant, status, issue, issueNo, issueDay, receipt, receiptNo, receiptDay)
}

// orderNumbered is an open order of the number.
func (s transportScene) orderNumbered(number string) stmt {
	return step(`INSERT INTO procurement.stock_transport_order (tenant_id, id, legal_entity_id, fiscal_year, number, supplying_plant_id, receiving_plant_id,
			order_date, created_by, updated_by) VALUES ($1, $2, $3, 2026, $4, $5, $6, '2026-11-20', $1, $1)`, s.tenant, s.order, s.company, number, s.from, s.plant)
}

// item is item 1 of the order, of the quantities in the unit.
func (s transportScene) item(qty, base, unit string, order uuid.UUID) stmt {
	return step(`INSERT INTO procurement.stock_transport_order_item (tenant_id, order_id, item_no, material_id, quantity, uom_code, base_quantity,
			supplying_storage_location_id, receiving_storage_location_id) VALUES ($1, $2, 1, $3, $4::numeric, $5, $6::numeric, $7, $8)`,
		s.tenant, order, s.material, qty, unit, base, id(), s.location)
}

func (s transportScene) orderItem(no int, material uuid.UUID) stmt {
	return step(`INSERT INTO procurement.stock_transport_order_item (tenant_id, order_id, item_no, material_id, quantity, uom_code, base_quantity,
			supplying_storage_location_id, receiving_storage_location_id) VALUES ($1, $2, $3, $4, 1, 'EA', 1, $5, $6)`,
		s.tenant, s.order, no, material, id(), s.location)
}

// TestStockTransportRules (S2h, migrations 000418 and 000419; ADR-0082
// As built S2h): a movement in transit adds to its transit stock row,
// which starts empty, must exist and never falls below zero, and to the
// valuation, not to the location's stock, and no count blocks it; a 351
// names its reference; a stock transport order is numbered once per
// company and year, between two plants, issued once in transit or
// received and received once received, moving forward only (or from open
// to cancelled, migration 000446, naming no goods issue) and keeping what
// it is; its items, a material once each, are insert-only.
func TestStockTransportRules(t *testing.T) {
	need(t)
	doc := id()
	cases := []struct {
		name string
		run  func(s transportScene) []stmt
		want string // empty: allowed
	}{
		{"an issue into transit", func(s transportScene) []stmt { return s.issued() }, ""},
		{"a transit row with stock", func(s transportScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.transit_stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id, quantity)
				VALUES ($1, $2, $3, $4, $5, $6, 1)`, s.tenant, s.transit, s.company, s.material, s.plant, s.location)}
		}, "stock_from_movements"},
		{"a transit row twice", func(s transportScene) []stmt {
			return []stmt{s.transitRow(), step(`INSERT INTO inventory.transit_stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id)
				VALUES ($1, $2, $3, $4, $5, $6)`, s.tenant, id(), s.company, s.material, s.plant, s.location)}
		}, "transit_stock_once"},
		{"a movement in transit without its row", func(s transportScene) []stmt {
			return append(s.issued()[:2], s.transitMovement(s.doc, 1, "10"))
		}, "movement_stock"},
		{"more out of transit than is in it", func(s transportScene) []stmt { return append(s.issued(), s.transitMovement(s.doc, 2, "-11")) },
			"transit_stock_not_negative"},
		{"a 351 without its reference", func(s transportScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
				created_by) VALUES ($1, $2, $3, 2026, 'MD1', '351', '2026-11-20', $1)`, s.tenant, s.doc, s.company)}
		}, "material_document_reference"},
		{"an issue into transit while the location is counted", func(s transportScene) []stmt {
			cs := countScene{inventoryScene: s.inventoryScene, count: id()}
			return append([]stmt{cs.document(cs.count, "PI1", "counting"), cs.item(cs.count, 1, s.material, "0")}, s.issued()...)
		}, ""},
		{"an open order", func(s transportScene) []stmt { return []stmt{s.orderRow("open", nil, nil), s.orderItem(1, s.material)} }, ""},
		{"a number twice", func(s transportScene) []stmt {
			o := s
			o.order = id()
			return []stmt{s.orderRow("open", nil, nil), o.orderRow("open", nil, nil)}
		}, "stock_transport_order_number_once"},
		{"one plant", func(s transportScene) []stmt {
			s.from = s.plant
			return []stmt{s.orderRow("open", nil, nil)}
		}, "stock_transport_order_plants"},
		{"an unknown status", func(s transportScene) []stmt { return []stmt{s.orderRow("shipped", nil, nil)} }, "stock_transport_order_status_check"},
		{"cancelled", func(s transportScene) []stmt { return []stmt{s.orderRow("cancelled", nil, nil)} }, ""},
		{"cancelled with an issue", func(s transportScene) []stmt { return []stmt{s.orderRow("cancelled", &doc, nil)} }, "stock_transport_order_issued"},
		{"cancelled with a receipt", func(s transportScene) []stmt { return []stmt{s.orderRow("cancelled", nil, &doc)} }, "stock_transport_order_received"},
		{"open to cancelled", func(s transportScene) []stmt {
			return []stmt{s.orderRow("open", nil, nil), step(`UPDATE procurement.stock_transport_order SET status = 'cancelled', version = 2 WHERE id = $1`, s.order)}
		}, ""},
		{"in transit to cancelled", func(s transportScene) []stmt {
			return []stmt{s.orderRow("in_transit", &doc, nil), step(`UPDATE procurement.stock_transport_order SET status = 'cancelled', issue_document_id = NULL,
				issue_document_number = NULL, issue_date = NULL WHERE id = $1`, s.order)}
		}, "stock_transport_order_fixed"},
		{"cancelled to open", func(s transportScene) []stmt {
			return []stmt{s.orderRow("cancelled", nil, nil), step(`UPDATE procurement.stock_transport_order SET status = 'open' WHERE id = $1`, s.order)}
		}, "stock_transport_order_fixed"},
		{"cancelled to in transit", func(s transportScene) []stmt {
			return []stmt{s.orderRow("cancelled", nil, nil), step(`UPDATE procurement.stock_transport_order SET status = 'in_transit', issue_document_id = $2,
				issue_document_number = 'MD1', issue_date = '2026-11-20' WHERE id = $1`, s.order, doc)}
		}, "stock_transport_order_fixed"},
		{"in transit without its issue", func(s transportScene) []stmt { return []stmt{s.orderRow("in_transit", nil, nil)} }, "stock_transport_order_issued"},
		{"open with an issue", func(s transportScene) []stmt { return []stmt{s.orderRow("open", &doc, nil)} }, "stock_transport_order_issued"},
		{"received without its receipt", func(s transportScene) []stmt { return []stmt{s.orderRow("received", &doc, nil)} }, "stock_transport_order_received"},
		{"in transit with a receipt", func(s transportScene) []stmt { return []stmt{s.orderRow("in_transit", &doc, &doc)} }, "stock_transport_order_received"},
		{"received", func(s transportScene) []stmt { return []stmt{s.orderRow("received", &doc, &doc)} }, ""},
		{"issued", func(s transportScene) []stmt {
			return []stmt{s.orderRow("open", nil, nil), step(`UPDATE procurement.stock_transport_order SET status = 'in_transit', issue_document_id = $2,
				issue_document_number = 'MD1', issue_date = '2026-11-20', version = 2 WHERE id = $1`, s.order, doc)}
		}, ""},
		{"open to received", func(s transportScene) []stmt {
			return []stmt{s.orderRow("open", nil, nil), step(`UPDATE procurement.stock_transport_order SET status = 'received', issue_document_id = $2,
				issue_document_number = 'MD1', issue_date = '2026-11-20', receipt_document_id = $2, receipt_document_number = 'MD2',
				receipt_date = '2026-11-20' WHERE id = $1`, s.order, doc)}
		}, "stock_transport_order_fixed"},
		{"an issue moved", func(s transportScene) []stmt {
			return []stmt{s.orderRow("in_transit", &doc, nil), step(`UPDATE procurement.stock_transport_order SET status = 'received', issue_document_number = 'MD9',
				receipt_document_id = $2, receipt_document_number = 'MD2', receipt_date = '2026-11-20' WHERE id = $1`, s.order, doc)}
		}, "stock_transport_order_fixed"},
		{"a number changed", func(s transportScene) []stmt {
			return []stmt{s.orderRow("open", nil, nil), step(`UPDATE procurement.stock_transport_order SET number = 'TO9' WHERE id = $1`, s.order)}
		}, "permission denied"},
		{"an order deleted", func(s transportScene) []stmt {
			return []stmt{s.orderRow("open", nil, nil), step(`DELETE FROM procurement.stock_transport_order WHERE id = $1`, s.order)}
		}, "permission denied"},
		{"a material twice", func(s transportScene) []stmt {
			return []stmt{s.orderRow("open", nil, nil), s.orderItem(1, s.material), s.orderItem(2, s.material)}
		}, "stock_transport_order_item_material_once"},
		{"item 101", func(s transportScene) []stmt {
			return []stmt{s.orderRow("open", nil, nil), s.orderItem(101, s.material)}
		},
			"stock_transport_order_item_no_check"},
		{"a number of 41 characters", func(s transportScene) []stmt { return []stmt{s.orderNumbered(strings.Repeat("T", 41))} },
			"stock_transport_order_number_check"},
		{"no number", func(s transportScene) []stmt { return []stmt{s.orderNumbered("")} }, "stock_transport_order_number_check"},
		{"version 0", func(s transportScene) []stmt {
			return []stmt{step(`INSERT INTO procurement.stock_transport_order (tenant_id, id, legal_entity_id, fiscal_year, number, supplying_plant_id,
				receiving_plant_id, order_date, version, created_by, updated_by) VALUES ($1, $2, $3, 2026, 'TO1', $4, $5, '2026-11-20', 0, $1, $1)`,
				s.tenant, s.order, s.company, s.from, s.plant)}
		}, "stock_transport_order_version_check"},
		{"an issue document number of 41 characters", func(s transportScene) []stmt {
			return []stmt{s.orderRow("open", nil, nil), step(`UPDATE procurement.stock_transport_order SET status = 'in_transit', issue_document_id = $2,
				issue_document_number = $3, issue_date = '2026-11-20', version = 2 WHERE id = $1`, s.order, doc, strings.Repeat("M", 41))}
		}, "stock_transport_order_issue_document_number_check"},
		{"a receipt document number of 41 characters", func(s transportScene) []stmt {
			return []stmt{s.orderRow("in_transit", &doc, nil), step(`UPDATE procurement.stock_transport_order SET status = 'received', receipt_document_id = $2,
				receipt_document_number = $3, receipt_date = '2026-11-20', version = 2 WHERE id = $1`, s.order, doc, strings.Repeat("M", 41))}
		}, "stock_transport_order_receipt_document_number_check"},
		{"an item of no quantity", func(s transportScene) []stmt {
			return []stmt{s.orderRow("open", nil, nil), s.item("0", "1", "EA", s.order)}
		},
			"stock_transport_order_item_quantity_check"},
		{"an item of no base quantity", func(s transportScene) []stmt {
			return []stmt{s.orderRow("open", nil, nil), s.item("1", "0", "EA", s.order)}
		},
			"stock_transport_order_item_base_quantity_check"},
		{"an item of an unknown unit", func(s transportScene) []stmt {
			return []stmt{s.orderRow("open", nil, nil), s.item("1", "1", "XX", s.order)}
		},
			"stock_transport_order_item_uom_code_fkey"},
		{"an item of no order", func(s transportScene) []stmt { return []stmt{s.item("1", "1", "EA", id())} }, "stock_transport_order_item_order_fkey"},
		{"an item changed", func(s transportScene) []stmt {
			return []stmt{s.orderRow("open", nil, nil), s.orderItem(1, s.material),
				step(`UPDATE procurement.stock_transport_order_item SET quantity = 2 WHERE order_id = $1`, s.order)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTransportScene()
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want == "" {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			if strings.Contains(c.want, " ") {
				failsWith(t, err, c.want)
				return
			}
			failsOn(t, err, c.want)
		})
	}
	t.Run("transit stock and valuation", func(t *testing.T) {
		s := newTransportScene()
		var transit, stock, valued string
		err := asApp(t, s.tenant, func(tx pgx.Tx) error {
			if err := all(tx, s.issued()...); err != nil {
				return err
			}
			return tx.QueryRow(context.Background(), `SELECT (SELECT trim_scale(quantity)::text FROM inventory.transit_stock WHERE id = $1),
				(SELECT count(*)::text FROM inventory.stock WHERE material_id = $2), (SELECT trim_scale(quantity)::text FROM inventory.valuation WHERE material_id = $2)`,
				s.transit, s.material).Scan(&transit, &stock, &valued)
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := transit + " " + stock + " " + valued; got != "10 0 10" {
			t.Errorf("in transit, stock rows and valuation: got %s, want 10 0 10", got)
		}
	})
}

// TestStockTransportTriggersAsOwner (S2h, migrations 000418 and 000446):
// past the grants, as the owner, an order moves forward only and keeps
// what it is, a received one never cancelled, and its items are neither
// changed nor deleted.
func TestStockTransportTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := newTransportScene()
	for name, c := range map[string]struct {
		st   stmt
		want string
	}{
		"received to open": {step(`UPDATE procurement.stock_transport_order SET status = 'open' WHERE id = $1`, s.order), "stock_transport_order_fixed"},
		"received to cancelled": {step(`UPDATE procurement.stock_transport_order SET status = 'cancelled', issue_document_id = NULL, issue_document_number = NULL,
			issue_date = NULL, receipt_document_id = NULL, receipt_document_number = NULL, receipt_date = NULL WHERE id = $1`, s.order),
			"stock_transport_order_fixed"},
		"its plant changed": {step(`UPDATE procurement.stock_transport_order SET receiving_plant_id = $2 WHERE id = $1`, s.order, id()), "stock_transport_order_fixed"},
		"an item changed": {step(`UPDATE procurement.stock_transport_order_item SET quantity = 2 WHERE order_id = $1`, s.order),
			"is insert-only"},
		"an item deleted": {step(`DELETE FROM procurement.stock_transport_order_item WHERE order_id = $1`, s.order), "is insert-only"},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", s.tenant.String()); err != nil {
				t.Fatal(err)
			}
			doc := id()
			if err := all(tx, s.orderRow("received", &doc, &doc), s.orderItem(1, s.material)); err != nil {
				t.Fatal(err)
			}
			if err := exec(tx, c.st.sql, c.st.args...); strings.Contains(c.want, " ") {
				failsWith(t, err, c.want)
			} else {
				failsOn(t, err, c.want)
			}
		})
	}
}

// TestStockTransportDownRefused: 000418's down migration refuses to drop
// stock transport orders and stock in transit (06 §6.4: a down migration
// loses no data).
func TestStockTransportDownRefused(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s := newTransportScene()
	if err := all(tx, append(append([]stmt{step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String())}, s.issued()...),
		step(`SELECT set_config('app.tenant_id', '', true)`))...); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, migrationNamed(t, "000418_stock_transport").Down), "stock transport orders exist")
}

// TestStockTransportCancelDownRefused: 000446's down migration refuses,
// rather than changes, a cancelled stock transport order, which the
// previous version cannot hold (06 §6.4: a down migration loses no data).
func TestStockTransportCancelDownRefused(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s := newTransportScene()
	if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String()), s.orderRow("cancelled", nil, nil),
		step(`SELECT set_config('app.tenant_id', '', true)`)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, migrationNamed(t, "000446_stock_transport_cancel").Down), "cancelled stock transport orders exist")
}
