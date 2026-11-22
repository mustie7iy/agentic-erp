package demandagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/demandagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

const (
	session = "01a1aa00-0000-7000-8000-000000000a01"
	le      = "01a1aa00-0000-7000-8000-000000000b01"
	foreign = "01a1aa00-0000-7000-8000-000000000b09" // another company
	p100    = "01a1aa00-0000-7000-8000-000000000c01"
	p200    = "01a1aa00-0000-7000-8000-000000000c02"
	p300    = "01a1aa00-0000-7000-8000-000000000c03"
	p900    = "01a1aa00-0000-7000-8000-000000000c09" // the other company's plant
	hinge   = "01a1aa00-0000-7000-8000-000000000d01"
	bracket = "01a1aa00-0000-7000-8000-000000000d02"
)

// now is the run's instant: 22 November 2026 in UTC, the company's zone.
// The weeks read run from 27 July; the horizon from 23 November.
var now = func() time.Time { return time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC) }

// plants are the hinge's plants: P300, P100 and P200 of the company (out
// of their codes' order) and P900 of another.
const plants = `[{"plant_id": "` + p300 + `", "plant_code": "P300", "plant_name": "Calgary plant", "legal_entity_id": "` + le + `"},
	{"plant_id": "` + p100 + `", "plant_code": "P100", "plant_name": "Toronto plant", "legal_entity_id": "` + le + `"},
	{"plant_id": "` + p200 + `", "plant_code": "P200", "plant_name": "Montreal plant", "legal_entity_id": "` + le + `"},
	{"plant_id": "` + p900 + `", "plant_code": "P900", "plant_name": "Sharjah plant", "legal_entity_id": "` + foreign + `"}]`

func materialWith(plants string) string {
	return `{"items": [{"id": "` + hinge + `", "number": "MA1", "description": "Hinge H30", "base_unit": "EA", "plants": ` + plants + `,
		"alternative_units": [{"unit": "BOX", "numerator": 20, "denominator": 1}, {"unit": "BAD", "numerator": 0, "denominator": 1}]}]}`
}

// line is a document line of the material at the plant.
func line(material, plant, qty string, inTransit bool) string {
	return fmt.Sprintf(`{"material_id": "%s", "plant_id": "%s", "in_transit": %t, "quantity": {"value": "%s", "unit": "EA"}}`, material, plant, inTransit, qty)
}

func doc(date, movement string, lines ...string) string {
	return `{"posting_date": "` + date + `", "movement_type": "` + movement + `", "lines": [` + strings.Join(lines, ", ") + `]}`
}

// documents are the hinge's material documents, the latest created
// first, whatever their dates. At P100: this week a delivery's issue of
// 30, a cost center's 9 and a reversal of 4 back (35); last week 13;
// five weeks ago an intercompany issue of 26; thirteen weeks ago 26,
// read by the accuracy alone. At P200 two weeks ago 7. Left out: a
// scrapping, an initial entry, an in-transit line, a line of another
// material and one at the other company's plant, a document dated before
// the weeks read and one after the day.
var documents = `{"items": [` + strings.Join([]string{
	doc("2026-11-23", "201", line(hinge, p100, "-1000", false)),
	doc("2026-11-22", "201", line(hinge, p100, "-9", false), line(bracket, p100, "-500", false)),
	doc("2026-11-21", "602", line(hinge, p100, "4", false)),
	doc("2026-11-20", "601", line(hinge, p100, "-30", false), line(hinge, p900, "-800", false)),
	doc("2026-11-19", "551", line(hinge, p100, "-100", false)),
	doc("2026-11-18", "561", line(hinge, p100, "40", false)),
	doc("2026-11-17", "643", line(hinge, p100, "-70", true)),
	doc("2026-11-10", "201", line(hinge, p100, "-13", false)),
	doc("2026-11-03", "201", line(hinge, p200, "-7", false)),
	doc("2026-10-15", "643", line(hinge, p100, "-26", false)),
	doc("2026-08-23", "201", line(hinge, p100, "-26", false)),
	doc("2026-07-26", "201", line(hinge, p100, "-1000", false)),
}, ", ") + `]}`

