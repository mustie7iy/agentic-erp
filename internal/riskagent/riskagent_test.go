package riskagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/recommend"
	"github.com/mustie7iy/agentic-erp/internal/riskagent"
)

const (
	session    = "01a1aa00-0000-7000-8000-000000000a01"
	tenant     = "01a1aa00-0000-7000-8000-000000000a02"
	failing    = "01a1aa00-0000-7000-8000-000000000b01" // duplicate payments: its control failed its last test
	clear      = "01a1aa00-0000-7000-8000-000000000b02" // payroll errors: its control passed, its exception overridden
	excepted   = "01a1aa00-0000-7000-8000-000000000b03" // access misuse: its control passed, with an exception accepted
	bare       = "01a1aa00-0000-7000-8000-000000000b04" // fraud: no control
	retiring   = "01a1aa00-0000-7000-8000-000000000b05" // old process: its one control retired
	payControl = "01a1aa00-0000-7000-8000-000000000c01"
	hrControl  = "01a1aa00-0000-7000-8000-000000000c02"
	orgControl = "01a1aa00-0000-7000-8000-000000000c03"
	oldControl = "01a1aa00-0000-7000-8000-000000000c04"
	failedTest = "01a1aa00-0000-7000-8000-000000000d01"
	otherTest  = "01a1aa00-0000-7000-8000-000000000d09"
	exception  = "01a1aa00-0000-7000-8000-000000000e01"
)

// risks are the open register: one per state of its controls.
const risks = `{"id": "` + failing + `", "title": "Duplicate payments to suppliers", "category": "Payments", "likelihood": 1, "impact": 4, "score": 4,
	"controls": [{"control_id": "` + payControl + `", "code": "PAY-01", "status": "active"}]},
	{"id": "` + clear + `", "title": "Payroll errors", "category": "People", "likelihood": 2, "impact": 3, "score": 6,
	"controls": [{"control_id": "` + hrControl + `", "code": "HR-01", "status": "active"}]},
	{"id": "` + excepted + `", "title": "Access misuse", "category": "Access", "likelihood": 2, "impact": 5, "score": 10,
	"controls": [{"control_id": "` + orgControl + `", "code": "ORG-01", "status": "active"}]},
	{"id": "` + bare + `", "title": "Fraud", "category": "Fraud", "likelihood": 1, "impact": 5, "score": 5, "controls": []},
	{"id": "` + retiring + `", "title": "Old process", "category": "Legacy", "likelihood": 1, "impact": 1, "score": 1,
	"controls": [{"control_id": "` + oldControl + `", "code": "OLD-01", "status": "retired"}]}`

// controls are the library with each control's last test.
const controls = `{"id": "` + payControl + `", "code": "PAY-01", "last_test": {"id": "` + failedTest + `", "result": "failed", "tested_on": "2026-11-15"}},
	{"id": "` + hrControl + `", "code": "HR-01", "last_test": {"id": "01a1aa00-0000-7000-8000-000000000d02", "result": "passed", "tested_on": "2026-11-01"}},
	{"id": "` + orgControl + `", "code": "ORG-01", "last_test": {"id": "01a1aa00-0000-7000-8000-000000000d03", "result": "passed", "tested_on": "2026-11-02"}},
	{"id": "` + oldControl + `", "code": "OLD-01"}`

// findings are the recommendations on controls, the newest first: ORG-01's
// latest an exception a person accepted before an older no_exception, HR-01's an
// exception a person overrode, a kind of another agent and PAY-01's
// no_exception.
const findings = `{"id": "` + exception + `", "object_id": "` + orgControl + `", "kind": "exception_found", "outcome": "accepted"},
	{"id": "01a1aa00-0000-7000-8000-000000000e02", "object_id": "` + orgControl + `", "kind": "no_exception"},
	{"id": "01a1aa00-0000-7000-8000-000000000e03", "object_id": "` + hrControl + `", "kind": "exception_found", "outcome": "overridden"},
	{"id": "01a1aa00-0000-7000-8000-000000000e04", "object_id": "` + oldControl + `", "kind": "review"},
	{"id": "01a1aa00-0000-7000-8000-000000000e05", "object_id": "` + payControl + `", "kind": "no_exception"}`

// fakeAPI is a stand-in for the cell's API: the agent's tenant, the open
// register, the library, the recommendations on controls, a control's
// tests, a session, and recommendations made answered by their bodies; it
// keeps every request it was sent.
type fakeAPI struct {
	mu      sync.Mutex
	calls   []string
	refuse  string // a path answered 403
	risks   string // the open risks; "": risks
	made    int
	created []string // the contents recommended
}

