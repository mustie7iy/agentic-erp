package supportagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/supportagent"
)

const (
	diagnosisID = "01a1aa00-0000-7000-8000-000000000d01"
	fixID       = "01a1aa00-0000-7000-8000-000000000e01"
)

// held is the diagnosis a user accepted: the invoice held by its match,
// event 41, under the company's price tolerance.
const held = "Root cause: configuration\nThe match held the invoice, event 41: its price is above `companies[1000].invoice_tolerance.price_percent` = `2`."

// fixAPI is a stand-in for the cell's API as a fix reads and writes it:
// the accepted diagnosis (or rec), the active configuration version 3, a
// session, the draft (created at version 1, each change one more), its
// simulation (running when first read, then succeeded and passed) and
// recommendations answered by their bodies; it keeps every call.
type fixAPI struct {
	fakeAPI
	rec   string // the diagnosis's answer; "": the accepted one
	gone  bool   // the diagnosis is not found
	mu    sync.Mutex
	log   []string
	reads int
	ver   int
	made  int
}

func (a *fixAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body) // a test's own request
	answer := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // a test's answer; a client gone has nothing to read
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.log = append(a.log, r.Method+" "+r.URL.Path)
	switch path := r.URL.Path; {
	case path == "/v1/recommendations/"+diagnosisID && a.gone:
		answer(404, `{"code": "request.not_found", "status": 404}`)
	case path == "/v1/recommendations/"+diagnosisID && a.rec != "":
		answer(200, a.rec)
	case path == "/v1/recommendations/"+diagnosisID:
		answer(200, diagnosisJSON("supplier_invoice", "root_cause", "accepted", held))
	case path == "/v1/change-requests" && r.Method == http.MethodPost:
		a.ver = 1
		answer(201, `{"id": "`+fixID+`", "version": 1, "status": "draft"}`)
	case path == "/v1/change-requests/"+fixID && r.Method == http.MethodPatch:
		a.ver++
		answer(200, fmt.Sprintf(`{"id": "%s", "version": %d, "status": "draft"}`, fixID, a.ver))
	case path == "/v1/change-requests/"+fixID+":simulate":
		answer(202, `{"id": "op1", "kind": "config.simulate", "status": "queued"}`)
	case path == "/v1/operations/op1":
		a.reads++
		if a.reads == 1 {
			answer(200, `{"id": "op1", "status": "running"}`)
			return
		}
		answer(200, fmt.Sprintf(`{"id": "op1", "status": "succeeded", "result": {"passed": true, "version": %d}}`, a.ver))
	case path == "/v1/recommendations":
		var body struct {
			ObjectID   string `json:"object_id"`
			ObjectType string `json:"object_type"`
			Kind       string
		}
		_ = json.Unmarshal(b, &body) // the agent's own JSON
		a.made++
		answer(201, fmt.Sprintf(`{"id": "r%d", "object_id": "%s", "object_type": "%s", "kind": "%s"}`, a.made, body.ObjectID, body.ObjectType, body.Kind))
	default:
		a.mu.Unlock()
		a.fakeAPI.ServeHTTP(w, &http.Request{Method: r.Method, URL: r.URL, Body: io.NopCloser(strings.NewReader(string(b)))})
		a.mu.Lock()
	}
}

// count is how many calls this stand-in answered whose method and path
// contain what.
func (a *fixAPI) count(what string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.log {
		if strings.Contains(c+"\x00", what) { // what ending in a NUL is the whole call
			n++
		}
	}
	return n
}

func diagnosisJSON(objectType, kind, outcome, content string) string {
	j, _ := json.Marshal(content) // a test's text
	return fmt.Sprintf(`{"id": "%s", "object_type": "%s", "object_id": "%s", "kind": "%s", "outcome": "%s", "content": %s}`,
		diagnosisID, objectType, invoice, kind, outcome, j)
}

