package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Foreign-currency revaluation (T48a, ADR-0042, migration 000091): the
// rules the database keeps for revalued accounts, their valuation
// accounts, revaluation runs and the items they valued.

// fxAccounts adds a revenue and an unrealised loss account to f's legal
// entity and returns them with an asset account that reconciles nothing.
func (f fixture) fxAccounts(t *testing.T) (revenue, loss, adjustment uuid.UUID) {
	t.Helper()
	revenue, loss, adjustment = id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		for _, a := range []struct {
			id       uuid.UUID
			num, typ string
		}{{revenue, "480100", "revenue"}, {loss, "690100", "expense"}, {adjustment, "211099", "liability"}} {
			if err := exec(tx, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, reconciliation_kind, status, created_by, updated_by)
				VALUES ($1,$2,$3,$4,$4,$5,'none','active',$6,$6)`, f.tenant, a.id, f.chart, a.num, a.typ, f.actor); err != nil {
				return err
			}
			if err := exec(tx, "INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1,$2,$3,$4,$4)",
				f.tenant, f.entity, a.id, f.actor); err != nil {
				return err
			}
		}
		return nil
	})
	return revenue, loss, adjustment
}

// flag sets account's revalue_foreign_currency in f's legal entity.
func (f fixture) flag(account uuid.UUID, on bool) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		return exec(tx, `UPDATE ledger.company_account SET revalue_foreign_currency = $3, version = version + 1
			WHERE legal_entity_id = $1 AND account_id = $2`, f.entity, account, on)
	}
}

// valuation inserts the valuation of account with the given accounts.
func (f fixture) valuation(account uuid.UUID, adjustment *uuid.UUID, gain, loss uuid.UUID, active bool) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO ledger.fx_valuation_account (tenant_id, legal_entity_id, account_id, adjustment_account_id, gain_account_id,
			loss_account_id, is_active, created_by, updated_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8)`, f.tenant, f.entity, account, adjustment, gain, loss, active, f.actor)
	}
}

