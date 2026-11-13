package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestMetricWatcherPermissions (T96a, migration 000308; ADR-0077 §6): the
// watchers' permissions are seeded and are no approval permissions.
func TestMetricWatcherPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'analytics.watcher.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "analytics.watcher.read analytics.watcher.write"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// watcherInsert sets a watcher: $3 name, $4 metric, $5 filters, $6
// company, $7 grain, $8 condition, $9 threshold, $10 currency, $11
// percent.
const watcherInsert = `INSERT INTO analytics.metric_watcher (tenant_id, id, name, metric, filters, company, grain, condition, threshold, currency, percent,
	created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $1, $1)`

// watcherScene is a tenant and its watcher.
type watcherScene struct{ tenant, watcher uuid.UUID }

func (s watcherScene) with(name, metric, filters string, company *string, grain, condition string, threshold, currency, percent *string) stmt {
	return step(watcherInsert, s.tenant, s.watcher, name, metric, filters, company, grain, condition, threshold, currency, percent)
}

// above is a watcher of revenue above 400 EUR in every company by month.
func (s watcherScene) above() stmt {
	return s.with("Revenue", "revenue", "[]", nil, "month", "above", text("400"), text("EUR"), nil)
}

func (s watcherScene) end(status, code, detail string) stmt {
	return step(`UPDATE analytics.metric_watcher SET status = $2, failure_code = nullif($3, ''), failure_detail = nullif($4, ''),
		version = version + 1, updated_by = $1 WHERE id = $5`, s.tenant, status, code, detail, s.watcher)
}

func text(s string) *string { return &s }

