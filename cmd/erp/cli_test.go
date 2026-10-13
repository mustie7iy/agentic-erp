package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// reply is a canned API answer; status 0 aborts the connection instead.
type reply struct {
	status int
	body   string
}

var aborted = reply{}

func ok(body string) reply { return reply{200, body} }

// routes answer "METHOD path" with the replies in order, the last one
// repeated; any other call is a 404 problem.
type routes map[string][]reply

// fakeAPI serves routes and points the CLI at them with both tokens set;
// vars overrides the environment (an empty value unsets a variable).
func fakeAPI(t *testing.T, r routes, vars ...string) {
	t.Helper()
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		key := req.Method + " " + req.URL.Path
		n := calls[key]
		calls[key]++
		mu.Unlock()
		replies, found := r[key]
		if !found {
			w.WriteHeader(404)
			w.Write([]byte(`{"code": "not_found", "detail": "` + key + `"}`))
			return
		}
		rep := replies[min(n, len(replies)-1)]
		if rep.status == 0 {
			panic(http.ErrAbortHandler)
		}
		w.Header().Set("ETag", `"1"`)
		w.WriteHeader(rep.status)
		w.Write([]byte(rep.body))
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{"ERP_API_URL": srv.URL, "ERP_TOKEN": "t", "ERP_AGENT_TOKEN": "a"}
	for i := 0; i+1 < len(vars); i += 2 {
		env[vars[i]] = vars[i+1]
	}
	getenv = func(k string) string { return env[k] }
	t.Cleanup(func() { getenv = os.Getenv })
}

// writeFile writes a fixture file the command under test reads; a failed write
// fails the test here rather than as a confusing command error later.
func writeFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// decodeRequest decodes a fake server's JSON request body into v. It runs on the
// server's goroutine, so a body the CLI got wrong is reported with t.Errorf and
// answered 400, and the caller returns.
func decodeRequest(t *testing.T, w http.ResponseWriter, r *http.Request, v any) bool {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Errorf("%s %s: decode the request body: %v", r.Method, r.URL.Path, err)
		w.WriteHeader(http.StatusBadRequest)
		return false
	}
	return true
}

// fails runs a command that must fail with output containing want.
func fails(t *testing.T, want string, args ...string) {
	t.Helper()
	code, got := cli(t, args...)
	if code == 0 || !strings.Contains(got, want) {
		t.Errorf("erp %s: exit %d, want failure with %q:\n%s", strings.Join(args, " "), code, want, got)
	}
}

func TestUsageAndMain(t *testing.T) {
	if code, got := cli(t); code != 2 || !strings.Contains(got, "usage") {
		t.Fatalf("no command: %d %s", code, got)
	}
	if code, _ := cli(t, "bogus"); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
	if code, _ := cli(t, "metrics", "-h"); code != 0 {
		t.Fatalf("help: %d", code)
	}
	var code int
	exit = func(c int) { code = c }
	args := os.Args
	os.Args = []string{"erp"}
	errOut = &strings.Builder{}
	t.Cleanup(func() { exit, os.Args, errOut = os.Exit, args, os.Stderr })
	main()
	if code != 2 {
		t.Fatalf("main: %d", code)
	}
}

func TestCallErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	fakeAPI(t, routes{}, "ERP_API_URL", "")
	fails(t, "no operation noSuchOperation", "call", "noSuchOperation") // on the default URL, refused before any call
	fakeAPI(t, routes{})
	fails(t, "name the operation", "call")
	fails(t, "name the operation", "call", "-p", "x=1")
	fails(t, "flag provided but not defined", "call", "listMigrationRuns", "-x")
	fails(t, "want name=value", "call", "listMigrationRuns", "-p", "status")
	fails(t, "no such file", "call", "createConnector", "-body", "@missing.json")
	fails(t, "body:", "call", "createConnector", "-body", "{")
	fails(t, "missing path parameter", "call", "getMigrationRun")
	fakeAPI(t, routes{}, "ERP_TOKEN", "")
	fails(t, "ERP_TOKEN is not set", "call", "listMigrationRuns")
	// An answer over the tool's size limit comes back truncated, no longer
	// JSON: the command fails rather than print status 0 and null.
	fakeAPI(t, routes{"GET /v1/migration-runs": {ok(`{"items": ["` + strings.Repeat("x", 50000) + `"]}`)}})
	fails(t, "cannot be read", "call", "listMigrationRuns")
}

func TestLocalPreparationErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFile(t, "file", nil)
	fails(t, "flag provided", "gen-mock-sap", "-x")
	fails(t, "not a directory", "gen-mock-sap", "-out", "file/extract.json")
	must(t, "gen-mock-sap", "-out", "extract.json", "-seed", "7")

	fails(t, "config generate | validate", "config")
	fails(t, "unknown config command", "config", "bogus")
	fails(t, "flag provided", "config", "generate", "-x")
	fails(t, "invalid -source", "config", "generate", "-source", "extract.json")
	fails(t, "127.0.0.1", "config", "generate", "-source", "odata:http://127.0.0.1:1")
	fails(t, "not a directory", "config", "generate", "-source", "file:extract.json", "-out", "file/draft.yaml")
	fails(t, "flag provided", "config", "validate", "-x")
	fails(t, "missing.yaml", "config", "validate", "-file", "missing.yaml")

	doc := fixture.Document(t)
	doc.Companies[0].TaxCodes = nil
	b, _ := doc.Marshal()
	writeFile(t, "warn.yaml", b)
	if got := must(t, "config", "validate", "-file", "warn.yaml"); !strings.Contains(got, "[warning]") {
		t.Fatalf("warnings: %s", got)
	}
	doc.Companies[0].InvoiceTolerance.PricePercent = 200
	b, _ = doc.Marshal()
	writeFile(t, "bad.yaml", b)
	fails(t, "validation errors", "config", "validate", "-file", "bad.yaml")
}

const runPath = "/v1/migration-runs/r1"

func migration(r routes) routes {
	base := routes{
		"POST /v1/migration-runs":        {{201, `{"id": "r1", "status": "queued"}`}},
		"GET " + runPath:                 {ok(`{"id": "r1", "status": "queued"}`)},
		"POST " + runPath + ":start":     {ok(`{}`)},
		"POST " + runPath + ":reconcile": {ok(`{}`)},
	}
	for k, v := range r {
		base[k] = v
	}
	return base
}

