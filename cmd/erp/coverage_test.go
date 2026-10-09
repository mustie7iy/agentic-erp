package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// failsAny runs a command that has to fail, whatever the message.
func failsAny(t *testing.T, args ...string) string {
	t.Helper()
	o, code := cli(t, args...)
	if code == 0 {
		t.Fatalf("erp %s: expected a failure, got:\n%s", strings.Join(args, " "), o)
	}
	return o
}

// fixedClock pins the platform clock for the test.
func fixedClock(t *testing.T) {
	t.Helper()
	clock = func() time.Time { return time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { clock = time.Now })
}

// setup migrates the mock SAP system and bootstraps a valid configuration
// in a fresh working directory. It returns the configuration, so tests can
// derive proposals from it.
func setup(t *testing.T) *config.Config {
	t.Helper()
	t.Chdir(t.TempDir())
	fixedClock(t)
	must(t, "gen-mock-sap", "-year", "2026")
	must(t, "migrate", "-company", "1000,2000")
	failsAny(t, "config", "generate", "-company", "1000,2000", "-out", "draft.yaml")
	cfg, err := config.Load("draft.yaml")
	if err != nil {
		t.Fatal(err)
	}
	super := cfg.User("super1")
	super.Roles = slices.DeleteFunc(super.Roles, func(r string) bool { return r == "Z_TREASURY_MGR" })
	cfg.Users = append(cfg.Users, config.User{ID: "analyst1", Name: "Analyst", Roles: []string{"config_analyst"}})
	writeConfig(t, "fixed.yaml", cfg)
	must(t, "config", "bootstrap", "-file", "fixed.yaml", "-as", "architect1")
	return cfg
}