func item(material, qty, unit, delivered, removed string) string {
	extra := ""
	if delivered != "" {
		extra += `, "delivered_quantity": {"value": "` + delivered + `", "unit": "` + unit + `"}`
	}
	if removed != "" {
		extra += `, "removed_in_change": ` + removed
	}
	return `{"material_id": "` + material + `", "quantity": {"value": "` + qty + `", "unit": "` + unit + `"}` + extra + `}`
}

func salesOrder(number, plant, ordered, requested string, items ...string) string {
	day := ""
	if requested != "" {
		day = `, "requested_delivery_date": "` + requested + `"`
	}
	return `{"document_number": "` + number + `", "status": "confirmed", "order_date": "` + ordered + `", "plant_id": "` + plant + `"` + day +
		`, "items": [` + strings.Join(items, ", ") + `]}`
}

// orders are the company's confirmed orders of the hinge: at P100, 3
// boxes (60) asked for on 30 November, the second week, beside a removed
// item and one of the bracket; 10 ordered on 1 November with none asked
// for, 4 delivered (6, due in the first week); one delivered in full;
// one asked for after the horizon. At P300, 2 asked for on 5 December, 3
// on 17 January, the horizon's last day (its eighth week), and 90 on 18
// January, the day after it, left out.
// Left out: an order of the other company's plant, and one a page gives
// that is not confirmed.
var orders = `{"items": [` + strings.Join([]string{
	salesOrder("SO1", p100, "2026-11-20", "2026-11-30", item(hinge, "3", "BOX", "0", ""), item(hinge, "50", "EA", "", "1"), item(bracket, "9", "EA", "", "")),
	salesOrder("SO2", p100, "2026-11-01", "", item(hinge, "10", "EA", "4", "")),
	salesOrder("SO3", p900, "2026-11-01", "", item(hinge, "90", "EA", "", "")),
	salesOrder("SO4", p100, "2026-11-01", "2027-02-01", item(hinge, "90", "EA", "", "")),
	salesOrder("SO5", p100, "2026-11-01", "", item(hinge, "5", "EA", "5", "")),
	salesOrder("SO6", p300, "2026-11-20", "2026-12-05", item(hinge, "2", "EA", "", "")),
	salesOrder("SO8", p300, "2026-11-20", "2027-01-17", item(hinge, "3", "EA", "", "")),
	salesOrder("SO9", p300, "2026-11-20", "2027-01-18", item(hinge, "90", "EA", "", "")),
	`{"document_number": "SO7", "status": "draft", "order_date": "2026-11-01", "plant_id": "` + p100 + `", "items": [` + item(hinge, "90", "EA", "", "") + `]}`,
}, ", ") + `]}`

// The lines the run computes, by plant code. P100: 74 issued in the 13
// weeks, 74/13 a week; the first week the 6 open, the second the 60; its
// accuracy over the last four weeks: forecasts 5, 4, 4, 4 against 35, 13,
// 0, 0, WAPE 47/17 = 276.47%. P200: 7/13 a week; forecasts 7/13, 7/13, 0,
// 0 against 0, 0, 7, 0, WAPE 750%. P300: nothing issued, nothing
// forecast.
const (
	line100 = "Forecast MA1 at P100 (plant " + p100 + "), EA per week from 2026-11-23: 6, 60, 5.692, 5.692, 5.692, 5.692, 5.692, 5.692; issued 74 in 13 weeks, " +
		"open orders 66; accuracy -176.47% over 4 weeks"
	line200 = "Forecast MA1 at P200 (plant " + p200 + "), EA per week from 2026-11-23: 0.538, 0.538, 0.538, 0.538, 0.538, 0.538, 0.538, 0.538; issued 7 " +
		"in 13 weeks, open orders 0; accuracy -650.00% over 4 weeks"
	line300 = "Forecast MA1 at P300 (plant " + p300 + "), EA per week from 2026-11-23: 0, 2, 0, 0, 0, 0, 0, 3; issued 0 in 13 weeks, open orders 5; " +
		"accuracy none over 4 weeks: nothing was forecast"
)

