package collectionsagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/collectionsagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

const (
	session  = "01a1aa00-0000-7000-8000-000000000a01"
	le       = "01a1aa00-0000-7000-8000-000000000b01"
	disputed = "01a1aa00-0000-7000-8000-000000000c01" // every overdue item disputed
	partly   = "01a1aa00-0000-7000-8000-000000000c02" // one overdue item disputed, one not
	plain    = "01a1aa00-0000-7000-8000-000000000c03" // no dispute case
	entryA   = "01a1aa00-0000-7000-8000-000000000e01"
	entryB   = "01a1aa00-0000-7000-8000-000000000e02"
	ar       = "01a1aa00-0000-7000-8000-000000000a12" // the customer reconciliation account
	ap       = "01a1aa00-0000-7000-8000-000000000a21" // a supplier reconciliation account
	keyDate  = "2026-11-15"
)

// fakeAPI is a stand-in for the cell's API: company 3000 and its
// worklist on the key date of three customers (Northwind, every overdue
// item disputed; Tyrell, one of its two overdue items disputed; Acme,
// without a case), their open dispute cases and open items, a session and
// recommendations answered by their bodies; it keeps every request it was
// sent.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []string
	worklist string // the worklist's body; "": the three customers
	status   map[string]int
	made     int
}

const threeCustomers = `{"company_code": "3000", "currency": "CAD", "customer_count": 3, "items": [
	{"customer_id": "` + partly + `", "customer_number": "100043", "customer_name": "Tyrell", "overdue_amount": {"amount": "900.00", "currency": "CAD"}, "open_dispute_cases": 1},
	{"customer_id": "` + disputed + `", "customer_number": "100042", "customer_name": "Northwind", "overdue_amount": {"amount": "400.00", "currency": "CAD"}, "open_dispute_cases": 1},
	{"customer_id": "` + plain + `", "customer_number": "100044", "customer_name": "Acme", "overdue_amount": {"amount": "250.00", "currency": "CAD"}, "open_dispute_cases": 0}]}`

// The open items: Northwind's overdue invoice (disputed), a credit, an
// invoice not yet due and, Northwind supplying too, a down payment made to
// it on a supplier account, which the worklist does not count; Tyrell's disputed invoice and one posted without
// terms, overdue since its posting date.
var openItems = map[string]string{
	disputed: `{"items": [
		{"fiscal_year": 2026, "journal_entry_id": "` + entryA + `", "line_no": 1, "account_id": "` + ar + `", "posting_date": "2026-10-01", "amount_lc": {"amount": "400.00"}, "payment_terms": {"net_due_date": "2026-10-31"}},
		{"fiscal_year": 2026, "journal_entry_id": "` + entryB + `", "line_no": 2, "account_id": "` + ar + `", "posting_date": "2026-09-01", "amount_lc": {"amount": "-420.00"}},
		{"fiscal_year": 2026, "journal_entry_id": "` + entryB + `", "line_no": 1, "account_id": "` + ar + `", "posting_date": "2026-11-01", "amount_lc": {"amount": "100.00"}, "payment_terms": {"net_due_date": "2026-12-01"}},
		{"fiscal_year": 2026, "journal_entry_id": "` + entryB + `", "line_no": 4, "account_id": "` + ap + `", "posting_date": "2026-09-15", "amount_lc": {"amount": "500.00"}, "special_gl": "A"}]}`,
	partly: `{"items": [
		{"fiscal_year": 2026, "journal_entry_id": "` + entryA + `", "line_no": 3, "account_id": "` + ar + `", "posting_date": "2026-10-01", "amount_lc": {"amount": "300.00"}, "payment_terms": {"net_due_date": "2026-10-31"}},
		{"fiscal_year": 2026, "journal_entry_id": "` + entryB + `", "line_no": 3, "account_id": "` + ar + `", "posting_date": "2026-10-01", "amount_lc": {"amount": "600.00"}}]}`,
}