func writeConfig(t *testing.T, path string, c *config.Config) {
	t.Helper()
	doc, _ := c.Marshal()
	if err := os.WriteFile(path, doc, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeModel points the Anthropic SDK (used by analytics.NewAgent and
// agent.NewClaudeResolver) and the platform agents at a scripted server.
func fakeModel(t *testing.T, steps ...llmtest.Step) *llmtest.Server {
	t.Helper()
	srv := llmtest.New(t, steps...)
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_API_KEY", "test")
	newRunner = srv.Runner
	t.Cleanup(func() { newRunner = llm.New })
	return srv
}

// broken is a step the fake model answers with HTTP 400 (no retries), so
// the agent call fails.
var broken = llmtest.Step{Text: "x", Expect: "this text never appears in a request"}

func TestMainUsesExitCode(t *testing.T) {
	t.Chdir(t.TempDir())
	var got int
	exit = func(code int) { got = code }
	t.Cleanup(func() { exit = os.Exit })
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	os.Args = []string{"erp"}
	main()
	if got != 2 {
		t.Fatalf("no command should exit 2, got %d", got)
	}
}

// Every command reports an undefined flag as an error.
func TestEveryCommandRejectsUnknownFlags(t *testing.T) {
	t.Chdir(t.TempDir())
	cmds := [][]string{
		{"gen-mock-sap"}, {"migrate"}, {"invoices"}, {"tasks"}, {"approve"}, {"reject"}, {"tb"}, {"audit-verify"},
		{"metrics"}, {"query"}, {"ask"}, {"report"}, {"capture"},
	}
	for _, sub := range []string{"generate", "bootstrap", "show", "validate", "propose", "evidence", "approve", "reject", "activate", "rollback", "list", "history", "diff"} {
		cmds = append(cmds, []string{"config", sub})
	}
	for group, subs := range map[string][]string{
		"regress": {"capture", "run"}, "req": {"create", "approve", "reject"}, "po": {"create"}, "contract": {"create"},
		"gr": {"post"}, "vendor": {"create", "bank", "block", "unblock", "approve", "reject"},
		"pay": {"propose", "release", "reject"}, "agent": {"config", "migration", "test"},
	} {
		for _, s := range subs {
			cmds = append(cmds, []string{group, s})
		}
	}
	for _, c := range cmds {
		fails(t, "flag provided but not defined", append(c, "-bogus")...)
	}
}

// Every command that needs flags says which are missing.
func TestRequiredFlags(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, c := range [][]string{
		{"approve"}, {"config", "bootstrap"}, {"config", "propose"}, {"config", "evidence"}, {"config", "approve"},
		{"config", "activate"}, {"config", "rollback"}, {"config", "diff"}, {"config", "validate"},
		{"req", "create"}, {"req", "approve"}, {"po", "create"}, {"contract", "create"}, {"gr", "post"},
		{"vendor", "create"}, {"vendor", "bank"}, {"vendor", "approve"}, {"pay", "propose"}, {"pay", "release"},
		{"capture"}, {"agent", "test"},
	} {
		fails(t, "required", c...)
	}
	fails(t, "-id or -file is required", "report")
	fails(t, "ask a question", "ask")
}

// When the audit log, configuration repository or store cannot be opened,
// every command fails before doing anything.
func TestEnvironmentFailures(t *testing.T) {
	t.Chdir(t.TempDir())
	os.WriteFile("bad.jsonl", []byte("{"), 0o644)
	os.WriteFile("invoice.pdf", []byte("%PDF"), 0o644)
	core.NewStore().Save(defaultStore)
	bad := []string{"-audit", "bad.jsonl"}
	for _, c := range [][]string{
		{"config", "bootstrap", "-file", "x", "-as", "a"}, {"config", "show"}, {"config", "propose", "-file", "x", "-title", "t", "-as", "a"},
		{"config", "evidence", "-cr", "CR-1"}, {"config", "approve", "-cr", "CR-1", "-as", "a"}, {"config", "activate", "-cr", "CR-1", "-as", "a"},
		{"config", "rollback", "-to", "1", "-as", "a", "-reason", "r"}, {"config", "list"}, {"config", "history"}, {"config", "diff", "-cr", "CR-1"},
		{"regress", "capture"}, {"regress", "run"}, {"req", "create", "-as", "a"}, {"req", "approve", "-id", "x", "-as", "a"},
		{"po", "create", "-req", "x", "-as", "a"}, {"contract", "create", "-as", "a", "-vendor", "v", "-target", "1"},
		{"gr", "post", "-po", "p", "-qty", "1", "-as", "a"}, {"vendor", "create", "-as", "a", "-id", "i", "-name", "n"},
		{"vendor", "bank", "-as", "a", "-id", "i"}, {"vendor", "block", "-as", "a", "-id", "i"}, {"vendor", "unblock", "-as", "a", "-id", "i"},
		{"vendor", "approve", "-change", "c", "-as", "a"}, {"pay", "propose", "-as", "a"}, {"pay", "release", "-run", "r", "-as", "a"},
		{"capture", "-file", "invoice.pdf"}, {"agent", "migration"}, {"agent", "test", "-cr", "CR-1"},
		{"invoices"}, {"approve", "-task", "t", "-as", "a"}, {"audit-verify"}, {"query", "-measures", "revenue"},
	} {
		fails(t, "audit log", append(c, bad...)...)
	}
	fails(t, "audit log", "agent", "config", "-audit", "bad.jsonl", "change")
	// Corrupt configuration repository and store.
	os.MkdirAll("brokencfg", 0o755)
	os.WriteFile("brokencfg/state.json", []byte("{"), 0o644)
	fails(t, "configuration repository", "config", "list", "-config-dir", "brokencfg")
	os.WriteFile("broken.json", []byte("{"), 0o644)
	failsAny(t, "config", "list", "-store", "broken.json")
	failsAny(t, "migrate", "-store", "broken.json")
	// Commands that need a store fail without one.
	missing := []string{"-store", "missing.json"}
	for _, c := range [][]string{{"tasks"}, {"tb"}, {"invoices"}, {"approve", "-task", "t", "-as", "a"}, {"query", "-measures", "revenue"}} {
		fails(t, "no such file", append(c, missing...)...)
	}
	fails(t, "no such file", "ask", "-store", "missing.json", "question")
}

func TestMigrateAndMockEdgeCases(t *testing.T) {
	t.Chdir(t.TempDir())
	os.WriteFile("afile", nil, 0o644)
	fails(t, "not a directory", "gen-mock-sap", "-out", "afile/x.json")
	fails(t, "invalid -source", "migrate", "-source", "nocolon")
	fails(t, "unknown source kind", "migrate", "-source", "ftp:x")
	// An OData system that answers with an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) }))
	defer srv.Close()
	fails(t, "HTTP 503", "migrate", "-source", "odata:"+srv.URL)

	// A broken document stops the load at the quality gate.
	e := sap.GenerateMock(42, "2026")
	e.ACDOCA[0].HSL = "1.00"
	sap.WriteExtract("bad.json", e)
	o := failsAny(t, "migrate", "-source", "file:bad.json", "-json", "result.json")
	if !strings.Contains(o, "Nothing was loaded") {
		t.Fatal(o)
	}
	if b, err := os.ReadFile("result.json"); err != nil || !json.Valid(b) {
		t.Fatal("JSON result not written")
	}
	// Forcing it loads the rest, and reconciliation reports the failure.
	if o := failsAny(t, "migrate", "-source", "file:bad.json", "-force"); !strings.Contains(o, "reconciliation did not pass") {
		t.Fatal(o)
	}
	// A store that cannot be saved.
	must(t, "gen-mock-sap")
	os.MkdirAll("data/store.json.tmp", 0o755)
	fails(t, "store.json.tmp", "migrate")
}

func TestInvoiceCommandsWithAgentAndLegacyApproval(t *testing.T) {
	t.Chdir(t.TempDir())
	fixedClock(t)
	must(t, "gen-mock-sap")
	must(t, "migrate", "-company", "1000")
	invoice := regexp.MustCompile(`SAPIV-\d+-\d+`)
	steps := []llmtest.Step{}
	for i := 0; i < 3; i++ {
		steps = append(steps,
			llmtest.ToolFn("escalate_to_human", func(body string) any {
				return map[string]any{"invoice_id": invoice.FindString(body), "reason": "variance", "recommendation": "ask for credit note\nwith detail",
					"proposed_action": "request_credit_note"}
			}),
			llmtest.Final("Escalated."))
	}
	steps = append(steps, broken) // the fourth exception fails at the model
	fakeModel(t, steps...)
	o := must(t, "invoices", "-agent")
	if !strings.Contains(o, "error:") || !strings.Contains(o, "with agent recommendation") {
		t.Fatalf("agent run:\n%s", o)
	}
	o = must(t, "tasks")
	if !strings.Contains(o, "recommendation (agent:ap-exceptions)") {
		t.Fatalf("tasks should show agent recommendations:\n%s", o)
	}
	task := idIn(t, `TASK-\d{8}`, o)
	// Without a configuration any named person may decide (legacy mode).
	if o := must(t, "approve", "-task", task, "-as", "controller"); !strings.HasPrefix(o, "allow") {
		t.Fatal(o)
	}
	failsAny(t, "approve", "-task", "TASK-NONE", "-as", "controller")
	if o := must(t, "tb", "-company", "1000", "-as-of", "2026-12-31"); !strings.Contains(o, "TOTAL (must be 0)") {
		t.Fatal(o)
	}
}

func TestDecideWithUnknownConfiguredUser(t *testing.T) {
	setup(t)
	must(t, "invoices")
	task := idIn(t, `TASK-\d{8}`, must(t, "tasks"))
	fails(t, "not a configured user", "approve", "-task", task, "-as", "nobody")
}

// pkgDir is the package directory, captured before any test changes directory.
var pkgDir, _ = os.Getwd()

func dashboardSteps() []llmtest.Step {
	qid := regexp.MustCompile(`q_[0-9a-f]{12}`)
	return []llmtest.Step{
		llmtest.Tool("list_metrics", map[string]any{}),
		llmtest.Tool("run_query", map[string]any{"measures": []string{"revenue"}, "by": []string{"fiscal_year"}}),
		llmtest.ToolFn("build_dashboard", func(body string) any {
			return map[string]any{"spec": map[string]any{"title": "Revenue by year",
				"tiles": []map[string]any{{"label": "Revenue", "query_id": qid.FindString(body), "measure": "revenue"}}}}
		}),
		llmtest.Final("Revenue is shown by year."),
	}
}

func TestAnalyticsCommands(t *testing.T) {
	t.Chdir(t.TempDir())
	fixedClock(t)
	must(t, "gen-mock-sap")
	must(t, "migrate", "-company", "1000,2000")
	os.WriteFile("afile", nil, 0o644)

	// metrics
	if o := must(t, "metrics"); !strings.Contains(o, "revenue") || !strings.Contains(o, "Everyday words") {
		t.Fatal(o)
	}
	semantic := filepath.Join(pkgDir, "..", "..", "internal", "analytics", "semantic.yaml")
	must(t, "metrics", "-semantic", semantic)
	fails(t, "no such file", "metrics", "-semantic", "missing.yaml")

	// query: filters, currency headers, an empty ratio cell, CSV export
	o := must(t, "query", "-measures", "revenue,operating_margin", "-by", "account", "-where", "fiscal_month <= 9",
		"-companies", "1000", "-semantic", semantic, "-csv", "r.csv")
	if !strings.Contains(o, "revenue (USD)") || !strings.Contains(o, "—") {
		t.Fatalf("query output:\n%s", o)
	}
	if b, _ := os.ReadFile("r.csv"); !strings.HasPrefix(string(b), "account,revenue,operating_margin") {
		t.Fatalf("csv: %s", b)
	}
	fails(t, "want \"dim op values\"", "query", "-measures", "revenue", "-where", "fiscal_month")
	fails(t, "unknown metric", "query", "-measures", "ebitda")
	fails(t, "no such file", "query", "-measures", "revenue", "-semantic", "missing.yaml")
	fails(t, "not a directory", "query", "-measures", "revenue", "-by", "company", "-csv", "afile/x.csv")
	broken := core.NewStore()
	broken.Companies["1"] = &core.Company{Code: "1", Currency: "USD"}
	broken.Journal["J1"] = &core.JournalEntry{ID: "J1", CompanyCode: "1", PostingDate: "2026-01-01", Lines: []core.JournalLine{{Account: "X", Amount: 1}}}
	broken.Save("badfacts.json")
	fails(t, "unknown account", "query", "-measures", "revenue", "-store", "badfacts.json")

	// ask: the agent builds a dashboard; then the saved report is rerun
	fakeModel(t, append(dashboardSteps(), llmtest.Step{Text: "x", Expect: "never in a request"})...)
	o = must(t, "ask", "-companies", "1000", "-as", "sales", "revenue", "by", "year")
	id := idIn(t, `rpt_[0-9a-f]+`, o)
	if o := must(t, "report", "-id", id, "-companies", "1000"); !strings.Contains(o, "Refreshed \"Revenue by year\"") {
		t.Fatal(o)
	}
	failsAny(t, "ask", "-companies", "1000", "again") // the model fails
	fakeModel(t, dashboardSteps()...)
	fails(t, "not a directory", "ask", "-companies", "1000", "-out", "afile/sub", "revenue")

	// report errors
	fails(t, "no such file", "report", "-id", "rpt_missing")
	saved := filepath.Join("data", "reports", id+".json")
	fails(t, "no such file", "report", "-file", saved, "-store", "missing.json")
	os.WriteFile("bad-report.json", []byte(`{"spec":{"title":"x","tiles":[{"label":"l","query_id":"q","measure":"nope"}]},"queries":{"q":{"measures":["nope"]}}}`), 0o644)
	fails(t, "unknown metric", "report", "-file", "bad-report.json")
	fails(t, "not a directory", "report", "-file", saved, "-companies", "1000", "-out", "afile/sub")
}
