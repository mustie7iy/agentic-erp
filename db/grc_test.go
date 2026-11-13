package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestGRCPermissions (T98a, migration 000296; ADR-0078 §6): the
// permissions of the control library are seeded and are no approval
// permissions.
func TestGRCPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'grc.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "grc.control.read grc.control.test grc.control.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestControlRules (T98a, migration 000296; ADR-0078 §2): a control has
// a code of the tenant's, unique whatever its case, texts that are not
// white space, a known frequency and nature; a change sets what it says
// with the next version, never its code; a retired control is fixed, and
// no control is deleted.
func TestControlRules(t *testing.T) {
	need(t)
	const control = `INSERT INTO grc.control (tenant_id, id, code, objective, process, owner_id, frequency, nature, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $1, $6, $7, $1, $1)`
	type scene struct{ tenant, control uuid.UUID }
	with := func(s scene, code, objective, process, frequency, nature string) stmt {
		return step(control, s.tenant, s.control, code, objective, process, frequency, nature)
	}
	record := func(s scene) stmt {
		return with(s, "PAY-01", "Released by another person.", "Payments", "continuous", "automated")
	}
	retire := func(s scene) stmt {
		return step(`UPDATE grc.control SET status = 'retired', version = version + 1, updated_by = $1 WHERE id = $2`, s.tenant, s.control)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a control", func(s scene) []stmt { return []stmt{record(s)} }, ""},
		{"a code with a space", func(s scene) []stmt {
			return []stmt{with(s, "PAY 01", "x", "Payments", "daily", "manual")}
		}, "control_code_check"},
		{"a code of 41 characters", func(s scene) []stmt {
			return []stmt{with(s, strings.Repeat("A", 41), "x", "Payments", "daily", "manual")}
		}, "control_code_check"},
		{"an objective of white space", func(s scene) []stmt {
			return []stmt{with(s, "PAY-01", "  ", "Payments", "daily", "manual")}
		}, "control_objective_check"},
		{"a process of white space", func(s scene) []stmt {
			return []stmt{with(s, "PAY-01", "x", " ", "daily", "manual")}
		}, "control_process_check"},
		{"a process with white space around it", func(s scene) []stmt {
			return []stmt{with(s, "PAY-01", "x", "Payments ", "daily", "manual")}
		}, "control_process_check"},
		{"an unknown frequency", func(s scene) []stmt {
			return []stmt{with(s, "PAY-01", "x", "Payments", "hourly", "manual")}
		}, "control_frequency_check"},
		{"an unknown nature", func(s scene) []stmt {
			return []stmt{with(s, "PAY-01", "x", "Payments", "daily", "hybrid")}
		}, "control_nature_check"},
		{"a code taken", func(s scene) []stmt {
			return []stmt{record(s), step(control, s.tenant, id(), "PAY-01", "x", "Payments", "daily", "manual")}
		}, "control_code_once"},
		{"a code taken in another case", func(s scene) []stmt {
			return []stmt{record(s), step(control, s.tenant, id(), "pay-01", "x", "Payments", "daily", "manual")}
		}, "control_code_once"},
		{"another code", func(s scene) []stmt {
			return []stmt{record(s), step(control, s.tenant, id(), "PAY-02", "x", "Payments", "daily", "manual")}
		}, ""},
		{"an unknown status", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.control SET status = 'draft', version = version + 1 WHERE id = $1`, s.control)}
		}, "control_status_check"},
		{"what it says changed", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.control SET objective = 'y', process = 'Treasury', owner_id = gen_random_uuid(),
				frequency = 'monthly', nature = 'manual', version = version + 1, updated_by = $1 WHERE id = $2`, s.tenant, s.control)}
		}, ""},
		{"changed without the next version", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.control SET frequency = 'monthly' WHERE id = $1`, s.control)}
		}, "optimistic lock"},
		{"recorded at version 0", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO grc.control (tenant_id, id, code, objective, process, owner_id, frequency, nature, version, created_by, updated_by)
				VALUES ($1, $2, 'PAY-01', 'x', 'Payments', $1, 'daily', 'manual', 0, $1, $1)`, s.tenant, s.control)}
		}, "control_version_check"},
		{"the code changed", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.control SET code = 'PAY-02' WHERE id = $1`, s.control)}
		}, "permission denied"},
		{"retired", func(s scene) []stmt { return []stmt{record(s), retire(s)} }, ""},
		{"a retired control changed", func(s scene) []stmt {
			return []stmt{record(s), retire(s), step(`UPDATE grc.control SET frequency = 'monthly', version = version + 1 WHERE id = $1`, s.control)}
		}, "control_retired_fixed"},
		{"a retired control made active again", func(s scene) []stmt {
			return []stmt{record(s), retire(s), step(`UPDATE grc.control SET status = 'active', version = version + 1 WHERE id = $1`, s.control)}
		}, "control_retired_fixed"},
		{"a control deleted", func(s scene) []stmt {
			return []stmt{record(s), step(`DELETE FROM grc.control`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), control: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
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

// TestControlTouched (T98a, migration 000296): a change of a control
// moves its updated_at, which the application cannot set itself.
func TestControlTouched(t *testing.T) {
	need(t)
	tenant, control := id(), id()
	var moved bool
	err := asApp(t, tenant, func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx, `INSERT INTO grc.control (tenant_id, id, code, objective, process, owner_id, frequency, nature,
				created_at, updated_at, created_by, updated_by)
			VALUES ($1, $2, 'PAY-01', 'x', 'Payments', $1, 'daily', 'manual', '2026-01-01', '2026-01-01', $1, $1)`, tenant, control); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `UPDATE grc.control SET frequency = 'monthly', version = version + 1, updated_by = $1 WHERE id = $2
			RETURNING updated_at > '2026-01-01' AND created_at = '2026-01-01' AND version = 2`, tenant, control).Scan(&moved)
	})
	if err != nil || !moved {
		t.Errorf("got %v and moved %v, want the change to move updated_at alone, at version 2", err, moved)
	}
}

// TestControlTestRules (T98a, migration 000296; ADR-0078 §3): a test is
// of a control of the tenant, passed or failed, with a note that is not
// white space, and is never changed or deleted.
func TestControlTestRules(t *testing.T) {
	need(t)
	const control = `INSERT INTO grc.control (tenant_id, id, code, objective, process, owner_id, frequency, nature, created_by, updated_by)
		VALUES ($1, $2, 'PAY-01', 'x', 'Payments', $1, 'daily', 'manual', $1, $1)`
	const test = `INSERT INTO grc.control_test (tenant_id, id, control_id, tested_on, result, note, tested_by) VALUES ($1, $2, $3, '2026-11-25', $4, $5, $1)`
	type scene struct{ tenant, control, test uuid.UUID }
	record := func(s scene) stmt { return step(control, s.tenant, s.control) }
	passed := func(s scene) stmt { return step(test, s.tenant, s.test, s.control, "passed", "Seen in run 12.") }
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a test", func(s scene) []stmt { return []stmt{record(s), passed(s)} }, ""},
		{"a failed test", func(s scene) []stmt {
			return []stmt{record(s), step(test, s.tenant, s.test, s.control, "failed", "Not seen.")}
		}, ""},
		{"a second test of the date", func(s scene) []stmt {
			return []stmt{record(s), passed(s), step(test, s.tenant, id(), s.control, "failed", "Seen again.")}
		}, ""},
		{"an unknown result", func(s scene) []stmt {
			return []stmt{record(s), step(test, s.tenant, s.test, s.control, "partly", "Seen.")}
		}, "control_test_result_check"},
		{"a note of white space", func(s scene) []stmt {
			return []stmt{record(s), step(test, s.tenant, s.test, s.control, "passed", "  ")}
		}, "control_test_note_check"},
		{"a note of 2001 characters", func(s scene) []stmt {
			return []stmt{record(s), step(test, s.tenant, s.test, s.control, "passed", strings.Repeat("n", 2001))}
		}, "control_test_note_check"},
		{"a test of no control", func(s scene) []stmt {
			return []stmt{step(test, s.tenant, s.test, s.control, "passed", "Seen.")}
		}, "control_test_control_fkey"},
		{"a test changed", func(s scene) []stmt {
			return []stmt{record(s), passed(s), step(`UPDATE grc.control_test SET result = 'failed' WHERE id = $1`, s.test)}
		}, "permission denied"},
		{"a test deleted", func(s scene) []stmt {
			return []stmt{record(s), passed(s), step(`DELETE FROM grc.control_test`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), control: id(), test: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
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

// TestControlTestImmutable (T98a, migration 000296): a test stays as it
// was recorded for the owner of the table too, whom no grant holds back.
func TestControlTestImmutable(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, control, test := id(), id(), id()
	if err := all(tx, step(`INSERT INTO grc.control (tenant_id, id, code, objective, process, owner_id, frequency, nature, created_by, updated_by)
			VALUES ($1, $2, 'PAY-01', 'x', 'Payments', $1, 'daily', 'manual', $1, $1)`, tenant, control),
		step(`INSERT INTO grc.control_test (tenant_id, id, control_id, tested_on, result, note, tested_by)
			VALUES ($1, $2, $3, '2026-11-25', 'passed', 'Seen.', $1)`, tenant, test, control)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, `UPDATE grc.control_test SET result = 'failed' WHERE id = $1`, test), "insert-only")
}
