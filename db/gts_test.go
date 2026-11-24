package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// tradeSet sets the scene's delivery's trade compliance, as shipping's
// Create, RecordScreening and ReleaseTradeComplianceBlock do (S2s, 000467):
// its status, reason code, whether the service answered and whether a
// person released it.
func (s shippingScene) tradeSet(status, reason any, screened, released bool) stmt {
	return step(`UPDATE shipping.outbound_delivery SET trade_compliance_status = $2, trade_compliance_reason = $3,
		trade_compliance_screened_at = CASE WHEN $4 THEN now() END, trade_compliance_released_by = CASE WHEN $5 THEN $1 END,
		trade_compliance_released_by_name = CASE WHEN $5 THEN 'Tess Trade' END, trade_compliance_released_at = CASE WHEN $5 THEN now() END, version = version + 1 WHERE id = $1`, s.delivery, status, reason, screened, released)
}

// TestDeliveryTradeComplianceRules (S2s, migration 000467; ADR-0082 As
// built S2s): a delivery's trade compliance is none, pending without an
// answer, blocked with the service's answer, or released by the service's
// answer or a person's release, who and when together; a reason code of 1
// to 40 characters comes with an answer; it changes while the delivery is
// created, moving its version, and a goods-issued delivery's no more.
func TestDeliveryTradeComplianceRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s shippingScene) []stmt
		want string // empty: allowed
	}{
		{"pending", func(s shippingScene) []stmt { return append(s.made(), s.tradeSet("pending", nil, false, false)) }, ""},
		{"blocked with a reason", func(s shippingScene) []stmt { return append(s.made(), s.tradeSet("blocked", "SPL", true, false)) }, ""},
		{"released by the service", func(s shippingScene) []stmt { return append(s.made(), s.tradeSet("released", nil, true, false)) }, ""},
		{"released by a person while pending", func(s shippingScene) []stmt { return append(s.made(), s.tradeSet("released", nil, false, true)) }, ""},
		{"released by a person, the block's reason kept", func(s shippingScene) []stmt {
			return append(s.made(), s.tradeSet("released", "SPL", true, true))
		}, ""},
		{"another status", func(s shippingScene) []stmt { return append(s.made(), s.tradeSet("review", nil, true, false)) },
			"outbound_delivery_trade_compliance_status_check"},
		{"an empty reason", func(s shippingScene) []stmt { return append(s.made(), s.tradeSet("blocked", "", true, false)) },
			"outbound_delivery_trade_compliance_reason_check"},
		{"a reason of 41 characters", func(s shippingScene) []stmt {
			return append(s.made(), s.tradeSet("blocked", strings.Repeat("R", 41), true, false))
		}, "outbound_delivery_trade_compliance_reason_check"},
		{"pending with an answer", func(s shippingScene) []stmt { return append(s.made(), s.tradeSet("pending", nil, true, false)) },
			"outbound_delivery_trade_compliance"},
		{"blocked without an answer", func(s shippingScene) []stmt { return append(s.made(), s.tradeSet("blocked", nil, false, false)) },
			"outbound_delivery_trade_compliance"},
		{"blocked by a person", func(s shippingScene) []stmt { return append(s.made(), s.tradeSet("blocked", nil, true, true)) },
			"outbound_delivery_trade_compliance"},
		{"released by no one", func(s shippingScene) []stmt { return append(s.made(), s.tradeSet("released", nil, false, false)) },
			"outbound_delivery_trade_compliance"},
		{"a reason without an answer", func(s shippingScene) []stmt { return append(s.made(), s.tradeSet("released", "SPL", false, true)) },
			"outbound_delivery_trade_compliance"},
		{"an answer without a status", func(s shippingScene) []stmt { return append(s.made(), s.tradeSet(nil, nil, true, false)) },
			"outbound_delivery_trade_compliance"},
		{"a release without its time", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery SET trade_compliance_status = 'released', trade_compliance_released_by = id,
				trade_compliance_released_by_name = 'Tess Trade', version = version + 1 WHERE id = $1`, s.delivery))
		}, "outbound_delivery_trade_compliance"},
		{"a release without its name", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery SET trade_compliance_status = 'released', trade_compliance_released_by = id,
				trade_compliance_released_at = now(), version = version + 1 WHERE id = $1`, s.delivery))
		}, "outbound_delivery_trade_compliance"},
		{"after the goods issue", func(s shippingScene) []stmt {
			return append(s.made(), step(shippingIssue, s.delivery, s.document), s.tradeSet("blocked", nil, true, false))
		}, "its goods issue is posted, so it changes no more"},
		{"without moving the version", func(s shippingScene) []stmt {
			return append(s.made(), step(`UPDATE shipping.outbound_delivery SET trade_compliance_status = 'pending' WHERE id = $1`, s.delivery))
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

// TestDeliveryTradeComplianceAsOwner (S2s, migration 000467): past the
// grants, as the owner, a goods-issued delivery's trade compliance changes
// no more.
func TestDeliveryTradeComplianceAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := shippingScene{tenant: id(), company: id(), delivery: id(), document: id()}
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", s.tenant.String()); err != nil {
		t.Fatal(err)
	}
	if err := all(tx, append(s.made(), s.tradeSet("released", nil, true, false), step(shippingIssue, s.delivery, s.document))...); err != nil {
		t.Fatal(err)
	}
	st := s.tradeSet("blocked", "SPL", true, false)
	failsOn(t, exec(tx, st.sql, st.args...), "outbound_delivery_issued_fixed")
}

