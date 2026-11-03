package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// snapshotRow is a cash snapshot to insert: a daily one of the fixture's
// company on 2026-04-17, with the columns of set changed.
func snapshotRow(f fixture, sid uuid.UUID, set map[string]any) stmt {
	row := map[string]any{"kind": "daily", "name": nil, "snapshot_date": "2026-04-17", "horizon_end": "2027-04-02", "config_version": 1,
		"time_zone": "America/Toronto"}
	for k, v := range set {
		row[k] = v
	}
	return step(`INSERT INTO cash.snapshot (tenant_id, id, legal_entity_id, kind, name, snapshot_date, time_zone, horizon_end, config_version, taken_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		f.tenant, sid, f.entity, row["kind"], row["name"], row["snapshot_date"], row["time_zone"], row["horizon_end"], row["config_version"], f.actor)
}

// flowRow is a line of the snapshot: a document-level outflow of a journal
// entry under PAY, with the columns of set changed.
func flowRow(f fixture, sid uuid.UUID, line int, set map[string]any) stmt {
	row := map[string]any{"value_date": "2026-04-21", "bucket_end": nil, "certainty": "expected", "liquidity_item": "PAY", "flow_count": 1,
		"supplier_id": f.supplier, "payment_id": nil, "document_kind": "journal_entry", "document_id": id(), "document_number": "5100000012", "late": false,
		"source": "payable", "flow_no": 1, "currency": "EUR"}
	for k, v := range set {
		row[k] = v
	}
	return step(`INSERT INTO cash.snapshot_flow (tenant_id, snapshot_id, line_no, flow_no, value_date, bucket_end, currency, source, certainty, late,
			liquidity_item, amount, flow_count, supplier_id, payment_id, document_kind, document_id, document_number)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, -78400, $12, $13, $14, $15, $16, $17)`,
		f.tenant, sid, line, row["flow_no"], row["value_date"], row["bucket_end"], row["currency"], row["source"], row["certainty"], row["late"],
		row["liquidity_item"], row["flow_count"], row["supplier_id"], row["payment_id"], row["document_kind"], row["document_id"], row["document_number"])
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
		{"a name of 200 characters", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, map[string]any{"kind": "on_demand", "name": strings.Repeat("B", 200)})}
		}, ""},
		{"a name of 201 characters", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, map[string]any{"kind": "on_demand", "name": strings.Repeat("B", 201)})}
		}, "snapshot_name_check"},
		{"no time zone", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, map[string]any{"time_zone": ""})}
		}, "snapshot_time_zone_check"},
		{"a time zone of 65 characters", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, map[string]any{"time_zone": strings.Repeat("Z", 65)})}
		}, "snapshot_time_zone_check"},
		{"a line number", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 0, nil)}
		}, "snapshot_flow_line_no_check"},
		{"a flow number", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"flow_no": 0})}
		}, "snapshot_flow_flow_no_check"},
		{"a count of no flow", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"value_date": "2026-05-18", "bucket_end": "2026-05-24", "flow_count": 0,
				"supplier_id": nil, "document_kind": nil, "document_id": nil, "document_number": nil})}
		}, "snapshot_flow_flow_count_check"},
		{"an unknown currency", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"currency": "XYZ"})}
		}, "snapshot_flow_currency_fkey"},
		{"no document number", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"document_number": ""})}
		}, "snapshot_flow_document_number_check"},
		{"a document number of 101 characters", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, map[string]any{"document_number": strings.Repeat("5", 101)})}
		}, "snapshot_flow_document_number_check"},
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
		{"a snapshot deleted", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), step(`DELETE FROM cash.snapshot WHERE id = $1`, sid)}
		}, "permission denied"},
		{"a line deleted", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{snapshotRow(f, sid, nil), flowRow(f, sid, 1, nil), step(`DELETE FROM cash.snapshot_flow WHERE snapshot_id = $1`, sid)}
		}, "permission denied"},
		{"retention on a day after tomorrow", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{step(`SELECT count(*) FROM cash.delete_expired_snapshots($1, current_date + 2)`, f.entity)}
		}, "a day after tomorrow"},
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
		{`has_table_privilege('erp_app', 'cash.snapshot', 'DELETE')`, false},
		{`has_function_privilege('erp_app', 'cash.delete_expired_snapshots(uuid, date)', 'EXECUTE')`, true},
		{`has_function_privilege('erp_readonly', 'cash.delete_expired_snapshots(uuid, date)', 'EXECUTE')`, false},
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

// TestCashSnapshotRetentionRule: the retention function deletes a
// company's snapshots dated more than 25 months before the day, months
// back clamped to the month's last day, with their lines, and no other
// tenant's (T53f, 000192).
func TestCashSnapshotRetentionRule(t *testing.T) {
	need(t)
	f, other := newFixture(t), newFixture(t)
	old, kept, leap := id(), id(), id()
	mustApp(t, other.tenant, func(tx pgx.Tx) error {
		return all(tx, snapshotRow(other, id(), map[string]any{"snapshot_date": "2024-01-01", "horizon_end": "2024-12-31"}))
	})
	var deleted []uuid.UUID
	var lines int
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		err := all(tx, memoItems(f),
			snapshotRow(f, old, map[string]any{"snapshot_date": "2024-02-28", "horizon_end": "2024-12-31"}), flowRow(f, old, 1, nil),
			snapshotRow(f, leap, map[string]any{"snapshot_date": "2024-02-29", "horizon_end": "2024-12-31", "kind": "on_demand", "name": "Leap"}),
			snapshotRow(f, kept, map[string]any{"snapshot_date": "2024-03-01", "horizon_end": "2024-12-31", "kind": "on_demand", "name": "Kept"}))
		if err != nil {
			return err
		}
		rows, _ := tx.Query(context.Background(), `SELECT deleted_id FROM cash.delete_expired_snapshots($1, '2026-03-31') ORDER BY deleted_date`, f.entity)
		if deleted, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM cash.snapshot_flow WHERE snapshot_id = $1`, old).Scan(&lines)
	})
	if len(deleted) != 1 || deleted[0] != old {
		t.Errorf("deleted on 31 March 2026 (before 29 February 2024): got %v, want only %v", deleted, old)
	}
	if lines != 0 {
		t.Errorf("the deleted snapshot's lines: got %d, want none", lines)
	}
	var others int
	mustApp(t, other.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM cash.snapshot`).Scan(&others)
	})
	if others != 1 {
		t.Errorf("another tenant's snapshot: got %d, want it kept", others)
	}
}
