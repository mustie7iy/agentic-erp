package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestReportPermissions (T95a, migration 000308; ADR-0076 §3): the
// permissions of saved reports are seeded and are no approval
// permissions.
func TestReportPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'analytics.report.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "analytics.report.read analytics.report.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// TestReportRules (T95a, migration 000308; ADR-0076 §1-§2): a report has
// a title that is not white space, a question of at most 2000
// characters, a specification and queries that are objects, a known
// status and a version from 1; a change sets its status with the next
// version and nothing else of it; a removed report is fixed; no report
// is deleted.
func TestReportRules(t *testing.T) {
	need(t)
	const report = `INSERT INTO analytics.report (tenant_id, id, title, question, spec, queries, owner_id) VALUES ($1, $2, $3, $4, $5, $6, $1)`
	type scene struct{ tenant, report uuid.UUID }
	with := func(s scene, title string, question any, spec, queries string) stmt {
		return step(report, s.tenant, s.report, title, question, spec, queries)
	}
	save := func(s scene) stmt {
		return with(s, "Revenue", "What is our revenue?", `{"title": "Revenue"}`, `{"rev": {"measures": ["revenue"]}}`)
	}
	remove := func(s scene) stmt {
		return step(`UPDATE analytics.report SET status = 'removed', version = version + 1 WHERE id = $1`, s.report)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a report", func(s scene) []stmt { return []stmt{save(s)} }, ""},
		{"a report without a question", func(s scene) []stmt { return []stmt{with(s, "Revenue", nil, `{}`, `{}`)} }, ""},
		{"a title of white space", func(s scene) []stmt { return []stmt{with(s, " ", nil, `{}`, `{}`)} }, "report_title_check"},
		{"a title of 201 characters", func(s scene) []stmt { return []stmt{with(s, strings.Repeat("t", 201), nil, `{}`, `{}`)} }, "report_title_check"},
		{"a question of 2001 characters", func(s scene) []stmt {
			return []stmt{with(s, "Revenue", strings.Repeat("q", 2001), `{}`, `{}`)}
		}, "report_question_check"},
		{"a specification that is no object", func(s scene) []stmt { return []stmt{with(s, "Revenue", nil, `[]`, `{}`)} }, "report_spec_check"},
		{"queries that are no object", func(s scene) []stmt { return []stmt{with(s, "Revenue", nil, `{}`, `"q"`)} }, "report_queries_check"},
		{"saved at version 0", func(s scene) []stmt {
			return []stmt{step(`INSERT INTO analytics.report (tenant_id, id, title, spec, queries, owner_id, version) VALUES ($1, $2, 'R', '{}', '{}', $1, 0)`,
				s.tenant, s.report)}
		}, "report_version_check"},
		{"an unknown status", func(s scene) []stmt {
			return []stmt{save(s), step(`UPDATE analytics.report SET status = 'draft', version = version + 1 WHERE id = $1`, s.report)}
		}, "report_status_check"},
		{"removed", func(s scene) []stmt { return []stmt{save(s), remove(s)} }, ""},
		{"removed without the next version", func(s scene) []stmt {
			return []stmt{save(s), step(`UPDATE analytics.report SET status = 'removed' WHERE id = $1`, s.report)}
		}, "optimistic lock"},
		{"the title changed", func(s scene) []stmt {
			return []stmt{save(s), step(`UPDATE analytics.report SET title = 'Other', version = version + 1 WHERE id = $1`, s.report)}
		}, "permission denied"},
		{"the owner changed", func(s scene) []stmt {
			return []stmt{save(s), step(`UPDATE analytics.report SET owner_id = gen_random_uuid(), version = version + 1 WHERE id = $1`, s.report)}
		}, "permission denied"},
		{"a removed report made active again", func(s scene) []stmt {
			return []stmt{save(s), remove(s), step(`UPDATE analytics.report SET status = 'active', version = version + 1 WHERE id = $1`, s.report)}
		}, "report_removed_fixed"},
		{"a report deleted", func(s scene) []stmt { return []stmt{save(s), step(`DELETE FROM analytics.report`)} }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), report: id()}
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

// TestReportRemovedFixed (T95a, migration 000308): a removed report
// stays as it is for the owner of the table too, whom no grant holds
// back.
func TestReportRemovedFixed(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, report := id(), id()
	if err := all(tx, step(`INSERT INTO analytics.report (tenant_id, id, title, spec, queries, owner_id, status) VALUES ($1, $2, 'R', '{}', '{}', $1, 'removed')`,
		tenant, report)); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `UPDATE analytics.report SET title = 'Other', version = version + 1 WHERE id = $1`, report)
	failsWith(t, err, "report_removed_fixed")
}

