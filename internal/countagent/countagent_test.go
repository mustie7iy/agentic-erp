package countagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/countagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

const (
	session = "01a1aa00-0000-7000-8000-000000000a01"
	le      = "01a1aa00-0000-7000-8000-000000000b01"
	pl      = "01a1aa00-0000-7000-8000-000000000b02"
	other   = "01a1aa00-0000-7000-8000-000000000b03" // another plant of the company
	main    = "01a1aa00-0000-7000-8000-000000000c01" // SL01
	spares  = "01a1aa00-0000-7000-8000-000000000c02" // SL02
	matA    = "01a1aa00-0000-7000-8000-000000000d01" // most of the consumption, counted 100 days ago
	matB    = "01a1aa00-0000-7000-8000-000000000d02" // the next 15 percent, never counted
	matC    = "01a1aa00-0000-7000-8000-000000000d03" // the rest, two movements, counted a year and a day ago
	matN    = "01a1aa00-0000-7000-8000-000000000d04" // no consumption, never counted
	batch   = "01a1aa00-0000-7000-8000-000000000d05" // batch-managed: counts refuse it
	serial  = "01a1aa00-0000-7000-8000-000000000d06" // serial-managed: counts refuse it
	pending = "01a1aa00-0000-7000-8000-000000000d07" // pending its approval
	opened  = "01a1aa00-0000-7000-8000-000000000d08" // an open count holds it
	fresh   = "01a1aa00-0000-7000-8000-000000000d09" // class C, counted ten days ago
	unknown = "01a1aa00-0000-7000-8000-000000000d10" // in stock, not among the plant's materials
)

// now is the run's instant: 21 November 2026 in UTC, the company's zone.
var now = func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) }

const stock = `{"items": [
	{"material_id": "` + matA + `", "material_number": "MA1", "storage_location_id": "` + main + `", "storage_location_code": "SL01"},
	{"material_id": "` + matB + `", "material_number": "MA2", "storage_location_id": "` + main + `", "storage_location_code": "SL01"},
	{"material_id": "` + matC + `", "material_number": "MA3", "storage_location_id": "` + spares + `", "storage_location_code": "SL02"},
	{"material_id": "` + matN + `", "material_number": "MA4", "storage_location_id": "` + main + `", "storage_location_code": "SL01"},
	{"material_id": "` + batch + `", "material_number": "MA5", "storage_location_id": "` + main + `", "storage_location_code": "SL01"},
	{"material_id": "` + serial + `", "material_number": "MA6", "storage_location_id": "` + main + `", "storage_location_code": "SL01"},
	{"material_id": "` + pending + `", "material_number": "MA7", "storage_location_id": "` + main + `", "storage_location_code": "SL01"},
	{"material_id": "` + opened + `", "material_number": "MA8", "storage_location_id": "` + main + `", "storage_location_code": "SL01"},
	{"material_id": "` + fresh + `", "material_number": "MA9", "storage_location_id": "` + main + `", "storage_location_code": "SL01"},
	{"material_id": "` + unknown + `", "material_number": "MA0", "storage_location_id": "` + main + `", "storage_location_code": "SL01"}]}`

const materials = `{"items": [{"id": "` + matA + `", "status": "active"}, {"id": "` + matB + `", "status": "blocked"}, {"id": "` + matC + `", "status": "active"},
	{"id": "` + matN + `", "status": "active"}, {"id": "` + batch + `", "status": "active", "batch_managed": true},
	{"id": "` + serial + `", "status": "active", "serial_managed": true}, {"id": "` + pending + `", "status": "pending"},
	{"id": "` + opened + `", "status": "active"}, {"id": "` + fresh + `", "status": "active"}]}`

// line is a document line of the material at the location, worth amount
// (none when empty) at plant.
func line(material, location, plant, amount string, inTransit bool) string {
	value := ""
	if amount != "" {
		value = `, "amount": {"amount": "` + amount + `", "currency": "CAD"}`
	}
	return fmt.Sprintf(`{"material_id": "%s", "plant_id": "%s", "storage_location_id": "%s", "in_transit": %t%s}`, material, plant, location, inTransit, value)
}

func doc(date, movement string, lines ...string) string {
	return `{"posting_date": "` + date + `", "movement_type": "` + movement + `", "lines": [` + strings.Join(lines, ", ") + `]}`
}

