package reports_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/reports"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// stored is a tenant with revenue whose API keeps objects in a directory,
// and the report scheduler wired as erp-server wires it.
type stored struct {
	env
	root string
	s    *reports.Scheduler
}

func withObjects(t *testing.T) stored {
	t.Helper()
	e := stored{env: setup(t), root: t.TempDir()}
	var cfg app.Config
	e.API = apitest.NewWith(t, func(c *app.Config) {
		c.Objects, c.Loopback = blobstore.Dir{Root: e.root}, &httpapi.Loopback{}
		cfg = *c
	})
	e.s = app.ReportScheduler(cfg, "erp_worker")
	return e
}

// runOnce runs the scheduler's round, which may run other tests'
// tenants' reports too.
func (e stored) runOnce(t *testing.T) {
	t.Helper()
	if _, err := e.s.RunOnce(context.Background()); err != nil {
		t.Fatalf("round: %v", err)
	}
}

// scheduled saves a report and gives it a daily schedule dated by
// company 2000; it returns the report's id.
func (e stored) scheduled(t *testing.T) string {
	t.Helper()
	rep, tag := e.save(t, reportOf(""))
	id := apitest.String(t, rep, "id")
	e.Must(200, "POST", "/v1/reports/"+id+":schedule", `{"frequency": "daily", "company": "2000"}`, tag)
	return id
}

// runs are the runs of a report as listed.
func (e stored) runs(t *testing.T, report string) []map[string]any {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/report-runs?report_id="+report, "", "")
	var out []map[string]any
	for _, it := range apitest.List(t, page, "items") {
		out = append(out, it.(map[string]any))
	}
	return out
}

// TestScheduledRun: a daily schedule's run is kept once with its results,
// read back as JSON and CSV, audited and published (T95b).
func TestScheduledRun(t *testing.T) {
	e := withObjects(t)
	rep, tag := e.save(t, reportOf(""))
	id := apitest.String(t, rep, "id")
	sched, tag2 := e.Must(200, "POST", "/v1/reports/"+id+":schedule", `{"frequency": "daily", "company": "2000"}`, tag)
	var today string
	pgtest.Scan(t, `SELECT (now() AT TIME ZONE 'UTC')::date::text`, nil, &today)
	if got := fmt.Sprint(sched["schedule"]); got != "map[company:2000 frequency:daily from:"+today+"]" || tag2 != `"2"` {
		t.Errorf("schedule: got %s at %s, want daily by 2000 from %s at \"2\"", got, tag2, today)
	}
	// Other tests' tenants' reports may run in a round too: what is
	// counted is this report's runs.
	e.runOnce(t)
	e.runOnce(t)
	runs := e.runs(t, id)
	if len(runs) != 1 {
		t.Fatalf("runs after two rounds: got %v, want 1: the day's run is kept once", runs)
	}
	run := runs[0]
	for f, want := range map[string]any{"report_title": "Revenue by company", "status": "succeeded", "run_date": today,
		"run_as": e.Tenant.Human.ID.String(), "data_through": "2026-03-20"} {
		if run[f] != want {
			t.Errorf("%s: got %v, want %v", f, run[f], want)
		}
	}
	if run["results"] != nil || len(fmt.Sprint(run["sha256"])) != 64 || run["run_as_name"] == "" {
		t.Errorf("listed run: got %v, want no results, a SHA-256 and the owner's name", run)
	}
	runID := apitest.String(t, run, "id")
	got, _ := e.Must(200, "GET", "/v1/report-runs/"+runID, "", "")
	if c := cells(t, got); c != "2000 500.00 EUR" {
		t.Errorf("kept results: got %s, want 2000's 500.00 EUR", c)
	}
	if got["spec"].(map[string]any)["title"] != "Revenue by company" {
		t.Errorf("kept spec: got %v", got["spec"])
	}
	csv := e.API.Do(apitest.Req{Method: "GET", Path: "/v1/report-runs/" + runID + "?format=csv", Tenant: e.Tenant.ID, As: e.Tenant.Human})
	if csv.Code != 200 || csv.Body.String() != "company,revenue,revenue_currency\n2000,500.00,EUR\n" {
		t.Errorf("CSV: got %d %s, want 2000's revenue", csv.Code, csv.Body)
	}
	events := apitest.Published(t, e.Tenant, outbox.Event{Context: "analytics", Name: "report_run_completed"})
	if len(events) != 1 || events[0]["status"] != "succeeded" || events[0]["report_run_id"] != runID || events[0]["sha256"] != run["sha256"] {
		t.Errorf("events: got %v, want the run's", events)
	}
	if a := e.auditedRun(t, runID); len(a) != 1 || !strings.HasPrefix(a[0], "analytics.report_run.complete") {
		t.Errorf("audit: got %v, want the run's", a)
	}
	e.Problem(422, "analytics.unknown_query", "GET", "/v1/report-runs/"+runID+"?format=csv&query_id=nope", "", "")
	e.Problem(422, "request.validation_failed", "GET", "/v1/report-runs/"+runID+"?format=pdf", "", "")
	// The same result as an Excel workbook (T95d): its sheet holds the
	// amount as a number with its currency's decimals, and its currency.
	xlsx := e.API.Do(apitest.Req{Method: "GET", Path: "/v1/report-runs/" + runID + "?format=xlsx", Tenant: e.Tenant.ID, As: e.Tenant.Human})
	if got := xlsx.Header().Get("Content-Type"); xlsx.Code != 200 || got != analytics.XLSXMediaType {
		t.Fatalf("workbook: got %d %s, want 200 %s", xlsx.Code, got, analytics.XLSXMediaType)
	}
	if sheet := sheetOf(t, xlsx.Body.Bytes()); !strings.Contains(sheet, `<sheet name="Report"`) || !strings.Contains(sheet, `<c r="B2" s="1"><v>500.00</v></c>`) ||
		!strings.Contains(sheet, `<t xml:space="preserve">EUR</t>`) {
		t.Errorf("workbook: got %s, want 2000's 500.00 EUR on the sheet Report", sheet)
	}
	e.Problem(422, "analytics.unknown_query", "GET", "/v1/report-runs/"+runID+"?format=xlsx&query_id=nope", "", "")
	path := filepath.Join(e.root, filepath.FromSlash(blobstore.Key(e.Tenant.ID, blobstore.KindReportRuns, runID+".json")))
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// Its bytes changed in storage: refused, never served.
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e.Problem(409, "analytics.report_run_changed", "GET", "/v1/report-runs/"+runID, "", "")
}

