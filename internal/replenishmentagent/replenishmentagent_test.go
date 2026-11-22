package replenishmentagent_test

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
	"github.com/mustie7iy/agentic-erp/internal/replenishmentagent"
)

const (
	session = "01a1aa00-0000-7000-8000-000000000a01"
	run     = "01a1aa00-0000-7000-8000-000000000a02"
	le      = "01a1aa00-0000-7000-8000-000000000b01"
	otherLE = "01a1aa00-0000-7000-8000-000000000b02"
	hinge   = "01a1aa00-0000-7000-8000-000000000c01" // proposed at the run's day, with sources
	gasket  = "01a1aa00-0000-7000-8000-000000000c02" // proposed without a day, with no source
	ra      = "01a1aa00-0000-7000-8000-000000000d01" // the hinge's proposal, a draft
	rb      = "01a1aa00-0000-7000-8000-000000000d02" // the gasket's, a draft naming no day
	rc      = "01a1aa00-0000-7000-8000-000000000d03" // a proposal a later run replaced: cancelled
	rd      = "01a1aa00-0000-7000-8000-000000000d04" // a proposal never read
	s1      = "01a1aa00-0000-7000-8000-000000000e01" // an active supplier, of the catalog
	s2      = "01a1aa00-0000-7000-8000-000000000e02" // an active supplier, of the contract
	s3      = "01a1aa00-0000-7000-8000-000000000e03" // a blocked supplier, last ordered from
	s4      = "01a1aa00-0000-7000-8000-000000000e04" // a supplier never read
	s5      = "01a1aa00-0000-7000-8000-000000000e05" // an active customer only
	cat1    = "01a1aa00-0000-7000-8000-000000000f01" // the hinge in the catalog, from s1
	cat2    = "01a1aa00-0000-7000-8000-000000000f02" // retired
	cat3    = "01a1aa00-0000-7000-8000-000000000f03" // of another company
	cat4    = "01a1aa00-0000-7000-8000-000000000f04" // of s4
	cat5    = "01a1aa00-0000-7000-8000-000000000f05" // of s5
	con1    = "01a1aa00-0000-7000-8000-000000000f11" // active, valid this year: item 1 the hinge, item 2 no material
	con2    = "01a1aa00-0000-7000-8000-000000000f12" // expired
	con3    = "01a1aa00-0000-7000-8000-000000000f13" // active, valid next year
	con4    = "01a1aa00-0000-7000-8000-000000000f14" // active, valid next year: the gasket's only source, which no order by its day can use
	po1     = "01a1aa00-0000-7000-8000-000000000f21" // released, from s3
	po2     = "01a1aa00-0000-7000-8000-000000000f22" // a draft, from s1
	po3     = "01a1aa00-0000-7000-8000-000000000f23" // closed, from s1, two items of the hinge
)

