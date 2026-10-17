package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Contract renewal and expiry alerts (T47g, migration 000066): a contract
// is renewed by a successor that continues it, from the day after it
// ends, with its legal entity, supplier and currency; once at a time; and
// a contract is alerted of its coming expiry once.

// contract inserts a contract of f in the status, valid in 2026, renewing
// of when it is not nil, and returns its id.
func (f fixture) contract(tx pgx.Tx, status, from, to string, of *uuid.UUID) (uuid.UUID, error) {
	c := id()
	return c, exec(tx, `INSERT INTO procurement.contract (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, valid_from,
		valid_to, target_value, status, renewal_of_id, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, 'USD', $6, $7, 100000, $8, $9, $10, $10)`,
		f.tenant, c, c.String()[24:], f.entity, f.supplier, from, to, status, of, f.actor)
}

func TestContractRenewal(t *testing.T) {
	need(t)
	// predecessor is an active contract of 2026.
	predecessor := func(t *testing.T, f fixture) uuid.UUID {
		var c uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
			c, err = f.contract(tx, "active", "2026-01-01", "2026-12-31", nil)
			return err
		})
		return c
	}
	for _, c := range []struct {
		name, sql string
		want      string
	}{
		{"renewed from the day after", "", ""},
		{"renewed with a gap", "UPDATE procurement.contract SET valid_from = '2027-01-02', valid_to = '2027-12-31', version = version + 1 WHERE id = $1", "from the day after it ends"},
		{"renewed overlapping", "UPDATE procurement.contract SET valid_from = '2026-12-31', version = version + 1 WHERE id = $1", "from the day after it ends"},
		{"renewed with another supplier", "UPDATE procurement.contract SET supplier_id = (SELECT id FROM partner.business_partner WHERE number = 'S0002'), version = version + 1 WHERE id = $1",
			"from the day after it ends"},
		{"renewed in another currency", "UPDATE procurement.contract SET currency = 'EUR', version = version + 1 WHERE id = $1", "from the day after it ends"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			old := predecessor(t, f)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				renewal, err := f.contract(tx, "draft", "2027-01-01", "2027-12-31", &old)
				if err != nil || c.sql == "" {
					return err
				}
				return exec(tx, c.sql, renewal)
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
	t.Run("renewed twice", func(t *testing.T) {
		f := newFixture(t)
		old := predecessor(t, f)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			if _, err := f.contract(tx, "draft", "2027-01-01", "2027-12-31", &old); err != nil {
				return err
			}
			_, err := f.contract(tx, "draft", "2027-01-01", "2027-06-30", &old)
			return err
		}), "contract_renewed_once")
	})
	t.Run("renewed again after the renewal was cancelled", func(t *testing.T) {
		f := newFixture(t)
		old := predecessor(t, f)
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			first, err := f.contract(tx, "draft", "2027-01-01", "2027-12-31", &old)
			if err != nil {
				return err
			}
			if err := exec(tx, `UPDATE procurement.contract SET status = 'cancelled', version = version + 1 WHERE id = $1`, first); err != nil {
				return err
			}
			_, err = f.contract(tx, "draft", "2027-01-01", "2027-12-31", &old)
			return err
		})
	})
	t.Run("its own renewal", func(t *testing.T) {
		f := newFixture(t)
		old := predecessor(t, f)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.contract SET renewal_of_id = id, version = version + 1 WHERE id = $1`, old)
		}), "contract_not_its_own_renewal")
	})
	t.Run("a renewal of another tenant's contract", func(t *testing.T) {
		f, other := newFixture(t), newFixture(t)
		theirs := predecessor(t, other)
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			_, err := f.contract(tx, "draft", "2027-01-01", "2027-12-31", &theirs)
			return err
		}), "contract_renewal_fk")
	})
}

func TestContractAlertedOnce(t *testing.T) {
	need(t)
	f := newFixture(t)
	var c uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
		c, err = f.contract(tx, "active", "2026-01-01", "2026-12-31", nil)
		return err
	})
	alert := func(day string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE procurement.contract SET expiry_alerted_on = $2, version = version + 1 WHERE id = $1`, c, day)
		}
	}
	mustApp(t, f.tenant, alert("2026-12-01"))
	failsWith(t, asApp(t, f.tenant, alert("2026-12-02")), "alerted of its expiry once")
	failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE procurement.contract SET expiry_alerted_on = NULL, version = version + 1 WHERE id = $1`, c)
	}), "alerted of its expiry once")
	// The contract changes otherwise as before.
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE procurement.contract SET status = 'expired', version = version + 1 WHERE id = $1`, c)
	})
}
