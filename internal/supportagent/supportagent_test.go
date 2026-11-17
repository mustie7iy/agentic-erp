package supportagent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/supportagent"
)

const (
	session = "01a1aa00-0000-7000-8000-000000000a01"
	version = "01a1aa00-0000-7000-8000-000000000b01"
)

// document is the active version's configuration: company 1000 with its
// price tolerance.
const document = `{"companies": [{"code": "1000", "invoice_tolerance": {"price_percent": 2, "price_absolute": "5.00"}}]}`

// fakeAPI is a stand-in for the cell's API: the active configuration
// version 3 and a session; it keeps every request it was sent.
type fakeAPI struct {
	mu      sync.Mutex
	calls   []string
	status  int // the active version's status; 0: 200
	session int // the session's status; 0: 201
}

func (a *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	case path == "/v1/config-versions":
		answer(200, `{"items": [`+v+`]}`)
	case path == "/v1/config-versions/"+version:
		answer(200, v)
	case path == "/v1/agent-sessions" && a.session != 0:
		answer(a.session, `{"code": "identity.permission_missing", "status": 403}`)
	case path == "/v1/agent-sessions":
		answer(201, `{"id": "`+session+`", "autonomy_level": 1}`)
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

func reply(text string) llmtest.Step {
	return llmtest.Tool(supportagent.Reply, map[string]any{"text": text})
}

const (
	question  = "How is an invoice of 500 dollars above the price tolerance released?"
	tolerance = "`companies[1000].invoice_tolerance.price_percent` = `2`"
	answer    = "1. An invoice above " + tolerance + " is held.\n2. Call releaseSupplierInvoice on the invoice of 500 dollars, as someone " +
		"holding `payables.invoice.review`; it answers `payables.invalid_status` unless the invoice is `on_hold`."
)

// told reports whether the model was told each text, which it reads as
// JSON text.
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

// TestAsk: the agent reads the contract and the active version and
// replies once. Its check refuses, and counts, a reply naming an
// operation the contract does not have, one naming none without saying
// so, a setting cited with another value or a path the version does not
// hold, a number outside the backticks the question does not write, and a
// second reply; a list's numbers and the question's are allowed.
func TestAsk(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t,
		llmtest.Tool(apitools.FindOperations, map[string]any{"query": "release hold"}),
		llmtest.Tool("getConfigVersion", map[string]any{"id": version}),
		reply("Call releaseInvoice."),
		reply("Nothing to do."),
		reply("Call releaseSupplierInvoice; `companies[1000].invoice_tolerance.price_percent` = `3`."),
		reply("Call releaseSupplierInvoice; `companies[1000].tolerance` = `2`."),
		reply("Call releaseSupplierInvoice for a variance within 7 percent."),
		reply(answer),
		reply("Call releaseSupplierInvoice."),
		llmtest.Final("Answered."))
	res, err := (&supportagent.Agent{LLM: model.Runner(), API: client(t, a)}).Ask(context.Background(), question)
	if err != nil {
		t.Fatal(err)
	}
	if res.Session != session || res.Reply != answer {
		t.Errorf("result: got session %s and reply %q, want %s and %q", res.Session, res.Reply, session, answer)
	}
	if res.Refused != 6 {
		t.Errorf("refused: got %d, want 6", res.Refused)
	}
	if n := a.sent(":end"); n != 1 {
		t.Errorf("sessions ended: got %d, want 1", n)
	}
	told(t, model.Requests[:1], "The tenant's active configuration is version 3 (id "+version+").\n<question>\n"+question+"\n</question>")
	told(t, model.Requests, "releaseSupplierInvoice", supportagent.ErrOperation.Error(), "not releaseInvoice", supportagent.ErrUnanswered.Error(),
		supportagent.ErrSetting.Error(), "not companies[1000].invoice_tolerance.price_percent = 3", "not companies[1000].tolerance = 2",
		supportagent.ErrNumber.Error(), "not 7", supportagent.ErrSecond.Error())
}

