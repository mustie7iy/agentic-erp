package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// expenseScene is a tenant's expense report ER1 of an employee in a
// company, entered.
type expenseScene struct{ tenant, actor, company, employee, report uuid.UUID }

const (
	expenseHeader = `INSERT INTO payables.expense_report (tenant_id, id, document_number, legal_entity_id, employee_id, report_date, currency,
		description, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, '2026-03-20', 'EUR', $6, $7, $8, $8)`
	expenseLineSQL = `INSERT INTO payables.expense_report_line (tenant_id, expense_report_id, line_no, expense_date, expense_type, account_id,
		amount, tax_code, tax_amount, description) VALUES ($1, $2, $3, '2026-03-18', $4, gen_random_uuid(), $5, $6, $7, $8)`
	postExpense = `UPDATE payables.expense_report SET status = 'posted', posting_date = '2026-03-21', journal_entry_year = 2026,
		journal_entry_id = gen_random_uuid(), payment_term_code = 'NT30', net_due_date = '2026-04-19', version = version + 1 WHERE id = $1`
)

func (s expenseScene) header(number string, description any, status string) stmt {
	return step(expenseHeader, s.tenant, s.report, number, s.company, s.employee, description, status, s.actor)
}

func (s expenseScene) entered() stmt { return s.header("ER1", "Trip", "entered") }

func (s expenseScene) line(no int, expenseType string, amount int, code any, tax int, text any) stmt {
	return step(expenseLineSQL, s.tenant, s.report, no, expenseType, amount, code, tax, text)
}

