package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestConsolidationPermissions (T68a, T68d; migrations 000223, 000224): the
// consolidation read and run permissions are seeded and are no approval
// permissions.
func TestConsolidationPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'consolidation.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "consolidation.read consolidation.run.manage"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestConsolidationRunRules (T68d, migration 000224; ADR-0059 §5): a run
// of a regular period starts queued and moves only along §5's
// transitions, one of a group queued or running at a time and one final
// per group and period; its problem, computation, finalization and
// supersession set exactly in their states and never rewritten; what it
// consolidates fixed. Its members and balances are written while it runs,
// a member's translation recorded once, and neither changed nor removed;
// a member names its ownership and first period and the parent none; a
// balance is a company account's, its trading partner another company,
// and the translation difference a group amount alone.
func TestConsolidationRunRules(t *testing.T) {
	need(t)
	const (
		runRow = `INSERT INTO consolidation.run (tenant_id, id, group_code, parent_id, companies, fiscal_year, fiscal_period, period_start, period_end,
			config_version, group_currency, rate_type, status, operation_id, created_by, updated_by)
			VALUES ($1, $2, 'MAPLE', $3, ARRAY[$3, $4]::uuid[], 2026, $5, '2026-11-01', '2026-11-30', 7, 'CAD', 'average', $6, gen_random_uuid(), $7, $7)`
		memberRow = `INSERT INTO consolidation.run_member (tenant_id, run_id, legal_entity_id, company_code, role, ownership_percent, first_fiscal_year,
			first_fiscal_period, local_currency, closing_rate, rate_valid_from) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
		balanceRow = `INSERT INTO consolidation.run_balance (tenant_id, run_id, legal_entity_id, line_no, group_account, account_id, trading_partner_id,
			kind, amount_lc, amount_gc_booked, amount_gc) VALUES ($1, $2, $3, $4, 'G1000', $5, $6, $7, $8, $9, $10)`
	)
	type scene struct{ tenant, actor, parent, member, run uuid.UUID }
	queued := func(s scene) stmt { return step(runRow, s.tenant, s.run, s.parent, s.member, 11, "queued", s.actor) }
	to := func(status, set string) stmt {
		return step(`UPDATE consolidation.run SET status = '` + status + `'` + set + `, version = version + 1`)
	}
	running := to("running", "")
	computed := to("computed", ", computed_at = now()")
	final := to("final", ", finalized_by = updated_by, finalized_at = now()")
	failed := to("failed", `, problem = '{"code": "consolidation.rate_stale", "detail": "x"}'`)
	parent := func(s scene) stmt {
		return step(memberRow, s.tenant, s.run, s.parent, "3000", "parent", nil, nil, nil, "CAD", "1", nil)
	}
	member := func(s scene) stmt {
		return step(memberRow, s.tenant, s.run, s.member, "1000", "member", "80", 2026, 11, "USD", "1.4", "2026-11-30")
	}
	translated := step(`UPDATE consolidation.run_member SET period_status = 'open', period_version = 3, translated_at = now() WHERE company_code = '1000'`)
	balance := func(s scene, kind string, account, partner any, lc any) stmt {
		return step(balanceRow, s.tenant, s.run, s.member, 1, account, partner, kind, lc, lc, 140)
	}
	cases := []struct {
		name string
		run  func(tx pgx.Tx, s scene) error
		want string // empty: allowed
	}{
		{"a run computed with its members and balances, final and superseded", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, parent(s), member(s), translated, balance(s, "balance", id(), s.parent, 100), computed, final,
				to("superseded", ", superseded_by = gen_random_uuid()"))
		}, ""},
		{"a run failed and discarded", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), failed, to("discarded", ""))
		}, ""},
		{"a run computed and discarded", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, computed, to("discarded", ""))
		}, ""},
		{"a run started running", func(tx pgx.Tx, s scene) error {
			return exec(tx, runRow, s.tenant, s.run, s.parent, s.member, 11, "running", s.actor)
		}, "a consolidation run starts queued"},
		{"a run of a special period", func(tx pgx.Tx, s scene) error {
			return exec(tx, runRow, s.tenant, s.run, s.parent, s.member, 13, "queued", s.actor)
		}, "run_regular_period"},
		{"two runs of the group queued", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), step(runRow, s.tenant, id(), s.parent, s.member, 10, "queued", s.actor))
		}, "consolidation_run_one_at_a_time"},
		{"two final runs of the group and period", func(tx pgx.Tx, s scene) error {
			other := s
			other.run = id()
			return all(tx, queued(s), running, computed, final, queued(other), step(`UPDATE consolidation.run SET status = 'running', version = version + 1
				WHERE id = $1`, other.run), step(`UPDATE consolidation.run SET status = 'computed', computed_at = now(), version = version + 1 WHERE id = $1`,
				other.run), step(`UPDATE consolidation.run SET status = 'final', finalized_by = updated_by, finalized_at = now(), version = version + 1
				WHERE id = $1`, other.run))
		}, "consolidation_run_one_final"},
		{"a running run finalized", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, final)
		}, "cannot change from running to final"},
		{"a discarded run brought back", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), failed, to("discarded", ""), to("failed", ""))
		}, "cannot change from discarded to failed"},
		{"a running run failed", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, failed)
		}, ""},
		{"a computed run running again", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, computed, running)
		}, "cannot change from computed to running"},
		{"a final run discarded", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, computed, final, to("discarded", ""))
		}, "cannot change from final to discarded"},
		{"a failed run computed", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), failed, to("computed", ", problem = NULL, computed_at = now()"))
		}, "cannot change from failed to computed"},
		{"a run's period ending before it starts", func(tx pgx.Tx, s scene) error {
			return exec(tx, strings.Replace(runRow, "'2026-11-30', 7", "'2026-10-31', 7", 1), s.tenant, s.run, s.parent, s.member, 11, "queued", s.actor)
		}, "run_period_dates"},
		{"a run of its parent alone", func(tx pgx.Tx, s scene) error {
			return exec(tx, strings.Replace(runRow, "ARRAY[$3, $4]::uuid[]", "array_remove(ARRAY[$3, $4]::uuid[], $4)", 1),
				s.tenant, s.run, s.parent, s.member, 11, "queued", s.actor)
		}, "run_companies"},
		{"a computed run's time rewritten", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, computed, step(`UPDATE consolidation.run SET computed_at = now() - interval '1 day', version = version + 1`))
		}, "keeps what it consolidates and its outcome"},
		{"a final run's finalizer rewritten", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, computed, final, step(`UPDATE consolidation.run SET finalized_by = gen_random_uuid(), version = version + 1`))
		}, "keeps what it consolidates and its outcome"},
		{"a run failed without its problem", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), to("failed", ""))
		}, "run_problem"},
		{"a failed run's problem rewritten", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), failed, step(`UPDATE consolidation.run SET problem = '{"code": "x"}', version = version + 1`))
		}, "keeps what it consolidates and its outcome"},
		{"a run computed without when", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, to("computed", ""))
		}, "run_computed"},
		{"a run final without who finalized it", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, computed, to("final", ""))
		}, "run_finalized"},
		{"a run superseded by none", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, computed, final, to("superseded", ""))
		}, "run_superseded"},
		{"a run's period changed", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), step(`UPDATE consolidation.run SET fiscal_period = 10, version = version + 1`))
		}, "keeps what it consolidates"},
		{"a run without its parent among its companies", func(tx pgx.Tx, s scene) error {
			return exec(tx, strings.Replace(runRow, "ARRAY[$3, $4]", "ARRAY[$4, gen_random_uuid()]", 1), s.tenant, s.run, s.parent, s.member, 11, "queued",
				s.actor)
		}, "run_parent_included"},
		{"a run changed without its version", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), step(`UPDATE consolidation.run SET status = 'running'`))
		}, "optimistic lock"},
		{"a run deleted", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), step(`DELETE FROM consolidation.run`))
		}, "permission denied"},
		{"a member written before the run runs", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), member(s))
		}, "its rows are written while it runs"},
		{"a parent with an ownership", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, step(memberRow, s.tenant, s.run, s.parent, "3000", "parent", "100", nil, nil, "CAD", "1", nil))
		}, "run_member_role"},
		{"a member without its first period", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, step(memberRow, s.tenant, s.run, s.member, "1000", "member", "80", nil, nil, "USD", "1.4", nil))
		}, "run_member_role"},
		{"a member translated without its period", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, member(s), step(`UPDATE consolidation.run_member SET translated_at = now()`))
		}, "run_member_translated"},
		{"a member translated twice", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, member(s), translated, translated)
		}, "records its translation once"},
		{"a member's rate changed", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, member(s), step(`UPDATE consolidation.run_member SET closing_rate = 1.5`))
		}, "permission denied"},
		{"a member translated after the run computed", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, member(s), computed, translated)
		}, "its rows are written while it runs"},
		{"a member removed", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, member(s), step(`DELETE FROM consolidation.run_member`))
		}, "permission denied"},
		{"a balance written after the run computed", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, member(s), computed, balance(s, "balance", id(), nil, 100))
		}, "its rows are written while it runs"},
		{"a balance of no member", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, balance(s, "balance", id(), nil, 100))
		}, "run_balance_tenant_id_run_id_legal_entity_id_fkey"},
		{"a balance changed", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, member(s), balance(s, "balance", id(), nil, 100), step(`UPDATE consolidation.run_balance SET amount_gc = 0`))
		}, "permission denied"},
		{"a balance without its account", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, member(s), balance(s, "balance", nil, nil, 100))
		}, "run_balance_account"},
		{"earlier results on a company account", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, member(s), balance(s, "prior_results", id(), nil, 100))
		}, "run_balance_account"},
		{"a balance whose trading partner is its own company", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, member(s), balance(s, "balance", id(), s.member, 100))
		}, "run_balance_partner"},
		{"a translation difference with a local amount", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, member(s), balance(s, "cta", nil, nil, 100))
		}, "run_balance_amounts"},
		{"a translation difference", func(tx pgx.Tx, s scene) error {
			return all(tx, queued(s), running, member(s), balance(s, "cta", nil, nil, nil))
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), parent: id(), member: id(), run: id()}
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

// TestConsolidationRunIsTheTenants: another tenant neither sees a run nor
// adds members to it.
func TestConsolidationRunIsTheTenants(t *testing.T) {
	need(t)
	a, b, actor, run := id(), id(), id(), id()
	mustApp(t, a, func(tx pgx.Tx) error {
		return all(tx, step(`INSERT INTO consolidation.run (tenant_id, id, group_code, parent_id, companies, fiscal_year, fiscal_period, period_start,
			period_end, config_version, group_currency, rate_type, status, operation_id, created_by, updated_by) VALUES ($1, $2, 'MAPLE', $3,
			ARRAY[$3, gen_random_uuid()], 2026, 11, '2026-11-01', '2026-11-30', 1, 'CAD', 'average', 'queued', gen_random_uuid(), $3, $3)`, a, run, actor),
			step(`UPDATE consolidation.run SET status = 'running', version = version + 1`))
	})
	err := asApp(t, b, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM consolidation.run`).Scan(&n); err != nil || n != 0 {
			t.Errorf("another tenant's runs: got %d (%v), want none", n, err)
		}
		return exec(tx, `INSERT INTO consolidation.run_member (tenant_id, run_id, legal_entity_id, company_code, role, local_currency, closing_rate)
			VALUES ($1, $2, gen_random_uuid(), '3000', 'parent', 'CAD', 1)`, b, run)
	})
	failsWith(t, err, "is <NULL>, not running")
}

