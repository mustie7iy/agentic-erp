package regulatoryagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/regulatoryagent"
)

const (
	session = "01a1aa00-0000-7000-8000-000000000a01"
	version = "01a1aa00-0000-7000-8000-000000000b01"
)

// notice is an invented notice: no authority issued it.
const notice = `INVENTED FOR A TEST: no authority issued this notice.
Notice EX-2099-01 of the Example Revenue Office.
From 1 July 2099 the rate of tax code V0, input tax exempt, rises from 0 percent to 2.5 percent.`

// document is the active version's configuration: company 1000 with its
// tax code V0 at 0 percent and its registration.
const document = `{"companies": [{"code": "1000", "tax_codes": [{"code": "V0", "description": "Input tax exempt", "rate_percent": "0"}],
	"tax_registrations": [{"code": "USFED"}]}]}`

// fakeAPI is a stand-in for the cell's API: the active configuration
// version 3, a session, and recommendations answered by their bodies; it
// keeps every request it was sent.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []string
	versions string // the answer of the active version's list; "": version 3
	status   int    // its status; 0: 200
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
	v := `{"id": "` + version + `", "number": 3, "is_active": true, "document": ` + document + `}`
	switch path := r.URL.Path; {
	case path == "/v1/config-versions" && a.status != 0:
		answer(a.status, `{"code": "identity.permission_missing", "status": 403}`)
	case path == "/v1/config-versions" && a.versions != "":
		answer(200, a.versions)
	case path == "/v1/config-versions":
		answer(200, `{"items": [`+v+`]}`)
	case path == "/v1/config-versions/"+version:
		answer(200, v)
	case path == "/v1/agent-sessions":
		answer(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case path == "/v1/recommendations":
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

func client(t *testing.T, a *fakeAPI) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL, Token: "t"}
}

var sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)

// rec is a recommendation of kind with the content on the version, in
// the session the conversation names.
func rec(kind, content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "config_version",
			"object_id": version, "kind": kind, "content": content}}
	})
}

var versionRead = llmtest.Tool("getConfigVersion", map[string]any{"id": version})

const (
	paths  = "Paths: companies[1000].tax_codes[V0]\n"
	effect = "Takes effect: \"From 1 July 2099\"\n"
	quote  = "The notice says \"the rate of tax code V0, input tax exempt, rises from 0 percent to 2.5 percent\". "
	caveat = regulatoryagent.Caveat
)

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

// TestRead: the agent reads the notice against active configuration
// version 3 in one session and recommends on it once. Its check refuses,
// and counts, a quotation the notice does not write (changed, or left
// open to the end), a content without the caveat, a number outside the
// quotations the run did not read, a change_needed without its paths, or
// without when it takes effect, or quoting nothing, and a path the
// version does not hold; nothing refused reaches the API.
func TestRead(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t, versionRead,
		rec("change_needed", paths+effect+"The notice says \"rises to 3 percent\". "+caveat),
		rec("change_needed", paths+effect+"The notice says \"rises from 0 percent to 2.5 percent. "+caveat),
		rec("change_needed", paths+effect+quote),
		rec("change_needed", paths+effect+quote+"Input tax goes up by 2.5 percent. "+caveat),
		rec("change_needed", effect+quote+caveat),
		rec("change_needed", paths+quote+caveat),
		rec("change_needed", paths+"Takes effect: not stated\nThe rate of V0 changes. "+caveat),
		rec("change_needed", "Paths: companies[1000].tax_codes[V0], companies[1000].tax_codes[V9]\n"+effect+quote+caveat),
		rec("change_needed", paths+effect+"The notice says “the rate of tax code V0, input tax exempt, rises from 0 percent to 2.5 percent”. Tax code V0 is at rate 0 in company 1000. "+caveat),
		llmtest.Final("Recommended a change of tax code V0's rate; a person checks the notice against its source."))
	res, err := (&regulatoryagent.Agent{LLM: model.Runner(), API: client(t, a)}).Read(context.Background(), notice)
	if err != nil {
		t.Fatal(err)
	}
	want := []recommend.Made{{ID: "r1", ObjectType: "config_version", ObjectID: version, Kind: "change_needed"}}
	if !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 8 {
		t.Errorf("refused: got %d, want 8", res.Refused)
	}
	told(t, model.Requests[:1], "The tenant's active configuration is version 3 (id "+version+").\n<notice>\n"+notice+"\n</notice>")
	told(t, model.Requests, regulatoryagent.ErrNotQuoted.Error(), regulatoryagent.ErrNoCaveat.Error(), regulatoryagent.ErrUnread.Error(),
		regulatoryagent.ErrForm.Error(), regulatoryagent.ErrPath.Error(), `not "rises to 3 percent"`, "not 2.5", `not "companies[1000].tax_codes[V9]"`)
}

