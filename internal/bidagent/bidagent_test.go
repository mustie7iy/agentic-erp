package bidagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/bidagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

const (
	session    = "01a1aa00-0000-7000-8000-000000000a01"
	rfq        = "01a1aa00-0000-7000-8000-000000000b01"
	cheaper    = "01a1aa00-0000-7000-8000-000000000c01"
	dearer     = "01a1aa00-0000-7000-8000-000000000c02"
	incomplete = "01a1aa00-0000-7000-8000-000000000c03"
	expired    = "01a1aa00-0000-7000-8000-000000000c04"
	unknown    = "01a1aa00-0000-7000-8000-000000000c09"
)

// ranked are the comparison's rows of the two ranked quotations, then the
// incomplete one, the expired one and a supplier that did not answer.
const ranked = `{"supplier_number": "BP90000002", "supplier_name": "Cabot Components", "answered": true, "rank": 1,
	"supplier_quotation_id": "` + cheaper + `", "complete": true, "expired": false, "total": {"amount": "28400.00", "currency": "CAD"},
	"prices": [{"rfq_item_no": 1, "amount": {"amount": "17000.00", "currency": "CAD"}, "lowest": true},
	{"rfq_item_no": 2, "amount": {"amount": "11400.00", "currency": "CAD"}, "lowest": false}]},
	{"supplier_number": "BP90000001", "supplier_name": "Tyrell Office Products", "answered": true, "rank": 2,
	"supplier_quotation_id": "` + dearer + `", "complete": true, "expired": false, "total": {"amount": "29760.00", "currency": "CAD"},
	"prices": [{"rfq_item_no": 1, "amount": {"amount": "18000.00", "currency": "CAD"}, "lowest": false},
	{"rfq_item_no": 2, "amount": {"amount": "11000.00", "currency": "CAD"}, "lowest": true}]},`

// unranked are the rows no comparison ranks.
const unranked = `{"supplier_number": "BP90000003", "supplier_name": "Initech", "answered": true, "rank": null,
	"supplier_quotation_id": "` + incomplete + `", "complete": false, "expired": false, "unquoted_item_nos": [2],
	"total": {"amount": "15000.00", "currency": "CAD"}},
	{"supplier_number": "BP90000004", "supplier_name": "Umbrella Supplies", "answered": true,
	"supplier_quotation_id": "` + expired + `", "complete": true, "expired": true, "total": {"amount": "20000.00", "currency": "CAD"}},
	{"supplier_number": "BP90000005", "supplier_name": "Soylent Office", "answered": false, "complete": false, "expired": false}`

// fakeAPI is a stand-in for the cell's API: the comparison of RFQ
// RQ100004 in company 3000, with ranked quotations or none, a session,
// and recommendations answered by their bodies; it keeps every request it
// was sent.
type fakeAPI struct {
	mu         sync.Mutex
	calls      []string
	status     string // the RFQ's status; "": published
	noneRanked bool
	comparison int // the comparison's status; 0: 200
	made       int
}

func (a *fakeAPI) body() string {
	status, rows := a.status, ranked+unranked
	if status == "" {
		status = "published"
	}
	if a.noneRanked {
		rows = unranked
	}
	return `{"rfq_id": "` + rfq + `", "rfq_document_number": "RQ100004", "company_code": "3000", "company_name": "Maple Ltd",
		"currency": "CAD", "status": "` + status + `", "as_of": "2026-11-13", "ranked_count": 2, "suppliers": [` + rows + `]}`
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
	case path == "/v1/rfqs/"+rfq+"/comparison" && a.comparison == 200:
		answer(200, `{}`)
	case path == "/v1/rfqs/"+rfq+"/comparison" && a.comparison != 0:
		answer(a.comparison, `{"code": "request.not_found", "status": 404}`)
	case path == "/v1/rfqs/"+rfq+"/comparison":
		answer(200, a.body())
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

// rec is a recommendation of kind with the content on the RFQ, in the
// session the conversation names.
func rec(kind, content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "rfq", "object_id": rfq,
			"kind": kind, "content": content}}
	})
}

var comparisonRead = llmtest.Tool("getRfqComparison", map[string]any{"id": rfq})

// refusedWith reports whether the model was told the refusal.
func refusedWith(t *testing.T, requests []string, want error) {
	t.Helper()
	told, _ := json.Marshal(want.Error()) // the model reads the refusal as JSON text
	if !strings.Contains(strings.Join(requests, "\n"), strings.Trim(string(told), `"`)) {
		t.Errorf("the model was not told %q", want)
	}
}