// TestRunPages: runs are listed the latest day first, a page at a time.
func TestRunPages(t *testing.T) {
	e := withObjects(t)
	id := e.scheduled(t)
	e.runOnce(t)
	pgtest.Exec(t, `INSERT INTO analytics.report_run (tenant_id, id, report_id, run_date, status, run_as, failure_code, failure_detail)
		VALUES ($1, gen_random_uuid(), $2, current_date - 1, 'failed', $3, 'x.refused', 'Refused.')`, e.Tenant.ID, id, e.Tenant.Human.ID)
	var dates []string
	path := "/v1/report-runs?page_size=1"
	for range 3 {
		page, _ := e.Must(200, "GET", path, "", "")
		for _, it := range apitest.List(t, page, "items") {
			dates = append(dates, fmt.Sprint(it.(map[string]any)["status"]))
		}
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		path = "/v1/report-runs?page_size=1&page_token=" + url.QueryEscape(next)
	}
	if got := strings.Join(dates, ", "); got != "succeeded, failed" {
		t.Errorf("runs: got %s, want today's then yesterday's", got)
	}
}

// auditedRun are the audit records of a run.
func (e stored) auditedRun(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	pgtest.Scan(t, `SELECT coalesce(array_agg(action ORDER BY seq), '{}') FROM audit.audit_event
		WHERE tenant_id = $1 AND object_type = 'analytics.report_run' AND object_id = $2`, []any{e.Tenant.ID, id}, &out)
	return out
}