// TestRevaluedAccounts: a flagged account is a balance-sheet account that
// reconciles no partner, and keeps its flag while a valuation names it; a
// valuation is of a reconciliation account with a balance-sheet adjustment
// account that reconciles nothing, or of a flagged account without one,
// with a revenue gain and an expense loss; an inactive one is not checked.
func TestRevaluedAccounts(t *testing.T) {
	need(t)
	f := newFixture(t)
	revenue, loss, adjustment := f.fxAccounts(t)
	equity := id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, reconciliation_kind, status, created_by, updated_by)
			VALUES ($1,$2,$3,'300000','Equity','equity','none','active',$4,$4)`, f.tenant, equity, f.chart, f.actor); err != nil {
			return err
		}
		return exec(tx, "INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1,$2,$3,$4,$4)",
			f.tenant, f.entity, equity, f.actor)
	})
	mustApp(t, f.tenant, f.flag(f.bank, true))
	mustApp(t, f.tenant, f.valuation(f.bank, nil, revenue, loss, true))
	mustApp(t, f.tenant, f.valuation(f.ap, &adjustment, revenue, loss, true))
	mustApp(t, f.tenant, f.valuation(f.expense, &f.ap, loss, revenue, false))
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, "INSERT INTO ledger.fx_valuation (tenant_id, legal_entity_id, created_by, updated_by) VALUES ($1,$2,$3,$3)", f.tenant, f.entity, f.actor)
	})
	for _, c := range []struct {
		name   string
		change func(tx pgx.Tx) error
		fails  string
	}{
		{"a flagged reconciliation account", f.flag(f.ap, true), "company_account_revalue_foreign_currency"},
		{"a flagged expense", f.flag(f.expense, true), "company_account_revalue_foreign_currency"},
		{"a flagged equity account", f.flag(equity, true), "company_account_revalue_foreign_currency"},
		{"unflagged while valued", f.flag(f.bank, false), "company_account_revalue_foreign_currency_valued"},
		{"an account neither reconciling nor flagged", f.valuation(f.tax, nil, revenue, loss, true), "fx_valuation_account_revalued"},
		{"a reconciliation account without adjustment", f.valuation(f.ap, nil, revenue, loss, true), "fx_valuation_account_adjustment"},
		{"a flagged account with adjustment", f.valuation(f.bank, &adjustment, revenue, loss, true), "fx_valuation_account_adjustment"},
		{"an adjustment that reconciles", f.valuation(f.ap, &f.ap, revenue, loss, true), "fx_valuation_account_adjustment"},
		{"an adjustment off the balance sheet", f.valuation(f.ap, &f.expense, revenue, loss, true), "fx_valuation_account_adjustment"},
		{"a gain that is no revenue", f.valuation(f.ap, &adjustment, loss, loss, true), "fx_valuation_account_gain"},
		{"a loss that is no expense", f.valuation(f.ap, &adjustment, revenue, revenue, true), "fx_valuation_account_loss"},
		{"an account of no company", f.valuation(id(), nil, revenue, loss, true), "fx_valuation_account_tenant_id_legal_entity_id_account_id_fkey"},
		{"another rate type", func(tx pgx.Tx) error {
			return exec(tx, "UPDATE ledger.fx_valuation SET rate_type = 'spot', version = version + 1 WHERE legal_entity_id = $1", f.entity)
		}, "fx_valuation_rate_type_check"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.change), c.fails) })
	}
	t.Run("a reconciliation account keeps its adjustment", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE ledger.fx_valuation_account SET adjustment_account_id = NULL, version = version + 1 WHERE account_id = $1", f.ap)
		}), "fx_valuation_account_adjustment")
	})
	t.Run("unflagged once inactive", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, "UPDATE ledger.fx_valuation_account SET is_active = false, version = version + 1 WHERE account_id = $1", f.bank); err != nil {
				return err
			}
			return f.flag(f.bank, false)(tx)
		})
	})
}

// run inserts a revaluation run of f at key date with the given entries
// (none when nil) and returns its id.
func (f fixture) run(tx pgx.Tx, key string, valuation, reversal *uuid.UUID) (uuid.UUID, error) {
	r := id()
	var year *int
	if valuation != nil {
		y := 2026
		year = &y
	}
	return r, exec(tx, `INSERT INTO ledger.revaluation_run (tenant_id, id, legal_entity_id, ledger_id, key_date, rate_type, valuation_entry_year,
		valuation_entry_id, reversal_entry_year, reversal_entry_id, created_by, updated_by) VALUES ($1,$2,$3,$4,$5,'average',$6,$7,$6,$8,$9,$9)`,
		f.tenant, r, f.entity, f.ledger, key, year, valuation, reversal, f.actor)
}

// TestRevaluationRuns: a run is posted first with its valuation entry on
// the key date and its reversal on the next day, or with neither; one run
// per key date is posted; a posted run may be cancelled, which frees the
// key date, and is otherwise fixed.
func TestRevaluationRuns(t *testing.T) {
	need(t)
	f := newFixture(t)
	var valuation, reversal, stray uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
		if valuation, err = f.entry(tx, 3, "2026-03-31", line{account: f.bank, amount: 10}, line{account: f.expense, amount: -10}); err != nil {
			return err
		}
		if reversal, err = f.entry(tx, 4, "2026-04-01", line{account: f.bank, amount: -10}, line{account: f.expense, amount: 10}); err != nil {
			return err
		}
		stray, err = f.entry(tx, 3, "2026-03-30", line{account: f.bank, amount: 10}, line{account: f.expense, amount: -10})
		return err
	})
	var posted uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) { posted, err = f.run(tx, "2026-03-31", &valuation, &reversal); return err })
	update := func(set string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, "UPDATE ledger.revaluation_run SET "+set+", version = version + 1 WHERE id = $1", posted)
		}
	}
	insert := func(key string, valuation, reversal *uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error { _, err := f.run(tx, key, valuation, reversal); return err }
	}
	for _, c := range []struct {
		name   string
		change func(tx pgx.Tx) error
		fails  string
	}{
		{"a second posted run of the key date", insert("2026-03-31", nil, nil), "revaluation_run_posted"},
		{"a valuation without reversal", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.revaluation_run (tenant_id, id, legal_entity_id, ledger_id, key_date, rate_type, valuation_entry_year,
				valuation_entry_id, created_by, updated_by) VALUES ($1,$2,$3,$4,'2026-02-28','average',2026,$5,$6,$6)`, f.tenant, id(), f.entity, f.ledger, valuation, f.actor)
		}, "revaluation_run_entries"},
		{"a valuation off the key date", insert("2026-02-28", &stray, &reversal), "revaluation_run_entry_dates"},
		{"a reversal off the next day", insert("2026-03-30", &stray, &reversal), "revaluation_run_entry_dates"},
		{"an entry of another legal entity", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.revaluation_run (tenant_id, id, legal_entity_id, ledger_id, key_date, rate_type, valuation_entry_year,
				valuation_entry_id, reversal_entry_year, reversal_entry_id, created_by, updated_by) VALUES ($1,$2,$3,$4,'2026-03-31','average',2026,$5,2026,$6,$7,$7)`,
				f.tenant, id(), id(), f.ledger, valuation, reversal, f.actor)
		}, "revaluation_run_entry_dates"},
		{"a cancelled run first", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.revaluation_run (tenant_id, id, legal_entity_id, ledger_id, key_date, rate_type, status, created_by, updated_by)
				VALUES ($1,$2,$3,$4,'2026-05-31','average','cancelled',$5,$5)`, f.tenant, id(), f.entity, f.ledger, f.actor)
		}, "revaluation_run_lifecycle"},
		{"a changed key date", update("key_date = '2026-04-30'"), "revaluation_run_fixed"},
		{"a dropped entry", update("reversal_entry_id = NULL, reversal_entry_year = NULL, valuation_entry_id = NULL, valuation_entry_year = NULL"),
			"revaluation_run_fixed"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.change), c.fails) })
	}
	t.Run("a rate type outside the list", func(t *testing.T) {
		failsOn(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.revaluation_run (tenant_id, id, legal_entity_id, ledger_id, key_date, rate_type, created_by, updated_by)
				VALUES ($1,$2,$3,$4,'2026-05-31','spot',$5,$5)`, f.tenant, id(), f.entity, f.ledger, f.actor)
		}), "revaluation_run_rate_type_check")
	})
	t.Run("cancelled once, then rerun", func(t *testing.T) {
		mustApp(t, f.tenant, update("status = 'cancelled'"))
		failsOn(t, asApp(t, f.tenant, update("status = 'posted'")), "revaluation_run_lifecycle")
		failsOn(t, asApp(t, f.tenant, insert("2026-03-31", &valuation, &reversal)), "revaluation_run_valuation_entry")
		var again uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
			again, err = f.entry(tx, 3, "2026-03-31", line{account: f.bank, amount: 10}, line{account: f.expense, amount: -10})
			return err
		})
		failsOn(t, asApp(t, f.tenant, insert("2026-03-31", &again, &reversal)), "revaluation_run_reversal_entry")
		mustApp(t, f.tenant, insert("2026-03-31", nil, nil))
	})
}

// TestRevaluationItems: an item is an open item (a journal line, with its
// partner) or a flagged account's balance in one currency (no line, no
// partner), valued once per run, at a positive rate, of an amount; items
// are insert-only.
func TestRevaluationItems(t *testing.T) {
	need(t)
	f := newFixture(t)
	var invoice, run uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) (err error) {
		if invoice, err = f.entry(tx, 3, "2026-03-02", line{account: f.expense, amount: 100}, line{account: f.ap, amount: -100, partner: &f.supplier}); err != nil {
			return err
		}
		run, err = f.run(tx, "2026-03-31", nil, nil)
		return err
	})
	item := func(no int, account uuid.UUID, partner, entry *uuid.UUID, lineNo *int, tc int, rate string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			var year *int
			if entry != nil {
				y := 2026
				year = &y
			}
			return exec(tx, `INSERT INTO ledger.revaluation_item (tenant_id, run_id, item_no, account_id, partner_id, fiscal_year, journal_entry_id, line_no,
				currency_tc, amount_tc, amount_lc, rate, rate_valid_from, valued_lc) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'GBP',$9,-90,$10,'2026-03-01',-95)`,
				f.tenant, run, no, account, partner, year, entry, lineNo, tc, rate)
		}
	}
	two, one := 2, 1
	mustApp(t, f.tenant, item(1, f.ap, &f.supplier, &invoice, &two, -100, "0.95"))
	mustApp(t, f.tenant, item(2, f.bank, nil, nil, nil, 50, "0.95"))
	for _, c := range []struct {
		name   string
		insert func(tx pgx.Tx) error
		fails  string
	}{
		{"an open item twice", item(3, f.ap, &f.supplier, &invoice, &two, -100, "0.95"), "revaluation_item_line"},
		{"a balance twice", item(3, f.bank, nil, nil, nil, 50, "0.95"), "revaluation_item_balance"},
		{"a balance with a partner", item(3, f.tax, &f.supplier, nil, nil, 50, "0.95"), "revaluation_item_kind"},
		{"a line without its number", item(3, f.ap, &f.supplier, &invoice, nil, -100, "0.95"), "revaluation_item_kind"},
		{"a line that is not posted", item(3, f.ap, &f.supplier, &invoice, &[]int{9}[0], -100, "0.95"),
			"revaluation_item_open_item"},
		{"no amount", item(3, f.expense, nil, &invoice, &one, 0, "0.95"), "revaluation_item_amount_tc_check"},
		{"a rate of 0", item(3, f.expense, nil, &invoice, &one, 100, "0"), "revaluation_item_rate_check"},
		{"another run's item", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.revaluation_item (tenant_id, run_id, item_no, account_id, currency_tc, amount_tc, amount_lc, rate,
				rate_valid_from, valued_lc) VALUES ($1,$2,1,$3,'GBP',1,1,1,'2026-03-01',1)`, f.tenant, id(), f.bank)
		}, "revaluation_item_tenant_id_run_id_fkey"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, f.tenant, c.insert), c.fails) })
	}
	t.Run("insert-only", func(t *testing.T) {
		err := asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, "UPDATE ledger.revaluation_item SET valued_lc = 0 WHERE run_id = $1", run)
		})
		failsWith(t, err, "permission denied")
	})
}
