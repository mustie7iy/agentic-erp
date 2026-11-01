package db

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Entry templates (ADR-0056, migrations 000183 and 000184): the rules the
// database keeps for a template and the entries parked from it.

// template saves an active template of f, owned by f.actor, with two
// lines, and returns its id; schedule is its schedule's columns' SQL
// values, or empty for none.
func (f fixture) template(t *testing.T, name, schedule string) uuid.UUID {
	t.Helper()
	tpl := id()
	cols, vals := "", ""
	if schedule != "" {
		cols, vals = ", frequency, day_of_month, starts_on, next_due_on", ", "+schedule
	}
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO ledger.entry_template (tenant_id, id, legal_entity_id, name, ledger_id, document_type, currency,
			created_by_kind, owner_id, created_by, updated_by`+cols+`) VALUES ($1,$2,$3,$4,$5,'SA','USD','human',$6,$6,$6`+vals+`)`,
			f.tenant, tpl, f.entity, name, f.ledger, f.actor); err != nil {
			return err
		}
		return exec(tx, `INSERT INTO ledger.entry_template_line (tenant_id, template_id, line_no, account_id, amount)
			VALUES ($1,$2,1,$3,100), ($1,$2,2,$4,-100)`, f.tenant, tpl, f.expense, f.bank)
	})
	return tpl
}

// changeTemplate updates template tpl as the application, setting set.
func (f fixture) changeTemplate(t *testing.T, tpl uuid.UUID, set string, args ...any) error {
	t.Helper()
	return asApp(t, f.tenant, func(tx pgx.Tx) error {
		return exec(tx, `UPDATE ledger.entry_template SET `+set+`, version = version + 1 WHERE id = $1`, append([]any{tpl}, args...)...)
	})
}

func TestEntryTemplateRules(t *testing.T) {
	need(t)
	f := newFixture(t)
	for _, c := range []struct {
		name string
		set  string
		want string // empty: allowed
	}{
		{"renamed", "name = 'Rent 2'", ""},
		{"scheduled", "frequency = 'monthly', day_of_month = 31, starts_on = '2026-01-01', next_due_on = '2026-01-31'", ""},
		{"half a schedule", "frequency = 'monthly'", "entry_template_schedule"},
		{"an end and a count", "frequency = 'yearly', day_of_month = 1, starts_on = '2026-01-01', ends_on = '2027-01-01', occurrences = 2", "entry_template_schedule"},
		{"an end before the start", "frequency = 'yearly', day_of_month = 1, starts_on = '2026-01-01', ends_on = '2025-01-01'", "entry_template_schedule"},
		{"a next date without a schedule", "next_due_on = '2026-01-01'", "entry_template_schedule"},
		{"a weekly schedule", "frequency = 'weekly', day_of_month = 1, starts_on = '2026-01-01'", "entry_template_frequency_check"},
		{"half a refusal", "refused_on = '2026-01-01'", "entry_template_refusal"},
		{"another owner", "owner_id = gen_random_uuid()", "owner of entry template"},
		{"another company", "legal_entity_id = gen_random_uuid()", "do not change"},
		{"another creator", "created_by = gen_random_uuid()", "do not change"},
		{"a blank name", "name = '  '", "entry_template_name_check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			expect(t, f.changeTemplate(t, f.template(t, "Rent "+c.name, ""), c.set), c.want)
		})
	}
	t.Run("a name once per company", func(t *testing.T) {
		f.template(t, "Insurance", "")
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.entry_template (tenant_id, id, legal_entity_id, name, ledger_id, document_type, currency, created_by_kind,
				owner_id, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, 'Insurance', $3, 'SA', 'USD', 'human', $4, $4, $4)`,
				f.tenant, f.entity, f.ledger, f.actor)
		}), "entry_template_name_once")
	})
	t.Run("a person owns what they save", func(t *testing.T) {
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.entry_template (tenant_id, id, legal_entity_id, name, ledger_id, document_type, currency, created_by_kind,
				owner_id, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, 'Unowned', $3, 'SA', 'USD', 'human', NULL, $4, $4)`,
				f.tenant, f.entity, f.ledger, f.actor)
		}), "entry_template_person_owns")
	})
	t.Run("retired for good, its lines with it", func(t *testing.T) {
		tpl := f.template(t, "Retired", "")
		expect(t, f.changeTemplate(t, tpl, "status = 'retired'"), "")
		expect(t, f.changeTemplate(t, tpl, "name = 'Again'"), "is retired")
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.entry_template_line SET amount = 200 WHERE template_id = $1 AND line_no = 1`, tpl)
		}), "cannot change")
	})
}

// occurrence parks an entry of f from template tpl for date, as kind.
func (f fixture) occurrence(tx pgx.Tx, tpl uuid.UUID, date any, kind string, owner *uuid.UUID) error {
	return exec(tx, `INSERT INTO ledger.parked_entry (tenant_id, id, legal_entity_id, ledger_id, document_type, document_date, posting_date,
		currency, created_by_kind, owner_id, created_by, updated_by, template_id, occurrence_date)
		VALUES ($1, gen_random_uuid(), $2, $3, 'SA', '2026-01-01', '2026-01-01', 'USD', $4, $5, $6, $6, $7, $8)`,
		f.tenant, f.entity, f.ledger, kind, owner, f.actor, tpl, date)
}

func TestParkedEntryOccurrences(t *testing.T) {
	need(t)
	f := newFixture(t)
	tpl := f.template(t, "Monthly", "'monthly', 1, '2026-01-01', '2026-01-01'")
	owner := &f.actor
	for _, c := range []struct {
		name  string
		date  any
		kind  string
		owner *uuid.UUID
		want  string
	}{
		{"the ERP's, owned", "2026-01-01", "system", owner, ""},
		{"the same date again", "2026-01-01", "system", owner, "parked_entry_occurrence_once"},
		{"the ERP's, unowned", "2026-02-01", "system", nil, "parked_entry_occurrence"},
		{"a date a person parked", "2026-03-01", "human", owner, "parked_entry_occurrence"},
		{"the ERP's without a date", nil, "system", owner, "parked_entry_occurrence"},
		{"a person's use of it", nil, "human", owner, ""},
		{"an unknown kind", nil, "robot", owner, "parked_entry_created_by_kind"},
	} {
		t.Run(c.name, func(t *testing.T) {
			expect(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return f.occurrence(tx, tpl, c.date, c.kind, c.owner) }), c.want)
		})
	}
	t.Run("an unknown template", func(t *testing.T) {
		failsWith(t, asApp(t, f.tenant, func(tx pgx.Tx) error { return f.occurrence(tx, id(), "2026-04-01", "system", owner) }), "parked_entry_template_fkey")
	})
	t.Run("the template and date are kept", func(t *testing.T) {
		var pe uuid.UUID
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT id FROM ledger.parked_entry WHERE template_id = $1 AND occurrence_date = '2026-01-01'`, tpl).Scan(&pe)
		})
		expect(t, f.change(t, pe, "occurrence_date = '2026-05-01'"), "does not change")
		expect(t, f.change(t, pe, "template_id = NULL, occurrence_date = NULL"), "does not change")
	})
}