// TestExpenseReportRules (T87a, migration 000308; ADR-0074): a report is
// numbered once per company, entered first and posted once, naming its
// entry, posting date and terms then and not before; posting is its only
// change, for the tables' owner too, and it is never deleted. Its lines
// have a number from 1 to 999, a type code, a gross above zero and the
// tax it holds below the gross, none without a code; they are never
// changed or deleted. Texts are not blank and at most 200 characters.
func TestExpenseReportRules(t *testing.T) {
	need(t)
	owner := step(`SET LOCAL ROLE erp_owner`)
	// The triggers off, as the database's owner: what the CHECK constraints
	// refuse on their own, behind the lifecycle trigger.
	replica := []stmt{step(`RESET ROLE`), step(`SET LOCAL session_replication_role = replica`)}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s expenseScene) error
		want string // empty: allowed
	}{
		{"entered with its lines, then posted", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(1, "TRAVEL", 11900, "V1", 1900, "Train"), s.line(2, "MEALS", 5000, nil, 0, nil), step(postExpense, s.report))
		}, ""},
		{"a number issued twice in the company", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), step(expenseHeader, s.tenant, id(), "ER1", s.company, s.employee, nil, "entered", s.actor))
		}, "expense_report_number_once"},
		{"an empty number", func(tx pgx.Tx, s expenseScene) error { return all(tx, s.header("", nil, "entered")) }, "expense_report_document_number_check"},
		{"a number of 41 characters", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.header(strings.Repeat("1", 41), nil, "entered"))
		}, "expense_report_document_number_check"},
		{"a blank description", func(tx pgx.Tx, s expenseScene) error { return all(tx, s.header("ER1", " ", "entered")) }, "expense_report_description_check"},
		{"a description of 201 characters", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.header("ER1", strings.Repeat("x", 201), "entered"))
		}, "expense_report_description_check"},
		{"an unknown status", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, append(replica, s.header("ER1", nil, "paid"))...)
		},
			"expense_report_status_check"},
		{"entered as posted", func(tx pgx.Tx, s expenseScene) error { return all(tx, s.header("ER1", nil, "posted")) }, "an expense report is entered first"},
		{"posted without its entry, the triggers off", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, append(replica, s.header("ER1", nil, "posted"))...)
		}, "expense_report_posted"},
		{"entered as posted with its entry, by the tables' owner", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, owner, step(`INSERT INTO payables.expense_report (tenant_id, id, document_number, legal_entity_id, employee_id, report_date,
				currency, status, posting_date, journal_entry_year, journal_entry_id, payment_term_code, net_due_date, created_by, updated_by)
				VALUES ($1, $2, 'ER1', $3, $4, '2026-03-20', 'EUR', 'posted', '2026-03-21', 2026, gen_random_uuid(), 'NT30', '2026-04-19', $5, $5)`,
				s.tenant, s.report, s.company, s.employee, s.actor))
		}, "an expense report is entered first"},
		{"posted without its terms", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), step(`UPDATE payables.expense_report SET status = 'posted', posting_date = '2026-03-21', journal_entry_year = 2026,
				journal_entry_id = gen_random_uuid(), version = version + 1 WHERE id = $1`, s.report))
		}, "expense_report_posted"},
		{"an entry without being posted, the triggers off", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, append([]stmt{s.entered()}, append(replica,
				step(`UPDATE payables.expense_report SET journal_entry_id = gen_random_uuid(), journal_entry_year = 2026, posting_date = '2026-03-21',
					payment_term_code = 'NT30', net_due_date = '2026-04-19' WHERE id = $1`, s.report))...)...)
		}, "expense_report_posted"},
		{"posted twice", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), step(postExpense, s.report), step(postExpense, s.report))
		}, "only an entered report changes"},
		{"an entered report's version moved without posting it", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), step(`UPDATE payables.expense_report SET version = version + 1 WHERE id = $1`, s.report))
		}, "only an entered report changes"},
		{"a posted report entered again, by the tables' owner", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), step(postExpense, s.report), owner, step(`UPDATE payables.expense_report SET status = 'entered', posting_date = NULL,
				journal_entry_year = NULL, journal_entry_id = NULL, payment_term_code = NULL, net_due_date = NULL WHERE id = $1`, s.report))
		}, "only an entered report changes"},
		{"the employee changed", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), step(`UPDATE payables.expense_report SET employee_id = gen_random_uuid()`))
		}, "permission denied"},
		{"a report deleted", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), step(`DELETE FROM payables.expense_report`))
		}, "permission denied"},
		{"a line numbered zero", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(0, "TRAVEL", 100, nil, 0, nil))
		}, "expense_report_line_line_no_check"},
		{"a line numbered 1000", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(1000, "TRAVEL", 100, nil, 0, nil))
		}, "expense_report_line_line_no_check"},
		{"a line numbered twice", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(1, "TRAVEL", 100, nil, 0, nil), s.line(1, "MEALS", 100, nil, 0, nil))
		}, "expense_report_line_pkey"},
		{"a type code in lower case", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(1, "travel", 100, nil, 0, nil))
		}, "expense_report_line_type_check"},
		{"a gross of zero", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(1, "TRAVEL", 0, nil, 0, nil))
		}, "expense_report_line_amount_positive"},
		{"tax without a code", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(1, "TRAVEL", 100, nil, 1, nil))
		}, "expense_report_line_tax"},
		{"tax of the whole gross", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(1, "TRAVEL", 100, "V1", 100, nil))
		}, "expense_report_line_tax"},
		{"negative tax", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(1, "TRAVEL", 100, "V1", -1, nil))
		}, "expense_report_line_tax"},
		{"a blank line text", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(1, "TRAVEL", 100, nil, 0, " "))
		}, "expense_report_line_description_check"},
		{"a line text of 201 characters", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(1, "TRAVEL", 100, nil, 0, strings.Repeat("x", 201)))
		}, "expense_report_line_description_check"},
		{"a line of no report", func(tx pgx.Tx, s expenseScene) error { return all(tx, s.line(1, "TRAVEL", 100, nil, 0, nil)) }, "expense_report_line_report_fkey"},
		{"a line changed", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(1, "TRAVEL", 100, nil, 0, nil), step(`UPDATE payables.expense_report_line SET amount = 200`))
		}, "permission denied"},
		{"a line deleted", func(tx pgx.Tx, s expenseScene) error {
			return all(tx, s.entered(), s.line(1, "TRAVEL", 100, nil, 0, nil), step(`DELETE FROM payables.expense_report_line`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := expenseScene{tenant: id(), actor: id(), company: id(), employee: id(), report: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return c.run(tx, s) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("another tenant sees none", func(t *testing.T) {
		s := expenseScene{tenant: id(), actor: id(), company: id(), employee: id(), report: id()}
		err := asApp(t, s.tenant, func(tx pgx.Tx) error {
			if err := all(tx, s.entered(), s.line(1, "TRAVEL", 100, nil, 0, nil), step(`SELECT set_config('app.tenant_id', $1, true)`, id().String())); err != nil {
				return err
			}
			var reports, lines int
			if err := tx.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM payables.expense_report), (SELECT count(*) FROM payables.expense_report_line)`).
				Scan(&reports, &lines); err != nil {
				return err
			}
			if reports != 0 || lines != 0 {
				return fmt.Errorf("another tenant reads %d reports and %d lines, want none", reports, lines)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
