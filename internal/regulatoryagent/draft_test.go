package regulatoryagent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/changedraft"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
	"github.com/mustie7iy/agentic-erp/internal/regulatoryagent"
)

const (
	accepted = "01a1aa00-0000-7000-8000-000000000d01"
	draftID  = "01a1aa00-0000-7000-8000-000000000e01"
)

// acceptedContent is the recommendation T107a left, which a tax
// accountant accepted.
const acceptedContent = paths + effect + quote + caveat

// draftAPI is a stand-in for the cell's API for a drafting run: the
// recommendation, active configuration version 3, a session, the draft
// (created at version 1, each change one more), its simulations (running
// when first read, then finished as finish says) and recommendations
// answered by their bodies; it keeps every request it was sent.
type draftAPI struct {
	fakeAPI
	refuse bool                // a simulation is refused
	gone   bool                // the recommendation is not found
	rec    string              // the recommendation's answer; "": the accepted one
	finish func(n int) string  // the nth simulation's operation once finished; nil: succeeded and passed
	mu     sync.Mutex          // guards what follows
	reads  map[string]int      // reads of each operation
	sims   int                 // simulations started
	ver    int                 // the draft's version
	sent   []string            // calls this stand-in answered
	bodies map[string][]string // bodies by call
}

func (a *draftAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body) // a test's own request
	answer := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // a test's answer; a client gone has nothing to read
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	a.sent = append(a.sent, call)
	if a.bodies == nil {
		a.bodies, a.reads = map[string][]string{}, map[string]int{}
	}
	a.bodies[call] = append(a.bodies[call], string(b))
	switch path := r.URL.Path; {
	case path == "/v1/recommendations/"+accepted && a.gone:
		answer(404, `{"code": "request.not_found", "status": 404}`)
	case path == "/v1/recommendations/"+accepted && a.rec != "":
		answer(200, a.rec)
	case path == "/v1/recommendations/"+accepted:
		j, _ := json.Marshal(acceptedContent) // a constant
		answer(200, `{"id": "`+accepted+`", "object_type": "config_version", "object_id": "`+version+`", "kind": "change_needed", "outcome": "accepted", "content": `+string(j)+`}`)
	case path == "/v1/change-requests" && r.Method == http.MethodPost:
		a.ver = 1
		answer(201, `{"id": "`+draftID+`", "version": 1, "status": "draft"}`)
	case path == "/v1/change-requests/"+draftID && r.Method == http.MethodPatch:
		a.ver++
		answer(200, fmt.Sprintf(`{"id": "%s", "version": %d, "status": "draft"}`, draftID, a.ver))
	case path == "/v1/change-requests/"+draftID+":simulate" && a.refuse:
		answer(409, `{"code": "config.invalid_status", "status": 409}`)
	case path == "/v1/change-requests/"+draftID+":simulate":
		a.sims++
		answer(202, fmt.Sprintf(`{"id": "op%d", "kind": "config.simulate", "status": "queued"}`, a.sims))
	case strings.HasPrefix(path, "/v1/operations/op"):
		a.reads[path]++
		var n int
		_, _ = fmt.Sscanf(path, "/v1/operations/op%d", &n) // the stand-in's own ids
		switch {
		case a.reads[path] == 1:
			answer(200, `{"id": "op", "status": "running"}`)
		case a.finish != nil:
			answer(200, a.finish(n))
		default:
			answer(200, fmt.Sprintf(`{"id": "op", "status": "succeeded", "result": {"passed": true, "version": %d}}`, n))
		}
	default:
		a.mu.Unlock()
		a.fakeAPI.ServeHTTP(w, &http.Request{Method: r.Method, URL: r.URL, Body: io.NopCloser(strings.NewReader(string(b)))})
		a.mu.Lock()
	}
}

// count is how many calls this stand-in answered whose method and path
// contain what.
func (a *draftAPI) count(what string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.sent {
		if strings.Contains(c+"\x00", what) { // what ending in a NUL is the whole call
			n++
		}
	}
	return n
}

func draftClient(t *testing.T, a *draftAPI) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL, Token: "t"}
}

// proposal is the active document with V0's rate, or what change says,
// changed.
func proposal(change func(map[string]any)) map[string]any {
	var doc map[string]any
	_ = json.Unmarshal([]byte(document), &doc) // a constant
	if change != nil {
		change(doc)
	}
	return doc
}

func v0(rate string) func(map[string]any) {
	return func(doc map[string]any) {
		co := doc["companies"].([]any)[0].(map[string]any)
		co["tax_codes"].([]any)[0].(map[string]any)["rate_percent"] = rate
	}
}

