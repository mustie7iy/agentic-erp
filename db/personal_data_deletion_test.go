package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestPersonalDataDeleted (T100c, migration 000337; ADR-0079 §12): a
// partner's personal data is deleted only while it is blocked, and once
// deleted neither the deletion, the block, the status nor the names
// change again, for the owner too; an address and a bank account change
// their personal columns only to the blanking markers, which passes the
// validity trigger while nothing else changes, and any other change is
// held to it as before.
func TestPersonalDataDeleted(t *testing.T) {
	need(t)
	const (
		block   = `UPDATE partner.business_partner SET status = 'blocked', personal_data_blocked_at = now(), version = version + 1 WHERE id = $1`
		deleted = `UPDATE partner.business_partner SET name = '[deleted]', personal_data_deleted_at = now(), version = version + 1 WHERE id = $1`
		address = `INSERT INTO partner.address (tenant_id, id, partner_id, purpose, line1, line2, city, country_code, valid_from, created_by)
			VALUES ($1, $2, $3, 'legal', 'Street 1', NULL, 'Town', 'US', current_date - 10, $1)`
		bank = `INSERT INTO partner.bank_account (tenant_id, id, partner_id, account_cipher, account_fingerprint, account_last4, account_scheme, bic,
			bank_country_code, holder_name, valid_from, partner_change_id, created_by) VALUES ($1, $2, $3, '\x01', repeat('a', 64), '1234', 'iban', 'COBADEFFXXX',
			'DE', 'Holder', current_date - 10, gen_random_uuid(), $1)`
		blankAddress = `UPDATE partner.address SET line1 = '[deleted]', city = '[deleted]' WHERE id = $1`
		blankBank    = `UPDATE partner.bank_account SET holder_name = '[deleted]', account_last4 = 'XXXX', bic = NULL, account_cipher = '\x02' WHERE id = $1`
	)
	f := newFixture(t) // its tenant; each case has a partner of its own
	type scene struct{ tenant, partner, row uuid.UUID }
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string
	}{
		{"deleted while blocked", func(s scene) []stmt { return []stmt{step(block, s.partner), step(deleted, s.partner)} }, ""},
		{"deleted unblocked", func(s scene) []stmt { return []stmt{step(deleted, s.partner)} }, "business_partner_personal_data_deleted_check"},
		{"deleted, then unblocked", func(s scene) []stmt {
			return []stmt{step(block, s.partner), step(deleted, s.partner),
				step(`UPDATE partner.business_partner SET personal_data_blocked_at = NULL, version = version + 1 WHERE id = $1`, s.partner)}
		}, "business_partner_personal_data_deleted_fixed"},
		{"deleted, then renamed, by the owner", func(s scene) []stmt {
			return []stmt{step(block, s.partner), step(deleted, s.partner), step(`SET LOCAL ROLE erp_owner`),
				step(`UPDATE partner.business_partner SET name = 'Back', version = version + 1 WHERE id = $1`, s.partner)}
		}, "business_partner_personal_data_deleted_fixed"},
		{"deleted, then deleted again", func(s scene) []stmt {
			return []stmt{step(block, s.partner), step(deleted, s.partner),
				step(`UPDATE partner.business_partner SET personal_data_deleted_at = now() + interval '1 day', version = version + 1 WHERE id = $1`, s.partner)}
		}, "business_partner_personal_data_deleted_fixed"},
		{"deleted, then given a legal name", func(s scene) []stmt {
			return []stmt{step(block, s.partner), step(deleted, s.partner),
				step(`UPDATE partner.business_partner SET legal_name = 'Ann Ltd', version = version + 1 WHERE id = $1`, s.partner)}
		}, "business_partner_personal_data_deleted_fixed"},
		{"deleted, then activated", func(s scene) []stmt {
			return []stmt{step(block, s.partner), step(deleted, s.partner),
				step(`UPDATE partner.business_partner SET status = 'active', version = version + 1 WHERE id = $1`, s.partner)}
		}, "business_partner_personal_data_deleted_fixed"},
		{"an address blanked", func(s scene) []stmt {
			return []stmt{step(address, s.tenant, s.row, s.partner), step(block, s.partner), step(deleted, s.partner), step(blankAddress, s.row)}
		}, ""},
		{"an active partner's address blanked", func(s scene) []stmt {
			return []stmt{step(address, s.tenant, s.row, s.partner), step(blankAddress, s.row)}
		}, "personal_data_not_deleted"},
		{"a blocked partner's address blanked", func(s scene) []stmt {
			return []stmt{step(address, s.tenant, s.row, s.partner), step(block, s.partner), step(blankAddress, s.row)}
		}, "personal_data_not_deleted"},
		{"an address renamed", func(s scene) []stmt {
			return []stmt{step(address, s.tenant, s.row, s.partner), step(block, s.partner), step(deleted, s.partner), step(`UPDATE partner.address SET line1 = 'Other', valid_to = current_date, ended_by = $2 WHERE id = $1`, s.row, s.tenant)}
		}, "address_personal_fixed"},
		{"an address blanked with a line it had not", func(s scene) []stmt {
			return []stmt{step(address, s.tenant, s.row, s.partner), step(block, s.partner), step(deleted, s.partner), step(`UPDATE partner.address SET line1 = '[deleted]', city = '[deleted]', line2 = '[deleted]', valid_to = current_date, ended_by = $2 WHERE id = $1`, s.row, s.tenant)}
		}, "address_personal_fixed"},
		{"an address blanked and moved", func(s scene) []stmt {
			return []stmt{step(address, s.tenant, s.row, s.partner), step(`UPDATE partner.address SET line1 = '[deleted]', city = '[deleted]', valid_to = current_date - 1,
				ended_by = $2 WHERE id = $1`, s.row, s.tenant)}
		}, "validity cannot be ended in the past"},
		{"a bank account blanked", func(s scene) []stmt {
			return []stmt{step(bank, s.tenant, s.row, s.partner), step(block, s.partner), step(deleted, s.partner), step(blankBank, s.row)}
		}, ""},
		{"a blocked partner's bank account blanked", func(s scene) []stmt {
			return []stmt{step(bank, s.tenant, s.row, s.partner), step(block, s.partner), step(blankBank, s.row)}
		}, "personal_data_not_deleted"},
		{"a bank account blanked and ended", func(s scene) []stmt {
			return []stmt{step(bank, s.tenant, s.row, s.partner), step(block, s.partner), step(deleted, s.partner), step(`UPDATE partner.bank_account SET holder_name = '[deleted]', account_last4 = 'XXXX', bic = NULL,
				valid_to = current_date, ended_by = $2 WHERE id = $1`, s.row, s.tenant)}
		}, ""},
		{"a bank account's holder changed", func(s scene) []stmt {
			return []stmt{step(bank, s.tenant, s.row, s.partner), step(block, s.partner), step(deleted, s.partner), step(`UPDATE partner.bank_account SET holder_name = 'Other', valid_to = current_date, ended_by = $2
				WHERE id = $1`, s.row, s.tenant)}
		}, "bank_account_personal_fixed"},
		{"a bank account blanked keeping its BIC", func(s scene) []stmt {
			return []stmt{step(bank, s.tenant, s.row, s.partner), step(block, s.partner), step(deleted, s.partner), step(`UPDATE partner.bank_account SET holder_name = '[deleted]', account_last4 = 'XXXX', valid_to = current_date, ended_by = $2 WHERE id = $1`, s.row, s.tenant)}
		}, "bank_account_personal_fixed"},
		{"a bank account's purpose changed", func(s scene) []stmt {
			return []stmt{step(bank, s.tenant, s.row, s.partner), step(`SET LOCAL ROLE erp_owner`), step(`UPDATE partner.bank_account SET purpose = 'collections' WHERE id = $1`, s.row)}
		}, "validity can only be ended or shortened"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: f.tenant, partner: id(), row: id()}
			number := strings.ToUpper(strings.ReplaceAll(s.partner.String(), "-", "")[:20])
			err := asApp(t, s.tenant, func(tx pgx.Tx) error {
				return all(tx, append([]stmt{step(`INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
					VALUES ($1, $2, $3, 'person', 'Ann', 'US', 'active', $1, $1)`, s.tenant, s.partner, number)}, c.run(s)...)...)
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

// TestDeletionRunRules (T100c, migration 000337; ADR-0079 §12): a run has
// at least one subject and a configuration version; one queued run per
// tenant; a completed run has its time and a failed one its code, and
// neither changes again, nor does a settled subject, its skip reason
// among them (one of four, on a skipped subject only); what a run and a
// subject are for never changes, and neither is deleted.
func TestDeletionRunRules(t *testing.T) {
	need(t)
	const (
		run      = `INSERT INTO records.deletion_run (tenant_id, id, config_version, subject_count, requested_by) VALUES ($1, $2, $3, $4, $1)`
		subject  = `INSERT INTO records.deletion_run_subject (tenant_id, run_id, partner_id, partner_number, blocked_at, retention_years, retention_ends) VALUES ($1, $2, $3, 'BP1', now(), $4, current_date)`
		complete = `UPDATE records.deletion_run SET status = 'completed', completed_at = now(), version = version + 1 WHERE id = $1`
		fail     = `UPDATE records.deletion_run SET status = 'failed', failure_code = 'identity.principal_blocked', version = version + 1 WHERE id = $1`
		blanked  = `UPDATE records.deletion_run_subject SET status = 'blanked', settled_at = now() WHERE run_id = $1`
	)
	type scene struct{ tenant, run uuid.UUID }
	queue := func(s scene) stmt { return step(run, s.tenant, s.run, 1, 1) }
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string
	}{
		{"a run with a subject", func(s scene) []stmt { return []stmt{queue(s), step(subject, s.tenant, s.run, id(), 0)} }, ""},
		{"no subject", func(s scene) []stmt { return []stmt{step(run, s.tenant, s.run, 1, 0)} }, "deletion_run_count_check"},
		{"configuration version 0", func(s scene) []stmt { return []stmt{step(run, s.tenant, s.run, 0, 1)} }, "deletion_run_config_check"},
		{"a second queued run", func(s scene) []stmt { return []stmt{queue(s), step(run, s.tenant, id(), 1, 1)} }, "deletion_run_one_queued"},
		{"a run after a completed one", func(s scene) []stmt { return []stmt{queue(s), step(complete, s.run), step(run, s.tenant, id(), 1, 1)} }, ""},
		{"completed without its time", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.deletion_run SET status = 'completed', version = version + 1 WHERE id = $1`, s.run)}
		}, "deletion_run_completed_check"},
		{"failed without a code", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.deletion_run SET status = 'failed', version = version + 1 WHERE id = $1`, s.run)}
		}, "deletion_run_failed_check"},
		{"an unknown status", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.deletion_run SET status = 'running', version = version + 1 WHERE id = $1`, s.run)}
		}, "deletion_run_status_check"},
		{"version 0", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO records.deletion_run (tenant_id, id, config_version, subject_count, requested_by, version) VALUES ($1, $2, 1, 1, $1, 0)`, s.tenant, s.run)}
		}, "deletion_run_version_check"},
		{"a completed run failed, by the owner", func(s scene) []stmt {
			return []stmt{queue(s), step(complete, s.run), step(`SET LOCAL ROLE erp_owner`), step(fail, s.run)}
		}, "deletion_run_settled_fixed"},
		{"the requester changed", func(s scene) []stmt {
			return []stmt{queue(s), step(`UPDATE records.deletion_run SET requested_by = gen_random_uuid() WHERE id = $1`, s.run)}
		}, "permission denied"},
		{"a run deleted", func(s scene) []stmt { return []stmt{queue(s), step(`DELETE FROM records.deletion_run`)} }, "permission denied"},
		{"100 years", func(s scene) []stmt { return []stmt{queue(s), step(subject, s.tenant, s.run, id(), 100)} }, "deletion_run_subject_retention_check"},
		{"a subject of no run", func(s scene) []stmt { return []stmt{step(subject, s.tenant, s.run, id(), 0)} }, "deletion_run_subject_tenant_id_run_id_fkey"},
		{"a subject blanked", func(s scene) []stmt {
			return []stmt{queue(s), step(subject, s.tenant, s.run, id(), 0), step(blanked, s.run)}
		}, ""},
		{"a subject settled without its time", func(s scene) []stmt {
			return []stmt{queue(s), step(subject, s.tenant, s.run, id(), 0), step(`UPDATE records.deletion_run_subject SET status = 'skipped' WHERE run_id = $1`, s.run)}
		}, "deletion_run_subject_settled_check"},
		{"an unknown subject status", func(s scene) []stmt {
			return []stmt{queue(s), step(subject, s.tenant, s.run, id(), 0), step(`UPDATE records.deletion_run_subject SET status = 'erased', settled_at = now() WHERE run_id = $1`, s.run)}
		}, "deletion_run_subject_status_check"},
		{"a blanked subject skipped, by the owner", func(s scene) []stmt {
			return []stmt{queue(s), step(subject, s.tenant, s.run, id(), 0), step(blanked, s.run), step(`SET LOCAL ROLE erp_owner`),
				step(`UPDATE records.deletion_run_subject SET status = 'skipped' WHERE run_id = $1`, s.run)}
		}, "deletion_run_subject_settled_fixed"},
		{"a subject skipped with its reason", func(s scene) []stmt {
			return []stmt{queue(s), step(subject, s.tenant, s.run, id(), 0),
				step(`UPDATE records.deletion_run_subject SET status = 'skipped', skip_reason = 'open_item', settled_at = now() WHERE run_id = $1`, s.run)}
		}, ""},
		{"an unknown skip reason", func(s scene) []stmt {
			return []stmt{queue(s), step(subject, s.tenant, s.run, id(), 0),
				step(`UPDATE records.deletion_run_subject SET status = 'skipped', skip_reason = 'tired', settled_at = now() WHERE run_id = $1`, s.run)}
		}, "deletion_run_subject_skip_reason_check"},
		{"a blanked subject with a skip reason", func(s scene) []stmt {
			return []stmt{queue(s), step(subject, s.tenant, s.run, id(), 0),
				step(`UPDATE records.deletion_run_subject SET status = 'blanked', skip_reason = 'open_item', settled_at = now() WHERE run_id = $1`, s.run)}
		}, "deletion_run_subject_skip_reason_skipped_check"},
		{"a skipped subject's reason changed, by the owner", func(s scene) []stmt {
			return []stmt{queue(s), step(subject, s.tenant, s.run, id(), 0),
				step(`UPDATE records.deletion_run_subject SET status = 'skipped', skip_reason = 'open_item', settled_at = now() WHERE run_id = $1`, s.run),
				step(`SET LOCAL ROLE erp_owner`), step(`UPDATE records.deletion_run_subject SET skip_reason = 'block_lifted' WHERE run_id = $1`, s.run)}
		}, "deletion_run_subject_settled_fixed"},
		{"a subject's partner changed", func(s scene) []stmt {
			return []stmt{queue(s), step(subject, s.tenant, s.run, id(), 0), step(`UPDATE records.deletion_run_subject SET partner_id = gen_random_uuid() WHERE run_id = $1`, s.run)}
		}, "permission denied"},
		{"a subject deleted", func(s scene) []stmt {
			return []stmt{queue(s), step(subject, s.tenant, s.run, id(), 0), step(`DELETE FROM records.deletion_run_subject`)}
		}, "permission denied"},
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