// TestReportScheduleRules (T95b, migration 000309; ADR-0076 §4): a
// schedule is whole, a known frequency with the weekday of a weekly one
// and the day of a monthly one alone, by a company code of 1 to 20
// characters; a removed report has none.
func TestReportScheduleRules(t *testing.T) {
	need(t)
	const report = `INSERT INTO analytics.report (tenant_id, id, title, spec, queries, owner_id) VALUES ($1, $2, 'R', '{}', '{}', $1)`
	type scene struct{ tenant, report uuid.UUID }
	set := func(s scene, frequency, company, weekday, day, from any) stmt {
		return step(`UPDATE analytics.report SET schedule_frequency = $2, schedule_company = $3, schedule_weekday = $4, schedule_day = $5,
			scheduled_from = $6, version = version + 1 WHERE id = $1`, s.report, frequency, company, weekday, day, from)
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"daily", func(s scene) []stmt { return []stmt{set(s, "daily", "1000", nil, nil, "2026-11-13")} }, ""},
		{"weekly on Sunday", func(s scene) []stmt { return []stmt{set(s, "weekly", "1000", 7, nil, "2026-11-13")} }, ""},
		{"monthly on the 28th", func(s scene) []stmt { return []stmt{set(s, "monthly", "1000", nil, 28, "2026-11-13")} }, ""},
		{"hourly", func(s scene) []stmt { return []stmt{set(s, "hourly", "1000", nil, nil, "2026-11-13")} }, "report_schedule_frequency_check"},
		{"a company of 21 characters", func(s scene) []stmt {
			return []stmt{set(s, "daily", strings.Repeat("c", 21), nil, nil, "2026-11-13")}
		}, "report_schedule_company_check"},
		{"weekday 8", func(s scene) []stmt { return []stmt{set(s, "weekly", "1000", 8, nil, "2026-11-13")} }, "report_schedule_weekday_check"},
		{"day 29", func(s scene) []stmt { return []stmt{set(s, "monthly", "1000", nil, 29, "2026-11-13")} }, "report_schedule_day_check"},
		{"weekly without its weekday", func(s scene) []stmt { return []stmt{set(s, "weekly", "1000", nil, nil, "2026-11-13")} }, "report_schedule_whole"},
		{"daily with a day", func(s scene) []stmt { return []stmt{set(s, "daily", "1000", nil, 3, "2026-11-13")} }, "report_schedule_whole"},
		{"without a company", func(s scene) []stmt { return []stmt{set(s, "daily", nil, nil, nil, "2026-11-13")} }, "report_schedule_whole"},
		{"without its first day", func(s scene) []stmt { return []stmt{set(s, "daily", "1000", nil, nil, nil)} }, "report_schedule_whole"},
		{"removed with a schedule", func(s scene) []stmt {
			return []stmt{set(s, "daily", "1000", nil, nil, "2026-11-13"),
				step(`UPDATE analytics.report SET status = 'removed', version = version + 1 WHERE id = $1`, s.report)}
		}, "report_removed_unscheduled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), report: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error {
				return all(tx, append([]stmt{step(report, s.tenant, s.report)}, c.run(s)...)...)
			})
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