// documents are the plant's material documents in the order they were
// created, the latest first: a goods receipt created today but dated
// before the year and one dated after the day, both left out, then A's
// issue of 800.00, B's scrapping of 150.00, C's goods issue of 60.00 less
// a reversal of 10.00, a receipt of C (no consumption), a transfer of A
// to the other plant whose receipt there is not counted, an in-transit
// line, then documents posted before the year, one in another currency,
// left out whatever they hold.
var documents = `{"items": [` + strings.Join([]string{
	doc("2025-11-10", "201", line(matN, main, pl, "-1000.00", false)),
	doc("2026-11-22", "201", line(matN, main, pl, "-1000.00", false)),
	doc("2026-11-20", "201", line(matA, main, pl, "-800.00", false)),
	doc("2026-11-19", "551", line(matB, main, pl, "-150.00", false)),
	doc("2026-11-18", "602", line(matC, spares, pl, "10.00", false)),
	doc("2026-11-17", "601", line(matC, spares, pl, "-60.00", false)),
	doc("2026-11-16", "501", line(matB, main, pl, "150.00", false)),
	doc("2026-11-15", "301", line(matA, main, pl, "-5.00", false), line(matA, "far", other, "5.00", false)),
	doc("2026-11-14", "351", line(matN, main, pl, "", true)),
	doc("2025-11-21", "201", line(matN, main, pl, "-1000.00", false)),
	`{"posting_date": "2025-11-01", "movement_type": "201", "lines": [{"plant_id": "` + pl + `", "amount": {"amount": "1", "currency": "EUR"}}]}`,
}, ", ") + `]}`

// counts are the plant's count documents: A posted 100 days ago, C a year
// and a day ago and once more two years ago, the fresh one ten days ago,
// one rejected for B, and an open one holding the opened material.
const counts = `{"items": [
	{"number": "PI5", "document_date": "2026-11-11", "status": "posted", "storage_location_id": "` + main + `", "items": [{"material_id": "` + fresh + `"}]},
	{"number": "PI4", "document_date": "2026-11-01", "status": "counting", "storage_location_id": "` + main + `", "items": [{"material_id": "` + opened + `"}]},
	{"number": "PI3", "document_date": "2026-10-01", "status": "rejected", "storage_location_id": "` + main + `", "items": [{"material_id": "` + matB + `"}]},
	{"number": "PI2", "document_date": "2026-08-13", "status": "posted", "storage_location_id": "` + main + `", "items": [{"material_id": "` + matA + `"}]},
	{"number": "PI1", "document_date": "2025-11-20", "status": "posted", "storage_location_id": "` + spares + `", "items": [{"material_id": "` + matC + `"}]},
	{"number": "PI0", "document_date": "2024-11-20", "status": "posted", "storage_location_id": "` + spares + `", "items": [{"material_id": "` + matC + `"}]}]}`

// The lines the run computes, in their rank.
const (
	lineA = "Count MA1 at P100/SL01 (material " + matA + ", storage location " + main + "): class A, 2 movements since 2025-11-22, last counted on 2026-08-13 (PI2)"
	lineB = "Count MA2 at P100/SL01 (material " + matB + ", storage location " + main + "): class B, 2 movements since 2025-11-22, never counted"
	lineC = "Count MA3 at P100/SL02 (material " + matC + ", storage location " + spares + "): class C, 2 movements since 2025-11-22, last counted on 2025-11-20 (PI1)"
	lineN = "Count MA4 at P100/SL01 (material " + matN + ", storage location " + main + "): class C, 0 movements since 2025-11-22, never counted"
)

// fakeAPI is a stand-in for the cell's API: company 3000 with plants
// P100 and P200, the stock, materials, documents and counts above, a
// session and recommendations answered by their bodies. It keeps every
// request it was sent.
type fakeAPI struct {
	mu         sync.Mutex
	calls      []string
	refuse     string // a path answered 403
	inactive   bool   // P100 is blocked
	stock      string // replaces the stock
	documents  string // replaces the documents
	indicators string // the materials' cycle counting indicators; none when empty
}

