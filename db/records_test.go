package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestRecordsPermissions (T99a, migration 000316; ADR-0079 §4; T99b,
// migration 000317, §8; T100a, migration 000318, §10): the permissions
// to read the retention status, to run archiving and to read the personal
// data inventory are seeded and are no approval permissions.
func TestRecordsPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'records.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "records.archive.run records.privacy.delete records.privacy.read records.retention.read"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestArchivingRunRules (T99b, migration 000317; ADR-0079 §6-§7): a run
// is of journal entries, a fiscal year and a rule's years in range, at a
// configuration version; one queued or completed run per company, kind
// and year, a failed one leaving room for another; a completed run names
// its file and manifest with their figures, a failed one its code, and
// neither is changed again; what a run is for and who asked never
// change, and no run is deleted.
func TestArchivingRunRules(t *testing.T) {
	need(t)
	const run = `INSERT INTO records.archiving_run (tenant_id, id, legal_entity_id, document_kind, fiscal_year, retention_years, config_version, requested_by)
		VALUES ($1, $2, $1, $3, $4, $5, $6, $1)`
	const complete = `UPDATE records.archiving_run SET status = 'completed', object_key = 'k', manifest_key = 'm', sha256 = sha256('f'), manifest_sha256 = sha256('m'),
		size_bytes = 1, entry_count = 1, line_count = 2, debit_total = 100, currency = 'EUR', completed_at = now(), version = version + 1 WHERE id = $1`
	const fail = `UPDATE records.archiving_run SET status = 'failed', failure_code = 'records.year_not_closed', version = version + 1 WHERE id = $1`
	type scene struct{ tenant, run uuid.UUID }
	queue := func(s scene) stmt { return step(run, s.tenant, s.run, "journal_entry", 2025, 0, 1) }
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a run", func(s scene) []stmt { return []stmt{queue(s)} }, ""},
		{"of personal data", func(s scene) []stmt { return []stmt{step(run, s.tenant, s.run, "personal_data", 2025, 0, 1)} }, "archiving_run_kind_check"},
		{"of year 1899", func(s scene) []stmt { return []stmt{step(run, s.tenant, s.run, "journal_entry", 1899, 0, 1)} }, "archiving_run_year_check"},
		{"of 100 years", func(s scene) []stmt { return []stmt{step(run, s.tenant, s.run, "journal_entry", 2025, 100, 1)} }, "archiving_run_retention_check"},
		{"at configuration version 0", func(s scene) []stmt { return []stmt{step(run, s.tenant, s.run, "journal_entry", 2025, 0, 0)} }, "archiving_run_config_check"},
		{"a second queued run of the year", func(s scene) []stmt { return []stmt{queue(s), step(run, s.tenant, id(), "journal_entry", 2025, 0, 1)} }, "archiving_run_once"},
		{"a second run after a completed one", func(s scene) []stmt {
			return []stmt{queue(s), step(complete, s.run), step(run, s.tenant, id(), "journal_entry", 2025, 0, 1)}
		}, "archiving_run_once"},
		{"a run after a failed one", func(s scene) []stmt {
			return []stmt{queue(s), step(fail, s.run), step(run, s.tenant, id(), "journal_entry", 2025, 0, 1)}
		}, ""},
		{"a run of another year", func(s scene) []stmt { return []stmt{queue(s), step(run, s.tenant, id(), "journal_entry", 2024, 0, 1)} }, ""},
		{"completed", func(s scene) []stmt { return []stmt{queue(s), step(complete, s.run)} }, ""},
		{"completed without its manifest", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.archiving_run SET status = 'completed', object_key = 'k', sha256 = sha256('f'), manifest_sha256 = sha256('m'),
				size_bytes = 1, entry_count = 1, line_count = 2, debit_total = 100, currency = 'EUR', completed_at = now(), version = version + 1 WHERE id = $1`, s.run)}
		}, "archiving_run_completed_check"},
		{"completed without its figures", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.archiving_run SET status = 'completed', object_key = 'k', manifest_key = 'm', completed_at = now(), version = version + 1 WHERE id = $1`, s.run)}
		}, "archiving_run_completed_check"},
		{"a queued run with a file", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.archiving_run SET object_key = 'k', manifest_key = 'm', sha256 = sha256('f'), manifest_sha256 = sha256('m'),
				size_bytes = 1, entry_count = 1, line_count = 2, debit_total = 100, currency = 'EUR', version = version + 1 WHERE id = $1`, s.run)}
		}, "archiving_run_completed_check"},
		{"a hash of 16 bytes", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.archiving_run SET status = 'completed', object_key = 'k', manifest_key = 'm', sha256 = '\x00112233445566778899aabbccddeeff'::bytea,
				manifest_sha256 = sha256('m'), size_bytes = 1, entry_count = 1, line_count = 2, debit_total = 100, currency = 'EUR', completed_at = now(), version = version + 1 WHERE id = $1`, s.run)}
		}, "archiving_run_file_check"},
		{"a manifest hash of 16 bytes", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.archiving_run SET status = 'completed', object_key = 'k', manifest_key = 'm', sha256 = sha256('f'),
				manifest_sha256 = '\x00112233445566778899aabbccddeeff'::bytea, size_bytes = 1, entry_count = 1, line_count = 2, debit_total = 100, currency = 'EUR',
				completed_at = now(), version = version + 1 WHERE id = $1`, s.run)}
		}, "archiving_run_file_check"},
		{"a negative debit total", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.archiving_run SET status = 'completed', object_key = 'k', manifest_key = 'm', sha256 = sha256('f'),
				manifest_sha256 = sha256('m'), size_bytes = 1, entry_count = 1, line_count = 2, debit_total = -1, currency = 'EUR', completed_at = now(), version = version + 1 WHERE id = $1`, s.run)}
		}, "archiving_run_file_check"},
		{"failed", func(s scene) []stmt { return []stmt{queue(s), step(fail, s.run)} }, ""},
		{"failed without a code", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.archiving_run SET status = 'failed', version = version + 1 WHERE id = $1`, s.run)}
		}, "archiving_run_failed_check"},
		{"an unknown status", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.archiving_run SET status = 'running', version = version + 1 WHERE id = $1`, s.run)}
		}, "archiving_run_status_check"},
		{"version 0", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO records.archiving_run (tenant_id, id, legal_entity_id, document_kind, fiscal_year, retention_years, config_version, requested_by,
				version) VALUES ($1, $2, $1, 'journal_entry', 2025, 0, 1, $1, 0)`, s.tenant, s.run)}
		}, "archiving_run_version_check"},
		{"a completed run failed", func(s scene) []stmt { return []stmt{queue(s), step(complete, s.run), step(fail, s.run)} }, "archiving_run_settled_fixed"},
		{"a failed run completed", func(s scene) []stmt { return []stmt{queue(s), step(fail, s.run), step(complete, s.run)} }, "archiving_run_settled_fixed"},
		{"a completed run queued again, by the owner", func(s scene) []stmt {
			return []stmt{queue(s), step(complete, s.run), step(`SET LOCAL ROLE erp_owner`), step(`UPDATE records.archiving_run SET status = 'queued', object_key = NULL,
				manifest_key = NULL, sha256 = NULL, manifest_sha256 = NULL, size_bytes = NULL, entry_count = NULL, line_count = NULL, debit_total = NULL, currency = NULL,
				completed_at = NULL WHERE id = $1`, s.run)}
		}, "archiving_run_settled_fixed"},
		{"the year changed", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.archiving_run SET fiscal_year = 2024 WHERE id = $1`, s.run)}
		}, "permission denied"},
		{"the requester changed", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.archiving_run SET requested_by = gen_random_uuid() WHERE id = $1`, s.run)}
		}, "permission denied"},
		{"a run deleted", func(s scene) []stmt { return []stmt{queue(s), step(`DELETE FROM records.archiving_run`)} }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), run: id()}
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

// TestArchivingRunTouched (T99b, migration 000317): settling a run moves
// its updated_at, which the application cannot set itself.
func TestArchivingRunTouched(t *testing.T) {
	need(t)
	tenant, run := id(), id()
	var moved bool
	err := asApp(t, tenant, func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx, `INSERT INTO records.archiving_run (tenant_id, id, legal_entity_id, document_kind, fiscal_year, retention_years, config_version,
				requested_by, created_at, updated_at)
			VALUES ($1, $2, $1, 'journal_entry', 2025, 0, 1, $1, '2026-01-01', '2026-01-01')`, tenant, run); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `UPDATE records.archiving_run SET status = 'failed', failure_code = 'x', version = version + 1 WHERE id = $1
			RETURNING updated_at > '2026-01-01' AND created_at = '2026-01-01'`, run).Scan(&moved)
	})
	if err != nil || !moved {
		t.Errorf("got %v and moved %v, want the change to move updated_at alone", err, moved)
	}
}