// TestReportRunRules (T95b, migration 000309; ADR-0076 §5): a run is of a
// report of the tenant, once per day, a success with its results' key,
// SHA-256 and date and no problem, a failure with a problem and no
// results; never changed or deleted.
func TestReportRunRules(t *testing.T) {
	need(t)
	const report = `INSERT INTO analytics.report (tenant_id, id, title, spec, queries, owner_id) VALUES ($1, $2, 'R', '{}', '{}', $1)`
	const run = `INSERT INTO analytics.report_run (tenant_id, id, report_id, run_date, status, run_as, object_key, sha256, data_through, failure_code, failure_detail)
		VALUES ($1, $2, $3, $4, $5, $1, $6, $7, $8, $9, $10)`
	type scene struct{ tenant, report, run uuid.UUID }
	sum := make([]byte, 32)
	ok := func(s scene) stmt {
		return step(run, s.tenant, s.run, s.report, "2026-11-13", "succeeded", "k", sum, "2026-11-12", nil, nil)
	}
	failed := func(s scene) stmt {
		return step(run, s.tenant, s.run, s.report, "2026-11-13", "failed", nil, nil, nil, "x.refused", "Refused.")
	}
	cases := []struct {
		name string
		run  func(s scene) []stmt
		want string // empty: allowed
	}{
		{"a success", func(s scene) []stmt { return []stmt{ok(s)} }, ""},
		{"a failure", func(s scene) []stmt { return []stmt{failed(s)} }, ""},
		{"a second run of the day", func(s scene) []stmt {
			return []stmt{ok(s), step(run, s.tenant, id(), s.report, "2026-11-13", "failed", nil, nil, nil, "x.refused", "Refused.")}
		}, "report_run_once"},
		{"a run of the next day", func(s scene) []stmt {
			return []stmt{ok(s), step(run, s.tenant, id(), s.report, "2026-11-14", "failed", nil, nil, nil, "x.refused", "Refused.")}
		}, ""},
		{"an unknown status", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.report, "2026-11-13", "running", nil, nil, nil, nil, nil)}
		}, "report_run_status_check"},
		{"a success without results", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.report, "2026-11-13", "succeeded", nil, nil, nil, nil, nil)}
		}, "report_run_outcome"},
		{"a success with a problem", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.report, "2026-11-13", "succeeded", "k", sum, "", "x.refused", "Refused.")}
		}, "report_run_outcome"},
		{"a failure without its detail", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.report, "2026-11-13", "failed", nil, nil, nil, "x.refused", nil)}
		}, "report_run_outcome"},
		{"a SHA-256 of 31 bytes", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.report, "2026-11-13", "succeeded", "k", sum[:31], "", nil, nil)}
		}, "report_run_sha256_check"},
		{"a key of 1025 characters", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.report, "2026-11-13", "succeeded", strings.Repeat("k", 1025), sum, "", nil, nil)}
		}, "report_run_object_key_check"},
		{"a date of 11 characters", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.report, "2026-11-13", "succeeded", "k", sum, "2026-11-123", nil, nil)}
		}, "report_run_data_through_check"},
		{"a code of 101 characters", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.report, "2026-11-13", "failed", nil, nil, nil, strings.Repeat("c", 101), "x")}
		}, "report_run_failure_code_check"},
		{"a detail of 2001 characters", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, s.report, "2026-11-13", "failed", nil, nil, nil, "x.refused", strings.Repeat("d", 2001))}
		}, "report_run_failure_detail_check"},
		{"a run of no report", func(s scene) []stmt {
			return []stmt{step(run, s.tenant, s.run, id(), "2026-11-13", "failed", nil, nil, nil, "x.refused", "Refused.")}
		}, "report_run_report_fkey"},
		{"a run changed", func(s scene) []stmt {
			return []stmt{ok(s), step(`UPDATE analytics.report_run SET status = 'failed' WHERE id = $1`, s.run)}
		}, "permission denied"},
		{"a run deleted", func(s scene) []stmt { return []stmt{ok(s), step(`DELETE FROM analytics.report_run`)} }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scene{tenant: id(), report: id(), run: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error {
				return all(tx, append([]stmt{step(report, s.tenant, s.report)}, c.run(s)...)...)
			})
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

// TestReportRunImmutable (T95b, migration 000309): a run stays as it was
// kept for the owner of the table too, whom no grant holds back.
func TestReportRunImmutable(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tenant, report, run := id(), id(), id()
	if err := all(tx, step(`INSERT INTO analytics.report (tenant_id, id, title, spec, queries, owner_id) VALUES ($1, $2, 'R', '{}', '{}', $1)`, tenant, report),
		step(`INSERT INTO analytics.report_run (tenant_id, id, report_id, run_date, status, run_as, failure_code, failure_detail)
			VALUES ($1, $2, $3, '2026-11-13', 'failed', $1, 'x.refused', 'Refused.')`, tenant, run, report)); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`UPDATE analytics.report_run SET failure_detail = 'Other.' WHERE id = $1`, `DELETE FROM analytics.report_run WHERE id = $1`} {
		_, err := tx.Exec(ctx, "SAVEPOINT s")
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, sql, run)
		failsWith(t, err, "insert-only")
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
	}
}
