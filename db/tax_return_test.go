package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Tax returns (T61i, migration 000174): a return starts prepared and moves
// prepared -> approved -> filed, or to withdrawn or rejected before
// filing, each move setting its own fields alone; its approver is never
// its preparer; a filed return names its confirmation, dated after its
// period; one return of a period is in progress and one original stands;
// an amendment corrects its period's filed return; its snapshot (boxes and
// items) is written while it is prepared and never changes; an item is
// reported in a return that stands and claims it, released only by one
// withdrawn or rejected, never moved from one to another.

// taxReturnScene is a tax scene with a registration of its legal entity
// and an entry of 2026-03-10 whose item is unreported.
type taxReturnScene struct {
	taxScene
	registration, entry uuid.UUID
}

func newTaxReturnScene(t *testing.T) taxReturnScene {
	t.Helper()
	s := taxReturnScene{taxScene: newTaxScene(t), registration: id()}
	mustApp(t, s.tenant, func(tx pgx.Tx) (err error) {
		if err := exec(tx, `INSERT INTO tax.registration (tenant_id, id, code, legal_entity_id, authority, number_cipher, number_fingerprint, number_last4,
				return_type, frequency, first_period_start, created_by, updated_by)
			VALUES ($1, $2, 'GST', $3, 'CRA', '\x01', repeat('a', 64), '0001', 'GST34', 'quarterly', '2026-01-01', $4, $4)`,
			s.tenant, s.registration, s.entity, s.actor); err != nil {
			return err
		}
		s.entry, err = s.taxedEntry(tx, true)
		return err
	})
	return s
}

// insertReturn inserts a prepared return of the scene's Q1 of the kind,
// amending amends when not nil, and claims the entry's item for it when
// claim is set.
func (s taxReturnScene) insertReturn(tx pgx.Tx, ret uuid.UUID, kind string, amends *uuid.UUID, claim bool) error {
	return s.insertReturnOf(tx, returnRow{id: ret, registration: s.registration, start: "2026-01-01", end: "2026-03-31", kind: kind, amends: amends}, claim)
}

// returnRow is a prepared return of a registration's period.
type returnRow struct {
	id, registration uuid.UUID
	start, end, kind string
	amends           *uuid.UUID
}

// insertReturnOf is insertReturn of any registration and period.
func (s taxReturnScene) insertReturnOf(tx pgx.Tx, r returnRow, claim bool) error {
	ret := r.id
	if err := exec(tx, `INSERT INTO tax.tax_return (tenant_id, id, registration_id, period_start, period_end, kind, amends_id, return_type, return_name,
			pack_country, pack_version, currency, item_count, items_hash, prior_item_count, prior_tax, above_threshold, approval_request_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'GST34', 'GST/HST', 'CA', '2026.3', 'USD', 1, repeat('b', 64), 0, 0, false,
			gen_random_uuid(), $8, $8)`, s.tenant, ret, r.registration, r.start, r.end, r.kind, r.amends, s.actor); err != nil {
		return err
	}
	if err := exec(tx, `INSERT INTO tax.tax_return_box (tenant_id, tax_return_id, box_no, code, label, kind, amount, prior_period_amount, corrected_amount,
			item_count) VALUES ($1, $2, 1, 'in', 'Input tax', 'selection', 500, 0, 500, 1)`, s.tenant, ret); err != nil {
		return err
	}
	if !claim {
		return nil
	}
	if err := s.snapshotItem(tx, ret); err != nil {
		return err
	}
	return exec(tx, `UPDATE ledger.tax_item SET reported_in = $2 WHERE journal_entry_id = $1`, s.entry, ret)
}

// snapshotItem adds the scene's item to the return's snapshot.
func (s taxReturnScene) snapshotItem(tx pgx.Tx, ret uuid.UUID) error {
	return exec(tx, `INSERT INTO tax.tax_return_item (tenant_id, tax_return_id, fiscal_year, journal_entry_id, item_no)
		SELECT tenant_id, $2, fiscal_year, journal_entry_id, item_no FROM ledger.tax_item WHERE journal_entry_id = $1`, s.entry, ret)
}

// move updates the return.
func move(ret uuid.UUID, set string) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		return exec(tx, `UPDATE tax.tax_return SET `+set+`, version = version + 1 WHERE id = $1`, ret)
	}
}

const (
	approve = `status = 'approved', approved_by = gen_random_uuid(), approved_at = now()`
	fileIt  = `status = 'filed', confirmation_number = 'CRA-1', filed_on = '2026-04-15', filed_by = gen_random_uuid(), filed_at = now()`
	endIt   = `decided_by = gen_random_uuid(), decided_at = now(), decision_comment = 'No'`
)