func TestMigrateErrors(t *testing.T) {
	loaded := ok(`{"id": "r1", "status": "loaded"}`)
	failed := ok(`{"id": "r1", "status": "failed", "report": {"checks": [{"company_code": "1000", "name": "balance", "sap": "1", "platform": "2", "pass": false}],
		"diffs": [{"company_code": "1000", "key": "211000", "sap": "1", "platform": "2"}], "missing_documents": ["1000/1900000001/2026"]}}`)
	for name, c := range map[string]struct {
		r    routes
		want string
	}{
		"create aborted":     {routes{"POST /v1/migration-runs": {aborted}}, "EOF"},
		"create refused":     {routes{"POST /v1/migration-runs": {{422, `{"code": "integration.invalid_scope"}`}}}, "invalid_scope"},
		"read aborted":       {routes{"GET " + runPath: {aborted}}, "EOF"},
		"read refused":       {routes{"GET " + runPath: {{404, `{"code": "not_found"}`}}}, "not_found"},
		"start aborted":      {routes{"POST " + runPath + ":start": {aborted}}, "EOF"},
		"reread aborted":     {routes{"GET " + runPath: {ok(`{}`), aborted}}, "EOF"},
		"reconcile refused":  {routes{"GET " + runPath: {ok(`{}`), loaded, {404, `{"code": "gone"}`}}}, "gone"},
		"reconciled aborted": {routes{"GET " + runPath: {ok(`{}`), loaded, ok(`{}`), aborted}}, "EOF"},
		"failed":             {routes{"GET " + runPath: {ok(`{}`), failed}}, "do not cut over"},
		"start refused":      {routes{"POST " + runPath + ":start": {{409, `{"code": "integration.invalid_status"}`}}}, "invalid_status"},
		"poll aborted":       {routes{"POST " + runPath + ":start": {ok(`{"id": "op1", "status": "queued"}`)}, "GET /v1/operations/op1": {aborted}}, "EOF"},
		"poll refused":       {routes{"POST " + runPath + ":start": {ok(`{"id": "op1", "status": "queued"}`)}, "GET /v1/operations/op1": {{404, `{"code": "not_found"}`}}}, "not_found"},
		"poll unreadable":    {routes{"POST " + runPath + ":start": {ok(`{"id": "op1", "status": "queued"}`)}, "GET /v1/operations/op1": {ok(`not json`)}}, "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeAPI(t, migration(c.r))
			fails(t, c.want, "migrate", "-connector", "c", "-legal-entities", "le")
		})
	}
	// A cell with a workflow engine answers an operation still running: the command waits for it.
	pollInterval = time.Millisecond
	defer func() { pollInterval = 500 * time.Millisecond }()
	fakeAPI(t, migration(routes{"POST " + runPath + ":start": {ok(`{"id": "op1", "status": "queued"}`)},
		"GET /v1/operations/op1": {ok(`{"id": "op1", "status": "running"}`), ok(`{"id": "op1", "status": "succeeded"}`)},
		"GET " + runPath:         {ok(`{}`), ok(`{"id": "r1", "status": "reconciled"}`)}}))
	if code, got := cli(t, "migrate", "-connector", "c", "-legal-entities", "le"); code != 0 || !strings.Contains(got, "reconciled") {
		t.Fatalf("waiting for the operation: %d %s", code, got)
	}
	fakeAPI(t, migration(routes{"GET " + runPath: {ok(`{}`), failed}}))
	_, got := cli(t, "migrate")
	for _, want := range []string{"FAIL", "difference 1000 211000", "1 SAP documents are not on the platform"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	// A reconciled run whose checks table cannot be printed fails the command.
	fakeAPI(t, migration(routes{"GET " + runPath: {ok(`{}`), ok(`{"id": "r1", "status": "reconciled", "report": {"checks": [{"company_code": "1000", "name": "balance", "pass": true}]}}`)}}))
	out = failing{}
	err := migrate(context.Background(), []string{"-connector", "c", "-legal-entities", "le"})
	out = os.Stdout
	if err == nil || !strings.Contains(err.Error(), "device full") {
		t.Errorf("unprintable report: got %v, want the print failure", err)
	}
	fails(t, "flag provided", "migrate", "-x")
	fakeAPI(t, routes{}, "ERP_TOKEN", "")
	fails(t, "ERP_TOKEN", "migrate")
}

const invoicePath = "/v1/supplier-invoices/i1"

func parked(r routes) routes {
	base := routes{
		"GET /v1/config-versions": {ok(`{"items": [{"document": {"companies": [{"code": "1000", "auto_post_limit": "100.00"}]}}]}`)},
		"GET /v1/legal-entities":  {ok(`{"items": [{"id": "le", "code": "1000"}]}`)},
		"GET /v1/supplier-invoices": {ok(`{"items": [{"id": "i0", "document_number": "5100", "legal_entity_id": "le"},
			{"id": "i1", "document_number": "5101", "legal_entity_id": "le", "purchase_order_id": "po", "gross_amount": {"amount": "10.00"}}]}`)},
		"GET " + invoicePath:             {ok(`{}`)},
		"POST " + invoicePath + ":match": {ok(`{"is_clean": true}`)},
		"POST " + invoicePath + ":post":  {ok(`{}`)},
		"GET /v1/recommendations":        {ok(`{"items": []}`)},
	}
	for k, v := range r {
		base[k] = v
	}
	return base
}

