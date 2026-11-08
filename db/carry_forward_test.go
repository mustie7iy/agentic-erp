package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The balance carry-forward (ADR-0060 §4-§6, §14, T64m2, migrations 000227
// and 000228): a period counts its openings; a run's years are consecutive,
// it chains only from the previous year's run, carries to an equity
// account, moves only from current to superseded naming its successor, is
// the only current one of its company, ledger and year; its balance and
// period rows are never changed, and a result row is on its retained
// earnings account alone.

// refused asserts err is a database error of the constraint or, for a
// grant or a trigger without one, whose message contains want; empty want:
// no error.
func refused(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		expect(t, err, "")
		return
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("got %v, want a database error of %s", err, want)
	}
	if pg.ConstraintName != want && !strings.Contains(pg.Message, want) {
		t.Fatalf("got %s (%q), want %s", pg.ConstraintName, pg.Message, want)
	}
}

// carryForward is f with an equity account and a 2025 run of its ledger
// carrying to it, its period rows recorded.
type carryForward struct {
	fixture
	equity, run uuid.UUID
}

func newCarryForward(t *testing.T) carryForward {
	t.Helper()
	c := carryForward{fixture: newFixture(t), equity: id(), run: id()}
	mustApp(t, c.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, status, created_by, updated_by)
			VALUES ($1,$2,$3,'330000','Retained earnings','equity','active',$4,$4)`, c.tenant, c.equity, c.chart, c.actor); err != nil {
			return err
		}
		if err := exec(tx, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by) VALUES ($1,$2,$3,$4,$4)`,
			c.tenant, c.entity, c.equity, c.actor); err != nil {
			return err
		}
		return c.insertRun(tx, c.run, 2025, nil, c.equity)
	})
	return c
}

// insertRun inserts a current run of c's company and ledger from year,
// chained from chained (all years when nil), carrying to account.
func (c carryForward) insertRun(tx pgx.Tx, run uuid.UUID, year int, chained *uuid.UUID, account uuid.UUID) error {
	basis := "all_years"
	if chained != nil {
		basis = "chained"
	}
	return exec(tx, `INSERT INTO ledger.carry_forward_run (tenant_id, id, legal_entity_id, ledger_id, from_fiscal_year, to_fiscal_year, basis,
		chained_from_id, retained_earnings_account_id, row_count, created_by) VALUES ($1,$2,$3,$4,$5::smallint,$5::smallint + 1,$6,$7,$8,0,$9)`,
		c.tenant, run, c.entity, c.ledger, year, basis, chained, account, c.actor)
}

// TestPeriodOpenedCount: a period starts at 0 openings (1 when inserted
// open), each change to open adds one whoever writes it, closing and
// locking add none, and the count is never set (ADR-0060 §6).
func TestPeriodOpenedCount(t *testing.T) {
	need(t)
	f := newFixture(t)
	count := func(t *testing.T) (n int) {
		t.Helper()
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT opened_count FROM ledger.period_status WHERE legal_entity_id = $1 AND fiscal_year = 2026
				AND period = 1`, f.entity).Scan(&n)
		})
		return n
	}
	set := func(t *testing.T, status string) {
		t.Helper()
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.period_status SET status = $2, version = version + 1 WHERE legal_entity_id = $1 AND fiscal_year = 2026 AND period = 1`, f.entity, status)
		})
	}
	if got := count(t); got != 0 {
		t.Fatalf("a new period's count: got %d, want 0", got)
	}
	for _, step := range []struct {
		status string
		want   int
	}{{"open", 1}, {"closed", 1}, {"open", 2}, {"closed", 2}, {"locked", 2}} {
		set(t, step.status)
		if got := count(t); got != step.want {
			t.Fatalf("the count after %s: got %d, want %d", step.status, got, step.want)
		}
	}
	t.Run("set directly", func(t *testing.T) {
		refused(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			return exec(tx, `UPDATE ledger.period_status SET opened_count = 5, version = version + 1 WHERE legal_entity_id = $1 AND fiscal_year = 2026 AND period = 2`, f.entity)
		}), "period_status_opened_count_set")
	})
	t.Run("inserted open", func(t *testing.T) {
		var n int
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT opened_count FROM ledger.period_status WHERE legal_entity_id = $1 AND fiscal_year = 2026
				AND period = 2`, f.entity).Scan(&n)
		})
		if n != 1 {
			t.Errorf("a period inserted open: got a count of %d, want 1", n)
		}
	})
	t.Run("inserted above 0", func(t *testing.T) {
		refused(t, asApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, "INSERT INTO ledger.fiscal_period VALUES ($1,$2,2026,13,'2026-12-01','2026-12-31',true)", f.tenant, f.calendar); err != nil {
				return err
			}
			return exec(tx, `INSERT INTO ledger.period_status (tenant_id, id, legal_entity_id, ledger_id, calendar_id, fiscal_year, period, status,
				updated_by, opened_count) VALUES ($1,gen_random_uuid(),$2,$3,$4,2026,13,'closed',$5,1)`, f.tenant, f.entity, f.ledger, f.calendar, f.actor)
		}), "period_status_opened_count_set")
	})
}

// TestRetainedEarningsDetermination: the account determination takes the
// retained earnings key.
func TestRetainedEarningsDetermination(t *testing.T) {
	need(t)
	c := newCarryForward(t)
	expect(t, asApp(t, c.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by)
			VALUES ($1,$2,'retained_earnings',$3,$4)`, c.tenant, c.entity, c.equity, c.actor)
	}), "")
}