func TestTaxReturnRules(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name  string
		moves []string
		fails string
	}{
		{"approved by another person", []string{approve}, ""},
		{"filed once approved", []string{approve, fileIt}, ""},
		{"withdrawn while prepared", []string{"status = 'withdrawn', " + endIt}, ""},
		{"withdrawn once approved", []string{approve, "status = 'withdrawn', " + endIt}, ""},
		{"rejected while prepared", []string{"status = 'rejected', " + endIt}, ""},
		{"approved by its preparer", []string{`status = 'approved', approved_by = created_by, approved_at = now()`}, "tax_return_four_eyes"},
		{"approved by nobody", []string{`status = 'approved'`}, "tax_return_approved"},
		{"approved without its time", []string{`status = 'approved', approved_by = gen_random_uuid()`}, "tax_return_approved"},
		{"filed unapproved", []string{fileIt}, "tax_return_transition"},
		{"filed without its confirmation", []string{approve, `status = 'filed', filed_on = '2026-04-15', filed_by = gen_random_uuid(), filed_at = now()`},
			"tax_return_filed"},
		{"filed on its period's last day", []string{approve, `status = 'filed', confirmation_number = 'CRA-1', filed_on = '2026-03-31',
			filed_by = gen_random_uuid(), filed_at = now()`}, "tax_return_filed"},
		{"withdrawn once filed", []string{approve, fileIt, "status = 'withdrawn', " + endIt}, "tax_return_transition"},
		{"rejected once approved", []string{approve, "status = 'rejected', " + endIt}, "tax_return_transition"},
		{"withdrawn by nobody", []string{"status = 'withdrawn'"}, "tax_return_ended"},
		{"its snapshot changed by a move", []string{approve + `, items_hash = repeat('c', 64)`}, "tax_return_fixed"},
		{"filing fields set by an approval", []string{approve + `, confirmation_number = 'CRA-1'`}, "tax_return_fixed"},
		{"its preparer changed", []string{approve + `, created_by = gen_random_uuid()`}, "tax_return_fixed"},
		{"changed without a move", []string{`return_name = 'Other'`}, "tax_return_transition"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTaxReturnScene(t)
			ret := id()
			mustApp(t, s.tenant, func(tx pgx.Tx) error { return s.insertReturn(tx, ret, "original", nil, false) })
			var err error
			for _, m := range c.moves {
				if err = asApp(t, s.tenant, move(ret, m)); err != nil {
					break
				}
			}
			if c.fails == "" {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			failsOn(t, err, c.fails)
		})
	}
	t.Run("starts prepared", func(t *testing.T) {
		s := newTaxReturnScene(t)
		failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO tax.tax_return (tenant_id, id, registration_id, period_start, period_end, kind, status, return_type, return_name,
				pack_country, pack_version, currency, item_count, items_hash, prior_item_count, prior_tax, above_threshold, approval_request_id, approved_by,
				approved_at, created_by, updated_by)
				VALUES ($1, gen_random_uuid(), $2, '2026-01-01', '2026-03-31', 'original', 'approved', 'GST34', 'GST/HST', 'CA', '2026.3', 'USD', 0,
					repeat('b', 64), 0, 0, false, gen_random_uuid(), gen_random_uuid(), now(), $3, $3)`, s.tenant, s.registration, s.actor)
		}), "tax_return_transition")
	})
}

func TestTaxReturnsOfAPeriod(t *testing.T) {
	need(t)
	s := newTaxReturnScene(t)
	first, second := id(), id()
	mustApp(t, s.tenant, func(tx pgx.Tx) error { return s.insertReturn(tx, first, "original", nil, false) })
	failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error { return s.insertReturn(tx, second, "original", nil, false) }), "tax_return_in_progress")
	failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error { return s.insertReturn(tx, second, "amendment", &first, false) }), "tax_return_amends_filed")
	failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error { return s.insertReturn(tx, second, "amendment", nil, false) }), "tax_return_amends")
	failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO tax.tax_return_box (tenant_id, tax_return_id, box_no, code, label, kind, amount, prior_period_amount, corrected_amount,
			item_count) VALUES ($1, $2, 2, 'in', 'Again', 'selection', 0, 0, 0, 0)`, s.tenant, first)
	}), "tax_return_box_code_once")
	mustApp(t, s.tenant, move(first, approve))
	mustApp(t, s.tenant, move(first, fileIt))
	t.Run("an amendment of another period's filed return", func(t *testing.T) {
		failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error {
			return s.insertReturnOf(tx, returnRow{id: id(), registration: s.registration, start: "2026-04-01", end: "2026-06-30", kind: "amendment",
				amends: &first}, false)
		}), "tax_return_amends_filed")
	})
	failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error { return s.insertReturn(tx, second, "original", nil, false) }), "tax_return_one_original")
	mustApp(t, s.tenant, func(tx pgx.Tx) error { return s.insertReturn(tx, second, "amendment", &first, false) })

	t.Run("its snapshot", func(t *testing.T) {
		box := func(ret uuid.UUID) func(tx pgx.Tx) error {
			return func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO tax.tax_return_box (tenant_id, tax_return_id, box_no, code, label, kind, amount, prior_period_amount,
					corrected_amount) VALUES ($1, $2, 2, 'net', 'Net tax', 'expression', 0, 0, 0)`, s.tenant, ret)
			}
		}
		failsOn(t, asApp(t, s.tenant, box(first)), "tax_return_snapshot")
		failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error { return s.snapshotItem(tx, first) }), "tax_return_snapshot")
		mustApp(t, s.tenant, func(tx pgx.Tx) error { return s.snapshotItem(tx, second) })
		failsWith(t, asApp(t, s.tenant, func(tx pgx.Tx) error {
			return exec(tx, `DELETE FROM tax.tax_return_item WHERE tax_return_id = $1`, second)
		}), "permission denied")
		mustApp(t, s.tenant, box(second))
		failsWith(t, asApp(t, s.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE tax.tax_return_box SET amount = 1 WHERE tax_return_id = $1`, second)
		}), "permission denied")
		failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO tax.tax_return_box (tenant_id, tax_return_id, box_no, code, label, kind, amount, prior_period_amount,
				corrected_amount) VALUES ($1, $2, 3, 'x', 'X', 'selection', 0, 0, 0)`, s.tenant, second)
		}), "tax_return_box_items")
	})
}

func TestTaxItemReported(t *testing.T) {
	need(t)
	release := func(entry uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.tax_item SET reported_in = NULL WHERE journal_entry_id = $1`, entry)
		}
	}
	t.Run("claimed by a return that claims it", func(t *testing.T) {
		s := newTaxReturnScene(t)
		ret := id()
		mustApp(t, s.tenant, func(tx pgx.Tx) error { return s.insertReturn(tx, ret, "original", nil, true) })
		failsOn(t, asApp(t, s.tenant, release(s.entry)), "tax_item_reported")
		failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.tax_item SET reported_in = gen_random_uuid() WHERE journal_entry_id = $1`, s.entry)
		}), "tax_item_fixed")
		mustApp(t, s.tenant, move(ret, "status = 'withdrawn', "+endIt))
		mustApp(t, s.tenant, release(s.entry))
		failsOn(t, asApp(t, s.tenant, release(s.entry)), "tax_item_fixed")
	})
	t.Run("not by one that does not stand or claim it", func(t *testing.T) {
		s := newTaxReturnScene(t)
		ret := id()
		mustApp(t, s.tenant, func(tx pgx.Tx) error { return s.insertReturn(tx, ret, "original", nil, false) })
		withdrawn, early, elsewhere := id(), id(), id()
		other, otherReg := id(), id()
		mustApp(t, s.tenant, func(tx pgx.Tx) error {
			// A return withdrawn, one of a period ending before the item's
			// tax date, one of a registration of another company: each
			// with the item in its snapshot.
			if err := s.insertReturnOf(tx, returnRow{id: withdrawn, registration: s.registration, start: "2025-12-01", end: "2026-03-31", kind: "original"}, false); err != nil {
				return err
			}
			if err := s.snapshotItem(tx, withdrawn); err != nil {
				return err
			}
			if err := exec(tx, `UPDATE tax.tax_return SET status = 'withdrawn', `+endIt+`, version = version + 1 WHERE id = $1`, withdrawn); err != nil {
				return err
			}
			if err := s.insertReturnOf(tx, returnRow{id: early, registration: s.registration, start: "2026-02-01", end: "2026-02-28", kind: "original"}, false); err != nil {
				return err
			}
			if err := s.snapshotItem(tx, early); err != nil {
				return err
			}
			for _, st := range []stmt{
				step(`INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
					VALUES ($1, $2, '2000', 'Other', 'CA', 'CAD', $3, 'active', $4, $4)`, s.tenant, other, s.calendar, s.actor),
				step(`INSERT INTO tax.registration (tenant_id, id, code, legal_entity_id, authority, number_cipher, number_fingerprint, number_last4,
						return_type, frequency, first_period_start, created_by, updated_by)
					VALUES ($1, $2, 'OTHER', $3, 'CRA', '\x01', repeat('c', 64), '0002', 'GST34', 'quarterly', '2026-01-01', $4, $4)`,
					s.tenant, otherReg, other, s.actor),
			} {
				if err := exec(tx, st.sql, st.args...); err != nil {
					return err
				}
			}
			if err := s.insertReturnOf(tx, returnRow{id: elsewhere, registration: otherReg, start: "2026-01-01", end: "2026-03-31", kind: "original"}, false); err != nil {
				return err
			}
			return s.snapshotItem(tx, elsewhere)
		})
		for name, to := range map[string]string{"no return": "gen_random_uuid()", "a return without it": "'" + ret.String() + "'",
			"a withdrawn return": "'" + withdrawn.String() + "'", "a period ending before its tax date": "'" + early.String() + "'",
			"another company's registration": "'" + elsewhere.String() + "'"} {
			t.Run(name, func(t *testing.T) {
				failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error {
					return exec(tx, `UPDATE ledger.tax_item SET reported_in = `+to+` WHERE journal_entry_id = $1`, s.entry)
				}), "tax_item_reported")
			})
		}
	})
}