func TestInvoicesErrors(t *testing.T) {
	model(t)
	for name, c := range map[string]struct {
		r    routes
		want string
	}{
		"config aborted":     {routes{"GET /v1/config-versions": {aborted}}, "EOF"},
		"config garbled":     {routes{"GET /v1/config-versions": {ok(`{`)}}, "unexpected end"},
		"no configuration":   {routes{"GET /v1/config-versions": {ok(`{"items": []}`)}}, "no active configuration"},
		"entities aborted":   {routes{"GET /v1/legal-entities": {aborted}}, "EOF"},
		"entities garbled":   {routes{"GET /v1/legal-entities": {ok(`{`)}}, "unexpected end"},
		"invoices aborted":   {routes{"GET /v1/supplier-invoices": {aborted}}, "EOF"},
		"invoices garbled":   {routes{"GET /v1/supplier-invoices": {ok(`{`)}}, "unexpected end"},
		"invoice aborted":    {routes{"GET " + invoicePath: {aborted}}, "EOF"},
		"match aborted":      {routes{"POST " + invoicePath + ":match": {aborted}}, "EOF"},
		"match refused":      {routes{"POST " + invoicePath + ":match": {{422, `{"code": "payables.no_order"}`}}}, "invoice 5101: API 422"},
		"reread aborted":     {routes{"GET " + invoicePath: {ok(`{}`), aborted}}, "EOF"},
		"post aborted":       {routes{"POST " + invoicePath + ":post": {aborted}}, "EOF"},
		"post refused":       {routes{"POST " + invoicePath + ":post": {{409, `{"code": "payables.period_closed"}`}}}, "period_closed"},
		"agent cannot start": {routes{"GET /v1/supplier-invoices": {ok(`{"items": []}`), aborted}}, "EOF"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeAPI(t, parked(c.r))
			fails(t, c.want, "invoices", "-agent")
		})
	}
	// The agent reports the invoices it could not investigate.
	fakeAPI(t, parked(nil))
	if got := must(t, "invoices", "-agent"); !strings.Contains(got, "Posted 1: 5101") || !strings.Contains(got, "5100: apitools: start session") {
		t.Fatalf("agent errors: %s", got)
	}
	fails(t, "flag provided", "invoices", "-x")
	fakeAPI(t, parked(nil), "ERP_AGENT_TOKEN", "")
	fails(t, "ERP_AGENT_TOKEN is not set", "invoices", "-agent")
	fakeAPI(t, routes{}, "ERP_TOKEN", "")
	fails(t, "ERP_TOKEN is not set", "invoices")
}

func TestAgentErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	model(t)
	fakeAPI(t, routes{})
	fails(t, "agent invoices | config", "agent")
	fails(t, "unknown agent", "agent", "bogus")
	fails(t, "flag provided", "agent", "config", "-x")
	fails(t, "say what to change", "agent", "config")
	fails(t, "no such file", "agent", "config", "-draft", "missing.yaml", "raise", "it")
	fails(t, "start session", "agent", "config", "raise", "it")
	fails(t, "flag provided", "agent", "migration", "-x")
	fails(t, "start session", "agent", "migration", "-connector", "c")
	fails(t, "flag provided", "agent", "test", "-x")
	fails(t, "404", "agent", "test", "-change-request", "cr")
	fakeAPI(t, routes{}, "ERP_AGENT_TOKEN", "")
	for _, a := range []string{"config", "migration", "test"} {
		fails(t, "ERP_AGENT_TOKEN is not set", "agent", a, "do", "it")
	}
}

func page(v ...any) reply {
	b, _ := json.Marshal(map[string]any{"items": v})
	return ok(string(b))
}

