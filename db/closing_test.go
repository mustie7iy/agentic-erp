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
// (T64c, migration 000195) is recorded whole, on a check task only, never
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

// TestClosePermissions: the close's permissions are seeded and are no
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
	if got := strings.Join(codes, " "); got != "closing.close.manage closing.close.read closing.close.sign_off" {
		t.Errorf("got %v, want closing.close.manage, closing.close.read and closing.close.sign_off", codes)
	}
}

// Sign-offs (T64d, migration 000197): a close's sign-off names at least
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
