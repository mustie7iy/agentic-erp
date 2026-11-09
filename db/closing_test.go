package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Period closes (T64b, migration 000194): one close per company and
// regular period, starting open, its company, period and template version
// fixed; the ledgers it covers written once; its tasks starting open,
// moving open <-> done with who completed them and when set exactly while
// done (and the note only then), the completion never rewritten while it
// stands, a check task naming its check and a manual one none, never
// depending on itself, and what the template gave them fixed. Nothing is
// deleted, and every change moves the row's version. A check task's result
// (T64c, migration 000196) is recorded whole, on a check task only, never
// at its creation and never removed; a check task is done only while its
// latest check passed.
func TestPeriodCloseRules(t *testing.T) {
	need(t)
	const (
		closeRow = `INSERT INTO closing.period_close (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, period_end, config_version, status,
			created_by, updated_by) VALUES ($1, $2, $3, 2026, $4, '2026-01-31', 1, $5, $6, $6)`
		taskRow = `INSERT INTO closing.close_task (tenant_id, close_id, code, seq, name, kind, check_kind, due_on, depends_on, status, completed_by,
			completed_at, note, created_by, updated_by) VALUES ($1, $2, $3, $4, 'A task', $5, $6, '2026-02-03', $7, $8, $9, $10, $11, $12, $12)`
		ledgerRow = `INSERT INTO closing.close_ledger (tenant_id, close_id, ledger_id, period_id) VALUES ($1, $2, $3, $4)`
	)
	type scene struct{ tenant, actor, entity, close uuid.UUID }
	open := func(s scene) stmt { return step(closeRow, s.tenant, s.close, s.entity, 1, "open", s.actor) }
	task := func(s scene, code string, seq int) stmt {
		return step(taskRow, s.tenant, s.close, code, seq, "manual", nil, []string{}, "open", nil, nil, nil, s.actor)
	}
	check := func(s scene) stmt {
		return step(taskRow, s.tenant, s.close, "NP", 1, "check", "no_parked_entries", []string{}, "open", nil, nil, nil, s.actor)
	}
	// checked records a check's result, passed or not, and sets the status,
	// as :check does.
	checked := func(passed, status string) stmt {
		return step(`UPDATE closing.close_task SET checked_at = now(), checked_by = updated_by, check_passed = $1::boolean, check_result = '{"summary": "x"}',
			status = $2, completed_by = CASE WHEN $2 = 'done' THEN updated_by END, completed_at = CASE WHEN $2 = 'done' THEN now() END,
			version = version + 1 WHERE code = 'NP'`, passed, status)
	}
	done := func(code string) stmt {
		return step(`UPDATE closing.close_task SET status = 'done', completed_by = updated_by, completed_at = now(), note = 'Done', version = version + 1
			WHERE code = $1`, code)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a close with its ledger and task", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(ledgerRow, s.tenant, s.close, id(), id()), task(s, "AP", 1))
		}, ""},
		{"a second close of the period", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(closeRow, s.tenant, id(), s.entity, 1, "open", s.actor))
		}, "period_close_once"},
		{"a close of a special period", func(tx pgx.Tx, s scene) error {
			return exec(tx, closeRow, s.tenant, s.close, s.entity, 13, "open", s.actor)
		}, "period_close_regular"},
		{"a close signed off at once", func(tx pgx.Tx, s scene) error {
			return exec(tx, closeRow, s.tenant, s.close, s.entity, 1, "signed_off", s.actor)
		}, "a period close starts open"},
		{"a close signed off and reopened", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(`UPDATE closing.period_close SET status = 'signed_off', version = version + 1`),
				step(`UPDATE closing.period_close SET status = 'open', version = version + 1`))
		}, ""},
		{"a close's period changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(`UPDATE closing.period_close SET fiscal_period = 2, version = version + 1`))
		}, "changes only its status"},
		{"a close changed without its version", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(`UPDATE closing.period_close SET status = 'signed_off'`))
		}, "optimistic lock"},
		{"a close deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(`DELETE FROM closing.period_close`))
		}, "permission denied"},
		{"a covered ledger changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(ledgerRow, s.tenant, s.close, id(), id()), step(`UPDATE closing.close_ledger SET period_id = gen_random_uuid()`))
		}, "permission denied"},
		{"a task done and reopened", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AP", 1), done("AP"), step(`UPDATE closing.close_task SET status = 'open', completed_by = NULL,
				completed_at = NULL, note = NULL, version = version + 1`))
		}, ""},
		{"a task assigned", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AP", 1), step(`UPDATE closing.close_task SET assignee_id = $1, version = version + 1`, s.actor))
		}, ""},
		{"a task created done", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(taskRow, s.tenant, s.close, "AP", 1, "manual", nil, []string{}, "done", s.actor, "2026-02-03T10:00:00Z", nil, s.actor))
		}, "a close task starts open"},
		{"a task done without who did it", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AP", 1), step(`UPDATE closing.close_task SET status = 'done', completed_at = now(), version = version + 1`))
		}, "close_task_completed"},
		{"a task reopened keeping who did it", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AP", 1), done("AP"), step(`UPDATE closing.close_task SET status = 'open', completed_at = NULL, note = NULL,
				version = version + 1`))
		}, "close_task_completed"},
		{"an open task with a note", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AP", 1), step(`UPDATE closing.close_task SET note = 'Early', version = version + 1`))
		}, "keeps its completion while it is open"},
		{"a task created open with a note", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(taskRow, s.tenant, s.close, "AP", 1, "manual", nil, []string{}, "open", nil, nil, "Early", s.actor))
		}, "close_task_completed"},
		{"a completion rewritten", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AP", 1), done("AP"), step(`UPDATE closing.close_task SET completed_by = gen_random_uuid(), version = version + 1`))
		}, "keeps its completion while it is done"},
		{"a task's due date changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AP", 1), step(`UPDATE closing.close_task SET due_on = '2026-02-10', version = version + 1`))
		}, "changes only its assignee, status and check result"},
		{"a manual task with a check", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(taskRow, s.tenant, s.close, "AP", 1, "manual", "no_parked_entries", []string{}, "open", nil, nil, nil, s.actor))
		}, "close_task_check_kind"},
		{"a check task without one", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(taskRow, s.tenant, s.close, "AP", 1, "check", nil, []string{}, "open", nil, nil, nil, s.actor))
		}, "close_task_check_kind"},
		{"a task depending on itself", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(taskRow, s.tenant, s.close, "AP", 1, "manual", nil, []string{"AP"}, "open", nil, nil, nil, s.actor))
		}, "close_task_depends_on"},
		{"two tasks in one place", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AP", 1), task(s, "TB", 1))
		}, "close_task_seq_once"},
		{"a task of no close", func(tx pgx.Tx, s scene) error {
			return all(tx, task(s, "AP", 1))
		}, "close_task_tenant_id_close_id_fkey"},
		{"a task deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AP", 1), step(`DELETE FROM closing.close_task`))
		}, "permission denied"},
		{"a check failing, then passing, done, reopened and checked again", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), check(s), checked("false", "open"), checked("true", "done"),
				step(`UPDATE closing.close_task SET status = 'open', completed_by = NULL, completed_at = NULL, version = version + 1`), checked("true", "done"))
		}, ""},
		{"a check task done by hand", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), check(s), done("NP"))
		}, "close_task_check_passed"},
		{"a check task done on a failing check", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), check(s), checked("false", "done"))
		}, "close_task_check_passed"},
		{"a check result on a manual task", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AP", 1), step(`UPDATE closing.close_task SET checked_at = now(), checked_by = updated_by, check_passed = false,
				check_result = '{}', version = version + 1`))
		}, "close_task_checked"},
		{"a check result without whether it passed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), check(s), step(`UPDATE closing.close_task SET checked_at = now(), checked_by = updated_by, check_result = '{}',
				version = version + 1`))
		}, "close_task_checked"},
		{"a check result that is no object", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), check(s), step(`UPDATE closing.close_task SET checked_at = now(), checked_by = updated_by, check_passed = false,
				check_result = '[]', version = version + 1`))
		}, "close_task_checked"},
		{"a check result removed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), check(s), checked("false", "open"), step(`UPDATE closing.close_task SET checked_at = NULL, checked_by = NULL,
				check_passed = NULL, check_result = NULL, version = version + 1`))
		}, "keeps its check result"},
		{"a task created checked", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(`INSERT INTO closing.close_task (tenant_id, close_id, code, seq, name, kind, check_kind, due_on, checked_at, checked_by,
				check_passed, check_result, created_by, updated_by) VALUES ($1, $2, 'NP', 1, 'A check', 'check', 'no_parked_entries', '2026-02-03', now(), $3,
				true, '{}', $3, $3)`, s.tenant, s.close, s.actor))
		}, "a close task starts unchecked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), entity: id(), close: id()}
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
}

// Year closes (T64m4, migration 000233): one close per company and fiscal
// year, starting open, its company, year and template version fixed; the
// ledgers it covers written once; its tasks under the period close's
// rules (the shared trigger functions name the year close's tables). Nothing
// is deleted, every change moves the row's version, and another tenant
// neither sees a year close nor adds tasks to it.
func TestYearCloseRules(t *testing.T) {
	need(t)
	const (
		closeRow = `INSERT INTO closing.year_close (tenant_id, id, legal_entity_id, fiscal_year, year_end, config_version, status, created_by, updated_by)
			VALUES ($1, $2, $3, $4, '2026-12-31', 1, $5, $6, $6)`
		taskRow = `INSERT INTO closing.year_close_task (tenant_id, close_id, code, seq, name, kind, check_kind, due_on, depends_on, status, completed_by,
			completed_at, note, checked_at, created_by, updated_by) VALUES ($1, $2, $3, $4, 'A task', $5, $6, '2027-01-29', $7, $8, $9, $10, $11, $12, $13, $13)`
		ledgerRow = `INSERT INTO closing.year_close_ledger (tenant_id, close_id, ledger_id) VALUES ($1, $2, $3)`
	)
	type scene struct{ tenant, actor, entity, close uuid.UUID }
	open := func(s scene) stmt { return step(closeRow, s.tenant, s.close, s.entity, 2026, "open", s.actor) }
	task := func(s scene, code string, seq int) stmt {
		return step(taskRow, s.tenant, s.close, code, seq, "manual", nil, []string{}, "open", nil, nil, nil, nil, s.actor)
	}
	check := func(s scene) stmt {
		return step(taskRow, s.tenant, s.close, "NP", 1, "check", "no_parked_entries", []string{}, "open", nil, nil, nil, nil, s.actor)
	}
	done := func(code string) stmt {
		return step(`UPDATE closing.year_close_task SET status = 'done', completed_by = updated_by, completed_at = now(), note = 'Done', version = version + 1
			WHERE code = $1`, code)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a close with its ledger and task", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(ledgerRow, s.tenant, s.close, id()), task(s, "AUDIT", 1))
		}, ""},
		{"a second close of the year", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(closeRow, s.tenant, id(), s.entity, 2026, "open", s.actor))
		}, "year_close_once"},
		{"a close of another year", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(closeRow, s.tenant, id(), s.entity, 2027, "open", s.actor))
		}, ""},
		{"a close of year 1899", func(tx pgx.Tx, s scene) error {
			return exec(tx, closeRow, s.tenant, s.close, s.entity, 1899, "open", s.actor)
		}, "year_close_fiscal_year_check"},
		{"a close signed off at once", func(tx pgx.Tx, s scene) error {
			return exec(tx, closeRow, s.tenant, s.close, s.entity, 2026, "signed_off", s.actor)
		}, "a year close starts open"},
		{"a close in another status", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(`UPDATE closing.year_close SET status = 'closed', version = version + 1`))
		}, "year_close_status_check"},
		{"a close signed off and reopened", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(`UPDATE closing.year_close SET status = 'signed_off', version = version + 1`),
				step(`UPDATE closing.year_close SET status = 'open', version = version + 1`))
		}, ""},
		{"a close's year end changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(`UPDATE closing.year_close SET year_end = '2026-12-30', version = version + 1`))
		}, "changes only its status"},
		{"a close changed without its version", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(`UPDATE closing.year_close SET status = 'signed_off'`))
		}, "optimistic lock"},
		{"a close deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(`DELETE FROM closing.year_close`))
		}, "permission denied"},
		{"a covered ledger changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(ledgerRow, s.tenant, s.close, id()), step(`UPDATE closing.year_close_ledger SET ledger_id = gen_random_uuid()`))
		}, "permission denied"},
		{"a ledger of no close", func(tx pgx.Tx, s scene) error {
			return all(tx, step(ledgerRow, s.tenant, s.close, id()))
		}, "year_close_ledger_tenant_id_close_id_fkey"},
		{"a task done and reopened", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AUDIT", 1), done("AUDIT"), step(`UPDATE closing.year_close_task SET status = 'open', completed_by = NULL,
				completed_at = NULL, note = NULL, version = version + 1`))
		}, ""},
		{"a task assigned", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AUDIT", 1), step(`UPDATE closing.year_close_task SET assignee_id = $1, version = version + 1`, s.actor))
		}, ""},
		{"a task created done", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(taskRow, s.tenant, s.close, "AUDIT", 1, "manual", nil, []string{}, "done", s.actor, "2027-01-04T10:00:00Z", nil, nil,
				s.actor))
		}, "a year close task starts open"},
		{"a task created checked", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(taskRow, s.tenant, s.close, "NP", 1, "check", "no_parked_entries", []string{}, "open", nil, nil, nil,
				"2027-01-04T10:00:00Z", s.actor))
		}, "a year close task starts unchecked"},
		{"a task done without who did it", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AUDIT", 1), step(`UPDATE closing.year_close_task SET status = 'done', completed_at = now(), version = version + 1`))
		}, "year_close_task_completed"},
		{"an open task with a note", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AUDIT", 1), step(`UPDATE closing.year_close_task SET note = 'Early', version = version + 1`))
		}, "keeps its completion while it is open"},
		{"a completion rewritten", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AUDIT", 1), done("AUDIT"), step(`UPDATE closing.year_close_task SET completed_by = gen_random_uuid(),
				version = version + 1`))
		}, "keeps its completion while it is done"},
		{"a task's due date changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AUDIT", 1), step(`UPDATE closing.year_close_task SET due_on = '2027-02-10', version = version + 1`))
		}, "changes only its assignee, status and check result"},
		{"a manual task with a check", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(taskRow, s.tenant, s.close, "AUDIT", 1, "manual", "no_parked_entries", []string{}, "open", nil, nil, nil, nil, s.actor))
		}, "year_close_task_check_kind"},
		{"a task depending on itself", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(taskRow, s.tenant, s.close, "AUDIT", 1, "manual", nil, []string{"AUDIT"}, "open", nil, nil, nil, nil, s.actor))
		}, "year_close_task_depends_on"},
		{"two tasks in one place", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AUDIT", 1), task(s, "TAX", 1))
		}, "year_close_task_seq_once"},
		{"a task of no close", func(tx pgx.Tx, s scene) error {
			return all(tx, task(s, "AUDIT", 1))
		}, "year_close_task_tenant_id_close_id_fkey"},
		{"a task deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), task(s, "AUDIT", 1), step(`DELETE FROM closing.year_close_task`))
		}, "permission denied"},
		{"a check task done by hand", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), check(s), done("NP"))
		}, "year_close_task_check_passed"},
		{"a check result removed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), check(s), step(`UPDATE closing.year_close_task SET checked_at = now(), checked_by = updated_by, check_passed = false,
				check_result = '{"summary": "x"}', version = version + 1`), step(`UPDATE closing.year_close_task SET checked_at = NULL, checked_by = NULL,
				check_passed = NULL, check_result = NULL, version = version + 1`))
		}, "keeps its check result"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), entity: id(), close: id()}
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
	t.Run("another tenant's", func(t *testing.T) {
		a, b := scene{tenant: id(), actor: id(), entity: id(), close: id()}, id()
		mustApp(t, a.tenant, func(tx pgx.Tx) error { return all(tx, open(a)) })
		err := asApp(t, b, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM closing.year_close`).Scan(&n); err != nil || n != 0 {
				t.Errorf("another tenant's year closes: got %d (%v), want none", n, err)
			}
			return exec(tx, `INSERT INTO closing.year_close_task (tenant_id, close_id, code, seq, name, kind, due_on, created_by, updated_by)
				VALUES ($1, $2, 'AUDIT', 1, 'A task', 'manual', '2027-01-29', $3, $3)`, b, a.close, a.actor)
		})
		failsWith(t, err, "year_close_task_tenant_id_close_id_fkey")
	})
}