// facts is the API data analytics reads: one expense, one parked invoice,
// no revenue.
func facts() routes {
	return routes{
		"GET /v1/legal-entities":    {page(map[string]any{"id": "le", "code": "1000", "local_currency": "USD"})},
		"GET /v1/business-partners": {page(map[string]any{"id": "p1", "number": "BP1", "name": "Acme"})},
		"GET /v1/accounts":          {page(map[string]any{"id": "a1", "number": "520000", "name": "Office", "account_type": "expense", "reconciliation_kind": "none"})},
		"GET /v1/open-items":        {page()},
		"GET /v1/journal-entries": {page(map[string]any{"id": "j1", "document_type": "KR", "posting_date": "2026-03-02", "lines": []any{
			map[string]any{"line_no": 1, "account_id": "a1", "amount_lc": map[string]any{"amount": "10.00", "currency": "USD"}}}})},
		"GET /v1/purchase-orders": {page()},
		"GET /v1/supplier-invoices": {page(map[string]any{"id": "i1", "supplier_id": "p1", "invoice_date": "2026-03-05", "status": "parked",
			"gross_amount": map[string]any{"amount": "10.00", "currency": "USD"}})},
	}
}

func TestAnalyticsErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFile(t, "file", nil)
	fakeAPI(t, facts())
	fails(t, "flag provided", "metrics", "-x")
	fails(t, "missing.yaml", "metrics", "-semantic", "missing.yaml")
	fails(t, "flag provided", "query", "-x")
	fails(t, "want \"dim op values\"", "query", "-where", "fiscal_year 2026")
	fails(t, "missing.yaml", "query", "-semantic", "missing.yaml", "-measures", "revenue")
	fails(t, "nope", "query", "-measures", "nope")
	fails(t, "not a directory", "query", "-measures", "invoice_count", "-csv", "file/q.csv")
	// A month without revenue shows a dash.
	if got := must(t, "query", "-measures", "revenue,invoice_count", "-by", "fiscal_month"); !strings.Contains(got, "—") {
		t.Fatalf("query: %s", got)
	}

	fails(t, "flag provided", "ask", "-x")
	fails(t, "ask a question", "ask")
	ask := llmtest.New(t)
	newAgent = func() *analytics.Agent { r := ask.Runner(); return &analytics.Agent{Client: r.Client, Model: r.Model} }
	t.Cleanup(func() { newAgent = analytics.NewAgent })
	fails(t, "script exhausted", "ask", "how", "much")
	ask = llmtest.New(t,
		llmtest.Tool("run_query", map[string]any{"measures": []string{"invoice_count"}}),
		llmtest.ToolFn("build_dashboard", func(b string) any {
			return map[string]any{"spec": analytics.Spec{Title: "Invoices", Tables: []analytics.Table{{Title: "t", QueryID: qidRe.FindString(b)}}}}
		}),
		llmtest.Final("Done."))
	fails(t, "not a directory", "ask", "-out", "file/reports", "how", "many")

	fails(t, "flag provided", "report", "-x")
	fails(t, "-id or -file is required", "report")
	fails(t, "no such file", "report", "-id", "missing")
	rep := func(name, measure string) string {
		b, _ := json.Marshal(analytics.Report{ID: "r1", Spec: analytics.Spec{Title: "t", Tables: []analytics.Table{{Title: "t", QueryID: "q"}}}, Queries: map[string]analytics.Query{"q": {Measures: []string{measure}}}})
		writeFile(t, name, b)
		return name
	}
	fails(t, "query q", "report", "-file", rep("bad.json", "nope"))
	fails(t, "not a directory", "report", "-file", rep("good.json", "invoice_count"), "-out", "file/reports")

	fakeAPI(t, routes{})
	fails(t, "/v1/legal-entities", "query", "-measures", "revenue")
	fakeAPI(t, routes{}, "ERP_TOKEN", "")
	fails(t, "ERP_TOKEN", "query", "-measures", "revenue")
	fails(t, "ERP_TOKEN", "ask", "why")
	fails(t, "ERP_TOKEN", "report", "-file", "good.json")
}

