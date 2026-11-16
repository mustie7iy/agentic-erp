package supportagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/supportagent"
)

const (
	invoice = "01a1aa00-0000-7000-8000-000000000c01"
	another = "01a1aa00-0000-7000-8000-000000000c09"
)

// diagAPI is a stand-in for the cell's API as the diagnosis reads it: a
// supplier invoice whose posting was refused for a closed period (audit
// event 41) beside another object's event 42 (whose own events it may
// not read), the active configuration
// version, a session and recommendations answered by their bodies.
type diagAPI struct {
	mu      sync.Mutex
	calls   []string
	missing bool // the invoice is not found
	config  int  // the active version's status; 0: 200
	made    int
}

func (a *diagAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	case path == "/v1/supplier-invoices/"+invoice && a.missing:
		answer(404, `{"code": "request.not_found", "status": 404}`)
	case path == "/v1/supplier-invoices/"+invoice:
		answer(200, `{"id": "`+invoice+`", "status": "parked", "document_number": "SI100007"}`)
	case path == "/v1/config-versions" && a.config != 0:
		answer(a.config, `{"code": "identity.permission_missing", "status": 403}`)
	case path == "/v1/config-versions":
		answer(200, `{"items": [`+v+`]}`)
	case path == "/v1/audit-events" && r.URL.Query().Get("object_id") == another:
		answer(403, `{"code": "identity.permission_missing", "status": 403}`)
	case path == "/v1/audit-events":
		answer(200, `{"items": [{"seq": 41, "object_id": "`+invoice+`", "action": "payables.supplier_invoice.post", "outcome": "refused",
			"detail": {"code": "ledger.period_not_open", "detail": "Period 6/2025 is closed; postings need it open."}},
			{"seq": 42, "object_id": "`+another+`", "action": "ledger.fiscal_period.close", "outcome": "succeeded"}]}`)
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

func diagClient(t *testing.T, a *diagAPI) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL, Token: "t"}
}

var sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)

// cause is a root cause with the content on the invoice, in the session
// the conversation names.
func cause(content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "supplier_invoice",
			"object_id": invoice, "kind": "root_cause", "content": content}}
	})
}

var eventsRead = llmtest.Tool("listAuditEvents", map[string]any{"object_id": invoice})

// TestDiagnose: the agent diagnoses a supplier invoice whose posting was
// refused once, in one session. Its check refuses, and counts, a first
// line without a class, an event the run did not read for the invoice (one
// never read, another object's), a cause without an event, a setting the
// active configuration does not hold and a number no tool returned; the
// closed period named with the refusal's event is recorded.
func TestDiagnose(t *testing.T) {
	a := &diagAPI{}
	model := llmtest.New(t, eventsRead,
		cause("The period is closed."),
		cause("Root cause: weather\nevent 41"),
		cause("Root cause: closed_period\nevent 7 refused the posting."),
		cause("Root cause: closed_period\nEvent 42 closed it."),
		cause("Root cause: master_data\nThe supplier is blocked."),
		cause("Root cause: configuration\nevent 41, under `companies[1000].invoice_tolerance.price_percent` = `3`."),
		cause("Root cause: closed_period\nevent 41: period 6/2025 is closed since 12 days."),
		cause("Root cause: closed_period\nevent 41: the posting of `98765.43` was refused."),
		llmtest.Tool(apitools.FindOperations, map[string]any{"operation_ids": []any{"postSupplierInvoice"}}),
		cause("Root cause: closed_period.\nThe posting was refused, event 41: \"Period 6/2025 is closed; postings need it open.\" "+
			"The tolerance `companies[1000].invoice_tolerance.price_percent` = `2` played no part; the report names no events 2025-06-15. Open the period, or post on a date of an open one."),
		llmtest.Final("The invoice's period is closed."))
	// The id as a person may type it, in capitals: the run reads it as the API writes it.
	res, err := (&supportagent.Agent{LLM: model.Runner(), API: diagClient(t, a)}).Diagnose(context.Background(), "supplier_invoice", strings.ToUpper(invoice),
		"Posting it on 2025-06-15 was refused.")
	if err != nil {
		t.Fatal(err)
	}
	want := []recommend.Made{{ID: "r1", ObjectType: "supplier_invoice", ObjectID: invoice, Kind: "root_cause"}}
	if !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 8 {
		t.Errorf("refused: got %d, want 8", res.Refused)
	}
	prompt := "The document is the supplier_invoice " + invoice + " (read it with getSupplierInvoice). The tenant's active configuration is version 3 (id " +
		version + ").\n<report>\nPosting it on 2025-06-15 was refused.\n</report>"
	if told, _ := json.Marshal(prompt); !strings.Contains(model.Requests[0], strings.Trim(string(told), `"`)) { // as the request writes it
		t.Errorf("prompt: got %s, want %q", model.Requests[0], prompt)
	}
	refusals := strings.Join(model.Requests, "\n")
	for _, want := range []error{supportagent.ErrClass, supportagent.ErrEvent, supportagent.ErrNoEvent, supportagent.ErrSetting, supportagent.ErrDiagnosisNumber} {
		told, _ := json.Marshal(want.Error()) // the model reads the refusal as JSON text
		if !strings.Contains(refusals, strings.Trim(string(told), `"`)) {
			t.Errorf("the model was not told %q", want)
		}
	}
	for _, want := range []string{"not event 7", "not event 42", "not 12", "not 98765.43"} {
		if !strings.Contains(refusals, want) {
			t.Errorf("refusals: want %q among them", want)
		}
	}
}

