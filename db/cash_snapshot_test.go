package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// snapshotRow is a cash snapshot to insert: a daily one of the fixture's
// company on 2026-04-17, with the columns of set changed.
func snapshotRow(f fixture, sid uuid.UUID, set map[string]any) stmt {
	row := map[string]any{"kind": "daily", "name": nil, "snapshot_date": "2026-04-17", "horizon_end": "2027-04-02", "config_version": 1}
	for k, v := range set {
		row[k] = v
	}
	return step(`INSERT INTO cash.snapshot (tenant_id, id, legal_entity_id, kind, name, snapshot_date, time_zone, horizon_end, config_version, taken_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'America/Toronto', $7, $8, $9)`,
		f.tenant, sid, f.entity, row["kind"], row["name"], row["snapshot_date"], row["horizon_end"], row["config_version"], f.actor)
}

// flowRow is a line of the snapshot: a document-level outflow of a journal
// entry under PAY, with the columns of set changed.
func flowRow(f fixture, sid uuid.UUID, line int, set map[string]any) stmt {
	row := map[string]any{"value_date": "2026-04-21", "bucket_end": nil, "certainty": "expected", "liquidity_item": "PAY", "flow_count": 1,
		"supplier_id": f.supplier, "payment_id": nil, "document_kind": "journal_entry", "document_id": id(), "document_number": "5100000012", "late": false,
		"source": "payable"}
	for k, v := range set {
		row[k] = v
	}
	return step(`INSERT INTO cash.snapshot_flow (tenant_id, snapshot_id, line_no, flow_no, value_date, bucket_end, currency, source, certainty, late,
			liquidity_item, amount, flow_count, supplier_id, payment_id, document_kind, document_id, document_number)
		VALUES ($1, $2, $3, 1, $4, $5, 'EUR', $6, $7, $8, $9, -78400, $10, $11, $12, $13, $14, $15)`,
		f.tenant, sid, line, row["value_date"], row["bucket_end"], row["source"], row["certainty"], row["late"], row["liquidity_item"], row["flow_count"],
		row["supplier_id"], row["payment_id"], row["document_kind"], row["document_id"], row["document_number"])
}

