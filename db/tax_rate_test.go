package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// taxScene is a fixture with tax code I5 (definition, a GST component on
// the tax account and a non-deductible PST component) and the rates' ids.
type taxScene struct {
	fixture
	gst2026, gst2027, pst uuid.UUID
}

func newTaxScene(t *testing.T) taxScene {
	t.Helper()
	s := taxScene{fixture: newFixture(t), gst2026: id(), gst2027: id(), pst: id()}
	mustApp(t, s.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO ledger.tax_code_definition (tenant_id, legal_entity_id, code, description, direction, kind, category, created_by, updated_by)
			VALUES ($1, $2, 'I5', 'GST + PST', 'input', 'standard', 'S', $3, $3)`, s.tenant, s.entity, s.actor); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO ledger.tax_component (tenant_id, legal_entity_id, code, component, jurisdiction, account_key, recoverable_pct,
				account_id, created_by, updated_by)
			VALUES ($1, $2, 'I5', 'gst', 'CA', 'input', 100, $3, $4, $4), ($1, $2, 'I5', 'pst_bc', 'CA-BC', 'non_deductible', 0, NULL, $4, $4)`,
			s.tenant, s.entity, s.tax, s.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO ledger.tax_rate (tenant_id, id, legal_entity_id, code, component, valid_from, valid_to, rate_pct, created_by)
			VALUES ($1, $3, $2, 'I5', 'gst', '2026-01-01', '2026-06-01', 5, $6), ($1, $4, $2, 'I5', 'gst', '2026-06-01', NULL, 4, $6),
				($1, $5, $2, 'I5', 'pst_bc', '2026-01-01', '2026-04-01', 7, $6)`, s.tenant, s.entity, s.gst2026, s.gst2027, s.pst, s.actor)
	})
	return s
}

// postTaxed posts an entry with a line of the code on the date (in an
// open period of 2026), with its tax items at that tax date.
func (s taxScene) postTaxed(t *testing.T, code, date string, period int) {
	t.Helper()
	mustApp(t, s.tenant, func(tx pgx.Tx) error {
		je, err := s.entry(tx, period, date, line{account: s.bank, amount: -100})
		if err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
			posting_date, account_id, amount_tc, amount_lc, amount_gc, tax_code) VALUES ($1,2026,$2,2,$3,$4,$5,$6,$7,100,100,90,$8)`,
			s.tenant, je, s.entity, s.ledger, period, date, s.expense, code); err != nil {
			return err
		}
		// The items, as the ledger writes them (T61c): a posting is an item
		// of the code with its tax date.
		return exec(tx, `SELECT ledger.write_tax_items($1, 2026::smallint, $2, $3::date, false)`, s.tenant, je, date)
	})
}