// TestPeriodCloseIsTheTenants: another tenant neither sees a close nor
// adds tasks to it.
func TestPeriodCloseIsTheTenants(t *testing.T) {
	need(t)
	a, b, actor, closeID := id(), id(), id(), id()
	mustApp(t, a, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO closing.period_close (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, period_end, config_version,
			created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), 2026, 1, '2026-01-31', 1, $3, $3)`, a, closeID, actor)
	})
	err := asApp(t, b, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM closing.period_close`).Scan(&n); err != nil || n != 0 {
			t.Errorf("another tenant's closes: got %d (%v), want none", n, err)
		}
		return exec(tx, `INSERT INTO closing.close_task (tenant_id, close_id, code, seq, name, kind, due_on, created_by, updated_by)
			VALUES ($1, $2, 'AP', 1, 'A task', 'manual', '2026-02-03', $3, $3)`, b, closeID, actor)
	})
	failsWith(t, err, "close_task_tenant_id_close_id_fkey")
}

// TestClosePermissions: the close's permissions (T64f's closing.accrual.decide,
// migration 000217, included) are seeded and are no
// approval permissions.
func TestClosePermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'closing.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := "closing.accrual.decide closing.accrual.propose closing.close.manage closing.close.read closing.close.sign_off closing.variance.comment closing.variance.propose"
	if got := strings.Join(codes, " "); got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// Sign-offs (T64d, migration 000199): a close's sign-off names at least