// gtsConnector is a gts connector of the fixture's tenant.
func gtsConnector(f fixture, cid uuid.UUID) stmt {
	return step(`INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
		VALUES ($1, $2, 'gts', 'Compliance', '{}', 'active', $3, $3)`, f.tenant, cid, f.actor)
}

// screenedIn are the columns of an inbound screening result recorded, a
// new map.
func screenedIn() map[string]any {
	return with(recordedIn(), map[string]any{"kind": "screening_result"})
}

// TestGTSMessageRules (S2s, migration 000467): a gts connector's messages
// are integration.wms_message's of their own kinds: a screening request
// outbound, sent once per delivery beside its other systems' messages,
// and a screening result inbound, recorded (never posted) or refused.
func TestGTSMessageRules(t *testing.T) {
	need(t)
	for name, c := range map[string]struct {
		run  func(f fixture, cid uuid.UUID) []stmt
		want string // empty: allowed
	}{
		"a pending screening request": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, map[string]any{"kind": "screening_request"})}
		}, ""},
		"a recorded screening result": {func(f fixture, cid uuid.UUID) []stmt { return []stmt{wmsRow(f, cid, screenedIn())} }, ""},
		"a refused screening result": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, with(refusedIn(), map[string]any{"kind": "screening_result"}))}
		}, ""},
		"a posted screening result": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, with(posted(), map[string]any{"kind": "screening_result"}))}
		}, "wms_message_direction"},
		"an inbound screening request": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, with(recordedIn(), map[string]any{"kind": "screening_request"}))}
		}, "wms_message_direction"},
		"an outbound screening result": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, map[string]any{"kind": "screening_result"})}
		}, "wms_message_direction"},
		"another kind": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, with(refusedIn(), map[string]any{"kind": "customs_declaration"}))}
		}, "wms_message_kind_check"},
		"a delivery's screening request twice": {func(f fixture, cid uuid.UUID) []stmt {
			doc := id()
			return []stmt{wmsRow(f, cid, map[string]any{"kind": "screening_request", "document_id": doc}),
				wmsRow(f, cid, map[string]any{"kind": "screening_request", "document_id": doc})}
		}, "wms_message_once"},
		"a delivery to its WMS, TMS and GTS": {func(f fixture, cid uuid.UUID) []stmt {
			doc := id()
			return []stmt{wmsRow(f, cid, map[string]any{"document_id": doc}), wmsRow(f, cid, map[string]any{"kind": "transport_request", "document_id": doc}),
				wmsRow(f, cid, map[string]any{"kind": "screening_request", "document_id": doc})}
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			cid := id()
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return all(tx, append([]stmt{gtsConnector(f, cid)}, c.run(f, cid)...)...) })
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

// TestTradeCompliancePrivileges (S2s, migration 000467): the application
// may set a delivery's trade compliance columns, and the release
// permission exists.
func TestTradeCompliancePrivileges(t *testing.T) {
	need(t)
	ctx := context.Background()
	for _, column := range []string{"trade_compliance_status", "trade_compliance_reason", "trade_compliance_screened_at", "trade_compliance_released_by",
		"trade_compliance_released_by_name", "trade_compliance_released_at"} {
		t.Run(column, func(t *testing.T) {
			var got bool
			if err := testDB.QueryRow(ctx, `SELECT has_column_privilege('erp_app', 'shipping.outbound_delivery', $1, 'UPDATE')`, column).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if !got {
				t.Errorf("erp_app's UPDATE of %s: got false, want true", column)
			}
		})
	}
	var n int
	if err := testDB.QueryRow(ctx, `SELECT count(*) FROM ref.permission WHERE code = 'shipping.trade_compliance.release' AND NOT is_approval`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the release permission: got %d, want 1", n)
	}
}

// TestGTSConnectorDown: 000467's down refuses, rather than drops, the gts
// connectors, their messages and the screened deliveries the previous
// version cannot hold (06 §6.4), and a role held to row-level security
// that would see none of them fails rather than drops them.
func TestGTSConnectorDown(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000467_gts_connector").Down
	for name, seed := range map[string]string{
		"a gts connector": `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), 'gts', 'G', '{}', 'active', $1, $1)`,
		"a screening request": `INSERT INTO integration.wms_message (tenant_id, id, connector_id, direction, message_id, kind, legal_entity_id, document_id,
			document_number, plant_code, body, status, next_attempt_at)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'outbound', 'M', 'screening_request', $1, $1, 'OD1', 'P1', '{}', 'pending', now())`,
		"a screening result": `INSERT INTO integration.wms_message (tenant_id, id, connector_id, direction, message_id, kind, body, status, problem_code,
			problem_detail) VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'inbound', 'M', 'screening_result', '{}', 'refused', 'c', 'd')`,
		"a screened delivery": `INSERT INTO shipping.outbound_delivery (tenant_id, id, legal_entity_id, fiscal_year, number, sales_order_id,
			sales_order_number, customer_id, customer_number, plant_id, storage_location_id, status, created_by, updated_by, trade_compliance_status)
			VALUES ($1, gen_random_uuid(), $1, 2026, 'OD1', gen_random_uuid(), 'SO1', gen_random_uuid(), '100001', gen_random_uuid(), gen_random_uuid(),
			'created', $1, $1, 'pending')`,
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
				step(seed, tenant), step(`SET LOCAL session_replication_role = origin`), step(`SELECT set_config('app.tenant_id', '', true)`),
				step(`SAVEPOINT migrator`)); err != nil {
				t.Fatal(err)
			}
			if err := asMigrator(tx); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, down), "row-level security")
			if err := exec(tx, `ROLLBACK TO SAVEPOINT migrator`); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, down), "gts connectors or screened deliveries exist")
		})
	}
}