// fakeAPI is a stand-in for the cell's API: company 3000, the hinge MA1
// and its documents and orders above, a session and recommendations
// answered by their bodies. It keeps every request it was sent.
type fakeAPI struct {
	mu        sync.Mutex
	calls     []string
	refuse    string // a path answered 403
	material  string // replaces the materials' answer
	documents string // replaces the documents
	orders    string // replaces the orders
}

func (a *fakeAPI) answer(path, query string) (int, string) {
	if path == a.refuse {
		return 403, `{"code": "identity.permission_missing", "status": 403}`
	}
	switch path {
	case "/v1/legal-entities":
		return 200, `{"items": [{"id": "` + le + `", "code": "3000"}]}`
	case "/v1/legal-entities/" + le + "/today":
		return 200, `{"time_zone": "UTC"}`
	case "/v1/materials":
		if !strings.Contains(query, "number=MA1") {
			return 200, `{"items": []}`
		}
		return 200, cmpOr(a.material, materialWith(plants))
	case "/v1/material-documents":
		return 200, cmpOr(a.documents, documents)
	case "/v1/sales-orders":
		return 200, cmpOr(a.orders, orders)
	}
	return 200, `{"items": []}`
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (a *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body) // a test's own request
	a.mu.Lock()
	a.calls = append(a.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	a.mu.Unlock()
	write := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // a test's answer; a client gone has nothing to read
	}
	switch r.URL.Path {
	case "/v1/agent-sessions":
		write(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case "/v1/recommendations":
		var body struct {
			ObjectID   string `json:"object_id"`
			ObjectType string `json:"object_type"`
			Kind       string `json:"kind"`
			Content    string `json:"content"`
		}
		_ = json.Unmarshal(b, &body) // the agent's own JSON
		answer, _ := json.Marshal(map[string]string{"id": "r1", "object_id": body.ObjectID, "object_type": body.ObjectType, "kind": body.Kind,
			"content": body.Content})
		write(201, string(answer))
	default:
		write(a.answer(r.URL.Path, r.URL.RawQuery))
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

// propose is a demand forecast on the hinge with the content.
func propose(content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "material",
			"object_id": hinge, "kind": "demand_forecast", "content": content}}
	})
}

// refusedWith reports whether the model was told the refusal.
func refusedWith(t *testing.T, requests []string, want error) {
	t.Helper()
	told, _ := json.Marshal(want.Error()) // the model reads the refusal as JSON text
	if !strings.Contains(strings.Join(requests, "\n"), strings.Trim(string(told), `"`)) {
		t.Errorf("the model was not told %q", want)
	}
}

func agentOn(t *testing.T, a *fakeAPI, model *llmtest.Server) *demandagent.Agent {
	t.Helper()
	return &demandagent.Agent{LLM: model.Runner(), API: client(t, a), Now: now}
}

