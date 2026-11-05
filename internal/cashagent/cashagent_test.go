package cashagent_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/cashagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
)

const (
	le      = "01a1aa00-0000-7000-8000-000000000201"
	session = "01a1aa00-0000-7000-8000-000000000a01"
)

// api is a stand-in for the cell's API: the company 3000 and its
// variance, a session, and proposals answered by their bodies; it keeps
// every request it was sent.
type api struct {
	mu       sync.Mutex
	calls    []string
	variance int // the variance's status; 0: 200
	start    int // the session's status; 0: 201
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	case path == "/v1/legal-entities":
		answer(200, `{"items": [{"id": "01a1aa00-0000-7000-8000-000000000999", "code": "1000"}, {"id": "`+le+`", "code": "3000"}]}`)
	case path == "/v1/liquidity-forecast-variance" && a.variance != 0:
		answer(a.variance, `{"code": "cash.snapshot_not_taken", "status": 409}`)
	case path == "/v1/agent-sessions" && a.start != 0:
		answer(a.start, `{"code": "identity.permission_missing", "status": 403}`)
	case path == "/v1/agent-sessions":
		answer(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case path == "/v1/liquidity-forecast-variance/comment-proposals" || path == "/v1/memo-record-proposals":
		var body struct{ Text, Reason string }
		_ = json.Unmarshal(b, &body) // the agent's own JSON
		if strings.Contains(body.Text+body.Reason, "refuse") {
			answer(422, `{"code": "cash.citation_not_attributed", "status": 422}`)
			return
		}
		answer(201, `{"id": "`+strings.TrimSpace(body.Text+body.Reason)+`"}`)
	default:
		answer(200, `{}`)
	}
}

// proposals are the proposal calls the API was sent.
func (a *api) proposals() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(a.calls), func(c string) bool { return !strings.Contains(c, "proposals") })
}

var sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)

// comment is a comment proposal on company 3000's variance of the 17th at
// lag 0, unless fields change it (nil: leave it out), with the session the
// conversation names.
func comment(text string, fields map[string]any) func(string) any {
	return func(conversation string) any {
		body := map[string]any{"legal_entity_id": le, "from": "2026-04-17", "to": "2026-04-17", "lag": 0, "liquidity_item": "OP_SUPPLIERS",
			"text": text, "session_id": sessionRe.FindStringSubmatch(conversation)[1], "citations": []any{map[string]any{"kind": "payment", "id": le}}}
		for k, v := range fields {
			body[k] = v
			if v == nil {
				delete(body, k)
			}
		}
		return map[string]any{"body": body}
	}
}

// memo is a memo record proposal of company 3000 unless fields change it.
func memo(reason string, fields map[string]any) func(string) any {
	return func(conversation string) any {
		body := map[string]any{"legal_entity_id": le, "session_id": sessionRe.FindStringSubmatch(conversation)[1], "reason": reason,
			"amount": map[string]any{"amount": "1200.00", "currency": "CAD"}, "value_date": "2026-05-01", "liquidity_item": "REC_CUSTOMERS"}
		for k, v := range fields {
			body[k] = v
		}
		return map[string]any{"body": body}
	}
}

func client(t *testing.T, a *api) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL, Token: "t"}
}

