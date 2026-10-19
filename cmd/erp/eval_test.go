package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	capturepkg "github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
)

// replayCell is a cell in process for replay runs (ADR-0039): the public
// API and a control plane that provisions throwaway tenants, both
// trusting one test issuer, which mints every token.
type replayCell struct {
	api, control *httptest.Server
	mint         eval.Mint
	extract      *swapExtractor // the server's own model calls (capture), scripted per case
}

// swapExtractor is capture's extraction on the scripted model the current
// case plays.
type swapExtractor struct {
	mu     sync.Mutex
	runner *llm.Runner
}

func (s *swapExtractor) Extract(ctx context.Context, pdf []byte) (*capturepkg.Extraction, error) {
	s.mu.Lock()
	r := s.runner
	s.mu.Unlock()
	return (&capturepkg.ClaudeExtractor{LLM: r}).Extract(ctx, pdf)
}

func newReplayCell(t *testing.T) replayCell {
	t.Helper()
	// The suites cap their tenants' AI; a cap's routes need priced models.
	pgtest.Exec(t, `INSERT INTO ref.model_price (model, effective_from, currency, input, output, cache_write, cache_read, recorded_by)
		VALUES ('claude-opus-5-5', '2026-01-01', 'USD', 400000000, 2000000000, 500000000, 20000000, 'test') ON CONFLICT DO NOTHING`)
	extract := &swapExtractor{}
	api := apitest.NewWith(t, func(c *app.Config) { c.Extractor = extract })
	is := api.Issuer
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	pool, keys := pgtest.Pool(t), apitest.FieldKeys()
	s := &controlplane.Service{Pool: pool, AppRole: "erp_app", ControlRole: "erp_control", Pages: pages, Keys: keys,
		Provisioner: &provisioning.Provisioner{Pool: pool, AppRole: "erp_app", Keys: keys, Issuers: []string{is.URL},
			Configuration: app.Configuration(app.Config{FieldKeys: keys})}}
	c := replayCell{extract: extract, api: httptest.NewServer(api.Handler), control: httptest.NewServer(controlplane.Handler(s, v, slog.New(slog.NewTextHandler(io.Discard, nil))))}
	t.Cleanup(c.api.Close)
	t.Cleanup(c.control.Close)
	c.mint = func(_ context.Context, tenant, subject string) (string, error) {
		if tenant == "" {
			return is.Token(t, uuid.Nil, subject, map[string]any{"tenant_id": nil}), nil
		}
		return is.Token(t, uuid.MustParse(tenant), subject, nil), nil
	}
	return c
}

// replay plays a case's scripted turns as the model, filling them from the
// saved values and from what the agent sent (the suite's replay_vars).
type replay struct {
	t    *testing.T
	vars map[string]*regexp.Regexp
	cell replayCell
}

func newReplay(t *testing.T, s eval.Suite, c replayCell) replay {
	t.Helper()
	r := replay{t: t, vars: map[string]*regexp.Regexp{}, cell: c}
	for name, re := range s.ReplayVars {
		r.vars[name] = regexp.MustCompile(re)
	}
	return r
}

func (r replay) For(_ context.Context, _ *apitools.Client, c eval.Case, saved map[string]string) (*llm.Runner, error) {
	steps := make([]llmtest.Step, 0, len(c.Replay))
	for _, turn := range c.Replay {
		filled := func(body string, v any) any {
			vals := map[string]string{}
			for k, s := range saved {
				vals[k] = s
			}
			for name, re := range r.vars {
				if m := re.FindStringSubmatch(body); m != nil {
					vals[name] = m[1]
				}
			}
			out, err := eval.Fill(v, vals)
			if err != nil {
				r.t.Errorf("replay of %q: %v", c.Name, err)
			}
			return out
		}
		if turn.Tool == "" {
			text := turn.Final
			steps = append(steps, llmtest.Final(fmt.Sprint(filled("", text))))
			continue
		}
		steps = append(steps, llmtest.ToolFn(turn.Tool, func(body string) any { return filled(body, turn.Input) }))
	}
	runner := llmtest.New(r.t, steps...).Runner()
	r.cell.extract.mu.Lock()
	r.cell.extract.runner = runner // the server's calls play the same turns
	r.cell.extract.mu.Unlock()
	return runner, nil
}