func create(base int, proposed map[string]any) llmtest.Step {
	return llmtest.Tool("createChangeRequest", map[string]any{"body": map[string]any{"title": "V0 at 2.5 percent", "base_version": base, "proposed": proposed}})
}

func update(id string, body map[string]any) llmtest.Step {
	return llmtest.Tool("updateChangeRequest", map[string]any{"id": id, "if_match": `"1"`, "body": body})
}

func simulate(id string) llmtest.Step {
	return llmtest.Tool("simulateChangeRequest", map[string]any{"id": id, "if_match": `"1"`})
}

// on is a recommendation of kind with the content on the draft.
func on(kind, content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "change_request",
			"object_id": draftID, "kind": kind, "content": content}}
	})
}

const ready = "Drafted V0's rate as the notice says: \"rises from 0 percent to 2.5 percent\"; the simulation passed. " + caveat

// TestDraft: from the accepted recommendation, the run drafts one change
// request based on the active version, changing only the paths the
// recommendation named; it changes and simulates only that draft, and it
// recommends ready_to_submit only once the simulation of the draft's last
// version passed, quoting only what the accepted recommendation quotes,
// with the caveat and no number it did not read. Nothing refused reaches
// the API.
func TestDraft(t *testing.T) {
	a := &draftAPI{}
	outside := proposal(func(doc map[string]any) {
		doc["companies"].([]any)[0].(map[string]any)["tax_registrations"] = []any{map[string]any{"code": "USSTATE"}}
	})
	model := llmtest.New(t, versionRead,
		create(2, proposal(v0("2.5"))),
		create(3, outside),
		create(3, proposal(nil)),
		create(3, map[string]any{"companies": "none"}),
		update(draftID, map[string]any{"title": "early"}),
		create(3, proposal(v0("2.5"))),
		create(3, proposal(v0("2.5"))),
		on("ready_to_submit", ready),
		simulate(version),
		simulate(draftID),
		update(draftID, map[string]any{"proposed": outside}),
		update(draftID, map[string]any{"base_version": 2}),
		update(draftID, map[string]any{"title": "V0 at 2.5 percent from 1 July 2099"}),
		on("ready_to_submit", ready),
		simulate(draftID),
		on("simulation_failed", "It failed. "+caveat),
		on("ready_to_submit", "The notice says \"rises to 2.5 percent\". "+caveat),
		on("ready_to_submit", "Drafted from \"rises from 0 percent to 2.5 percent\"."),
		on("ready_to_submit", "Input tax rises by 2.5 percent. "+caveat),
		on("ready_to_submit", ready),
		llmtest.Final("Drafted the change request and simulated it; a person submits it."))
	res, err := (&regulatoryagent.Agent{LLM: model.Runner(), API: draftClient(t, a), Poll: time.Millisecond}).Draft(context.Background(), accepted)
	if err != nil {
		t.Fatal(err)
	}
	if want := []recommend.Made{{ID: "r1", ObjectType: "change_request", ObjectID: draftID, Kind: "ready_to_submit"}}; !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 6 {
		t.Errorf("refused: got %d, want 6", res.Refused)
	}
	for call, want := range map[string]int{"POST /v1/change-requests\x00": 1, "PATCH /v1/change-requests/": 1, ":simulate": 2} {
		if got := a.count(call); got != want {
			t.Errorf("%s: got %d calls, want %d", call, got, want)
		}
	}
	told(t, model.Requests[:1], "The accepted recommendation "+accepted+":\n<recommendation>\n"+acceptedContent+"\n</recommendation>",
		"you may recommend on object_type change_request")
	told(t, model.Requests, changedraft.ErrBase.Error(), changedraft.ErrOutside.Error(), "not companies[1000].tax_registrations",
		"it changes nothing", "the proposal is no configuration", changedraft.ErrNotTheDraft.Error(), changedraft.ErrNotSimulated.Error(),
		regulatoryagent.ErrNotQuoted.Error(), regulatoryagent.ErrNoCaveat.Error(), regulatoryagent.ErrUnread.Error())
}