// TestTaxCodeSchema: a definition's code, direction, kind and category
// are known values; a component's key, jurisdiction and account key are,
// a non-deductible component has no account and recovers nothing, and
// every other one has an account; a rate is 0 to below 100% and ends
// after it starts; and the database refuses changing what never changes.
func TestTaxCodeSchema(t *testing.T) {
	need(t)
	s := newTaxScene(t)
	definition := func(code, direction, kind, category string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.tax_code_definition (tenant_id, legal_entity_id, code, description, direction, kind, category, created_by, updated_by)
				VALUES ($1, $2, $3, 'X', $4, $5, nullif($6, ''), $7, $7)`, s.tenant, s.entity, code, direction, kind, category, s.actor)
		}
	}
	component := func(code, key, jurisdiction, accountKey string, recoverable int, account *uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.tax_component (tenant_id, legal_entity_id, code, component, jurisdiction, account_key, recoverable_pct,
					account_id, created_by, updated_by) VALUES ($1, $2, $3, $4, nullif($5, ''), $6, $7, $8, $9, $9)`,
				s.tenant, s.entity, code, key, jurisdiction, accountKey, recoverable, account, s.actor)
		}
	}
	rate := func(component, from string, to *string, pct float64) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.tax_rate (tenant_id, id, legal_entity_id, code, component, valid_from, valid_to, rate_pct, created_by)
				VALUES ($1, $2, $3, 'I5', $4, $5, $6, $7::numeric, $8)`, s.tenant, id(), s.entity, component, from, to, pct, s.actor)
		}
	}
	tax := &s.tax
	for _, c := range []struct {
		name  string
		write func(tx pgx.Tx) error
		fails string
	}{
		{"a lower-case code", definition("i6", "input", "standard", ""), "tax_code_definition_code_check"},
		{"another direction", definition("I6", "both", "standard", ""), "tax_code_definition_direction_check"},
		{"another kind", definition("I6", "input", "luxury", ""), "tax_code_definition_kind_check"},
		{"another category", definition("I6", "input", "standard", "X"), "tax_code_definition_category_check"},
		{"a component of no code", component("I9", "gst", "", "input", 100, tax), "tax_component_tenant_id_legal_entity_id_code_fkey"},
		{"an upper-case component", component("I5", "GST", "", "input", 100, tax), "tax_component_component_check"},
		{"a jurisdiction of no country", component("I5", "hst", "Canada", "input", 100, tax), "tax_component_jurisdiction_check"},
		{"another account key", component("I5", "hst", "", "expense", 100, tax), "tax_component_account_key_check"},
		{"a recoverable share above 100", component("I5", "hst", "", "input", 101, tax), "tax_component_recoverable_pct_check"},
		{"an input component without an account", component("I5", "hst", "", "input", 100, nil), "tax_component_account"},
		{"a non-deductible component with an account", component("I5", "hst", "", "non_deductible", 0, tax), "tax_component_account"},
		{"a non-deductible component recovering", component("I5", "hst", "", "non_deductible", 50, nil), "tax_component_non_deductible"},
		{"an account of no company", component("I5", "hst", "", "input", 100, new(id())), "tax_component_tenant_id_legal_entity_id_account_id_fkey"},
		{"a rate of no component", rate("hst", "2027-01-01", nil, 5), "tax_rate_tenant_id_legal_entity_id_code_component_fkey"},
		{"a rate of 100", rate("pst_bc", "2027-01-01", nil, 100), "tax_rate_rate_pct_check"},
		{"a negative rate", rate("pst_bc", "2027-01-01", nil, -1), "tax_rate_rate_pct_check"},
		{"an end before the start", rate("pst_bc", "2027-01-01", new("2027-01-01"), 7), "tax_rate_check"},
		{"overlapping rates", rate("gst", "2026-12-01", nil, 3), "tax_rate_no_overlap"},
		{"an end without who", func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.tax_rate SET valid_to = '2026-03-01', ended_at = now() WHERE id = $1`, s.pst)
		}, "tax_rate_ended"},
		{"a withdrawal without who", func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.tax_rate SET withdrawn_at = now() WHERE id = $1`, s.pst)
		}, "tax_rate_withdrawn"},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, s.tenant, c.write), c.fails) })
	}
	for _, c := range []struct{ name, sql string }{
		{"a direction", `UPDATE ledger.tax_code_definition SET direction = 'output', version = version + 1 WHERE code = 'I5'`},
		{"an account key", `UPDATE ledger.tax_component SET account_key = 'self_input', version = version + 1 WHERE component = 'gst'`},
		{"the former accounts", `UPDATE ledger.tax_component SET former_account_ids = '{}', version = version + 1 WHERE component = 'gst'`},
		{"a rate's value", `UPDATE ledger.tax_rate SET rate_pct = 6 WHERE component = 'gst'`},
		{"a rate's start", `UPDATE ledger.tax_rate SET valid_from = '2026-02-01' WHERE component = 'gst'`},
		{"a rate, deleted", `DELETE FROM ledger.tax_rate WHERE component = 'gst'`},
	} {
		t.Run("changing "+c.name, func(t *testing.T) {
			failsWith(t, asApp(t, s.tenant, func(tx pgx.Tx) error { return exec(tx, c.sql) }), "permission denied")
		})
	}
}

// TestTaxComponentAccountHistory: a component's account changes in place
// and its former accounts are kept, once each.
func TestTaxComponentAccountHistory(t *testing.T) {
	need(t)
	s := newTaxScene(t)
	move := func(to uuid.UUID) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.tax_component SET account_id = $1, version = version + 1, updated_by = $2 WHERE component = 'gst'`, to, s.actor)
		}
	}
	mustApp(t, s.tenant, move(s.bank))
	mustApp(t, s.tenant, move(s.tax))
	mustApp(t, s.tenant, move(s.bank))
	var former []uuid.UUID
	if err := testDB.QueryRow(context.Background(), `SELECT former_account_ids FROM ledger.tax_component WHERE tenant_id = $1 AND component = 'gst'`,
		s.tenant).Scan(&former); err != nil {
		t.Fatal(err)
	}
	if len(former) != 2 || former[0] != s.tax || former[1] != s.bank {
		t.Errorf("former accounts: got %v, want [tax bank]", former)
	}
}

