package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
)

// savedID is a report's id in erp-server.
const savedID = "0192a000-0000-7000-8000-000000000301"

// TestSavedReports: erp report -save saves a report file in erp-server,
// and -id with erp-server's id runs it there and writes its dashboard
// (T95a).
func TestSavedReports(t *testing.T) {
	t.Chdir(t.TempDir())
	b, _ := json.Marshal(analytics.Report{ID: "r1", Spec: analytics.Spec{Title: "Invoices", Tables: []analytics.Table{{Title: "t", QueryID: "q"}}},
		Queries: map[string]analytics.Query{"q": {Measures: []string{"invoice_count"}}}}) // plain data
	writeFile(t, "r1.json", b)
	run := `{"report_id": "` + savedID + `", "title": "Invoices", "spec": {"title": "Invoices", "tables": [{"title": "t", "query_id": "q_0123456789ab"}]},
		"results": [` + answer + `], "data_through": "2026-03-05"}`
	fakeAPI(t, routes{"POST /v1/reports": {{201, `{"id": "` + savedID + `", "title": "Invoices"}`}},
		"GET /v1/reports/" + savedID: {ok(`{"id": "` + savedID + `"}`)}, "POST /v1/reports/" + savedID + ":run": {ok(run)}})
	if got := must(t, "report", "-save", "-file", "r1.json"); !strings.Contains(got, `Saved "Invoices" in erp-server as report `+savedID) {
		t.Errorf("save: got %s, want the report's id", got)
	}
	got := must(t, "report", "-id", savedID, "-out", "reports")
	if !strings.Contains(got, `Ran "Invoices" in erp-server with data through 2026-03-05`) {
		t.Errorf("run: got %s, want the title and the data's date", got)
	}
	if _, err := os.Stat(filepath.Join("reports", savedID+".html")); err != nil {
		t.Errorf("dashboard: %v", err)
	}
	fails(t, "not a directory", "report", "-id", savedID, "-out", "r1.json/reports")
	printFails(t, "report", "-save", "-file", "r1.json")
	printFails(t, "report", "-id", savedID, "-out", "reports")
}

// TestSavedReportsRefused: erp-server's refusals and answers that do not
// read fail the command.
func TestSavedReportsRefused(t *testing.T) {
	t.Chdir(t.TempDir())
	b, _ := json.Marshal(analytics.Report{ID: "r1", Spec: analytics.Spec{Title: "Invoices"}}) // plain data
	writeFile(t, "r1.json", b)
	get, run := "GET /v1/reports/"+savedID, "POST /v1/reports/"+savedID+":run"
	gone := reply{404, `{"code": "request.not_found", "detail": "No report with this id."}`}
	for name, c := range map[string]struct {
		routes routes
		args   []string
		want   string
	}{
		"save refused":           {routes{"POST /v1/reports": {{422, `{"code": "analytics.invalid_report", "detail": "bad"}`}}}, []string{"-save", "-file", "r1.json"}, "analytics.invalid_report"},
		"save unreadable":        {routes{"POST /v1/reports": {{201, `[`}}}, []string{"-save", "-file", "r1.json"}, "save report"},
		"not found":              {routes{get: {gone}}, []string{"-id", savedID}, "request.not_found"},
		"run refused":            {routes{get: {ok(`{}`)}, run: {{409, `{"code": "analytics.report_removed"}`}}}, []string{"-id", savedID}, "analytics.report_removed"},
		"run unreadable":         {routes{get: {ok(`{}`)}, run: {ok(`[`)}}, []string{"-id", savedID}, "decode report"},
		"a result that is wrong": {routes{get: {ok(`{}`)}, run: {ok(`{"results": [{"columns": [], "rows": [[{"value": "1"}]]}]}`)}}, []string{"-id", savedID}, "a row of 1 cells"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeAPI(t, c.routes)
			fails(t, c.want, append([]string{"report"}, c.args...)...)
		})
	}
	fails(t, "erp-server's already", "report", "-save", "-id", savedID)
	fakeAPI(t, routes{}, "ERP_TOKEN", "")
	fails(t, "ERP_TOKEN", "report", "-id", savedID)
	fails(t, "ERP_TOKEN", "report", "-save", "-file", "r1.json")
	fakeAPI(t, routes{}, "ERP_API_URL", "http://127.0.0.1:1")
	fails(t, "save report", "report", "-save", "-file", "r1.json")
	fails(t, "run report", "report", "-id", savedID)
}