// TestCarryForwardRunRules: a run's years are consecutive, it chains only
// from its company and ledger's run of the previous year, carries to an
// equity account, and is the one current run of its year.
func TestCarryForwardRunRules(t *testing.T) {
	need(t)
	c := newCarryForward(t)
	other := id()
	mustApp(t, c.tenant, func(tx pgx.Tx) error { return c.insertRun(tx, other, 2023, nil, c.equity) })
	for _, tc := range []struct {
		name string
		run  func(tx pgx.Tx) error
		want string // empty: allowed
	}{
		{"chained from the previous year", func(tx pgx.Tx) error { return c.insertRun(tx, id(), 2026, &c.run, c.equity) }, ""},
		{"chained from another year", func(tx pgx.Tx) error { return c.insertRun(tx, id(), 2026, &other, c.equity) }, "carry_forward_run_chain"},
		{"chained from another ledger's run", func(tx pgx.Tx) error {
			ledger2, run := id(), id()
			if err := exec(tx, `INSERT INTO ledger.ledger (tenant_id, id, code, name, is_leading, accounting_principle, created_by)
				VALUES ($1,$2,'2L','Local',false,'HGB',$3)`, c.tenant, ledger2, c.actor); err != nil {
				return err
			}
			if err := exec(tx, `INSERT INTO ledger.carry_forward_run (tenant_id, id, legal_entity_id, ledger_id, from_fiscal_year, to_fiscal_year, basis,
				retained_earnings_account_id, row_count, created_by) VALUES ($1,$2,$3,$4,2025,2026,'all_years',$5,0,$6)`,
				c.tenant, run, c.entity, ledger2, c.equity, c.actor); err != nil {
				return err
			}
			return c.insertRun(tx, id(), 2026, &run, c.equity)
		}, "carry_forward_run_chain"},
		{"a second current run of the year", func(tx pgx.Tx) error { return c.insertRun(tx, id(), 2025, nil, c.equity) }, "carry_forward_run_current"},
		{"to an expense account", func(tx pgx.Tx) error { return c.insertRun(tx, id(), 2024, nil, c.expense) }, "carry_forward_run_retained_earnings"},
		{"years not consecutive", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.carry_forward_run (tenant_id, id, legal_entity_id, ledger_id, from_fiscal_year, to_fiscal_year, basis,
				retained_earnings_account_id, row_count, created_by) VALUES ($1,$2,$3,$4,2022,2024,'all_years',$5,0,$6)`,
				c.tenant, id(), c.entity, c.ledger, c.equity, c.actor)
		}, "carry_forward_run_years"},
		{"chained without a predecessor", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.carry_forward_run (tenant_id, id, legal_entity_id, ledger_id, from_fiscal_year, to_fiscal_year, basis,
				retained_earnings_account_id, row_count, created_by) VALUES ($1,$2,$3,$4,2024,2025,'chained',$5,0,$6)`,
				c.tenant, id(), c.entity, c.ledger, c.equity, c.actor)
		}, "carry_forward_run_basis"},
		{"inserted superseded", func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.carry_forward_run (tenant_id, id, legal_entity_id, ledger_id, from_fiscal_year, to_fiscal_year, basis,
				retained_earnings_account_id, status, superseded_by_id, superseded_at, row_count, created_by)
				VALUES ($1,$2,$3,$4,2024,2025,'all_years',$5,'superseded',$6,now(),0,$7)`, c.tenant, id(), c.entity, c.ledger, c.equity, c.run, c.actor)
		}, "carry_forward_run_lifecycle"},
	} {
		t.Run(tc.name, func(t *testing.T) { refused(t, asApp(t, c.tenant, tc.run), tc.want) })
	}
}

// TestCarryForwardRunSuperseded: a run moves from current to superseded
// once, naming a successor that exists at commit, and nothing else of it
// changes; it is never deleted.
func TestCarryForwardRunSuperseded(t *testing.T) {
	need(t)
	supersede := func(c carryForward, tx pgx.Tx, successor uuid.UUID) error {
		return exec(tx, `UPDATE ledger.carry_forward_run SET status = 'superseded', superseded_by_id = $2, superseded_at = now(), version = version + 1
			WHERE id = $1`, c.run, successor)
	}
	succeeded := func(c carryForward) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			next := id()
			if err := supersede(c, tx, next); err != nil {
				return err
			}
			return c.insertRun(tx, next, 2025, nil, c.equity)
		}
	}
	for _, tc := range []struct {
		name string
		run  func(c carryForward) func(tx pgx.Tx) error
		want string // empty: allowed
	}{
		{"by a new run", succeeded, ""},
		{"naming no run", func(c carryForward) func(tx pgx.Tx) error {
			return func(tx pgx.Tx) error { return supersede(c, tx, id()) }
		}, "violates foreign key constraint"},
		{"without a successor", func(c carryForward) func(tx pgx.Tx) error {
			return func(tx pgx.Tx) error {
				return exec(tx, `UPDATE ledger.carry_forward_run SET status = 'superseded', superseded_at = now(), version = version + 1 WHERE id = $1`, c.run)
			}
		}, "carry_forward_run_superseded"},
		{"superseded with its rows' count changed", func(c carryForward) func(tx pgx.Tx) error {
			return func(tx pgx.Tx) error {
				next := id()
				if err := exec(tx, `UPDATE ledger.carry_forward_run SET status = 'superseded', superseded_by_id = $2, superseded_at = now(), row_count = 3,
					version = version + 1 WHERE id = $1`, c.run, next); err != nil {
					return err
				}
				return c.insertRun(tx, next, 2025, nil, c.equity)
			}
		}, "carry_forward_run_fixed"},
		{"superseded by itself", func(c carryForward) func(tx pgx.Tx) error {
			return func(tx pgx.Tx) error { return supersede(c, tx, c.run) }
		}, "carry_forward_run_superseded"},
		{"its rows' count changed", func(c carryForward) func(tx pgx.Tx) error {
			return func(tx pgx.Tx) error {
				return exec(tx, `UPDATE ledger.carry_forward_run SET row_count = 3, version = version + 1 WHERE id = $1`, c.run)
			}
		}, "carry_forward_run_lifecycle"},
		{"deleted", func(c carryForward) func(tx pgx.Tx) error {
			return func(tx pgx.Tx) error { return exec(tx, `DELETE FROM ledger.carry_forward_run WHERE id = $1`, c.run) }
		}, "permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCarryForward(t)
			refused(t, asApp(t, c.tenant, tc.run(c)), tc.want)
		})
	}
	c := newCarryForward(t)
	mustApp(t, c.tenant, succeeded(c))
	for _, tc := range []struct{ name, sql string }{
		{"current again", `UPDATE ledger.carry_forward_run SET status = 'current', superseded_by_id = NULL, superseded_at = NULL, version = version + 1 WHERE id = $1`},
		{"another successor", `UPDATE ledger.carry_forward_run SET superseded_by_id = $1, version = version + 1 WHERE id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refused(t, asApp(t, c.tenant, func(tx pgx.Tx) error { return exec(tx, tc.sql, c.run) }), "carry_forward_run_lifecycle")
		})
	}
}

