package planningagent_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/mustie7iy/agentic-erp/internal/planningagent"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

const (
	session  = "01a1aa00-0000-7000-8000-000000000a01"
	le       = "01a1aa00-0000-7000-8000-000000000b01"
	otherLE  = "01a1aa00-0000-7000-8000-000000000b02"
	fc       = "01a1aa00-0000-7000-8000-000000000c01"
	admin    = "01a1aa00-0000-7000-8000-000000000d01" // CC-1100, active
	sales    = "01a1aa00-0000-7000-8000-000000000d02" // CC-1300, active
	pending  = "01a1aa00-0000-7000-8000-000000000d03" // CC-1900, pending
	foreign  = "01a1aa00-0000-7000-8000-000000000d04" // CC-2100 of another company
	office   = "01a1aa00-0000-7000-8000-000000000e01" // 6000, in the plan against actual
	travel   = "01a1aa00-0000-7000-8000-000000000e02" // 6100, in a plan line
	elsewise = "01a1aa00-0000-7000-8000-000000000e03" // an account of another company's plan
	unknown  = "01a1aa00-0000-7000-8000-000000000e09"
)

// centers are the companies' cost centers.
const centers = `{"items": [
	{"id": "` + admin + `", "legal_entity_id": "` + le + `", "code": "CC-1100", "name": "Finance office", "status": "active"},
	{"id": "` + sales + `", "legal_entity_id": "` + le + `", "code": "CC-1300", "name": "Sales office", "status": "active"},
	{"id": "` + pending + `", "legal_entity_id": "` + le + `", "code": "CC-1900", "name": "New office", "status": "pending"},
	{"id": "` + foreign + `", "legal_entity_id": "` + otherLE + `", "code": "CC-2100", "name": "Berlin office", "status": "active"}]}`

// planActual is the plan against actual of category FC in 2026: CC-1100
// and CC-1300 on 6000, and a row of another company.
const planActual = `{"items": [
	{"legal_entity_id": "` + le + `", "cost_center_id": "` + admin + `", "account_id": "` + office + `", "fiscal_year": 2026,
	 "plan": {"amount": "1050.00", "currency": "CAD"}, "actual": {"amount": "1250.00", "currency": "CAD"}, "variance": {"amount": "200.00", "currency": "CAD"}},
	{"legal_entity_id": "` + le + `", "cost_center_id": "` + sales + `", "account_id": "` + office + `", "fiscal_year": 2026,
	 "plan": {"amount": "350.00", "currency": "CAD"}, "actual": {"amount": "-80.00", "currency": "CAD"}, "variance": {"amount": "-430.00", "currency": "CAD"}},
	{"legal_entity_id": "` + otherLE + `", "cost_center_id": "` + foreign + `", "account_id": "` + elsewise + `", "fiscal_year": 2026,
	 "plan": {"amount": "990.00", "currency": "EUR"}, "actual": {"amount": "999.00", "currency": "EUR"}, "variance": {"amount": "9.00", "currency": "EUR"}}]}`

// planLines are the category's lines: a cost center's on 6100, a profit
// center's on another account, and another company's.
const planLines = `{"items": [
	{"legal_entity_id": "` + le + `", "cost_center_id": "` + admin + `", "account_id": "` + travel + `", "fiscal_period": 11, "amount": {"amount": "75.00", "currency": "CAD"}},
	{"legal_entity_id": "` + le + `", "profit_center_id": "` + admin + `", "account_id": "` + unknown + `", "fiscal_period": 11, "amount": {"amount": "5.00", "currency": "CAD"}},
	{"legal_entity_id": "` + otherLE + `", "cost_center_id": "` + foreign + `", "account_id": "` + elsewise + `", "fiscal_period": 11, "amount": {"amount": "6.00", "currency": "EUR"}}]}`

