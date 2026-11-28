package replenishmentagent_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	plant   = "01a1aa00-0000-7000-8000-000000000a03" // the run's plant
	le      = "01a1aa00-0000-7000-8000-000000000b01"
	otherLE = "01a1aa00-0000-7000-8000-000000000b02"
	hinge   = "01a1aa00-0000-7000-8000-000000000c01" // proposed at the run's day, with sources
	gasket  = "01a1aa00-0000-7000-8000-000000000c02" // proposed without a day, with no source
	ra      = "01a1aa00-0000-7000-8000-000000000d01" // the hinge's proposal, a draft
	rb      = "01a1aa00-0000-7000-8000-000000000d02" // the gasket's, a draft naming no day
	rc      = "01a1aa00-0000-7000-8000-000000000d03" // a proposal a later run replaced: cancelled
	rd      = "01a1aa00-0000-7000-8000-000000000d04" // a proposal never read
	re      = "01a1aa00-0000-7000-8000-000000000d05" // a proposal of two lots (ADR-0084 As built S2k2), a draft
	rf      = "01a1aa00-0000-7000-8000-000000000d06" // a proposal of lots with fractions, answered out of item order
	rg      = "01a1aa00-0000-7000-8000-000000000d07" // items of two materials: no proposal of one
	rh      = "01a1aa00-0000-7000-8000-000000000d08" // items in two units: no proposal of one
	ri      = "01a1aa00-0000-7000-8000-000000000d09" // no item
	rj      = "01a1aa00-0000-7000-8000-000000000d0a" // a quantity that is no plain decimal
	rk      = "01a1aa00-0000-7000-8000-000000000d0b" // a proposal in weekly lots (ADR-0084 As built S2k3), needed by its first lot's day
	rl      = "01a1aa00-0000-7000-8000-000000000d0c" // weekly lots, the last one after con1's validity ends
	rm      = "01a1aa00-0000-7000-8000-000000000d0d" // weekly lots whose items a person has changed: no longer the run line's
	rn      = "01a1aa00-0000-7000-8000-000000000d0e" // a run line's lots not in the contract's shape: no day of the line's
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
	"/v1/mrp-runs/" + run: `{"id": "` + run + `", "plant_id": "` + plant + `", "plant_code": "P100", "plant_name": "Toronto plant", "legal_entity_id": "` + le + `", "company_code": "3000",
		"status": "completed", "run_date": "2026-11-20", "lines": [
		{"material_number": "MA-HINGE", "shortage": {"value": "30", "unit": "EA"}, "requisition_id": "` + ra + `", "requisition_number": "PR1"},
		{"material_number": "MA-GASKET", "shortage": {"value": "2.5", "unit": "KG"}, "requisition_id": "` + rb + `", "requisition_number": "PR2"},
		{"material_number": "MA-HINGE", "requisition_id": "` + rc + `", "requisition_number": "PR3"},
		{"material_number": "MA-OTHER", "requisition_id": "` + rd + `", "requisition_number": "PR4"},
		{"material_number": "MA-HINGE", "lot_size": "replenish_to_maximum", "lots": 2, "requisition_id": "` + re + `", "requisition_number": "PR5"},
		{"material_number": "MA-HINGE", "lot_size": "replenish_to_maximum", "lots": 3, "requisition_id": "` + rf + `", "requisition_number": "PR6"},
		{"material_number": "MA-HINGE", "requisition_id": "` + rg + `", "requisition_number": "PR7"},
		{"material_number": "MA-HINGE", "requisition_id": "` + rh + `", "requisition_number": "PR8"},
		{"material_number": "MA-HINGE", "requisition_id": "` + ri + `", "requisition_number": "PR9"},
		{"material_number": "MA-HINGE", "requisition_id": "` + rj + `", "requisition_number": "PR10"},
		{"material_number": "MA-HINGE", "lot_size": "weekly", "lots": 2, "requisition_id": "` + rk + `", "requisition_number": "PR11",
			"lot_schedule": [{"quantity": {"value": "30", "unit": "EA"}, "needed_by": "2026-11-23"}, {"quantity": {"value": "20", "unit": "EA"}, "needed_by": "2026-11-30"}]},
		{"material_number": "MA-HINGE", "lot_size": "weekly", "lots": 2, "requisition_id": "` + rl + `", "requisition_number": "PR12",
			"lot_schedule": [{"quantity": {"value": "30", "unit": "EA"}, "needed_by": "2026-12-28"}, {"quantity": {"value": "20", "unit": "EA"}, "needed_by": "2027-01-04"}]},
		{"material_number": "MA-HINGE", "lot_size": "weekly", "lots": 2, "requisition_id": "` + rm + `", "requisition_number": "PR13",
			"lot_schedule": [{"quantity": {"value": "30", "unit": "EA"}, "needed_by": "2026-11-23"}, {"quantity": {"value": "20", "unit": "EA"}, "needed_by": "2026-11-30"}]},
		{"material_number": "MA-HINGE", "lot_size": "weekly", "lots": 2, "requisition_id": "` + rn + `", "requisition_number": "PR14",
			"lot_schedule": [{"quantity": {"value": "30", "unit": "EA"}, "needed_by": "2026-11-23"}, {"quantity": {"value": "x", "unit": "EA"}}]},
		{"material_number": "MA-NONE", "shortage": {"value": "0", "unit": "EA"}}]}`,
	"/v1/requisitions/" + ra: requisition(ra, "draft", `"needed_by": "2026-11-21", `, hinge, "30", "EA"),
	"/v1/requisitions/" + rb: requisition(rb, "draft", "", gasket, "2.5", "KG"),
	"/v1/requisitions/" + rc: requisition(rc, "cancelled", "", hinge, "30", "EA"),
	"/v1/requisitions/" + re: lots(re, lot{1, hinge, "25", "EA"}, lot{2, hinge, "15", "EA"}),
	"/v1/requisitions/" + rf: lots(rf, lot{2, hinge, "7.5", "EA"}, lot{1, hinge, "12.5", "EA"}, lot{3, hinge, "0.25", "EA"}),
	"/v1/requisitions/" + rg: lots(rg, lot{1, hinge, "20", "EA"}, lot{2, gasket, "20", "EA"}),
	"/v1/requisitions/" + rh: lots(rh, lot{1, hinge, "20", "EA"}, lot{2, hinge, "20", "BOX"}),
	"/v1/requisitions/" + ri: lots(ri),
	"/v1/requisitions/" + rj: lots(rj, lot{1, hinge, "-3", "EA"}),
	"/v1/requisitions/" + rk: lotsBy(rk, "2026-11-23", lot{1, hinge, "30", "EA"}, lot{2, hinge, "20.000", "EA"}),
	"/v1/requisitions/" + rl: lotsBy(rl, "2026-12-28", lot{1, hinge, "30", "EA"}, lot{2, hinge, "20", "EA"}),
	"/v1/requisitions/" + rm: lotsBy(rm, "2026-11-23", lot{1, hinge, "30", "EA"}, lot{2, hinge, "25", "EA"}),
	"/v1/requisitions/" + rn: lotsBy(rn, "2026-11-23", lot{1, hinge, "30", "EA"}, lot{2, hinge, "20", "EA"}),
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
	return fmt.Sprintf(`{"id": %q, "legal_entity_id": %q, "status": %q, %s"items": [{"item_no": 1, "material_id": %q, "quantity": {"value": %q, "unit": %q},
		"unit_price": {"amount": "1.20", "currency": "CAD"}}]}`, id, le, status, day, material, value, unit)
}

