package taxagent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/taxagent"
)

const (
	reg     = "01a1aa00-0000-7000-8000-000000000e01"
	session = "01a1aa00-0000-7000-8000-000000000a01"
)

// fakeAPI is a stand-in for the cell's API: registration GSTHST and its
// draft of the second quarter, a session, and recommendations answered by
// their content; it keeps every request it was sent.
type fakeAPI struct {
	mu    sync.Mutex
	calls []string
	draft int // the draft's status; 0: 200
	start int // the session's status; 0: 201
	regs  string
	made  int // the recommendations it accepted
}

func (a *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body) // a test's own request
	a.mu.Lock()
	a.calls = append(a.calls, r.Method+" "+r.URL.Path)
	a.mu.Unlock()
	answer := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // a test's answer; a client gone has nothing to read
	}
	switch path := r.URL.Path; {
	case path == "/v1/tax-registrations":
		regs := a.regs
		if regs == "" {
			regs = `{"id": "01a1aa00-0000-7000-8000-000000000e09", "code": "QST"}, {"id": "` + reg + `", "code": "GSTHST", "company_code": "3000"}`
		}
		answer(200, `{"items": [`+regs+`]}`)
	case strings.HasSuffix(path, "/draft-return") && a.draft != 0:
		answer(a.draft, `{"code": "tax.period_not_ended", "status": 409}`)
	case strings.HasSuffix(path, "/draft-return"):
		answer(200, `{"return_type": "GST34", "period_start": "2026-04-01", "period_end": "2026-06-30"}`)
	case path == "/v1/agent-sessions" && a.start != 0:
		answer(a.start, `{"code": "identity.permission_missing", "status": 403}`)
	case path == "/v1/agent-sessions":
		answer(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case path == "/v1/recommendations":
		var body struct{ Content string }
		_ = json.Unmarshal(b, &body) // the agent's own JSON
		if strings.Contains(body.Content, "refuse") {
			answer(422, `{"code": "request.validation_failed", "status": 422}`)
			return
		}
		a.mu.Lock()
		a.made++
		id := fmt.Sprintf("r%d", a.made)
		a.mu.Unlock()
		answer(201, `{"id": "`+id+`"}`)
	default:
		answer(200, `{}`)
	}
}

// recommendations are the recommendation calls the API was sent.
func (a *fakeAPI) recommendations() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(a.calls), func(c string) bool { return !strings.Contains(c, "recommendations") })
}

var sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)

// content is an explanation of the draft as the prompt asks for one.
const content = "GST34 of 2026-04-01 to 2026-06-30: line 103 70.00, up 65.00 on 2026-01-01 to 2026-03-31 from entries 100002 and 100003."

// explain is an explanation of the draft, unless fields change it (nil:
// leave it out), in the session the conversation names.
func explain(fields map[string]any) func(string) any {
	return func(conversation string) any {
		body := map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "tax_registration", "object_id": reg,
			"kind": "explain_draft_return", "content": content}
		for k, v := range fields {
			body[k] = v
			if v == nil {
				delete(body, k)
			}
		}
		return map[string]any{"body": body}
	}
}

func client(t *testing.T, a *fakeAPI) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL, Token: "t"}
}

// TestExplain: the agent explains GSTHST's draft of the second quarter in
// one session, reading through its tools and recommending once; the
// API's refusal and the run's own guard (another session, registration,
// object type, kind or period, a period named only after another, no
// body, a second explanation) are counted, and nothing but the run's one
// explanation reaches the API.
func TestExplain(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t,
		llmtest.Tool("getTaxReturnDraft", map[string]any{"id": reg, "period_start": "2026-04-01"}),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"content": "GST34 of 2026-04-01 to 2026-06-30: refuse it."})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"session_id": reg})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"object_id": session})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"object_type": "tax_return"})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"kind": "prepare_return"})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"content": "GST34 of 2026-01-01 to 2026-03-31, not 2026-04-01 to 2026-06-30: prepare it."})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"content": "Line 103 of 2026-04-01 to 2026-06-30 is 70.00."})),
		llmtest.Tool("createRecommendation", map[string]any{}),
		llmtest.ToolFn("createRecommendation", explain(nil)),
		llmtest.ToolFn("createRecommendation", explain(nil)),
		llmtest.Final("Explained the draft; nothing else."))
	res, err := (&taxagent.Agent{LLM: model.Runner(), API: client(t, a)}).Explain(context.Background(), "GSTHST", "2026-04-01")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(res.Recommendations, " "), "r1"; got != want {
		t.Errorf("recommendations: got %q, want %q", got, want)
	}
	if res.Refused != 9 {
		t.Errorf("refused: got %d, want 9: the API's one and the guard's eight", res.Refused)
	}
	if res.Session != session {
		t.Errorf("session: got %s, want %s", res.Session, session)
	}
	if res.Reply != "Explained the draft; nothing else." {
		t.Errorf("reply: got %q, want the model's last text", res.Reply)
	}
	if got := len(a.recommendations()); got != 2 {
		t.Errorf("recommendations sent: got %d %v, want 2: the guard keeps the others from the API", got, a.recommendations())
	}
	offered := model.ToolNames()
	slices.Sort(offered)
	want := slices.Clone(taxagent.Tools)
	slices.Sort(want)
	if strings.Join(offered, " ") != strings.Join(want, " ") {
		t.Errorf("tools offered: got %v, want %v", offered, want)
	}
	if !strings.Contains(model.Requests[0], "GSTHST (id "+reg+", company 3000) for the period 2026-04-01 to 2026-06-30") {
		t.Errorf("prompt: got %s, want the draft named", model.Requests[0])
	}
}