// fakeAPI is a stand-in for the cell's API: companies 3000 (CAD) and 2000,
// plan category FC, open or locked, the cost centers, the plan against
// actual and the plan lines, a session and recommendations answered by
// their bodies; it keeps every request it was sent.
type fakeAPI struct {
	mu         sync.Mutex
	calls      []string
	locked     bool
	noCategory bool
	categories int // the categories' status; 0: 200
	reads      int // the status of the cost centers, the plan against actual and the plan lines; 0: 200
	companies  int // the companies' status; 0: 200
	made       int
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
	case path == "/v1/legal-entities" && a.companies != 0:
		answer(a.companies, `{"code": "request.forbidden", "status": 403}`)
	case path == "/v1/legal-entities":
		answer(200, `{"items": [{"id": "`+otherLE+`", "code": "2000", "local_currency": "EUR"}, {"id": "`+le+`", "code": "3000", "local_currency": "CAD"}]}`)
	case path == "/v1/plan-categories" && a.categories != 0:
		answer(a.categories, `{"code": "identity.permission_missing", "status": 403}`)
	case path == "/v1/plan-categories" && a.noCategory:
		answer(200, `{"items": []}`)
	case path == "/v1/plan-categories":
		answer(200, fmt.Sprintf(`{"items": [{"id": %q, "code": "FC", "name": "Forecast 2026", "locked": %v}]}`, fc, a.locked))
	case a.reads != 0 && (path == "/v1/cost-centers" || path == "/v1/cost-center-plan-actual" || path == "/v1/plan-lines"):
		answer(a.reads, `{"code": "identity.permission_missing", "status": 403}`)
	case path == "/v1/cost-centers":
		answer(200, centers)
	case path == "/v1/cost-center-plan-actual":
		answer(200, planActual)
	case path == "/v1/plan-lines":
		answer(200, planLines)
	case path == "/v1/agent-sessions":
		answer(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case path == "/v1/recommendations":
		a.recommended(b, answer)
	default:
		answer(200, `{}`)
	}
}

// recommended answers a recommendation with its body.
func (a *fakeAPI) recommended(b []byte, answer func(int, string)) {
	var body struct {
		ObjectID   string `json:"object_id"`
		ObjectType string `json:"object_type"`
		Kind       string
	}
	_ = json.Unmarshal(b, &body) // the agent's own JSON
	a.mu.Lock()
	a.made++
	id := fmt.Sprintf("r%d", a.made)
	a.mu.Unlock()
	answer(201, `{"id": "`+id+`", "object_id": "`+body.ObjectID+`", "object_type": "`+body.ObjectType+`", "kind": "`+body.Kind+`"}`)
}

// sent are the calls the API was sent whose method and path contain
// what.
func (a *fakeAPI) sent(what string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(a.calls), func(c string) bool { return !strings.Contains(c, what) })
}

func client(t *testing.T, a *fakeAPI) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL, Token: "t"}
}

var sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)

// rec is a plan proposal with the content on category FC, in the
// session the conversation names.
func rec(content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "plan_category", "object_id": fc,
			"kind": "plan_proposal", "content": content}}
	})
}

// reads are the reads a run makes before it proposes.
var reads = []llmtest.Step{
	llmtest.Tool("listCostCenters", map[string]any{"legal_entity_id": le}),
	llmtest.Tool("listCostCenterPlanActual", map[string]any{"plan_category_id": fc, "legal_entity_id": le, "fiscal_year": 2026}),
	llmtest.Tool("listPlanLines", map[string]any{"legal_entity_id": le, "fiscal_year": 2026, "plan_category_id": fc}),
}

// line is a plan line of the proposal.
func line(center, account string, period int, amount string) string {
	return fmt.Sprintf("Line %s account %s period %d: %s", center, account, period, amount)
}

