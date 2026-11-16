package controlagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/controlagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

const (
	session  = "01a1aa00-0000-7000-8000-000000000a01"
	broken   = "01a1aa00-0000-7000-8000-000000000b01" // ORG-01: a rule link broken, a permission link holding
	holding  = "01a1aa00-0000-7000-8000-000000000b02" // PAY-01: its one link holds
	unlinked = "01a1aa00-0000-7000-8000-000000000b03" // REC-01: no link
	ruleLink = "01a1aa00-0000-7000-8000-000000000c01"
	permLink = "01a1aa00-0000-7000-8000-000000000c02"
	payLink  = "01a1aa00-0000-7000-8000-000000000c03"
	rule     = "01a1aa00-0000-7000-8000-000000000d01"
)

// rows are the matrix's controls: one with a broken link beside one that
// holds, one whose link holds and one without links.
const rows = `{"control": {"id": "` + broken + `", "code": "ORG-01", "objective": "Who manages the organisation does not release payment runs.", "process": "Access", "nature": "automated", "frequency": "continuous"},
	"link_state": "broken", "links": [{"id": "` + ruleLink + `", "kind": "sod_rule", "sod_rule_id": "` + rule + `", "holds": false,
	"broken_reason": "sod_rule_not_started"}, {"id": "` + permLink + `", "kind": "permission", "permission_code": "tenancy.org.manage", "holds": true}]},
	{"control": {"id": "` + holding + `", "code": "PAY-01", "process": "Payments", "nature": "automated", "frequency": "continuous",
	"last_test": {"result": "passed", "tested_on": "2026-11-13"}}, "link_state": "holding",
	"links": [{"id": "` + payLink + `", "kind": "configuration_path", "configuration_path": "companies[3000].release_dual_limit", "holds": true}]},
	{"control": {"id": "` + unlinked + `", "code": "REC-01", "process": "Reconciliation", "nature": "manual", "frequency": "monthly"},
	"link_state": "none", "links": []}`

