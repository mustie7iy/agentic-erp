package taxagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/taxagent"
)

const (
	reg     = "01a1aa00-0000-7000-8000-000000000e01"
	session = "01a1aa00-0000-7000-8000-000000000a01"
	le      = "01a1aa00-0000-7000-8000-000000000b01" // the registrant
	member  = "01a1aa00-0000-7000-8000-000000000b02" // a tax group's member
	other   = "01a1aa00-0000-7000-8000-000000000b09" // a company the registration does not cover
)

// documents are the parked documents and invoices the fake API answers,
// by id: parked ones of the registration's companies, a posted one and
// one of another company.
var documents = map[string]string{
	"pe1": `{"id": "pe1", "legal_entity_id": "` + le + `", "status": "parked"}`,
	"pe2": `{"id": "pe2", "legal_entity_id": "` + le + `", "status": "posted"}`,
	"pe3": `{"id": "pe3", "legal_entity_id": "` + other + `", "status": "parked"}`,
	"pe4": `{"id": "pe4", "legal_entity_id": "` + member + `", "status": "parked"}`,
	"si1": `{"id": "si1", "legal_entity_id": "` + le + `", "status": "parked"}`,
}

// fakeAPI is a stand-in for the cell's API: registration GSTHST and its
// draft of the second quarter, a session, the parked documents, and
// recommendations and parked entries answered by their content; it keeps
// every request it was sent.
type fakeAPI struct {
	mu     sync.Mutex
	calls  []string
	draft  int // the draft's status; 0: 200
	start  int // the session's status; 0: 201
	regs   string
	made   int // the recommendations it accepted
	parked int // the entries it parked
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
	case path == "/v1/tax-registrations":
		regs := a.regs
		if regs == "" {
			regs = `{"id": "01a1aa00-0000-7000-8000-000000000e09", "code": "QST"}, {"id": "` + reg + `", "code": "GSTHST", "company_code": "3000", "legal_entity_id": "` + le + `", "group_members": [{"legal_entity_id": "` + member + `"}]}`
		}
		answer(200, `{"items": [`+regs+`]}`)
	case strings.HasSuffix(path, "/draft-return") && a.draft != 0:
		answer(a.draft, `{"code": "tax.period_not_ended", "status": 409}`)
	case strings.HasSuffix(path, "/draft-return"):
		answer(200, `{"return_type": "GST34", "period_start": "2026-04-01", "period_end": "2026-06-30"}`)
	case path == "/v1/agent-sessions" && a.start != 0:
		answer(a.start, `{"code": "identity.permission_missing", "status": 403}`)
	case path == "/v1/agent-sessions":
		answer(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case path == "/v1/recommendations":
		var body struct {
			Content  string
			ObjectID string `json:"object_id"`
		}
		_ = json.Unmarshal(b, &body) // the agent's own JSON
		if strings.Contains(body.Content, "refuse") {
			answer(422, `{"code": "request.validation_failed", "status": 422}`)
			return
		}
		a.mu.Lock()
		a.made++
		id := fmt.Sprintf("r%d", a.made)
		a.mu.Unlock()
		answer(201, `{"id": "`+id+`", "object_id": "`+body.ObjectID+`"}`)
	case path == "/v1/parked-journal-entries" && r.Method == http.MethodPost:
		if strings.Contains(string(b), "refuse") {
			answer(422, `{"code": "ledger.unknown_tax_code", "status": 422}`)
			return
		}
		a.mu.Lock()
		a.parked++
		id := fmt.Sprintf("p%d", a.parked)
		a.mu.Unlock()
		answer(201, `{"id": "`+id+`"}`)
	case strings.HasPrefix(path, "/v1/parked-journal-entries/") || strings.HasPrefix(path, "/v1/supplier-invoices/"):
		doc, ok := documents[path[strings.LastIndex(path, "/")+1:]]
		if !ok {
			answer(404, `{"code": "request.not_found", "status": 404}`)
			return
		}
		answer(200, doc)
	default:
		answer(200, `{}`)
	}
}

// sent are the calls the API was sent whose method and path contain
// what.
func (a *fakeAPI) sent(what string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(a.calls), func(c string) bool { return !strings.Contains(c, what) })
}

var sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)

// content is an explanation of the draft as the prompt asks for one.
const content = "GST34 of 2026-04-01 to 2026-06-30: line 103 70.00, up 65.00 on 2026-01-01 to 2026-03-31 from entries 100002 and 100003."