// TestConsolidationEliminationRules (T68f, migration 000233; ADR-0059 §5,
// §8): a run's differences, entries and entry lines are written while it
// runs and neither changed nor removed; a pair has both sides, their sum
// and the tolerance, and its status is the difference's against the
// tolerance; outside_group and no_trading_partner have the amount alone,
// the latter no partner; one difference per elimination, company and
// partner, none included; an intercompany entry names its elimination and
// partner, a company of the run, once per pair, and the others neither;
// a line is never zero, names an account or trading partner only with
// its company, and never its own company as partner; each entry has two
// lines or more summing to zero at commit.
func TestConsolidationEliminationRules(t *testing.T) {
	need(t)
	const (
		runRow = `INSERT INTO consolidation.run (tenant_id, id, group_code, parent_id, companies, fiscal_year, fiscal_period, period_start, period_end,
			config_version, group_currency, rate_type, status, operation_id, created_by, updated_by)
			VALUES ($1, $2, 'MAPLE', $3, ARRAY[$3, $4]::uuid[], 2026, 11, '2026-11-01', '2026-11-30', 7, 'CAD', 'average', 'queued', gen_random_uuid(), $5, $5)`
		memberRow = `INSERT INTO consolidation.run_member (tenant_id, run_id, legal_entity_id, company_code, role, ownership_percent, first_fiscal_year,
			first_fiscal_period, local_currency, closing_rate) VALUES ($1, $2, $3, '3000', 'parent', NULL, NULL, NULL, 'CAD', 1),
			($1, $2, $4, '1000', 'member', 80, 2026, 11, 'USD', 1.4)`
		diffRow = `INSERT INTO consolidation.run_difference (tenant_id, id, run_id, line_no, elimination_code, legal_entity_id, partner_id, status, amount,
			partner_amount, difference, tolerance) VALUES ($1, gen_random_uuid(), $2, $3, 'IC_BALANCES', $4, $5, $6, $7, $8, $9, $10)`
		entryRow = `INSERT INTO consolidation.run_entry (tenant_id, run_id, entry_no, kind, legal_entity_id, partner_id, elimination_code)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`
		lineRow = `INSERT INTO consolidation.run_entry_line (tenant_id, run_id, entry_no, line_no, group_account, legal_entity_id, account_id,
			trading_partner_id, amount_gc) VALUES ($1, $2, 1, $3, 'G1250', $4, $5, $6, $7)`
	)
	type scene struct{ tenant, actor, parent, member, run uuid.UUID }
	running := func(s scene) stmt {
		return step(`WITH r AS (`+runRow+` RETURNING 1) SELECT 1`, s.tenant, s.run, s.parent, s.member, s.actor)
	}
	start := func(s scene) []stmt {
		return []stmt{running(s), step(`UPDATE consolidation.run SET status = 'running', version = version + 1`),
			step(memberRow, s.tenant, s.run, s.parent, s.member)}
	}
	pair := func(s scene, status string, amount, partner, diff, tol any) stmt {
		return step(diffRow, s.tenant, s.run, 1, s.member, s.parent, status, amount, partner, diff, tol)
	}
	alone := func(s scene, status string, partner any) stmt {
		return step(diffRow, s.tenant, s.run, 2, s.member, partner, status, 45, nil, nil, nil)
	}
	entry := func(s scene) stmt {
		return step(entryRow, s.tenant, s.run, 1, "intercompany", s.member, s.parent, "IC_BALANCES")
	}
	line := func(s scene, no int, company, account, partner, amount any) stmt {
		return step(lineRow, s.tenant, s.run, no, company, account, partner, amount)
	}
	balanced := func(s scene) []stmt {
		return []stmt{entry(s), line(s, 1, s.member, id(), s.parent, -2800), line(s, 2, s.parent, id(), s.member, 2700), line(s, 3, nil, nil, nil, 100)}
	}
	atCommit := step(`SET CONSTRAINTS ALL IMMEDIATE`)
	with := func(s scene, rest ...stmt) []stmt { return append(start(s), rest...) }
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"differences of every status and a balanced entry", func(s scene) []stmt {
			return with(s, append([]stmt{pair(s, "open", -2800, 2700, -100, 10), alone(s, "no_trading_partner", nil),
				step(diffRow, s.tenant, s.run, 3, s.member, id(), "outside_group", 45, nil, nil, nil),
				step(diffRow, s.tenant, s.run, 4, s.parent, s.member, "matched", 10, -10, 0, 0),
				step(strings.Replace(diffRow, "'IC_BALANCES'", "'IC_RESULTS'", 1), s.tenant, s.run, 5, s.member, s.parent, "within_tolerance", 10, -9, 1, 100)},
				append(balanced(s), atCommit)...)...)
		}, ""},
		{"a difference after the run computed", func(s scene) []stmt {
			return with(s, step(`UPDATE consolidation.run SET status = 'computed', computed_at = now(), version = version + 1`), pair(s, "matched", 1, -1, 0, 0))
		}, "its rows are written while it runs"},
		{"a difference changed", func(s scene) []stmt {
			return with(s, pair(s, "open", -2800, 2700, -100, 10), step(`UPDATE consolidation.run_difference SET amount = 0`))
		}, "permission denied"},
		{"a difference removed", func(s scene) []stmt {
			return with(s, pair(s, "open", -2800, 2700, -100, 10), step(`DELETE FROM consolidation.run_difference`))
		}, "permission denied"},
		{"a match with a difference", func(s scene) []stmt { return with(s, pair(s, "matched", -2800, 2700, -100, 1000)) }, "run_difference_status"},
		{"within a tolerance it exceeds", func(s scene) []stmt { return with(s, pair(s, "within_tolerance", -2800, 2700, -100, 10)) }, "run_difference_status"},
		{"open above its tolerance", func(s scene) []stmt { return with(s, pair(s, "open", -2800, 2700, -100, 10)) }, ""},
		{"open below its tolerance", func(s scene) []stmt { return with(s, pair(s, "open", -2800, 2795, -5, 1000)) }, "run_difference_status"},
		{"a pair without its partner's amount", func(s scene) []stmt { return with(s, pair(s, "open", -2800, nil, -100, 10)) }, "run_difference_pair"},
		{"a difference that is not the sum", func(s scene) []stmt { return with(s, pair(s, "open", -2800, 2700, -50, 10)) }, "run_difference_pair"},
		{"a negative tolerance", func(s scene) []stmt { return with(s, pair(s, "matched", 1, -1, 0, -1)) }, "run_difference_tolerance_check"},
		{"outside the group with a tolerance", func(s scene) []stmt {
			return with(s, step(diffRow, s.tenant, s.run, 2, s.member, id(), "outside_group", 45, nil, nil, 10))
		}, "run_difference_pair"},
		{"no trading partner naming one", func(s scene) []stmt { return with(s, alone(s, "no_trading_partner", id())) }, "run_difference_partner"},
		{"outside the group naming none", func(s scene) []stmt { return with(s, alone(s, "outside_group", nil)) }, "run_difference_partner"},
		{"a pair of a company with itself", func(s scene) []stmt {
			return with(s, step(diffRow, s.tenant, s.run, 1, s.member, s.member, "matched", 1, -1, 0, 0))
		}, "run_difference_partner"},
		{"a pair twice", func(s scene) []stmt {
			return with(s, pair(s, "matched", 1, -1, 0, 0), step(diffRow, s.tenant, s.run, 2, s.member, s.parent, "matched", 1, -1, 0, 0))
		}, "duplicate key"},
		{"a company without a trading partner twice", func(s scene) []stmt {
			return with(s, alone(s, "no_trading_partner", nil), step(diffRow, s.tenant, s.run, 3, s.member, nil, "no_trading_partner", 5, nil, nil, nil))
		}, "duplicate key"},
		{"a difference of no company of the run", func(s scene) []stmt {
			return with(s, step(diffRow, s.tenant, s.run, 1, id(), s.parent, "matched", 1, -1, 0, 0))
		}, "run_difference_tenant_id_run_id_legal_entity_id_fkey"},
		{"an entry after the run computed", func(s scene) []stmt {
			return with(s, step(`UPDATE consolidation.run SET status = 'computed', computed_at = now(), version = version + 1`), entry(s))
		}, "its rows are written while it runs"},
		{"an entry line changed", func(s scene) []stmt {
			return with(s, append(balanced(s), step(`UPDATE consolidation.run_entry_line SET amount_gc = 1`))...)
		}, "permission denied"},
		{"an entry's lines removed", func(s scene) []stmt {
			return with(s, append(balanced(s), step(`DELETE FROM consolidation.run_entry_line`))...)
		}, "permission denied"},
		{"an unbalanced entry", func(s scene) []stmt {
			return with(s, entry(s), line(s, 1, s.member, id(), s.parent, -2800), line(s, 2, s.parent, id(), s.member, 2700), atCommit)
		}, "2 line(s) summing to -100"},
		{"an entry of one line", func(s scene) []stmt { return with(s, entry(s), line(s, 1, nil, nil, nil, 100), atCommit) }, "1 line(s) summing to 100"},
		{"a line of zero", func(s scene) []stmt { return with(s, entry(s), line(s, 1, nil, nil, nil, 0)) }, "run_entry_line_amount_gc_check"},
		{"a line's account without its company", func(s scene) []stmt { return with(s, entry(s), line(s, 1, nil, id(), nil, 1)) }, "run_entry_line_company"},
		{"a line's trading partner without its company", func(s scene) []stmt {
			return with(s, entry(s), line(s, 1, nil, nil, s.parent, 1))
		}, "run_entry_line_company"},
		{"a line naming its own company as partner", func(s scene) []stmt {
			return with(s, entry(s), line(s, 1, s.member, id(), s.member, 1))
		}, "run_entry_line_company"},
		{"a line of no entry", func(s scene) []stmt { return with(s, line(s, 1, nil, nil, nil, 1)) }, "run_entry_line_tenant_id_run_id_entry_no_fkey"},
		{"an intercompany entry without its partner", func(s scene) []stmt {
			return with(s, step(entryRow, s.tenant, s.run, 1, "intercompany", s.member, nil, "IC_BALANCES"))
		}, "run_entry_pair"},
		{"an investment entry of an elimination", func(s scene) []stmt {
			return with(s, step(entryRow, s.tenant, s.run, 1, "investment", s.member, nil, "IC_BALANCES"))
		}, "run_entry_pair"},
		{"an entry of a company with itself", func(s scene) []stmt {
			return with(s, step(entryRow, s.tenant, s.run, 1, "intercompany", s.member, s.member, "IC_BALANCES"))
		}, "run_entry_pair"},
		{"an entry with a partner outside the run", func(s scene) []stmt {
			return with(s, step(entryRow, s.tenant, s.run, 1, "intercompany", s.member, id(), "IC_BALANCES"))
		}, "run_entry_tenant_id_run_id_partner_id_fkey"},
		{"an entry line after the run computed", func(s scene) []stmt {
			return with(s, entry(s), step(`UPDATE consolidation.run SET status = 'computed', computed_at = now(), version = version + 1`),
				line(s, 1, nil, nil, nil, 1))
		}, "its rows are written while it runs"},
		{"a line of a company outside the run", func(s scene) []stmt {
			return with(s, entry(s), line(s, 1, id(), nil, nil, 1))
		}, "run_entry_line_tenant_id_run_id_legal_entity_id_fkey"},
		{"an entry of a company outside the run", func(s scene) []stmt {
			return with(s, step(entryRow, s.tenant, s.run, 1, "investment", id(), nil, nil))
		}, "run_entry_tenant_id_run_id_legal_entity_id_fkey"},
		{"two differences on one line", func(s scene) []stmt {
			return with(s, pair(s, "matched", 1, -1, 0, 0), step(diffRow, s.tenant, s.run, 1, s.member, nil, "no_trading_partner", 5, nil, nil, nil))
		}, "run_difference_tenant_id_run_id_line_no_key"},
		{"a difference on line 0", func(s scene) []stmt {
			return with(s, step(diffRow, s.tenant, s.run, 0, s.member, nil, "no_trading_partner", 5, nil, nil, nil))
		}, "run_difference_line_no_check"},
		{"an unknown status", func(s scene) []stmt {
			return with(s, step(diffRow, s.tenant, s.run, 1, s.member, id(), "accepted", 5, nil, nil, nil))
		}, "run_difference_status_check"},
		{"a difference of a malformed elimination code", func(s scene) []stmt {
			return with(s, step(strings.Replace(diffRow, "'IC_BALANCES'", "'ic balances'", 1), s.tenant, s.run, 1, s.member, nil, "no_trading_partner", 5,
				nil, nil, nil))
		}, "run_difference_elimination_code_check"},
		{"an entry of an unknown kind", func(s scene) []stmt {
			return with(s, step(entryRow, s.tenant, s.run, 1, "goodwill", s.member, nil, nil))
		}, "run_entry_kind_check"},
		{"an entry of a malformed elimination code", func(s scene) []stmt {
			return with(s, step(entryRow, s.tenant, s.run, 1, "intercompany", s.member, s.parent, "ic balances"))
		}, "run_entry_elimination_code_check"},
		{"entry 0", func(s scene) []stmt {
			return with(s, step(entryRow, s.tenant, s.run, 0, "intercompany", s.member, s.parent, "IC_BALANCES"))
		}, "run_entry_entry_no_check"},
		{"a line 0", func(s scene) []stmt { return with(s, entry(s), line(s, 0, nil, nil, nil, 1)) }, "run_entry_line_line_no_check"},
		{"a line on a malformed group account", func(s scene) []stmt {
			return with(s, entry(s), step(strings.Replace(lineRow, "'G1250'", "'g-1250'", 1), s.tenant, s.run, 1, nil, nil, nil, 1))
		}, "run_entry_line_group_account_check"},
		{"an entry changed", func(s scene) []stmt {
			return with(s, append(balanced(s), step(`UPDATE consolidation.run_entry SET kind = 'nci'`))...)
		}, "permission denied"},
		{"an entry itself removed", func(s scene) []stmt {
			return with(s, append(balanced(s), step(`DELETE FROM consolidation.run_entry`))...)
		}, "permission denied"},
		{"a pair's entry twice", func(s scene) []stmt {
			return with(s, entry(s), step(entryRow, s.tenant, s.run, 2, "intercompany", s.member, s.parent, "IC_BALANCES"))
		}, "run_entry_one_per_pair"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), actor: id(), parent: id(), member: id(), run: id()}
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
