package db

import (
	"context"
	"strconv"
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
		{"an objective of 1001 characters", func(s scene) []stmt {
			return []stmt{with(s, "PAY-01", strings.Repeat("o", 1001), "Payments", "daily", "manual")}
		}, "control_objective_check"},
		{"a process of 101 characters", func(s scene) []stmt {
			return []stmt{with(s, "PAY-01", "x", strings.Repeat("p", 101), "daily", "manual")}
		}, "control_process_check"},
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

// TestFrameworkReferenceRules (T98b, migration 000297; ADR-0078 §9): a
// control's framework references are at most 50 texts of 1 to 200
// characters without white space around them, none twice and none null;
// a change may replace them.
func TestFrameworkReferenceRules(t *testing.T) {
	need(t)
	const control = `INSERT INTO grc.control (tenant_id, id, code, objective, process, owner_id, frequency, nature, framework_references, created_by, updated_by)
		VALUES ($1, $2, 'PAY-01', 'x', 'Payments', $1, 'daily', 'manual', $3, $1, $1)`
	many := make([]string, 51)
	for i := range many {
		many[i] = "REF " + strconv.Itoa(i)
	}
	cases := []struct {
		name string
		refs []*string
		want string // empty: allowed
	}{
		{"none", texts(), ""},
		{"two references", texts("SOX 404", "ICFR 4.2"), ""},
		{"fifty references", texts(many[:50]...), ""},
		{"fifty-one references", texts(many...), "control_framework_references_check"},
		{"an empty reference", texts("SOX 404", ""), "control_framework_references_check"},
		{"a reference of 201 characters", texts(strings.Repeat("r", 201)), "control_framework_references_check"},
		{"a reference with white space around it", texts("SOX 404 "), "control_framework_references_check"},
		{"a reference twice", texts("SOX 404", "SOX 404"), "control_framework_references_check"},
		{"a null reference", []*string{nil}, "control_framework_references_check"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenant := id()
			err := asApp(t, tenant, func(tx pgx.Tx) error { return exec(tx, control, tenant, id(), c.refs) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
	t.Run("replaced by a change", func(t *testing.T) {
		tenant, ctl := id(), id()
		err := asApp(t, tenant, func(tx pgx.Tx) error {
			return all(tx, step(control, tenant, ctl, texts("SOX 404")),
				step(`UPDATE grc.control SET framework_references = '{ICFR 4.2}', version = version + 1, updated_by = $1 WHERE id = $2`, tenant, ctl))
		})
		if err != nil {
			t.Fatalf("got %v, want no error", err)
		}
	})
	t.Run("a control recorded without them has none", func(t *testing.T) {
		tenant := id()
		var n int
		err := asApp(t, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `INSERT INTO grc.control (tenant_id, id, code, objective, process, owner_id, frequency, nature, created_by, updated_by)
				VALUES ($1, $2, 'PAY-01', 'x', 'Payments', $1, 'daily', 'manual', $1, $1) RETURNING cardinality(framework_references)`, tenant, id()).Scan(&n)
		})
		if err != nil || n != 0 {
			t.Errorf("got %d references and %v, want none", n, err)
		}
	})
}

// texts are the texts as a text array's elements.
func texts(s ...string) []*string {
	out := make([]*string, len(s))
	for i := range s {
		out[i] = &s[i]
	}
	return out
}

// TestControlLinkRules (T98b, migration 000297; ADR-0078 §7): a link is
// of a control of the tenant, to a permission code, a rule's id or a
// configuration path in the form of its kind; a control has one active
// link to a target; removing a link is its only change, once, and no
// link is deleted. What a link names is another context's: no foreign
// key holds it.
func TestControlLinkRules(t *testing.T) {
	need(t)
	const control = `INSERT INTO grc.control (tenant_id, id, code, objective, process, owner_id, frequency, nature, created_by, updated_by)
		VALUES ($1, $2, 'PAY-01', 'x', 'Payments', $1, 'daily', 'manual', $1, $1)`
	const link = `INSERT INTO grc.control_link (tenant_id, id, control_id, kind, target, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $1, $1)`
	type scene struct{ tenant, control, link uuid.UUID }
	record := func(s scene) stmt { return step(control, s.tenant, s.control) }
	to := func(s scene, kind, target string) stmt { return step(link, s.tenant, s.link, s.control, kind, target) }
	remove := func(s scene) stmt {
		return step(`UPDATE grc.control_link SET status = 'removed', version = version + 1, updated_by = $1 WHERE id = $2`, s.tenant, s.link)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a link to a permission no catalog holds", func(s scene) []stmt { return []stmt{record(s), to(s, "permission", "payments.run.vanish")} }, ""},
		{"a link to a rule no tenant has", func(s scene) []stmt { return []stmt{record(s), to(s, "sod_rule", id().String())} }, ""},
		{"a link to a configuration path", func(s scene) []stmt {
			return []stmt{record(s), to(s, "configuration_path", "companies[3000].release_dual_limit")}
		}, ""},
		{"an unknown kind", func(s scene) []stmt { return []stmt{record(s), to(s, "report", "a.b")} }, "control_link_kind_check"},
		{"a permission that is no code", func(s scene) []stmt { return []stmt{record(s), to(s, "permission", "Payments")} }, "control_link_target_check"},
		{"a rule that is no id", func(s scene) []stmt { return []stmt{record(s), to(s, "sod_rule", "payments.run.release")} }, "control_link_target_check"},
		{"an empty path", func(s scene) []stmt { return []stmt{record(s), to(s, "configuration_path", "")} }, "control_link_target_check"},
		{"a path with white space around it", func(s scene) []stmt {
			return []stmt{record(s), to(s, "configuration_path", " payments")}
		}, "control_link_target_check"},
		{"a path of 501 characters", func(s scene) []stmt {
			return []stmt{record(s), to(s, "configuration_path", strings.Repeat("p", 501))}
		}, "control_link_target_check"},
		{"a link of no control", func(s scene) []stmt { return []stmt{to(s, "permission", "payments.run.release")} }, "control_link_control_fkey"},
		{"a second link to the target", func(s scene) []stmt {
			return []stmt{record(s), to(s, "permission", "payments.run.release"),
				step(link, s.tenant, id(), s.control, "permission", "payments.run.release")}
		}, "control_link_once"},
		{"a link to another target", func(s scene) []stmt {
			return []stmt{record(s), to(s, "permission", "payments.run.release"),
				step(link, s.tenant, id(), s.control, "permission", "payments.file.transmit")}
		}, ""},
		{"removed", func(s scene) []stmt { return []stmt{record(s), to(s, "permission", "payments.run.release"), remove(s)} }, ""},
		{"linked again after the removal", func(s scene) []stmt {
			return []stmt{record(s), to(s, "permission", "payments.run.release"), remove(s),
				step(link, s.tenant, id(), s.control, "permission", "payments.run.release")}
		}, ""},
		{"removed without the next version", func(s scene) []stmt {
			return []stmt{record(s), to(s, "permission", "payments.run.release"),
				step(`UPDATE grc.control_link SET status = 'removed' WHERE id = $1`, s.link)}
		}, "optimistic lock"},
		{"an unknown status", func(s scene) []stmt {
			return []stmt{record(s), to(s, "permission", "payments.run.release"),
				step(`UPDATE grc.control_link SET status = 'broken', version = version + 1 WHERE id = $1`, s.link)}
		}, "control_link_status_check"},
		{"made at version 0", func(s scene) []stmt {
			return []stmt{record(s), step(`INSERT INTO grc.control_link (tenant_id, id, control_id, kind, target, version, created_by, updated_by)
				VALUES ($1, $2, $3, 'permission', 'payments.run.release', 0, $1, $1)`, s.tenant, s.link, s.control)}
		}, "control_link_version_check"},
		{"a removed link made active again", func(s scene) []stmt {
			return []stmt{record(s), to(s, "permission", "payments.run.release"), remove(s),
				step(`UPDATE grc.control_link SET status = 'active', version = version + 1 WHERE id = $1`, s.link)}
		}, "control_link_removed_fixed"},
		{"the target changed", func(s scene) []stmt {
			return []stmt{record(s), to(s, "permission", "payments.run.release"),
				step(`UPDATE grc.control_link SET target = 'payments.file.transmit' WHERE id = $1`, s.link)}
		}, "permission denied"},
		{"the kind changed", func(s scene) []stmt {
			return []stmt{record(s), to(s, "permission", "payments.run.release"), step(`UPDATE grc.control_link SET kind = 'sod_rule' WHERE id = $1`, s.link)}
		}, "permission denied"},
		{"the control changed", func(s scene) []stmt {
			return []stmt{record(s), to(s, "permission", "payments.run.release"),
				step(`UPDATE grc.control_link SET control_id = gen_random_uuid() WHERE id = $1`, s.link)}
		}, "permission denied"},
		{"a link deleted", func(s scene) []stmt {
			return []stmt{record(s), to(s, "permission", "payments.run.release"), step(`DELETE FROM grc.control_link`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), control: id(), link: id()}
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