// TestExplain: the agent explains company 3000's variance in one session,
// reading through its tools and proposing; a proposal the API answers
// twice is one, the API's refusals and the run's own guard (another
// company, period or lag) are counted, and nothing outside the run's
// variance reaches the API.
func TestExplain(t *testing.T) {
	a := &api{}
	model := llmtest.New(t,
		llmtest.Tool("getForecastVarianceDocuments", map[string]any{"legal_entity_id": le, "from": "2026-04-17", "to": "2026-04-17", "lag": 0}),
		llmtest.ToolFn("proposeForecastVarianceComment", comment("c1", nil)),
		llmtest.ToolFn("proposeForecastVarianceComment", comment("c1", nil)),
		llmtest.ToolFn("proposeForecastVarianceComment", comment("refuse", nil)),
		llmtest.ToolFn("proposeForecastVarianceComment", comment("other period", map[string]any{"to": "2026-04-18"})),
		llmtest.ToolFn("proposeForecastVarianceComment", comment("other lag", map[string]any{"lag": 7})),
		llmtest.ToolFn("proposeForecastVarianceComment", comment("default lag", map[string]any{"lag": nil})),
		llmtest.ToolFn("proposeForecastVarianceComment", comment("other company", map[string]any{"legal_entity_id": session})),
		llmtest.ToolFn("proposeMemoRecord", memo("m1", nil)),
		llmtest.ToolFn("proposeMemoRecord", memo("other company", map[string]any{"legal_entity_id": session})),
		llmtest.Final("OP_SUPPLIERS: proposed. TAX: not explained, no document behind it."))
	res, err := (&cashagent.Agent{LLM: model.Runner(), API: client(t, a)}).Explain(context.Background(),
		cashagent.Period{Company: "3000", From: "2026-04-17", To: "2026-04-17", Lag: 0})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(res.Comments, " "), "c1"; got != want {
		t.Errorf("comments: got %q, want %q", got, want)
	}
	if got, want := strings.Join(res.Memos, " "), "m1"; got != want {
		t.Errorf("memo records: got %q, want %q", got, want)
	}
	if res.Refused != 6 {
		t.Errorf("refused: got %d, want 6: the API's one and the guard's five", res.Refused)
	}
	if res.Session != session || !strings.Contains(res.Reply, "not explained") {
		t.Errorf("session and reply: got %s %q", res.Session, res.Reply)
	}
	if got := len(a.proposals()); got != 4 {
		t.Errorf("proposals sent: got %d %v, want 4: the guard keeps the others from the API", got, a.proposals())
	}
	offered := model.ToolNames()
	slices.Sort(offered)
	want := slices.Clone(cashagent.Tools)
	slices.Sort(want)
	if strings.Join(offered, " ") != strings.Join(want, " ") {
		t.Errorf("tools offered: got %v, want %v", offered, want)
	}
}

// TestExplainDefaultLag: a run at the API's default lag admits a
// proposal that names none.
func TestExplainDefaultLag(t *testing.T) {
	a := &api{}
	model := llmtest.New(t, llmtest.ToolFn("proposeForecastVarianceComment", comment("c7", map[string]any{"lag": nil})), llmtest.Final("Done."))
	res, err := (&cashagent.Agent{LLM: model.Runner(), API: client(t, a), MaxIterations: 5}).Explain(context.Background(),
		cashagent.Period{Company: "3000", From: "2026-04-17", To: "2026-04-17", Lag: cashagent.DefaultLag})
	if err != nil || strings.Join(res.Comments, " ") != "c7" {
		t.Errorf("got %v %v, want the proposal", res, err)
	}
}

// TestExplainRefusals: a company the agent may not read, a variance the
// API refuses, a session it may not start, an operation the contract
// does not have, and a model that fails stop the run, the last two after
// the session started.
func TestExplainRefusals(t *testing.T) {
	period := cashagent.Period{Company: "3000", From: "2026-04-17", To: "2026-04-17", Lag: 0}
	model := llmtest.New(t)
	for name, c := range map[string]struct {
		a      *api
		period cashagent.Period
		want   string
	}{
		"another company":          {&api{}, cashagent.Period{Company: "9999"}, "no company 9999"},
		"a variance not compared":  {&api{variance: 409}, period, "cash.snapshot_not_taken"},
		"a session not started":    {&api{start: 403}, period, "identity.permission_missing"},
		"the model's failure":      {&api{}, period, "500"},
		"the legal entities' list": {&api{}, period, ""},
	} {
		t.Run(name, func(t *testing.T) {
			ag := &cashagent.Agent{LLM: model.Runner(), API: client(t, c.a)}
			if name == "the legal entities' list" {
				ag.API = &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
			}
			res, err := ag.Explain(context.Background(), c.period)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
			if name == "the model's failure" && res.Session != session {
				t.Errorf("session: got %q, want the one started", res.Session)
			}
		})
	}
	tools := cashagent.Tools
	cashagent.Tools = append(slices.Clone(tools), "noSuchOperation")
	t.Cleanup(func() { cashagent.Tools = tools })
	a := &api{}
	res, err := (&cashagent.Agent{LLM: model.Runner(), API: client(t, a)}).Explain(context.Background(), period)
	if err == nil || !strings.Contains(err.Error(), "noSuchOperation") || res.Session != session {
		t.Errorf("an operation not in the contract: got %v %v", res, err)
	}
	if !slices.Contains(a.calls, "POST /v1/agent-sessions/"+session+":end") {
		t.Errorf("the session was not ended: %v", a.calls)
	}
}
