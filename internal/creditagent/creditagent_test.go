package creditagent_test

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
	"time"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/creditagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

const (
	session  = "01a1aa00-0000-7000-8000-000000000a01"
	le       = "01a1aa00-0000-7000-8000-000000000b01"
	customer = "01a1aa00-0000-7000-8000-000000000c02"
	limit    = "01a1aa00-0000-7000-8000-000000000c01"
	stranger = "01a1aa00-0000-7000-8000-000000000c09"
)

// fakeAPI is a stand-in for the cell's API: company 3000, the exposure
// of its customer 100042 (1470.00 CAD against a limit of 2470.00, or
// without one), the customer's invoices and payments beside another
// customer's, a session, and recommendations answered by their bodies;
// it keeps every request it was sent.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []string
	noLimit  bool
	exposure int  // the exposure's status; 0: 200
	invoices int  // the invoices' status; 0: 200
	draft    bool // the customer's one invoice was never posted
	made     int
}

// status is the status of the customer's one invoice.
func (a *fakeAPI) status() string {
	if a.draft {
		return "draft"
	}
	return "posted"
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
	case path == "/v1/legal-entities":
		answer(200, `{"items": [{"id": "`+le+`", "code": "3000"}]}`)
	case path == "/v1/credit-exposure" && a.exposure == 200:
		answer(200, `[]`)
	case path == "/v1/credit-exposure" && a.exposure != 0:
		answer(a.exposure, `{"code": "request.not_found", "status": 404}`)
	case path == "/v1/credit-exposure" && a.noLimit:
		answer(200, `{"company_code": "3000", "customer_id": "`+customer+`", "customer_number": "100042", "customer_name": "Northwind Traders",
			"currency": "CAD", "exposure": {"amount": "1470.00", "currency": "CAD"}, "has_limit": false}`)
	case path == "/v1/credit-exposure":
		answer(200, `{"company_code": "3000", "customer_id": "`+customer+`", "customer_number": "100042", "customer_name": "Northwind Traders",
			"currency": "CAD", "exposure": {"amount": "1470.00", "currency": "CAD"}, "has_limit": true, "credit_limit_id": "`+limit+`",
			"limit": {"amount": "2470.00", "currency": "CAD"}, "available": {"amount": "1000.00", "currency": "CAD"}}`)
	case path == "/v1/customer-invoices" && a.invoices != 0:
		answer(a.invoices, `{"code": "identity.permission_missing", "status": 403}`)
	case path == "/v1/customer-invoices":
		answer(200, `{"items": [{"customer_id": "`+stranger+`", "status": "posted", "gross_amount": {"amount": "9000.00", "currency": "CAD"}},
			{"customer_id": "`+customer+`", "status": "`+a.status()+`", "gross_amount": {"amount": "3000.00", "currency": "CAD"}}]}`)
	case path == "/v1/incoming-payments":
		answer(200, `{"items": [{"customer_id": "`+stranger+`", "amount": {"amount": "50.00", "currency": "CAD"}}]}`)
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

// rec is a recommendation of kind with the content on the customer's
// limit, or on the object given, in the session the conversation names.
func rec(kind, content string, object ...string) func(string) any {
	return func(conversation string) any {
		body := map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "credit_limit", "object_id": limit,
			"kind": kind, "content": content}
		if len(object) == 2 {
			body["object_type"], body["object_id"] = object[0], object[1]
		}
		return map[string]any{"body": body}
	}
}

var (
	exposureRead = llmtest.Tool("getCreditExposure", map[string]any{"legal_entity_id": le, "customer_id": customer})
	invoicesRead = llmtest.Tool("listCustomerInvoices", map[string]any{"customer_id": customer})
	paymentsRead = llmtest.Tool("listIncomingPayments", map[string]any{"customer_id": customer})
)