func (a *fakeAPI) answer(path string) (int, string) {
	if path == a.refuse {
		return 403, `{"code": "identity.permission_missing", "status": 403}`
	}
	status := map[bool]string{false: "active", true: "blocked"}[a.inactive]
	switch path {
	case "/v1/legal-entities":
		return 200, `{"items": [{"id": "` + le + `", "code": "3000"}]}`
	case "/v1/org-units":
		return 200, `{"items": [{"id": "` + other + `", "legal_entity_id": "` + le + `", "code": "P200", "name": "Montreal plant", "status": "active"},
			{"id": "` + pl + `", "legal_entity_id": "` + le + `", "code": "P100", "name": "Toronto plant", "status": "` + status + `"}]}`
	case "/v1/legal-entities/" + le + "/today":
		return 200, `{"time_zone": "UTC"}`
	case "/v1/stock":
		return 200, cmpOr(a.stock, stock)
	case "/v1/materials":
		return 200, materials
	case "/v1/material-documents":
		return 200, cmpOr(a.documents, documents)
	case "/v1/physical-inventory-documents":
		return 200, counts
	case "/v1/cycle-count-materials":
		return 200, cmpOr(a.indicators, `{"items": []}`)
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
		write(a.answer(r.URL.Path))
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

// propose is a count proposal on the plant with the content.
func propose(content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "plant",
			"object_id": pl, "kind": "count_proposal", "content": content}}
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

func agentOn(t *testing.T, a *fakeAPI, model *llmtest.Server) *countagent.Agent {
	t.Helper()
	return &countagent.Agent{LLM: model.Runner(), API: client(t, a), Now: now}
}

// TestPlan: the run classes the plant's materials by their consumption
// of the year (A 800.00 of 1000.00, B the next 150.00, C the 50.00 left
// and none), counts their movements at each storage location (not in
// transit, not at another plant) and finds them due: A last counted 100
// days ago, past its 90; B never (a rejected count is none); C a year and
// a day ago; the one without consumption never. The batch- and
// serial-managed, the pending, the one an open count holds, the one
// counted ten days ago and one that is no material of the plant are
// left out, and so are documents dated outside the year, wherever the list has them. Its check
// refuses a line not given, lines out of their order or twice, a content
// naming none, and a number the run did not read; nothing refused
// reaches the API.
func TestPlan(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t,
		llmtest.Tool("listPhysicalInventoryDocuments", map[string]any{"plant_id": pl}),
		llmtest.Tool("listMaterialDocuments", map[string]any{"plant_id": pl}),
		propose("Count MA1 at P100/SL01: class A\nIt moves most."),
		propose(lineB+"\n"+lineA+"\nB first."),
		propose(lineA+"\n"+lineA+"\nTwice."),
		propose("All of them, they are due."),
		propose(lineA+"\nIt moved 4711 times."),
		propose(lineA+"\n"+lineB+"\n"+lineN+"\nMA1 was issued for 800.00 CAD and MA2 scrapped for 150.00 CAD; MA4 was never counted. MA3 is left for next week."),
		llmtest.Final("Proposed three counts at P100; the controller decides."))
	res, err := agentOn(t, a, model).Plan(context.Background(), "3000", "P100")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Recommendations) != 1 || res.Recommendations[0].Kind != countagent.Proposal || res.Recommendations[0].ObjectID != pl {
		t.Fatalf("recommendations: got %v, want one count_proposal on the plant", res.Recommendations)
	}
	if res.Refused != 5 {
		t.Errorf("refused: got %d, want 5", res.Refused)
	}
	if got := len(a.sent("POST /v1/recommendations")); got != 1 {
		t.Errorf("recommendations sent: got %d, want 1: the check keeps the others from the API", got)
	}
	want := "Due for a count, ranked, each with its line:\\n" + lineA + "\\n" + lineB + "\\n" + lineC + "\\n" + lineN + "\\nYou may"
	if !strings.Contains(model.Requests[0], want) {
		t.Errorf("prompt: got %s, want %q", model.Requests[0], want)
	}
	for _, not := range []string{"MA5", "MA6", "MA7", "MA8", "MA9", "MA0", "more are due"} {
		if strings.Contains(model.Requests[0], not) {
			t.Errorf("prompt: got %s, want no %s", model.Requests[0], not)
		}
	}
	for _, e := range []error{countagent.ErrLine, countagent.ErrOrder, countagent.ErrForm, countagent.ErrUnread} {
		refusedWith(t, model.Requests, e)
	}
	if got, want := res.Names[pl], "P100 Toronto plant"; got != want {
		t.Errorf("the plant's name: got %q, want %q", got, want)
	}
	if got := a.sent("plant_id=" + pl); len(got) != 7 {
		t.Errorf("reads of the plant: got %v, want the stock, materials, indicators, documents and counts before the session and the model's two", got)
	}
}