// lot is a requisition's item: its number, material and quantity.
type lot struct {
	no              int
	material, value string
	unit            string
}

// lots is a draft requisition needed by the day after the run's, of the
// items.
func lots(id string, items ...lot) string { return lotsBy(id, "2026-11-21", items...) }

// lotsBy is a draft requisition needed by the day, of the items.
func lotsBy(id, day string, items ...lot) string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = fmt.Sprintf(`{"item_no": %d, "material_id": %q, "quantity": {"value": %q, "unit": %q}}`, it.no, it.material, it.value, it.unit)
	}
	return fmt.Sprintf(`{"id": %q, "legal_entity_id": %q, "status": "draft", "needed_by": %q, "items": [%s]}`, id, le, day, strings.Join(out, ", "))
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

// The hinge's deliveries at the plant, as getSupplierDeliveryPerformance
// rows: over the year ending on the run's day, 2025-11-21 to 2026-11-20,
// the contract's supplier s2 measured and the blocked s3 thin and
// undated, the catalog's s1 with no receipt; over any other window, such
// as the read's default on the day after the run's, s2 with a fifth
// receipt, a late one, and no other supplier.
var (
	year = map[string]string{
		s2: `{"supplier_id": "` + s2 + `", "receipts": 4, "dated_receipts": 4, "on_time_receipts": 3, "on_time_percent": 75, "planned_lead_days": 5,
			"observed_lead_days": 6, "history": "measured"}`,
		s3: `{"supplier_id": "` + s3 + `", "receipts": 1, "dated_receipts": 0, "on_time_receipts": 0, "observed_lead_days": 9, "history": "thin"}`}
	later = map[string]string{
		s2: `{"supplier_id": "` + s2 + `", "receipts": 5, "dated_receipts": 5, "on_time_receipts": 3, "on_time_percent": 60, "planned_lead_days": 5,
			"observed_lead_days": 7, "history": "measured"}`}
)

