package savingsagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
	"github.com/mustie7iy/agentic-erp/internal/savingsagent"
)

const (
	session  = "01a1aa00-0000-7000-8000-000000000a01"
	le       = "01a1aa00-0000-7000-8000-000000000b01"
	tyrell   = "01a1aa00-0000-7000-8000-000000000c01"
	acme     = "01a1aa00-0000-7000-8000-000000000c02"
	office   = "01a1aa00-0000-7000-8000-000000000d01" // account 6000
	order    = "01a1aa00-0000-7000-8000-000000000e01" // above its contract's price
	material = "01a1aa00-0000-7000-8000-000000000f01"
	period   = "2026-01-01..2026-11-15"
)

// The spend answers: by supplier, Tyrell off contract in CAD and USD and
// Acme wholly under contract; by account and supplier, account 6000 from
// both in CAD and 6100 from Tyrell alone; by account, their totals.
const (
	bySupplier = `{"columns": [{"name": "vendor"}, {"name": "vendor_name"}, {"name": "currency"}, {"name": "spend_off_contract"}], "rows": [
		[{"value": "100001"}, {"value": "Tyrell"}, {"value": "CAD"}, {"value": "4700.00", "currency": "CAD"}],
		[{"value": "100002"}, {"value": "Acme"}, {"value": "CAD"}, {"value": "0.00", "currency": "CAD"}],
		[{"value": "100003"}, {"value": "Initech"}, {"value": "CAD"}, {"value": null, "currency": "CAD"}],
		[{"value": "100001"}, {"value": "Tyrell"}, {"value": "USD"}, {"value": "120.00", "currency": "USD"}]]}`
	byAccountSupplier = `{"columns": [{"name": "account"}, {"name": "vendor"}, {"name": "currency"}, {"name": "spend"}], "rows": [
		[{"value": "6000"}, {"value": "100001"}, {"value": "CAD"}, {"value": "4700.00", "currency": "CAD"}],
		[{"value": "6000"}, {"value": "100002"}, {"value": "CAD"}, {"value": "800.00", "currency": "CAD"}],
		[{"value": "6100"}, {"value": "100001"}, {"value": "CAD"}, {"value": "300.00", "currency": "CAD"}],
		[{"value": null}, {"value": "100002"}, {"value": "CAD"}, {"value": "50.00", "currency": "CAD"}]]}`
	byAccount = `{"columns": [{"name": "account"}, {"name": "account_name"}, {"name": "currency"}, {"name": "spend"}], "rows": [
		[{"value": "6000"}, {"value": "Office supplies"}, {"value": "CAD"}, {"value": "5500.00", "currency": "CAD"}],
		[{"value": "6100"}, {"value": "Travel"}, {"value": "CAD"}, {"value": "300.00", "currency": "CAD"}]]}`
	contracts = `{"items": [{"document_number": "4600000001", "supplier_id": "` + acme + `", "valid_from": "2026-01-01", "valid_to": "2026-12-31",
		"items": [{"item_no": 1, "material_id": "` + material + `", "unit_price": {"amount": "40.00", "currency": "CAD"}, "price_unit": 1}]}]}`
	orders = `{"items": [
		{"id": "` + order + `", "document_number": "4500000007", "supplier_id": "` + acme + `", "status": "released", "order_date": "2026-03-01",
		 "total_amount": {"amount": "450.00", "currency": "CAD"}, "items": [{"item_no": 1, "material_id": "` + material + `", "unit_price": {"amount": "45.00", "currency": "CAD"}, "price_unit": 1}]},
		{"id": "x6", "document_number": "4500000013", "supplier_id": "` + acme + `", "status": "closed", "order_date": "2026-04-01",
		 "total_amount": {"amount": "900.00", "currency": "CAD"}, "items": [{"item_no": 1, "material_id": "` + material + `", "unit_price": {"amount": "41.00", "currency": "CAD"}, "price_unit": 1}]},
		{"id": "x1", "document_number": "4500000008", "supplier_id": "` + acme + `", "status": "released", "order_date": "2026-03-01", "contract_id": "c1",
		 "total_amount": {"amount": "1.00", "currency": "CAD"}, "items": [{"item_no": 1, "material_id": "` + material + `", "unit_price": {"amount": "99.00", "currency": "CAD"}}]},
		{"id": "x2", "document_number": "4500000009", "supplier_id": "` + acme + `", "status": "draft", "order_date": "2026-03-01",
		 "total_amount": {"amount": "2.00", "currency": "CAD"}, "items": [{"item_no": 1, "material_id": "` + material + `", "unit_price": {"amount": "99.00", "currency": "CAD"}}]},
		{"id": "x3", "document_number": "4500000010", "supplier_id": "` + acme + `", "status": "closed", "order_date": "2025-03-01",
		 "total_amount": {"amount": "3.00", "currency": "CAD"}, "items": [{"item_no": 1, "material_id": "` + material + `", "unit_price": {"amount": "99.00", "currency": "CAD"}},
		 {"item_no": 2, "unit_price": {"amount": "99.00", "currency": "CAD"}}, {"item_no": 3, "material_id": "` + material + `", "contract_item_no": 1, "unit_price": {"amount": "99.00", "currency": "CAD"}}]},
		{"id": "x4", "document_number": "4500000011", "supplier_id": "` + tyrell + `", "status": "closed", "order_date": "2026-03-01",
		 "total_amount": {"amount": "4.00", "currency": "CAD"}, "items": [{"item_no": 1, "material_id": "` + material + `", "unit_price": {"amount": "99.00", "currency": "CAD"}}]},
		{"id": "x5", "document_number": "4500000012", "supplier_id": "` + acme + `", "status": "closed", "order_date": "2026-03-01",
		 "total_amount": {"amount": "5.00", "currency": "CAD"}, "items": [{"item_no": 1, "material_id": "` + material + `", "unit_price": {"amount": "30.00", "currency": "CAD"}}]}]}`
)