// the answers of the fake API.
var answers = map[string]string{
	"/v1/mrp-runs/" + run: `{"id": "` + run + `", "plant_code": "P100", "plant_name": "Toronto plant", "legal_entity_id": "` + le + `", "company_code": "3000",
		"status": "completed", "run_date": "2026-11-20", "lines": [
		{"material_number": "MA-HINGE", "shortage": {"value": "30", "unit": "EA"}, "requisition_id": "` + ra + `", "requisition_number": "PR1"},
		{"material_number": "MA-GASKET", "shortage": {"value": "2.5", "unit": "KG"}, "requisition_id": "` + rb + `", "requisition_number": "PR2"},
		{"material_number": "MA-HINGE", "requisition_id": "` + rc + `", "requisition_number": "PR3"},
		{"material_number": "MA-OTHER", "requisition_id": "` + rd + `", "requisition_number": "PR4"},
		{"material_number": "MA-NONE", "shortage": {"value": "0", "unit": "EA"}}]}`,
	"/v1/requisitions/" + ra: requisition(ra, "draft", `"needed_by": "2026-11-21", `, hinge, "30", "EA"),
	"/v1/requisitions/" + rb: requisition(rb, "draft", "", gasket, "2.5", "KG"),
	"/v1/requisitions/" + rc: requisition(rc, "cancelled", "", hinge, "30", "EA"),
	"/v1/catalog-items": `{"items": [` + catalog(cat1, le, s1, hinge, "active") + `, ` + catalog(cat2, le, s1, hinge, "retired") + `, ` +
		catalog(cat3, otherLE, s1, hinge, "active") + `, ` + catalog(cat5, le, s5, hinge, "active") +
		`, {"id": "01a1aa00-0000-7000-8000-000000000f06", "legal_entity_id": "` + le + `", "supplier_id": "` + s1 + `", "status": "active"}]}`,
	"/v1/catalog-items/" + cat4: catalog(cat4, le, s4, hinge, "active"),
	"/v1/contracts": `{"items": [` + contract(con2, "expired", "2025-01-01", "2025-12-31") + `, ` + contract(con3, "active", "2027-01-01", "2027-12-31") + `, ` +
		`{"id": "` + con4 + `", "legal_entity_id": "` + le + `", "supplier_id": "` + s2 + `", "status": "active", "valid_from": "2027-01-01", "valid_to": "2027-12-31",
		"items": [{"item_no": 1, "material_id": "` + gasket + `", "unit_price": {"amount": "4.00", "currency": "CAD"}}]}]}`,
	"/v1/contracts/" + con1: contract(con1, "active", "2026-01-01", "2026-12-31"),
	"/v1/purchase-orders": `{"items": [` + order(po2, s1, "draft", hinge) + `, ` + order(po3, s1, "closed", hinge, hinge) + `, ` +
		order("01a1aa00-0000-7000-8000-000000000f24", s1, "released", "") + `]}`,
	"/v1/purchase-orders/" + po1:  order(po1, s3, "released", hinge),
	"/v1/business-partners/" + s1: partner(s1, "active", "supplier"),
	"/v1/business-partners/" + s2: partner(s2, "active", "supplier"),
	"/v1/business-partners/" + s3: partner(s3, "blocked", "supplier"),
	"/v1/business-partners/" + s5: partner(s5, "active", "customer"),
}

func requisition(id, status, day, material, value, unit string) string {
	return fmt.Sprintf(`{"id": %q, "legal_entity_id": %q, "status": %q, %s"items": [{"material_id": %q, "quantity": {"value": %q, "unit": %q},
		"unit_price": {"amount": "1.20", "currency": "CAD"}}]}`, id, le, status, day, material, value, unit)
}

func catalog(id, company, supplier, material, status string) string {
	return fmt.Sprintf(`{"id": %q, "legal_entity_id": %q, "supplier_id": %q, "material_id": %q, "status": %q, "unit_price": {"amount": "1.10", "currency": "CAD"}}`,
		id, company, supplier, material, status)
}

func contract(id, status, from, to string) string {
	return fmt.Sprintf(`{"id": %q, "legal_entity_id": %q, "supplier_id": %q, "status": %q, "valid_from": %q, "valid_to": %q, "items": [
		{"item_no": 1, "material_id": %q, "unit_price": {"amount": "1.05", "currency": "CAD"}}, {"item_no": 2, "unit_price": {"amount": "9.00", "currency": "CAD"}}]}`,
		id, le, s2, status, from, to, hinge)
}

func order(id, supplier, status string, materials ...string) string {
	items := make([]string, len(materials))
	for i, m := range materials {
		items[i] = fmt.Sprintf(`{"item_no": %d, "material_id": %q, "unit_price": {"amount": "1.15", "currency": "CAD"}}`, i+1, m)
	}
	return fmt.Sprintf(`{"id": %q, "legal_entity_id": %q, "supplier_id": %q, "status": %q, "items": [%s]}`, id, le, supplier, status, strings.Join(items, ", "))
}

func partner(id, status, role string) string {
	return fmt.Sprintf(`{"id": %q, "number": "BP1", "status": %q, "roles": [%q]}`, id, status, role)
}