// one covered ledger and at most twenty, as a list; its note, when given,
// is text of at most 2,000 characters; it belongs to a close of its
// tenant; and it is never updated or deleted. Another tenant does not see
// it.
func TestCloseSignOffRules(t *testing.T) {
	need(t)
	const (
		closeRow = `INSERT INTO closing.period_close (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, period_end, config_version,
			created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), 2026, 1, '2026-01-31', 1, $3, $3)`
		signRow = `INSERT INTO closing.sign_off (tenant_id, id, close_id, signed_by, note, locked) VALUES ($1, gen_random_uuid(), $2, $3, $4, $5)`
		one     = `[{"ledger_id": "0192a000-0000-7000-8000-000000000a01", "locked": true}]`
	)
	type scene struct{ tenant, actor, close uuid.UUID }
	open := func(s scene) stmt { return step(closeRow, s.tenant, s.close, s.actor) }
	sign := func(s scene, note any, locked string) stmt {
		return step(signRow, s.tenant, s.close, s.actor, note, locked)
	}
	many := "[" + strings.TrimSuffix(strings.Repeat(`{"locked": true},`, 21), ",") + "]"
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a sign-off with a note, and a second", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), sign(s, "Closed", one), sign(s, nil, one))
		}, ""},
		{"a sign-off of no close", func(tx pgx.Tx, s scene) error { return all(tx, sign(s, nil, one)) }, "sign_off_tenant_id_close_id_fkey"},
		{"a sign-off without a ledger", func(tx pgx.Tx, s scene) error { return all(tx, open(s), sign(s, nil, `[]`)) }, "sign_off_locked"},
		{"a sign-off of 21 ledgers", func(tx pgx.Tx, s scene) error { return all(tx, open(s), sign(s, nil, many)) }, "sign_off_locked"},
		{"a sign-off's ledgers as no list", func(tx pgx.Tx, s scene) error { return all(tx, open(s), sign(s, nil, `{}`)) }, "sign_off_locked"},
		{"an empty note", func(tx pgx.Tx, s scene) error { return all(tx, open(s), sign(s, "", one)) }, "sign_off_note_check"},
		{"a note too long", func(tx pgx.Tx, s scene) error { return all(tx, open(s), sign(s, strings.Repeat("n", 2001), one)) }, "sign_off_note_check"},
		{"a sign-off changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), sign(s, nil, one), step(`UPDATE closing.sign_off SET note = 'Later'`))
		}, "permission denied"},
		{"a sign-off deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), sign(s, nil, one), step(`DELETE FROM closing.sign_off`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), close: id()}
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
	t.Run("changed by the owner", func(t *testing.T) {
		s := scene{tenant: id(), actor: id(), close: id()}
		mustApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, open(s), sign(s, nil, one)) })
		_, err := testDB.Exec(context.Background(), `UPDATE closing.sign_off SET note = 'Later' WHERE tenant_id = $1`, s.tenant)
		failsWith(t, err, "insert-only")
	})
	t.Run("another tenant's", func(t *testing.T) {
		a, b := scene{tenant: id(), actor: id(), close: id()}, id()
		mustApp(t, a.tenant, func(tx pgx.Tx) error { return all(tx, open(a), sign(a, nil, one)) })
		mustApp(t, b, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM closing.sign_off`).Scan(&n); err != nil || n != 0 {
				t.Errorf("another tenant's sign-offs: got %d (%v), want none", n, err)
			}
			return nil
		})
	})
}

// Variance comments (T64i, migration 000202): an agent's comment starts
// proposed, in its session, and a person's accepted, in none; a proposal
// is accepted (its text then replaceable, the proposal's kept) or rejected
// with a reason, an accepted comment superseded, and nothing else moves or
// changes; who decided and when are set exactly once decided; a row has at
// most one accepted comment, and a session one proposal per row. Its
// citations, at most 20 and each entry once, are never updated or
// deleted, nor is a comment. Another tenant sees neither.
func TestVarianceCommentRules(t *testing.T) {
	need(t)
	const (
		closeRow = `INSERT INTO closing.period_close (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, period_end, config_version,
			created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), 2026, 1, '2026-01-31', 1, $3, $3)`
		commentRow = `INSERT INTO closing.variance_comment (tenant_id, id, close_id, basis, account_id, account_number, account_name, text, author_kind,
			author_id, session_id, currency, actual, comparison, status, decided_by, decided_at, created_by, updated_by)
			VALUES ($1, $2, $3, 'prior_period', $4, '6000', 'Rent', 'Rent doubled', $5, $6, $7, 'EUR', 200000, 100000, $8, $9, $10, $6, $6)`
		citeRow = `INSERT INTO closing.variance_citation (tenant_id, comment_id, seq, journal_entry_year, journal_entry_id, document_number, posting_date)
			VALUES ($1, $2, $3, 2026, $4, '100001', '2026-01-15')`
	)
	type scene struct{ tenant, actor, close, account, comment, session uuid.UUID }
	open := func(s scene) stmt { return step(closeRow, s.tenant, s.close, s.actor) }
	proposal := func(s scene, id, session uuid.UUID) stmt {
		return step(commentRow, s.tenant, id, s.close, s.account, "agent", s.actor, session, "proposed", nil, nil)
	}
	written := func(s scene, id uuid.UUID) stmt {
		return step(commentRow, s.tenant, id, s.close, s.account, "human", s.actor, nil, "accepted", s.actor, "2026-02-03T10:00:00Z")
	}
	set := func(sql string, args ...any) stmt {
		return step(`UPDATE closing.variance_comment SET `+sql+`, version = version + 1 WHERE id = $1`, args...)
	}
	accept := func(id uuid.UUID) stmt {
		return set(`status = 'accepted', decided_by = updated_by, decided_at = now()`, id)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a proposal accepted with its text replaced, then superseded by a person's", func(tx pgx.Tx, s scene) error {
			other := id()
			return all(tx, open(s), proposal(s, s.comment, s.session), step(citeRow, s.tenant, s.comment, 1, id()), step(citeRow, s.tenant, s.comment, 2, id()),
				set(`status = 'accepted', text = 'Rent doubled in January', proposed_text = text, decided_by = updated_by, decided_at = now()`, s.comment),
				set(`status = 'superseded'`, s.comment), written(s, other))
		}, ""},
		{"a proposal rejected with a reason", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), proposal(s, s.comment, s.session), set(`status = 'rejected', decided_by = updated_by, decided_at = now(),
				decision_reason = 'Not the reason'`, s.comment))
		}, ""},
		{"a proposal rejected without a reason", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), proposal(s, s.comment, s.session), set(`status = 'rejected', decided_by = updated_by, decided_at = now()`, s.comment))
		}, "close_variance_comment_decided"},
		{"a proposal accepted without who decided", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), proposal(s, s.comment, s.session), set(`status = 'accepted', decided_at = now()`, s.comment))
		}, "close_variance_comment_decided"},
		{"an agent's comment created accepted", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(commentRow, s.tenant, s.comment, s.close, s.account, "agent", s.actor, s.session, "accepted", s.actor, "2026-02-03T10:00:00Z"))
		}, "starts proposed"},
		{"a person's comment created proposed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(commentRow, s.tenant, s.comment, s.close, s.account, "human", s.actor, nil, "proposed", nil, nil))
		}, "starts proposed"},
		{"an agent's comment without a session", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(commentRow, s.tenant, s.comment, s.close, s.account, "agent", s.actor, nil, "proposed", nil, nil))
		}, "variance_comment_session"},
		{"a person's comment in a session", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(commentRow, s.tenant, s.comment, s.close, s.account, "human", s.actor, s.session, "accepted", s.actor, "2026-02-03T10:00:00Z"))
		}, "variance_comment_session"},
		{"a rejected proposal with a replaced text", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), proposal(s, s.comment, s.session), set(`status = 'rejected', text = 'Other', proposed_text = text, decided_by = updated_by,
				decided_at = now(), decision_reason = 'No'`, s.comment))
		}, "variance_comment_edited"},
		{"a person's comment created with a proposal's text", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(`INSERT INTO closing.variance_comment (tenant_id, id, close_id, basis, account_id, account_number, account_name, text,
				proposed_text, author_kind, author_id, currency, actual, comparison, status, decided_by, decided_at, created_by, updated_by)
				VALUES ($1, $2, $3, 'plan', $4, '6000', 'Rent', 'Rent', 'Before', 'human', $5, 'EUR', 0, 0, 'accepted', $5, now(), $5, $5)`, s.tenant, s.comment, s.close, s.account, s.actor))
		}, "starts proposed"},
		{"two accepted comments of a row", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), written(s, s.comment), written(s, id()))
		}, "variance_comment_accepted_once"},
		{"two proposals of a session for a row", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), proposal(s, s.comment, s.session), proposal(s, id(), s.session))
		}, "variance_comment_proposed_once"},
		{"proposals of two sessions for a row", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), proposal(s, s.comment, s.session), proposal(s, id(), id()))
		}, ""},
		{"a rejected proposal accepted", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), proposal(s, s.comment, s.session), set(`status = 'rejected', decided_by = updated_by, decided_at = now(),
				decision_reason = 'No'`, s.comment), set(`status = 'accepted', decision_reason = NULL`, s.comment))
		}, "does not move from rejected to accepted"},
		{"a proposal superseded", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), proposal(s, s.comment, s.session), set(`status = 'superseded', decided_by = updated_by, decided_at = now()`, s.comment))
		}, "does not move from proposed to superseded"},
		{"an accepted comment's text changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), written(s, s.comment), set(`text = 'Later'`, s.comment))
		}, "keeps its text and decision"},
		{"a superseded comment's decision changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), proposal(s, s.comment, s.session), accept(s.comment), set(`status = 'superseded', decided_at = now() + interval '1 day'`, s.comment))
		}, "keeps its text and decision"},
		{"a comment's figures changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), written(s, s.comment), set(`actual = 1`, s.comment))
		}, "changes only its status and decision"},
		{"a comment changed without its version", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), proposal(s, s.comment, s.session), step(`UPDATE closing.variance_comment SET status = 'accepted', decided_by = updated_by,
				decided_at = now()`))
		}, "optimistic lock"},
		{"an empty text", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), proposal(s, s.comment, s.session), set(`status = 'accepted', text = ' ', decided_by = updated_by, decided_at = now()`, s.comment))
		}, "variance_comment_text_check"},
		{"a comment of no close", func(tx pgx.Tx, s scene) error { return all(tx, written(s, s.comment)) }, "variance_comment_tenant_id_close_id_fkey"},
		{"a comment deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), written(s, s.comment), step(`DELETE FROM closing.variance_comment`))
		}, "permission denied"},
		{"an entry cited twice", func(tx pgx.Tx, s scene) error {
			entry := id()
			return all(tx, open(s), written(s, s.comment), step(citeRow, s.tenant, s.comment, 1, entry), step(citeRow, s.tenant, s.comment, 2, entry))
		}, "variance_citation_once"},
		{"a 21st citation", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), written(s, s.comment), step(citeRow, s.tenant, s.comment, 21, id()))
		}, "variance_citation_seq_check"},
		{"a citation of no comment", func(tx pgx.Tx, s scene) error {
			return all(tx, step(citeRow, s.tenant, s.comment, 1, id()))
		}, "variance_citation_tenant_id_comment_id_fkey"},
		{"a citation changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), written(s, s.comment), step(citeRow, s.tenant, s.comment, 1, id()),
				step(`UPDATE closing.variance_citation SET document_number = '9'`))
		}, "permission denied"},
		{"a citation deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), written(s, s.comment), step(citeRow, s.tenant, s.comment, 1, id()), step(`DELETE FROM closing.variance_citation`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), close: id(), account: id(), comment: id(), session: id()}
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
	t.Run("a citation changed by the owner", func(t *testing.T) {
		s := scene{tenant: id(), actor: id(), close: id(), account: id(), comment: id()}
		mustApp(t, s.tenant, func(tx pgx.Tx) error {
			return all(tx, open(s), written(s, s.comment), step(citeRow, s.tenant, s.comment, 1, id()))
		})
		_, err := testDB.Exec(context.Background(), `UPDATE closing.variance_citation SET document_number = '9' WHERE tenant_id = $1`, s.tenant)
		failsWith(t, err, "insert-only")
	})
	t.Run("another tenant's", func(t *testing.T) {
		a, b := scene{tenant: id(), actor: id(), close: id(), account: id(), comment: id()}, id()
		mustApp(t, a.tenant, func(tx pgx.Tx) error {
			return all(tx, open(a), written(a, a.comment), step(citeRow, a.tenant, a.comment, 1, id()))
		})
		mustApp(t, b, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM closing.variance_comment) + (SELECT count(*) FROM closing.variance_citation)`).
				Scan(&n); err != nil || n != 0 {
				t.Errorf("another tenant's comments and citations: got %d (%v), want none", n, err)
			}
			return nil
		})
	})
}