// TestDraftSimulationFailed: a simulation that failed, or passed nothing,
// admits simulation_failed and refuses ready_to_submit; one the API
// refused, or that does not finish within the agent's patience, admits
// neither.
func TestDraftSimulationFailed(t *testing.T) {
	for name, c := range map[string]struct {
		finish func(int) string
		want   []recommend.Made
		refuse bool
	}{
		"refused": {nil, nil, true},
		"failed": {func(int) string {
			return `{"id": "op", "status": "failed", "error": {"code": "config.validation_failed"}}`
		}, failed, false},
		"not passed": {func(int) string {
			return `{"id": "op", "status": "succeeded", "result": {"passed": false, "version": 1}}`
		}, failed, false},
		"unfinished": {func(int) string { return `{"id": "op", "status": "running"}` }, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			a := &draftAPI{finish: c.finish, refuse: c.refuse}
			model := llmtest.New(t, create(3, proposal(v0("2.5"))), simulate(draftID),
				on("ready_to_submit", ready), on("simulation_failed", "The simulation failed. "+caveat), llmtest.Final("Done."))
			agent := &regulatoryagent.Agent{LLM: model.Runner(), API: draftClient(t, a), Poll: time.Millisecond, Patience: 50 * time.Millisecond}
			res, err := agent.Draft(context.Background(), accepted)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(res.Recommendations, c.want) {
				t.Errorf("recommendations: got %v, want %v", res.Recommendations, c.want)
			}
		})
	}
}

var failed = []recommend.Made{{ID: "r1", ObjectType: "change_request", ObjectID: draftID, Kind: "simulation_failed"}}

// TestDraftRefusals: a recommendation that is open, overridden, of
// another kind or object, made on a version no longer active, or whose
// paths the active version does not hold, and a recommendation or
// version the agent may not read, stop the run before a session starts.
func TestDraftRefusals(t *testing.T) {
	rec := func(kind, object, outcome, content string) string {
		j, _ := json.Marshal(content) // a test's text
		return fmt.Sprintf(`{"id": "%s", "object_type": "%s", "object_id": "%s", "kind": "%s", "outcome": "%s", "content": %s}`,
			accepted, object, version, kind, outcome, j)
	}
	for name, c := range map[string]struct {
		api  *draftAPI
		want string
	}{
		"open":                  {&draftAPI{rec: rec("change_needed", "config_version", "", acceptedContent)}, "change_needed on config_version, open"},
		"overridden":            {&draftAPI{rec: rec("change_needed", "config_version", "overridden", acceptedContent)}, "overridden"},
		"no change":             {&draftAPI{rec: rec("no_change", "config_version", "accepted", acceptedContent)}, "no_change on config_version"},
		"another object":        {&draftAPI{rec: rec("change_needed", "rfq", "accepted", acceptedContent)}, "change_needed on rfq"},
		"a path not held":       {&draftAPI{rec: rec("change_needed", "config_version", "accepted", "Paths: companies[1000].tax_codes[V9]\n")}, "are not all the active version's"},
		"no paths":              {&draftAPI{rec: rec("change_needed", "config_version", "accepted", quote)}, "are not all the active version's"},
		"an older version":      {&draftAPI{rec: strings.Replace(rec("change_needed", "config_version", "accepted", acceptedContent), version, accepted, 1)}, "version 3 is active"},
		"a recommendation gone": {&draftAPI{gone: true}, "request.not_found"},
		"a version refused":     {&draftAPI{fakeAPI: fakeAPI{status: 403}}, "identity.permission_missing"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (&regulatoryagent.Agent{LLM: llmtest.New(t).Runner(), API: draftClient(t, c.api)}).Draft(context.Background(), accepted)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error saying %q", err, c.want)
			}
			if n := c.api.count("agent-sessions"); n != 0 {
				t.Errorf("sessions: got %d, want none", n)
			}
		})
	}
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	if _, err := (&regulatoryagent.Agent{LLM: llmtest.New(t).Runner(), API: gone}).Draft(context.Background(), accepted); err == nil {
		t.Error("the API gone: got no error")
	}
}

// TestDraftNever: the drafting's tools are its reads, the three writes of
// a draft and the one that recommends; every other write of the
// configuration, a decision on its approval and on a recommendation are
// on its never list, and none of them is a tool.
func TestDraftNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	tools := regulatoryagent.DraftTools()
	for _, id := range tools {
		op, ok := ops[id]
		read := ok && op.Method == http.MethodGet
		if !read && !slices.Contains(changedraft.Writes, id) && id != recommend.Proposer {
			t.Errorf("%s: got in the contract %v, method %s; want a read, a draft's write or the proposer", id, ok, op.Method)
		}
	}
	for id, op := range ops {
		if op.Method != http.MethodGet && strings.HasPrefix(op.Permission, "config.") && !slices.Contains(changedraft.Writes, id) &&
			!slices.Contains(changedraft.Never, id) {
			t.Errorf("never list: got %v, want it to hold %s", changedraft.Never, id)
		}
	}
	for _, id := range changedraft.Never {
		if _, ok := ops[id]; !ok || slices.Contains(tools, id) {
			t.Errorf("%s: got in the contract %v, among the tools %v; want in the contract and no tool", id, ok, slices.Contains(tools, id))
		}
	}
}