func fixClient(t *testing.T, a *fixAPI) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL, Token: "t"}
}

// tolerated is the active document with company 1000's tolerance as
// percent and absolute.
func tolerated(percent int, absolute string) map[string]any {
	return map[string]any{"companies": []any{map[string]any{"code": "1000", "invoice_tolerance": map[string]any{"price_percent": percent, "price_absolute": absolute}}}}
}

func draftFix(base int, proposed map[string]any) llmtest.Step {
	return llmtest.Tool("createChangeRequest", map[string]any{"body": map[string]any{"title": "Price tolerance 10 percent", "base_version": base, "proposed": proposed}})
}

// onFix is a recommendation of kind with the content on the draft.
func onFix(kind, content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "change_request",
			"object_id": fixID, "kind": kind, "content": content}}
	})
}

const fixed = "Change: `companies[1000].invoice_tolerance.price_percent` from `2` to `10`\nThe match held the invoice, event 41; the simulation of the draft passed."

// TestFix: from the accepted diagnosis, the run drafts one change request
// on the active version changing one setting the diagnosis cites; a draft
// changing two settings, or one it does not cite, is refused. It
// recommends ready_to_submit once the simulation of the draft's last
// version passed, with the draft's change on its first line; a first line
// that is not the change, a setting cited with another value and a number
// no tool returned are refused. Nothing refused reaches the API.
func TestFix(t *testing.T) {
	a := &fixAPI{}
	model := llmtest.New(t, llmtest.Tool("getRecommendation", map[string]any{"id": diagnosisID}),
		draftFix(3, tolerated(10, "50.00")),
		draftFix(3, tolerated(2, "50.00")),
		draftFix(3, tolerated(10, "5.00")),
		onFix("ready_to_submit", fixed),
		llmtest.Tool("simulateChangeRequest", map[string]any{"id": fixID, "if_match": `"1"`}),
		onFix("ready_to_submit", "Change: `companies[1000].invoice_tolerance.price_percent` from `2` to `12`\nIt passed."),
		onFix("ready_to_submit", fixed+" Under `companies[1000].invoice_tolerance.price_absolute` = `7.00`."),
		onFix("ready_to_submit", fixed+" It saves 300 a month."),
		onFix("simulation_failed", fixed),
		onFix("ready_to_submit", fixed+" `companies[1000].invoice_tolerance.price_absolute` = `5.00` stays."),
		llmtest.Final("Drafted the fix and simulated it; a person submits it."))
	res, err := (&supportagent.Agent{LLM: model.Runner(), API: fixClient(t, a), Poll: time.Millisecond}).Fix(context.Background(), diagnosisID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []recommend.Made{{ID: "r1", ObjectType: "change_request", ObjectID: fixID, Kind: "ready_to_submit"}}; !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 5 {
		t.Errorf("refused: got %d, want 5", res.Refused)
	}
	for call, want := range map[string]int{"POST /v1/change-requests\x00": 1, ":simulate": 1, "POST /v1/recommendations\x00": 1} {
		if got := a.count(call); got != want {
			t.Errorf("%s: got %d calls, want %d", call, got, want)
		}
	}
	told(t, model.Requests[:1], "The accepted diagnosis "+diagnosisID+" cites the settings companies[1000].invoice_tolerance.price_percent:\n<diagnosis>\n"+held+"\n</diagnosis>",
		"you may recommend on object_type change_request")
	told(t, model.Requests, changedraft.ErrOutside.Error(), "not companies[1000].invoice_tolerance.price_absolute, companies[1000].invoice_tolerance.price_percent",
		"not companies[1000].invoice_tolerance.price_absolute", changedraft.ErrNotSimulated.Error(), supportagent.ErrChange.Error(),
		"Change: `companies[1000].invoice_tolerance.price_percent` from `2` to `10`", supportagent.ErrSetting.Error(), "not 300")
}

// TestFixRefusals: a diagnosis that is open, of another kind, on an object
// that is no document, of a class other than configuration, citing no
// setting or one the active version no longer holds with that value, and a
// diagnosis or version the agent may not read, stop the run before a
// session starts.
func TestFixRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		api  *fixAPI
		want string
	}{
		"open":               {&fixAPI{rec: diagnosisJSON("supplier_invoice", "root_cause", "", held)}, `outcome ""`},
		"overridden":         {&fixAPI{rec: diagnosisJSON("supplier_invoice", "root_cause", "overridden", held)}, `outcome "overridden"`},
		"another kind":       {&fixAPI{rec: diagnosisJSON("supplier_invoice", "release_hold", "accepted", held)}, "release_hold on supplier_invoice"},
		"no document":        {&fixAPI{rec: diagnosisJSON("config_version", "root_cause", "accepted", held)}, "root_cause on config_version"},
		"another class":      {&fixAPI{rec: diagnosisJSON("supplier_invoice", "root_cause", "accepted", "Root cause: closed_period\nevent 41")}, `class "closed_period"`},
		"no setting cited":   {&fixAPI{rec: diagnosisJSON("supplier_invoice", "root_cause", "accepted", "Root cause: configuration\nevent 41")}, "citing 0 settings"},
		"a stale value":      {&fixAPI{rec: diagnosisJSON("supplier_invoice", "root_cause", "accepted", strings.Replace(held, "`2`", "`3`", 1))}, "version 3 is active"},
		"a path not held":    {&fixAPI{rec: diagnosisJSON("supplier_invoice", "root_cause", "accepted", strings.Replace(held, "1000", "2000", 1))}, supportagent.ErrFixStale.Error()},
		"a diagnosis gone":   {&fixAPI{gone: true}, "request.not_found"},
		"a version refused":  {&fixAPI{fakeAPI: fakeAPI{status: 403}}, "identity.permission_missing"},
		"the class's period": {&fixAPI{rec: diagnosisJSON("supplier_invoice", "root_cause", "accepted", "Root cause: configuration.\n"+held[26:])}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&supportagent.Agent{LLM: llmtest.New(t, llmtest.Final("Nothing drafted.")).Runner(), API: fixClient(t, c.api)}).Fix(context.Background(), diagnosisID)
			if c.want == "" {
				if err != nil {
					t.Fatalf("a class ending in a full stop: got %v, want no error", err)
				}
				if res.Session == "" {
					t.Error("a class ending in a full stop: got no session, want one")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error saying %q", err, c.want)
			}
			if n := c.api.count("agent-sessions"); n != 0 {
				t.Errorf("sessions: got %d, want none", n)
			}
		})
	}
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	if _, err := (&supportagent.Agent{LLM: llmtest.New(t).Runner(), API: gone}).Fix(context.Background(), diagnosisID); err == nil {
		t.Error("the API gone: got no error")
	}
}

// TestFixTools: a fix's tools are its reads, the draft's three writes and
// the one that recommends; a drafting's never list (submitting,
// withdrawing or activating a change request, a decision on an approval
// or a recommendation) is in the contract and none of it is a tool, and
// no write of master data or a document is one either.
func TestFixTools(t *testing.T) {
	ops := api.MustOperations()
	tools := supportagent.FixTools()
	for _, id := range tools {
		op, ok := ops[id]
		if read := ok && op.Method == http.MethodGet; !read && !slices.Contains(changedraft.Writes, id) && id != recommend.Proposer {
			t.Errorf("%s: got in the contract %v, method %s; want a read, a draft's write or the proposer", id, ok, op.Method)
		}
	}
	for _, id := range changedraft.Never {
		if _, ok := ops[id]; !ok || slices.Contains(tools, id) {
			t.Errorf("%s: got in the contract %v, among the tools %v; want in the contract and no tool", id, ok, slices.Contains(tools, id))
		}
	}
}