// deliveries answers a read of the hinge's delivery performance: the
// year's rows over the run's year, the later ones over any other window,
// none at another plant or of another material, of the supplier alone
// when the read is narrowed to one.
func deliveries(q url.Values) string {
	rows := later
	if q.Get("from") == "2025-11-21" && q.Get("to") == "2026-11-20" {
		rows = year
	}
	var out []string
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		if q.Get("plant_id") == plant && q.Get("material_id") == hinge && (q.Get("supplier_id") == "" || q.Get("supplier_id") == id) {
			out = append(out, rows[id])
		}
	}
	return fmt.Sprintf(`{"plant_id": %q, "material_id": %q, "from": %q, "to": %q, "min_receipts": 3, "suppliers": [%s], "totals": {"receipts": 0}}`,
		q.Get("plant_id"), q.Get("material_id"), q.Get("from"), q.Get("to"), strings.Join(out, ", "))
}

// measures are s2's measures over the run's year as an order names them,
// and laterMeasures over the year to the day after the run's.
const (
	measures      = "Measures: 4 receipts (measured), lead time 6 days against 5 planned, on time 3 of 4 (75%)"
	laterMeasures = "Measures: 5 receipts (measured), lead time 7 days against 5 planned, on time 3 of 5 (60%)"
)

// checked is the query of the check's own read of s2's measures: the
// hinge at the run's plant over the year ending on the run's day.
const checked = "from=2025-11-21&material_id=" + hinge + "&plant_id=" + plant + "&supplier_id=" + s2 + "&to=2026-11-20"

func partner(id, status, role string) string {
	return fmt.Sprintf(`{"id": %q, "number": "BP1", "status": %q, "roles": [%q]}`, id, status, role)
}

// fakeAPI is a stand-in for the cell's API: the run and what its reads
// answer, a session and recommendations answered by their bodies; it
// keeps every request it was sent.
type fakeAPI struct {
	mu      sync.Mutex
	calls   []string
	queries []string // the delivery performance reads' queries
	runs    string   // the run's answer instead of answers'
	perf    string   // the delivery performance's answer instead of deliveries'
	perfAt  int      // its status; 0: 200
	fails   int      // the delivery performance reads answered 503 before any is answered
	status  int      // every read's but the run's; 0: 200
	made    int
}

func (a *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body) // a test's own request
	a.mu.Lock()
	a.calls = append(a.calls, r.Method+" "+r.URL.Path)
	if r.URL.Path == "/v1/supplier-delivery-performance" {
		a.queries = append(a.queries, r.URL.RawQuery)
	}
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
	case path == "/v1/supplier-delivery-performance" && a.perf != "":
		answer(cmp.Or(a.perfAt, 200), a.perf)
	case path == "/v1/supplier-delivery-performance" && a.failed():
		answer(503, `{"code": "platform.unavailable", "status": 503}`)
	case path == "/v1/supplier-delivery-performance" && a.status != 0:
		answer(a.status, `{"code": "identity.permission_missing", "status": 403}`)
	case path == "/v1/supplier-delivery-performance":
		answer(200, deliveries(r.URL.Query()))
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

