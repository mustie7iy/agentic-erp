package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// taxLine adds line n of entry je on account with the code (in USD, the
// group amount 90%).
func (s taxScene) taxLine(tx pgx.Tx, je uuid.UUID, n int, account uuid.UUID, amount int64, code string) error {
	return s.taxLineIn(tx, s.ledger, je, n, account, amount, code)
}

// taxLineIn is taxLine in a ledger.
func (s taxScene) taxLineIn(tx pgx.Tx, ledger, je uuid.UUID, n int, account uuid.UUID, amount int64, code string) error {
	return exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
		posting_date, account_id, amount_tc, amount_lc, amount_gc, tax_code) VALUES ($1,2026,$2,$3,$4,$5,3,'2026-03-10',$6,$7,$7,$8,nullif($9, ''))`,
		s.tenant, je, n, s.entity, ledger, account, amount, amount*9/10, code)
}

// header inserts an entry's header dated 2026-03-10, with that tax date
// as the ledger writes it, or with none as the release before T61c did.
func (s taxScene) header(tx pgx.Tx, taxDate bool) (uuid.UUID, error) {
	return s.headerIn(tx, s.ledger, taxDate)
}

// headerIn is header in a ledger.
func (s taxScene) headerIn(tx pgx.Tx, ledger uuid.UUID, taxDate bool) (uuid.UUID, error) {
	je := id()
	return je, exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
		document_number, document_date, posting_date, tax_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by)
		VALUES ($1,2026,$2,$3,$4,3,'SA',$5,'2026-03-10','2026-03-10',CASE WHEN $6 THEN '2026-03-10'::date END,'USD','USD','EUR',1,0.9,'ledger',$7)`,
		s.tenant, je, s.entity, ledger, je.String()[24:], taxDate, s.actor)
}

