package db

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestPartnerValidityCompanyDay (migration 000347; ADR-0051 As built,
// hdates): a business partner's address and bank account may be ended on
// the database's yesterday, the day partner's day (the earliest of the
// companies' days) is in Toronto from 19:00, but not before it; identity's
// assignments keep the database's day (TestAssignmentsAreEndedNeverRewritten).
func TestPartnerValidityCompanyDay(t *testing.T) {
	need(t)
	const (
		address = `INSERT INTO partner.address (tenant_id, id, partner_id, purpose, line1, city, country_code, valid_from, created_by)
			VALUES ($1, $2, $3, 'legal', 'Street 1', 'Town', 'US', current_date - 10, $1)`
		bank = `INSERT INTO partner.bank_account (tenant_id, id, partner_id, account_cipher, account_fingerprint, account_last4, account_scheme, bic,
			bank_country_code, holder_name, valid_from, partner_change_id, created_by) VALUES ($1, $2, $3, '\x01', repeat('a', 64), '1234', 'iban', 'COBADEFFXXX',
			'DE', 'Holder', current_date - 10, gen_random_uuid(), $1)`
	)
	f := newFixture(t)
	for _, c := range []struct {
		name, insert, table string
		back                int
		want                string
	}{
		{"an address ended yesterday", address, "partner.address", 1, ""},
		{"an address ended the day before", address, "partner.address", 2, "validity cannot be ended in the past"},
		{"a bank account ended yesterday", bank, "partner.bank_account", 1, ""},
		{"a bank account ended the day before", bank, "partner.bank_account", 2, "validity cannot be ended in the past"},
	} {
		t.Run(c.name, func(t *testing.T) {
			partner, row := id(), id()
			number := strings.ToUpper(strings.ReplaceAll(partner.String(), "-", "")[:20])
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return all(tx,
					step(`INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
						VALUES ($1, $2, $3, 'organization', 'Ann Ltd', 'US', 'active', $1, $1)`, f.tenant, partner, number),
					step(c.insert, f.tenant, row, partner),
					step(`UPDATE `+c.table+` SET valid_to = current_date - $2::int, ended_by = $3 WHERE id = $1`, row, c.back, f.tenant))
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