// failed reports whether a delivery performance read is one of the first
// fails, which are answered 503.
func (a *fakeAPI) failed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fails == 0 {
		return false
	}
	a.fails--
	return true
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
	llmtest.Tool("getRequisition", map[string]any{"id": re}),
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
	llmtest.Tool("getSupplierDeliveryPerformance", map[string]any{"plant_id": plant, "material_id": hinge, "from": "2025-11-21", "to": "2026-11-20"}),
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
	first := "Order 30 EA from supplier " + s2 + " by 2026-11-21: contract " + con1 + " item 1"
	why := "\nWhy: s2 delivered 3 of 4 receipts on time; contract item 1 prices the hinge at 1.05 CAD, below the catalog's 1.10 CAD; the run's shortage is 30 EA."
	good := first + "\n" + measures + why
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
		{re, "order", ordered("40", "EA", s1, "2026-11-21", "catalog item "+cat1), replenishmentagent.ErrLots},
		{rg, "order", ordered("40", "EA", s1, "2026-11-21", "catalog item "+cat1), replenishmentagent.ErrProposal},
		{ra, "order", first + why, replenishmentagent.ErrMeasures},
		{ra, "order", first + "\nMeasures: no receipts" + why, replenishmentagent.ErrMeasures},
		{ra, "order", first + "\nMeasures: 1 receipts (thin), lead time 9 days" + why, replenishmentagent.ErrMeasures},
		{ra, "order", first + "\n" + laterMeasures + why, replenishmentagent.ErrMeasures},
		{ra, "order", first + "\n" + measures + "\n" + laterMeasures + why, replenishmentagent.ErrMeasures},
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
	if got := a.queries[len(a.queries)-1]; got != checked {
		t.Errorf("the check's read: got %s, want %s", got, checked)
	}
	if got, want := len(a.queries), 2; got != want {
		t.Errorf("delivery performance reads: got %d, want %d: the model's and the check's, kept for the run", got, want)
	}
	if got, want := res.Names[ra], "requisition PR1 of MA-HINGE"; got != want {
		t.Errorf("the hinge's proposal named: got %q, want %q", got, want)
	}
	prompt := "Planning run " + run + " of plant P100, Toronto plant, in company 3000 (legal entity " + le + "), planned on 2026-11-20. " +
		"Recommend, for each requisition it proposed, what to order, from which supplier and when. " +
		"Its suppliers' delivery performance is measured from 2025-11-21 to 2026-11-20."
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