// explain is an explanation of the draft, unless fields change it (nil:
// leave it out), in the session the conversation names.
func explain(fields map[string]any) func(string) any {
	return func(conversation string) any {
		body := map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "tax_registration", "object_id": reg,
			"kind": "explain_draft_return", "content": content}
		for k, v := range fields {
			body[k] = v
			if v == nil {
				delete(body, k)
			}
		}
		return map[string]any{"body": body}
	}
}

func client(t *testing.T, a *fakeAPI) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL, Token: "t"}
}

// TestExplain: the agent explains GSTHST's draft of the second quarter in
// one session, reading through its tools and recommending once; the
// API's refusal and the run's own guard (another session, registration,
// object type, kind or period, a period named only after another, no
// body, a second explanation) are counted, and nothing but the run's one
// explanation reaches the API.
func TestExplain(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t,
		llmtest.Tool("getTaxReturnDraft", map[string]any{"id": reg, "period_start": "2026-04-01"}),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"content": "GST34 of 2026-04-01 to 2026-06-30: refuse it."})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"session_id": reg})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"object_id": session})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"object_type": "tax_return"})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"kind": "prepare_return"})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"content": "GST34 of 2026-01-01 to 2026-03-31, not 2026-04-01 to 2026-06-30: prepare it."})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"content": "Line 103 of 2026-04-01 to 2026-06-30 is 70.00."})),
		llmtest.Tool("createRecommendation", map[string]any{}),
		llmtest.ToolFn("createRecommendation", explain(nil)),
		llmtest.ToolFn("createRecommendation", explain(nil)),
		llmtest.Final("Explained the draft; nothing else."))
	res, err := (&taxagent.Agent{LLM: model.Runner(), API: client(t, a)}).Explain(context.Background(), "GSTHST", "2026-04-01")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(res.Recommendations, " "), "r1"; got != want {
		t.Errorf("recommendations: got %q, want %q", got, want)
	}
	if res.Refused != 9 {
		t.Errorf("refused: got %d, want 9: the API's one and the guard's eight", res.Refused)
	}
	if res.Session != session {
		t.Errorf("session: got %s, want %s", res.Session, session)
	}
	if res.Reply != "Explained the draft; nothing else." {
		t.Errorf("reply: got %q, want the model's last text", res.Reply)
	}
	if got := len(a.sent("recommendations")); got != 2 {
		t.Errorf("recommendations sent: got %d %v, want 2: the guard keeps the others from the API", got, a.sent("recommendations"))
	}
	offered := model.ToolNames()
	slices.Sort(offered)
	want := slices.Clone(taxagent.Tools)
	slices.Sort(want)
	if strings.Join(offered, " ") != strings.Join(want, " ") {
		t.Errorf("tools offered: got %v, want %v", offered, want)
	}
	if !strings.Contains(model.Requests[0], "GSTHST (id "+reg+", company 3000, legal entity "+le+") for the period 2026-04-01 to 2026-06-30") {
		t.Errorf("prompt: got %s, want the draft named", model.Requests[0])
	}
}

// correct is a tax code correction of the document id of objectType in
// the session the conversation names.
func correct(objectType, id, content string) func(string) any {
	return func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": objectType,
			"object_id": id, "kind": "correct_tax_code", "content": content}}
	}
}

// adjust is an adjustment entry of the company le, of the tax date day,
// its tax account's line coded code and its header text.
func adjust(le, day, code, text string) map[string]any {
	return map[string]any{"body": map[string]any{"legal_entity_id": le, "document_date": "2026-06-30", "posting_date": "2026-06-30",
		"tax_date": day, "currency": "CAD", "header_text": text, "lines": []any{
			map[string]any{"account_id": "a2300", "amount": map[string]any{"amount": "100.00", "currency": "CAD"}, "tax_code": code},
			map[string]any{"account_id": "a2300", "amount": map[string]any{"amount": "-100.00", "currency": "CAD"}}}}}
}

