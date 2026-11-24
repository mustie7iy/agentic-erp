package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// shipmentSet records a shipment on the scene's delivery, as shipping's
// RecordShipment does (S2r, 000462).
func (s shippingScene) shipmentSet(number, carrier string, amount any, currency string) stmt {
	return step(`UPDATE shipping.outbound_delivery SET shipment_number = $2, carrier_code = $3, freight_amount = $4, freight_currency = $5,
		shipment_recorded_at = now(), version = version + 1 WHERE id = $1`, s.delivery, number, carrier, amount, currency)
}

// TestDeliveryShipmentRules (S2r, migration 000462; ADR-0082 As built
// S2r): a delivery's shipment names its number and carrier's code of 1 to
// 40 characters, a freight cost not below zero of a known currency and
// when it was recorded, all or none; the application sets it once, while
// the delivery is created or after its goods issue, moving its version;
// a recorded shipment changes no more, and a goods-issued delivery
// changes by nothing else.
func TestDeliveryShipmentRules(t *testing.T) {
	need(t)
	issued := func(s shippingScene, more ...stmt) []stmt {
		return append(append(s.made(), step(shippingIssue, s.delivery, s.document)), more...)
	}
	cases := []struct {
		name string
		run  func(s shippingScene) []stmt
		want string // empty: allowed
	}{
		{"a created delivery's shipment", func(s shippingScene) []stmt { return append(s.made(), s.shipmentSet("SH1", "NWT", 4500, "CAD")) }, ""},
		{"a goods-issued delivery's shipment", func(s shippingScene) []stmt { return issued(s, s.shipmentSet("SH1", "NWT", 0, "CAD")) }, ""},
		{"issued after its shipment", func(s shippingScene) []stmt {
			return append(s.made(), s.shipmentSet("SH1", "NWT", 4500, "CAD"), step(shippingIssue, s.delivery, s.document))
		}, ""},
		{"an empty shipment number", func(s shippingScene) []stmt { return append(s.made(), s.shipmentSet("", "NWT", 1, "CAD")) },
			"outbound_delivery_shipment_number_check"},
		{"a shipment number of 41 characters", func(s shippingScene) []stmt {
			return append(s.made(), s.shipmentSet(strings.Repeat("S", 41), "NWT", 1, "CAD"))
		}, "outbound_delivery_shipment_number_check"},
		{"an empty carrier", func(s shippingScene) []stmt { return append(s.made(), s.shipmentSet("SH1", "", 1, "CAD")) },
			"outbound_delivery_carrier_code_check"},
		{"a carrier of 41 characters", func(s shippingScene) []stmt {
			return append(s.made(), s.shipmentSet("SH1", strings.Repeat("C", 41), 1, "CAD"))
		}, "outbound_delivery_carrier_code_check"},
		{"a negative freight cost", func(s shippingScene) []stmt { return append(s.made(), s.shipmentSet("SH1", "NWT", -1, "CAD")) },
			"outbound_delivery_freight_amount_check"},
		{"an unknown currency", func(s shippingScene) []stmt { return append(s.made(), s.shipmentSet("SH1", "NWT", 1, "XXX")) },
			"outbound_delivery_freight_currency_fkey"},
		{"a shipment without its cost", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery SET shipment_number = 'SH1', carrier_code = 'NWT', shipment_recorded_at = now(),
				version = version + 1 WHERE id = $1`, s.delivery))
		}, "outbound_delivery_shipment"},
		{"a shipment without its time", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery SET shipment_number = 'SH1', carrier_code = 'NWT', freight_amount = 1,
				freight_currency = 'CAD', version = version + 1 WHERE id = $1`, s.delivery))
		}, "outbound_delivery_shipment"},
		{"a cost without a shipment", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery SET freight_amount = 1, freight_currency = 'CAD', version = version + 1
				WHERE id = $1`, s.delivery))
		}, "outbound_delivery_shipment"},
		{"a shipment recorded twice", func(s shippingScene) []stmt {
			return append(s.made(), s.shipmentSet("SH1", "NWT", 1, "CAD"), s.shipmentSet("SH2", "NWT", 1, "CAD"))
		}, "its shipment is recorded, so it changes no more"},
		{"a shipment recorded twice after the goods issue", func(s shippingScene) []stmt {
			return issued(s, s.shipmentSet("SH1", "NWT", 1, "CAD"), s.shipmentSet("SH1", "NWT", 2, "CAD"))
		}, "its shipment is recorded, so it changes no more"},
		{"an issued delivery changed with a shipment recorded", func(s shippingScene) []stmt {
			return issued(s, s.shipmentSet("SH1", "NWT", 1, "CAD"), step(`UPDATE shipping.outbound_delivery SET version = version + 1`))
		}, "its goods issue is posted, so it changes no more"},
		{"an issued delivery's status with its shipment", func(s shippingScene) []stmt {
			return issued(s, step(`UPDATE shipping.outbound_delivery SET status = 'created', goods_issue_date = NULL, material_document_id = NULL,
				material_document_number = NULL, currency = NULL, shipment_number = 'SH1', carrier_code = 'NWT', freight_amount = 1, freight_currency = 'CAD',
				shipment_recorded_at = now(), version = version + 1`))
		}, "its goods issue is posted, so it changes no more"},
		{"a shipment without moving the version", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery SET shipment_number = 'SH1', carrier_code = 'NWT', freight_amount = 1,
				freight_currency = 'CAD', shipment_recorded_at = now()`))
		}, "optimistic lock"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := shippingScene{tenant: id(), company: id(), delivery: id(), document: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
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

// TestDeliveryShipmentAsOwner (S2r, migration 000462): past the grants, as
// the owner, a recorded shipment changes no more, and a goods-issued
// delivery takes its shipment alone.
func TestDeliveryShipmentAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		issued, shipped bool
		st              func(s shippingScene) stmt
		want            string
	}{
		"a shipment's cost changed": {false, true, func(s shippingScene) stmt {
			return step(`UPDATE shipping.outbound_delivery SET freight_amount = 2, version = version + 1 WHERE id = $1`, s.delivery)
		}, "outbound_delivery_shipment_kept"},
		"a shipment removed": {true, true, func(s shippingScene) stmt {
			return step(`UPDATE shipping.outbound_delivery SET shipment_number = NULL, carrier_code = NULL, freight_amount = NULL, freight_currency = NULL,
				shipment_recorded_at = NULL, version = version + 1 WHERE id = $1`, s.delivery)
		}, "outbound_delivery_shipment_kept"},
		"an issued delivery's number with its shipment": {true, false, func(s shippingScene) stmt {
			return step(`UPDATE shipping.outbound_delivery SET number = 'OD9', shipment_number = 'SH1', carrier_code = 'NWT', freight_amount = 1,
				freight_currency = 'CAD', shipment_recorded_at = now(), version = version + 1 WHERE id = $1`, s.delivery)
		}, "outbound_delivery_issued_fixed"},
	} {
		t.Run(name, func(t *testing.T) {
			s := shippingScene{tenant: id(), company: id(), delivery: id(), document: id()}
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", s.tenant.String()); err != nil {
				t.Fatal(err)
			}
			setup := s.made()
			if c.shipped {
				setup = append(setup, s.shipmentSet("SH1", "NWT", 1, "CAD"))
			}
			if c.issued {
				setup = append(setup, step(shippingIssue, s.delivery, s.document))
			}
			if err := all(tx, setup...); err != nil {
				t.Fatal(err)
			}
			st := c.st(s)
			failsOn(t, exec(tx, st.sql, st.args...), c.want)
		})
	}
}