// The open dispute cases: Northwind's over two invoices (T86c), its own
// line the one not yet due, so only its second item disputes the overdue
// invoice; Tyrell's over one of its two overdue invoices.
var cases = map[string]string{
	disputed: `{"items": [{"fiscal_year": 2026, "journal_entry_id": "` + entryB + `", "line_no": 1, "items": [
		{"fiscal_year": 2026, "journal_entry_id": "` + entryB + `", "line_no": 1}, {"fiscal_year": 2026, "journal_entry_id": "` + entryA + `", "line_no": 1}]}]}`,
	partly: `{"items": [{"fiscal_year": 2026, "journal_entry_id": "` + entryA + `", "line_no": 3, "items": [
		{"fiscal_year": 2026, "journal_entry_id": "` + entryA + `", "line_no": 3}]}]}`,
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
	if s := a.status[r.URL.Path]; s != 0 {
		answer(s, `{"code": "identity.permission_missing", "status": 403}`)
		return
	}
	q := r.URL.Query()
	switch path := r.URL.Path; path {
	case "/v1/legal-entities":
		answer(200, `{"items": [{"id": "`+le+`", "code": "3000"}]}`)
	case "/v1/legal-entities/" + le + "/today":
		answer(200, `{"legal_entity_id": "`+le+`", "date": "2026-11-15", "time_zone": "Asia/Dubai"}`)
	case "/v1/accounts":
		answer(200, `{"items": [{"id": "`+ar+`", "reconciliation_kind": "customer"}, {"id": "`+ap+`", "reconciliation_kind": "supplier"}]}`)
	case "/v1/collection-worklist":
		body := a.worklist
		if body == "" {
			body = threeCustomers
		}
		answer(200, body)
	case "/v1/dispute-cases":
		answer(200, cases[q.Get("customer_id")])
	case "/v1/open-items":
		answer(200, openItems[q.Get("partner_id")])
	case "/v1/agent-sessions":
		answer(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case "/v1/recommendations":
		var body struct {
			ObjectID   string `json:"object_id"`
			ObjectType string `json:"object_type"`
			Kind       string
			Content    string
		}
		_ = json.Unmarshal(b, &body) // the agent's own JSON
		a.mu.Lock()
		a.made++
		id := fmt.Sprintf("r%d", a.made)
		a.mu.Unlock()
		content, _ := json.Marshal(body.Content) // a string encodes
		answer(201, `{"id": "`+id+`", "object_id": "`+body.ObjectID+`", "object_type": "`+body.ObjectType+`", "kind": "`+body.Kind+`", "content": `+string(content)+`}`)
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

// rec is a recommendation of kind with the content on the customer, in
// the session the conversation names.
func rec(customer, kind, content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "business_partner",
			"object_id": customer, "kind": kind, "content": content}}
	})
}

var worklistRead = llmtest.Tool("getCollectionWorklist", map[string]any{"legal_entity_id": le, "key_date": keyDate})