// TestPropose: besides its explanation, the agent parks adjustment
// entries of the period and recommends tax code corrections of the
// parked documents it read. The run's guard refuses, and counts, a
// correction of a document it did not read, read in another status or of
// a company the registration does not cover, of another object type, of
// another kind or session, or a second one of a document; and an entry
// to park of another company, with a tax date outside the period or not
// a date, without a coded line or without a body. The API's refusals are
// counted; nothing the guard refuses reaches it.
func TestPropose(t *testing.T) {
	a := &fakeAPI{}
	const pe, si = "parked_journal_entry", "supplier_invoice"
	steps := []llmtest.Step{
		llmtest.Tool("getParkedJournalEntry", map[string]any{"id": "pe1"}),
		llmtest.Tool("getParkedJournalEntry", map[string]any{"id": "pe2"}),
		llmtest.Tool("getParkedJournalEntry", map[string]any{"id": "pe3"}),
		llmtest.Tool("getParkedJournalEntry", map[string]any{"id": "pe4"}),
		llmtest.Tool("getParkedJournalEntry", map[string]any{"id": "pe9"}),
		llmtest.Tool("getSupplierInvoice", map[string]any{"id": "si1"}),
		llmtest.ToolFn("createRecommendation", explain(nil)),
		llmtest.ToolFn("createRecommendation", correct(pe, "pe1", "Line 2: GO5; refuse it.")),
		llmtest.ToolFn("createRecommendation", correct(pe, "pe1", "Lines 2 and 3: GO5, the tax account's line carries none.")),
		llmtest.ToolFn("createRecommendation", correct(pe, "pe1", "Again.")),
		llmtest.ToolFn("createRecommendation", correct(pe, "pe2", "Posted already.")),
		llmtest.ToolFn("createRecommendation", correct(pe, "pe3", "Another company's.")),
		llmtest.ToolFn("createRecommendation", correct(pe, "pe9", "Never read.")),
		llmtest.ToolFn("createRecommendation", correct(pe, "si1", "An invoice called an entry.")),
		llmtest.ToolFn("createRecommendation", correct(si, "si1", "Item 1: the code its supplier's tax fits.")),
		llmtest.ToolFn("createRecommendation", correct(pe, "pe4", "The group member's entry: GO5.")),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"kind": "correct_tax_code"})),
		llmtest.ToolFn("createRecommendation", explain(map[string]any{"kind": "post_entry", "object_type": pe, "object_id": "pe1"})),
		llmtest.ToolFn("createRecommendation", func(c string) any {
			in := correct(pe, "pe4", "Another session's.")(c).(map[string]any)
			in["body"].(map[string]any)["session_id"] = reg
			return in
		}),
		llmtest.Tool("parkJournalEntry", adjust(le, "2026-06-30", "GI", "Tax agent: entry 100004's instalment on line 110")),
		llmtest.Tool("parkJournalEntry", adjust(member, "2026-04-01", "GI", "The group member's")),
		llmtest.Tool("parkJournalEntry", adjust(le, "2026-06-30", "XX", "refuse: no such code")),
		llmtest.Tool("parkJournalEntry", adjust(other, "2026-06-30", "GI", "Another company's")),
		llmtest.Tool("parkJournalEntry", adjust(le, "2026-07-01", "GI", "After the period")),
		llmtest.Tool("parkJournalEntry", adjust(le, "2026-03-31", "GI", "Before the period")),
		llmtest.Tool("parkJournalEntry", adjust(le, "2026-06-30T00:00:00Z", "GI", "Not a date")),
		llmtest.Tool("parkJournalEntry", adjust(le, "2026-06-30", "", "No code")),
		llmtest.Tool("parkJournalEntry", map[string]any{}),
		llmtest.Final("Explained the draft, parked two adjustments and proposed three corrections."),
	}
	model := llmtest.New(t, steps...)
	res, err := (&taxagent.Agent{LLM: model.Runner(), API: client(t, a)}).Explain(context.Background(), "GSTHST", "2026-04-01")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(res.Recommendations, " "), "r1 r2 r3 r4"; got != want {
		t.Errorf("recommendations: got %q, want %q: the explanation and three corrections", got, want)
	}
	if got, want := strings.Join(res.Parked, " "), "p1 p2"; got != want {
		t.Errorf("parked: got %q, want %q", got, want)
	}
	if res.Refused != 16 {
		t.Errorf("refused: got %d, want 16: the API's two and the guard's fourteen", res.Refused)
	}
	if got := len(a.sent("recommendations")); got != 5 {
		t.Errorf("recommendations sent: got %d %v, want 5: the guard keeps the others from the API", got, a.sent("recommendations"))
	}
	if got := len(a.sent("POST /v1/parked-journal-entries")); got != 3 {
		t.Errorf("entries sent to park: got %d, want 3: the guard keeps the others from the API", got)
	}
	if got := a.sent(":post"); len(got) != 0 {
		t.Errorf("posts: got %v, want none", got)
	}
}