func TestTokenCommand(t *testing.T) {
	minted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dev/token" || r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		var in map[string]string
		if !decodeRequest(t, w, r, &in) {
			return
		}
		if in["subject"] == "nobody" {
			w.WriteHeader(404)
			w.Write([]byte(`{"code": "not_found"}`))
			return
		}
		w.Write([]byte(`{"token": "t-` + in["subject"] + `", "expires_in": 3600}`))
	}))
	defer minted.Close()
	env := map[string]string{"ERP_ADMIN_URL": minted.URL}
	getenv = func(k string) string { return env[k] }
	t.Cleanup(func() { getenv = os.Getenv })
	if got := must(t, "token", "-tenant", "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d01", "-subject", "ada"); !strings.Contains(got, "t-ada") {
		t.Fatalf("token: %s", got)
	}
	fails(t, "flag provided", "token", "-x")
	fails(t, "-subject is required", "token", "-tenant", "x")
	if got := must(t, "token", "-subject", "ops"); !strings.Contains(got, "t-ops") {
		t.Fatalf("operator token: %s", got)
	}
	fails(t, "ERP_DEV_ISSUER", "token", "-tenant", "x", "-subject", "nobody")
	env["ERP_ADMIN_URL"] = "http://127.0.0.1:1"
	fails(t, "connection refused", "token", "-tenant", "x", "-subject", "ada")
	env["ERP_ADMIN_URL"] = "://bad"
	fails(t, "missing protocol scheme", "token", "-tenant", "x", "-subject", "ada")
	delete(env, "ERP_ADMIN_URL") // the default admin listener is not running here
	fails(t, "connection refused", "token", "-tenant", "x", "-subject", "ada")
}