// fakeAPI is a stand-in for the cell's API: the run and what its reads
// answer, a session and recommendations answered by their bodies; it
// keeps every request it was sent.
type fakeAPI struct {
	mu     sync.Mutex
	calls  []string
	runs   string // the run's answer instead of answers'
	status int    // every read's but the run's; 0: 200
	made   int
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
	path := r.URL.Path
	switch body, known := answers[path]; {
	case path == "/v1/mrp-runs/"+run && a.runs != "":
		answer(200, a.runs)
	case path == "/v1/agent-sessions":
		answer(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case strings.HasPrefix(path, "/v1/agent-sessions/"):
		answer(200, `{}`) // the session ended
	case path == "/v1/recommendations":
		a.recommended(b, answer)
	case a.status != 0 && path != "/v1/mrp-runs/"+run:
		answer(a.status, `{"code": "identity.permission_missing", "status": 403}`)
	case known:
		answer(200, body)
	default:
		answer(404, `{"code": "request.not_found", "status": 404}`)
	}
}

// recommended answers a recommendation with its body.
func (a *fakeAPI) recommended(b []byte, answer func(int, string)) {
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

// rec is a recommendation of the kind with the content on the
// requisition, in the session the conversation names.
func rec(requisition, kind, content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "requisition",
			"object_id": requisition, "kind": kind, "content": content}}
	})
}

// reads are the reads a run makes before it recommends.
var reads = []llmtest.Step{
	llmtest.Tool("getMrpRun", map[string]any{"id": run}),
	llmtest.Tool("getRequisition", map[string]any{"id": ra}),
	llmtest.Tool("getRequisition", map[string]any{"id": rb}),
	llmtest.Tool("getRequisition", map[string]any{"id": rc}),
	llmtest.Tool("listCatalogItems", map[string]any{"legal_entity_id": le, "text": "MA-HINGE"}),
	llmtest.Tool("getCatalogItem", map[string]any{"id": cat4}),
	llmtest.Tool("listContracts", map[string]any{"legal_entity_id": le, "material_id": hinge}),
	llmtest.Tool("getContract", map[string]any{"id": con1}),
	llmtest.Tool("listPurchaseOrders", map[string]any{"legal_entity_id": le, "material_id": hinge}),
	llmtest.Tool("getPurchaseOrder", map[string]any{"id": po1}),
	llmtest.Tool("getBusinessPartner", map[string]any{"id": s1}),
	llmtest.Tool("getBusinessPartner", map[string]any{"id": s2}),
	llmtest.Tool("getBusinessPartner", map[string]any{"id": s3}),
	llmtest.Tool("getBusinessPartner", map[string]any{"id": s5}),
}

// ordered is an order of the hinge's proposal: its first line, then its
// reasons.
func ordered(qty, unit, supplier, by, source string) string {
	return fmt.Sprintf("Order %s %s from supplier %s by %s: %s\nWhy: the run's shortage is 30 EA.", qty, unit, supplier, by, source)
}