// TestAdvise: the agent recommends once on each customer of the worklist
// in one session. Its check refuses, and counts, a contact of a customer
// whose overdue items are all disputed, wait_for_dispute without a case,
// a content that does not start with the overdue amount, a number the run
// did not read (in the reasons or the draft message) and a contact without
// its draft; a customer with an undisputed overdue item beside a disputed
// one may be reminded; nothing refused reaches the API.
func TestAdvise(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t, worklistRead,
		rec(disputed, "send_reminder", "Overdue 400.00 CAD.\nDraft message: please pay 400.00 CAD."),
		rec(plain, "wait_for_dispute", "Overdue 250.00 CAD, nothing disputed."),
		rec(plain, "send_reminder", "Acme owes 250.00 CAD.\nDraft message: please pay."),
		rec(plain, "send_reminder", "Overdue 250.00 CAD on 17 invoices.\nDraft message: please pay."),
		rec(plain, "send_reminder", "Overdue 250.00 CAD, never dunned."),
		rec(plain, "send_reminder", "Overdue 250.00 CAD, never dunned.\nDraft message: please pay 260.00 CAD with the fee."),
		rec(plain, "send_reminder", "Overdue 250.00 CAD, never dunned.\n  Draft message: Dear Acme, 250.00 CAD is overdue; please pay it."),
		rec(disputed, "wait_for_dispute", "Overdue 400.00 CAD, all of it disputed in an open case."),
		rec(partly, "send_reminder", "Overdue 900.00 CAD, part of it disputed.\nDraft message: Dear Tyrell, please pay the items not in dispute."),
		rec(plain, "call", "Overdue 250.00 CAD.\nDraft message: we will call."),
		llmtest.Final("Recommended on each customer."))
	now := func() time.Time { return time.Date(2026, 11, 14, 23, 30, 0, 0, time.FixedZone("EST", -5*3600)) }
	res, err := (&collectionsagent.Agent{LLM: model.Runner(), API: client(t, a), Now: now}).Advise(context.Background(), "3000", "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range res.Recommendations {
		got = append(got, m.ObjectID+" "+m.Kind)
	}
	if want := []string{plain + " send_reminder", disputed + " wait_for_dispute", partly + " send_reminder"}; !slices.Equal(got, want) {
		t.Errorf("recommendations: got %v, want %v", got, want)
	}
	if res.Refused != 7 {
		t.Errorf("refused: got %d, want 7: the check's six and the pattern's one (a second on Acme)", res.Refused)
	}
	if got := len(a.sent("recommendations")); got != 3 {
		t.Errorf("recommendations sent: got %d, want 3: the check keeps the others from the API", got)
	}
	if got, want := res.Names[disputed], "100042 Northwind"; got != want {
		t.Errorf("names: got %q, want %q", got, want)
	}
	for _, want := range []string{"the key date is " + keyDate, "The worklist lists 3 customers; you recommend on the first 3 customers",
		"Customer 100042 Northwind (business partner " + disputed + "): every overdue item of it is disputed in an open case.",
		"Customer 100043 Tyrell (business partner " + partly + "): some of its items are disputed in open cases, not every overdue one.",
		"Customer 100044 Acme (business partner " + plain + "): no open dispute case."} {
		if !strings.Contains(model.Requests[0], want) {
			t.Errorf("prompt: got %s, want it to hold %q", model.Requests[0], want)
		}
	}
	refusals := strings.Join(model.Requests, "\n")
	for _, want := range []error{collectionsagent.ErrDisputed, collectionsagent.ErrNoDispute, collectionsagent.ErrNoOverdue, collectionsagent.ErrUnread, collectionsagent.ErrNoDraft} {
		told, _ := json.Marshal(want.Error()) // the model reads the refusal as JSON text
		if !strings.Contains(refusals, strings.Trim(string(told), `"`)) {
			t.Errorf("the model was not told %q", want)
		}
	}
	for _, want := range []string{"not 17", "not 260.00"} {
		if !strings.Contains(refusals, want) {
			t.Errorf("the refusal of numbers the run did not read: want %q", want)
		}
	}
}

// TestAdviseOverdueRead: the overdue amount a content starts with is one
// a tool of the run returned as money: one standing only in the prompt
// (a customer numbered as its whole amount) is refused until the
// worklist is read.
func TestAdviseOverdueRead(t *testing.T) {
	a := &fakeAPI{worklist: `{"company_code": "3000", "currency": "JPY", "customer_count": 1, "items": [
		{"customer_id": "` + plain + `", "customer_number": "4000", "customer_name": "Acme", "overdue_amount": {"amount": "4000", "currency": "JPY"}, "open_dispute_cases": 0}]}`}
	model := llmtest.New(t,
		rec(plain, "no_action", "Overdue 4000 JPY, customer 4000."),
		llmtest.Tool("getCollectionWorklist", map[string]any{"legal_entity_id": le, "key_date": keyDate}),
		rec(plain, "no_action", "Overdue 4000 JPY, customer 4000; it is one customer."),
		llmtest.Final("Done."))
	res, err := (&collectionsagent.Agent{LLM: model.Runner(), API: client(t, a)}).Advise(context.Background(), "3000", keyDate)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recommendations) != 1 || res.Refused != 1 {
		t.Errorf("got %d recommendations, %d refused; want 1 and 1", len(res.Recommendations), res.Refused)
	}
	if !strings.Contains(strings.Join(model.Requests, "\n"), "the overdue amount 4000 is no amount a tool of the run returned") {
		t.Error("the refusal does not name the overdue amount")
	}
	if !strings.Contains(model.Requests[0], "The worklist lists one customer") {
		t.Errorf("prompt: got %s, want one customer in words", model.Requests[0])
	}
}