// TestReadNoChange: no_change needs no paths and no quotation; what it
// quotes is the notice's, and it carries the caveat.
func TestReadNoChange(t *testing.T) {
	model := llmtest.New(t, versionRead,
		rec("no_change", "The notice concerns \"Notice EX-2099-02\"."+caveat),
		rec("no_change", "The notice concerns \"Notice EX-2099-01\" of an office the configuration names nowhere."),
		rec("no_change", "The configuration holds no tax the notice changes: V0 is the notice's, but \"\" is quoted empty. "+caveat),
		llmtest.Final("No change needed."))
	res, err := (&regulatoryagent.Agent{LLM: model.Runner(), API: client(t, &fakeAPI{})}).Read(context.Background(), notice)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recommendations) != 1 || res.Recommendations[0].Kind != "no_change" || res.Refused != 2 {
		t.Errorf("got %v and %d refused, want one no_change and two refused", res.Recommendations, res.Refused)
	}
}

// TestNever: the agent's tools are reads and the one recommendation; none
// is a write of the configuration, every one of which is on its never
// list (ADR-0072, T107a).
func TestNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	tools := regulatoryagent.Tools()
	for _, id := range tools {
		if op, ok := ops[id]; !ok || op.Method != http.MethodGet && id != recommend.Proposer {
			t.Errorf("%s: got in the contract %v, method %s; want a read", id, ok, op.Method)
		}
	}
	for id, op := range ops {
		if op.Method != http.MethodGet && strings.HasPrefix(op.Permission, "config.") && !slices.Contains(regulatoryagent.Never, id) {
			t.Errorf("never list: got %v, want it to hold %s", regulatoryagent.Never, id)
		}
	}
	for _, id := range regulatoryagent.Never {
		if _, ok := ops[id]; !ok || slices.Contains(tools, id) {
			t.Errorf("%s: got in the contract %v, among the tools %v; want in the contract and no tool", id, ok, slices.Contains(tools, id))
		}
	}
}

// TestReadRefusals: a notice that is empty, too long or not text, an
// active version the agent may not read or that is none, and an API that
// is gone stop the run before a session starts.
func TestReadRefusals(t *testing.T) {
	model := llmtest.New(t)
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		api    *apitools.Client
		notice string
		want   string
	}{
		"an empty notice":        {client(t, &fakeAPI{}), "", "a notice is UTF-8 text"},
		"a notice too long":      {client(t, &fakeAPI{}), strings.Repeat("x", regulatoryagent.MaxNotice+1), "a notice is UTF-8 text"},
		"a notice not text":      {client(t, &fakeAPI{}), "\xff\xfe", "a notice is UTF-8 text"},
		"a version refused":      {client(t, &fakeAPI{status: 403}), notice, "identity.permission_missing"},
		"no active version":      {client(t, &fakeAPI{versions: `{"items": []}`}), notice, "the active configuration version: no active version"},
		"a version without id":   {client(t, &fakeAPI{versions: `{"items": [{"number": 1}]}`}), notice, "no active version"},
		"the API gone":           {gone, notice, "127.0.0.1:1"},
		"a list that is no list": {client(t, &fakeAPI{versions: `[]`}), notice, "decode"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&regulatoryagent.Agent{LLM: model.Runner(), API: c.api}).Read(context.Background(), c.notice)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
			if res.Session != "" {
				t.Errorf("a session: got %s, want none", res.Session)
			}
		})
	}
}