// fakeAPI is a stand-in for the cell's API: company 3000, its spend, its
// suppliers and accounts by number, an active contract and its orders, a
// session and recommendations answered by their bodies; status refuses a
// path with that status, and empty answers it with no items.
type fakeAPI struct {
	mu     sync.Mutex
	calls  []string
	status map[string]int
	empty  map[string]bool
	made   int
}

func (a *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body) // a test's own request
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, r.Method+" "+r.URL.Path)
	answer := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // a test's answer; a client gone has nothing to read
	}
	path := r.URL.Path
	if s := a.status[path]; s != 0 {
		answer(s, `{"code": "identity.permission_missing", "status": 403}`)
		return
	}
	if a.empty[path] {
		answer(200, `{"items": [], "rows": []}`)
		return
	}
	switch path {
	case "/v1/legal-entities":
		answer(200, `{"items": [{"id": "`+le+`", "code": "3000"}]}`)
	case "/v1/analytics-queries":
		answer(200, spend(b))
	case "/v1/business-partners":
		answer(200, map[string]string{"100001": `{"items": [{"id": "` + tyrell + `"}]}`, "100002": `{"items": [{"id": "` + acme + `"}]}`}[r.URL.Query().Get("number")])
	case "/v1/accounts":
		answer(200, `{"items": [{"id": "`+office+`"}]}`)
	case "/v1/contracts":
		answer(200, contracts)
	case "/v1/purchase-orders":
		answer(200, orders)
	case "/v1/purchase-orders/" + order:
		answer(200, `{"id": "`+order+`", "total_amount": {"amount": "450.00", "currency": "CAD"}}`)
	case "/v1/agent-sessions":
		answer(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case "/v1/recommendations":
		var body struct {
			ObjectID   string `json:"object_id"`
			ObjectType string `json:"object_type"`
			Kind       string
		}
		_ = json.Unmarshal(b, &body) // the agent's own JSON
		a.made++
		answer(201, fmt.Sprintf(`{"id": "r%d", "object_id": "%s", "object_type": "%s", "kind": "%s"}`, a.made, body.ObjectID, body.ObjectType, body.Kind))
	default:
		answer(200, `{}`)
	}
}

// spend answers a query by what it breaks the spend down by.
func spend(body []byte) string {
	var q struct{ By []string }
	_ = json.Unmarshal(body, &q) // the agent's own JSON
	switch strings.Join(q.By, " ") {
	case "vendor vendor_name currency":
		return bySupplier
	case "account vendor currency":
		return byAccountSupplier
	default:
		return byAccount
	}
}

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

func on(objectType, id, kind, content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": objectType,
			"object_id": id, "kind": kind, "content": content}}
	})
}

var query = llmtest.Tool("runAnalyticsQuery", map[string]any{"body": map[string]any{"measures": []any{"spend_off_contract"},
	"by": []any{"vendor", "vendor_name", "currency"}, "companies": []any{"3000"}}})

const (
	offTyrell  = "Spend off contract 4700.00 CAD, 120.00 USD"
	spentOn600 = "Spend 5500.00 CAD"
)

// told reports whether the model was told each text, which it reads as
// JSON text.
func told(t *testing.T, requests []string, want ...string) {
	t.Helper()
	all := strings.Join(requests, "\n")
	for _, w := range want {
		j, _ := json.Marshal(w)
		if !strings.Contains(all, strings.Trim(string(j), `"`)) {
			t.Errorf("the model was not told %q", w)
		}
	}
}

