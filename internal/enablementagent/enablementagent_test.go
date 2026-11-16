package enablementagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/enablementagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

const (
	session = "01a1aa00-0000-7000-8000-000000000a01"
	version = "01a1aa00-0000-7000-8000-000000000b01"
)

// document is the active version's configuration: company 1000's invoice
// tolerance, two requisition tiers and the payment release.
const document = `{"companies": [{"code": "1000", "invoice_tolerance": {"price_percent": 2, "price_absolute": "5.00"}}],
	"purchasing": {"requisition_approval": [{"up_to": "10000.00", "steps": [{"role": "Z_PURCH_MGR", "approvals": 1}]},
		{"steps": [{"role": "Z_PURCH_MGR", "approvals": 1}, {"role": "process_owner", "approvals": 1}]}]},
	"payments": {"release_role": "Z_TREASURY_MGR", "release_approvals": 1}}`

// roles are the tenant's roles: two active, one inactive.
const roles = `{"items": [{"code": "Z_PURCH_MGR", "status": "active"}, {"code": "Z_TREASURY_MGR", "status": "active"},
	{"code": "OLD_BUYER", "status": "inactive"}]}`

// fakeAPI is a stand-in for the cell's API: the active configuration
// version 3, the roles, a session, and recommendations answered by their
// bodies with their content; it keeps every request it was sent.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []string
	status   int // the active version's status; 0: 200
	roles    int // the roles' status; 0: 200
	sessions int // the session's status; 0: 201
	made     int
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
	refused := `{"code": "identity.permission_missing", "status": 403}`
	v := `{"id": "` + version + `", "number": 3, "is_active": true, "document": ` + document + `}`
	switch path := r.URL.Path; {
	case path == "/v1/config-versions" && a.status != 0:
		answer(a.status, refused)
	case path == "/v1/config-versions":
		answer(200, `{"items": [`+v+`]}`)
	case path == "/v1/config-versions/"+version:
		answer(200, v)
	case path == "/v1/roles" && a.roles != 0:
		answer(a.roles, refused)
	case path == "/v1/roles":
		answer(200, roles)
	case path == "/v1/agent-sessions" && a.sessions != 0:
		answer(a.sessions, refused)
	case path == "/v1/agent-sessions":
		answer(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case path == "/v1/recommendations":
		a.mu.Lock()
		a.made++
		id := fmt.Sprintf("r%d", a.made)
		a.mu.Unlock()
		var made map[string]any
		_ = json.Unmarshal(b, &made) // the agent's own JSON
		made["id"] = id
		out, _ := json.Marshal(made) // decoded from JSON, so it encodes again
		answer(201, string(out))
	default:
		answer(200, `{}`)
	}
}

// sent is how many calls the API was sent whose method and path contain
// what.
func (a *fakeAPI) sent(what string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		if strings.Contains(c, what) {
			n++
		}
	}
	return n
}

func client(t *testing.T, a *fakeAPI) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL, Token: "t"}
}

var sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)

// rec is a process description with the content on the version, in the
// session the conversation names.
func rec(content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "config_version",
			"object_id": version, "kind": "process_document", "content": content}}
	})
}

// told reports whether the model was told each text, which it reads as
// JSON text: a refusal, or its prompt.
func told(t *testing.T, requests []string, want ...string) {
	t.Helper()
	all := strings.Join(requests, "\n")
	for _, w := range want {
		j, _ := json.Marshal(w) // the model reads the refusal as JSON text
		if !strings.Contains(all, strings.Trim(string(j), `"`)) {
			t.Errorf("the model was not told %q", w)
		}
	}
}

const (
	heading   = "# Purchase to pay, configuration version 3\n"
	limit     = "`purchasing.requisition_approval[0].up_to` = `10000.00`"
	tolerance = "`companies[1000].invoice_tolerance.price_percent` = `2`"
	body      = "## Requisitions\n1. A requisition up to " + limit + " is approved by role `Z_PURCH_MGR`.\n" +
		"2. Above it role `Z_PURCH_MGR`, then `purchasing.requisition_approval[1].steps[1].role` = `process_owner`, approve.\n" +
		"## Invoices\nAn invoice whose price is above " + tolerance + " percent of the order's is held; role `Z_TREASURY_MGR` releases payments."
)