// echo is the test adapter: it opens a session for the task's purpose,
// asks the model which tool to record and records it.
func echo(ctx context.Context, env eval.Env) error {
	s, err := env.Agent.Do(ctx, "POST", "/v1/agent-sessions", nil, map[string]any{"purpose": env.Task["purpose"], "model": env.LLM.Model}, "")
	if err != nil {
		return err
	}
	var session struct{ ID string }
	if err := s.Decode(&session); err != nil {
		return err
	}
	msg, err := env.LLM.Client.Beta.Messages.New(ctx, env.LLM.Params("Name a tool."))
	if err != nil {
		return err
	}
	res, err := env.Agent.Do(ctx, "POST", "/v1/agent-sessions/"+session.ID+"/tool-calls", nil, map[string]any{"tool": llm.Text(msg), "input": map[string]any{}}, "")
	if err != nil {
		return err
	}
	return res.Err()
}

// TestEvalReplay: erp eval run runs a suite through a real control plane
// and API, here with the scripted model in place of the gateway, and
// writes its report; every suite of the repository replays the same way.
func TestEvalReplay(t *testing.T) {
	c := newReplayCell(t)
	evalAgents["echo"] = echo
	t.Cleanup(func() { delete(evalAgents, "echo") })
	vars := map[string]string{"ERP_CONTROL_URL": c.control.URL, "ERP_API_URL": c.api.URL}
	getenv = func(k string) string { return vars[k] }
	t.Cleanup(func() { getenv = os.Getenv })
	suitePath, err := filepath.Abs("testdata/eval/echo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := eval.Load(suitePath)
	if err != nil {
		t.Fatal(err)
	}
	models, mint := evalModels, evalMint
	evalModels = func(string) eval.Models { return newReplay(t, s, c) }
	evalMint = func(string) eval.Mint { return c.mint }
	t.Cleanup(func() { evalModels, evalMint = models, mint })
	dir := t.TempDir()
	got := must(t, "eval", "run", "-suite", suitePath, "-out", dir)
	if !strings.Contains(got, "1 of 1 cases passed, 0 safety failures, level 2") {
		t.Errorf("output: got %s", got)
	}
	reports, _ := filepath.Glob(filepath.Join(dir, "echo", "none", "*.json"))
	if len(reports) != 1 {
		t.Errorf("reports: got %v, want one under echo/none", reports)
	}
	// Every throwaway tenant was offboarded.
	var open int
	pgtest.Scan(t, `SELECT count(*) FROM tenancy.tenant WHERE name LIKE 'eval-%' AND status <> 'offboarded'`, nil, &open)
	if open != 0 {
		t.Errorf("tenants left: got %d, want 0", open)
	}
	// A report that cannot be written fails the run.
	if code, got := cli(t, "eval", "run", "-suite", suitePath, "-repeats", "1", "-out", "/dev/null/x"); code == 0 || !strings.Contains(got, "report directory") {
		t.Errorf("exit %d, output %q, want the report refused", code, got)
	}
	// Its summary not printed, the command fails.
	var errBuf strings.Builder
	out, errOut = failing{}, &errBuf
	t.Cleanup(func() { out, errOut = os.Stdout, os.Stderr })
	if code := run(context.Background(), []string{"eval", "run", "-suite", suitePath, "-out", t.TempDir(), "-repeats", "1"}); code == 0 {
		t.Error("a summary not printed: got exit 0")
	}
}

// TestEvalSuites: every suite of the repository (evals/*/suite.yaml)
// replays to level 2: its replays are what the agent should do, so the
// suite, its setup through the API and its checks hold together. A suite
// that does not is broken, not the agent.
func TestEvalSuites(t *testing.T) {
	suites, _ := filepath.Glob("../../evals/*/suite.yaml")
	if len(suites) == 0 {
		t.Fatal("no suites under evals/")
	}
	for _, path := range suites {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			c := newReplayCell(t)
			vars := map[string]string{"ERP_CONTROL_URL": c.control.URL, "ERP_API_URL": c.api.URL}
			getenv = func(k string) string { return vars[k] }
			t.Cleanup(func() { getenv = os.Getenv })
			s, err := eval.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			models, mint := evalModels, evalMint
			evalModels = func(string) eval.Models { return newReplay(t, s, c) }
			evalMint = func(string) eval.Mint { return c.mint }
			t.Cleanup(func() { evalModels, evalMint = models, mint })
			out := t.TempDir()
			if got := must(t, "eval", "run", "-suite", path, "-out", out); !strings.Contains(got, "safety failures, level 2") {
				reports, _ := filepath.Glob(filepath.Join(out, "*", "*", "*.json"))
				for _, r := range reports {
					b, _ := os.ReadFile(r)
					t.Log(string(b))
				}
				t.Errorf("replay: got %s, want level 2", got)
			}
		})
	}
}

