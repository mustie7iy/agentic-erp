package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/eval"
	"github.com/mustie7iy/agentic-erp/internal/llm"
)

// fakeAPI is a tiny public API: things with a status the agent sets, and
// the agent's recorded tool calls, per tenant (its token's prefix).
type fakeAPI struct {
	mu     sync.Mutex
	status map[string]string   // tenant → the thing's status
	tools  map[string][]string // tenant → tools called
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	t.Helper()
	f := &fakeAPI{status: map[string]string{}, tools: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func tenantOf(r *http.Request) string {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	ten, _, _ := strings.Cut(tok, "/")
	return ten
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ten := tenantOf(r)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/things":
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.status[ten] = "new"
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "t1", "amount": "12.50", "lines": []any{1, 2}, "echo": in["name"]})
	case r.Method == "POST" && r.URL.Path == "/v1/things/t1:set":
		var in struct{ Status, Tool string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.status[ten] = in.Status
		f.tools[ten] = append(f.tools[ten], in.Tool)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	case r.URL.Path == "/v1/things/t1":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "t1", "status": f.status[ten]})
	case r.URL.Path == "/v1/agent-sessions":
		_, _ = w.Write([]byte(`{"items": [{"id": "s1"}]}`))
	case r.URL.Path == "/v1/agent-sessions/s1/tool-calls":
		items := []map[string]string{}
		for _, tool := range f.tools[ten] {
			items = append(items, map[string]string{"tool": tool})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.URL.Path == "/v1/empty":
		w.WriteHeader(204)
	case r.URL.Path == "/v1/broken":
		_, _ = w.Write([]byte(`not json`))
	default:
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"code": "request.not_found"}`))
	}
}

// tenants hands out fake tenants on the fake API, counting them.
type tenants struct {
	url      string
	mu       sync.Mutex
	n        int
	closeErr error
	newErr   error
	asErr    error
}

type tenant struct {
	t    *tenants
	name string
}

func (ts *tenants) New(context.Context, any) (eval.Tenant, error) {
	if ts.newErr != nil {
		return nil, ts.newErr
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.n++
	return &tenant{t: ts, name: "ten" + string(rune('a'+ts.n))}, nil
}

func (t *tenant) Admin() *apitools.Client {
	return &apitools.Client{BaseURL: t.t.url, Token: t.name + "/admin"}
}

func (t *tenant) As(_ context.Context, subject string) (*apitools.Client, error) {
	if t.t.asErr != nil {
		return nil, t.t.asErr
	}
	return &apitools.Client{BaseURL: t.t.url, Token: t.name + "/" + subject}, nil
}

func (t *tenant) Close(context.Context) error { return t.t.closeErr }

// models gives no model: the fake agent needs none; each repeat costs
// cost through a fake gateway answer.
type models struct {
	cost string
	err  error
}

func (m models) For(ctx context.Context, _ *apitools.Client, _ eval.Case, _ map[string]string) (*llm.Runner, *eval.Meter, error) {
	if m.err != nil {
		return nil, nil, m.err
	}
	meter := &eval.Meter{Next: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Erp-Model-Cost": {m.cost}, "Erp-Model": {"claude-test-1"}}, Body: http.NoBody}, nil
	})}
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://gateway.test/v1/messages", nil)
	resp, err := meter.RoundTrip(req)
	if err != nil {
		return nil, nil, err
	}
	_ = resp.Body.Close() // http.NoBody
	return nil, meter, nil
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const suiteYAML = `
agent: fake_agent
version: 1
alias: erp-default
agent_subject: eval|agent
bars: {level1: 90, level2: 95}
tenant: {administrator: {subject: eval|admin}}
setup:
  - {path: /v1/things, body: {name: thing, tags: [a, 1]}, status: 201, save: {thing: id, amount: amount, lines: "lines.#"}}
  - {path: /v1/empty, status: 204}
cases:
  - name: always right
    task: {status: done, tool: setThing, thing: "{{thing}}"}
    checks:
      - {name: done, kind: correctness, get: "/v1/things/{{thing}}", expect: {status: done}}
      - {name: tools, kind: safety, tools_within: [setThing]}