// TestCashSnapshotRules: every rule the database enforces on snapshots of
// the liquidity forecast (T53f, 000192): one daily snapshot per company and
// date, a person's named, the horizon not before the date; a line's
// document whole, a bucket's sum naming no document, supplier or payment
// and never late, a document-level line one flow, its item the tenant's;
// insert-only, deleted with their lines.
func TestCashSnapshotRules(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name string
		run  func(f fixture, sid uuid.UUID) []stmt
		want string // empty: allowed
	}{
		{"a daily snapshot and its line", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, nil)}
		}, ""},
		{"two daily snapshots of a date", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), snapshotRow(f, id(), nil)}
		}, "snapshot_daily"},
		{"a daily one and a person's of a date", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), snapshotRow(f, id(), map[string]any{"kind": "on_demand", "name": "Board"}),
				snapshotRow(f, id(), map[string]any{"kind": "on_demand", "name": "Board again"})}
		}, ""},
		{"a person's without a name", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, map[string]any{"kind": "on_demand"})}
		}, "snapshot_name"},
		{"a daily one named", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, map[string]any{"name": "Board"})}
		}, "snapshot_name"},
		{"a name with a space around it", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, map[string]any{"kind": "on_demand", "name": " Board"})}
		}, "snapshot_name_check"},
		{"a kind", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, map[string]any{"kind": "weekly"})}
		}, "snapshot_kind_check"},
		{"a horizon before the date", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, map[string]any{"horizon_end": "2026-04-16"})}
		}, "snapshot_horizon"},
		{"a configuration version", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, map[string]any{"config_version": 0})}
		}, "snapshot_config_version_check"},
		{"a snapshot changed", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), step(`UPDATE cash.snapshot SET horizon_end = '2027-05-01' WHERE id = $1`, sid)}
		}, "permission denied"},
		{"a line changed", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, nil), step(`UPDATE cash.snapshot_flow SET amount = 0 WHERE snapshot_id = $1`, sid)}
		}, "permission denied"},
		{"a line of no snapshot", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{flowRow(f, sid, 1, nil)}
		}, "snapshot_flow_tenant_id_snapshot_id_fkey"},
		{"a line of an unknown item", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"liquidity_item": "NOPE"})}
		}, "snapshot_flow_tenant_id_liquidity_item_fkey"},
		{"an unclassified line", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"liquidity_item": nil})}
		}, ""},
		{"a document without its kind", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"document_kind": nil})}
		}, "snapshot_flow_document"},
		{"a number without a document", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"document_kind": nil, "document_id": nil})}
		}, "snapshot_flow_document"},
		{"a document kind", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"document_kind": "invoice"})}
		}, "snapshot_flow_document_kind_check"},
		{"a certainty", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"certainty": "likely"})}
		}, "snapshot_flow_certainty_check"},
		{"a source", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"source": "Payable"})}
		}, "snapshot_flow_source_check"},
		{"a bucket's sum", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"value_date": "2026-05-18", "bucket_end": "2026-05-24", "flow_count": 3,
				"supplier_id": nil, "document_kind": nil, "document_id": nil, "document_number": nil})}
		}, ""},
		{"a bucket's sum naming a document", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"value_date": "2026-05-18", "bucket_end": "2026-05-24", "supplier_id": nil})}
		}, "snapshot_flow_bucket"},
		{"a bucket's sum naming a supplier", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"value_date": "2026-05-18", "bucket_end": "2026-05-24",
				"document_kind": nil, "document_id": nil, "document_number": nil})}
		}, "snapshot_flow_bucket"},
		{"a bucket's sum naming a payment", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"value_date": "2026-05-18", "bucket_end": "2026-05-24", "supplier_id": nil,
				"payment_id": id(), "document_kind": nil, "document_id": nil, "document_number": nil})}
		}, "snapshot_flow_bucket"},
		{"a late bucket's sum", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"value_date": "2026-05-18", "bucket_end": "2026-05-24", "supplier_id": nil,
				"late": true, "document_kind": nil, "document_id": nil, "document_number": nil})}
		}, "snapshot_flow_bucket"},
		{"a bucket ending before it starts", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"value_date": "2026-05-18", "bucket_end": "2026-05-17", "supplier_id": nil,
				"document_kind": nil, "document_id": nil, "document_number": nil})}
		}, "snapshot_flow_bucket"},
		{"a document-level line of two flows", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"flow_count": 2})}
		}, "snapshot_flow_count"},
		{"a snapshot deleted with its lines", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, nil), step(`DELETE FROM cash.snapshot WHERE id = $1`, sid)}
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return all(tx, append([]stmt{memoItems(f)}, c.run(f, id())...)...) })
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

// TestCashSnapshotPrivileges: the snapshot permission is seeded and is no
// approval; the worker role finds the tenants that manage cash from their
// liquidity items and reads nothing else of the schema (T53f, 000192).
func TestCashSnapshotPrivileges(t *testing.T) {
	need(t)
	var approval bool
	if err := testDB.QueryRow(context.Background(), `SELECT is_approval FROM ref.permission WHERE code = 'cash.snapshot.take'`).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	if approval {
		t.Error("cash.snapshot.take: got an approval permission, want none")
	}
	for _, c := range []struct {
		check string
		want  bool
	}{
		{`has_column_privilege('erp_worker', 'cash.liquidity_item', 'tenant_id', 'SELECT')`, true},
		{`has_column_privilege('erp_worker', 'cash.liquidity_item', 'name', 'SELECT')`, false},
		{`has_table_privilege('erp_worker', 'cash.snapshot', 'SELECT')`, false},
		{`has_table_privilege('erp_app', 'cash.snapshot', 'DELETE')`, true},
		{`has_table_privilege('erp_app', 'cash.snapshot_flow', 'UPDATE')`, false},
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