// TestPropose: the agent proposes category FC's plan of 2027 for company
// 3000 in one session and recommends on it once. Its check refuses, and
// counts, every proposal a controller could not apply as it stands or
// whose figures the run did not read; nothing refused reaches the API.
func TestPropose(t *testing.T) {
	why := "\nWhy CC-1100: actual 1250.00 CAD against a plan of 1050.00 CAD."
	good := line(admin, office, 11, "1250.00 CAD") + "\n" + line(admin, travel, 11, "75.00 CAD") + "\n" + line(sales, office, 11, "-80.00 CAD") + why +
		"\nWhy CC-1300: actual 80.00 CAD below the plan of 350.00 CAD."
	refusals := []struct {
		content string
		want    error
	}{
		{"Plan CC-1100 at 1250.00 CAD." + why, planningagent.ErrForm},
		{"Line CC-1100 for 1250.00 CAD" + why, planningagent.ErrForm},
		{line(admin, office, 2026, "1250.00 CAD") + why, planningagent.ErrForm},
		{"Line CC-1100 account 6000 period 11: 1250.00 CAD" + why, planningagent.ErrUnread},
		{line(admin, office, 11, "1312.50 CAD") + why, planningagent.ErrUnread},
		{line(pending, office, 11, "1250.00 CAD") + why, planningagent.ErrCenter},
		{line(foreign, office, 11, "1250.00 CAD") + why, planningagent.ErrCenter},
		{line(admin, elsewise, 11, "1250.00 CAD") + why, planningagent.ErrAccount},
		{line(admin, unknown, 11, "1250.00 CAD") + why, planningagent.ErrAccount},
		{line(admin, office, 11, "1250.00 EUR") + why, planningagent.ErrAmount},
		{line(admin, office, 11, "2026 CAD") + why, planningagent.ErrAmount},
		{line(admin, office, 11, "1250.00 CAD") + "\n" + line(admin, office, 11, "1050.00 CAD") + why, planningagent.ErrTwice},
		{line(admin, office, 11, "1250.00 CAD"), planningagent.ErrReason},
		{line(admin, office, 11, "1250.00 CAD") + "\nWhy CC-1100: the plan was 1050.00 CAD.", planningagent.ErrReason},
		{line(sales, office, 11, "350.00 CAD") + why, planningagent.ErrReason},
	}
	steps := slices.Clone(reads)
	for _, r := range refusals {
		steps = append(steps, rec(r.content))
	}
	steps = append(steps, rec(good), llmtest.Final("Proposed the 2027 plan of FC; the controller decides."))
	a := &fakeAPI{}
	model := llmtest.New(t, steps...)
	res, err := (&planningagent.Agent{LLM: model.Runner(), API: client(t, a)}).Propose(context.Background(), "3000", "FC", 2027)
	if err != nil {
		t.Fatal(err)
	}
	if want := []recommend.Made{{ID: "r1", ObjectType: "plan_category", ObjectID: fc, Kind: "plan_proposal"}}; !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != len(refusals) {
		t.Errorf("refused: got %d, want %d", res.Refused, len(refusals))
	}
	if got := len(a.sent("recommendations")); got != 1 {
		t.Errorf("recommendations sent: got %d, want 1: the check keeps the others from the API", got)
	}
	prompt := "Company 3000 (legal entity " + le + "), whose currency is CAD. Plan category FC, Forecast 2026 (id " + fc +
		"). Propose its plan of fiscal year 2027 for the company's cost centers from fiscal year 2026's plan and actual figures."
	if !strings.Contains(model.Requests[0], prompt) {
		t.Errorf("prompt: got %s, want %q", model.Requests[0], prompt)
	}
	// Each refusal is told in the request after its recommendation: the
	// first after the reads' and the first request.
	for i, r := range refusals {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			told, _ := json.Marshal(r.want.Error()) // the model reads the refusal as JSON text
			msg := strings.Trim(string(told), `"`)
			if before, after := strings.Count(model.Requests[len(reads)+i], msg), strings.Count(model.Requests[len(reads)+i+1], msg); after != before+1 {
				t.Errorf("refusal of %q: told %d times before, %d after; want %v once more", r.content, before, after, r.want)
			}
		})
	}
}

