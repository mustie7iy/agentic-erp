package agents_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"agents.session.read", "agents.session.record", "agents.recommendation.decide"}

func setup(t *testing.T) (apitest.Client, apitest.Client) {
	t.Helper()
	ten := pgtest.NewTenant(t, perms...)
	person := apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: ten.Human}
	agent := person
	agent.As = ten.Agent
	return person, agent
}

func TestSessionsAndRecommendations(t *testing.T) {
	person, agent := setup(t)
	person.Problem(403, "agents.not_an_agent", "POST", "/v1/agent-sessions", `{"purpose": "p", "model": "m"}`, "")
	s, tag := agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Investigate held invoices", "model": "claude-opus-5-5"}`, "")
	path := "/v1/agent-sessions/" + s["id"].(string)
	if s["status"] != "running" || s["agent_id"] != agent.As.ID.String() || s["cost"] != nil || tag != `"1"` {
		t.Fatalf("started: %v", s)
	}
	for i, tool := range []string{"getSupplierInvoice", "holdSupplierInvoice"} {
		c, _ := agent.Must(201, "POST", path+"/tool-calls", `{"tool": "`+tool+`", "input": {"id": "x"}, "result_summary": "200", "is_error": false}`, "")
		if c["seq"] != float64(i+1) || c["input"].(map[string]any)["id"] != "x" || c["result_summary"] != "200" {
			t.Fatalf("tool call: %v", c)
		}
	}
	agent.Must(201, "POST", path+"/tool-calls", `{"tool": "listOpenItems", "input": {}}`, "")
	person.Problem(403, "agents.not_your_session", "POST", path+"/tool-calls", `{"tool": "x", "input": {}}`, "")
	agent.Problem(404, "request.not_found", "POST", "/v1/agent-sessions/"+uuid.NewString()+"/tool-calls", `{"tool": "x", "input": {}}`, "")
	calls, _ := person.Must(200, "GET", path+"/tool-calls?page_size=2", "", "")
	if len(calls["items"].([]any)) != 2 || calls["next_page_token"] == nil {
		t.Fatalf("calls: %v", calls)
	}
	rest, _ := person.Must(200, "GET", path+"/tool-calls?page_size=2&page_token="+url.QueryEscape(calls["next_page_token"].(string)), "", "")
	if items := rest["items"].([]any); len(items) != 1 || items[0].(map[string]any)["seq"] != float64(3) || rest["next_page_token"] != nil {
		t.Fatalf("rest: %v", rest)
	}
	person.Problem(404, "request.not_found", "GET", "/v1/agent-sessions/"+uuid.NewString()+"/tool-calls", "", "")
	person.Problem(400, "request.invalid_page_token", "GET", path+"/tool-calls?page_token=x", "", "")

	object := uuid.NewString()
	body := `{"session_id": "` + s["id"].(string) + `", "object_type": "supplier_invoice", "object_id": "` + object + `", "kind": "release_hold", "content": "Release it."}`
	rec, rtag := agent.Must(201, "POST", "/v1/recommendations", body, "")
	if rec["outcome"] != nil || rec["agent_id"] != agent.As.ID.String() || rtag != `"1"` {
		t.Fatalf("recommendation: %v", rec)
	}
	agent.Problem(422, "request.reference_not_found", "POST", "/v1/recommendations", strings.Replace(body, s["id"].(string), uuid.NewString(), 1), "")
	made := apitest.Published(t, person.Tenant, outbox.Event{Context: "agents", Name: "recommendation_made"})
	if len(made) != 1 || made[0]["content"] != "Release it." {
		t.Fatalf("made: %v", made)
	}
	recPath := "/v1/recommendations/" + rec["id"].(string)
	agent.Problem(403, "identity.agent_not_allowed", "POST", recPath+":decide", `{"outcome": "accepted"}`, rtag)
	person.Problem(422, "agents.invalid_outcome", "POST", recPath+":decide", `{"outcome": "maybe"}`, rtag)
	decided, rtag := person.Must(200, "POST", recPath+":decide", `{"outcome": "accepted", "comment": "Done"}`, rtag)
	if decided["outcome"] != "accepted" || decided["decided_by"] != person.As.ID.String() {
		t.Fatalf("decided: %v", decided)
	}
	person.Problem(409, "agents.already_decided", "POST", recPath+":decide", `{"outcome": "ignored"}`, rtag)
	if ev := apitest.Published(t, person.Tenant, outbox.Event{Context: "agents", Name: "recommendation_decided"}); len(ev) != 1 || ev[0]["outcome"] != "accepted" {
		t.Fatalf("decided events: %v", ev)
	}
	agent.Must(201, "POST", "/v1/recommendations", body, "")
	for query, n := range map[string]int{"?outcome=open": 1, "?outcome=accepted": 1, "?object_type=supplier_invoice&object_id=" + object + "&session_id=" + s["id"].(string): 2} {
		if page, _ := person.Must(200, "GET", "/v1/recommendations"+query, "", ""); len(page["items"].([]any)) != n {
			t.Errorf("%s: %v", query, page)
		}
	}
	page, _ := person.Must(200, "GET", "/v1/recommendations?page_size=1", "", "")
	if page["next_page_token"] == nil {
		t.Fatalf("recommendations page: %v", page)
	}
	if got, _ := person.Must(200, "GET", recPath, "", ""); got["outcome"] != "accepted" {
		t.Fatalf("get: %v", got)
	}

	agent.Problem(422, "agents.invalid_status", "POST", path+":end", `{"status": "running", "input_tokens": 1, "output_tokens": 1}`, tag)
	agent.Problem(422, "agents.unknown_currency", "POST", path+":end", `{"status": "completed", "input_tokens": 1, "output_tokens": 1,
		"cost": {"amount": "1.00", "currency": "XXX"}}`, tag)
	agent.Problem(400, "request.invalid_amount", "POST", path+":end", `{"status": "completed", "input_tokens": 1, "output_tokens": 1,
		"cost": {"amount": "1.0", "currency": "USD"}}`, tag)
	ended, _ := agent.Must(200, "POST", path+":end", `{"status": "completed", "input_tokens": 18200, "output_tokens": 2100,
		"cost": {"amount": "0.42", "currency": "USD"}}`, tag)
	if ended["status"] != "completed" || ended["cost"].(map[string]any)["amount"] != "0.42" || ended["ended_at"] == nil {
		t.Fatalf("ended: %v", ended)
	}
	agent.Problem(409, "agents.session_ended", "POST", path+"/tool-calls", `{"tool": "x", "input": {}}`, "")
	other, otag := agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "Quick", "model": "m"}`, "")
	quick, _ := agent.Must(200, "POST", "/v1/agent-sessions/"+other["id"].(string)+":end", `{"status": "refused", "input_tokens": 0, "output_tokens": 0}`, otag)
	if quick["cost"].(map[string]any)["amount"] != "0.00" {
		t.Fatalf("no cost: %v", quick)
	}
	list, _ := person.Must(200, "GET", "/v1/agent-sessions?agent_id="+agent.As.ID.String()+"&status=completed", "", "")
	if len(list["items"].([]any)) != 1 {
		t.Fatalf("sessions: %v", list)
	}
	list, _ = person.Must(200, "GET", "/v1/agent-sessions?page_size=1", "", "")
	if list["next_page_token"] == nil {
		t.Fatalf("sessions page: %v", list)
	}
	if got, _ := person.Must(200, "GET", path, "", ""); got["input_tokens"] != float64(18200) {
		t.Fatalf("get: %v", got)
	}
	person.Problem(404, "request.not_found", "GET", "/v1/agent-sessions/"+uuid.NewString(), "", "")
	person.Problem(404, "request.not_found", "GET", "/v1/recommendations/"+uuid.NewString(), "", "")
}

func TestDatabaseFailures(t *testing.T) {
	person, agent := setup(t)
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	h := &agents.Endpoints{Pages: pages}
	s, _ := agent.Must(201, "POST", "/v1/agent-sessions", `{"purpose": "p", "model": "m"}`, "")
	session := uuid.MustParse(s["id"].(string))
	agent.Must(201, "POST", "/v1/agent-sessions/"+s["id"].(string)+"/tool-calls", `{"tool": "x", "input": {"a": 1}}`, "")
	rec, _ := agent.Must(201, "POST", "/v1/recommendations", `{"session_id": "`+s["id"].(string)+`", "object_type": "x", "object_id": "`+uuid.NewString()+
		`", "kind": "y", "content": "z"}`, "")
	recID := uuid.MustParse(rec["id"].(string))
	byAgent := map[string]func(ctx context.Context) error{
		"start": func(ctx context.Context) error {
			_, err := h.StartAgentSession(ctx, &gen.AgentSessionStart{Purpose: "p", Model: "m"}, gen.StartAgentSessionParams{})
			return err
		},
		"end": func(ctx context.Context) error {
			_, err := h.EndAgentSession(ctx, &gen.AgentSessionEnd{Status: "completed", Cost: gen.NewOptMoney(gen.Money{Amount: "1.00", Currency: "EUR"})},
				gen.EndAgentSessionParams{ID: session, IfMatch: "*"})
			return err
		},
		"record": func(ctx context.Context) error {
			_, err := h.RecordAgentToolCall(ctx, &gen.AgentToolCallCreate{Tool: "x", Input: gen.AgentToolCallCreateInput{"a": []byte("1")},
				ResultSummary: gen.NewOptString("ok")}, gen.RecordAgentToolCallParams{ID: session})
			return err
		},
		"recommend": func(ctx context.Context) error {
			_, err := h.CreateRecommendation(ctx, &gen.RecommendationCreate{SessionID: session, ObjectType: "x", ObjectID: uuid.New(), Kind: "y", Content: "z"},
				gen.CreateRecommendationParams{})
			return err
		},
	}
	byPerson := map[string]func(ctx context.Context) error{
		"get session": func(ctx context.Context) error {
			_, err := h.GetAgentSession(ctx, gen.GetAgentSessionParams{ID: session})
			return err
		},
		"list sessions": func(ctx context.Context) error {
			_, err := h.ListAgentSessions(ctx, gen.ListAgentSessionsParams{})
			return err
		},
		"list calls": func(ctx context.Context) error {
			_, err := h.ListAgentToolCalls(ctx, gen.ListAgentToolCallsParams{ID: session})
			return err
		},
		"get recommendation": func(ctx context.Context) error {
			_, err := h.GetRecommendation(ctx, gen.GetRecommendationParams{ID: recID})
			return err
		},
		"list recommendations": func(ctx context.Context) error {
			_, err := h.ListRecommendations(ctx, gen.ListRecommendationsParams{})
			return err
		},
		"decide": func(ctx context.Context) error {
			_, err := h.DecideRecommendation(ctx, &gen.RecommendationDecide{Outcome: "ignored"}, gen.DecideRecommendationParams{ID: recID, IfMatch: "*"})
			return err
		},
	}
	for as, calls := range map[string]map[string]func(ctx context.Context) error{"agent": byAgent, "person": byPerson} {
		actor := person.As
		if as == "agent" {
			actor = agent.As
		}
		for name, call := range calls {
			if call(context.Background()) == nil {
				t.Errorf("%s outside a unit of work", name)
			}
			apitest.EveryFailure(t, person.Tenant, actor, name, call)
		}
	}
	no := func(int32) error { return json.Unmarshal([]byte("x"), new(any)) }
	for name, call := range map[string]func(ctx context.Context) error{
		"end mismatch": func(ctx context.Context) error {
			_, err := agents.EndSession(ctx, session, no, agents.Usage{})
			return err
		},
		"decide mismatch": func(ctx context.Context) error { _, err := agents.Decide(ctx, recID, no, "accepted", ""); return err },
		"decide unknown": func(ctx context.Context) error {
			_, err := agents.Decide(ctx, uuid.New(), no, "accepted", "")
			return err
		},
		"end unknown": func(ctx context.Context) error {
			_, err := agents.EndSession(ctx, uuid.New(), no, agents.Usage{})
			return err
		},
		"sessions order": func(ctx context.Context) error {
			_, err := h.ListAgentSessions(ctx, gen.ListAgentSessionsParams{OrderBy: gen.NewOptString("x")})
			return err
		},
		"recommendations order": func(ctx context.Context) error {
			_, err := h.ListRecommendations(ctx, gen.ListRecommendationsParams{OrderBy: gen.NewOptString("x")})
			return err
		},
		"calls page size": func(ctx context.Context) error {
			_, err := h.ListAgentToolCalls(ctx, gen.ListAgentToolCallsParams{ID: session, PageSize: gen.NewOptInt32(0)})
			return err
		},
	} {
		if err := apitest.InRequest(t, person.Tenant, agent.As, call); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