// TestPlanIndicators: a material with a cycle counting indicator at the
// plant is due by its indicator's interval rather than its class's, and
// its line names the indicator (ADR-0082 As built S2c2): the one counted
// ten days ago is due every 7 days, A counted 100 days ago is not due
// every 365; the others keep their class's.
func TestPlanIndicators(t *testing.T) {
	model := llmtest.New(t, llmtest.Final("Nothing proposed."))
	a := &fakeAPI{indicators: `{"items": [{"material_id": "` + fresh + `", "indicator_code": "W", "interval_days": 7},
		{"material_id": "` + matA + `", "indicator_code": "Y", "interval_days": 365}]}`}
	if _, err := agentOn(t, a, model).Plan(context.Background(), "3000", "P100"); err != nil {
		t.Fatal(err)
	}
	fresh := "Count MA9 at P100/SL01 (material " + fresh + ", storage location " + main + "): class C, cycle counting indicator W every 7 days, " +
		"0 movements since 2025-11-22, last counted on 2026-11-11 (PI5)"
	want := "Due for a count, ranked, each with its line:\\n" + lineB + "\\n" + lineC + "\\n" + lineN + "\\n" + fresh + "\\nYou may"
	if !strings.Contains(model.Requests[0], want) {
		t.Errorf("prompt: got %s, want %q", model.Requests[0], want)
	}
	// An indicator taken off (S2c3) is not read: the material falls back to
	// its class's interval.
	if got, want := a.calls, "GET /v1/cycle-count-materials?page_size=500&plant_id="+pl+"&status=active"; !slices.Contains(got, want) {
		t.Errorf("calls: got %v, want %s among them", got, want)
	}
}