// TestForecast: the run reads the hinge's documents and confirmed orders
// and computes each of the company's plants' forecast in the order of
// their codes; the other company's plant is none of them. Its check
// refuses a line not given, lines out of their order, a content without
// a plant's line or without reasons, and a number the run did not read,
// a figure with a fraction among them; nothing refused reaches the API.
func TestForecast(t *testing.T) {
	a := &fakeAPI{}
	all := line100 + "\n" + line200 + "\n" + line300 + "\n"
	model := llmtest.New(t,
		llmtest.Tool("listSalesOrders", map[string]any{"legal_entity_id": le, "status": "confirmed", "plant_id": p100, "material_id": hinge}),
		propose("Forecast MA1 at P100: 60 a week\nThe big order."),
		propose(line200+"\n"+line100+"\n"+line300+"\nP200 first."),
		propose(line100+"\n"+line300+"\nP200 left out."),
		propose(all),
		propose(all+"P100's average is 5.692 a week."),
		propose(all+"SO1 asks for 4711."),
		propose(all+"SO1 asks for 3 BOX at P100 in the second week; the 13 weeks' issues are low, and the accuracy too low to trust."),
		llmtest.Final("Forecast the hinge at three plants; the planner decides."))
	res, err := agentOn(t, a, model).Forecast(context.Background(), "3000", "MA1")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recommendations) != 1 || res.Recommendations[0].Kind != demandagent.Forecast || res.Recommendations[0].ObjectID != hinge {
		t.Fatalf("recommendations: got %v, want one demand_forecast on the hinge", res.Recommendations)
	}
	if res.Refused != 6 {
		t.Errorf("refused: got %d, want 6", res.Refused)
	}
	if got := len(a.sent("POST /v1/recommendations")); got != 1 {
		t.Errorf("recommendations sent: got %d, want 1: the check keeps the others from the API", got)
	}
	want := "Each plant's forecast, with its line:\\n" + line100 + "\\n" + line200 + "\\n" + line300
	if !strings.Contains(model.Requests[0], want) {
		t.Errorf("prompt: got %s, want %q", model.Requests[0], want)
	}
	if !strings.Contains(model.Requests[0], "the 17 weeks of seven days from 2026-07-27 to 2026-11-22") {
		t.Errorf("prompt: got %s, want the weeks read", model.Requests[0])
	}
	for _, not := range []string{"P900", "more plants"} {
		if strings.Contains(model.Requests[0], not) {
			t.Errorf("prompt: got %s, want no %s", model.Requests[0], not)
		}
	}
	for _, e := range []error{demandagent.ErrLine, demandagent.ErrOrder, demandagent.ErrForm, demandagent.ErrUnread} {
		refusedWith(t, model.Requests, e)
	}
	if got, want := res.Names[hinge], "MA1 Hinge H30"; got != want {
		t.Errorf("the material's name: got %q, want %q", got, want)
	}
	if got := a.sent("GET /v1/sales-orders?"); len(got) != 2 || !strings.Contains(got[0], "material_id="+hinge) || !strings.Contains(got[0], "status=confirmed") {
		t.Errorf("reads of the orders: got %v, want the company's confirmed orders of the hinge before the session and the model's", got)
	}
}