// TestExplainByID: a registration is found by its id as by its code.
func TestExplainByID(t *testing.T) {
	model := llmtest.New(t, llmtest.ToolFn("createRecommendation", explain(nil)), llmtest.Final("Done."))
	res, err := (&taxagent.Agent{LLM: model.Runner(), API: client(t, &fakeAPI{}), MaxIterations: 5}).Explain(context.Background(), reg, "2026-04-01")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if got := strings.Join(res.Recommendations, " "); got != "r1" {
		t.Errorf("recommendations: got %q, want r1", got)
	}
}

// TestNeverTransitions: the agent's tools read or propose, none moves a
// return, and the operations that do are not open to agents, so no tool
// of theirs can be built (ADR-0049 §13).
func TestNeverTransitions(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range taxagent.Tools {
		t.Run(id, func(t *testing.T) {
			op, ok := ops[id]
			if !ok {
				t.Fatalf("%s: not in the contract", id)
			}
			if op.Method != http.MethodGet && !op.Proposes {
				t.Errorf("%s: got %s without x-autonomy: propose, want a read or a proposal", id, op.Method)
			}
		})
	}
	for _, id := range []string{"prepareTaxReturn", "fileTaxReturn", "withdrawTaxReturn", "approveApprovalRequest", "rejectApprovalRequest",
		"decideRecommendation"} {
		t.Run(id, func(t *testing.T) {
			if ops[id].AgentAllowed {
				t.Errorf("%s: open to agents, want x-agent: false", id)
			}
			if _, err := apitools.Tools(&apitools.Client{}, nil, id); err == nil || !strings.Contains(err.Error(), "not open to agents") {
				t.Errorf("%s as a tool: got %v, want it refused", id, err)
			}
		})
	}
}

// TestExplainRefusals: a registration the agent may not read or that a
// code names twice, a draft the API refuses, a session it may not start,
// an operation the contract does not have, and a model that fails stop
// the run, the last two after the session started.
func TestExplainRefusals(t *testing.T) {
	model := llmtest.New(t)
	twice := `{"id": "a", "code": "GSTHST"}, {"id": "b", "code": "GSTHST"}`
	fake := func(a *fakeAPI) func(*testing.T) *apitools.Client {
		return func(t *testing.T) *apitools.Client { return client(t, a) }
	}
	gone := func(*testing.T) *apitools.Client { return &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"} }
	noDraft := func(t *testing.T) *apitools.Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/tax-registrations" {
				_, _ = w.Write([]byte(`{"items": [{"id": "` + reg + `", "code": "GSTHST"}]}`)) // a test's answer
				return
			}
			_, _ = w.Write([]byte(`[]`)) // a test's answer
		}))
		t.Cleanup(srv.Close)
		return &apitools.Client{BaseURL: srv.URL, Token: "t"}
	}
	for name, c := range map[string]struct {
		api      func(*testing.T) *apitools.Client
		reg      string
		want     string
		sessions bool
	}{
		"another registration":     {fake(&fakeAPI{}), "VAT", "no tax registration VAT", false},
		"a code of two":            {fake(&fakeAPI{regs: twice}), "GSTHST", "give the registration's id", false},
		"a draft refused":          {fake(&fakeAPI{draft: 409}), "GSTHST", "tax.period_not_ended", false},
		"a session not started":    {fake(&fakeAPI{start: 403}), "GSTHST", "identity.permission_missing", false},
		"the model's failure":      {fake(&fakeAPI{}), "GSTHST", "500", true},
		"a draft that is no draft": {noDraft, "GSTHST", "draft return", false},
		"the registrations' list":  {gone, "GSTHST", "127.0.0.1:1", false},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&taxagent.Agent{LLM: model.Runner(), API: c.api(t)}).Explain(context.Background(), c.reg, "2026-04-01")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
			if got := res.Session != ""; got != c.sessions {
				t.Errorf("a session: got %v, want %v", got, c.sessions)
			}
		})
	}
	tools := taxagent.Tools
	taxagent.Tools = append(slices.Clone(tools), "noSuchOperation")
	t.Cleanup(func() { taxagent.Tools = tools })
	a := &fakeAPI{}
	res, err := (&taxagent.Agent{LLM: model.Runner(), API: client(t, a)}).Explain(context.Background(), "GSTHST", "2026-04-01")
	if err == nil || !strings.Contains(err.Error(), "noSuchOperation") {
		t.Errorf("an operation not in the contract: got %v, want it named", err)
	}
	if res.Session != session {
		t.Errorf("session: got %q, want the one started", res.Session)
	}
	if !slices.Contains(a.calls, "POST /v1/agent-sessions/"+session+":end") {
		t.Errorf("the session was not ended: %v", a.calls)
	}
}