// TestPlanMaxLines: a run lists the first MaxLines due materials and says
// how many more are due.
func TestPlanMaxLines(t *testing.T) {
	var rows []string
	for i := range countagent.MaxLines + 2 {
		rows = append(rows, fmt.Sprintf(`{"material_id": "%s", "material_number": "MA%02d", "storage_location_id": "sl%02d", "storage_location_code": "S%02d"}`,
			matN, i, i, i))
	}
	model := llmtest.New(t, llmtest.Final("Nothing proposed."))
	if _, err := agentOn(t, &fakeAPI{stock: `{"items": [` + strings.Join(rows, ", ") + `]}`}, model).Plan(context.Background(), "3000", "P100"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(model.Requests[0], "Count MA"); got != countagent.MaxLines {
		t.Errorf("lines listed: got %d, want %d", got, countagent.MaxLines)
	}
	if !strings.Contains(model.Requests[0], "2 more are due and not listed") {
		t.Errorf("prompt: got %s, want the two more named", model.Requests[0])
	}
	if strings.Contains(model.Requests[0], "MA41") {
		t.Errorf("prompt: got %s, want the last left out", model.Requests[0])
	}
}

// TestPlanOneMovement: a material moved once is named so.
func TestPlanOneMovement(t *testing.T) {
	model := llmtest.New(t, llmtest.Final("Nothing proposed."))
	a := &fakeAPI{documents: `{"items": [` + doc("2026-11-20", "561", line(matN, main, pl, "4.00", false)) + `]}`,
		stock: `{"items": [{"material_id": "` + matN + `", "material_number": "MA4", "storage_location_id": "` + main + `", "storage_location_code": "SL01"}]}`}
	if _, err := agentOn(t, a, model).Plan(context.Background(), "3000", "P100"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.Requests[0], "class C, 1 movement since 2025-11-22, never counted") {
		t.Errorf("prompt: got %s, want one movement", model.Requests[0])
	}
}

// TestPlanNothingDue: a plant with nothing due gets no session and a
// reply saying so.
func TestPlanNothingDue(t *testing.T) {
	a := &fakeAPI{stock: `{"items": []}`}
	res, err := agentOn(t, a, llmtest.New(t)).Plan(context.Background(), "3000", "P100")
	if err != nil {
		t.Fatal(err)
	}
	if res.Session != "" || len(res.Recommendations) != 0 {
		t.Errorf("result: got %+v, want no session and no recommendation", res)
	}
	if want := "Nothing is due for a count at plant P100 on 2026-11-21"; !strings.HasPrefix(res.Reply, want) {
		t.Errorf("reply: got %q, want it to start %q", res.Reply, want)
	}
	if got := a.sent("agent-sessions"); len(got) != 0 {
		t.Errorf("sessions: got %v, want none", got)
	}
}

// TestPlanRefusals: a company, plant or read the agent may not have, a
// plant not active, amounts it cannot add up and an API that is gone stop
// the run before a session starts.
func TestPlanRefusals(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	huge := `{"items": [` + doc("2026-11-20", "201", line(matA, main, pl, "-92233720368547758.07", false), line(matA, main, pl, "-0.01", false)) + `]}`
	sum := `{"items": [` + doc("2026-11-20", "201", line(matA, main, pl, "-92233720368547758.07", false), line(matB, main, pl, "-0.01", false)) + `]}`
	euro := `{"items": [` + doc("2026-11-20", "201", line(matA, main, pl, "-8.00", false)) + `, {"posting_date": "2026-11-19", "movement_type": "551",
		"lines": [{"plant_id": "` + pl + `", "amount": {"amount": "1.00", "currency": "EUR"}}]}]}`
	for name, c := range map[string]struct {
		api            *apitools.Client
		company, plant string
		want           string
	}{
		"no such company":     {client(t, &fakeAPI{}), "9999", "P100", "countagent: company 9999: apitools"},
		"the plants":          {client(t, &fakeAPI{refuse: "/v1/org-units"}), "3000", "P100", "the plants of company 3000"},
		"no such plant":       {client(t, &fakeAPI{}), "3000", "P900", "no plant P900 in company 3000"},
		"a blocked plant":     {client(t, &fakeAPI{inactive: true}), "3000", "P100", "P100 is blocked"},
		"the day":             {client(t, &fakeAPI{refuse: "/v1/legal-entities/" + le + "/today"}), "3000", "P100", "the day of company 3000"},
		"the stock":           {client(t, &fakeAPI{refuse: "/v1/stock"}), "3000", "P100", "the stock of plant P100"},
		"the materials":       {client(t, &fakeAPI{refuse: "/v1/materials"}), "3000", "P100", "the materials of plant P100"},
		"the documents":       {client(t, &fakeAPI{refuse: "/v1/material-documents"}), "3000", "P100", "the material documents of plant P100"},
		"the counts":          {client(t, &fakeAPI{refuse: "/v1/physical-inventory-documents"}), "3000", "P100", "the count documents of plant P100"},
		"the indicators":      {client(t, &fakeAPI{refuse: "/v1/cycle-count-materials"}), "3000", "P100", "the cycle counting indicators of plant P100"},
		"another currency":    {client(t, &fakeAPI{documents: euro}), "3000", "P100", "an amount in EUR beside CAD"},
		"another fraction":    {client(t, &fakeAPI{documents: `{"items": [` + doc("2026-11-20", "201", line(matA, main, pl, "-8.00", false), line(matA, main, pl, "-8.5", false)) + `]}`}), "3000", "P100", `amount "-8.5" must have exactly 2 decimals`},
		"a material too much": {client(t, &fakeAPI{documents: huge}), "3000", "P100", "the consumption of material " + matA},
		"the plant too much":  {client(t, &fakeAPI{documents: sum}), "3000", "P100", "the plant's consumption"},
		"the API gone at all": {gone, "3000", "P100", "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&countagent.Agent{LLM: llmtest.New(t).Runner(), API: c.api, Now: now}).Plan(context.Background(), c.company, c.plant)
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
// is an operation of its never list, which holds every write of count
// documents and goods movements (ADR-0072, S2e).
func TestNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	tools := countagent.Tools()
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
		for _, of := range []string{"PhysicalInventory", "CycleCount", "MaterialDocument", "GoodsReceipt", "GoodsIssue"} {
			if strings.Contains(id, of) && !slices.Contains(countagent.Never, id) {
				t.Errorf("never list: got %v, want it to hold %s", countagent.Never, id)
			}
		}
	}
	for _, id := range countagent.Never {
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