// TestCarryForwardRows: a result row is on the run's retained earnings
// account alone, and balance and period rows are never changed.
func TestCarryForwardRows(t *testing.T) {
	need(t)
	c := newCarryForward(t)
	row := func(account uuid.UUID, kind string) func(tx pgx.Tx) error {
		return func(tx pgx.Tx) error {
			return exec(tx, `INSERT INTO ledger.carry_forward_balance (tenant_id, run_id, account_id, kind, currency_tc, amount_tc, amount_lc, amount_gc)
				VALUES ($1,$2,$3,$4,'USD',10,10,9)`, c.tenant, c.run, account, kind)
		}
	}
	for _, tc := range []struct {
		name string
		run  func(tx pgx.Tx) error
		want string // empty: allowed
	}{
		{"a balance row", row(c.bank, "balance"), ""},
		{"the result on retained earnings", row(c.equity, "result"), ""},
		{"a result on another account", row(c.bank, "result"), "carry_forward_balance_result"},
	} {
		t.Run(tc.name, func(t *testing.T) { refused(t, asApp(t, c.tenant, tc.run), tc.want) })
	}
	mustApp(t, c.tenant, func(tx pgx.Tx) error {
		return exec(tx, `INSERT INTO ledger.carry_forward_period (tenant_id, run_id, period_id, opened_count)
			SELECT tenant_id, $2, id, opened_count FROM ledger.period_status WHERE legal_entity_id = $1`, c.entity, c.run)
	})
	for _, tc := range []struct{ name, sql string }{
		{"a balance changed", `UPDATE ledger.carry_forward_balance SET amount_tc = 11 WHERE run_id = $1`},
		{"a balance deleted", `DELETE FROM ledger.carry_forward_balance WHERE run_id = $1`},
		{"a period's count changed", `UPDATE ledger.carry_forward_period SET opened_count = 9 WHERE run_id = $1`},
		{"a period deleted", `DELETE FROM ledger.carry_forward_period WHERE run_id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := testDB.Exec(context.Background(), tc.sql, c.run)
			refused(t, err, "is insert-only")
		})
	}
}
