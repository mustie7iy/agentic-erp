package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestEvidenceExportRules (T101a, migration 000338; ADR-0011 As built):
// an export covers one to 100 companies, none null, for a period of one
// day to 371; a completed export names its manifest, its files and their
// figures and the chain's verification, a failed one its code, and
// neither is changed again; a chain that did not verify names where it
// broke; what an export covers and who asked never change, and no export
// is deleted; the permission is seeded and is no approval permission.
func TestEvidenceExportRules(t *testing.T) {
	need(t)
	const export = `INSERT INTO audit.evidence_export (tenant_id, id, legal_entity_ids, period_from, period_to, with_client_addresses, requested_by)
		VALUES ($1, $2, $3, $4::date, $5::date, true, $1)`
	const complete = `UPDATE audit.evidence_export SET status = 'completed', manifest_key = 'k', manifest_sha256 = sha256('m'), files = '[]', audit_event_count = 1,
		journal_entry_count = 2, approval_decision_count = 0, chain_verified = true, chain_last_seq = 9, chain_checked = 9, chain_anchors_checked = 1,
		completed_at = now(), version = version + 1 WHERE id = $1`
	const fail = `UPDATE audit.evidence_export SET status = 'failed', failure_code = 'identity.principal_blocked', version = version + 1 WHERE id = $1`
	type scene struct{ tenant, export uuid.UUID }
	queue := func(s scene) stmt {
		return step(export, s.tenant, s.export, []uuid.UUID{s.tenant}, "2026-11-01", "2026-11-30")
	}
	completed := [][2]string{{"status", "'completed'"}, {"manifest_key", "'k'"}, {"manifest_sha256", "sha256('m')"}, {"files", "'[]'"},
		{"audit_event_count", "1"}, {"journal_entry_count", "2"}, {"approval_decision_count", "0"}, {"chain_verified", "true"}, {"chain_last_seq", "9"},
		{"chain_checked", "9"}, {"chain_anchors_checked", "1"}, {"completed_at", "now()"}}
	// completeWith completes the export with some of its columns set
	// otherwise ("column = value", comma-separated).
	completeWith := func(set string) func(s scene) []stmt {
		over := map[string]string{}
		for _, a := range strings.Split(set, ", ") {
			col, val, _ := strings.Cut(a, " = ")
			over[col] = val
		}
		var sets []string
		for _, c := range completed {
			if v, ok := over[c[0]]; ok {
				c[1] = v
				delete(over, c[0])
			}
			sets = append(sets, c[0]+" = "+c[1])
		}
		for col, val := range over {
			sets = append(sets, col+" = "+val)
		}
		return func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE audit.evidence_export SET `+strings.Join(sets, ", ")+`, version = version + 1 WHERE id = $1`, s.export)}
		}
	}
	many := make([]uuid.UUID, 101)
	for i := range many {
		many[i] = uuid.New()
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"an export", func(s scene) []stmt { return []stmt{queue(s)} }, ""},
		{"of no company", func(s scene) []stmt {
			return []stmt{step(export, s.tenant, s.export, []uuid.UUID{}, "2026-11-01", "2026-11-30")}
		}, "evidence_export_companies_check"},
		{"of 101 companies", func(s scene) []stmt {
			return []stmt{step(export, s.tenant, s.export, many, "2026-11-01", "2026-11-30")}
		},
			"evidence_export_companies_check"},
		{"of a null company", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO audit.evidence_export (tenant_id, id, legal_entity_ids, period_from, period_to, with_client_addresses, requested_by)
				VALUES ($1, $2, ARRAY[$1, NULL]::uuid[], '2026-11-01', '2026-11-30', true, $1)`, s.tenant, s.export)}
		}, "evidence_export_companies_check"},
		{"of one day", func(s scene) []stmt {
			return []stmt{step(export, s.tenant, s.export, []uuid.UUID{s.tenant}, "2026-11-01", "2026-11-01")}
		}, ""},
		{"of 371 days", func(s scene) []stmt {
			return []stmt{step(export, s.tenant, s.export, []uuid.UUID{s.tenant}, "2026-01-01", "2027-01-06")}
		}, ""},
		{"of 372 days", func(s scene) []stmt {
			return []stmt{step(export, s.tenant, s.export, []uuid.UUID{s.tenant}, "2026-01-01", "2027-01-07")}
		},
			"evidence_export_period_check"},
		{"ending before it starts", func(s scene) []stmt {
			return []stmt{step(export, s.tenant, s.export, []uuid.UUID{s.tenant}, "2026-11-30", "2026-11-01")}
		}, "evidence_export_period_check"},
		{"completed", func(s scene) []stmt { return []stmt{queue(s), step(complete, s.export)} }, ""},
		{"completed with a broken chain", completeWith("chain_verified = false, chain_broken_seq = 4"), ""},
		{"a broken chain without where", completeWith("chain_verified = false"), "evidence_export_chain_check"},
		{"a verified chain broken somewhere", completeWith("chain_broken_seq = 4"), "evidence_export_chain_check"},
		{"broken at 0", completeWith("chain_verified = false, chain_broken_seq = 0"), "evidence_export_chain_check"},
		{"completed without its manifest's hash", completeWith("manifest_sha256 = NULL"), "evidence_export_completed_check"},
		{"completed without its files", completeWith("files = NULL"), "evidence_export_completed_check"},
		{"completed without its counts", completeWith("journal_entry_count = NULL"), "evidence_export_completed_check"},
		{"completed without the chain's checks", completeWith("chain_anchors_checked = NULL"), "evidence_export_completed_check"},
		{"completed without when", completeWith("completed_at = NULL"), "evidence_export_completed_check"},
		{"a hash of 16 bytes", completeWith(`manifest_sha256 = '\x00112233445566778899aabbccddeeff'::bytea`), "evidence_export_figures_check"},
		{"files not a list", completeWith(`files = '{}'`), "evidence_export_figures_check"},
		{"a negative count", completeWith("audit_event_count = -1"), "evidence_export_figures_check"},
		{"a negative head", completeWith("chain_last_seq = -1"), "evidence_export_figures_check"},
		{"a negative number checked", completeWith("chain_checked = -1"), "evidence_export_figures_check"},
		{"a queued export with a manifest", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE audit.evidence_export SET manifest_key = 'k', manifest_sha256 = sha256('m'), files = '[]', audit_event_count = 1,
				journal_entry_count = 2, approval_decision_count = 0, chain_verified = true, chain_last_seq = 9, chain_checked = 9, chain_anchors_checked = 1,
				version = version + 1 WHERE id = $1`, s.export)}
		}, "evidence_export_completed_check"},
		{"failed", func(s scene) []stmt { return []stmt{queue(s), step(fail, s.export)} }, ""},
		{"failed without a code", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE audit.evidence_export SET status = 'failed', version = version + 1 WHERE id = $1`, s.export)}
		}, "evidence_export_failed_check"},
		{"an unknown status", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE audit.evidence_export SET status = 'running', version = version + 1 WHERE id = $1`, s.export)}
		}, "evidence_export_status_check"},
		{"version 0", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO audit.evidence_export (tenant_id, id, legal_entity_ids, period_from, period_to, with_client_addresses, requested_by, version)
				VALUES ($1, $2, ARRAY[$1::uuid], '2026-11-01', '2026-11-30', true, $1, 0)`, s.tenant, s.export)}
		}, "evidence_export_version_check"},
		{"a completed export failed", func(s scene) []stmt { return []stmt{queue(s), step(complete, s.export), step(fail, s.export)} }, "evidence_export_settled_fixed"},
		{"a failed export completed", func(s scene) []stmt { return []stmt{queue(s), step(fail, s.export), step(complete, s.export)} }, "evidence_export_settled_fixed"},
		{"a completed export queued again, by the owner", func(s scene) []stmt {
			return []stmt{queue(s), step(complete, s.export), step(`SET LOCAL ROLE erp_owner`), step(`UPDATE audit.evidence_export SET status = 'queued',
				manifest_key = NULL, manifest_sha256 = NULL, files = NULL, audit_event_count = NULL, journal_entry_count = NULL, approval_decision_count = NULL,
				chain_verified = NULL, chain_last_seq = NULL, chain_checked = NULL, chain_anchors_checked = NULL, completed_at = NULL WHERE id = $1`, s.export)}
		}, "evidence_export_settled_fixed"},
		{"the companies changed", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE audit.evidence_export SET legal_entity_ids = ARRAY[gen_random_uuid()] WHERE id = $1`, s.export)}
		}, "permission denied"},
		{"the addresses changed", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE audit.evidence_export SET with_client_addresses = false WHERE id = $1`, s.export)}
		}, "permission denied"},
		{"the requester changed", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE audit.evidence_export SET requested_by = gen_random_uuid() WHERE id = $1`, s.export)}
		}, "permission denied"},
		{"an export deleted", func(s scene) []stmt { return []stmt{queue(s), step(`DELETE FROM audit.evidence_export`)} }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), export: id()}
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
	var approval bool
	if err := testDB.QueryRow(context.Background(), `SELECT is_approval FROM ref.permission WHERE code = 'audit.evidence.export'`).Scan(&approval); err != nil || approval {
		t.Errorf("the permission: got approval %v, %v, want it seeded and no approval permission", approval, err)
	}
}

// TestEvidenceExportTouched (T101a, migration 000338): settling an export
// moves its updated_at, which the application cannot set itself; the
// worker role reads the tenants with queued exports and nothing more.
func TestEvidenceExportTouched(t *testing.T) {
	need(t)
	tenant, export := id(), id()
	var moved bool
	err := asApp(t, tenant, func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx, `INSERT INTO audit.evidence_export (tenant_id, id, legal_entity_ids, period_from, period_to, with_client_addresses, requested_by,
				created_at, updated_at)
			VALUES ($1, $2, ARRAY[$1::uuid], '2026-11-01', '2026-11-30', false, $1, '2026-01-01', '2026-01-01')`, tenant, export); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `UPDATE audit.evidence_export SET status = 'failed', failure_code = 'x', version = version + 1 WHERE id = $1
			RETURNING updated_at > '2026-01-01' AND created_at = '2026-01-01'`, export).Scan(&moved)
	})
	if err != nil || !moved {
		t.Errorf("got %v and moved %v, want the change to move updated_at alone", err, moved)
	}
	var grants string
	if err := testDB.QueryRow(context.Background(), `SELECT string_agg(column_name, ' ' ORDER BY column_name) FROM information_schema.column_privileges
		WHERE grantee = 'erp_worker' AND table_schema = 'audit' AND table_name = 'evidence_export' AND privilege_type = 'SELECT'`).Scan(&grants); err != nil ||
		grants != "status tenant_id" {
		t.Errorf("the worker's columns: got %q, %v, want status and tenant_id", grants, err)
	}
}
