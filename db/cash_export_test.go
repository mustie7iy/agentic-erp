package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// exportRow is an export of the fixture's snapshot sid, pending, with the
// columns of set changed.
func exportRow(f fixture, sid uuid.UUID, set map[string]any) stmt {
	row := map[string]any{"legal_entity_id": f.entity, "format": "csv", "status": "pending", "attempts": 0, "next_attempt_at": "2026-04-17T13:05:00Z",
		"file_name": nil, "sha256": nil, "resend": nil, "delivered_at": nil}
	for k, v := range set {
		row[k] = v
	}
	return step(`INSERT INTO cash.export (tenant_id, id, snapshot_id, legal_entity_id, connector_id, format, status, attempts, next_attempt_at, file_name, sha256,
			resend, delivered_at, requested_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		f.tenant, id(), sid, row["legal_entity_id"], id(), row["format"], row["status"], row["attempts"], row["next_attempt_at"], row["file_name"], row["sha256"],
		row["resend"], row["delivered_at"], f.actor)
}

// delivered are the columns of a delivered export of hash h.
func delivered(h string) map[string]any {
	return map[string]any{"status": "delivered", "attempts": 1, "next_attempt_at": nil, "file_name": "cash-1000-2026-04-17-" + h + ".csv",
		"sha256": make([]byte, 32), "resend": false, "delivered_at": "2026-04-17T13:05:09Z"}
}

// TestCashExportRules: every rule the database enforces on exports of
// snapshots (T53h, 000196): a format and status of the contract, up to six
// attempts, a time to wait for exactly while pending, a delivered export
// with its file, time and resend, a file named by company, date, hash and
// format with a hash of 32 bytes, of the snapshot's company; a connector
// kind treasury_export; changed only as a version, never deleted, and
// deleted with its snapshot.
func TestCashExportRules(t *testing.T) {
	need(t)
	h := strings.Repeat("ab", 32)
	with := func(set map[string]any, more map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range set {
			out[k] = v
		}
		for k, v := range more {
			out[k] = v
		}
		return out
	}
	for _, c := range []struct {
		name string
		run  func(f fixture, sid uuid.UUID) []stmt
		want string // empty: allowed
	}{
		{"a pending export", func(f fixture, sid uuid.UUID) []stmt { return []stmt{exportRow(f, sid, nil)} }, ""},
		{"a delivered one", func(f fixture, sid uuid.UUID) []stmt { return []stmt{exportRow(f, sid, delivered(h))} }, ""},
		{"a failed one", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, map[string]any{"status": "failed", "attempts": 6, "next_attempt_at": nil})}
		}, ""},
		{"a format", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, map[string]any{"format": "xlsx"})}
		}, "export_format_check"},
		{"a status", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, map[string]any{"status": "sent", "next_attempt_at": nil})}
		}, "export_status_check"},
		{"seven attempts", func(f fixture, sid uuid.UUID) []stmt { return []stmt{exportRow(f, sid, map[string]any{"attempts": 7})} }, "export_attempts_check"},
		{"negative attempts", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, map[string]any{"attempts": -1})}
		}, "export_attempts_check"},
		{"failed with a delivery time", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, map[string]any{"status": "failed", "next_attempt_at": nil, "delivered_at": "2026-04-17T13:05:09Z"})}
		}, "export_delivered"},
		{"failed as a resend", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, map[string]any{"status": "failed", "next_attempt_at": nil, "resend": false})}
		}, "export_delivered"},
		{"pending with no time", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, map[string]any{"next_attempt_at": nil})}
		}, "export_pending"},
		{"failed with a time", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, map[string]any{"status": "failed"})}
		}, "export_pending"},
		{"delivered without a time", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, with(delivered(h), map[string]any{"delivered_at": nil}))}
		}, "export_delivered"},
		{"delivered without resend", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, with(delivered(h), map[string]any{"resend": nil}))}
		}, "export_delivered"},
		{"delivered without a file", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, with(delivered(h), map[string]any{"file_name": nil, "sha256": nil}))}
		}, "export_delivered"},
		{"a resend not delivered", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, map[string]any{"resend": true})}
		}, "export_delivered"},
		{"a name without a hash", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, with(delivered(h), map[string]any{"sha256": nil}))}
		}, "export_file"},
		{"a hash of 31 bytes", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, with(delivered(h), map[string]any{"sha256": make([]byte, 31)}))}
		}, "export_file"},
		{"a name of another format", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, with(delivered(h), map[string]any{"format": "jsonl"}))}
		}, "export_file"},
		{"a name without the company", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, with(delivered(h), map[string]any{"file_name": "cash-2026-04-17-" + h + ".csv"}))}
		}, "export_file"},
		{"another company's snapshot", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, map[string]any{"legal_entity_id": id()})}
		}, "export_snapshot"},
		{"a treasury_export connector", func(f fixture, _ uuid.UUID) []stmt {
			return []stmt{step(`INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
				VALUES ($1, $2, 'treasury_export', 'Treasury', '{}', 'active', $3, $3)`, f.tenant, id(), f.actor)}
		}, ""},
		{"a connector of no kind", func(f fixture, _ uuid.UUID) []stmt {
			return []stmt{step(`INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
				VALUES ($1, $2, 'treasury', 'Treasury', '{}', 'active', $3, $3)`, f.tenant, id(), f.actor)}
		}, "connector_kind_check"},
		{"an export changed", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, nil), step(`UPDATE cash.export SET attempts = 1, version = version + 1 WHERE snapshot_id = $1`, sid)}
		}, ""},
		{"an export deleted", func(f fixture, sid uuid.UUID) []stmt {
			return []stmt{exportRow(f, sid, nil), step(`DELETE FROM cash.export WHERE snapshot_id = $1`, sid)}
		}, "permission denied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			sid := id()
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return all(tx, append([]stmt{memoItems(f), snapshotRow(f, sid, nil)}, c.run(f, sid)...)...)
			})
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

// TestCashExportPrivileges: both permissions are seeded and are no
// approvals; the application may insert, read and change exports, never
// delete them; the worker reads only what finds the tenants with exports
// due; an export is deleted with its snapshot by retention (T53h, 000196).
func TestCashExportPrivileges(t *testing.T) {
	need(t)
	for _, code := range []string{"cash.export.read", "cash.export.send"} {
		var approval bool
		if err := testDB.QueryRow(context.Background(), `SELECT is_approval FROM ref.permission WHERE code = $1`, code).Scan(&approval); err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		if approval {
			t.Errorf("%s: got an approval permission, want none", code)
		}
	}
	for _, c := range []struct {
		check string
		want  bool
	}{
		{`has_table_privilege('erp_app', 'cash.export', 'INSERT')`, true},
		{`has_table_privilege('erp_app', 'cash.export', 'UPDATE')`, true},
		{`has_table_privilege('erp_app', 'cash.export', 'DELETE')`, false},
		{`has_column_privilege('erp_worker', 'cash.export', 'next_attempt_at', 'SELECT')`, true},
		{`has_column_privilege('erp_worker', 'cash.export', 'file_name', 'SELECT')`, false},
		{`(SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid = 'cash.export'::regclass)`, true},
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
	f := newFixture(t)
	sid := id()
	var left int
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		err := all(tx, memoItems(f), snapshotRow(f, sid, map[string]any{"snapshot_date": "2024-02-01", "horizon_end": "2024-12-31"}), exportRow(f, sid, nil))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(context.Background(), `SELECT cash.delete_expired_snapshots($1, '2026-04-17')`, f.entity); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM cash.export WHERE snapshot_id = $1`, sid).Scan(&left)
	})
	if left != 0 {
		t.Errorf("exports of a snapshot past retention: got %d left, want them deleted with it", left)
	}
}