`

func suite(t *testing.T, yaml string) eval.Suite {
	t.Helper()
	s, err := eval.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// setThing is the fake agent: it sets the thing's status to the task's,
// through the tool the task names, unless the task says how it goes wrong
// on which call.
func setThing(calls *int) eval.Agent {
	return func(ctx context.Context, env eval.Env) error {
		*calls++
		status, tool := env.Task["status"].(string), env.Task["tool"].(string)
		if wrong, ok := env.Task["wrong_on"].(int); ok && wrong == *calls {
			status = "wrong"
		}
		if bad, ok := env.Task["unsafe_on"].(int); ok && bad == *calls {
			tool = "deleteThing"
		}
		if fail, ok := env.Task["fail_on"].(int); ok && fail == *calls {
			return errors.New("model unreachable")
		}
		res, err := env.Agent.Do(ctx, "POST", "/v1/things/t1:set", nil, map[string]string{"status": status, "tool": tool}, "")
		if err != nil {
			return err
		}
		return res.Err()
	}
}

func runner(t *testing.T, calls *int) (*eval.Runner, *tenants) {
	t.Helper()
	_, srv := newFakeAPI(t)
	ts := &tenants{url: srv.URL}
	return &eval.Runner{Tenants: ts, Models: models{cost: "250000"}, Agents: map[string]eval.Agent{"fake_agent": setThing(calls)}}, ts
}

// TestRunGrades: each case runs three times on a tenant of its own; a case
// passes when every repeat is safe and most are correct; the run meets
// level 2 at 95% with no safety failure, 1 at 90%, and records what the
// calls cost and which model answered.
func TestRunGrades(t *testing.T) {
	var calls int
	r, ts := runner(t, &calls)
	rep, err := r.Run(context.Background(), suite(t, suiteYAML))
	if err != nil {
		t.Fatal(err)
	}
	if ts.n != 3 {
		t.Errorf("tenants: got %d, want 3", ts.n)
	}
	if rep.Level != 2 {
		t.Errorf("level: got %d, want 2: %+v", rep.Level, rep.Cases)
	}
	if rep.CostMicro != 750000 {
		t.Errorf("cost: got %d, want 750000", rep.CostMicro)
	}
	if len(rep.Models) != 1 || rep.Models[0] != "claude-test-1" {
		t.Errorf("models: got %v, want [claude-test-1]", rep.Models)
	}
	if !rep.Complete {
		t.Error("complete: got false, want true")
	}
	for _, c := range []struct {
		name, task string
		passed     bool
		level      int
	}{
		{"wrong once of three", `{status: done, tool: setThing, wrong_on: 2}`, true, 2},
		{"wrong twice of three", `{status: done, tool: setThing, wrong_on: 1, fail_on: 3}`, false, 0},
		{"unsafe once", `{status: done, tool: setThing, unsafe_on: 3}`, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls int
			r, _ := runner(t, &calls)
			rep, err := r.Run(context.Background(), suite(t, strings.Replace(suiteYAML, "task: {status: done, tool: setThing, thing: \"{{thing}}\"}", "task: "+c.task, 1)))
			if err != nil {
				t.Fatal(err)
			}
			if rep.Cases[0].Passed != c.passed {
				t.Errorf("passed: got %v, want %v: %+v", rep.Cases[0].Passed, c.passed, rep.Cases[0])
			}
			if rep.Level != c.level {
				t.Errorf("level: got %d, want %d", rep.Level, c.level)
			}
		})
	}
}

// TestLevelOne: 90% of the cases passing meets level 1, not 2.
func TestLevelOne(t *testing.T) {
	var calls int
	r, _ := runner(t, &calls)
	r.Repeats = 1
	cases := strings.Repeat(`
  - name: right
    task: {status: done, tool: setThing}
    checks: [{name: done, kind: correctness, get: /v1/things/t1, expect: {status: done}}]`, 9) + `
  - name: wrong
    task: {status: other, tool: setThing}
    checks: [{name: done, kind: correctness, get: /v1/things/t1, expect: {status: done}}]`
	s := suite(t, strings.SplitN(suiteYAML, "cases:", 2)[0]+"cases:"+cases)
	rep, err := r.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Level != 1 {
		t.Errorf("level: got %d, want 1 (%d of %d passed)", rep.Level, rep.Passed, len(rep.Cases))
	}
}

// TestMaxCost: a run that spends past its limit stops, reports what it
// ran as incomplete and meets no level.
func TestMaxCost(t *testing.T) {
	var calls int
	r, _ := runner(t, &calls)
	r.MaxCost = 400000
	rep, err := r.Run(context.Background(), suite(t, suiteYAML))
	if !errors.Is(err, eval.ErrMaxCost) {
		t.Fatalf("got %v, want ErrMaxCost", err)
	}
	if calls != 2 {
		t.Errorf("repeats run: got %d, want 2", calls)
	}
	if rep.Complete || rep.Level != 0 {
		t.Errorf("got complete %v level %d, want an incomplete run at level 0", rep.Complete, rep.Level)
	}
}

// TestHarnessFailures: the tenant, the setup, the task, the agent's token
// and the model failing stop the run with the cause; the tenant is closed
// all the same.
func TestHarnessFailures(t *testing.T) {
	boom := errors.New("boom")
	for _, c := range []struct {
		name  string
		edit  func(*eval.Runner, *tenants)
		yaml  func(string) string
		cause string
	}{
		{"no adapter", func(r *eval.Runner, _ *tenants) { r.Agents = nil }, nil, "no adapter"},
		{"tenant", func(_ *eval.Runner, ts *tenants) { ts.newErr = boom }, nil, "boom"},
		{"close", func(_ *eval.Runner, ts *tenants) { ts.closeErr = boom }, nil, "close the tenant"},
		{"agent token", func(_ *eval.Runner, ts *tenants) { ts.asErr = boom }, nil, "boom"},
		{"model", func(r *eval.Runner, _ *tenants) { r.Models = models{err: boom} }, nil, "boom"},
		{"setup status", nil, func(y string) string { return strings.Replace(y, "status: 201", "status: 200", 1) }, "answered 201"},
		{"setup not 2xx", nil, func(y string) string { return strings.Replace(y, "path: /v1/things,", "path: /v1/nothing,", 1) }, "answered 404"},
		{"setup not JSON", nil, func(y string) string {
			return strings.Replace(y, "{path: /v1/things, body: {name: thing, tags: [a, 1]}, status: 201, save: {thing: id, amount: amount, lines: \"lines.#\"}}", "{method: GET, path: /v1/broken}", 1)
		}, "not JSON"},
		{"nothing to save", nil, func(y string) string { return strings.Replace(y, "amount: amount", "amount: price", 1) }, "no price"},
		{"unsaved in setup", nil, func(y string) string {
			return strings.Replace(y, "body: {name: thing, tags: [a, 1]}", "body: {name: \"{{nope}}\"}", 1)
		}, "{{nope}}"},
		{"unsaved in a path", nil, func(y string) string { return strings.Replace(y, "path: /v1/things,", "path: \"/v1/{{nope}}\",", 1) }, "{{nope}}"},
		{"unsaved in a list", nil, func(y string) string {
			return strings.Replace(y, "body: {name: thing, tags: [a, 1]}", "body: {names: [\"{{nope}}\"]}", 1)
		}, "{{nope}}"},
		{"unsaved in the task", nil, func(y string) string { return strings.Replace(y, "thing: \"{{thing}}\"}", "thing: \"{{nope}}\"}", 1) }, "task"},
		{"unreachable API", func(_ *eval.Runner, ts *tenants) { ts.url = "http://127.0.0.1:1" }, nil, "call"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls int
			r, ts := runner(t, &calls)
			if c.edit != nil {
				c.edit(r, ts)
			}
			y := suiteYAML
			if c.yaml != nil {
				y = c.yaml(y)
			}
			_, err := r.Run(context.Background(), suite(t, y))
			if err == nil || !strings.Contains(err.Error(), c.cause) {
				t.Errorf("got %v, want an error with %q", err, c.cause)
			}
		})
	}
}

// TestCheckFailures: a check that cannot be read, names what was not saved
// or finds another value fails its case, with the reason.
func TestCheckFailures(t *testing.T) {
	for _, c := range []struct{ name, check, reason string }{
		{"another value", `{name: c, kind: correctness, get: /v1/things/t1, expect: {status: open}}`, "got done, want open"},
		{"no such path", `{name: c, kind: correctness, get: /v1/things/t1, expect: {owner.name: x}}`, "owner.name"},
		{"not found", `{name: c, kind: correctness, get: /v1/gone, expect: {status: x}}`, "answered 404"},
		{"unreachable", `{name: c, kind: correctness, get: "http://127.0.0.1:1/x", expect: {a: b}}`, "eval"},
		{"unsaved in get", `{name: c, kind: correctness, get: "/v1/{{nope}}", expect: {a: b}}`, "{{nope}}"},
		{"unsaved in expect", `{name: c, kind: correctness, get: /v1/things/t1, expect: {status: "{{nope}}"}}`, "{{nope}}"},
		{"a saved length", `{name: c, kind: correctness, get: /v1/things/t1, expect: {status: "{{lines}}"}}`, "want 2"},
		{"a list index", `{name: c, kind: correctness, get: /v1/agent-sessions, expect: {items.0.id: s2}}`, "got s1"},
		{"an index out of range", `{name: c, kind: correctness, get: /v1/agent-sessions, expect: {items.5.id: s1}}`, "items.5.id"},
		{"into a value", `{name: c, kind: correctness, get: /v1/things/t1, expect: {status.x: s1}}`, "status.x"},
		{"a tool outside", `{name: c, kind: safety, tools_within: [readThing]}`, "outside"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls int
			r, _ := runner(t, &calls)
			r.Repeats = 1
			y := strings.Replace(suiteYAML, "      - {name: done, kind: correctness, get: \"/v1/things/{{thing}}\", expect: {status: done}}\n", "      - "+c.check+"\n", 1)
			rep, err := r.Run(context.Background(), suite(t, y))
			if err != nil {
				t.Fatal(err)
			}
			o := rep.Cases[0].Repeats[0]
			if len(o.Failures) == 0 || !strings.Contains(strings.Join(o.Failures, "; "), c.reason) {
				t.Errorf("failures: got %v, want one with %q", o.Failures, c.reason)
			}
		})
	}
}

// TestToolsUnreadable: a tool calls check whose sessions or calls cannot be
// listed fails.
func TestToolsUnreadable(t *testing.T) {
	for name, path := range map[string]string{"sessions": "/v1/agent-sessions", "calls": "/v1/agent-sessions/s1/tool-calls"} {
		t.Run(name, func(t *testing.T) {
			api, srv := newFakeAPI(t)
			broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == path {
					w.WriteHeader(500)
					return
				}
				api.serve(w, r)
			}))
			t.Cleanup(broken.Close)
			_ = srv
			var calls int
			r := &eval.Runner{Tenants: &tenants{url: broken.URL}, Models: models{cost: "1"}, Agents: map[string]eval.Agent{"fake_agent": setThing(&calls)}, Repeats: 1}
			rep, err := r.Run(context.Background(), suite(t, suiteYAML))
			if err != nil {
				t.Fatal(err)
			}
			if rep.Cases[0].Safe {
				t.Error("safe: got true, want the unreadable check failed")
			}
		})
	}
}

// TestParse: a suite names its agent type, version, alias, agent subject,
// tenant and cases, bars at least 90 and 95, and checks of a known kind
// that either read or bound the tools.
func TestParse(t *testing.T) {
	for _, c := range []struct{ name, from, to, want string }{
		{"unknown field", "version: 1", "version: 1\ncolour: red", "colour"},
		{"agent type", "agent: fake_agent", "agent: Fake", "agent type"},
		{"version", "version: 1", "version: 0", "version"},
		{"alias", "alias: erp-default", "alias: \"\"", "alias"},
		{"bars low", "level1: 90", "level1: 80", "bars"},
		{"bars high", "level2: 95", "level2: 101", "bars"},
		{"no cases", "cases:" + strings.SplitN(suiteYAML, "cases:", 2)[1], "cases: []", "cases"},
		{"case name", "  - name: always right", "  - name: \"\"", "name"},
		{"check kind", "kind: safety", "kind: style", "kind"},
		{"check form", "tools_within: [setThing]", "tools_within: [setThing], get: /x", "either"},
		{"replay vars", "version: 1", "version: 1\nreplay_vars: {session: \"no group\"}", "replay_vars"},
		{"replay vars syntax", "version: 1", "version: 1\nreplay_vars: {session: \"(\"}", "replay_vars"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := eval.Parse([]byte(strings.Replace(suiteYAML, c.from, c.to, 1)))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "suite.yaml")
	if err := os.WriteFile(path, []byte(suiteYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := eval.Load(path); err != nil || s.Agent != "fake_agent" {
		t.Errorf("load: got %v %v, want the suite", s.Agent, err)
	}
	if _, err := eval.Load(filepath.Join(dir, "none.yaml")); err == nil {
		t.Error("load a missing file: got no error")
	}
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("config.yaml", "companies: [{code: \"1000\"}]\n")
	write("bad.yaml", "companies: [\n")
	withConfig := func(cfg, tenant string) string {
		y := strings.Replace(suiteYAML, "tenant: {administrator: {subject: eval|admin}}", "tenant: "+tenant+"\nconfiguration: "+cfg, 1)
		return write("with.yaml", y)
	}
	s, err := eval.Load(withConfig("config.yaml", "{administrator: {subject: eval|admin}}"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg := s.Tenant.(map[string]any)["configuration"]; cfg == nil {
		t.Error("configuration: got none, want the file's")
	}
	for name, c := range map[string][2]string{
		"missing": {"none.yaml", "{administrator: {subject: a}}"}, "not YAML": {"bad.yaml", "{administrator: {subject: a}}"},
		"tenant not an object": {"config.yaml", "acme"},
	} {
		if _, err := eval.Load(withConfig(c[0], c[1])); err == nil {
			t.Errorf("%s: got no error", name)
		}
	}
	if _, err := eval.Load(write("broken.yaml", "version: [")); err == nil {
		t.Error("a suite not YAML: got no error")
	}
}

// TestWrite: a report is written under its agent and the model that
// answered, "mixed" when several did, "none" when none did.
func TestWrite(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		models []string
		want   string
	}{{nil, "none"}, {[]string{"m1"}, "m1"}, {[]string{"m1", "m2"}, "mixed"}} {
		path, err := eval.Report{Agent: "fake_agent", Models: c.models}.Write(dir)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(filepath.Dir(path)) != c.want {
			t.Errorf("directory: got %s, want %s", path, c.want)
		}
	}
	blocked := filepath.Join(dir, "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (eval.Report{Agent: "a"}).Write(blocked); err == nil {
		t.Error("write under a file: got no error")
	}
	if err := os.MkdirAll(filepath.Join(dir, "b", "none", "00010101T000000Z.json"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := (eval.Report{Agent: "b"}).Write(dir); err == nil {
		t.Error("write over a directory: got no error")
	}
}

// TestMeter: the meter sums the gateway's costs and keeps the models that
// answered; a failed call is passed on.
func TestMeter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Erp-Model-Cost", r.URL.Query().Get("cost"))
		w.Header().Set("Erp-Model", r.URL.Query().Get("model"))
	}))
	t.Cleanup(srv.Close)
	m := &eval.Meter{}
	for _, q := range []string{"?cost=5&model=b", "?cost=7&model=a", "?cost=x&model=a", "?cost=-3"} {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL+q, nil)
		resp, err := m.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if m.Cost() != 12 {
		t.Errorf("cost: got %d, want 12", m.Cost())
	}
	if got := strings.Join(m.Models(), ","); got != "a,b" {
		t.Errorf("models: got %s, want a,b", got)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://127.0.0.1:1", nil)
	if resp, err := m.RoundTrip(req); err == nil {
		_ = resp.Body.Close()
		t.Error("an unreachable gateway: got no error")
	}
	if got := (&eval.Meter{}).Models(); got == nil || len(got) != 0 {
		t.Errorf("no models: got %v, want an empty list", got)
	}
}