// TestForecastMaxLines: a run lists the first MaxLines plants and says how
// many more there are.
func TestForecastMaxLines(t *testing.T) {
	var ps []string
	for i := range demandagent.MaxLines + 2 {
		ps = append(ps, fmt.Sprintf(`{"plant_id": "pl%02d", "plant_code": "P%02d", "legal_entity_id": "%s"}`, i, i, le))
	}
	model := llmtest.New(t, llmtest.Final("Nothing recommended."))
	a := &fakeAPI{material: materialWith("[" + strings.Join(ps, ", ") + "]"), documents: `{"items": [` + doc("2026-11-20", "201", line(hinge, "pl00", "-1", false)) + `]}`}
	if _, err := agentOn(t, a, model).Forecast(context.Background(), "3000", "MA1"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(model.Requests[0], "Forecast MA1 at P"); got != demandagent.MaxLines {
		t.Errorf("lines listed: got %d, want %d", got, demandagent.MaxLines)
	}
	if !strings.Contains(model.Requests[0], "2 more plants are not listed") {
		t.Errorf("prompt: got %s, want the two more named", model.Requests[0])
	}
	if strings.Contains(model.Requests[0], "at P21") {
		t.Errorf("prompt: got %s, want the last left out", model.Requests[0])
	}
}

// TestForecastNothing: a material with no issue in the weeks read and no
// open order gets no session and a reply saying so.
func TestForecastNothing(t *testing.T) {
	a := &fakeAPI{documents: `{"items": [` + doc("2026-07-26", "201", line(hinge, p100, "-5", false)) + `]}`, orders: `{"items": []}`}
	res, err := agentOn(t, a, llmtest.New(t)).Forecast(context.Background(), "3000", "MA1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Session != "" || len(res.Recommendations) != 0 {
		t.Errorf("result: got %+v, want no session and no recommendation", res)
	}
	if want := "Nothing to forecast for material MA1 in company 3000 on 2026-11-22: no goods issue of it at its plants since 2026-07-27"; !strings.HasPrefix(res.Reply, want) {
		t.Errorf("reply: got %q, want it to start %q", res.Reply, want)
	}
	if got := a.sent("agent-sessions"); len(got) != 0 {
		t.Errorf("sessions: got %v, want none", got)
	}
}

// TestForecastRefusals: a company, material or read the agent may not
// have, a material of no plant of the company, a quantity in a unit the
// material has not or that is no decimal, a date that is none and an API
// that is gone stop the run before a session starts.
func TestForecastRefusals(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	one := func(s string) string { return `{"items": [` + s + `]}` }
	for name, c := range map[string]struct {
		api      *apitools.Client
		material string
		want     string
	}{
		"no such company":     {client(t, &fakeAPI{}), "MA1", "demandagent: company 9999: apitools"},
		"no such material":    {client(t, &fakeAPI{}), "MA9", "no material MA9"},
		"the materials":       {client(t, &fakeAPI{refuse: "/v1/materials"}), "MA1", "demandagent: material MA1"},
		"no plant of it":      {client(t, &fakeAPI{material: materialWith(`[]`)}), "MA1", "MA1 is extended to no plant of company 3000"},
		"the day":             {client(t, &fakeAPI{refuse: "/v1/legal-entities/" + le + "/today"}), "MA1", "the day of company 3000"},
		"the documents":       {client(t, &fakeAPI{refuse: "/v1/material-documents"}), "MA1", "the material documents of material MA1"},
		"the orders":          {client(t, &fakeAPI{refuse: "/v1/sales-orders"}), "MA1", "the sales orders of material MA1"},
		"an issue's unit":     {client(t, &fakeAPI{documents: one(doc("2026-11-20", "201", strings.Replace(line(hinge, p100, "-1", false), `"EA"`, `"KG"`, 1)))}), "MA1", "a quantity in KG"},
		"an issue's value":    {client(t, &fakeAPI{documents: one(doc("2026-11-20", "201", line(hinge, p100, "lots", false)))}), "MA1", `a quantity "lots"`},
		"a posting date":      {client(t, &fakeAPI{documents: one(doc("2026-11-1x", "201", line(hinge, p100, "-1", false)))}), "MA1", `a posting date "2026-11-1x"`},
		"an order's day":      {client(t, &fakeAPI{orders: one(salesOrder("SO1", p100, "someday", "", item(hinge, "1", "EA", "", "")))}), "MA1", "order SO1's day"},
		"an order's unit":     {client(t, &fakeAPI{orders: one(salesOrder("SO1", p100, "2026-11-20", "", item(hinge, "1", "BAD", "", "")))}), "MA1", "order SO1: demandagent: a quantity in BAD"},
		"a delivered value":   {client(t, &fakeAPI{orders: one(salesOrder("SO1", p100, "2026-11-20", "", item(hinge, "1", "EA", "x", "")))}), "MA1", `order SO1: demandagent: a quantity "x"`},
		"the API gone at all": {gone, "MA1", "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			company := "3000"
			if name == "no such company" {
				company = "9999"
			}
			res, err := (&demandagent.Agent{LLM: llmtest.New(t).Runner(), API: c.api, Now: now}).Forecast(context.Background(), company, c.material)
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
// is an operation of its never list, which holds every write of goods
// movements, counts, sales and returns orders and their deliveries,
// requisitions, purchase orders, schedule lines and planning (ADR-0072,
// S2m).
func TestNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	tools := demandagent.Tools()
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
	for id, op := range ops {
		if op.Method == http.MethodGet {
			continue
		}
		for _, of := range []string{"MaterialDocument", "GoodsReceipt", "GoodsIssue", "StockTransport", "PhysicalInventory", "SalesOrder", "SalesDelivery",
			"OutboundDelivery", "ReturnsOrder", "ReturnReceipt", "Requisition", "PurchaseOrder", "ScheduleLine", "GrIrClearing", "MrpRun", "MaterialPlan"} {
			if strings.Contains(id, of) && !slices.Contains(demandagent.Never, id) {
				t.Errorf("never list: got %v, want it to hold %s", demandagent.Never, id)
			}
		}
	}
	for _, id := range demandagent.Never {
		t.Run(id, func(t *testing.T) {
			if slices.Contains(tools, id) {
				t.Errorf("%s is among the agent's tools", id)
			}
			if _, ok := ops[id]; !ok {
				t.Errorf("%s: not in the contract", id)
			}
		})
	}
}