// TestExplainByID: a registration is found by its id as by its code.
func TestExplainByID(t *testing.T) {
	model := llmtest.New(t, llmtest.ToolFn("createRecommendation", explain(nil)), llmtest.Final("Done."))
	res, err := (&taxagent.Agent{LLM: model.Runner(), API: client(t, &fakeAPI{}), MaxIterations: 5}).Explain(context.Background(), reg, "2026-04-01")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if got := strings.Join(res.Recommendations, " "); got != "r1" {
		t.Errorf("recommendations: got %q, want r1", got)
	}
}

// TestNeverTransitions: the agent's tools read or propose, none moves a
// return, and the operations that do are not open to agents, so no tool
// of theirs can be built (ADR-0049 §13).
func TestNeverTransitions(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range taxagent.Tools {
		t.Run(id, func(t *testing.T) {
			op, ok := ops[id]
			if !ok {
				t.Fatalf("%s: not in the contract", id)
			}
			if op.Method != http.MethodGet && !op.Proposes {
				t.Errorf("%s: got %s without x-autonomy: propose, want a read or a proposal", id, op.Method)
			}
		})
	}
	for _, id := range []string{"prepareTaxReturn", "fileTaxReturn", "withdrawTaxReturn", "approveApprovalRequest", "rejectApprovalRequest",
		"decideRecommendation", "postParkedJournalEntry"} {
		t.Run(id, func(t *testing.T) {
			if ops[id].AgentAllowed {
				t.Errorf("%s: open to agents, want x-agent: false", id)
			}
			if _, err := apitools.Tools(&apitools.Client{}, nil, id); err == nil || !strings.Contains(err.Error(), "not open to agents") {
				t.Errorf("%s as a tool: got %v, want it refused", id, err)
			}
		})
	}
}

// TestExplainRefusals: a registration the agent may not read or that a
// code names twice, a draft the API refuses, a session it may not start,
// an operation the contract does not have, and a model that fails stop
// the run, the last two after the session started.
func TestExplainRefusals(t *testing.T) {
	model := llmtest.New(t)
	twice := `{"id": "a", "code": "GSTHST"}, {"id": "b", "code": "GSTHST"}`
	fake := func(a *fakeAPI) func(*testing.T) *apitools.Client {
		return func(t *testing.T) *apitools.Client {
			t.Helper()
			return client(t, a)
		}
	}
	gone := func(*testing.T) *apitools.Client { return &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"} }
	noDraft := func(t *testing.T) *apitools.Client {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/tax-registrations" {
				_, _ = w.Write([]byte(`{"items": [{"id": "` + reg + `", "code": "GSTHST"}]}`)) // a test's answer
				return
			}
			_, _ = w.Write([]byte(`[]`)) // a test's answer
		}))
		t.Cleanup(srv.Close)
		return &apitools.Client{BaseURL: srv.URL, Token: "t"}
	}
	for name, c := range map[string]struct {
		api      func(*testing.T) *apitools.Client
		reg      string
		want     string
		sessions bool
	}{
		"another registration":     {fake(&fakeAPI{}), "VAT", "no tax registration VAT", false},
		"a code of two":            {fake(&fakeAPI{regs: twice}), "GSTHST", "give the registration's id", false},
		"a draft refused":          {fake(&fakeAPI{draft: 409}), "GSTHST", "tax.period_not_ended", false},
		"a session not started":    {fake(&fakeAPI{start: 403}), "GSTHST", "identity.permission_missing", false},
		"the model's failure":      {fake(&fakeAPI{}), "GSTHST", "500", true},
		"a draft that is no draft": {noDraft, "GSTHST", "draft return", false},
		"the registrations' list":  {gone, "GSTHST", "127.0.0.1:1", false},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&taxagent.Agent{LLM: model.Runner(), API: c.api(t)}).Explain(context.Background(), c.reg, "2026-04-01")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
			if got := res.Session != ""; got != c.sessions {
				t.Errorf("a session: got %v, want %v", got, c.sessions)
			}
		})
	}
	tools := taxagent.Tools
	taxagent.Tools = append(slices.Clone(tools), "noSuchOperation")
	t.Cleanup(func() { taxagent.Tools = tools })
	a := &fakeAPI{}
	res, err := (&taxagent.Agent{LLM: model.Runner(), API: client(t, a)}).Explain(context.Background(), "GSTHST", "2026-04-01")
	if err == nil || !strings.Contains(err.Error(), "noSuchOperation") {
		t.Errorf("an operation not in the contract: got %v, want it named", err)
	}
	if res.Session != session {
		t.Errorf("session: got %q, want the one started", res.Session)
	}
	if !slices.Contains(a.calls, "POST /v1/agent-sessions/"+session+":end") {
		t.Errorf("the session was not ended: %v", a.calls)
	}
}
