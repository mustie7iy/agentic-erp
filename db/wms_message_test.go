package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// wmsConnector is a wms connector of the fixture's tenant.
func wmsConnector(f fixture, cid uuid.UUID) stmt {
	return step(`INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
		VALUES ($1, $2, 'wms', 'Warehouse', '{}', 'active', $3, $3)`, f.tenant, cid, f.actor)
}

// wmsRow is a message of the connector, an outbound one pending, with the
// columns of set changed.
func wmsRow(f fixture, cid uuid.UUID, set map[string]any) stmt {
	row := map[string]any{"id": id(), "direction": "outbound", "message_id": id().String(), "kind": "outbound_delivery", "legal_entity_id": f.entity,
		"document_id": id(), "document_number": "OD1", "plant_code": "P1", "status": "pending", "attempts": 0, "next_attempt_at": "2026-11-22T10:00:00Z",
		"last_error": nil, "sent_at": nil, "problem_code": nil, "problem_detail": nil, "material_document_id": nil, "material_document_number": nil,
		"acknowledged_at": nil}
	for k, v := range set {
		row[k] = v
	}
	return step(`INSERT INTO integration.wms_message (tenant_id, id, connector_id, direction, message_id, kind, legal_entity_id, document_id, document_number,
			plant_code, body, status, attempts, next_attempt_at, last_error, sent_at, problem_code, problem_detail, material_document_id,
			material_document_number, acknowledged_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, '{}', $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		f.tenant, row["id"], cid, row["direction"], row["message_id"], row["kind"], row["legal_entity_id"], row["document_id"], row["document_number"],
		row["plant_code"], row["status"], row["attempts"], row["next_attempt_at"], row["last_error"], row["sent_at"], row["problem_code"],
		row["problem_detail"], row["material_document_id"], row["material_document_number"], row["acknowledged_at"])
}

// sentOut are the columns of a sent outbound message; posted and refusedIn
// those of inbound ones; each a new map, which with may change.
func sentOut() map[string]any {
	return map[string]any{"status": "sent", "attempts": 1, "next_attempt_at": nil, "sent_at": "2026-11-22T10:00:01Z"}
}

func posted() map[string]any {
	return map[string]any{"direction": "inbound", "kind": "goods_issue", "status": "posted", "next_attempt_at": nil, "plant_code": nil,
		"material_document_id": id(), "material_document_number": "MD1"}
}

func refusedIn() map[string]any {
	return map[string]any{"direction": "inbound", "kind": "goods_issue", "status": "refused", "next_attempt_at": nil, "legal_entity_id": nil,
		"document_id": nil, "document_number": nil, "plant_code": nil, "problem_code": "integration.wms_unknown_delivery", "problem_detail": "Not sent."}
}

// TestWMSMessageRules: every rule the database enforces on the messages
// exchanged with warehouse systems (S2q1, 000445; S2q2's receipt kinds,
// 000447): the directions, kinds and statuses of each (one of none is refused by wms_message_direction,
// which PostgreSQL checks before the columns' own checks), what an
// outbound message names and an inbound one records, a time to wait for
// exactly while pending and a time sent exactly when sent, up to six
// attempts, the columns' lengths, a delivery sent once and a message id
// once per connector and direction, of a connector of the tenant; never
// deleted by the application.
func TestWMSMessageRules(t *testing.T) {
	need(t)
	for name, c := range map[string]struct {
		set  map[string]any
		want string // empty: allowed
	}{
		"a pending outbound message":     {nil, ""},
		"a sent one":                     {sentOut(), ""},
		"a failed one":                   {map[string]any{"status": "failed", "attempts": 6, "next_attempt_at": nil, "last_error": "integration.wms_refused"}, ""},
		"a posted inbound one":           {posted(), ""},
		"a refused one of no delivery":   {refusedIn(), ""},
		"another direction":              {map[string]any{"direction": "sideways"}, "wms_message_direction"},
		"an outbound goods receipt":      {map[string]any{"kind": "goods_receipt"}, "wms_message_direction"},
		"a pending inbound receipt":      {map[string]any{"kind": "inbound_receipt"}, ""},
		"a posted goods receipt":         {with(posted(), map[string]any{"kind": "goods_receipt"}), ""},
		"a refused goods receipt":        {with(refusedIn(), map[string]any{"kind": "goods_receipt"}), ""},
		"an inbound inbound receipt":     {with(posted(), map[string]any{"kind": "inbound_receipt"}), "wms_message_direction"},
		"another kind outbound":          {map[string]any{"kind": "putaway"}, "wms_message_direction"},
		"another kind inbound":           {with(posted(), map[string]any{"kind": "putaway"}), "wms_message_kind_check"},
		"another status":                 {map[string]any{"status": "lost"}, "wms_message_direction"},
		"an outbound goods issue":        {map[string]any{"kind": "goods_issue"}, "wms_message_direction"},
		"an outbound message posted":     {map[string]any{"status": "posted", "next_attempt_at": nil}, "wms_message_direction"},
		"an inbound message pending":     {with(posted(), map[string]any{"status": "pending", "next_attempt_at": "2026-11-22T10:00:00Z"}), "wms_message_direction"},
		"an outbound one of no delivery": {map[string]any{"document_id": nil}, "wms_message_outbound"},
		"an outbound one of no plant":    {map[string]any{"plant_code": nil}, "wms_message_outbound"},
		"an outbound one with a problem": {map[string]any{"problem_code": "x", "problem_detail": "y"}, "wms_message_outbound"},
		"an outbound one acknowledged":   {map[string]any{"acknowledged_at": "2026-11-22T10:00:01Z"}, "wms_message_outbound"},
		"failed unattempted":             {map[string]any{"status": "failed", "next_attempt_at": nil}, "wms_message_outbound"},
		"pending with no time":           {map[string]any{"next_attempt_at": nil}, "wms_message_pending"},
		"sent with a time to wait for":   {with(sentOut(), map[string]any{"next_attempt_at": "2026-11-22T10:00:00Z"}), "wms_message_pending"},
		"sent without its time":          {with(sentOut(), map[string]any{"sent_at": nil}), "wms_message_sent"},
		"pending with a time sent":       {map[string]any{"sent_at": "2026-11-22T10:00:01Z"}, "wms_message_sent"},
		"an inbound one attempted":       {with(posted(), map[string]any{"attempts": 1}), "wms_message_inbound"},
		"an inbound one with an error":   {with(posted(), map[string]any{"last_error": "x"}), "wms_message_inbound"},
		"refused without its problem":    {with(refusedIn(), map[string]any{"problem_code": nil, "problem_detail": nil}), "wms_message_inbound"},
		"a problem without its detail":   {with(refusedIn(), map[string]any{"problem_detail": nil}), "wms_message_inbound"},
		"posted with a problem":          {with(posted(), map[string]any{"problem_code": "x", "problem_detail": "y"}), "wms_message_inbound"},
		"posted without its document":    {with(posted(), map[string]any{"material_document_id": nil, "material_document_number": nil}), "wms_message_inbound"},
		"a document without its number":  {with(posted(), map[string]any{"material_document_number": nil}), "wms_message_inbound"},
		"refused naming a document":      {with(refusedIn(), map[string]any{"material_document_id": id(), "material_document_number": "MD1"}), "wms_message_inbound"},
		"posted of no delivery":          {with(posted(), map[string]any{"document_id": nil}), "wms_message_inbound"},
		"posted of no company":           {with(posted(), map[string]any{"legal_entity_id": nil}), "wms_message_inbound"},
		"seven attempts":                 {map[string]any{"attempts": 7}, "wms_message_attempts_check"},
		"an empty message id":            {map[string]any{"message_id": ""}, "wms_message_message_id_check"},
		"a long document number":         {map[string]any{"document_number": strings.Repeat("x", 41)}, "wms_message_document_number_check"},
		"a long plant code":              {map[string]any{"plant_code": "P123456789012345678901"}, "wms_message_plant_code_check"},
		"an empty last error":            {map[string]any{"last_error": ""}, "wms_message_last_error_check"},
		"an empty problem code":          {with(refusedIn(), map[string]any{"problem_code": ""}), "wms_message_problem_code_check"},
		"an empty problem detail":        {with(refusedIn(), map[string]any{"problem_detail": ""}), "wms_message_problem_detail_check"},
		"an empty document number":       {with(posted(), map[string]any{"material_document_number": ""}), "wms_message_material_document_number_check"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			cid := id()
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return all(tx, wmsConnector(f, cid), wmsRow(f, cid, c.set)) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	for name, c := range map[string]struct {
		run  func(f fixture, cid uuid.UUID) []stmt
		want string
	}{
		"a delivery sent twice": {func(f fixture, cid uuid.UUID) []stmt {
			doc := id()
			return []stmt{wmsRow(f, cid, map[string]any{"document_id": doc}), wmsRow(f, cid, map[string]any{"document_id": doc})}
		}, "wms_message_once"},
		"an order sent twice": {func(f fixture, cid uuid.UUID) []stmt {
			doc := id()
			return []stmt{wmsRow(f, cid, map[string]any{"kind": "inbound_receipt", "document_id": doc}),
				wmsRow(f, cid, map[string]any{"kind": "inbound_receipt", "document_id": doc, "plant_code": "P2"})}
		}, "wms_message_once"},
		"a delivery reported twice": {func(f fixture, cid uuid.UUID) []stmt {
			doc := id()
			return []stmt{wmsRow(f, cid, map[string]any{"document_id": doc}), wmsRow(f, cid, with(posted(), map[string]any{"document_id": doc}))}
		}, ""},
		"a message id twice": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, with(posted(), map[string]any{"message_id": "M1"})), wmsRow(f, cid, with(refusedIn(), map[string]any{"message_id": "M1"}))}
		}, "wms_message_tenant_id_connector_id_direction_message_id_key"},
		"a message id each way": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, map[string]any{"message_id": "M1"}), wmsRow(f, cid, with(posted(), map[string]any{"message_id": "M1"}))}
		}, ""},
		"an unknown connector": {func(f fixture, _ uuid.UUID) []stmt { return []stmt{wmsRow(f, id(), nil)} }, "wms_message_tenant_id_connector_id_fkey"},
		"deleted": {func(f fixture, cid uuid.UUID) []stmt {
			return []stmt{wmsRow(f, cid, nil), step(`DELETE FROM integration.wms_message WHERE connector_id = $1`, cid)}
		}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			cid := id()
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return all(tx, append([]stmt{wmsConnector(f, cid)}, c.run(f, cid)...)...) })
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

// TestWMSMessageKept: what a message is never changes, an inbound one's
// outcome stays as recorded and its acknowledgement is set once, and a
// sent or failed outbound one keeps its status, for the owner too
// (wms_message_kept, 000445); what the exchange changes it may.
func TestWMSMessageKept(t *testing.T) {
	need(t)
	for name, c := range map[string]struct {
		set    map[string]any
		update string
		want   string // empty: allowed
	}{
		"a pending one sent":           {nil, `status = 'sent', attempts = 1, next_attempt_at = NULL, sent_at = now()`, ""},
		"a pending one attempted":      {nil, `attempts = 1, last_error = 'platform.internal'`, ""},
		"an inbound one acknowledged":  {posted(), `acknowledged_at = now()`, ""},
		"its body":                     {nil, `body = '{"x": 1}'`, "wms_message_kept"},
		"its delivery":                 {nil, `document_id = gen_random_uuid()`, "wms_message_kept"},
		"its connector's message id":   {nil, `message_id = 'other'`, "wms_message_kept"},
		"its plant":                    {nil, `plant_code = 'P2'`, "wms_message_kept"},
		"an inbound one's outcome":     {posted(), `material_document_number = 'MD2'`, "wms_message_kept"},
		"an inbound one refused since": {posted(), `status = 'refused', problem_code = 'x', problem_detail = 'y', material_document_id = NULL, material_document_number = NULL`, "wms_message_kept"},
		"its acknowledgement again":    {with(posted(), map[string]any{"acknowledged_at": "2026-11-22T10:00:01Z"}), `acknowledged_at = now()`, "wms_message_kept"},
		"a sent one pending again":     {sentOut(), `status = 'pending', sent_at = NULL, next_attempt_at = now()`, "wms_message_kept"},
		"a failed one pending again":   {map[string]any{"status": "failed", "attempts": 6, "next_attempt_at": nil}, `status = 'pending', next_attempt_at = now()`, "wms_message_kept"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			cid := id()
			mustApp(t, f.tenant, func(tx pgx.Tx) error { return all(tx, wmsConnector(f, cid), wmsRow(f, cid, c.set)) })
			err := asApp(t, f.tenant, asOwner(func(tx pgx.Tx) error {
				return all(tx, step(`UPDATE integration.wms_message SET `+c.update+`, version = version + 1 WHERE connector_id = $1`, cid))
			}))
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

// TestWMSMessagePrivileges: the application may insert, read and change
// messages, never delete them; the worker reads only what finds the
// tenants with messages due; RLS holds the table to its tenant.
func TestWMSMessagePrivileges(t *testing.T) {
	need(t)
	for _, c := range []struct {
		check string
		want  bool
	}{
		{`has_table_privilege('erp_app', 'integration.wms_message', 'INSERT')`, true},
		{`has_table_privilege('erp_app', 'integration.wms_message', 'UPDATE')`, true},
		{`has_table_privilege('erp_app', 'integration.wms_message', 'DELETE')`, false},
		{`has_column_privilege('erp_worker', 'integration.wms_message', 'next_attempt_at', 'SELECT')`, true},
		{`has_column_privilege('erp_worker', 'integration.wms_message', 'body', 'SELECT')`, false},
		{`(SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid = 'integration.wms_message'::regclass)`, true},
	} {
		t.Run(c.check, func(t *testing.T) {
			var got bool
			if err := testDB.QueryRow(context.Background(), `SELECT `+c.check).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestWMSConnectorDown: 000445's down refuses, rather than drops, the wms
// connectors and messages the previous version cannot hold (06 §6.4).
func TestWMSConnectorDown(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000445_wms_connector").Down
	for name, seed := range map[string]string{
		"a wms connector": `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
			VALUES ($1, gen_random_uuid(), 'wms', 'W', '{}', 'active', $1, $1)`,
		"a message": `INSERT INTO integration.wms_message (tenant_id, id, connector_id, direction, message_id, kind, body, status, problem_code, problem_detail)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'inbound', 'M', 'goods_issue', '{}', 'refused', 'c', 'd')`,
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
			failsWith(t, exec(tx, down), "wms connectors exist")
		})
	}
}

// TestWMSReceiptsDown: 000447's down refuses, rather than drops, the
// receipt messages the previous version cannot hold (06 §6.4).
func TestWMSReceiptsDown(t *testing.T) {
	need(t)
	down := migrationNamed(t, "000447_wms_receipts").Down
	for kind, seed := range map[string]string{
		"inbound_receipt": `INSERT INTO integration.wms_message (tenant_id, id, connector_id, direction, message_id, kind, legal_entity_id, document_id,
			document_number, plant_code, body, status, next_attempt_at)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'outbound', 'M', 'inbound_receipt', $1, $1, 'PO1', 'P1', '{}', 'pending', now())`,
		"goods_receipt": `INSERT INTO integration.wms_message (tenant_id, id, connector_id, direction, message_id, kind, body, status, problem_code, problem_detail)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), 'inbound', 'M', 'goods_receipt', '{}', 'refused', 'c', 'd')`,
	} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			tenant := id()
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String()), step(`SET LOCAL session_replication_role = replica`),
				step(seed, tenant),
				step(`SET LOCAL session_replication_role = origin`), step(`SELECT set_config('app.tenant_id', '', true)`)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, down), "wms receipt messages exist")
		})
	}
}
