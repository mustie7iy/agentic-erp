package db

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestGRCPermissions (T98a, migration 000304; ADR-0078 §6, §13; T98c,
// migration 000315): the permissions of the control library and the risk
// register are seeded and are no approval
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
	if got, want := strings.Join(codes, " "), "grc.control.read grc.control.test grc.control.write grc.risk.read grc.risk.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestControlRules (T98a, migration 000304; ADR-0078 §2): a control has
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

// TestControlTouched (T98a, migration 000304): a change of a control
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

// TestControlTestRules (T98a, migration 000304; ADR-0078 §3): a test is
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

// TestControlTestImmutable (T98a, migration 000304): a test stays as it
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

// TestFrameworkReferenceRules (T98b, migration 000305; ADR-0078 §9): a
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

// TestControlLinkRules (T98b, migration 000305; ADR-0078 §7): a link is
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

// riskSQL records a risk of the tenant ($1) with the id ($2), rated 2 by
// 4, by the tenant's id as its people.
const riskSQL = `INSERT INTO grc.risk (tenant_id, id, title, category, owner_id, likelihood, impact, created_by, updated_by)
	VALUES ($1, $2, 'Duplicate payments', 'Payments', $1, 2, 4, $1, $1)`

// TestRiskRules (T98c, migration 000315; ADR-0078 §10): a risk has a
// title and a category without white space around them, a likelihood
// and an impact from 1 to 5 whose product is its score; it is closed
// once with a reason, who and when, and is then fixed; no risk is
// deleted.
func TestRiskRules(t *testing.T) {
	need(t)
	const with = `INSERT INTO grc.risk (tenant_id, id, title, category, owner_id, likelihood, impact, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $1, $5, $6, $1, $1)`
	type scene struct{ tenant, risk uuid.UUID }
	record := func(s scene) stmt { return step(riskSQL, s.tenant, s.risk) }
	risk := func(s scene, title, category string, likelihood, impact int) stmt {
		return step(with, s.tenant, s.risk, title, category, likelihood, impact)
	}
	closeIt := func(s scene) stmt {
		return step(`UPDATE grc.risk SET status = 'closed', close_reason = 'Mitigated.', closed_at = now(), closed_by = $1, version = version + 1
			WHERE id = $2`, s.tenant, s.risk)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a risk", func(s scene) []stmt { return []stmt{record(s)} }, ""},
		{"a title of 201 characters", func(s scene) []stmt { return []stmt{risk(s, strings.Repeat("t", 201), "Payments", 1, 1)} }, "risk_title_check"},
		{"an empty title", func(s scene) []stmt { return []stmt{risk(s, "", "Payments", 1, 1)} }, "risk_title_check"},
		{"a title with white space around it", func(s scene) []stmt { return []stmt{risk(s, " Fraud", "Payments", 1, 1)} }, "risk_title_check"},
		{"a category of 101 characters", func(s scene) []stmt { return []stmt{risk(s, "Fraud", strings.Repeat("c", 101), 1, 1)} }, "risk_category_check"},
		{"a category with white space around it", func(s scene) []stmt { return []stmt{risk(s, "Fraud", "Payments ", 1, 1)} }, "risk_category_check"},
		{"a likelihood of 0", func(s scene) []stmt { return []stmt{risk(s, "Fraud", "Payments", 0, 1)} }, "risk_likelihood_check"},
		{"a likelihood of 6", func(s scene) []stmt { return []stmt{risk(s, "Fraud", "Payments", 6, 1)} }, "risk_likelihood_check"},
		{"an impact of 0", func(s scene) []stmt { return []stmt{risk(s, "Fraud", "Payments", 1, 0)} }, "risk_impact_check"},
		{"an impact of 6", func(s scene) []stmt { return []stmt{risk(s, "Fraud", "Payments", 1, 6)} }, "risk_impact_check"},
		{"a score set", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO grc.risk (tenant_id, id, title, category, owner_id, likelihood, impact, score, created_by, updated_by)
				VALUES ($1, $2, 'Fraud', 'Payments', $1, 2, 2, 25, $1, $1)`, s.tenant, s.risk)}
		}, "non-DEFAULT value into column \"score\""},
		{"rated again", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.risk SET likelihood = 1, impact = 4, version = version + 1, updated_by = $1 WHERE id = $2`, s.tenant, s.risk)}
		}, ""},
		{"changed without the next version", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.risk SET category = 'Treasury' WHERE id = $1`, s.risk)}
		}, "optimistic lock"},
		{"recorded at version 0", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO grc.risk (tenant_id, id, title, category, owner_id, likelihood, impact, version, created_by, updated_by)
				VALUES ($1, $2, 'Fraud', 'Payments', $1, 1, 1, 0, $1, $1)`, s.tenant, s.risk)}
		}, "risk_version_check"},
		{"an unknown status", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.risk SET status = 'accepted', version = version + 1 WHERE id = $1`, s.risk)}
		}, "risk_status_check"},
		{"closed", func(s scene) []stmt { return []stmt{record(s), closeIt(s)} }, ""},
		{"closed without a reason", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.risk SET status = 'closed', closed_at = now(), closed_by = $1, version = version + 1 WHERE id = $2`, s.tenant, s.risk)}
		}, "risk_closed_check"},
		{"closed without who", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.risk SET status = 'closed', close_reason = 'x', closed_at = now(), version = version + 1 WHERE id = $1`, s.risk)}
		}, "risk_closed_check"},
		{"a reason while open", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.risk SET close_reason = 'x', closed_at = now(), closed_by = $1, version = version + 1 WHERE id = $2`, s.tenant, s.risk)}
		}, "risk_closed_check"},
		{"a reason of white space", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.risk SET status = 'closed', close_reason = ' ', closed_at = now(), closed_by = $1, version = version + 1
				WHERE id = $2`, s.tenant, s.risk)}
		}, "risk_close_reason_check"},
		{"a reason of 1001 characters", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.risk SET status = 'closed', close_reason = repeat('r', 1001), closed_at = now(), closed_by = $1,
				version = version + 1 WHERE id = $2`, s.tenant, s.risk)}
		}, "risk_close_reason_check"},
		{"a closed risk changed", func(s scene) []stmt {
			return []stmt{record(s), closeIt(s), step(`UPDATE grc.risk SET category = 'Treasury', version = version + 1 WHERE id = $1`, s.risk)}
		}, "risk_closed_fixed"},
		{"a closed risk opened again", func(s scene) []stmt {
			return []stmt{record(s), closeIt(s), step(`UPDATE grc.risk SET status = 'open', close_reason = NULL, closed_at = NULL, closed_by = NULL,
				version = version + 1 WHERE id = $1`, s.risk)}
		}, "risk_closed_fixed"},
		{"the creator changed", func(s scene) []stmt {
			return []stmt{record(s), step(`UPDATE grc.risk SET created_by = gen_random_uuid() WHERE id = $1`, s.risk)}
		}, "permission denied"},
		{"a risk deleted", func(s scene) []stmt { return []stmt{record(s), step(`DELETE FROM grc.risk`)} }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), risk: id()}
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
	t.Run("the score is the product", func(t *testing.T) {
		s := scene{tenant: id(), risk: id()}
		var score int
		err := asApp(t, s.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), riskSQL+` RETURNING score`, s.tenant, s.risk).Scan(&score)
		})
		if err != nil || score != 8 {
			t.Errorf("got %d and %v, want 8", score, err)
		}
	})
}