// TestScore: the agent reviews the limit of customer 100042 in company
// 3000 in one session and recommends on it once. Its check refuses, and
// counts, a content that gives a score, writes a number the run did not
// read (an amount, in whatever form, or a count of the agent's own), does
// not start with the limit recommended in the company's
// currency, or whose limit does not fit its kind; nothing refused
// reaches the API.
func TestScore(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t, exposureRead, invoicesRead, paymentsRead,
		llmtest.ToolFn("createRecommendation", rec("lower_limit", "Limit 1470.00 CAD: the customer scores 3 of 10.")),
		llmtest.ToolFn("createRecommendation", rec("lower_limit", "Limit 1500.00 CAD: a round amount above the exposure of 1470.00 CAD.")),
		llmtest.ToolFn("createRecommendation", rec("lower_limit", "Limit 1470.00 CAD: it owes that and about $1600, on 3 invoices.")),
		llmtest.ToolFn("createRecommendation", rec("lower_limit", "Limit 2026 CAD: the year of today's date, which the run was given.")),
		llmtest.ToolFn("createRecommendation", rec("lower_limit", "Limit 100042 CAD: the customer's number.")),
		llmtest.ToolFn("createRecommendation", rec("lower_limit", "Limit 9000.00 CAD: above the limit of 2470.00, so no lower limit.")),
		llmtest.ToolFn("createRecommendation", rec("lower_limit", "Lower the limit to 1470.00 CAD.")),
		llmtest.ToolFn("createRecommendation", rec("lower_limit", "Limit 1470.00 USD.")),
		llmtest.ToolFn("createRecommendation", rec("lower_limit", "Limit, as it is.")),
		llmtest.ToolFn("createRecommendation", rec("raise_limit", "Limit 1470.00 CAD.")),
		llmtest.ToolFn("createRecommendation", rec("raise_limit", "Limit 2470.00 CAD.")),
		llmtest.ToolFn("createRecommendation", rec("keep_limit", "Limit 1470.00 CAD.")),
		llmtest.ToolFn("createRecommendation", rec("set_limit", "Limit 1470.00 CAD.")),
		llmtest.ToolFn("createRecommendation", rec("lower_limit", "Limit 1470.00 CAD: the exposure, against a limit of 2,470.00 CAD with 1000.00 CAD available; its one invoice is 3000.00 CAD. It is given no score: one invoice, no payments.")),
		llmtest.Final("Recommended lowering the limit to the exposure."))
	now := func() time.Time { return time.Date(2026, 11, 12, 23, 30, 0, 0, time.FixedZone("EST", -5*3600)) }
	res, err := (&creditagent.Agent{LLM: model.Runner(), API: client(t, a), Now: now}).Score(context.Background(), "3000", customer)
	if err != nil {
		t.Fatal(err)
	}
	want := []recommend.Made{{ID: "r1", ObjectType: "credit_limit", ObjectID: limit, Kind: "lower_limit"}}
	if !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 13 {
		t.Errorf("refused: got %d, want 13: the check's twelve and the pattern's one (set_limit on a limit)", res.Refused)
	}
	if got := len(a.sent("recommendations")); got != 1 {
		t.Errorf("recommendations sent: got %d, want 1: the check keeps the others from the API", got)
	}
	prompt := "Customer 100042 Northwind Traders (business partner " + customer + ") in company 3000 (legal entity " + le + "), whose currency is CAD. It has a credit limit, " + limit + ". Today is 2026-11-13 (UTC)."
	if !strings.Contains(model.Requests[0], prompt) {
		t.Errorf("prompt: got %s, want %q", model.Requests[0], prompt)
	}
	refusals := strings.Join(model.Requests, "\n")
	for _, want := range []error{creditagent.ErrScore, creditagent.ErrUnread, creditagent.ErrNoLimit} {
		// The model reads the refusal as JSON text.
		told, _ := json.Marshal(want.Error())
		if !strings.Contains(refusals, strings.Trim(string(told), `"`)) {
			t.Errorf("the model was not told %q", want)
		}
	}
	for _, want := range []string{"not 1500.00", "not 1600, 3", "the limit 2026 is no amount a tool returned", "the limit 100042 is no amount"} {
		if !strings.Contains(refusals, want) {
			t.Errorf("the refusal of numbers the run did not read: want %q", want)
		}
	}
}