// TestDocument: the agent describes purchase to pay from active
// configuration version 3 in one session and recommends the description
// on the version once. Its check refuses, and counts, a first line that
// is not the heading, a description citing no setting, a setting cited
// with another value or at a path the version does not hold, a role the
// tenant does not have or whose role is inactive, and a number outside
// the citations; a list's numbering is allowed. The description is
// recorded as written.
func TestDocument(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t, llmtest.Tool("getConfigVersion", map[string]any{"id": version}),
		llmtest.Tool("listRoles", map[string]any{}),
		rec("# Purchase to pay\n"+body),
		rec(heading+"Requisitions are approved by role `Z_PURCH_MGR`."),
		rec(heading+"Held above `companies[1000].invoice_tolerance.price_percent` = `3` percent."),
		rec(heading+"Held above `companies[1000].tolerance` = `2` percent."),
		rec(heading+body+" Role `Z_NOBODY` approves."),
		rec(heading+body+" Role `OLD_BUYER` approves."),
		rec(heading+body+" Payments wait 5 days."),
		rec(heading+body),
		llmtest.Final("Described purchase to pay."))
	res, err := (&enablementagent.Agent{LLM: model.Runner(), API: client(t, a)}).Document(context.Background(), "purchase_to_pay")
	if err != nil {
		t.Fatal(err)
	}
	want := []recommend.Made{{ID: "r1", ObjectType: "config_version", ObjectID: version, Kind: "process_document", Content: heading + body}}
	if !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 7 {
		t.Errorf("refused: got %d, want 7", res.Refused)
	}
	told(t, model.Requests[:1], "The tenant's active configuration is version 3 (id "+version+").", "The description's first line is:\n"+strings.TrimSuffix(heading, "\n"),
		"Describe the process purchase to pay. It rests on the approval of requisitions")
	told(t, model.Requests, enablementagent.ErrHeading.Error(), enablementagent.ErrUncited.Error(), enablementagent.ErrSetting.Error(),
		"not companies[1000].invoice_tolerance.price_percent = 3", "not companies[1000].tolerance = 2", enablementagent.ErrRole.Error(),
		"not Z_NOBODY", "not OLD_BUYER", enablementagent.ErrNumber.Error(), "not 5")
}

// TestDocumentRefusals: a process the first version does not know stops
// the run before anything is read; an active version or roles the agent
// may not read, and an API that is gone, stop it before a session starts.
func TestDocumentRefusals(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	unknown := &fakeAPI{}
	for name, c := range map[string]struct {
		api     *apitools.Client
		process string
		want    string
	}{
		"a process unknown": {client(t, unknown), "record_to_report", `a process is one of order_to_cash, purchase_to_pay, not "record_to_report"`},
		"a version refused": {client(t, &fakeAPI{status: 403}), "order_to_cash", "identity.permission_missing"},
		"roles refused":     {client(t, &fakeAPI{roles: 403}), "order_to_cash", "enablementagent: the roles: apitools: list /v1/roles: API 403 identity.permission_missing"},
		"the API gone":      {gone, "order_to_cash", "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&enablementagent.Agent{LLM: llmtest.New(t).Runner(), API: c.api}).Document(context.Background(), c.process)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error saying %q", err, c.want)
			}
			if res.Session != "" {
				t.Errorf("session: got %s, want none", res.Session)
			}
		})
	}
	if n := unknown.sent("/v1/"); n != 0 {
		t.Errorf("an unknown process: got %d calls, want none", n)
	}
}

// TestNever: the agent's tools are reads and the one recommendation; none
// is a write of the configuration or of access, every one of which is on
// its never list (ADR-0072 §5, T109a).
func TestNever(t *testing.T) {
	ops := api.MustOperations()
	tools := enablementagent.Tools()
	for _, id := range slices.Concat(tools, enablementagent.HelpReads) {
		if op, ok := ops[id]; !ok || op.Method != http.MethodGet && id != recommend.Proposer {
			t.Errorf("%s: got in the contract %v, method %s; want a read", id, ok, op.Method)
		}
	}
	for id, op := range ops {
		writes := strings.HasPrefix(op.Permission, "config.") || op.Permission == "identity.access.manage"
		if op.Method != http.MethodGet && writes && !slices.Contains(enablementagent.Never, id) {
			t.Errorf("never list: got %v, want it to hold %s", enablementagent.Never, id)
		}
	}
	for _, id := range enablementagent.Never {
		if _, ok := ops[id]; !ok || slices.Contains(tools, id) || slices.Contains(enablementagent.HelpTools(), id) {
			t.Errorf("%s: got in the contract %v, among the tools %v; want in the contract and no tool", id, ok, slices.Contains(tools, id))
		}
	}
	if got, want := enablementagent.Processes(), []string{"order_to_cash", "purchase_to_pay"}; !slices.Equal(got, want) {
		t.Errorf("processes: got %v, want %v", got, want)
	}
}