// tmsConnector is a tms connector of the fixture's tenant.
func tmsConnector(f fixture, cid uuid.UUID) stmt {
	return step(`INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
		VALUES ($1, $2, 'tms', 'Transport', '{}', 'active', $3, $3)`, f.tenant, cid, f.actor)
}

// recordedIn are the columns of an inbound shipment recorded, a new map.
func recordedIn() map[string]any {
	return map[string]any{"direction": "inbound", "kind": "shipment", "status": "recorded", "next_attempt_at": nil, "plant_code": nil}
}

// TestTMSMessageRules (S2r, migration 000462): a tms connector's messages
// are integration.wms_message's of their own kinds: a transport request
// outbound, sent once per delivery beside the delivery's WMS message, and
// a shipment inbound, recorded (never posted, naming no material
// document) or refused; only a shipment is recorded.
func TestTMSMessageRules(t *testing.T) {
	need(t)
	for name, c := range map[string]struct {
		run  func(f fixture, cid uuid.UUID) []stmt
		want string // empty: allowed
	}{
		"a pending transport request": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, map[string]any{"kind": "transport_request"})}
		}, ""},
		"a recorded shipment": {func(f fixture, cid uuid.UUID) []stmt { return []stmt{wmsRow(f, cid, recordedIn())} }, ""},
		"a refused shipment": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, with(refusedIn(), map[string]any{"kind": "shipment"}))}
		}, ""},
		"a posted shipment": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, with(posted(), map[string]any{"kind": "shipment"}))}
		}, "wms_message_direction"},
		"a recorded goods issue": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, with(recordedIn(), map[string]any{"kind": "goods_issue"}))}
		}, "wms_message_direction"},
		"an inbound transport request": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, with(recordedIn(), map[string]any{"kind": "transport_request"}))}
		}, "wms_message_direction"},
		"an outbound shipment": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, map[string]any{"kind": "shipment"})}
		},
			"wms_message_direction"},
		"a recorded shipment naming a material document": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, with(recordedIn(), map[string]any{"material_document_id": id(), "material_document_number": "MD1"}))}
		}, "wms_message_inbound"},
		"a recorded shipment of no delivery": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, with(recordedIn(), map[string]any{"document_id": nil}))}
		}, "wms_message_inbound"},
		"another status": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, with(recordedIn(), map[string]any{"status": "planned"}))}
		}, "wms_message_status_check"},
		"a delivery's transport request twice": {func(f fixture, cid uuid.UUID) []stmt {
			doc := id()
			return []stmt{wmsRow(f, cid, map[string]any{"kind": "transport_request", "document_id": doc}),
				wmsRow(f, cid, map[string]any{"kind": "transport_request", "document_id": doc})}
		}, "wms_message_once"},
		"a delivery to its WMS and its TMS": {func(f fixture, cid uuid.UUID) []stmt {
			doc := id()
			return []stmt{wmsRow(f, cid, map[string]any{"document_id": doc}), wmsRow(f, cid, map[string]any{"kind": "transport_request", "document_id": doc})}
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			cid := id()
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return all(tx, append([]stmt{tmsConnector(f, cid)}, c.run(f, cid)...)...) })
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