// TestTokenReportsAMalformedAnswer: an answer that is not JSON is reported
// with the decode error, not only its status.
func TestTokenReportsAMalformedAnswer(t *testing.T) {
	minted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"token": `))
	}))
	defer minted.Close()
	getenv = func(k string) string { return map[string]string{"ERP_ADMIN_URL": minted.URL}[k] }
	t.Cleanup(func() { getenv = os.Getenv })
	fails(t, "unexpected EOF", "token", "-subject", "ada")
}

// failing is a writer whose writes and close fail.
type failing struct{}

func (failing) Write([]byte) (int, error) { return 0, errors.New("device full") }
func (failing) Close() error              { return errors.New("close failed") }

// summaryFails prints the table and fails on the summary line after it.
type summaryFails struct{}

func (summaryFails) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "data through") {
		return 0, errors.New("device full")
	}
	return len(p), nil
}

// closeFails writes and fails only on close, as a file whose last buffered
// data could not be stored.
type closeFails struct{ bytes.Buffer }

func (*closeFails) Close() error { return errors.New("close failed") }

// TestQueryReportsLostOutput: a table that cannot be printed and a CSV file
// that cannot be closed (so it may be truncated) fail the command.
func TestQueryReportsLostOutput(t *testing.T) {
	t.Chdir(t.TempDir())
	fakeAPI(t, facts())
	t.Cleanup(func() {
		out, createFile = os.Stdout, func(name string) (io.WriteCloser, error) { return os.Create(name) }
	})
	t.Run("print", func(t *testing.T) {
		out = failing{}
		if err := queryCmd(context.Background(), []string{"-measures", "invoice_count"}); err == nil || !strings.Contains(err.Error(), "device full") {
			t.Errorf("got %v, want the print failure", err)
		}
	})
	t.Run("summary", func(t *testing.T) {
		out = summaryFails{}
		if err := queryCmd(context.Background(), []string{"-measures", "invoice_count"}); err == nil || !strings.Contains(err.Error(), "device full") {
			t.Errorf("got %v, want the summary's print failure", err)
		}
	})
	t.Run("close", func(t *testing.T) {
		out = io.Discard
		createFile = func(string) (io.WriteCloser, error) { return &closeFails{}, nil }
		if err := queryCmd(context.Background(), []string{"-measures", "invoice_count", "-csv", "q.csv"}); err == nil || !strings.Contains(err.Error(), "close failed") {
			t.Errorf("got %v, want the close failure", err)
		}
	})
	t.Run("write and close", func(t *testing.T) {
		out = io.Discard
		createFile = func(string) (io.WriteCloser, error) { return failing{}, nil }
		if err := queryCmd(context.Background(), []string{"-measures", "invoice_count", "-csv", "q.csv"}); err == nil || !strings.Contains(err.Error(), "device full") {
			t.Errorf("got %v, want the write failure, reported first", err)
		}
	})
}

func TestTenantsCommand(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" key="+r.Header.Get("Idempotency-Key")+" if-match="+r.Header.Get("If-Match"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer op" {
			w.WriteHeader(401)
			w.Write([]byte(`{"code": "authn.invalid_token", "detail": "A bearer token is required."}`))
			return
		}
		w.Header().Set("ETag", `"3"`)
		switch {
		case r.URL.Path == "/control/v1/tenants" && r.Method == "GET":
			w.Write([]byte(`{"items": [{"id": "t1", "status": "active"}], "next_page_token": "n"}`))
		case r.URL.Path == "/control/v1/tenants" && r.Method == "POST":
			var in map[string]any
			if !decodeRequest(t, w, r, &in) {
				return
			}
			if r.Header.Get("Idempotency-Key") == "replayed-key" {
				w.Header().Set("Idempotent-Replayed", "true")
			}
			if in["name"] == "" {
				w.WriteHeader(422)
				w.Write([]byte(`{"code": "tenancy.invalid_tenant", "detail": "The tenant request is invalid."}`))
				return
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"id": "t2", "name": "` + in["name"].(string) + `", "status": "active", "provisioned": {"configuration_version": 1}}`))
		case r.URL.Path == "/control/v1/tenants/t1":
			w.Write([]byte(`{"id": "t1", "status": "active", "version": 3}`))
		case r.URL.Path == "/control/v1/tenants/t1/fingerprint":
			w.Write([]byte(`{"id": "t1", "status": "active", "version": 3, "fingerprint": {"tables": [{"name": "identity.principal", "rows": 12}], "audit_chain_head": {"seq": 7, "hash": "ab"}}}`))
		case r.URL.Path == "/control/v1/tenants/t1:suspend" && r.Header.Get("If-Match") == `"3"`:
			w.Write([]byte(`{"id": "t1", "status": "suspended", "version": 4}`))
		case r.URL.Path == "/control/v1/tenants/t1:resume" && r.Header.Get("If-Match") == `"3"`:
			w.Write([]byte(`{"id": "t1", "status": "active", "version": 5}`))
		case r.URL.Path == "/control/v1/tenants/t1:rotateKey" && r.Header.Get("If-Match") == `"3"`:
			w.Write([]byte(`{"id": "t1", "status": "active", "version": 6, "rotation": {"resealed": 2}}`))
		case r.URL.Path == "/control/v1/tenants/t1:erase" && r.Header.Get("If-Match") == `"3"`:
			w.Write([]byte(`{"id": "t1", "status": "offboarded", "version": 7, "erasure": {"objects": 3}}`))
		case r.URL.Path == "/control/v1/tenants/t1:offboard":
			w.WriteHeader(409)
			w.Write([]byte(`{"code": "tenancy.invalid_transition", "detail": "The tenant is offboarded; offboard applies to active or suspended tenants."}`))
		case r.URL.Path == "/control/v1/tenants/plain":
			w.Write([]byte(`not json`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"code": "request.not_found", "detail": "No tenant with this id."}`))
		}
	}))
	defer control.Close()
	env := map[string]string{"ERP_CONTROL_URL": control.URL, "ERP_OPERATOR_TOKEN": "op"}
	getenv = func(k string) string { return env[k] }
	t.Cleanup(func() { getenv = os.Getenv })

	if got := must(t, "tenants", "list", "-status", "active", "-order-by", "name asc", "-page-size", "2", "-page-token", "p"); !strings.Contains(got, `"next_page_token": "n"`) {
		t.Fatalf("list: %s", got)
	}
	if got := must(t, "tenants", "get", "-id", "t1"); !strings.Contains(got, `"etag": "\"3\""`) {
		t.Fatalf("get: %s", got)
	}
	doc, _ := fixture.Document(t).Marshal()
	file := t.TempDir() + "/config.yaml"
	writeFile(t, file, doc)
	if got := must(t, "tenants", "create", "-name", "Acme", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "ada", "-admin-name", "Ada", "-admin-email", "ada@example.com", "-config", file); !strings.Contains(got, `"name": "Acme"`) {
		t.Fatalf("create: %s", got)
	}
	if got := must(t, "tenants", "create", "-name", "Acme", "-region", "eu-west", "-currency", "EUR", "-admin-subject", "ada", "-admin-name", "Ada", "-config", file, "-idempotency-key", "replayed-key"); !strings.Contains(got, "replayed:") {
		t.Fatalf("replay: %s", got)
	}
	if got := must(t, "tenants", "suspend", "-id", "t1"); !strings.Contains(got, `"suspended"`) {
		t.Fatalf("suspend: %s", got)
	}
	if got := must(t, "tenants", "resume", "-id", "t1", "-if-match", `"3"`); got == "" {
		t.Fatalf("resume: %s", got)
	}
	if got := must(t, "tenants", "erase", "-id", "t1"); !strings.Contains(got, `"objects": 3`) {
		t.Fatalf("erase: %s", got)
	}
	if got := must(t, "tenants", "rotate-key", "-id", "t1"); !strings.Contains(got, `"resealed": 2`) {
		t.Fatalf("rotate-key: %s", got)
	}
	fails(t, "tenancy.invalid_transition", "tenants", "offboard", "-id", "t1")
	fails(t, "tenancy.invalid_tenant", "tenants", "create", "-config", file)
	if got := must(t, "tenants", "fingerprint", "-id", "t1"); !strings.Contains(got, `"rows": 12`) || !strings.Contains(got, `"seq": 7`) || !strings.Contains(got, `"etag": "\"3\""`) {
		t.Fatalf("fingerprint: %s", got)
	}
	fails(t, "request.not_found", "tenants", "get", "-id", "nope")
	fails(t, "request.not_found", "tenants", "fingerprint", "-id", "nope")
	fails(t, "-id is required", "tenants", "fingerprint")
	fails(t, "request.not_found", "tenants", "suspend", "-id", "nope")
	fails(t, "not JSON", "tenants", "get", "-id", "plain")
	fails(t, "usage", "tenants")
	fails(t, "unknown command", "tenants", "destroy")
	fails(t, "-id is required", "tenants", "get")
	fails(t, "-id is required", "tenants", "suspend")
	fails(t, "-config is required", "tenants", "create", "-name", "x")
	fails(t, "no such file", "tenants", "create", "-config", "/no/such.yaml")
	for _, sub := range []string{"list", "get", "fingerprint", "create", "suspend"} {
		fails(t, "flag provided", "tenants", sub, "-x")
	}
	env["ERP_OPERATOR_TOKEN"] = "wrong"
	fails(t, "authn.invalid_token", "tenants", "list")
	delete(env, "ERP_OPERATOR_TOKEN")
	fails(t, "ERP_OPERATOR_TOKEN is not set", "tenants", "list")
	env["ERP_OPERATOR_TOKEN"], env["ERP_CONTROL_URL"] = "op", "http://127.0.0.1:1"
	fails(t, "connection refused", "tenants", "list")
	env["ERP_CONTROL_URL"] = "://bad"
	fails(t, "missing protocol scheme", "tenants", "list")
	delete(env, "ERP_CONTROL_URL") // the default admin listener is not running here
	fails(t, "connection refused", "tenants", "list")
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(seen, "\n"), "GET /control/v1/tenants?order_by=name+asc&page_size=2&page_token=p&status=active") ||
		!strings.Contains(strings.Join(seen, "\n"), `POST /control/v1/tenants/t1:suspend key= if-match="3"`) {
		t.Fatalf("calls:\n%s", strings.Join(seen, "\n"))
	}
}