// TestDiagnoseUnknown: a cause the events do not show is unknown, which
// needs no event; a refused read of the events shows none.
func TestDiagnoseUnknown(t *testing.T) {
	a := &diagAPI{}
	model := llmtest.New(t, llmtest.Tool("listAuditEvents", map[string]any{"object_id": another}),
		cause("Root cause: unknown\nThe audit trail shows nothing refused; the service's logs are not read."),
		llmtest.Final("Unknown."))
	res, err := (&supportagent.Agent{LLM: model.Runner(), API: diagClient(t, a)}).Diagnose(context.Background(), "supplier_invoice", invoice, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recommendations) != 1 || res.Refused != 0 {
		t.Errorf("got %v, %d refused; want one unknown and none refused", res.Recommendations, res.Refused)
	}
}

// TestDiagnoseRefusals: a kind the agent does not read, an id that is
// none, an account of what happened that is too long or not text, a
// document or a configuration the API refuses and an API that is gone
// stop the run before a session starts.
func TestDiagnoseRefusals(t *testing.T) {
	model := llmtest.New(t)
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		api            *apitools.Client
		kind, id, said string
		want           string
	}{
		"a kind not read":        {diagClient(t, &diagAPI{}), "bank_statement", invoice, "", "a document is one of the kinds"},
		"an id that is none":     {diagClient(t, &diagAPI{}), "supplier_invoice", "SI100007", "", `not supplier_invoice "SI100007"`},
		"too long a report":      {diagClient(t, &diagAPI{}), "supplier_invoice", invoice, strings.Repeat("x", supportagent.MaxReport+1), "what happened is UTF-8 text"},
		"a report that is bytes": {diagClient(t, &diagAPI{}), "supplier_invoice", invoice, "\xff", "what happened is UTF-8 text"},
		"the document not found": {diagClient(t, &diagAPI{missing: true}), "supplier_invoice", invoice, "", "request.not_found"},
		"the version refused":    {diagClient(t, &diagAPI{config: 403}), "supplier_invoice", invoice, "", "the active configuration version"},
		"the API gone":           {gone, "supplier_invoice", invoice, "", "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&supportagent.Agent{LLM: model.Runner(), API: c.api}).Diagnose(context.Background(), c.kind, c.id, c.said)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
			if res.Session != "" {
				t.Errorf("a session: got %s, want none", res.Session)
			}
		})
	}
}

// TestDiagnoseTools: the diagnosis's tools are reads of the contract open
// to agents, each document's read among them, the contract's search and
// the one recommendation: nothing that writes.
func TestDiagnoseTools(t *testing.T) {
	ops := api.MustOperations()
	tools := supportagent.DiagnoseTools()
	for _, read := range supportagent.Documents {
		if !slices.Contains(tools, read) {
			t.Errorf("tools: got %v, want the document read %s among them", tools, read)
		}
	}
	for _, id := range tools {
		t.Run(id, func(t *testing.T) {
			if id == apitools.FindOperations {
				return
			}
			op, ok := ops[id]
			if !ok || !op.AgentAllowed || (op.Method != http.MethodGet && id != recommend.Proposer) {
				t.Errorf("%s: got in the contract %v, open to agents %v, method %s; want a read open to agents", id, ok, op.AgentAllowed, op.Method)
			}
		})
	}
	if _, err := apitools.Tools(&apitools.Client{}, nil, slices.DeleteFunc(slices.Clone(tools), func(id string) bool { return id == apitools.FindOperations })...); err != nil {
		t.Errorf("the tools as the pattern builds them: %v", err)
	}
}