// TestAskUnanswered: a question no operation answers gets a reply that
// names none and says so.
func TestAskUnanswered(t *testing.T) {
	model := llmtest.New(t, reply("Booking a flight is not something this system does. "+supportagent.Unanswered), llmtest.Final("Answered."))
	res, err := (&supportagent.Agent{LLM: model.Runner(), API: client(t, &fakeAPI{})}).Ask(context.Background(), "How do I book a flight?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(res.Reply, supportagent.Unanswered) || res.Refused != 0 {
		t.Errorf("got reply %q and %d refused, want the unanswered sentence and none", res.Reply, res.Refused)
	}
}

// TestAskRefusals: a question that is empty, too long or not text, an
// active version the agent may not read, a session it may not start and
// an API that is gone stop the run before a session; a model that fails,
// and one that gives no reply the check admits, fail the run after it.
func TestAskRefusals(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		api      *apitools.Client
		question string
		want     string
	}{
		"an empty question":   {client(t, &fakeAPI{}), "", "a question is UTF-8 text"},
		"a question too long": {client(t, &fakeAPI{}), strings.Repeat("x", supportagent.MaxQuestion+1), "a question is UTF-8 text"},
		"a question not text": {client(t, &fakeAPI{}), "\xff\xfe", "a question is UTF-8 text"},
		"a version refused":   {client(t, &fakeAPI{status: 403}), question, "identity.permission_missing"},
		"a session refused":   {client(t, &fakeAPI{session: 403}), question, "identity.permission_missing"},
		"the API gone":        {gone, question, "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&supportagent.Agent{LLM: llmtest.New(t).Runner(), API: c.api}).Ask(context.Background(), c.question)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error saying %q", err, c.want)
			}
			if res.Session != "" {
				t.Errorf("session: got %s, want none", res.Session)
			}
		})
	}
	if _, err := (&supportagent.Agent{LLM: llmtest.New(t).Runner(), API: client(t, &fakeAPI{})}).Ask(context.Background(), question); err == nil {
		t.Error("a model that fails: got no error")
	}
	model := llmtest.New(t, reply("Call releaseInvoice."), llmtest.Final("Done."))
	res, err := (&supportagent.Agent{LLM: model.Runner(), API: client(t, &fakeAPI{}), MaxIterations: 5}).Ask(context.Background(), question)
	if err == nil || !strings.Contains(err.Error(), supportagent.ErrNoReply.Error()) || res.Refused != 1 {
		t.Errorf("no reply admitted: got %v and %d refused, want ErrNoReply and 1", err, res.Refused)
	}
}

// TestTools: the agent's tools are reads of the contract open to agents,
// the contract's search and its reply: none writes.
func TestTools(t *testing.T) {
	ops := api.MustOperations()
	for _, id := range supportagent.Reads {
		if op, ok := ops[id]; !ok || op.Method != http.MethodGet || !op.AgentAllowed {
			t.Errorf("%s: got in the contract %v, method %s, open to agents %v; want a read open to agents", id, ok, op.Method, op.AgentAllowed)
		}
	}
	if got, want := supportagent.Tools(), append(slices.Clone(supportagent.Reads), apitools.FindOperations, supportagent.Reply); !slices.Equal(got, want) {
		t.Errorf("tools: got %v, want %v", got, want)
	}
}

// TestAskToolsUnknown: an operation the contract does not have ends the
// session rather than leave it running (review:batch76).
func TestAskToolsUnknown(t *testing.T) {
	reads := supportagent.Reads
	supportagent.Reads = append(slices.Clone(reads), "noSuchOperation")
	t.Cleanup(func() { supportagent.Reads = reads })
	a := &fakeAPI{}
	_, err := (&supportagent.Agent{LLM: llmtest.New(t).Runner(), API: client(t, a)}).Ask(context.Background(), question)
	if err == nil || !strings.Contains(err.Error(), "noSuchOperation") {
		t.Errorf("ask: got %v, want the operation named", err)
	}
	if got := a.sent(session + ":end"); got != 1 {
		t.Errorf("sessions ended: got %d, want 1", got)
	}
}