// TestProposeWithoutReads: a proposal before the run read the cost
// centers and the plan, or after the API refused it them, is refused: the
// check knows only what the run read, and nothing reaches the API.
func TestProposeWithoutReads(t *testing.T) {
	proposed := rec(line(admin, office, 11, "1250.00 CAD") + "\nWhy CC-1100: actual 1250.00 CAD.")
	for name, c := range map[string]struct {
		a     *fakeAPI
		steps []llmtest.Step
	}{
		"nothing read":  {&fakeAPI{}, []llmtest.Step{proposed}},
		"reads refused": {&fakeAPI{reads: 403}, append(slices.Clone(reads), proposed)},
	} {
		t.Run(name, func(t *testing.T) {
			model := llmtest.New(t, append(c.steps, llmtest.Final("Nothing read, nothing proposed."))...)
			res, err := (&planningagent.Agent{LLM: model.Runner(), API: client(t, c.a)}).Propose(context.Background(), "3000", "FC", 2027)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Recommendations) != 0 || res.Refused != 1 {
				t.Errorf("got %v, refused %d, want none made and one refused", res.Recommendations, res.Refused)
			}
			if got := len(c.a.sent("recommendations")); got != 0 {
				t.Errorf("recommendations sent: got %d, want none", got)
			}
		})
	}
}

// TestNever: the agent's tools are reads and the one recommendation; none
// is an operation of its never list, every write of planning is on it,
// and the contract closes each to agents (ADR-0072, T105e).
func TestNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	tools := planningagent.Tools()
	for _, id := range tools {
		t.Run(id, func(t *testing.T) {
			op, ok := ops[id]
			if !ok {
				t.Fatalf("%s: not in the contract", id)
			}
			if op.Method != http.MethodGet && !op.Reads && id != recommend.Proposer {
				t.Errorf("%s: got %s, want a read", id, op.Method)
			}
		})
	}
	for id, op := range ops {
		if op.Method != http.MethodGet && strings.Contains(id, "Plan") && !slices.Contains(planningagent.Never, id) {
			t.Errorf("never list: got %v, want it to hold %s", planningagent.Never, id)
		}
	}
	for _, id := range planningagent.Never {
		t.Run(id, func(t *testing.T) {
			if slices.Contains(tools, id) {
				t.Errorf("%s is among the agent's tools", id)
			}
			if op, ok := ops[id]; !ok || op.AgentAllowed {
				t.Errorf("%s: got in the contract %v, open to agents %v; want it there, closed to agents", id, ok, op.AgentAllowed)
			}
		})
	}
}

// TestProposeRefusals: a company or category the agent may not read or
// that does not exist, a locked category, and an API that is gone stop
// the run before a session starts.
func TestProposeRefusals(t *testing.T) {
	model := llmtest.New(t)
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		api      *apitools.Client
		company  string
		want     string
		sentinel error
	}{
		"the companies refused":  {client(t, &fakeAPI{companies: 403}), "3000", "planningagent: the companies: ", nil},
		"no such company":        {client(t, &fakeAPI{}), "9999", "no company 9999 that the agent may read", nil},
		"the categories refused": {client(t, &fakeAPI{categories: 403}), "3000", "plan category FC: ", nil},
		"no such category":       {client(t, &fakeAPI{noCategory: true}), "3000", "no plan category FC", nil},
		"a locked category":      {client(t, &fakeAPI{locked: true}), "3000", "locked", planningagent.ErrLocked},
		"the API gone":           {gone, "3000", "127.0.0.1:1", nil},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&planningagent.Agent{LLM: model.Runner(), API: c.api}).Propose(context.Background(), c.company, "FC", 2027)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
			if c.sentinel != nil && !errors.Is(err, c.sentinel) {
				t.Errorf("got %v, want %v", err, c.sentinel)
			}
			if res.Session != "" {
				t.Errorf("a session: got %s, want none", res.Session)
			}
		})
	}
}