// taxedEntry is an entry of 105.00 on I5: 100.00 expense, 5.00 GST on the
// tax account, the bank; items written unless items is false.
func (s taxScene) taxedEntry(tx pgx.Tx, items bool) (uuid.UUID, error) {
	je, err := s.header(tx, true)
	if err == nil {
		err = s.taxLine(tx, je, 1, s.bank, -10500, "")
	}
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
			je, err := s.header(tx, true)
			if err == nil {
				err = s.taxLine(tx, je, 1, s.bank, -10500, "")
			}
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
		{"two components on one account", func(tx pgx.Tx) error {
			// The database is the backstop: the ledger refuses such a code
			// (ledger.tax_code_components), each component taking the
			// account's whole tax.
			for _, st := range []stmt{
				step(`INSERT INTO ledger.tax_code_definition (tenant_id, legal_entity_id, code, description, direction, kind, created_by, updated_by)
					VALUES ($1, $2, 'H2', 'GST + QST', 'input', 'standard', $3, $3)`, s.tenant, s.entity, s.actor),
				step(`INSERT INTO ledger.tax_component (tenant_id, legal_entity_id, code, component, account_key, recoverable_pct, account_id, created_by, updated_by)
					VALUES ($1, $2, 'H2', 'gst', 'input', 100, $3, $4, $4), ($1, $2, 'H2', 'qst', 'input', 100, $3, $4, $4)`, s.tenant, s.entity, s.tax, s.actor),
				step(`INSERT INTO ledger.tax_rate (tenant_id, id, legal_entity_id, code, component, valid_from, rate_pct, created_by)
					VALUES ($1, gen_random_uuid(), $2, 'H2', 'gst', '2026-01-01', 5, $3), ($1, gen_random_uuid(), $2, 'H2', 'qst', '2026-01-01', 10, $3)`,
					s.tenant, s.entity, s.actor),
			} {
				if err := exec(tx, st.sql, st.args...); err != nil {
					return err
				}
			}
			je, err := s.header(tx, true)
			if err == nil {
				err = s.taxLine(tx, je, 1, s.bank, -11500, "")
			}
			if err == nil {
				err = s.taxLine(tx, je, 2, s.expense, 10000, "H2")
			}
			if err == nil {
				err = s.taxLine(tx, je, 3, s.tax, 1500, "H2")
			}
			if err == nil {
				err = exec(tx, `SELECT ledger.write_tax_items($1, 2026::smallint, $2, '2026-03-10'::date, false)`, s.tenant, je)
			}
			return err
		}, "tax_item_lines"},
		{"items that differ in local currency", func(tx pgx.Tx) error {
			je, err := s.taxedEntry(tx, false)
			if err == nil {
				err = exec(tx, `INSERT INTO ledger.tax_item (tenant_id, fiscal_year, journal_entry_id, item_no, legal_entity_id, code, component, direction,
					kind, account_key, account_id, tax_date, tax_rate_id, rate_pct, recoverable_pct, base_tc, tax_tc, recoverable_tc, base_lc, tax_lc, recoverable_lc)
					VALUES ($1, 2026, $2, 1, $3, 'I5', 'gst', 'input', 'standard', 'input', $4, '2026-03-10', $5, 5, 100, 10000, 500, 500, 10000, 499, 499)`,
					s.tenant, je, s.entity, s.tax, s.gst2026)
			}
			return err
		}, "tax_item_lines"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, s.tenant, c.write), c.fails) })
	}
	t.Run("an entry of the release before, without items", func(t *testing.T) {
		mustApp(t, s.tenant, func(tx pgx.Tx) error {
			je, err := s.header(tx, false)
			if err == nil {
				err = s.taxLine(tx, je, 1, s.bank, -10500, "")
			}
			if err == nil {
				err = s.taxLine(tx, je, 2, s.expense, 10000, "I5")
			}
			if err == nil {
				err = s.taxLine(tx, je, 3, s.tax, 500, "I5")
			}
			return err
		})
	})
	t.Run("a code posting to no account, without items", func(t *testing.T) {
		mustApp(t, s.tenant, func(tx pgx.Tx) error {
			for _, st := range []stmt{
				step(`INSERT INTO ledger.tax_code_definition (tenant_id, legal_entity_id, code, description, direction, kind, created_by, updated_by)
					VALUES ($1, $2, 'P7', 'BC PST', 'input', 'standard', $3, $3)`, s.tenant, s.entity, s.actor),
				step(`INSERT INTO ledger.tax_component (tenant_id, legal_entity_id, code, component, account_key, recoverable_pct, created_by, updated_by)
					VALUES ($1, $2, 'P7', 'pst_bc', 'non_deductible', 0, $3, $3)`, s.tenant, s.entity, s.actor),
				step(`INSERT INTO ledger.tax_rate (tenant_id, id, legal_entity_id, code, component, valid_from, rate_pct, created_by)
					VALUES ($1, gen_random_uuid(), $2, 'P7', 'pst_bc', '2026-01-01', 7, $3)`, s.tenant, s.entity, s.actor),
			} {
				if err := exec(tx, st.sql, st.args...); err != nil {
					return err
				}
			}
			je, err := s.header(tx, true)
			if err == nil {
				err = s.taxLine(tx, je, 1, s.bank, -10700, "")
			}
			if err == nil {
				err = s.taxLine(tx, je, 2, s.expense, 10700, "P7")
			}
			if err == nil {
				err = exec(tx, `SELECT ledger.write_tax_items($1, 2026::smallint, $2, '2026-03-10'::date, false)`, s.tenant, je)
			}
			return err
		})
	})
	t.Run("a parallel ledger's entry", func(t *testing.T) {
		local := id()
		mustApp(t, s.tenant, func(tx pgx.Tx) error {
			return all(tx, step(`INSERT INTO ledger.ledger (tenant_id, id, code, name, is_leading, accounting_principle, created_by)
				VALUES ($1, $2, '2L', 'Local', false, 'HGB', $3)`, s.tenant, local, s.actor),
				step(`INSERT INTO ledger.company_ledger (tenant_id, legal_entity_id, ledger_id, is_leading, created_by) VALUES ($1, $2, $3, true, $5), ($1, $2, $4, false, $5)`,
					s.tenant, s.entity, s.ledger, local, s.actor),
				step(`INSERT INTO ledger.period_status (tenant_id, id, legal_entity_id, ledger_id, calendar_id, fiscal_year, period, status, updated_by)
					VALUES ($1, gen_random_uuid(), $2, $3, $4, 2026, 3, 'open', $5)`, s.tenant, s.entity, local, s.calendar, s.actor))
		})
		lines := func(tx pgx.Tx, ledger, je uuid.UUID) (err error) {
			for i, l := range []struct {
				account uuid.UUID
				amount  int64
				code    string
			}{{s.bank, -10500, ""}, {s.expense, 10000, "I5"}, {s.tax, 500, "I5"}} {
				if err == nil {
					err = s.taxLineIn(tx, ledger, je, i+1, l.account, l.amount, l.code)
				}
			}
			return err
		}
		entry := func(items bool) func(tx pgx.Tx) error {
			return func(tx pgx.Tx) error {
				je, err := s.headerIn(tx, local, true)
				if err == nil {
					err = lines(tx, local, je)
				}
				if err == nil && items {
					err = exec(tx, `SELECT ledger.write_tax_items($1, 2026::smallint, $2, '2026-03-10'::date, false)`, s.tenant, je)
				}
				return err
			}
		}
		// A copy of a leading ledger's entry carries its tax lines without
		// items (ADR-0045).
		mustApp(t, s.tenant, func(tx pgx.Tx) error {
			first, copied := id(), id()
			err := all(tx, step(`INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
				document_number, document_date, posting_date, tax_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by,
				posting_group_id) VALUES ($1,2026,$2,$3,$4,3,'SA','GROUP1','2026-03-10','2026-03-10','2026-03-10','USD','USD','EUR',1,0.9,'ledger',$5,$2)`,
				s.tenant, first, s.entity, s.ledger, s.actor),
				step(`INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
				document_date, posting_date, tax_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by, posting_group_id)
				SELECT tenant_id, fiscal_year, $2, legal_entity_id, $3, fiscal_period, document_type, document_number, document_date, posting_date, tax_date,
					currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by, id FROM ledger.journal_entry WHERE id = $1`,
					first, copied, local))
			if err == nil {
				err = lines(tx, s.ledger, first)
			}
			if err == nil {
				err = lines(tx, local, copied)
			}
			if err == nil {
				err = exec(tx, `SELECT ledger.write_tax_items($1, 2026::smallint, $2, '2026-03-10'::date, false)`, s.tenant, first)
			}
			return err
		})
		// Alone, it may carry no tax code (product owner, 2026-10-21), nor
		// items.
		failsOn(t, asApp(t, s.tenant, entry(false)), "tax_code_off_leading")
		failsOn(t, asApp(t, s.tenant, entry(true)), "tax_item_leading")
	})
	t.Run("an item of an entry committed before", func(t *testing.T) {
		failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.tax_item (tenant_id, fiscal_year, journal_entry_id, item_no, legal_entity_id, code, component, direction,
				kind, account_key, account_id, tax_date, tax_rate_id, rate_pct, recoverable_pct, base_tc, tax_tc, recoverable_tc, base_lc, tax_lc, recoverable_lc)
				VALUES ($1, 2026, $2, 9, $3, 'I5', 'gst', 'input', 'standard', 'input', $4, '2026-03-10', $5, 5, 100, 0, 1, 1, 0, 1, 1)`,
				s.tenant, entry, s.entity, s.tax, s.gst2026)
		}), "tax_item_lines")
	})

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
		del := "DELETE FROM ledger.tax_item WHERE journal_entry_id = $1"
		for _, change := range []func(tx pgx.Tx) error{report("tax_tc = 0"), func(tx pgx.Tx) error { return exec(tx, del, entry) }} {
			failsWith(t, asApp(t, s.tenant, change), "permission denied")
		}
		// Past the grants, as the owner, the trigger holds.
		for _, sql := range []string{"UPDATE ledger.tax_item SET tax_tc = 0 WHERE journal_entry_id = $1", del} {
			_, err := testDB.Exec(context.Background(), sql, entry)
			failsOn(t, err, "tax_item_fixed")
		}
	})
}

// TestTaxItemsBackfilled: 000144 writes the items of the entries posted
// before, from their lines at their posting date: a tax line on a
// component's former account is its tax as one on its current account is,
// the code's other lines its base, once per component; an entry without
// codes has none, nor has a parallel ledger's (ADR-0045); the rate check
// reads them.
func TestTaxItemsBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, 144)
	f := fixtureIDs()
	rate := id()
	var taxed, plain, local uuid.UUID
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
		if taxed, err = f.entry(tx, 2, "2026-02-10", line{account: f.bank, amount: -130}); err != nil {
			return err
		}
		for i, l := range []struct {
			account uuid.UUID
			amount  int64
		}{{f.expense, 100}, {f.tax, 20}, {f.bank, 10}} {
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
		if plain, err = f.entry(tx, 2, "2026-02-11", line{account: f.bank, amount: -5}, line{account: f.expense, amount: 5}); err != nil {
			return err
		}
		local, err = f.parallelTaxedEntry(tx)
		return err
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	var got string
	var n int
	if err := conn.QueryRow(context.Background(), `SELECT string_agg(code || '/' || component || ' ' || tax_date || ' ' || tax_tc || ' '
			|| recoverable_tc || ' ' || (account_id = $2)::text || ' ' || (tax_rate_id = $3)::text, '; ' ORDER BY tax_tc DESC)
			|| ', base ' || sum(base_tc) || ' on ' || count(*) FILTER (WHERE base_tc <> 0),
			(SELECT count(*) FROM ledger.tax_item WHERE journal_entry_id IN ($4, $5))
		FROM ledger.tax_item WHERE journal_entry_id = $1`, taxed, f.tax, rate, plain, local).Scan(&got, &n); err != nil {
		t.Fatal(err)
	}
	// The tax on the current account (100000) is the component's too; the
	// base is on one item only.
	if want := "V1/tax 2026-02-10 20 20 true true; V1/tax 2026-02-10 10 10 false true, base 100 on 1"; got != want {
		t.Errorf("items of the taxed entry: got %s, want %s", got, want)
	}
	if n != 0 {
		t.Errorf("items of an entry without codes and of a parallel ledger's: got %d, want none", n)
	}
	var posted bool
	if err := conn.QueryRow(context.Background(), `SELECT ledger.tax_code_posted($1, $2, 'V1', '2026-02-01', '2026-03-01')`, f.tenant, f.entity).Scan(&posted); err != nil {
		t.Fatal(err)
	}
	if !posted {
		t.Error("the backfilled posting: got not posted, want the rate in use")
	}
}

// parallelTaxedEntry gives the fixture's legal entity a parallel ledger 2L
// beside 0L and posts there an entry with V1 lines, as a posting's copy
// carries them.
func (f fixture) parallelTaxedEntry(tx pgx.Tx) (uuid.UUID, error) {
	local, je := id(), id()
	err := all(tx, step(`INSERT INTO ledger.ledger (tenant_id, id, code, name, is_leading, accounting_principle, created_by)
			VALUES ($1, $2, '2L', 'Local', false, 'HGB', $3)`, f.tenant, local, f.actor),
		step(`INSERT INTO ledger.company_ledger (tenant_id, legal_entity_id, ledger_id, is_leading, created_by) VALUES ($1, $2, $3, true, $5), ($1, $2, $4, false, $5)`,
			f.tenant, f.entity, f.ledger, local, f.actor),
		step(`INSERT INTO ledger.period_status (tenant_id, id, legal_entity_id, ledger_id, calendar_id, fiscal_year, period, status, updated_by)
			VALUES ($1, gen_random_uuid(), $2, $3, $4, 2026, 2, 'open', $5)`, f.tenant, f.entity, local, f.calendar, f.actor),
		step(`INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
			document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by)
			VALUES ($1,2026,$2,$3,$4,2,'SA','LOCAL1','2026-02-12','2026-02-12','USD','USD','EUR',1,0.9,'ledger',$5)`, f.tenant, je, f.entity, local, f.actor))
	for i, l := range []struct {
		account uuid.UUID
		amount  int64
		code    *string
	}{{f.bank, -120, nil}, {f.expense, 100, &[]string{"V1"}[0]}, {f.tax, 20, &[]string{"V1"}[0]}} {
		if err == nil {
			err = exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
				posting_date, account_id, amount_tc, amount_lc, amount_gc, tax_code) VALUES ($1,2026,$2,$3,$4,$5,2,'2026-02-12',$6,$7,$7,$8,$9)`,
				f.tenant, je, i+1, f.entity, local, l.account, l.amount, l.amount*9/10, l.code)
		}
	}
	return je, err
}

