package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"integration.connector.read", "integration.connector.manage", "integration.migration.read", "integration.migration.run",
	"platform.operation.read", "ledger.entry.read", "ledger.account.read", "payables.invoice.read", "payables.invoice.capture", "payables.invoice.post", "payables.invoice.hold",
	"procurement.order.read", "procurement.receipt.read", "partner.supplier.read", "tenancy.org.read", "config.change.read",
	"config.change.propose", "config.version.read", "agents.session.read", "agents.session.record"}

// env is a configured tenant behind an API server the CLI talks to, with
// the mock SAP extract of 2026 in the server's extract directory.
type env struct {
	fixture.Env
	dir string
}

func setup(t *testing.T) env {
	t.Helper()
	f := fixture.New(t, perms...)
	f.Tenant.AllCompanies(t, f.Tenant.Agent)
	dir := t.TempDir()
	if err := sap.WriteExtract(filepath.Join(dir, "extract.json"), sap.GenerateMock(42, "2026")); err != nil {
		t.Fatal(err)
	}
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir })
	srv := httptest.NewServer(f.API.Handler)
	t.Cleanup(srv.Close)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.9, 'ECB', $2)`, f.Tenant.ID, f.Tenant.Human.ID)
	vars := map[string]string{"ERP_API_URL": srv.URL, "ERP_TOKEN": f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Human.Subject, nil),
		"ERP_AGENT_TOKEN": f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Agent.Subject, nil)}
	getenv = func(k string) string { return vars[k] }
	t.Cleanup(func() { getenv = os.Getenv })
	t.Chdir(t.TempDir())
	return env{Env: f, dir: dir}
}

// cli runs a command and returns its exit code and output.
func cli(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var buf, errBuf bytes.Buffer
	out, errOut = &buf, &errBuf
	t.Cleanup(func() { out, errOut = os.Stdout, os.Stderr })
	code := run(context.Background(), args)
	return code, buf.String() + errBuf.String()
}

func must(t *testing.T, args ...string) string {
	t.Helper()
	code, got := cli(t, args...)
	if code != 0 {
		t.Fatalf("erp %s: exit %d\n%s", strings.Join(args, " "), code, got)
	}
	return got
}

// body is the JSON an "erp call" printed after its status line.
func body(t *testing.T, output string) map[string]any {
	t.Helper()
	_, js, _ := strings.Cut(output, "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatalf("not an API answer: %s", output)
	}
	return m
}

func model(t *testing.T, steps ...llmtest.Step) {
	t.Helper()
	srv := llmtest.New(t, steps...)
	newRunner = srv.Runner
	t.Cleanup(func() { newRunner = llm.New })
}

var (
	sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)
	invoiceRe = regexp.MustCompile(`Invoice ([0-9a-f-]{36})`)
	qidRe     = regexp.MustCompile(`q_[0-9a-f]{12}`)
)

// TestEndToEnd is T6's exit criterion through the command line: a scope
// migrated from SAP through the integration API, the invoice rule and the
// agents working through API tools only, and analytics over the data the
// API returns.
func TestEndToEnd(t *testing.T) {
	e := setup(t)
	if got := must(t, "gen-mock-sap", "-out", "extract.json", "-year", "2025", "-through", "6"); !strings.Contains(got, "ACDOCA") {
		t.Fatalf("mock: %s", got)
	}
	if got := must(t, "config", "generate", "-source", "file:extract.json", "-company", "1000,2000", "-year", "2025", "-out", "draft.yaml"); !strings.Contains(got, "no findings") {
		t.Fatalf("draft: %s", got)
	}
	must(t, "config", "validate", "-file", "draft.yaml")

	conn := body(t, must(t, "call", "createConnector", "-body", `{"kind": "sap_file", "name": "SAP PRD", "settings": {"path": "extract.json"}}`))
	id := conn["id"].(string)
	got := must(t, "migrate", "-connector", id, "-legal-entities", e.US.String(), "-year", "2026")
	for _, want := range []string{"reconciled", "Vendors"} {
		if !strings.Contains(got, want) {
			t.Fatalf("migrate: got %s, want it to contain %q", got, want)
		}
	}
	// The rule matches parked invoices: clean ones post, others stay held for the agent.
	got = must(t, "invoices")
	for _, want := range []string{"Posted", "(match)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("invoices: got %s, want it to contain %q", got, want)
		}
	}
	steps := []llmtest.Step{}
	for range 6 {
		steps = append(steps, llmtest.ToolFn("createRecommendation", func(b string) any {
			return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(b)[1], "object_type": "supplier_invoice",
				"object_id": invoiceRe.FindStringSubmatch(b)[1], "kind": "other", "content": "Check the price with the buyer."}}
		}), llmtest.Final("ok"))
	}
	model(t, steps...)
	if got := must(t, "agent", "invoices"); !strings.Contains(got, "recommendations") {
		t.Fatalf("agent invoices: %s", got)
	}
	recs := body(t, must(t, "call", "listRecommendations", "-p", "outcome=open"))
	if len(recs["items"].([]any)) == 0 {
		t.Fatal("no recommendations")
	}

	model(t, llmtest.Final("Company 1000 is ready."))
	if got := must(t, "agent", "migration", "-connector", id, "-legal-entities", e.US.String()); !strings.Contains(got, "ready") {
		t.Fatalf("agent migration: %s", got)
	}
	model(t, llmtest.Final("Company 1000 is ready."))
	printFails(t, "agent", "migration", "-connector", id, "-legal-entities", e.US.String())
	model(t, llmtest.Final("Nothing to change."))
	if got := must(t, "agent", "config", "-draft", "draft.yaml", "raise", "the", "tolerance"); !strings.Contains(got, "Nothing to change.") {
		t.Fatalf("agent config: %s", got)
	}
	model(t, llmtest.Final("Nothing to change."))
	printFails(t, "agent", "config", "-draft", "draft.yaml", "raise", "the", "tolerance")
	doc := fixture.Document(t)
	doc.Companies[1].InvoiceTolerance.PricePercent = 3
	b, _ := json.Marshal(map[string]any{"title": "Tolerance", "rationale": "r", "base_version": 1, "proposed": doc})
	writeFile(t, "cr.json", b)
	cr := body(t, must(t, "call", "createChangeRequest", "-body", "@cr.json"))
	model(t, llmtest.Final("## UAT\n1. Post an invoice."))
	if got := must(t, "agent", "test", "-change-request", cr["id"].(string), "-out", "uat.md"); !strings.Contains(got, "UAT") {
		t.Fatalf("agent test: %s", got)
	}
	model(t, llmtest.Final("## UAT\n1. Post an invoice."))
	must(t, "agent", "test", "-change-request", cr["id"].(string))
	model(t, llmtest.Final("## UAT\n1. Post an invoice."))
	printFails(t, "agent", "test", "-change-request", cr["id"].(string))
	model(t, llmtest.Final("## UAT\n1. Post an invoice."))
	fails(t, "agent test: write scripts", "agent", "test", "-change-request", cr["id"].(string), "-out", filepath.Join(t.TempDir(), "missing", "uat.md"))

	// Analytics reads what the person may see through the API.
	if got := must(t, "metrics"); !strings.Contains(got, "revenue") {
		t.Fatalf("metrics: %s", got)
	}
	got = must(t, "query", "-measures", "revenue,invoice_count", "-by", "fiscal_year", "-where", "fiscal_month <= 9", "-companies", "1000", "-csv", "q.csv")
	for _, want := range []string{"2026", "data through"} {
		if !strings.Contains(got, want) {
			t.Fatalf("query: got %s, want it to contain %q", got, want)
		}
	}
	ask := llmtest.New(t,
		llmtest.Tool("run_query", map[string]any{"measures": []string{"revenue"}, "by": []string{"fiscal_year"}}),
		llmtest.ToolFn("build_dashboard", func(b string) any {
			return map[string]any{"spec": analytics.Spec{Title: "Revenue", Tables: []analytics.Table{{Title: "t", QueryID: qidRe.FindString(b)}}}}
		}),
		llmtest.Final("Revenue is up."))
	newAgent = func() *analytics.Agent {
		r := ask.Runner()
		return &analytics.Agent{Client: r.Client, Model: r.Model, Today: func() time.Time { return time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC) }}
	}
	t.Cleanup(func() { newAgent = analytics.NewAgent })
	got = must(t, "ask", "-out", "reports", "revenue", "by", "year")
	reportID := regexp.MustCompile(`erp report -id (\S+)`).FindStringSubmatch(got)[1]
	if got := must(t, "report", "-id", reportID, "-out", "reports"); !strings.Contains(got, "Refreshed") {
		t.Fatalf("report: %s", got)
	}

	// People call any operation, approvals included; failures exit 1.
	if code, got := cli(t, "call", "getMigrationRun", "-p", "id=01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d01"); code != 1 || !strings.Contains(got, "404") {
		t.Fatalf("not found: %d %s", code, got)
	}
	if code, got := cli(t, "call", "approveApprovalRequest", "-p", "id=01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d01", "-if-match", `"1"`,
		"-body", `{}`); code != 1 || !strings.Contains(got, "404") {
		t.Fatalf("approval: %d %s", code, got)
	}
	if got := must(t, "call", "listSupplierInvoices", "-p", "status=posted", "-p", "expand=supplier,purchase_order"); body(t, got)["items"] == nil {
		t.Fatalf("list: %s", got)
	}
}
