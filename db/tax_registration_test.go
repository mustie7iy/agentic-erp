package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The tax filing context's registrations (T61g, migration 000172): a code
// once in the tenant, the authority, return type and frequency of their
// forms, a first period starting by the 28th, an end after the start, the
// registrant not among the group's other members; what the periods follow
// from never changes under the code, while the end and the active flag
// do, by versions; nothing is deleted.
func TestTaxRegistrationRules(t *testing.T) {
	need(t)
	const insert = `INSERT INTO tax.registration (tenant_id, id, code, legal_entity_id, group_member_ids, authority, number_cipher,
		number_fingerprint, number_last4, return_type, frequency, first_period_start, last_period_end, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, '\x01', repeat('a', 64), '0001', $7, $8, $9, $10, $11, $11)`
	reg := func(f fixture, set map[string]any) stmt {
		v := map[string]any{"code": "GST", "members": []uuid.UUID{}, "authority": "CRA", "return": "GST34", "frequency": "quarterly",
			"first": "2026-01-01", "last": nil}
		for k, x := range set {
			v[k] = x
		}
		return step(insert, f.tenant, id(), v["code"], f.entity, v["members"], v["authority"], v["return"], v["frequency"], v["first"], v["last"], f.actor)
	}
	update := func(set string) stmt {
		return step(`UPDATE tax.registration SET ` + set + `, version = version + 1 WHERE code = 'GST'`)
	}
	for _, c := range []struct {
		name string
		run  func(tx pgx.Tx, f fixture) error
		want string // empty: allowed
	}{
		{"a registration", func(tx pgx.Tx, f fixture) error { return all(tx, reg(f, nil)) }, ""},
		{"a tax group", func(tx pgx.Tx, f fixture) error { return all(tx, reg(f, map[string]any{"members": []uuid.UUID{id()}})) }, ""},
		{"ended and deactivated", func(tx pgx.Tx, f fixture) error {
			return all(tx, reg(f, nil), update(`last_period_end = '2026-12-31', is_active = false`))
		}, ""},
		{"changed without its version", func(tx pgx.Tx, f fixture) error {
			return all(tx, reg(f, nil), step(`UPDATE tax.registration SET is_active = false WHERE code = 'GST'`))
		}, "optimistic lock"},
		{"deleted", func(tx pgx.Tx, f fixture) error {
			return all(tx, reg(f, nil), step(`DELETE FROM tax.registration WHERE code = 'GST'`))
		}, "permission denied"},
		{"a code twice", func(tx pgx.Tx, f fixture) error {
			return all(tx, reg(f, nil), reg(f, map[string]any{"authority": "RQ"}))
		}, "registration_code_once"},
		{"a code's form", func(tx pgx.Tx, f fixture) error { return all(tx, reg(f, map[string]any{"code": "gst"})) }, "registration_code_check"},
		{"an authority's form", func(tx pgx.Tx, f fixture) error { return all(tx, reg(f, map[string]any{"authority": "cra"})) }, "registration_authority_check"},
		{"a return type's form", func(tx pgx.Tx, f fixture) error { return all(tx, reg(f, map[string]any{"return": "gst 34"})) }, "registration_return_type_check"},
		{"a frequency", func(tx pgx.Tx, f fixture) error { return all(tx, reg(f, map[string]any{"frequency": "weekly"})) }, "registration_frequency_check"},
		{"a start after the 28th", func(tx pgx.Tx, f fixture) error {
			return all(tx, reg(f, map[string]any{"first": "2026-01-29"}))
		}, "registration_first_period_start_check"},
		{"an end before the start", func(tx pgx.Tx, f fixture) error {
			return all(tx, reg(f, map[string]any{"last": "2025-12-31"}))
		}, "registration_check"},
		{"the registrant a member", func(tx pgx.Tx, f fixture) error {
			return all(tx, reg(f, map[string]any{"members": []uuid.UUID{f.entity}}))
		}, "registration_members"},
		{"a fingerprint's form", func(tx pgx.Tx, f fixture) error {
			return all(tx, reg(f, nil), update(`number_fingerprint = 'x'`))
		}, "registration_number_fingerprint_check"},
		{"no last four", func(tx pgx.Tx, f fixture) error {
			return all(tx, reg(f, nil), update(`number_last4 = ''`))
		}, "registration_number_last4_check"},
		{"the number resealed", func(tx pgx.Tx, f fixture) error {
			return all(tx, reg(f, nil), update(`number_cipher = '\x02', number_fingerprint = repeat('b', 64)`))
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error { return c.run(tx, f) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	for _, set := range []string{"code = 'GST2'", "legal_entity_id = gen_random_uuid()", "group_member_ids = ARRAY[gen_random_uuid()]",
		"authority = 'RQ'", "return_type = 'FPZ500'", "frequency = 'monthly'", "first_period_start = '2026-02-01'"} {
		t.Run("fixed: "+strings.SplitN(set, " ", 2)[0], func(t *testing.T) {
			f := newFixture(t)
			failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return all(tx, reg(f, nil), update(set)) }), "keeps its authority, return, frequency, first period and companies")
		})
	}
}

// TestTaxRegistrationPermission: the registrations' read permission is
// seeded and is no approval permission (T61g, 000172).
func TestTaxRegistrationPermission(t *testing.T) {
	var approval bool
	if err := testDB.QueryRow(context.Background(), `SELECT is_approval FROM ref.permission WHERE code = 'tax.registration.read'`).Scan(&approval); err != nil {
		t.Fatalf("tax.registration.read: %v", err)
	}
	if approval {
		t.Error("tax.registration.read: got an approval permission, want a read permission")
	}
}
