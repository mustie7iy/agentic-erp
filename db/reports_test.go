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