// TestReviewBefore: a run the agent may not read, one not completed, one
// whose day is no date (it has no year to measure suppliers over) and an
// API that is gone stop the run before a session starts; a run that
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
		"a run day that is no date": {`{"id": "` + run + `", "status": "completed", "run_date": "soon", "lines": [{"requisition_id": "` + ra + `"}]}`, run,
			`the planning run's day "soon"`},
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
// recommendation; every write of requisitions, purchase orders, stock
// transport orders, RFQs, planning runs and planning data is on its never
// list, by its name or by its path (an intercompany order's goods issue,
// postIntercompanyGoodsIssue, names no purchase order but is one's
// action), and none of the list is among its tools (ADR-0072, S2l).
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
		for _, of := range []string{"Requisition", "PurchaseOrder", "StockTransportOrder", "Rfq", "ScheduleLine", "MrpRun", "MaterialPlan", "SopPlan"} {
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
// it is recommended, naming that the supplier has delivered nothing to
// the plant in the window.
func TestReviewFromHistory(t *testing.T) {
	model := llmtest.New(t, llmtest.Tool("getRequisition", map[string]any{"id": ra}),
		llmtest.Tool("listPurchaseOrders", map[string]any{"legal_entity_id": le, "material_id": hinge}),
		llmtest.Tool("getBusinessPartner", map[string]any{"id": s1}),
		llmtest.Tool("getSupplierDeliveryPerformance", map[string]any{"plant_id": plant, "material_id": hinge}),
		rec(ra, "order", "Order 30 EA from supplier "+s1+" by 2026-11-21: purchase order "+po3+"\nMeasures: no receipts\nWhy: last ordered from it at 1.15 CAD."),
		llmtest.Final("Recommended ordering the hinge from the supplier last ordered from."))
	res, err := (&replenishmentagent.Agent{LLM: model.Runner(), API: client(t, &fakeAPI{})}).Review(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if want := []recommend.Made{{ID: "r1", ObjectType: "requisition", ObjectID: ra, Kind: "order"}}; !slices.Equal(res.Recommendations, want) || res.Refused != 0 {
		t.Errorf("got %v, refused %d, want %v and none refused", res.Recommendations, res.Refused, want)
	}
}

// TestReviewMeasures: the check reads the order's supplier's measures
// itself, over the year ending on the run's day, so that no read of the
// model's makes an order pass or fail. An order naming what a read of
// another window or supplier told, when it differs, is refused, and the
// refusal tells the line wanted, whatever the model read: the read's
// default on the day after the run's (the run reviewed on a later day),
// a window ending the day before the run's day, a past or a future
// year's, one narrowed to another supplier. With no read of the model's,
// or one to the run's day, the year's line is made. The check reads once
// for the run, over its fixed window.
func TestReviewMeasures(t *testing.T) {
	first := "Order 30 EA from supplier " + s2 + " by 2026-11-21: contract " + con1 + " item 1\n"
	why := "\nWhy: the contract's 1.05 CAD."
	before := []llmtest.Step{llmtest.Tool("getRequisition", map[string]any{"id": ra}), llmtest.Tool("getContract", map[string]any{"id": con1}),
		llmtest.Tool("getBusinessPartner", map[string]any{"id": s2})}
	read := func(in map[string]any) []llmtest.Step {
		return []llmtest.Step{llmtest.Tool("getSupplierDeliveryPerformance", in)}
	}
	window := func(from, to string) map[string]any {
		return map[string]any{"plant_id": plant, "material_id": hinge, "from": from, "to": to}
	}
	for name, c := range map[string]struct {
		reads []llmtest.Step
		named string // the measures line of the first order
	}{
		"not read":                     {nil, measures},
		"to the run's day":             {read(window("2025-11-21", "2026-11-20")), measures},
		"the default on a later day":   {read(map[string]any{"plant_id": plant, "material_id": hinge}), laterMeasures},
		"ending the day before":        {read(window("2025-11-20", "2026-11-19")), laterMeasures},
		"a past year's":                {read(window("2024-11-21", "2025-11-20")), laterMeasures},
		"a future year's":              {read(window("2026-11-21", "2027-11-20")), laterMeasures},
		"narrowed to another supplier": {read(map[string]any{"plant_id": plant, "material_id": hinge, "supplier_id": s1}), noReceiptsLine},
	} {
		t.Run(name, func(t *testing.T) {
			refused := 0
			steps := append(slices.Concat(before, c.reads), rec(ra, "order", first+c.named+why))
			if c.named != measures {
				refused = 1
				steps = append(steps, rec(ra, "order", first+measures+why))
			}
			a := &fakeAPI{}
			model := llmtest.New(t, append(steps, llmtest.Final("Recommended ordering the hinge on its contract."))...)
			res, err := (&replenishmentagent.Agent{LLM: model.Runner(), API: client(t, a)}).Review(context.Background(), run)
			if err != nil {
				t.Fatal(err)
			}
			if want := []recommend.Made{{ID: "r1", ObjectType: "requisition", ObjectID: ra, Kind: "order"}}; !slices.Equal(res.Recommendations, want) {
				t.Errorf("recommendations: got %v, want %v", res.Recommendations, want)
			}
			if res.Refused != refused {
				t.Errorf("refused: got %d, want %d", res.Refused, refused)
			}
			told, _ := json.Marshal(fmt.Sprintf("want %q", measures)) // the model reads the refusal as JSON text
			if want := strings.Trim(string(told), `"`); refused == 1 && !strings.Contains(model.Requests[len(steps)-1], want) {
				t.Errorf("the refusal: got %s, want it to tell %s", model.Requests[len(steps)-1], want)
			}
			if got := slices.DeleteFunc(slices.Clone(a.queries), func(q string) bool { return q != checked }); len(got) != 1 {
				t.Errorf("the check's reads: got %v of %v, want one of %s", got, a.queries, checked)
			}
		})
	}
}

// noReceiptsLine is the measures line of a supplier with no receipt.
const noReceiptsLine = "Measures: no receipts"

// TestReviewMeasuresUnread: an order is refused while the check cannot
// read its supplier's measures (the API refuses the read, or answers in
// another shape), and nothing reaches the API.
func TestReviewMeasuresUnread(t *testing.T) {
	order := rec(ra, "order", "Order 30 EA from supplier "+s2+" by 2026-11-21: contract "+con1+" item 1\n"+measures+"\nWhy: the contract's 1.05 CAD.")
	for name, c := range map[string]struct {
		perf   string
		status int
		want   string
	}{
		"refused":       {`{"code": "identity.permission_missing", "status": 403}`, 403, "identity.permission_missing"},
		"not its shape": {`{"suppliers": 7}`, 0, "decode response body"},
	} {
		t.Run(name, func(t *testing.T) {
			a := &fakeAPI{perf: c.perf, perfAt: c.status}
			model := llmtest.New(t, llmtest.Tool("getRequisition", map[string]any{"id": ra}), llmtest.Tool("getContract", map[string]any{"id": con1}),
				llmtest.Tool("getBusinessPartner", map[string]any{"id": s2}), order, llmtest.Final("Nothing recommended."))
			res, err := (&replenishmentagent.Agent{LLM: model.Runner(), API: client(t, a)}).Review(context.Background(), run)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Recommendations) != 0 || res.Refused != 1 {
				t.Errorf("got %v, refused %d, want none made and one refused", res.Recommendations, res.Refused)
			}
			if last := model.Requests[len(model.Requests)-1]; !strings.Contains(last, "the delivery performance of supplier "+s2) || !strings.Contains(last, c.want) {
				t.Errorf("the refusal: got %s, want the read's error with %q", last, c.want)
			}
			if got := len(a.sent("recommendations")); got != 0 {
				t.Errorf("recommendations sent: got %d, want none", got)
			}
		})
	}
}

// TestReviewMeasuresRetried: a check read that failed is not kept: the
// next order reads again and, served, is made.
func TestReviewMeasuresRetried(t *testing.T) {
	order := rec(ra, "order", "Order 30 EA from supplier "+s2+" by 2026-11-21: contract "+con1+" item 1\n"+measures+"\nWhy: the contract's 1.05 CAD.")
	a := &fakeAPI{fails: 1}
	model := llmtest.New(t, llmtest.Tool("getRequisition", map[string]any{"id": ra}), llmtest.Tool("getContract", map[string]any{"id": con1}),
		llmtest.Tool("getBusinessPartner", map[string]any{"id": s2}), order, order, llmtest.Final("Recommended ordering the hinge on its contract."))
	res, err := (&replenishmentagent.Agent{LLM: model.Runner(), API: client(t, a)}).Review(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if want := []recommend.Made{{ID: "r1", ObjectType: "requisition", ObjectID: ra, Kind: "order"}}; !slices.Equal(res.Recommendations, want) || res.Refused != 1 {
		t.Errorf("got %v, refused %d, want %v and the first refused", res.Recommendations, res.Refused, want)
	}
	if got := a.queries; !slices.Equal(got, []string{checked, checked}) {
		t.Errorf("the check's reads: got %v, want two of %s", got, checked)
	}
}

// TestReviewLots: an order of a proposal of several lots (ADR-0084 As
// built S2k2) is one order of its lots: of their sum, written as the run
// line's proposed is, naming each lot in its items' order by the order's
// day; a proposal of one lot may name it. Anything else is refused, the
// lots before the check spends a read on the supplier's measures, and
// the refusal tells the lines wanted. A requisition that is not of one
// material in one unit is no proposal to order.
func TestReviewLots(t *testing.T) {
	first := func(qty string) string {
		return "Order " + qty + " EA from supplier " + s2 + " by 2026-11-21: contract " + con1 + " item 1\n"
	}
	tail := measures + "\nWhy: the contract's 1.05 CAD."
	both := "Lot 1: 25 EA by 2026-11-21\nLot 2: 15 EA by 2026-11-21\n"
	for name, c := range map[string]struct {
		on, content string
		want        error  // nil: made
		tells       string // the lines a lots refusal wants
	}{
		"one order of the lots":            {re, first("40") + both + tail, nil, ""},
		"indented lot lines":               {re, first("40") + "  Lot 1: 25 EA by 2026-11-21\n  Lot 2: 15 EA by 2026-11-21\n" + tail, nil, ""},
		"lots with fractions":              {rf, first("20.25") + "Lot 1: 12.5 EA by 2026-11-21\nLot 2: 7.5 EA by 2026-11-21\nLot 3: 0.25 EA by 2026-11-21\n" + tail, nil, ""},
		"a proposal of one lot naming it":  {ra, first("30") + "Lot 1: 30 EA by 2026-11-21\n" + tail, nil, ""},
		"the sum written otherwise":        {re, first("40.0") + both + tail, replenishmentagent.ErrQuantity, ""},
		"one lot's quantity":               {re, first("25") + both + tail, replenishmentagent.ErrQuantity, ""},
		"no lot named":                     {re, first("40") + tail, replenishmentagent.ErrLots, both},
		"one of two named":                 {re, first("40") + "Lot 1: 25 EA by 2026-11-21\n" + tail, replenishmentagent.ErrLots, both},
		"out of order":                     {re, first("40") + "Lot 2: 15 EA by 2026-11-21\nLot 1: 25 EA by 2026-11-21\n" + tail, replenishmentagent.ErrLots, both},
		"another split":                    {re, first("40") + "Lot 1: 20 EA by 2026-11-21\nLot 2: 20 EA by 2026-11-21\n" + tail, replenishmentagent.ErrLots, both},
		"a lot on another day":             {re, first("40") + "Lot 1: 25 EA by 2026-11-21\nLot 2: 15 EA by 2026-11-28\n" + tail, replenishmentagent.ErrLots, both},
		"a lot named twice":                {re, first("40") + both + "Lot 2: 15 EA by 2026-11-21\n" + tail, replenishmentagent.ErrLots, both},
		"a wrong lot of a proposal of one": {ra, first("30") + "Lot 2: 30 EA by 2026-11-21\n" + tail, replenishmentagent.ErrLots, "Lot 1: 30 EA by 2026-11-21\n"},
		"a line beginning as a lot":        {re, first("40") + both + "Lot sizes 99 EA\n" + tail, replenishmentagent.ErrLots, both},
		"items of two materials":           {rg, first("40") + tail, replenishmentagent.ErrProposal, ""},
		"items in two units":               {rh, first("40") + tail, replenishmentagent.ErrProposal, ""},
		"no item":                          {ri, first("40") + tail, replenishmentagent.ErrProposal, ""},
		"a quantity that is no plain one":  {rj, first("40") + tail, replenishmentagent.ErrProposal, ""},
	} {
		t.Run(name, func(t *testing.T) {
			a := &fakeAPI{}
			model := llmtest.New(t, llmtest.Tool("getRequisition", map[string]any{"id": c.on}), llmtest.Tool("getContract", map[string]any{"id": con1}),
				llmtest.Tool("getBusinessPartner", map[string]any{"id": s2}), rec(c.on, "order", c.content), llmtest.Final("Done."))
			res, err := (&replenishmentagent.Agent{LLM: model.Runner(), API: client(t, a)}).Review(context.Background(), run)
			if err != nil {
				t.Fatal(err)
			}
			if c.want == nil {
				made(t, res, a, c.on)
				return
			}
			refusedFirst(t, res, a, model.Requests, c.want, c.tells)
		})
	}
}

// made checks that one order on the proposal was made, after the check
// read its supplier's measures once.
func made(t *testing.T, res recommend.Result, a *fakeAPI, on string) {
	t.Helper()
	if want := []recommend.Made{{ID: "r1", ObjectType: "requisition", ObjectID: on, Kind: "order"}}; !slices.Equal(res.Recommendations, want) || res.Refused != 0 {
		t.Errorf("got %v, refused %d, want %v and none refused", res.Recommendations, res.Refused, want)
	}
	if !slices.Equal(a.queries, []string{checked}) {
		t.Errorf("the check's reads: got %v, want one of %s", a.queries, checked)
	}
}

// refusedFirst checks that the order was refused with want, telling the
// lines wanted when a lots refusal names them, before the check read any
// supplier's measures, and that nothing reached the API.
func refusedFirst(t *testing.T, res recommend.Result, a *fakeAPI, requests []string, want error, tells string) {
	t.Helper()
	if len(res.Recommendations) != 0 || res.Refused != 1 {
		t.Errorf("got %v, refused %d, want none made and one refused", res.Recommendations, res.Refused)
	}
	msg := want.Error()
	if tells != "" {
		msg += fmt.Sprintf(": want %q", strings.TrimSuffix(tells, "\n"))
	}
	told, _ := json.Marshal(msg) // the model reads the refusal as JSON text
	if last := requests[len(requests)-1]; !strings.Contains(last, strings.Trim(string(told), `"`)) {
		t.Errorf("the refusal: got %s, want it to tell %s", last, msg)
	}
	if len(a.queries) != 0 {
		t.Errorf("delivery performance reads: got %v, want none: the refusal comes first", a.queries)
	}
	if got := len(a.sent("recommendations")); got != 0 {
		t.Errorf("recommendations sent: got %d, want none", got)
	}
}

// TestReviewPeriodLots: an order of a proposal by period (ADR-0084 As
// built S2k3) names each lot by the day its run line's lot_schedule needs
// it by, not the requisition's single day, which is the first lot's and
// the order's; a lot named by the order's day is refused, telling each
// lot's own. A contract is a source of it only while valid to its last
// lot's day; the catalog item, valid always, is one. A requisition whose
// items a person has changed since the run is no longer the run line's
// lots: its lots are named by its own day, as other proposals' are, and
// so are the lots of a run line whose lots are not in the contract's shape.
func TestReviewPeriodLots(t *testing.T) {
	onContract := func(qty, by string) string {
		return "Order " + qty + " EA from supplier " + s2 + " by " + by + ": contract " + con1 + " item 1\n"
	}
	weekly := "Lot 1: 30 EA by 2026-11-23\nLot 2: 20.000 EA by 2026-11-30\n"
	late := "Lot 1: 30 EA by 2026-12-28\nLot 2: 20 EA by 2027-01-04\n"
	why := "\nWhy: weekly lots."
	for name, c := range map[string]struct {
		on, content string
		want        error  // nil: made
		tells       string // the lines a lots refusal wants
	}{
		"each lot by its own day": {rk, onContract("50", "2026-11-23") + weekly + measures + why, nil, ""},
		"every lot by the order's day": {rk, onContract("50", "2026-11-23") + "Lot 1: 30 EA by 2026-11-23\nLot 2: 20.000 EA by 2026-11-23\n" + measures + why,
			replenishmentagent.ErrLots, weekly},
		"the order by the last lot's day":       {rk, onContract("50", "2026-11-30") + weekly + measures + why, replenishmentagent.ErrWhen, ""},
		"a contract ending before the last lot": {rl, onContract("50", "2026-12-28") + late + measures + why, replenishmentagent.ErrSource, ""},
		"the catalog to the last lot": {rl, "Order 50 EA from supplier " + s1 + " by 2026-12-28: catalog item " + cat1 + "\n" + late + noReceiptsLine + why,
			nil, ""},
		"items changed since the run": {rm, onContract("55", "2026-11-23") + "Lot 1: 30 EA by 2026-11-23\nLot 2: 25 EA by 2026-11-23\n" + measures + why, nil, ""},
		"changed items by the run line's days": {rm, onContract("55", "2026-11-23") + "Lot 1: 30 EA by 2026-11-23\nLot 2: 25 EA by 2026-11-30\n" + measures + why,
			replenishmentagent.ErrLots, "Lot 1: 30 EA by 2026-11-23\nLot 2: 25 EA by 2026-11-23\n"},
		"lots not in the contract's shape": {rn, onContract("50", "2026-11-23") + "Lot 1: 30 EA by 2026-11-23\nLot 2: 20 EA by 2026-11-23\n" + measures + why,
			nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			a := &fakeAPI{}
			model := llmtest.New(t, llmtest.Tool("getRequisition", map[string]any{"id": c.on}), llmtest.Tool("getContract", map[string]any{"id": con1}),
				llmtest.Tool("listCatalogItems", map[string]any{"legal_entity_id": le, "text": "MA-HINGE"}),
				llmtest.Tool("getBusinessPartner", map[string]any{"id": s1}), llmtest.Tool("getBusinessPartner", map[string]any{"id": s2}),
				rec(c.on, "order", c.content), llmtest.Final("Done."))
			res, err := (&replenishmentagent.Agent{LLM: model.Runner(), API: client(t, a)}).Review(context.Background(), run)
			if err != nil {
				t.Fatal(err)
			}
			if c.want == nil {
				if want := []recommend.Made{{ID: "r1", ObjectType: "requisition", ObjectID: c.on, Kind: "order"}}; !slices.Equal(res.Recommendations, want) || res.Refused != 0 {
					t.Errorf("got %v, refused %d, want %v and none refused", res.Recommendations, res.Refused, want)
				}
				return
			}
			refusedFirst(t, res, a, model.Requests, c.want, c.tells)
		})
	}
}