// TestReview: the agent reviews the run's proposals in one session and
// recommends on each still to be ordered once. Its check refuses, and
// counts, every recommendation a buyer could not order from as it
// stands, or whose source, supplier or figures the run did not read;
// nothing refused reaches the API.
func TestReview(t *testing.T) {
	good := "Order 30 EA from supplier " + s2 + " by 2026-11-21: contract " + con1 + " item 1\nWhy: contract item 1 prices the hinge at 1.05 CAD, " +
		"below the catalog's 1.10 CAD; the run's shortage is 30 EA."
	none := "No catalog item, contract or purchase order of the gasket was found in the company: source it by a request for quotation."
	refusals := []struct {
		on, kind, content string
		want              error
	}{
		{ra, "order", "Order thirty hinges from the catalog.\nWhy: the shortage.", replenishmentagent.ErrForm},
		{ra, "order", strings.Split(ordered("30", "EA", s1, "2026-11-21", "catalog item "+cat1), "\n")[0], replenishmentagent.ErrForm},
		{ra, "order", ordered("30", "EA", s1, "2026-11-21", "catalog item "+cat1) + " At 1.17 CAD.", replenishmentagent.ErrUnread},
		{ra, "order", ordered("35", "EA", s1, "2026-11-21", "catalog item "+cat1), replenishmentagent.ErrQuantity},
		{ra, "order", ordered("30", "KG", s1, "2026-11-21", "catalog item "+cat1), replenishmentagent.ErrQuantity},
		{ra, "order", ordered("30", "EA", s1, "2026-11-20", "catalog item "+cat1), replenishmentagent.ErrWhen},
		{ra, "order", ordered("30", "EA", s1, "2026-11-21", "catalog item "+cat2), replenishmentagent.ErrSource},
		{ra, "order", ordered("30", "EA", s1, "2026-11-21", "catalog item "+cat3), replenishmentagent.ErrSource},
		{ra, "order", ordered("30", "EA", s2, "2026-11-21", "catalog item "+cat1), replenishmentagent.ErrSource},
		{ra, "order", ordered("30", "EA", s2, "2026-11-21", "contract "+con2+" item 1"), replenishmentagent.ErrSource},
		{ra, "order", ordered("30", "EA", s2, "2026-11-21", "contract "+con3+" item 1"), replenishmentagent.ErrSource},
		{ra, "order", ordered("30", "EA", s2, "2026-11-21", "contract "+con1+" item 2"), replenishmentagent.ErrSource},
		{ra, "order", ordered("30", "EA", s1, "2026-11-21", "purchase order "+po2), replenishmentagent.ErrSource},
		{ra, "order", ordered("30", "EA", s3, "2026-11-21", "purchase order "+po1), replenishmentagent.ErrSupplier},
		{ra, "order", ordered("30", "EA", s4, "2026-11-21", "catalog item "+cat4), replenishmentagent.ErrSupplier},
		{ra, "order", ordered("30", "EA", s5, "2026-11-21", "catalog item "+cat5), replenishmentagent.ErrSupplier},
		{ra, "source_needed", none, replenishmentagent.ErrHasSource},
		{rb, "source_needed", ordered("2.5", "KG", s1, "2026-11-20", "catalog item "+cat1), replenishmentagent.ErrForm},
		{rb, "source_needed", none + " It needs 2.75 KG.", replenishmentagent.ErrUnread},
		{rb, "order", ordered("2.5", "KG", s1, "2026-11-21", "catalog item "+cat1), replenishmentagent.ErrWhen},
		{rb, "order", ordered("2.5", "KG", s2, "2026-11-20", "contract "+con4+" item 1"), replenishmentagent.ErrSource},
		{rc, "order", ordered("30", "EA", s1, "2026-11-21", "catalog item "+cat1), replenishmentagent.ErrProposal},
		{rd, "source_needed", none, replenishmentagent.ErrProposal},
	}
	steps := slices.Clone(reads)
	for _, r := range refusals {
		steps = append(steps, rec(r.on, r.kind, r.content))
	}
	steps = append(steps, rec(ra, "order", good), rec(rb, "source_needed", none), rec(ra, "order", good),
		llmtest.Final("Recommended ordering the hinge on its contract and sourcing the gasket; PR3 was replaced and needs nothing."))
	a := &fakeAPI{}
	model := llmtest.New(t, steps...)
	res, err := (&replenishmentagent.Agent{LLM: model.Runner(), API: client(t, a), MaxIterations: len(steps)}).Review(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	want := []recommend.Made{{ID: "r1", ObjectType: "requisition", ObjectID: ra, Kind: "order"}, {ID: "r2", ObjectType: "requisition", ObjectID: rb, Kind: "source_needed"}}
	if !slices.Equal(res.Recommendations, want) {
		t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
	}
	if res.Refused != len(refusals)+1 {
		t.Fatalf("refused: got %d, want %d: the check's and the second on PR1", res.Refused, len(refusals)+1)
	}
	if got := len(a.sent("recommendations")); got != 2 {
		t.Errorf("recommendations sent: got %d, want 2: the check keeps the others from the API", got)
	}
	if got, want := res.Names[ra], "requisition PR1 of MA-HINGE"; got != want {
		t.Errorf("the hinge's proposal named: got %q, want %q", got, want)
	}
	prompt := "Planning run " + run + " of plant P100, Toronto plant, in company 3000 (legal entity " + le + "), planned on 2026-11-20. " +
		"Recommend, for each requisition it proposed, what to order, from which supplier and when."
	if !strings.Contains(model.Requests[0], prompt) {
		t.Errorf("prompt: got %s, want %q", model.Requests[0], prompt)
	}
	// Each refusal is told in the request after its recommendation.
	for i, r := range refusals {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			told, _ := json.Marshal(r.want.Error()) // the model reads the refusal as JSON text
			msg := strings.Trim(string(told), `"`)
			if before, after := strings.Count(model.Requests[len(reads)+i], msg), strings.Count(model.Requests[len(reads)+i+1], msg); after != before+1 {
				t.Errorf("refusal of %q: told %d times before, %d after; want %v once more", r.content, before, after, r.want)
			}
		})
	}
}