// TestEvalDefaults: outside tests, a run's model is the cell's gateway on
// the suite's alias, and its tokens come from the development issuer.
func TestEvalDefaults(t *testing.T) {
	if g, ok := evalModels("erp-analytics").(eval.Gateway); !ok || g.Alias != "erp-analytics" {
		t.Errorf("models: got %#v, want the gateway on the suite's alias", evalModels("erp-analytics"))
	}
	if _, err := evalMint("http://127.0.0.1:1")(context.Background(), "", "eval-runner"); err == nil {
		t.Error("an unreachable development issuer: got no error")
	}
}

// TestEvalCmdRefusals: the command needs eval run, a readable suite and a
// positive cost limit; a run that fails still writes what it ran.
func TestEvalCmdRefusals(t *testing.T) {
	suitePath, _ := filepath.Abs("testdata/eval/echo.yaml")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"eval"}, "eval run"},
		{[]string{"eval", "go"}, "eval run"},
		{[]string{"eval", "run", "-nope"}, "eval run"},
		{[]string{"eval", "run", "-suite", "missing.yaml"}, "read suite"},
		{[]string{"eval", "run", "-suite", suitePath, "-max-cost", "0"}, "-max-cost"},
		{[]string{"eval", "run", "-suite", suitePath, "-max-cost", "x"}, "-max-cost"},
		{[]string{"eval", "run", "-suite", suitePath}, "no adapter"},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			c.args = append(c.args, "-out", t.TempDir()) // a run never writes into the repository
			code, got := cli(t, c.args...)
			if code == 0 || !strings.Contains(got, c.want) {
				t.Errorf("exit %d, output %q, want a failure with %q", code, got, c.want)
			}
		})
	}
}

// TestAdapterFailures: an adapter whose API cannot be reached fails its
// case; one invoice it could not review fails it too; a document the suite
// names but does not have, or a capture that never reached the API.
func TestAdapterFailures(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	if err := apExceptions(context.Background(), eval.Env{Agent: gone, LLM: &llm.Runner{}}); err == nil {
		t.Error("ap_exceptions without its API: got no error")
	}
	held := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/supplier-invoices":
			_, _ = w.Write([]byte(`{"items": [{"id": "0192a000-0000-7000-8000-000000000701", "document_number": "1900000001"}]}`))
		case "/v1/recommendations":
			_, _ = w.Write([]byte(`{"items": []}`))
		default:
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"code": "platform.unavailable"}`))
		}
	}))
	t.Cleanup(held.Close)
	err := apExceptions(context.Background(), eval.Env{Agent: &apitools.Client{BaseURL: held.URL, Token: "t"}, LLM: &llm.Runner{}})
	if err == nil || !strings.Contains(err.Error(), "1900000001") {
		t.Errorf("an invoice not reviewed: got %v, want its number in the error", err)
	}
	dir := t.TempDir()
	if err := captureEval(context.Background(), eval.Env{Agent: gone, Dir: dir, Task: map[string]any{"document": "none.pdf"}}); err == nil {
		t.Error("a missing document: got no error")
	}
	if err := os.WriteFile(filepath.Join(dir, "a.pdf"), []byte("%PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := captureEval(context.Background(), eval.Env{Agent: gone, Dir: dir, Task: map[string]any{"document": "a.pdf"}}); err == nil {
		t.Error("a capture that never reached the API: got no error")
	}
}