// TestScoreWithoutHistory: a customer whose posted invoices and payments
// the run did not read (none read, a read refused, only other customers'
// in the answers, an invoice never posted) gets keep_limit or nothing:
// another limit is refused.
func TestScoreWithoutHistory(t *testing.T) {
	for name, c := range map[string]struct {
		api   *fakeAPI
		reads []llmtest.Step
	}{
		"nothing read":                   {&fakeAPI{}, []llmtest.Step{exposureRead}},
		"only another customer's":        {&fakeAPI{}, []llmtest.Step{exposureRead, paymentsRead}},
		"the invoices refused, no pages": {&fakeAPI{invoices: 403}, []llmtest.Step{exposureRead, invoicesRead, llmtest.Tool("getBusinessPartner", map[string]any{"id": customer})}},
		"an invoice never posted":        {&fakeAPI{draft: true}, []llmtest.Step{exposureRead, invoicesRead}},
	} {
		t.Run(name, func(t *testing.T) {
			a, reads := c.api, c.reads
			steps := append(slices.Clone(reads),
				llmtest.ToolFn("createRecommendation", rec("lower_limit", "Limit 1470.00 CAD: the exposure.")),
				llmtest.ToolFn("createRecommendation", rec("keep_limit", "Limit 2470.00 CAD: no invoice or payment of the customer was found; it has no history.")),
				llmtest.Final("No history: the limit is kept."))
			res, err := (&creditagent.Agent{LLM: llmtest.New(t, steps...).Runner(), API: client(t, a), MaxIterations: 8}).Score(context.Background(), "3000", customer)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Recommendations) != 1 || res.Recommendations[0].Kind != "keep_limit" {
				t.Errorf("recommendations: got %v, want one keep_limit", res.Recommendations)
			}
			if res.Refused != 1 {
				t.Errorf("refused: got %d, want 1: the lower limit", res.Refused)
			}
		})
	}
}

// TestScoreWithoutALimit: a customer without a limit is the object, and
// set_limit the one kind: with its history read a limit is recommended;
// keeping a limit it does not have is the pattern's to refuse, and a
// limit without history the check's.
func TestScoreWithoutALimit(t *testing.T) {
	onCustomer := []string{"business_partner", customer}
	a := &fakeAPI{noLimit: true}
	model := llmtest.New(t, exposureRead,
		llmtest.ToolFn("createRecommendation", rec("set_limit", "Limit 1470.00 CAD: what it owes.", onCustomer...)),
		llmtest.ToolFn("createRecommendation", rec("keep_limit", "Limit 1470.00 CAD.", onCustomer...)),
		llmtest.ToolFn("createRecommendation", rec("set_limit", "Limit 1470.00 CAD.")),
		invoicesRead,
		llmtest.ToolFn("createRecommendation", rec("set_limit", "Limit 3000.00 CAD: its largest invoice; it owes 1470.00 CAD.", onCustomer...)),
		llmtest.Final("Recommended a first limit."))
	res, err := (&creditagent.Agent{LLM: model.Runner(), API: client(t, a)}).Score(context.Background(), "3000", customer)
	if err != nil {
		t.Fatal(err)
	}
	want := []recommend.Made{{ID: "r1", ObjectType: "business_partner", ObjectID: customer, Kind: "set_limit"}}
	if !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 3 {
		t.Errorf("refused: got %d, want 3: no history, keep_limit on a customer, the limit it does not have", res.Refused)
	}
	if !strings.Contains(model.Requests[0], "It has no credit limit.") {
		t.Errorf("prompt: got %s, want it to say the customer has no limit", model.Requests[0])
	}
}

// TestNever: the agent's tools are reads and the one recommendation; none
// is an operation of its never list, and the contract closes those to
// agents (ADR-0072, T105a).
func TestNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	tools := creditagent.Tools()
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
	for _, id := range creditagent.Never {
		t.Run(id, func(t *testing.T) {
			if slices.Contains(tools, id) {
				t.Errorf("%s is among the agent's tools", id)
			}
			if op, ok := ops[id]; !ok || op.AgentAllowed {
				t.Errorf("%s: got in the contract %v, open to agents %v; want x-agent: false", id, ok, op.AgentAllowed)
			}
			if _, err := apitools.Tools(&apitools.Client{}, nil, id); err == nil || !strings.Contains(err.Error(), "not open to agents") {
				t.Errorf("%s as a tool: got %v, want it refused", id, err)
			}
		})
	}
}

// TestScoreRefusals: a company the agent may not read, an exposure the
// API refuses or that is none, and an API that is gone stop the run
// before a session starts.
func TestScoreRefusals(t *testing.T) {
	model := llmtest.New(t)
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		api     *apitools.Client
		company string
		want    string
	}{
		"another company":       {client(t, &fakeAPI{}), "9999", "no company 9999"},
		"an exposure refused":   {client(t, &fakeAPI{exposure: 404}), "3000", "request.not_found"},
		"an exposure that is 0": {client(t, &fakeAPI{exposure: 200}), "3000", "the credit exposure of customer " + customer + " in company 3000"},
		"the API gone":          {gone, "3000", "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&creditagent.Agent{LLM: model.Runner(), API: c.api}).Score(context.Background(), c.company, customer)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
			if res.Session != "" {
				t.Errorf("a session: got %s, want none", res.Session)
			}
		})
	}
}