// TestAdvise: the run finds Tyrell's spend off contract (Acme's is none),
// account 6000 bought from two suppliers (6100 from one, spend on no
// account left out) and order 4500000007 above Acme's contract (an order
// on a contract, a draft, one before its contract, another supplier's and
// a cheaper one are not), and recommends on each once. Its check refuses,
// and counts, a first line before a tool returned its amounts, one that is
// not the spend found, a saving stated with a figure and a number no tool
// returned.
func TestAdvise(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t,
		on("business_partner", tyrell, "bring_under_contract", offTyrell+"\nNo contract."),
		query,
		on("business_partner", tyrell, "bring_under_contract", "Spend off contract 4700.00 CAD\nNo contract."),
		on("business_partner", tyrell, "bring_under_contract", offTyrell+"\nA contract saves about 470.00 CAD."),
		on("business_partner", tyrell, "bring_under_contract", offTyrell+"\nTyrell is 85 percent of the account."),
		on("business_partner", tyrell, "bring_under_contract", offTyrell+"\nNone of Tyrell's spend is under a contract; negotiate one."),
		llmtest.Tool("runAnalyticsQuery", map[string]any{"body": map[string]any{"measures": []any{"spend"}, "by": []any{"account", "account_name", "currency"}}}),
		on("account", office, "consolidate_suppliers", spentOn600+"\nBought from Tyrell and Acme."),
		llmtest.Tool("getPurchaseOrder", map[string]any{"id": order}),
		on("purchase_order", order, "use_contract_price", "Ordered 450.00 CAD\nItem 1 at 45.00 CAD, above contract 4600000001 item 1 at 40.00 CAD."),
		on("purchase_order", order, "use_contract_price", "Ordered 450.00 CAD\nItem 1 is above contract 4600000001 item 1."),
		llmtest.Final("Three opportunities."))
	res, err := (&savingsagent.Agent{LLM: model.Runner(), API: client(t, a)}).Advise(context.Background(), "3000", period)
	if err != nil {
		t.Fatal(err)
	}
	want := []recommend.Made{{ID: "r1", ObjectType: "business_partner", ObjectID: tyrell, Kind: "bring_under_contract"},
		{ID: "r2", ObjectType: "account", ObjectID: office, Kind: "consolidate_suppliers"},
		{ID: "r3", ObjectType: "purchase_order", ObjectID: order, Kind: "use_contract_price"}}
	if !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != 5 {
		t.Errorf("refused: got %d, want 5", res.Refused)
	}
	if got := res.Names[tyrell] + "; " + res.Names[office] + "; " + res.Names[order]; got != "100001 Tyrell; 6000 Office supplies; 4500000007" {
		t.Errorf("names: got %q", got)
	}
	told(t, model.Requests[:1], "the period is 2026-01-01 to 2026-11-15. The run found 4 opportunities; you recommend on the first 4 opportunities, at most 10 opportunities a run.",
		`the first line is "Spend off contract <amount> CAD, <amount> USD"`, `"Spend <amount> CAD"`, `"Ordered <amount> CAD"`,
		"has item 1 above item 1 of contract 4600000001: use_contract_price")
	told(t, model.Requests, savingsagent.ErrSpend.Error(), "4700.00 is no amount a tool of the run returned", savingsagent.ErrSaving.Error(),
		savingsagent.ErrUnread.Error()+": not 85", "not 45.00, 40.00")
}

// TestAdviseMax: a run recommends on at most Max opportunities, the
// suppliers first; the default period is the year to today, UTC.
func TestAdviseMax(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t, on("account", office, "consolidate_suppliers", spentOn600), llmtest.Final("One."))
	now := func() time.Time { return time.Date(2026, 11, 15, 23, 0, 0, 0, time.FixedZone("EST", -5*3600)) }
	res, err := (&savingsagent.Agent{LLM: model.Runner(), API: client(t, a), Max: 1, Now: now}).Advise(context.Background(), "3000", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Names) != 1 || res.Names[tyrell] == "" || res.Refused != 1 {
		t.Errorf("got names %v, %d refused; want Tyrell alone and the account refused", res.Names, res.Refused)
	}
	told(t, model.Requests[:1], "the period is 2025-11-17 to 2026-11-16.", "you recommend on the first one opportunity, at most one opportunity a run.")
}