// TestShipmentPrivileges (S2r, migration 000462): the application may set
// a delivery's shipment columns.
func TestShipmentPrivileges(t *testing.T) {
	need(t)
	for _, column := range []string{"shipment_number", "carrier_code", "freight_amount", "freight_currency", "shipment_recorded_at"} {
		t.Run(column, func(t *testing.T) {
			var got bool
			if err := testDB.QueryRow(context.Background(), `SELECT has_column_privilege('erp_app', 'shipping.outbound_delivery', $1, 'UPDATE')`,
				column).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if !got {
				t.Errorf("erp_app's UPDATE of %s: got false, want true", column)
			}
		})
	}
}

// TestTMSConnectorDown: 000462's down refuses, rather than drops, the tms
// connectors, their messages and the recorded shipments the previous
// version cannot hold (06 §6.4).
func TestTMSConnectorDown(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000462_tms_connector").Down
	for name, seed := range map[string]string{
		"a tms connector": `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), 'tms', 'T', '{}', 'active', $1, $1)`,
		"a transport request": `INSERT INTO integration.wms_message (tenant_id, id, connector_id, direction, message_id, kind, legal_entity_id, document_id,
			document_number, plant_code, body, status, next_attempt_at)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'outbound', 'M', 'transport_request', $1, $1, 'OD1', 'P1', '{}', 'pending', now())`,
		"a shipment": `INSERT INTO integration.wms_message (tenant_id, id, connector_id, direction, message_id, kind, body, status, problem_code, problem_detail)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'inbound', 'M', 'shipment', '{}', 'refused', 'c', 'd')`,
		"a delivery's shipment": `INSERT INTO shipping.outbound_delivery (tenant_id, id, legal_entity_id, fiscal_year, number, sales_order_id,
			sales_order_number, customer_id, customer_number, plant_id, storage_location_id, status, created_by, updated_by, shipment_number, carrier_code,
			freight_amount, freight_currency, shipment_recorded_at)
			VALUES ($1, gen_random_uuid(), $1, 2026, 'OD1', gen_random_uuid(), 'SO1', gen_random_uuid(), '100001', gen_random_uuid(), gen_random_uuid(),
			'created', $1, $1, 'SH1', 'NWT', 1, 'CAD', now())`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant := id()
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
				step(seed, tenant), step(`SET LOCAL session_replication_role = origin`), step(`SELECT set_config('app.tenant_id', '', true)`)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, down), "tms connectors or recorded shipments exist")
		})
	}
}