// TestTaxRatesInUse: once a posting of the code falls in a rate's dates,
// the rate is never withdrawn and its end never moves past the posting;
// a new rate never covers a posted date; an end moving over no posting,
// and withdrawing an unused rate, are allowed; a withdrawn rate never
// changes; and a withdrawn rate no longer blocks a new one of its dates.
func TestTaxRatesInUse(t *testing.T) {
	need(t)
	s := newTaxScene(t)
	s.postTaxed(t, "I5", "2026-03-15", 3) // in gst2026 and pst
	s.postTaxed(t, "I5", "2026-05-10", 5) // in gst2026, after pst ended
	update := func(sql string, args ...any) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error { return exec(tx, sql, append([]any{s.actor}, args...)...) }
	}
	end := `UPDATE ledger.tax_rate SET valid_to = $3, ended_at = now(), ended_by = $1 WHERE id = $2`
	withdraw := `UPDATE ledger.tax_rate SET withdrawn_at = now(), withdrawn_by = $1 WHERE id = $2`
	for _, c := range []struct {
		name  string
		write func(tx pgx.Tx) error
	}{
		{"withdrawing a used rate", update(withdraw, s.gst2026)},
		{"ending a used rate before its posting", update(end, s.gst2026, "2026-04-01")},
		{"an ended rate extended over a posting", update(end, s.pst, "2026-06-01")},
		{"an ended rate made open over a posting", update(end, s.pst, nil)},
		{"a new rate over a posted date", func(tx pgx.Tx) error {
			if err := exec(tx, `INSERT INTO ledger.tax_component (tenant_id, legal_entity_id, code, component, account_key, recoverable_pct, account_id,
					created_by, updated_by) VALUES ($1, $2, 'I5', 'extra', 'input', 100, $3, $4, $4)`, s.tenant, s.entity, s.tax, s.actor); err != nil {
				return err
			}
			return exec(tx, `INSERT INTO ledger.tax_rate (tenant_id, id, legal_entity_id, code, component, valid_from, rate_pct, created_by)
				VALUES ($1, $2, $3, 'I5', 'extra', '2026-05-01', 1, $4)`, s.tenant, id(), s.entity, s.actor)
		}},
	} {
		t.Run(c.name, func(t *testing.T) { failsOn(t, asApp(t, s.tenant, c.write), "tax_rate_in_use") })
	}
	mustApp(t, s.tenant, update(end, s.pst, "2026-05-01"))     // moves over no posting
	mustApp(t, s.tenant, update(end, s.gst2027, "2027-01-01")) // a future rate ends
	mustApp(t, s.tenant, update(withdraw, s.gst2027))          // and is withdrawn, unused
	failsOn(t, asApp(t, s.tenant, update(end, s.gst2027, nil)), "tax_rate_withdrawn_fixed")
	mustApp(t, s.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO ledger.tax_rate (tenant_id, id, legal_entity_id, code, component, valid_from, rate_pct, created_by)
			VALUES ($1, $2, $3, 'I5', 'gst', '2026-06-01', 3.5, $4)`, s.tenant, id(), s.entity, s.actor)
	})
}

// TestTaxRatesBackfilled: each code of ledger.tax_code before 000124
// becomes a definition with one component, "tax", on its latest account,
// the earlier accounts its former ones, and a rate per validity row; the
// copy is not refused for the postings that used them.
func TestTaxRatesBackfilled(t *testing.T) {
	conn := emptyDatabase(t)
	migrateBefore(t, conn, 124)
	f := fixtureIDs()
	inTenant(t, conn, f.tenant, func(tx pgx.Tx) error {
		if err := f.seed(tx); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO ledger.tax_code (tenant_id, legal_entity_id, code, description, direction, rate_pct, account_id, valid_from, valid_to,
				created_by, ended_at, ended_by)
			VALUES ($1, $2, 'V1', 'Old', 'input', 19, $3, '2026-01-01', '2026-03-01', $4, now(), $4),
				($1, $2, 'V1', 'Input 16', 'input', 16, $5, '2026-03-01', NULL, $4, NULL, NULL)`, f.tenant, f.entity, f.tax, f.actor, f.expense); err != nil {
			return err
		}
		je, err := f.entry(tx, 2, "2026-02-10", line{account: f.bank, amount: -100})
		if err != nil {
			return err
		}
		return exec(tx, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
			posting_date, account_id, amount_tc, amount_lc, amount_gc, tax_code) VALUES ($1,2026,$2,2,$3,$4,2,'2026-02-10',$5,100,100,90,'V1')`,
			f.tenant, je, f.entity, f.ledger, f.expense)
	})
	if _, err := Migrate(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	var got string
	var former []uuid.UUID
	if err := conn.QueryRow(context.Background(), `SELECT d.description || ' ' || d.direction || ' ' || d.kind || ' ' || c.component || ' ' || c.account_key
			|| ' ' || trim_scale(c.recoverable_pct) || ' ' || (c.account_id = $2)::text || ' ' || (SELECT string_agg(trim_scale(r.rate_pct) || ' '
			|| r.valid_from || ' ' || coalesce(r.valid_to::text, 'open') || ' ' || (r.ended_by IS NOT NULL)::text, ', ' ORDER BY r.valid_from)
			FROM ledger.tax_rate r WHERE r.tenant_id = c.tenant_id AND r.code = c.code), c.former_account_ids
		FROM ledger.tax_code_definition d JOIN ledger.tax_component c ON c.tenant_id = d.tenant_id AND c.code = d.code
		WHERE d.tenant_id = $1`, f.tenant, f.expense).Scan(&got, &former); err != nil {
		t.Fatal(err)
	}
	if want := "Input 16 input standard tax input 100 true 19 2026-01-01 2026-03-01 true, 16 2026-03-01 open false"; got != want {
		t.Errorf("V1: got %s, want %s", got, want)
	}
	if len(former) != 1 || former[0] != f.tax {
		t.Errorf("former accounts: got %v, want [%s]", former, f.tax)
	}
}