// TestMetricWatcherRules (T96a, migration 000308; ADR-0077 §1): a watcher
// has a name, a metric, filters as an array, at most one company, a known
// grain and condition with exactly its settings, a value and a percentage
// as decimals; it ends removed or failed with why, once, at the next
// version; what it says and whose it is never change, and none is deleted.
func TestMetricWatcherRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s watcherScene) []stmt
		want string // empty: allowed
	}{
		{"a watcher above a value", func(s watcherScene) []stmt { return []stmt{s.above()} }, ""},
		{"a watcher of a change in one company", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", `[{"dim": "account", "op": "in", "values": ["400000"]}]`, text("2000"), "year", "change", nil, nil, text("12.5"))}
		}, ""},
		{"a count above a value", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "invoice_count", "[]", nil, "month", "below", text("-3.25"), nil, nil)}
		}, ""},
		{"a name of white space", func(s watcherScene) []stmt {
			return []stmt{s.with(" ", "revenue", "[]", nil, "month", "change", nil, nil, text("1"))}
		}, "metric_watcher_name_check"},
		{"a name of 201 characters", func(s watcherScene) []stmt {
			return []stmt{s.with(strings.Repeat("n", 201), "revenue", "[]", nil, "month", "change", nil, nil, text("1"))}
		}, "metric_watcher_name_check"},
		{"a metric of 101 characters", func(s watcherScene) []stmt {
			return []stmt{s.with("x", strings.Repeat("m", 101), "[]", nil, "month", "change", nil, nil, text("1"))}
		}, "metric_watcher_metric_check"},
		{"filters that are no array", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "{}", nil, "month", "change", nil, nil, text("1"))}
		}, "metric_watcher_filters_check"},
		{"a company of 21 characters", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", text(strings.Repeat("c", 21)), "month", "change", nil, nil, text("1"))}
		}, "metric_watcher_company_check"},
		{"an unknown grain", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", nil, "week", "change", nil, nil, text("1"))}
		}, "metric_watcher_grain_check"},
		{"an unknown condition", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", nil, "month", "equal", text("1"), nil, nil)}
		}, "metric_watcher_condition_check"},
		{"a value that is no decimal", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", nil, "month", "above", text("1e3"), text("EUR"), nil)}
		}, "metric_watcher_threshold_check"},
		{"a value of 7 decimals", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", nil, "month", "above", text("1.0000001"), text("EUR"), nil)}
		}, "metric_watcher_threshold_check"},
		{"a currency in lower case", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", nil, "month", "above", text("1"), text("eur"), nil)}
		}, "metric_watcher_currency_check"},
		{"a negative percentage", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", nil, "month", "change", nil, nil, text("-1"))}
		}, "metric_watcher_percent_check"},
		{"a percentage of 5 decimals", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", nil, "month", "change", nil, nil, text("1.00001"))}
		}, "metric_watcher_percent_check"},
		{"above without a value", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", nil, "month", "above", nil, nil, nil)}
		}, "metric_watcher_condition_whole"},
		{"a change with a value", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", nil, "month", "change", text("1"), nil, text("1"))}
		}, "metric_watcher_condition_whole"},
		{"a change without a percentage", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", nil, "month", "change", nil, nil, nil)}
		}, "metric_watcher_condition_whole"},
		{"above with a percentage", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", nil, "month", "above", text("1"), nil, text("1"))}
		}, "metric_watcher_condition_whole"},
		{"a currency without a value", func(s watcherScene) []stmt {
			return []stmt{s.with("x", "revenue", "[]", nil, "month", "change", nil, text("EUR"), text("1"))}
		}, "metric_watcher_condition_whole"},
		{"removed", func(s watcherScene) []stmt { return []stmt{s.above(), s.end("removed", "", "")} }, ""},
		{"failed with why", func(s watcherScene) []stmt {
			return []stmt{s.above(), s.end("failed", "analytics.watcher_metric_removed", "No metric.")}
		}, ""},
		{"failed without why", func(s watcherScene) []stmt { return []stmt{s.above(), s.end("failed", "", "")} }, "metric_watcher_failure"},
		{"removed with a failure", func(s watcherScene) []stmt {
			return []stmt{s.above(), s.end("removed", "analytics.watcher_metric_removed", "No metric.")}
		}, "metric_watcher_failure"},
		{"a failure without its detail", func(s watcherScene) []stmt {
			return []stmt{s.above(), s.end("failed", "analytics.watcher_metric_removed", "")}
		}, "metric_watcher_failure"},
		{"a failure code of 101 characters", func(s watcherScene) []stmt {
			return []stmt{s.above(), s.end("failed", strings.Repeat("c", 101), "No metric.")}
		}, "metric_watcher_failure_code_check"},
		{"a failure detail of 2001 characters", func(s watcherScene) []stmt {
			return []stmt{s.above(), s.end("failed", "analytics.watcher_metric_removed", strings.Repeat("d", 2001))}
		}, "metric_watcher_failure_detail_check"},
		{"an unknown status", func(s watcherScene) []stmt { return []stmt{s.above(), s.end("paused", "", "")} }, "metric_watcher_status_check"},
		{"a removed watcher made active", func(s watcherScene) []stmt {
			return []stmt{s.above(), s.end("removed", "", ""), s.end("active", "", "")}
		}, "metric_watcher_fixed"},
		{"a failed watcher removed", func(s watcherScene) []stmt {
			return []stmt{s.above(), s.end("failed", "analytics.watcher_metric_removed", "No metric."), s.end("removed", "", "")}
		}, "metric_watcher_fixed"},
		{"ended without the next version", func(s watcherScene) []stmt {
			return []stmt{s.above(), step(`UPDATE analytics.metric_watcher SET status = 'removed' WHERE id = $1`, s.watcher)}
		}, "optimistic lock"},
		{"set at version 0", func(s watcherScene) []stmt {
			return []stmt{step(`INSERT INTO analytics.metric_watcher (tenant_id, id, name, metric, grain, condition, percent, version, created_by, updated_by)
				VALUES ($1, $2, 'x', 'revenue', 'month', 'change', '1', 0, $1, $1)`, s.tenant, s.watcher)}
		}, "metric_watcher_version_check"},
		{"the metric changed", func(s watcherScene) []stmt {
			return []stmt{s.above(), step(`UPDATE analytics.metric_watcher SET metric = 'spend', version = version + 1 WHERE id = $1`, s.watcher)}
		}, "permission denied"},
		{"the creator changed", func(s watcherScene) []stmt {
			return []stmt{s.above(), step(`UPDATE analytics.metric_watcher SET created_by = gen_random_uuid(), version = version + 1 WHERE id = $1`, s.watcher)}
		}, "permission denied"},
		{"a watcher deleted", func(s watcherScene) []stmt {
			return []stmt{s.above(), step(`DELETE FROM analytics.metric_watcher`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := watcherScene{tenant: id(), watcher: id()}
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

// TestMetricWatcherFixed (T96a, migration 000308): a removed watcher stays
// as it is for the owner of the table too, whom no grant holds back.
func TestMetricWatcherFixed(t *testing.T) {
	need(t)
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s := watcherScene{tenant: id(), watcher: id()}
	if err := all(tx, s.above(), s.end("removed", "", "")); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, `UPDATE analytics.metric_watcher SET name = 'y', version = version + 1 WHERE id = $1`, s.watcher), "metric_watcher_fixed")
}

// alertInsert raises an alert: $3 watcher, $4 fiscal year, $5 fiscal
// period, $6 companies, $7 findings.
const alertInsert = `INSERT INTO analytics.metric_alert (tenant_id, id, watcher_id, fiscal_year, fiscal_period, companies, permissions, findings, created_by)
	VALUES ($1, gen_random_uuid(), $2, $3, $4, $5, '{ledger.entry.read}', $6, $1)`

// TestMetricAlertRules (T96a, migration 000308; ADR-0077 §3): an alert is
// of a watcher of the tenant, once per period, in a fiscal year and month,
// naming at least one company and one finding; it is open and is neither
// changed nor deleted here.
func TestMetricAlertRules(t *testing.T) {
	need(t)
	const finding = `[{"company": "2000", "figure": "500.00", "compared_with": "400"}]`
	month := func(s watcherScene, year, period int) stmt {
		return step(alertInsert, s.tenant, s.watcher, year, period, []string{"2000"}, finding)
	}
	cases := []struct {
		name string
		run  func(s watcherScene) []stmt
		want string // empty: allowed
	}{
		{"an alert", func(s watcherScene) []stmt { return []stmt{s.above(), month(s, 2026, 3)} }, ""},
		{"another period", func(s watcherScene) []stmt { return []stmt{s.above(), month(s, 2026, 3), month(s, 2026, 4)} }, ""},
		{"a period twice", func(s watcherScene) []stmt { return []stmt{s.above(), month(s, 2026, 3), month(s, 2026, 3)} }, "metric_alert_once"},
		{"a year twice", func(s watcherScene) []stmt {
			year := step(alertInsert, s.tenant, s.watcher, 2026, nil, []string{"2000"}, finding)
			return []stmt{s.above(), year, year}
		}, "metric_alert_once"},
		{"a month 13", func(s watcherScene) []stmt { return []stmt{s.above(), month(s, 2026, 13)} }, "metric_alert_fiscal_period_check"},
		{"a year 0", func(s watcherScene) []stmt { return []stmt{s.above(), month(s, 0, 3)} }, "metric_alert_fiscal_year_check"},
		{"no company", func(s watcherScene) []stmt {
			return []stmt{s.above(), step(alertInsert, s.tenant, s.watcher, 2026, 3, []string{}, finding)}
		}, "metric_alert_companies_check"},
		{"no finding", func(s watcherScene) []stmt {
			return []stmt{s.above(), step(alertInsert, s.tenant, s.watcher, 2026, 3, []string{"2000"}, "[]")}
		}, "metric_alert_findings_check"},
		{"findings that are no array", func(s watcherScene) []stmt {
			return []stmt{s.above(), step(alertInsert, s.tenant, s.watcher, 2026, 3, []string{"2000"}, "{}")}
		}, "metric_alert_findings_check"},
		{"an alert of no watcher", func(s watcherScene) []stmt { return []stmt{month(s, 2026, 3)} }, "metric_alert_watcher_fkey"},
		{"an unknown status", func(s watcherScene) []stmt {
			return []stmt{s.above(), step(`INSERT INTO analytics.metric_alert (tenant_id, id, watcher_id, fiscal_year, companies, permissions, findings, status, created_by)
				VALUES ($1, gen_random_uuid(), $2, 2026, '{2000}', '{}', $3, 'closed', $1)`, s.tenant, s.watcher, finding)}
		}, "metric_alert_status_check"},
		{"an alert changed", func(s watcherScene) []stmt {
			return []stmt{s.above(), month(s, 2026, 3), step(`UPDATE analytics.metric_alert SET companies = '{1000}', version = version + 1`)}
		}, "permission denied"},
		{"an alert deleted", func(s watcherScene) []stmt {
			return []stmt{s.above(), month(s, 2026, 3), step(`DELETE FROM analytics.metric_alert`)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := watcherScene{tenant: id(), watcher: id()}
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