// TestReviewWithoutReads: a recommendation before the run read its
// proposal, or after the API refused it every read, is refused: the
// check knows only what the run read, and nothing reaches the API.
func TestReviewWithoutReads(t *testing.T) {
	proposed := rec(ra, "order", ordered("30", "EA", s1, "2026-11-21", "catalog item "+cat1))
	for name, c := range map[string]struct {
		a     *fakeAPI
		steps []llmtest.Step
	}{
		"nothing read":  {&fakeAPI{}, []llmtest.Step{proposed}},
		"reads refused": {&fakeAPI{status: 403}, append(slices.Clone(reads), proposed)},
	} {
		t.Run(name, func(t *testing.T) {
			model := llmtest.New(t, append(c.steps, llmtest.Final("Nothing read, nothing recommended."))...)
			res, err := (&replenishmentagent.Agent{LLM: model.Runner(), API: client(t, c.a)}).Review(context.Background(), run)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Recommendations) != 0 || res.Refused != 1 {
				t.Errorf("got %v, refused %d, want none made and one refused", res.Recommendations, res.Refused)
			}
			if got := len(c.a.sent("recommendations")); got != 0 {
				t.Errorf("recommendations sent: got %d, want none", got)
			}
		})
	}
}

// TestReviewBefore: a run the agent may not read, one not completed and
// an API that is gone stop the run before a session starts; a run that
// proposed nothing starts none and says so.
func TestReviewBefore(t *testing.T) {
	for name, c := range map[string]struct {
		runs, id string
		want     string
	}{
		"not found":    {"", "01a1aa00-0000-7000-8000-000000000a09", "request.not_found"},
		"still queued": {`{"id": "` + run + `", "status": "queued", "lines": []}`, run, replenishmentagent.ErrNotCompleted.Error() + ": " + run + " is queued"},
		"failed": {`{"id": "` + run + `", "status": "failed", "error": {"code": "config.no_number_range"}, "lines": []}`, run,
			replenishmentagent.ErrNotCompleted.Error() + ": " + run + " failed with config.no_number_range"},
		"not its shape": {`{"id": 7}`, run, "decode response body"},
	} {
		t.Run(name, func(t *testing.T) {
			a := &fakeAPI{runs: c.runs}
			_, err := (&replenishmentagent.Agent{LLM: llmtest.New(t).Runner(), API: client(t, a)}).Review(context.Background(), c.id)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
			if got := len(a.sent("agent-sessions")); got != 0 {
				t.Errorf("sessions: got %d, want none", got)
			}
		})
	}
	t.Run("nothing proposed", func(t *testing.T) {
		a := &fakeAPI{runs: `{"id": "` + run + `", "plant_code": "P100", "status": "completed", "lines": [{"material_number": "MA-NONE"}]}`}
		res, err := (&replenishmentagent.Agent{LLM: llmtest.New(t).Runner(), API: client(t, a)}).Review(context.Background(), run)
		if err != nil {
			t.Fatal(err)
		}
		if want := "Planning run " + run + " of plant P100 proposed nothing: no recommendation."; res.Reply != want || res.Session != "" {
			t.Errorf("got %+v, want no session and the reply %q", res, want)
		}
	})
	t.Run("the API gone", func(t *testing.T) {
		gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
		if _, err := (&replenishmentagent.Agent{LLM: llmtest.New(t).Runner(), API: gone}).Review(context.Background(), run); err == nil {
			t.Error("got no error, want the read's")
		}
	})
}