// TestAdviseNothingOverdue: a worklist without customers starts no
// session and says so.
func TestAdviseNothingOverdue(t *testing.T) {
	a := &fakeAPI{worklist: `{"company_code": "3000", "currency": "CAD", "customer_count": 0, "items": []}`}
	res, err := (&collectionsagent.Agent{LLM: llmtest.New(t).Runner(), API: client(t, a)}).Advise(context.Background(), "3000", keyDate)
	if err != nil {
		t.Fatal(err)
	}
	if res.Session != "" || res.Reply != "Nothing of company 3000 is overdue on "+keyDate+": no customer to recommend on." {
		t.Errorf("got session %q, reply %q; want none and the reply", res.Session, res.Reply)
	}
	if got := a.sent("agent-sessions"); len(got) != 0 {
		t.Errorf("sessions: got %v, want none", got)
	}
}

// TestAdviseRefusals: a key date that is none, a company the agent may
// not read, a worklist, dispute cases or open items the API refuses and an
// API that is gone stop the run before a session starts.
func TestAdviseRefusals(t *testing.T) {
	model := llmtest.New(t)
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		api     *apitools.Client
		company string
		key     string
		want    string
	}{
		"a key date that is none":   {client(t, &fakeAPI{}), "3000", "15.11.2026", "the key date is a date"},
		"another company":           {client(t, &fakeAPI{}), "9999", keyDate, "no company 9999"},
		"the worklist refused":      {client(t, &fakeAPI{status: map[string]int{"/v1/collection-worklist": 403}}), "3000", keyDate, "the collection worklist of company 3000"},
		"the cases refused":         {client(t, &fakeAPI{status: map[string]int{"/v1/dispute-cases": 403}}), "3000", keyDate, "the open dispute cases of customer 100043"},
		"the open items refused":    {client(t, &fakeAPI{status: map[string]int{"/v1/open-items": 403}}), "3000", keyDate, "the open items of customer 100043"},
		"the accounts refused":      {client(t, &fakeAPI{status: map[string]int{"/v1/accounts": 403}}), "3000", keyDate, "the accounts of legal entity " + le},
		"the company's day refused": {client(t, &fakeAPI{status: map[string]int{"/v1/legal-entities/" + le + "/today": 403}}), "3000", "", "collectionsagent: company 3000"},
		"the API gone":              {gone, "3000", keyDate, "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&collectionsagent.Agent{LLM: model.Runner(), API: c.api}).Advise(context.Background(), c.company, c.key)
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
// is an operation of its never list, and the contract closes those to
// agents (ADR-0072 As built, T105b).
func TestNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	tools := collectionsagent.Tools()
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
	// Every write of the collections context's dunning and dispute cases.
	for id, op := range ops {
		if op.Method != http.MethodGet && (strings.Contains(id, "Dunning") || strings.Contains(id, "DisputeCase")) && !slices.Contains(collectionsagent.Never, id) {
			t.Errorf("never list: got %v, want it to hold %s", collectionsagent.Never, id)
		}
	}
	for _, id := range collectionsagent.Never {
		t.Run(id, func(t *testing.T) {
			if slices.Contains(tools, id) {
				t.Errorf("%s is among the agent's tools", id)
			}
			if op, ok := ops[id]; !ok || op.AgentAllowed {
				t.Errorf("%s: got in the contract %v, open to agents %v; want x-agent: false", id, ok, op.AgentAllowed)
			}
		})
	}
}