func (a *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body) // a test's own request
	a.mu.Lock()
	a.calls = append(a.calls, r.Method+" "+r.URL.RequestURI())
	a.mu.Unlock()
	answer := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // a test's answer; a client gone has nothing to read
	}
	if r.URL.Path == a.refuse {
		answer(403, `{"code": "identity.permission_missing", "status": 403}`)
		return
	}
	switch {
	case r.URL.Path == "/v1/me":
		answer(200, `{"id": "01a1aa00-0000-7000-8000-000000000a03", "tenant_id": "`+tenant+`"}`)
	case r.URL.Path == "/v1/risks":
		list := a.risks
		if list == "" {
			list = risks
		}
		answer(200, `{"items": [`+list+`]}`)
	case r.URL.Path == "/v1/controls":
		answer(200, `{"items": [`+controls+`]}`)
	case r.URL.Path == "/v1/controls/"+payControl+"/tests":
		answer(200, `{"items": [{"id": "`+failedTest+`", "result": "failed", "tested_on": "2026-11-15"}]}`)
	case r.URL.Path == "/v1/recommendations" && r.Method == http.MethodGet:
		answer(200, `{"items": [`+findings+`]}`)
	case r.URL.Path == "/v1/agent-sessions":
		answer(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case r.URL.Path == "/v1/recommendations":
		a.recommended(b, answer)
	default:
		answer(200, `{}`)
	}
}

func (a *fakeAPI) recommended(b []byte, answer func(int, string)) {
	var body struct {
		ObjectID   string `json:"object_id"`
		ObjectType string `json:"object_type"`
		Kind       string
		Content    string
	}
	_ = json.Unmarshal(b, &body) // the agent's own JSON
	a.mu.Lock()
	a.made++
	id := fmt.Sprintf("r%d", a.made)
	a.created = append(a.created, body.Content)
	a.mu.Unlock()
	answer(201, `{"id": "`+id+`", "object_id": "`+body.ObjectID+`", "object_type": "`+body.ObjectType+`", "kind": "`+body.Kind+`"}`)
}

func client(t *testing.T, a *fakeAPI) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL, Token: "t"}
}

var sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)

// rec is a recommendation of kind with the content on the object, in the
// session the conversation names.
func rec(objectType, object, kind, content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": objectType,
			"object_id": object, "kind": kind, "content": content}}
	})
}

// refusedWith reports whether the model was told the refusal.
func refusedWith(t *testing.T, requests []string, want error) {
	t.Helper()
	told, _ := json.Marshal(want.Error()) // the model reads the refusal as JSON text
	if !strings.Contains(strings.Join(requests, "\n"), strings.Trim(string(told), `"`)) {
		t.Errorf("the model was not told %q", want)
	}
}

// TestReview: the agent reviews the five open risks and the tenant in
// one session and recommends once on each. Its check refuses, and counts,
// a rating equal to the risk's or off the scale, a new rating of a risk
// whose controls all passed with no exception, a new rating citing
// nothing, one citing a test no read returned, one writing a number no
// read returned, and a new risk without its title and category; nothing
// refused reaches the API.
func TestReview(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t,
		llmtest.Tool("listControlTests", map[string]any{"id": payControl}),
		llmtest.Tool("listRecommendations", map[string]any{"object_type": "control"}),
		rec("risk", failing, "rate_again", "Rate: likelihood 1, impact 4\nAs it was."),
		rec("risk", failing, "rate_again", "Rate: likelihood 6, impact 4\nOff the scale."),
		rec("risk", clear, "rate_again", "Rate: likelihood 3, impact 3\nPAY-01 failed (control test "+failedTest+")."),
		rec("risk", failing, "rate_again", "Rate: likelihood 2, impact 4\nIts control failed."),
		rec("risk", failing, "rate_again", "Rate: likelihood 2, impact 4\nIts control failed (control test "+otherTest+")."),
		rec("risk", failing, "rate_again", "Rate: likelihood 2, impact 4\nIts control failed 2.75 times (control test "+failedTest+")."),
		rec("risk", failing, "rate_again", "Rate: likelihood 2, impact 4\nPAY-01 failed its last test on 2026-11-15 (control test "+failedTest+")."),
		rec("risk", clear, "keep", "HR-01 passed its last test; the exception on it was overridden."),
		rec("risk", excepted, "rate_again", "Rate: likelihood 3, impact 5\nORG-01's rule link is broken (recommendation "+exception+")."),
		rec("tenant", tenant, "new_risk", "A new risk: payment runs released by organisation managers."),
		rec("tenant", tenant, "new_risk", "New risk: Payment runs released by organisation managers\nCategory: Payments\n"+
			"Nothing keeps whoever manages the organisation from releasing payment runs (recommendation "+exception+")."),
		llmtest.Final("Two risks to rate again, one new risk."))
	res, err := (&riskagent.Agent{LLM: model.Runner(), API: client(t, a)}).Review(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []recommend.Made{{ID: "r1", ObjectType: "risk", ObjectID: failing, Kind: "rate_again"}, {ID: "r2", ObjectType: "risk", ObjectID: clear, Kind: "keep"},
		{ID: "r3", ObjectType: "risk", ObjectID: excepted, Kind: "rate_again"}, {ID: "r4", ObjectType: "tenant", ObjectID: tenant, Kind: "new_risk"}}
	if !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 7 {
		t.Errorf("refused: got %d, want 7", res.Refused)
	}
	if len(a.created) != 4 {
		t.Errorf("recommendations sent: got %d, want 4: the check keeps the others from the API", len(a.created))
	}
	if got, want := fmt.Sprint(res.Names), fmt.Sprint(map[string]string{tenant: "the tenant", failing: "Duplicate payments to suppliers",
		clear: "Payroll errors", excepted: "Access misuse", bare: "Fraud", retiring: "Old process"}); got != want {
		t.Errorf("names: got %s, want %s", got, want)
	}
	for _, line := range []string{
		"Review the risk register: 5 open risk(s), as the register gives them today.",
		`Risk "Duplicate payments to suppliers" (id ` + failing + `), category "Payments": likelihood 1, impact 4, score 4. Control PAY-01 (id ` +
			payControl + `), active: last tested 2026-11-15, failed; no exception found.`,
		`Risk "Payroll errors" (id ` + clear + `), category "People": likelihood 2, impact 3, score 6. Control HR-01 (id ` + hrControl +
			`), active: last tested 2026-11-01, passed; an exception found, overridden by a person. Its controls all passed their last test and none has an exception: keep it.`,
		// An exception that stands makes the risk not clear: no "keep it".
		`Risk "Access misuse" (id ` + excepted + `), category "Access": likelihood 2, impact 5, score 10. Control ORG-01 (id ` + orgControl +
			"), active: last tested 2026-11-02, passed; an exception found.\n",
		`Risk "Old process" (id ` + retiring + `), category "Legacy": likelihood 1, impact 1, score 1. Control OLD-01 (id ` + oldControl +
			"), retired: never tested; not monitored.\n",
		`Risk "Fraud" (id ` + bare + `), category "Fraud": likelihood 1, impact 5, score 5. No control mitigates it.`,
		"A risk not yet in the register is recommended on the tenant (object_id " + tenant + ").",
	} {
		quoted, _ := json.Marshal(line) // the prompt reaches the model as JSON text
		if !strings.Contains(model.Requests[0], strings.Trim(string(quoted), `"`)) {
			t.Errorf("prompt: got %s, want the line %q", model.Requests[0], line)
		}
	}
	for _, refusal := range []error{riskagent.ErrRating, riskagent.ErrClear, riskagent.ErrNoEvidence, riskagent.ErrUncited, riskagent.ErrUnread, riskagent.ErrNewRisk} {
		refusedWith(t, model.Requests, refusal)
	}
}