// TestScheduledRunOnce: however many workers claim a day's run, it is
// kept once.
func TestScheduledRunOnce(t *testing.T) {
	e := withObjects(t)
	id := e.scheduled(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.s.RunOnce(context.Background())
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if runs := e.runs(t, id); len(runs) != 1 {
		t.Errorf("runs after four workers: got %d, want 1", len(runs))
	}
}

// TestScheduledRunFails: an owner deactivated since, or a query refused,
// fails the run, which is kept with the problem; the schedule stays.
func TestScheduledRunFails(t *testing.T) {
	for name, c := range map[string]struct {
		breakIt func(e stored, report string)
		code    string
	}{
		"owner deactivated": {func(e stored, _ string) {
			pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, e.Tenant.Human.ID)
		}, "identity.principal_blocked"},
		"its permission revoked": {func(e stored, _ string) {
			pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'analytics.query.run'`, e.Tenant.ID)
		}, "identity.permission_missing"},
		"barred by a segregation-of-duties rule": {func(e stored, _ string) {
			pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
				VALUES ($1, gen_random_uuid(), 'analytics.report.read', 'analytics.report.write', 'Read or write', current_date - 1, $2)`,
				e.Tenant.ID, e.Tenant.Human.ID)
		}, "identity.sod_conflict"},
		"a metric removed": {func(_ stored, report string) {
			pgtest.Exec(t, `UPDATE analytics.report SET queries = '{"rev": {"measures": ["gone"]}}', version = version + 1 WHERE id = $1`, report)
		}, "analytics.report_metric_removed"},
	} {
		t.Run(name, func(t *testing.T) {
			e := withObjects(t)
			id := e.scheduled(t)
			c.breakIt(e, id)
			e.runOnce(t)
			var status, code string
			pgtest.Scan(t, `SELECT status, failure_code FROM analytics.report_run WHERE report_id = $1`, []any{id}, &status, &code)
			if status != "failed" || code != c.code {
				t.Errorf("run: got %s %s, want failed %s", status, code, c.code)
			}
			var scheduled bool
			pgtest.Scan(t, `SELECT schedule_frequency IS NOT NULL FROM analytics.report WHERE id = $1`, []any{id}, &scheduled)
			if !scheduled {
				t.Error("schedule after a failed run: got none, want it in place")
			}
		})
	}
	t.Run("an agent the kill switch stops", func(t *testing.T) {
		e := withObjects(t)
		id := e.scheduled(t)
		pgtest.Exec(t, `UPDATE analytics.report SET owner_id = $2, version = version + 1 WHERE id = $1`, id, e.Tenant.Agent.ID)
		s := *e.s
		s.Agents = func(context.Context, pgx.Tx) error { return problem.Forbidden("agents.ai_disabled", "AI is off.") }
		if _, err := s.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		var code string
		pgtest.Scan(t, `SELECT failure_code FROM analytics.report_run WHERE report_id = $1`, []any{id}, &code)
		if code != "agents.ai_disabled" {
			t.Errorf("run: got %q, want agents.ai_disabled", code)
		}
	})
	t.Run("read back", func(t *testing.T) {
		e := withObjects(t)
		id := e.scheduled(t)
		pgtest.Exec(t, `UPDATE analytics.report SET queries = '{"rev": {"measures": ["gone"]}}', version = version + 1 WHERE id = $1`, id)
		e.runOnce(t)
		run := e.runs(t, id)[0]
		runID := apitest.String(t, run, "id")
		got, _ := e.Must(200, "GET", "/v1/report-runs/"+runID, "", "")
		failure, _ := got["failure"].(map[string]any)
		if failure["code"] != "analytics.report_metric_removed" || got["results"] != nil || got["sha256"] != nil {
			t.Errorf("failed run: got %v, want its problem and no results", got)
		}
		e.Problem(409, "analytics.report_run_failed", "GET", "/v1/report-runs/"+runID+"?format=csv", "", "")
		page, _ := e.Must(200, "GET", "/v1/report-runs?status=succeeded", "", "")
		if items := apitest.List(t, page, "items"); len(items) != 0 {
			t.Errorf("succeeded runs: got %v, want none", items)
		}
	})
}

// TestScheduleRefusals: a schedule names its frequency's day and a
// company the configuration has; a removed report has none; ending a
// schedule needs one; without object storage it is 503.
func TestScheduleRefusals(t *testing.T) {
	e := withObjects(t)
	rep, tag := e.save(t, reportOf(""))
	id := apitest.String(t, rep, "id")
	for name, c := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"an unknown frequency":       {`{"frequency": "hourly", "company": "2000"}`, 422, "request.validation_failed"},
		"weekly without a weekday":   {`{"frequency": "weekly", "company": "2000"}`, 422, "request.validation_failed"},
		"daily with a weekday":       {`{"frequency": "daily", "company": "2000", "weekday": 1}`, 422, "request.validation_failed"},
		"monthly without its day":    {`{"frequency": "monthly", "company": "2000"}`, 422, "request.validation_failed"},
		"a company the tenant lacks": {`{"frequency": "daily", "company": "9999"}`, 422, "analytics.unknown_company"},
	} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(c.status, c.code, "POST", "/v1/reports/"+id+":schedule", c.body, tag)
		})
	}
	e.Problem(409, "analytics.report_not_scheduled", "POST", "/v1/reports/"+id+":unschedule", "", tag)
	sched, tag := e.Must(200, "POST", "/v1/reports/"+id+":schedule", `{"frequency": "monthly", "company": "1000", "day_of_month": 28}`, tag)
	if got := fmt.Sprint(sched["schedule"].(map[string]any)["day_of_month"]); got != "28" {
		t.Errorf("monthly: got %v, want day 28", sched["schedule"])
	}
	unsched, tag := e.Must(200, "POST", "/v1/reports/"+id+":unschedule", "", tag)
	if unsched["schedule"] != nil || tag != `"3"` {
		t.Errorf("unscheduled: got %v at %s, want no schedule at \"3\"", unsched["schedule"], tag)
	}
	_, tag = e.Must(200, "POST", "/v1/reports/"+id+":schedule", `{"frequency": "weekly", "company": "1000", "weekday": 7}`, tag)
	removed, tag := e.Must(200, "POST", "/v1/reports/"+id+":remove", "", tag)
	if removed["schedule"] != nil {
		t.Errorf("removed: got %v, want its schedule ended", removed["schedule"])
	}
	e.Problem(409, "analytics.report_removed", "POST", "/v1/reports/"+id+":schedule", `{"frequency": "daily", "company": "2000"}`, tag)
	plain := setup(t)
	rep, tag = plain.save(t, reportOf(""))
	if code, body, _ := plain.Do("POST", "/v1/reports/"+apitest.String(t, rep, "id")+":schedule", `{"frequency": "daily", "company": "2000"}`, tag); code != 503 {
		t.Errorf("without object storage: got %d %v, want 503", code, body)
	}
	if code, body, _ := plain.Do("GET", "/v1/report-runs/"+apitest.String(t, rep, "id"), "", ""); code != 404 {
		t.Errorf("an unknown run: got %d %v, want 404", code, body)
	}
}