// TestRiskClosedFixedForTheOwner (T98c, migration 000315): a closed risk
// stays as it was closed for the owner of the table too, whom no grant
// holds back.
func TestRiskClosedFixedForTheOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, risk := id(), id()
	if err := all(tx, step(riskSQL, tenant, risk), step(`UPDATE grc.risk SET status = 'closed', close_reason = 'Mitigated.', closed_at = now(),
		closed_by = $1, version = version + 1 WHERE id = $2`, tenant, risk)); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, `UPDATE grc.risk SET category = 'Treasury', version = version + 1 WHERE id = $1`, risk), "risk_closed_fixed")
}

// TestRiskRatingRules (T98c, migration 000315; ADR-0078 §11): a rating
// is of a risk of the tenant, with a likelihood and an impact from 1 to
// 5, and is never changed or deleted, by the owner of the table either.
func TestRiskRatingRules(t *testing.T) {
	need(t)
	const rating = `INSERT INTO grc.risk_rating (tenant_id, id, risk_id, likelihood, impact, rated_by) VALUES ($1, $2, $3, $4, $5, $1)`
	type scene struct{ tenant, risk, rating uuid.UUID }
	record := func(s scene) stmt { return step(riskSQL, s.tenant, s.risk) }
	rate := func(s scene, likelihood, impact int) stmt {
		return step(rating, s.tenant, s.rating, s.risk, likelihood, impact)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a rating", func(s scene) []stmt { return []stmt{record(s), rate(s, 2, 4)} }, ""},
		{"a likelihood of 0", func(s scene) []stmt { return []stmt{record(s), rate(s, 0, 4)} }, "risk_rating_likelihood_check"},
		{"a likelihood of 6", func(s scene) []stmt { return []stmt{record(s), rate(s, 6, 4)} }, "risk_rating_likelihood_check"},
		{"an impact of 0", func(s scene) []stmt { return []stmt{record(s), rate(s, 2, 0)} }, "risk_rating_impact_check"},
		{"an impact of 6", func(s scene) []stmt { return []stmt{record(s), rate(s, 2, 6)} }, "risk_rating_impact_check"},
		{"a rating of no risk", func(s scene) []stmt { return []stmt{rate(s, 2, 4)} }, "risk_rating_risk_fkey"},
		{"a rating changed", func(s scene) []stmt {
			return []stmt{record(s), rate(s, 2, 4), step(`UPDATE grc.risk_rating SET impact = 5 WHERE id = $1`, s.rating)}
		}, "permission denied"},
		{"a rating deleted", func(s scene) []stmt { return []stmt{record(s), rate(s, 2, 4), step(`DELETE FROM grc.risk_rating`)} }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), risk: id(), rating: id()}
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
	t.Run("insert-only for the owner", func(t *testing.T) {
		ctx := context.Background()
		tx, err := testDB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		s := scene{tenant: id(), risk: id(), rating: id()}
		if err := all(tx, record(s), rate(s, 2, 4)); err != nil {
			t.Fatal(err)
		}
		failsWith(t, exec(tx, `UPDATE grc.risk_rating SET impact = 5 WHERE id = $1`, s.rating), "insert-only")
	})
}