// TestReviewRefusals: who the agent is, the register, the library and the
// recommendations the API refuses, more open risks than a run reviews and
// an API that is gone stop the run before a session starts.
func TestReviewRefusals(t *testing.T) {
	model := llmtest.New(t)
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	many := strings.Repeat(`{"id": "`+bare+`", "title": "X", "controls": []},`, riskagent.MaxRisks) + `{"id": "` + bare + `", "title": "X", "controls": []}`
	for name, c := range map[string]struct {
		api  *apitools.Client
		want string
	}{
		"who it is refused":           {client(t, &fakeAPI{refuse: "/v1/me"}), "riskagent: who the agent is: API 403"},
		"the register refused":        {client(t, &fakeAPI{refuse: "/v1/risks"}), "riskagent: the open risks: apitools: list /v1/risks: API 403"},
		"more than a run's":           {client(t, &fakeAPI{risks: many}), fmt.Sprintf("more than %d open risks to review", riskagent.MaxRisks)},
		"the library refused":         {client(t, &fakeAPI{refuse: "/v1/controls"}), "riskagent: the controls: apitools: list /v1/controls: API 403"},
		"the recommendations refused": {client(t, &fakeAPI{refuse: "/v1/recommendations"}), "riskagent: the control monitoring agent's findings: apitools: list /v1/recommendations: API 403"},
		"the API gone":                {gone, "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&riskagent.Agent{LLM: model.Runner(), API: c.api}).Review(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
			if res.Session != "" {
				t.Errorf("a session: got %s, want none", res.Session)
			}
		})
	}
}

// TestNever: the agent's tools are reads and the one recommendation; none
// is an operation of its never list, which holds every write of the risk
// register (ADR-0072, T105g).
func TestNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	tools := riskagent.Tools()
	for _, id := range tools {
		t.Run(id, func(t *testing.T) {
			op, ok := ops[id]
			if !ok {
				t.Fatalf("%s: not in the contract", id)
			}
			if op.Method != http.MethodGet && id != recommend.Proposer {
				t.Errorf("%s: got %s, want a read", id, op.Method)
			}
		})
	}
	for id, op := range ops {
		if op.Method != http.MethodGet && strings.Contains(id, "Risk") && !slices.Contains(riskagent.Never, id) {
			t.Errorf("never list: got %v, want it to hold %s", riskagent.Never, id)
		}
	}
	for _, id := range riskagent.Never {
		t.Run(id, func(t *testing.T) {
			if slices.Contains(tools, id) {
				t.Errorf("%s is among the agent's tools", id)
			}
			if _, ok := ops[id]; !ok {
				t.Errorf("%s: not in the contract", id)
			}
		})
	}
}