// TestScheduleDays: a schedule is due on the days its frequency names,
// from the day it was set.
func TestScheduleDays(t *testing.T) {
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	one, seven, fifteen := int32(1), int32(7), int32(15)
	from := day("2026-11-10") // a Tuesday
	for name, c := range map[string]struct {
		s    reports.Schedule
		day  string
		want bool
	}{
		"daily":                  {reports.Schedule{Frequency: "daily", From: from}, "2026-11-11", true},
		"daily before its start": {reports.Schedule{Frequency: "daily", From: from}, "2026-11-09", false},
		"weekly on Monday":       {reports.Schedule{Frequency: "weekly", Weekday: &one, From: from}, "2026-11-16", true},
		"weekly not on Monday":   {reports.Schedule{Frequency: "weekly", Weekday: &one, From: from}, "2026-11-17", false},
		"weekly on Sunday":       {reports.Schedule{Frequency: "weekly", Weekday: &seven, From: from}, "2026-11-15", true},
		"monthly on the 15th":    {reports.Schedule{Frequency: "monthly", Day: &fifteen, From: from}, "2026-11-15", true},
		"monthly not on it":      {reports.Schedule{Frequency: "monthly", Day: &fifteen, From: from}, "2026-11-16", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := c.s.On(day(c.day)); got != c.want {
				t.Errorf("on %s: got %v, want %v", c.day, got, c.want)
			}
		})
	}
}

// TestSchedulerDatabaseFailures: every statement of a round failing in
// turn fails the round, and a later round keeps the run.
func TestSchedulerDatabaseFailures(t *testing.T) {
	e := withObjects(t)
	id := e.scheduled(t)
	pool := pgtest.Pool(t)
	s := *e.s
	for n := 1; ; n++ {
		count := 0
		s.Begin = func(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, sc)
			if sc.TenantID != e.Tenant.ID && sc.TenantID != uuid.Nil { // other tests' tenants' reports run as they are
				return tx, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, err
		}
		_, err := s.RunOnce(context.Background())
		if err == nil {
			if n < 5 {
				t.Fatalf("statement %d: round succeeded, want every statement covered", n)
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: got %v, want the injected failure", n, err)
		}
	}
	if runs := e.runs(t, id); len(runs) != 1 {
		t.Errorf("runs: got %d, want 1", len(runs))
	}
}

// TestSchedulerRun: the loop runs rounds until its context ends.
func TestSchedulerRun(t *testing.T) {
	e := withObjects(t)
	id := e.scheduled(t)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	e.s.Interval = 10 * time.Millisecond
	go func() { e.s.Run(ctx); close(stopped) }()
	pgtest.Eventually(t, time.Minute, func() bool { return len(e.runs(t, id)) == 1 }, "report %s not run", id)
	cancel()
	<-stopped
}

// sheetOf is a workbook's workbook and sheet parts, one after the other.
func sheetOf(t *testing.T, b []byte) string {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("workbook: %v", err)
	}
	var out strings.Builder
	for _, name := range []string{"xl/workbook.xml", "xl/worksheets/sheet1.xml"} {
		f, err := z.Open(name)
		if err != nil {
			t.Fatalf("workbook part %s: %v", name, err)
		}
		part, err := io.ReadAll(f)
		if err != nil {
			t.Fatalf("workbook part %s: %v", name, err)
		}
		out.Write(part)
	}
	return out.String()
}