// fakeAPI is a stand-in for the cell's API: the control matrix, the
// audit events of the rule, a session, and recommendations answered by
// their bodies; it keeps every request it was sent.
type fakeAPI struct {
	mu     sync.Mutex
	calls  []string
	rows   string // the matrix's rows; "": rows
	matrix int    // the matrix's status; 0: 200
	made   int
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
	switch r.URL.Path {
	case "/v1/control-matrix":
		a.answerMatrix(answer)
	case "/v1/audit-events":
		answer(200, `{"items": [{"seq": 41, "action": "identity.sod_rule.create", "object_type": "identity.sod_rule", "object_id": "`+rule+`"},
			{"seq": 42, "action": "identity.sod_rule.end", "object_type": "identity.sod_rule", "object_id": "`+rule+`"}]}`)
	case "/v1/agent-sessions":
		answer(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case "/v1/recommendations":
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
	default:
		answer(200, `{}`)
	}
}

func (a *fakeAPI) answerMatrix(answer func(int, string)) {
	switch {
	case a.matrix != 0:
		answer(a.matrix, `{"code": "identity.permission_missing", "status": 403}`)
	case a.rows == "":
		answer(200, `{"as_of": "2026-11-15", "items": [`+rows+`]}`)
	case a.rows == "none":
		answer(200, `{"as_of": "2026-11-15", "items": []}`)
	default:
		answer(200, `{"as_of": "2026-11-15", "items": [`+a.rows+`]}`)
	}
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

// rec is a recommendation of kind with the content on the control, in
// the session the conversation names.
func rec(control, kind, content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "control",
			"object_id": control, "kind": kind, "content": content}}
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

// TestMonitor: the agent monitors the matrix's three controls in one
// session and recommends once on each. Its check refuses, and counts, no
// exception on a control with a broken link, an exception naming other
// links than the broken ones, an audit event no read returned, a number
// no read returned, an exception on a control whose links hold without
// an event, and no exception on a control without a link that does not
// say nothing could be checked; nothing refused reaches the API.
func TestMonitor(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t,
		llmtest.Tool("listAuditEvents", map[string]any{"object_type": "identity.sod_rule", "object_id": rule}),
		rec(broken, "no_exception", "The permission link holds."),
		rec(broken, "exception_found", "Broken links: "+permLink+"\nThe permission."),
		rec(broken, "exception_found", "Broken links: "+ruleLink+"\nThe rule ended: audit event 13, a number read as a day, not an event."),
		rec(broken, "exception_found", "Broken links: "+ruleLink+"\nThe rule is 2.50 days from starting."),
		rec(broken, "exception_found", "Broken links: "+ruleLink+"\nThe rule is not in force: it was made to start later (audit event 41) and ended before it could hold (Audit Event 42)."),
		rec(holding, "exception_found", "The release limit looks high."),
		rec(holding, "no_exception", "Its link holds and nothing read shows an exception."),
		rec(unlinked, "no_exception", "No exception."),
		rec(unlinked, "no_exception", "No link. "+controlagent.NothingChecked),
		llmtest.Final("One exception: ORG-01's rule link is broken."))
	res, err := (&controlagent.Agent{LLM: model.Runner(), API: client(t, a)}).Monitor(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []recommend.Made{{ID: "r1", ObjectType: "control", ObjectID: broken, Kind: "exception_found"},
		{ID: "r2", ObjectType: "control", ObjectID: holding, Kind: "no_exception"}, {ID: "r3", ObjectType: "control", ObjectID: unlinked, Kind: "no_exception"}}
	if !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 6 {
		t.Errorf("refused: got %d, want 6", res.Refused)
	}
	if got := len(a.sent("recommendations")); got != 3 {
		t.Errorf("recommendations sent: got %d, want 3: the check keeps the others from the API", got)
	}
	if got, want := res.Names, map[string]string{broken: "ORG-01", holding: "PAY-01", unlinked: "REC-01"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("names: got %v, want %v", got, want)
	}
	for _, line := range []string{
		"Monitor the tenant's active controls, 3 as the control matrix gives them today:",
		`Control ORG-01 (id ` + broken + `), objective "Who manages the organisation does not release payment runs.", process "Access", automated, continuous, never tested. Link ` + ruleLink + ` to the sod_rule ` + rule +
			`: broken: sod_rule_not_started. Link ` + permLink + ` to the permission tenancy.org.manage: holds.`,
		`Control PAY-01 (id ` + holding + `), objective "", process "Payments", automated, continuous, last tested 2026-11-13, passed. Link ` + payLink +
			` to the configuration_path companies[3000].release_dual_limit: holds.`,
		`Control REC-01 (id ` + unlinked + `), objective "", process "Reconciliation", manual, monthly, never tested. No link.`,
	} {
		quoted, _ := json.Marshal(line) // the prompt reaches the model as JSON text
		if !strings.Contains(model.Requests[0], strings.Trim(string(quoted), `"`)) {
			t.Errorf("prompt: got %s, want the line %q", model.Requests[0], line)
		}
	}
	for _, refusal := range []error{controlagent.ErrBroken, controlagent.ErrUncited, controlagent.ErrUnread, controlagent.ErrNoEvidence, controlagent.ErrNothingChecked} {
		refusedWith(t, model.Requests, refusal)
	}
	if got := a.sent("control-matrix"); !slices.Equal(got, []string{"GET /v1/control-matrix?page_size=500"}) {
		t.Errorf("matrix read: got %v, want one page of the whole matrix", got)
	}
}

// TestMonitorProcess: a run of one process reads the matrix of that
// process, and one with no control to monitor starts no session and says
// so.
func TestMonitorProcess(t *testing.T) {
	a := &fakeAPI{rows: "none"}
	model := llmtest.New(t)
	res, err := (&controlagent.Agent{LLM: model.Runner(), API: client(t, a)}).Monitor(context.Background(), "Access management")
	if err != nil {
		t.Fatal(err)
	}
	if want := `No control to monitor: the control matrix has none of the active controls of the process "Access management".`; res.Reply != want || res.Session != "" {
		t.Errorf("result: got %+v, want no session and the reply %q", res, want)
	}
	if got := a.sent("control-matrix"); !slices.Equal(got, []string{"GET /v1/control-matrix?page_size=500&process=Access+management"}) {
		t.Errorf("matrix read: got %v, want the process's", got)
	}
	if got := len(a.sent("agent-sessions")); got != 0 {
		t.Errorf("sessions: got %d, want none", got)
	}
}

// TestMonitorRefusals: a matrix the API refuses, more controls than a run
// monitors, and an API that is gone stop the run before a session starts.
func TestMonitorRefusals(t *testing.T) {
	model := llmtest.New(t)
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	many := strings.Repeat(`{"control": {"id": "`+holding+`", "code": "X"}, "links": []},`, controlagent.MaxControls) +
		`{"control": {"id": "` + holding + `", "code": "X"}, "links": []}`
	for name, c := range map[string]struct {
		api  *apitools.Client
		want string
	}{
		"the matrix refused": {client(t, &fakeAPI{matrix: 403}), "controlagent: the control matrix: apitools: list /v1/control-matrix: API 403 identity.permission_missing"},
		"more than a run's":  {client(t, &fakeAPI{rows: many}), fmt.Sprintf("more than %d controls to monitor: name a process", controlagent.MaxControls)},
		"the API gone":       {gone, "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&controlagent.Agent{LLM: model.Runner(), API: c.api}).Monitor(context.Background(), "")
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
// is an operation of its never list, which holds every write of the
// control library, and the contract closes recording a test to agents
// (ADR-0072, T105f).
func TestNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	tools := controlagent.Tools()
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
		if op.Method != http.MethodGet && strings.Contains(id, "Control") && !slices.Contains(controlagent.Never, id) {
			t.Errorf("never list: got %v, want it to hold %s", controlagent.Never, id)
		}
	}
	for _, id := range controlagent.Never {
		t.Run(id, func(t *testing.T) {
			if slices.Contains(tools, id) {
				t.Errorf("%s is among the agent's tools", id)
			}
			if _, ok := ops[id]; !ok {
				t.Errorf("%s: not in the contract", id)
			}
		})
	}
	if ops["createControlTest"].AgentAllowed {
		t.Error("createControlTest: open to agents, want x-agent: false")
	}
}