// TestRiskControlRules (T98c, migration 000315; ADR-0078 §12): a risk
// names a control of the tenant's library once at a time; removing it is
// the only change, once, with who and when, and a removed control can be
// named again; nothing is deleted.
func TestRiskControlRules(t *testing.T) {
	need(t)
	const control = `INSERT INTO grc.control (tenant_id, id, code, objective, process, owner_id, frequency, nature, created_by, updated_by)
		VALUES ($1, $2, 'PAY-01', 'x', 'Payments', $1, 'daily', 'manual', $1, $1)`
	const named = `INSERT INTO grc.risk_control (tenant_id, id, risk_id, control_id, created_by) VALUES ($1, $2, $3, $4, $1)`
	type scene struct{ tenant, risk, control, link uuid.UUID }
	setup := func(s scene) []stmt {
		return []stmt{step(riskSQL, s.tenant, s.risk), step(control, s.tenant, s.control)}
	}
	name := func(s scene) stmt { return step(named, s.tenant, s.link, s.risk, s.control) }
	remove := func(s scene) stmt {
		return step(`UPDATE grc.risk_control SET status = 'removed', removed_at = now(), removed_by = $1 WHERE id = $2`, s.tenant, s.link)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a control named", func(s scene) []stmt { return append(setup(s), name(s)) }, ""},
		{"a control of no library", func(s scene) []stmt {
			return []stmt{step(riskSQL, s.tenant, s.risk), name(s)}
		}, "risk_control_control_fkey"},
		{"a control of no risk", func(s scene) []stmt {
			return []stmt{step(control, s.tenant, s.control), name(s)}
		}, "risk_control_risk_fkey"},
		{"a control named twice", func(s scene) []stmt {
			return append(setup(s), name(s), step(named, s.tenant, id(), s.risk, s.control))
		}, "risk_control_once"},
		{"removed", func(s scene) []stmt { return append(setup(s), name(s), remove(s)) }, ""},
		{"named again after the removal", func(s scene) []stmt {
			return append(setup(s), name(s), remove(s), step(named, s.tenant, id(), s.risk, s.control))
		}, ""},
		{"removed without who", func(s scene) []stmt {
			return append(setup(s), name(s), step(`UPDATE grc.risk_control SET status = 'removed', removed_at = now() WHERE id = $1`, s.link))
		}, "risk_control_removed_check"},
		{"removed without when", func(s scene) []stmt {
			return append(setup(s), name(s), step(`UPDATE grc.risk_control SET status = 'removed', removed_by = $1 WHERE id = $2`, s.tenant, s.link))
		}, "risk_control_removed_check"},
		{"an unknown status", func(s scene) []stmt {
			return append(setup(s), name(s), step(`UPDATE grc.risk_control SET status = 'broken' WHERE id = $1`, s.link))
		}, "risk_control_status_check"},
		{"a removed control made active again", func(s scene) []stmt {
			return append(setup(s), name(s), remove(s), step(`UPDATE grc.risk_control SET status = 'active', removed_at = NULL, removed_by = NULL
				WHERE id = $1`, s.link))
		}, "risk_control_removed_fixed"},
		{"the control changed", func(s scene) []stmt {
			return append(setup(s), name(s), step(`UPDATE grc.risk_control SET control_id = gen_random_uuid() WHERE id = $1`, s.link))
		}, "permission denied"},
		{"deleted", func(s scene) []stmt { return append(setup(s), name(s), step(`DELETE FROM grc.risk_control`)) }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), risk: id(), control: id(), link: id()}
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
	t.Run("a removed control fixed for the owner", func(t *testing.T) {
		ctx := context.Background()
		tx, err := testDB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		s := scene{tenant: id(), risk: id(), control: id(), link: id()}
		if err := all(tx, append(setup(s), name(s), remove(s))...); err != nil {
			t.Fatal(err)
		}
		failsWith(t, exec(tx, `UPDATE grc.risk_control SET status = 'active', removed_at = NULL, removed_by = NULL WHERE id = $1`, s.link),
			"risk_control_removed_fixed")
	})
}
