package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type stmt struct {
	sql  string
	args []any
}

func step(sql string, args ...any) stmt { return stmt{sql, args} }

// taxLine adds line n of entry je on account with the code (in USD, the
// group amount 90%).
func (s taxScene) taxLine(tx pgx.Tx, je uuid.UUID, n int, account uuid.UUID, amount int64, code string) error {
	return exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
		posting_date, account_id, amount_tc, amount_lc, amount_gc, tax_code) VALUES ($1,2026,$2,$3,$4,$5,3,'2026-03-10',$6,$7,$7,$8,nullif($9, ''))`,
		s.tenant, je, n, s.entity, s.ledger, account, amount, amount*9/10, code)
}

// taxedEntry is an entry of 105.00 on I5: 100.00 expense, 5.00 GST on the
// tax account, the bank; items written unless items is false.
func (s taxScene) taxedEntry(tx pgx.Tx, items bool) (uuid.UUID, error) {
	je, err := s.entry(tx, 3, "2026-03-10", line{account: s.bank, amount: -10500})
	if err == nil {
		err = s.taxLine(tx, je, 2, s.expense, 10000, "I5")
	}
	if err == nil {
		err = s.taxLine(tx, je, 3, s.tax, 500, "I5")
	}
	if err == nil && items {
		err = exec(tx, `SELECT ledger.write_tax_items($1, 2026::smallint, $2, '2026-03-10'::date, false)`, s.tenant, je)
	}
	return je, err
}

// TestTaxItemRules: an entry's tax lines have their items when it
// commits, of its legal entity and adding up to its tax lines per account
// and code; output tax recovers nothing and input tax at most itself; an
// item is fixed but for the return that reports it, set once (T61c).
func TestTaxItemRules(t *testing.T) {
	need(t)
	s := newTaxScene(t)
	var entry uuid.UUID
	mustApp(t, s.tenant, func(tx pgx.Tx) (err error) { entry, err = s.taxedEntry(tx, true); return err })
	var got string
	mustApp(t, s.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT string_agg(code || '/' || component || ' ' || base_tc || ' ' || tax_tc || ' ' || recoverable_tc || ' '
			|| trim_scale(rate_pct) || ' ' || (account_id = $2)::text, '; ' ORDER BY item_no) FROM ledger.tax_item WHERE journal_entry_id = $1`, entry, s.tax).Scan(&got)
	})
	// The non-deductible PST has no line of its own and no item (T61d).
	if want := "I5/gst 10000 500 500 5 true"; got != want {
		t.Errorf("items: got %s, want %s", got, want)
	}
	for _, c := range []struct {
		name  string
		write func(tx pgx.Tx) error
		fails string
	}{
		{"tax lines without items", func(tx pgx.Tx) error { _, err := s.taxedEntry(tx, false); return err }, "tax_item_missing"},
		{"items that differ from the lines", func(tx pgx.Tx) error {
			je, err := s.entry(tx, 3, "2026-03-10", line{account: s.bank, amount: -10500})
			if err == nil {
				err = s.taxLine(tx, je, 2, s.expense, 10000, "I5")
			}
			if err == nil {
				err = exec(tx, `SELECT ledger.write_tax_items($1, 2026::smallint, $2, '2026-03-10'::date, false)`, s.tenant, je)
			}
			if err == nil {
				err = s.taxLine(tx, je, 3, s.tax, 500, "I5") // after the items: theirs is no tax
			}
			return err
		}, "tax_item_lines"},
		{"an item of another legal entity", func(tx pgx.Tx) error {
			je, err := s.taxedEntry(tx, true)
			if err != nil {
				return err
			}
			other := id()
			for _, st := range []stmt{
				step(`INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
					VALUES ($1, $2, '2000', 'Other', 'CA', 'CAD', $3, 'active', $4, $4)`, s.tenant, other, s.calendar, s.actor),
				step(`INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1, $2, $3, $4, $4)`,
					s.tenant, other, s.tax, s.actor),
				step(`INSERT INTO ledger.tax_code_definition (tenant_id, legal_entity_id, code, description, direction, kind, created_by, updated_by)
					SELECT tenant_id, $2, code, description, direction, kind, created_by, updated_by FROM ledger.tax_code_definition
					WHERE tenant_id = $1 AND code = 'I5'`, s.tenant, other),
				step(`INSERT INTO ledger.tax_component (tenant_id, legal_entity_id, code, component, account_key, recoverable_pct, account_id, created_by, updated_by)
					VALUES ($1, $2, 'I5', 'gst', 'input', 100, $3, $4, $4)`, s.tenant, other, s.tax, s.actor),
				step(`INSERT INTO ledger.tax_item (tenant_id, fiscal_year, journal_entry_id, item_no, legal_entity_id, code, component, direction, kind, account_key,
					account_id, tax_date, tax_rate_id, rate_pct, recoverable_pct, base_tc, tax_tc, recoverable_tc, base_lc, tax_lc, recoverable_lc)
					VALUES ($1, 2026, $2, 2, $3, 'I5', 'gst', 'input', 'standard', 'input', NULL, '2026-03-10', $4, 5, 100, 0, 0, 0, 0, 0, 0)`,
					s.tenant, je, other, s.gst2026),
			} {
				if err := exec(tx, st.sql, st.args...); err != nil {
					return err
				}
			}
			return nil
		}, "tax_item_entity"},
		{"output tax recovered", func(tx pgx.Tx) error {
			je, err := s.taxedEntry(tx, true)
			if err != nil {
				return err
			}
			return exec(tx, `INSERT INTO ledger.tax_item (tenant_id, fiscal_year, journal_entry_id, item_no, legal_entity_id, code, component, direction,
				kind, account_key, account_id, tax_date, tax_rate_id, rate_pct, recoverable_pct, base_tc, tax_tc, recoverable_tc, base_lc, tax_lc, recoverable_lc)
				VALUES ($1, 2026, $2, 2, $3, 'I5', 'gst', 'output', 'standard', 'output', NULL, '2026-03-10', $4, 5, 100, 0, 0, 1, 0, 0, 0)`,
				s.tenant, je, s.entity, s.gst2026)
		}, "tax_item_recoverable"},
		{"more recovered than charged", func(tx pgx.Tx) error {
			je, err := s.taxedEntry(tx, true)
			if err != nil {
				return err
			}
			return exec(tx, `INSERT INTO ledger.tax_item (tenant_id, fiscal_year, journal_entry_id, item_no, legal_entity_id, code, component, direction,
				kind, account_key, account_id, tax_date, tax_rate_id, rate_pct, recoverable_pct, base_tc, tax_tc, recoverable_tc, base_lc, tax_lc, recoverable_lc)
				VALUES ($1, 2026, $2, 2, $3, 'I5', 'gst', 'input', 'standard', 'input', NULL, '2026-03-10', $4, 5, 100, 0, 10, 11, 0, 10, 10)`,
				s.tenant, je, s.entity, s.gst2026)
		}, "tax_item_recoverable"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, s.tenant, c.write), c.fails) })
	}
	report := func(set string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, "UPDATE ledger.tax_item SET "+set+" WHERE journal_entry_id = $1", entry)
		}
	}
	t.Run("reported once", func(t *testing.T) {
		failsOn(t, asApp(t, s.tenant, report("reported_in = NULL")), "tax_item_fixed")
		mustApp(t, s.tenant, report("reported_in = gen_random_uuid()"))
		failsOn(t, asApp(t, s.tenant, report("reported_in = gen_random_uuid()")), "tax_item_fixed")
	})
	t.Run("otherwise fixed", func(t *testing.T) {
		for _, change := range []func(tx pgx.Tx) error{report("tax_tc = 0"),
			func(tx pgx.Tx) error {
				return exec(tx, "DELETE FROM ledger.tax_item WHERE journal_entry_id = $1", entry)
			}} {
			failsWith(t, asApp(t, s.tenant, change), "permission denied")
		}
	})
}