// TestAdviseNone: a period without spend off contract, accounts bought
// from several suppliers or active contracts starts no session and says
// so.
func TestAdviseNone(t *testing.T) {
	a := &fakeAPI{empty: map[string]bool{"/v1/analytics-queries": true, "/v1/contracts": true}}
	res, err := (&savingsagent.Agent{LLM: llmtest.New(t).Runner(), API: client(t, a)}).Advise(context.Background(), "3000", period)
	if err != nil || res.Session != "" || !strings.Contains(res.Reply, "No savings opportunity in company 3000's purchasing from 2026-01-01 to 2026-11-15") {
		t.Errorf("got %v, session %q, reply %q; want no session and why", err, res.Session, res.Reply)
	}
	if a.sent("/v1/purchase-orders") != 0 {
		t.Error("orders read without an active contract")
	}
}

// TestAdviseRefusals: a period that is no period, a maximum below one, a
// company the agent may not read, and a read it is refused or that finds
// no supplier stop the run before a session starts.
func TestAdviseRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		api            *fakeAPI
		company, which string
		max            int
		want           string
	}{
		"no period":          {&fakeAPI{}, "3000", "2026-01-01", 0, "the period is FROM..TO"},
		"a period backwards": {&fakeAPI{}, "3000", "2026-02-01..2026-01-01", 0, "the period is FROM..TO"},
		"a bad end":          {&fakeAPI{}, "3000", "2026-01-01..soon", 0, "the period is FROM..TO"},
		"below one":          {&fakeAPI{}, "3000", period, -1, "at least one"},
		"another company":    {&fakeAPI{}, "9999", period, 0, "no company 9999"},
		"the companies":      {&fakeAPI{status: map[string]int{"/v1/legal-entities": 403}}, "3000", period, 0, "403"},
		"the spend":          {&fakeAPI{status: map[string]int{"/v1/analytics-queries": 403}}, "3000", period, 0, "spend_off_contract of company 3000 by vendor, vendor_name"},
		"a supplier":         {&fakeAPI{empty: map[string]bool{"/v1/business-partners": true}}, "3000", period, 0, "supplier 100001: /v1/business-partners?number=100001 lists none"},
		"an account":         {&fakeAPI{status: map[string]int{"/v1/accounts": 403}}, "3000", period, 0, "account 6000"},
		"the contracts":      {&fakeAPI{status: map[string]int{"/v1/contracts": 403}}, "3000", period, 0, "the active contracts of company 3000"},
		"the orders":         {&fakeAPI{status: map[string]int{"/v1/purchase-orders": 403}}, "3000", period, 0, "the purchase orders of company 3000"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (&savingsagent.Agent{LLM: llmtest.New(t).Runner(), API: client(t, c.api), Max: c.max}).Advise(context.Background(), c.company, c.which)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error saying %q", err, c.want)
			}
			if n := c.api.sent("agent-sessions"); n != 0 {
				t.Errorf("sessions: got %d, want none", n)
			}
		})
	}
}

// TestAdviseSecondQueryRefused: the second spend query refused stops the
// run as the first does.
func TestAdviseSecondQueryRefused(t *testing.T) {
	a := &secondRefused{fakeAPI: fakeAPI{}}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	for _, n := range []int{2, 3} {
		a.refuse, a.n = n, 0
		_, err := (&savingsagent.Agent{LLM: llmtest.New(t).Runner(), API: &apitools.Client{BaseURL: srv.URL, Token: "t"}}).Advise(context.Background(), "3000", period)
		if err == nil || !strings.Contains(err.Error(), "spend of company 3000 by account") {
			t.Errorf("query %d refused: got %v, want the spend by account named", n, err)
		}
	}
}

// secondRefused refuses the refuse-th spend query.
type secondRefused struct {
	fakeAPI
	n, refuse int
}

func (a *secondRefused) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/analytics-queries" {
		if a.n++; a.n == a.refuse {
			w.WriteHeader(http.StatusForbidden)
			return
		}
	}
	a.fakeAPI.ServeHTTP(w, r)
}

// TestNever: the agent's tools are reads open to agents, runAnalyticsQuery
// a read that is no GET among them, and the one that recommends; what it
// never calls (creating an RFQ, a contract or an order) is in the
// contract and no tool of its.
func TestNever(t *testing.T) {
	ops := api.MustOperations()
	tools := savingsagent.Tools()
	for _, id := range tools {
		op, ok := ops[id]
		if !ok || !op.AgentAllowed || (op.Method != http.MethodGet && !op.Reads && id != recommend.Proposer) {
			t.Errorf("%s: got in the contract %v, open to agents %v, method %s; want a read open to agents", id, ok, op.AgentAllowed, op.Method)
		}
	}
	for _, id := range savingsagent.Never {
		if _, ok := ops[id]; !ok || slices.Contains(tools, id) {
			t.Errorf("%s: got in the contract %v, among the tools %v; want in the contract and no tool", id, ok, slices.Contains(tools, id))
		}
	}
}