// TestCompare: the agent compares RFQ RQ100004's quotations in one session
// and recommends on it once. Its check refuses, and counts, an award of a
// quotation the comparison does not rank (incomplete, expired, unknown),
// one not named on the first line as the form asks, and a content writing
// a number the run did not read (a difference it computed); nothing
// refused reaches the API.
func TestCompare(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t, comparisonRead,
		rec("award", "Award: "+incomplete+"\nInitech's total of 15000.00 CAD is the lowest."),
		rec("award", "Award: "+expired+"\nUmbrella's 20000.00 CAD."),
		rec("award", "Award: "+unknown+"\nA quotation of another RFQ."),
		rec("award", "Cabot Components' quotation, "+cheaper+", is the cheapest."),
		rec("award", "Award: "+cheaper+"\nCabot Components' 28400.00 CAD is 1360.00 CAD below Tyrell's 29760.00 CAD."),
		rec("award", "Award: "+cheaper+"\nCabot Components (BP90000002) quotes 28400.00 CAD, ranked first; Tyrell Office Products 29760.00 CAD. "+
			"For item 2 Tyrell quoted the lower amount, 11000.00 CAD against 11400.00 CAD. Left out: Initech's leaves item 2 unquoted, "+
			"Umbrella Supplies' expired; Soylent Office did not answer."),
		llmtest.Final("Recommended awarding Cabot Components' quotation; the buyer decides."))
	res, err := (&bidagent.Agent{LLM: model.Runner(), API: client(t, a)}).Compare(context.Background(), rfq)
	if err != nil {
		t.Fatal(err)
	}
	want := []recommend.Made{{ID: "r1", ObjectType: "rfq", ObjectID: rfq, Kind: "award"}}
	if !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 5 {
		t.Errorf("refused: got %d, want 5", res.Refused)
	}
	if got := len(a.sent("recommendations")); got != 1 {
		t.Errorf("recommendations sent: got %d, want 1: the check keeps the others from the API", got)
	}
	prompt := "RFQ RQ100004 (id " + rfq + ") of company 3000 Maple Ltd, in CAD, published; the company's date today is 2026-11-13."
	if !strings.Contains(model.Requests[0], prompt) {
		t.Errorf("prompt: got %s, want %q", model.Requests[0], prompt)
	}
	refusedWith(t, model.Requests, bidagent.ErrNotRanked)
	refusedWith(t, model.Requests, bidagent.ErrUnread)
	if !strings.Contains(strings.Join(model.Requests, "\n"), "not 1360.00") {
		t.Error("the refusal of the difference computed: want it named")
	}
}

// TestCompareWithoutRanked: an RFQ none of whose quotations the comparison
// ranks gets no_award: any award is refused.
func TestCompareWithoutRanked(t *testing.T) {
	a := &fakeAPI{noneRanked: true}
	model := llmtest.New(t, comparisonRead,
		rec("award", "Award: "+incomplete+"\nThe only quotation priced, 15000.00 CAD."),
		rec("no_award", "No award: Initech's quotation leaves item 2 unquoted and Umbrella Supplies' expired; Soylent Office did not answer."),
		llmtest.Final("No quotation is complete and valid: no award."))
	res, err := (&bidagent.Agent{LLM: model.Runner(), API: client(t, a)}).Compare(context.Background(), rfq)
	if err != nil {
		t.Fatal(err)
	}
	if want := []recommend.Made{{ID: "r1", ObjectType: "rfq", ObjectID: rfq, Kind: "no_award"}}; !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 1 {
		t.Errorf("refused: got %d, want 1: the award", res.Refused)
	}
}

// TestCompareNotPublished: an RFQ already awarded is recommended for no
// award: an award of a quotation it ranks is refused.
func TestCompareNotPublished(t *testing.T) {
	a := &fakeAPI{status: "awarded"}
	model := llmtest.New(t, comparisonRead,
		rec("award", "Award: "+cheaper+"\nCabot Components, 28400.00 CAD."),
		rec("no_award", "The RFQ is awarded already."),
		llmtest.Final("The RFQ is awarded already."))
	res, err := (&bidagent.Agent{LLM: model.Runner(), API: client(t, a)}).Compare(context.Background(), rfq)
	if err != nil {
		t.Fatal(err)
	}
	if want := []recommend.Made{{ID: "r1", ObjectType: "rfq", ObjectID: rfq, Kind: "no_award"}}; !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 1 {
		t.Errorf("refused: got %d, want 1: the award", res.Refused)
	}
	refusedWith(t, model.Requests, bidagent.ErrNotPublished)
}

// TestNever: the agent's tools are reads and the one recommendation; none
// is an operation of its never list, and the contract closes the award
// to agents (ADR-0072, T105c).
func TestNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	tools := bidagent.Tools()
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
	// Every write of an RFQ and its quotations is on the list.
	for id, op := range ops {
		if op.Method != http.MethodGet && (strings.Contains(id, "Rfq") || strings.Contains(id, "SupplierQuotation")) && !slices.Contains(bidagent.Never, id) {
			t.Errorf("never list: got %v, want it to hold %s", bidagent.Never, id)
		}
	}
	for _, id := range bidagent.Never {
		t.Run(id, func(t *testing.T) {
			if slices.Contains(tools, id) {
				t.Errorf("%s is among the agent's tools", id)
			}
			if _, ok := ops[id]; !ok {
				t.Errorf("%s: not in the contract", id)
			}
		})
	}
	if ops["awardRfq"].AgentAllowed {
		t.Error("awardRfq: open to agents, want x-agent: false")
	}
}

// TestCompareRefusals: a comparison the API refuses or that names no RFQ,
// and an API that is gone stop the run before a session starts.
func TestCompareRefusals(t *testing.T) {
	model := llmtest.New(t)
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		api  *apitools.Client
		want string
	}{
		"a comparison refused": {client(t, &fakeAPI{comparison: 404}), "request.not_found"},
		"a comparison of none": {client(t, &fakeAPI{comparison: 200}), "the comparison of RFQ " + rfq + ": no RFQ in the answer"},
		"the API gone":         {gone, "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&bidagent.Agent{LLM: model.Runner(), API: c.api}).Compare(context.Background(), rfq)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
			if res.Session != "" {
				t.Errorf("a session: got %s, want none", res.Session)
			}
		})
	}
}