// TestTaxBaseRules: a line's declared base (000147, T61e) is in both
// currencies or neither, on a line with a tax code, a tax line of an
// import code; the component's item takes it as its base; a parked line
// declares one only with a code.
func TestTaxBaseRules(t *testing.T) {
	need(t)
	s := newTaxScene(t)
	mustApp(t, s.tenant, func(tx pgx.Tx) error {
		return all(tx, step(`INSERT INTO ledger.tax_code_definition (tenant_id, legal_entity_id, code, description, direction, kind, created_by, updated_by)
				VALUES ($1, $2, 'IC5', 'Import GST', 'input', 'import', $3, $3)`, s.tenant, s.entity, s.actor),
			step(`INSERT INTO ledger.tax_component (tenant_id, legal_entity_id, code, component, account_key, recoverable_pct, account_id, created_by, updated_by)
				VALUES ($1, $2, 'IC5', 'gst', 'input', 100, $3, $4, $4)`, s.tenant, s.entity, s.tax, s.actor),
			step(`INSERT INTO ledger.tax_rate (tenant_id, id, legal_entity_id, code, component, valid_from, rate_pct, created_by)
				VALUES ($1, gen_random_uuid(), $2, 'IC5', 'gst', '2026-01-01', 5, $3)`, s.tenant, s.entity, s.actor))
	})
	based := func(account uuid.UUID, code string, tc, lc *int64) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			je, err := s.header(tx, true)
			if err == nil {
				err = s.taxLine(tx, je, 1, s.bank, -500, "")
			}
			if err == nil {
				err = exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
					posting_date, account_id, amount_tc, amount_lc, amount_gc, tax_code, tax_base_tc, tax_base_lc)
					VALUES ($1,2026,$2,2,$3,$4,3,'2026-03-10',$5,500,500,450,nullif($6, ''),$7,$8)`, s.tenant, je, s.entity, s.ledger, account, code, tc, lc)
			}
			if err == nil {
				err = exec(tx, `SELECT ledger.write_tax_items($1, 2026::smallint, $2, '2026-03-10'::date, false)`, s.tenant, je)
			}
			return err
		}
	}
	base, baseLC := int64(10000), int64(9000)
	var got string
	mustApp(t, s.tenant, func(tx pgx.Tx) error {
		if err := based(s.tax, "IC5", &base, &baseLC)(tx); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT string_agg(code || ' ' || base_tc || '/' || base_lc || ' ' || tax_tc, '; ') FROM ledger.tax_item
			WHERE code = 'IC5'`).Scan(&got)
	})
	if want := "IC5 10000/9000 500"; got != want {
		t.Errorf("items: got %s, want %s", got, want)
	}
	for name, c := range map[string]struct {
		write func(tx pgx.Tx) error
		fails string
	}{
		"in one currency only": {based(s.tax, "IC5", &base, nil), "journal_line_tax_base"},
		"without a tax code":   {based(s.tax, "", &base, &baseLC), "journal_line_tax_base_import"},
		"not an import code":   {based(s.tax, "I5", &base, &baseLC), "journal_line_tax_base_import"},
		"not a tax line":       {based(s.expense, "IC5", &base, &baseLC), "journal_line_tax_base_import"},
		"a parked line without a code": {func(tx pgx.Tx) error {
			pe := id()
			return all(tx, step(`INSERT INTO ledger.parked_entry (tenant_id, id, legal_entity_id, ledger_id, document_type, document_date, posting_date,
					currency, created_by_kind, owner_id, created_by, updated_by) VALUES ($1, $2, $3, $4, 'SA', '2026-03-10', '2026-03-10', 'USD', 'human', $5, $5, $5)`,
				s.tenant, pe, s.entity, s.ledger, s.actor),
				step(`INSERT INTO ledger.parked_entry_line (tenant_id, parked_entry_id, line_no, account_id, amount, tax_base) VALUES ($1, $2, 1, $3, 500, 10000)`,
					s.tenant, pe, s.tax))
		}, "parked_entry_line_tax_base"},
	} {
		t.Run(name, func(t *testing.T) { failsOn(t, asApp(t, s.tenant, c.write), c.fails) })
	}
	// A reversal or copy repeats the original's account after the
	// component moved on: a former account takes the base too.
	mustApp(t, s.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `UPDATE ledger.tax_component SET account_id = $1, version = version + 1 WHERE code = 'IC5'`, s.bank); err != nil {
			return err
		}
		return based(s.tax, "IC5", &base, &baseLC)(tx)
	})
}