// TestNever: the agent's tools are reads open to agents and the one
// recommendation; every write of requisitions, purchase orders, planning
// runs and planning data is on its never list, by its name or by its
// path (an intercompany order's goods issue, postIntercompanyGoodsIssue,
// names no purchase order but is one's action), and none of the list is
// among its tools (ADR-0072, S2l).
func TestNever(t *testing.T) {
	ops := api.MustOperations()
	tools := replenishmentagent.Tools()
	for _, id := range tools {
		op, ok := ops[id]
		if !ok || !op.AgentAllowed || (op.Method != http.MethodGet && !op.Reads && id != recommend.Proposer) {
			t.Errorf("%s: got in the contract %v, open to agents %v, method %s; want a read open to agents", id, ok, op.AgentAllowed, op.Method)
		}
	}
	for id, op := range ops {
		writes := op.Method != http.MethodGet && !op.Reads
		for _, of := range []string{"Requisition", "PurchaseOrder", "ScheduleLine", "MrpRun", "MaterialPlan"} {
			if writes && strings.Contains(id, of) && !slices.Contains(replenishmentagent.Never, id) {
				t.Errorf("never list: got %v, want it to hold %s", replenishmentagent.Never, id)
			}
		}
		for _, path := range []string{"/v1/requisitions", "/v1/purchase-orders"} {
			if writes && strings.HasPrefix(op.Path, path) && !slices.Contains(replenishmentagent.Never, id) {
				t.Errorf("never list: got %v, want it to hold %s (%s %s)", replenishmentagent.Never, id, op.Method, op.Path)
			}
		}
	}
	for _, id := range replenishmentagent.Never {
		if _, ok := ops[id]; !ok || slices.Contains(tools, id) {
			t.Errorf("%s: got in the contract %v, among the tools %v; want in the contract and no tool", id, ok, slices.Contains(tools, id))
		}
	}
}

// TestReviewFromHistory: the supplier last ordered from is a source of
// the material: an order from a closed purchase order with two items of
// it is recommended.
func TestReviewFromHistory(t *testing.T) {
	model := llmtest.New(t, llmtest.Tool("getRequisition", map[string]any{"id": ra}),
		llmtest.Tool("listPurchaseOrders", map[string]any{"legal_entity_id": le, "material_id": hinge}),
		llmtest.Tool("getBusinessPartner", map[string]any{"id": s1}),
		rec(ra, "order", "Order 30 EA from supplier "+s1+" by 2026-11-21: purchase order "+po3+"\nWhy: last ordered from it at 1.15 CAD."),
		llmtest.Final("Recommended ordering the hinge from the supplier last ordered from."))
	res, err := (&replenishmentagent.Agent{LLM: model.Runner(), API: client(t, &fakeAPI{})}).Review(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if want := []recommend.Made{{ID: "r1", ObjectType: "requisition", ObjectID: ra, Kind: "order"}}; !slices.Equal(res.Recommendations, want) || res.Refused != 0 {
		t.Errorf("got %v, refused %d, want %v and none refused", res.Recommendations, res.Refused, want)
	}
}