// Accrual proposals (T64e, migration 000216): a proposal starts proposed
// and moves only to rejected, with who decided, when and why set exactly
// then; nothing else of it changes, and it is never deleted. Its amount is
// more than zero, a center's code and name are kept exactly with it, and
// one proposal names a parked entry. Its evidence, 1-10 documents each
// once, a journal entry with its fiscal year and no supplier, an invoice
// or an order with its supplier and no fiscal year, is never updated or
// deleted. Another tenant sees neither.
func TestAccrualProposalRules(t *testing.T) {
	need(t)
	const (
		closeRow = `INSERT INTO closing.period_close (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, period_end, config_version,
			created_by, updated_by) VALUES ($1, $2, gen_random_uuid(), 2026, 1, '2026-01-31', 1, $3, $3)`
		proposalRow = `INSERT INTO closing.accrual_proposal (tenant_id, id, close_id, session_id, agent_id, account_id, account_number, account_name,
			accrual_account_id, accrual_account_number, accrual_account_name, cost_center_id, cost_center_code, cost_center_name, currency, amount,
			description, reason, parked_entry_id, posting_date, status, created_by, updated_by)
			VALUES ($1, $2, $3, gen_random_uuid(), $4, gen_random_uuid(), '6000', 'Courier', gen_random_uuid(), '2190', 'Accrued liabilities',
				$5, $6, $7, 'CAD', $8, 'Courier of January', 'Monthly invoices', $9, '2026-01-31', $10, $4, $4)`
		evidenceRow = `INSERT INTO closing.accrual_evidence (tenant_id, proposal_id, seq, kind, document_id, document_number, fiscal_year, supplier_id)
			VALUES ($1, $2, $3, $4, gen_random_uuid(), '5100000001', $5, $6)`
	)
	type scene struct{ tenant, actor, close, proposal uuid.UUID }
	open := func(s scene) stmt { return step(closeRow, s.tenant, s.close, s.actor) }
	propose := func(s scene, proposal uuid.UUID, amount int, status string) stmt {
		return step(proposalRow, s.tenant, proposal, s.close, s.actor, nil, nil, nil, amount, id(), status)
	}
	set := func(sql string, args ...any) stmt {
		return step(`UPDATE closing.accrual_proposal SET `+sql+`, version = version + 1 WHERE id = $1`, args...)
	}
	invoice := func(s scene, seq int) stmt {
		return step(evidenceRow, s.tenant, s.proposal, seq, "supplier_invoice", nil, id())
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a proposal with its evidence, rejected", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 120000, "proposed"), invoice(s, 1),
				step(evidenceRow, s.tenant, s.proposal, 2, "journal_entry", 2026, nil), step(evidenceRow, s.tenant, s.proposal, 3, "purchase_order", nil, id()),
				set(`status = 'rejected', decided_by = updated_by, decided_at = now(), decision_reason = 'Invoiced already'`, s.proposal))
		}, ""},
		{"a proposal with its centers", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(proposalRow, s.tenant, s.proposal, s.close, s.actor, id(), "ADMIN", "Administration", 100, id(), "proposed"))
		}, ""},
		{"a cost center without its code", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(proposalRow, s.tenant, s.proposal, s.close, s.actor, id(), nil, "Administration", 100, id(), "proposed"))
		}, "accrual_proposal_centers"},
		{"an empty cost center code", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(proposalRow, s.tenant, s.proposal, s.close, s.actor, id(), " ", "Administration", 100, id(), "proposed"))
		}, "accrual_proposal_cost_center_code"},
		{"an empty profit center code", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(strings.Replace(proposalRow, "cost_center_id, cost_center_code, cost_center_name", "profit_center_id, profit_center_code, profit_center_name", 1),
				s.tenant, s.proposal, s.close, s.actor, id(), "", "Region", 100, id(), "proposed"))
		}, "accrual_proposal_profit_center_code"},
		{"a code without its cost center", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(proposalRow, s.tenant, s.proposal, s.close, s.actor, nil, "ADMIN", nil, 100, id(), "proposed"))
		}, "accrual_proposal_centers"},
		{"an amount of zero", func(tx pgx.Tx, s scene) error { return all(tx, open(s), propose(s, s.proposal, 0, "proposed")) }, "accrual_proposal_amount"},
		{"a negative amount", func(tx pgx.Tx, s scene) error { return all(tx, open(s), propose(s, s.proposal, -1, "proposed")) }, "accrual_proposal_amount"},
		{"a proposal created rejected", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(`INSERT INTO closing.accrual_proposal (tenant_id, id, close_id, session_id, agent_id, account_id, account_number,
				account_name, accrual_account_id, accrual_account_number, accrual_account_name, currency, amount, description, reason, parked_entry_id,
				posting_date, status, decided_by, decided_at, decision_reason, created_by, updated_by) VALUES ($1, $2, $3, gen_random_uuid(), $4,
				gen_random_uuid(), '6000', 'Courier', gen_random_uuid(), '2190', 'Accrued', 'CAD', 100, 'A', 'B', gen_random_uuid(), '2026-01-31',
				'rejected', $4, now(), 'No', $4, $4)`, s.tenant, s.proposal, s.close, s.actor))
		}, "starts proposed"},
		{"a proposal of an unknown status", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "posted"))
		}, "starts proposed"},
		{"a rejection without a reason", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), set(`status = 'rejected', decided_by = updated_by, decided_at = now()`, s.proposal))
		}, "accrual_proposal_decided"},
		{"a decision while proposed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), set(`decided_by = updated_by, decided_at = now(), decision_reason = 'No'`, s.proposal))
		}, "does not move from proposed to proposed"},
		{"a rejected proposal proposed again", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"),
				set(`status = 'rejected', decided_by = updated_by, decided_at = now(), decision_reason = 'No'`, s.proposal),
				set(`status = 'proposed', decided_by = NULL, decided_at = NULL, decision_reason = NULL`, s.proposal))
		}, "does not move from rejected to proposed"},
		{"a rejected proposal's reason changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"),
				set(`status = 'rejected', decided_by = updated_by, decided_at = now(), decision_reason = 'No'`, s.proposal),
				set(`decision_reason = 'Later'`, s.proposal))
		}, "does not move from rejected to rejected"},
		{"a proposal's amount changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), set(`amount = 200`, s.proposal))
		}, "changes only its status and decision"},
		{"a proposal changed without its version", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), step(`UPDATE closing.accrual_proposal SET status = 'rejected',
				decided_by = updated_by, decided_at = now(), decision_reason = 'No'`))
		}, "optimistic lock"},
		{"two proposals of one parked entry", func(tx pgx.Tx, s scene) error {
			entry := id()
			return all(tx, open(s), step(proposalRow, s.tenant, s.proposal, s.close, s.actor, nil, nil, nil, 100, entry, "proposed"),
				step(proposalRow, s.tenant, id(), s.close, s.actor, nil, nil, nil, 100, entry, "proposed"))
		}, "accrual_proposal_parked_once"},
		{"an empty description", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(strings.Replace(proposalRow, "'Courier of January'", "' '", 1), s.tenant, s.proposal, s.close, s.actor, nil, nil, nil,
				100, id(), "proposed"))
		}, "accrual_proposal_description_check"},
		{"a description too long", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), step(strings.Replace(proposalRow, "'Courier of January'", "repeat('d', 201)", 1), s.tenant, s.proposal, s.close, s.actor,
				nil, nil, nil, 100, id(), "proposed"))
		}, "accrual_proposal_description_check"},
		{"a proposal of no close", func(tx pgx.Tx, s scene) error {
			return all(tx, propose(s, s.proposal, 100, "proposed"))
		}, "accrual_proposal_tenant_id_close_id_fkey"},
		{"a proposal deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), step(`DELETE FROM closing.accrual_proposal`))
		}, "permission denied"},
		{"a document cited twice", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), step(`INSERT INTO closing.accrual_evidence (tenant_id, proposal_id, seq, kind,
				document_id, document_number, supplier_id) SELECT $1, $2, n, 'supplier_invoice', '0192a000-0000-7000-8000-000000000611', '5100000001', $3
				FROM generate_series(1, 2) n`, s.tenant, s.proposal, id()))
		}, "accrual_evidence_once"},
		{"an 11th document", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), invoice(s, 11))
		}, "accrual_evidence_seq_check"},
		{"a document of an unknown kind", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), step(evidenceRow, s.tenant, s.proposal, 1, "goods_receipt", nil, id()))
		}, "accrual_evidence_kind_check"},
		{"a journal entry without its fiscal year", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), step(evidenceRow, s.tenant, s.proposal, 1, "journal_entry", nil, nil))
		}, "accrual_evidence_kind_keys"},
		{"a journal entry with a supplier", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), step(evidenceRow, s.tenant, s.proposal, 1, "journal_entry", 2026, id()))
		}, "accrual_evidence_kind_keys"},
		{"an invoice without its supplier", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), step(evidenceRow, s.tenant, s.proposal, 1, "supplier_invoice", nil, nil))
		}, "accrual_evidence_kind_keys"},
		{"an order with a fiscal year", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), step(evidenceRow, s.tenant, s.proposal, 1, "purchase_order", 2026, id()))
		}, "accrual_evidence_kind_keys"},
		{"evidence of no proposal", func(tx pgx.Tx, s scene) error { return all(tx, invoice(s, 1)) }, "accrual_evidence_tenant_id_proposal_id_fkey"},
		{"evidence changed", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), invoice(s, 1), step(`UPDATE closing.accrual_evidence SET document_number = '9'`))
		}, "permission denied"},
		{"evidence deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), invoice(s, 1), step(`DELETE FROM closing.accrual_evidence`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), close: id(), proposal: id()}
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
	t.Run("evidence changed by the owner", func(t *testing.T) {
		s := scene{tenant: id(), actor: id(), close: id(), proposal: id()}
		mustApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, open(s), propose(s, s.proposal, 100, "proposed"), invoice(s, 1)) })
		_, err := testDB.Exec(context.Background(), `UPDATE closing.accrual_evidence SET document_number = '9' WHERE tenant_id = $1`, s.tenant)
		failsWith(t, err, "insert-only")
	})
	t.Run("another tenant's", func(t *testing.T) {
		a, b := scene{tenant: id(), actor: id(), close: id(), proposal: id()}, id()
		mustApp(t, a.tenant, func(tx pgx.Tx) error { return all(tx, open(a), propose(a, a.proposal, 100, "proposed"), invoice(a, 1)) })
		mustApp(t, b, func(tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM closing.accrual_proposal) + (SELECT count(*) FROM closing.accrual_evidence)`).
				Scan(&n); err != nil || n != 0 {
				t.Errorf("another tenant's proposals and evidence: got %d (%v), want none", n, err)
			}
			return nil
		})
	})
}