// TestLineJurisdictionRules: a taxed line names a jurisdiction exactly
// when its code's component levies its lines' subdivision, one of that
// component's country (000150, T61f), but for a reversal, which repeats
// the original's; a component levies its lines' subdivisions only as a
// country; the items are per jurisdiction.
func TestLineJurisdictionRules(t *testing.T) {
	need(t)
	s := newTaxScene(t)
	mustApp(t, s.tenant, func(tx pgx.Tx) error {
		return all(tx, step(`INSERT INTO ledger.tax_code_definition (tenant_id, legal_entity_id, code, description, direction, kind, created_by, updated_by)
				VALUES ($1, $2, 'S5', 'UAE standard', 'output', 'standard', $3, $3)`, s.tenant, s.entity, s.actor),
			step(`INSERT INTO ledger.tax_component (tenant_id, legal_entity_id, code, component, jurisdiction, jurisdiction_on_line, account_key,
				recoverable_pct, account_id, created_by, updated_by) VALUES ($1, $2, 'S5', 'vat', 'AE', true, 'output', 100, $3, $4, $4)`,
				s.tenant, s.entity, s.tax, s.actor),
			step(`INSERT INTO ledger.tax_rate (tenant_id, id, legal_entity_id, code, component, valid_from, rate_pct, created_by)
				VALUES ($1, gen_random_uuid(), $2, 'S5', 'vat', '2026-01-01', 5, $3)`, s.tenant, s.entity, s.actor))
	})
	line := func(tx pgx.Tx, je uuid.UUID, n int, account uuid.UUID, amount int64, code, jurisdiction string) error {
		return exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
			posting_date, account_id, amount_tc, amount_lc, amount_gc, tax_code, tax_jurisdiction)
			VALUES ($1,2026,$2,$3,$4,$5,3,'2026-03-10',$6,$7,$7,$8,nullif($9, ''),nullif($10, ''))`,
			s.tenant, je, n, s.entity, s.ledger, account, amount, amount*9/10, code, jurisdiction)
	}
	sale := func(jurisdiction string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			je, err := s.header(tx, true)
			for i, l := range []struct {
				account uuid.UUID
				amount  int64
				code    string
			}{{s.bank, 2100, ""}, {s.expense, -1000, "S5"}, {s.tax, -50, "S5"}, {s.expense, -1000, "S5"}, {s.tax, -50, "S5"}} {
				j := ""
				if l.code != "" {
					j = jurisdiction
					if i > 2 && jurisdiction == "AE-DU" {
						j = "AE-AZ"
					}
				}
				if err == nil {
					err = line(tx, je, i+1, l.account, l.amount, l.code, j)
				}
			}
			if err == nil {
				err = exec(tx, `SELECT ledger.write_tax_items($1, 2026::smallint, $2, '2026-03-10'::date, false)`, s.tenant, je)
			}
			return err
		}
	}
	mustApp(t, s.tenant, sale("AE-DU"))
	var got string
	mustApp(t, s.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT string_agg(jurisdiction || ' ' || base_tc || ' ' || tax_tc, '; ' ORDER BY item_no)
			FROM ledger.tax_item WHERE code = 'S5'`).Scan(&got)
	})
	if want := "AE-AZ -1000 -50; AE-DU -1000 -50"; got != want {
		t.Errorf("items: got %s, want %s", got, want)
	}
	parkedLine := func(code, jurisdiction string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			pe := id()
			return all(tx, step(`INSERT INTO ledger.parked_entry (tenant_id, id, legal_entity_id, ledger_id, document_type, document_date, posting_date,
					currency, created_by_kind, owner_id, created_by, updated_by) VALUES ($1, $2, $3, $4, 'SA', '2026-03-10', '2026-03-10', 'USD', 'human', $5, $5, $5)`,
				s.tenant, pe, s.entity, s.ledger, s.actor),
				step(`INSERT INTO ledger.parked_entry_line (tenant_id, parked_entry_id, line_no, account_id, amount, tax_code, tax_jurisdiction)
					VALUES ($1, $2, 1, $3, 500, nullif($4, ''), $5)`, s.tenant, pe, s.tax, code, jurisdiction))
		}
	}
	for name, c := range map[string]struct {
		write func(tx pgx.Tx) error
		fails string
	}{
		"none where the code levies the line's": {sale(""), "journal_line_tax_jurisdiction_code"},
		"another country's":                     {sale("CA-ON"), "journal_line_tax_jurisdiction_code"},
		"one the code does not levy": {func(tx pgx.Tx) error {
			je, err := s.header(tx, true)
			if err == nil {
				err = line(tx, je, 1, s.expense, 10000, "I5", "CA-BC")
			}
			return err
		}, "journal_line_tax_jurisdiction_code"},
		"one without a code": {func(tx pgx.Tx) error {
			je, err := s.header(tx, true)
			if err == nil {
				err = line(tx, je, 1, s.expense, 10000, "", "AE-DU")
			}
			return err
		}, "journal_line_tax_jurisdiction"},
		"not a subdivision": {func(tx pgx.Tx) error {
			je, err := s.header(tx, true)
			if err == nil {
				err = line(tx, je, 1, s.expense, -1000, "S5", "AE-dubai")
			}
			return err
		}, "journal_line_tax_jurisdiction"},
		"a parked line's without a code":    {parkedLine("", "AE-DU"), "parked_entry_line_tax_jurisdiction"},
		"a parked line's not a subdivision": {parkedLine("S5", "AE"), "parked_entry_line_tax_jurisdiction"},
		"a subdivision levying its lines'": {func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.tax_component SET jurisdiction = 'AE-DU', version = version + 1 WHERE code = 'S5'`)
		}, "tax_component_jurisdiction_on_line"},
	} {
		t.Run(name, func(t *testing.T) { failsOn(t, asApp(t, s.tenant, c.write), c.fails) })
	}
	// A reversal repeats the original's jurisdiction after the code changed.
	mustApp(t, s.tenant, func(tx pgx.Tx) error {
		orig, err := s.header(tx, true)
		if err == nil {
			err = exec(tx, `UPDATE ledger.tax_component SET jurisdiction_on_line = false, version = version + 1 WHERE code = 'S5'`)
		}
		rev := id()
		if err == nil {
			err = exec(tx, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type,
				document_number, document_date, posting_date, tax_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by,
				reversal_of_id, reversal_of_year) VALUES ($1,2026,$2,$3,$4,3,'SA','REV1','2026-03-10','2026-03-10','2026-03-10','USD','USD','EUR',1,0.9,'ledger',$5,$6,2026)`,
				s.tenant, rev, s.entity, s.ledger, s.actor, orig)
		}
		if err == nil {
			err = line(tx, rev, 1, s.tax, 50, "S5", "AE-DU")
		}
		if err == nil {
			err = line(tx, rev, 2, s.bank, -50, "", "")
		}
		if err == nil {
			err = exec(tx, `SELECT ledger.write_tax_items($1, 2026::smallint, $2, '2026-03-10'::date, false)`, s.tenant, rev)
		}
		if err == nil {
			err = line(tx, orig, 1, s.bank, 10, "", "")
		}
		if err == nil {
			err = line(tx, orig, 2, s.bank, -10, "", "")
		}
		return err
	})
}