// TestTaxItemsBackfilled: 000140 writes the items of the entries posted
// before, from their lines at their posting date: a tax line on a
// component's former account is its tax, the code's other lines its base,
// and an entry without codes has none; the rate check reads them.
func TestTaxItemsBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, 140)
	f := fixtureIDs()
	rate := id()
	var taxed, plain uuid.UUID
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) (err error) {
		if err := f.seed(tx); err != nil {
			return err
		}
		for _, st := range []stmt{
			step(`INSERT INTO ledger.tax_code_definition (tenant_id, legal_entity_id, code, description, direction, kind, created_by, updated_by)
				VALUES ($1, $2, 'V1', 'Input', 'input', 'standard', $3, $3)`, f.tenant, f.entity, f.actor),
			step(`INSERT INTO ledger.tax_component (tenant_id, legal_entity_id, code, component, account_key, recoverable_pct, account_id, created_by, updated_by)
				VALUES ($1, $2, 'V1', 'tax', 'input', 100, $3, $4, $4)`, f.tenant, f.entity, f.tax, f.actor),
			step(`INSERT INTO ledger.tax_rate (tenant_id, id, legal_entity_id, code, component, valid_from, rate_pct, created_by)
				VALUES ($1, $2, $3, 'V1', 'tax', '2026-01-01', 19, $4)`, f.tenant, rate, f.entity, f.actor),
		} {
			if err := exec(tx, st.sql, st.args...); err != nil {
				return err
			}
		}
		if taxed, err = f.entry(tx, 2, "2026-02-10", line{account: f.bank, amount: -119}); err != nil {
			return err
		}
		for i, l := range []struct {
			account uuid.UUID
			amount  int64
		}{{f.expense, 100}, {f.tax, 19}} {
			if err := exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
				posting_date, account_id, amount_tc, amount_lc, amount_gc, tax_code) VALUES ($1,2026,$2,$3,$4,$5,2,'2026-02-10',$6,$7,$7,$8,'V1')`,
				f.tenant, taxed, i+2, f.entity, f.ledger, l.account, l.amount, l.amount*9/10); err != nil {
				return err
			}
		}
		// The component's account moves on: 140000 becomes a former one.
		if err := exec(tx, `UPDATE ledger.tax_component SET account_id = $2, version = version + 1 WHERE tenant_id = $1`, f.tenant, f.bank); err != nil {
			return err
		}
		plain, err = f.entry(tx, 2, "2026-02-11", line{account: f.bank, amount: -5}, line{account: f.expense, amount: 5})
		return err
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	var got string
	var n int
	if err := conn.QueryRow(context.Background(), `SELECT string_agg(code || '/' || component || ' ' || tax_date || ' ' || base_tc || ' ' || tax_tc || ' '
			|| recoverable_tc || ' ' || (account_id = $2)::text || ' ' || (tax_rate_id = $3)::text, '; '),
			(SELECT count(*) FROM ledger.tax_item WHERE journal_entry_id = $4)
		FROM ledger.tax_item WHERE journal_entry_id = $1`, taxed, f.tax, rate, plain).Scan(&got, &n); err != nil {
		t.Fatal(err)
	}
	if want := "V1/tax 2026-02-10 100 19 19 true true"; got != want {
		t.Errorf("items of the taxed entry: got %s, want %s", got, want)
	}
	if n != 0 {
		t.Errorf("items of an entry without codes: got %d, want none", n)
	}
	var posted bool
	if err := conn.QueryRow(context.Background(), `SELECT ledger.tax_code_posted($1, $2, 'V1', '2026-02-01', '2026-03-01')`, f.tenant, f.entity).Scan(&posted); err != nil {
		t.Fatal(err)
	}
	if !posted {
		t.Error("the backfilled posting: got not posted, want the rate in use")
	}
}
