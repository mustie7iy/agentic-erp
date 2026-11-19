package inventory_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

const counts = "/v1/physical-inventory-documents"

// countPerms are what counting and posting differences take besides perms.
var countPerms = append([]string{"inventory.count.write"}, perms...)

// countSetup is setup with the permissions to count.
func countSetup(t *testing.T) env {
	t.Helper()
	return setupWith(t, countPerms...)
}

// count creates a count document of the materials at S1 of P1 and returns it.
func (e env) count(t *testing.T, ms ...string) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", counts, e.countBody(t, e.p1, e.s1, ms...), "")
	return got
}

func (e env) countBody(t *testing.T, plant, location uuid.UUID, ms ...string) string {
	t.Helper()
	items := make([]any, len(ms))
	for i, m := range ms {
		items[i] = map[string]any{"material_id": m}
	}
	return mustJSON(t, map[string]any{"plant_id": plant, "storage_location_id": location, "items": items})
}

// enter enters the counts (item number, quantity, unit) of the document at
// its version, wanting status, and returns the answer.
func (e env) enter(t *testing.T, id string, status int, entries ...[3]string) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", counts+"/"+id, "", "")
	items := make([]any, len(entries))
	for i, en := range entries {
		var no int
		if _, err := fmt.Sscan(en[0], &no); err != nil {
			t.Fatal(err)
		}
		items[i] = map[string]any{"item_no": no, "quantity": map[string]any{"value": en[1], "unit": en[2]}}
	}
	code, got, _ := e.Do("POST", counts+"/"+id+":enterCount", mustJSON(t, map[string]any{"items": items}), tag)
	if code != status {
		t.Fatalf("enter counts: got %d %v, want %d", code, got, status)
	}
	return got
}

// countItems are a document's items as "no material book counted (entered)".
func countItems(t *testing.T, d map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range rows(t, d, "items") {
		book := it["book_quantity"].(map[string]any)
		s := fmt.Sprintf("%v %v %v %v", it["item_no"], it["material_number"], book["value"], book["unit"])
		if c, ok := it["counted_quantity"].(map[string]any); ok {
			entry := it["entry_quantity"].(map[string]any)
			s += fmt.Sprintf(" counted %v (%v %v)", c["value"], entry["value"], entry["unit"])
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

// TestCount: a count document freezes each material's book quantity at
// the storage location, a material never moved there at zero, is numbered
// from the company's range, blocks postings of its materials there (not
// elsewhere), and is counted in several entries, in any unit, zero
// included, then counted; each step audited.
func TestCount(t *testing.T) {
	e := countSetup(t)
	bolt := e.material(t, "Hex bolt M10", "raw_material", true)
	e.value(t, bolt, e.p1, "standard", "0.50")
	e.post(t, "561", e.item("10", "EA", nil))
	e.post(t, "561", e.item("4", "EA", map[string]any{"storage_location_id": e.s2}))
	d := e.count(t, e.bracket, bolt)
	id := d["id"].(string)
	for _, w := range []struct{ name, got, want string }{
		{"status", fmt.Sprint(d["status"]), "counting"},
		{"number", fmt.Sprint(strings.HasPrefix(fmt.Sprint(d["number"]), "PI")), "true"},
		{"location", fmt.Sprintf("%v/%v %v", d["plant_code"], d["storage_location_code"], d["company_code"]), "P1/S1 2000"},
		{"items", countItems(t, d), fmt.Sprintf("1 %s 10 EA; 2 %s 0 EA", e.number(t, e.bracket), e.number(t, bolt))},
	} {
		if w.got != w.want {
			t.Errorf("%s: got %q, want %q", w.name, w.got, w.want)
		}
	}
	status, got, _ := e.Do("POST", docs, body(t, "201", e.item("1", "EA", map[string]any{"cost_center_id": e.c1})), "")
	if status != 409 || got["code"] != "inventory.posting_blocked" || !strings.Contains(fmt.Sprint(got["detail"]), fmt.Sprint(d["number"])) {
		t.Errorf("an issue at the counted location: got %d %v, want 409 inventory.posting_blocked naming %v", status, got, d["number"])
	}
	e.post(t, "201", e.item("1", "EA", map[string]any{"storage_location_id": e.s2, "cost_center_id": e.c1}))
	e.enter(t, id, 200, [3]string{"2", "0", "EA"})
	after := e.enter(t, id, 200, [3]string{"1", "0.5", "BOX"})
	if got, want := countItems(t, after), fmt.Sprintf("1 %s 10 EA counted 10 (0.5 BOX); 2 %s 0 EA counted 0 (0 EA)", e.number(t, e.bracket),
		e.number(t, bolt)); got != want {
		t.Errorf("counted: got %q, want %q", got, want)
	}
	if after["status"] != "counted" || after["version"] != float64(3) {
		t.Errorf("after the counts: got %v version %v, want counted version 3", after["status"], after["version"])
	}
	for _, action := range []string{"create", "count"} {
		if a := e.audits(t, id, "inventory.count_document."+action); len(a) == 0 || a[0]["number"] != d["number"] {
			t.Errorf("audit of %s: got %v, want one naming %v", action, a, d["number"])
		}
	}
	list, _ := e.Must(200, "GET", counts+"?material_id="+bolt+"&status=counted&plant_id="+e.p1.String()+"&storage_location_id="+e.s1.String(), "", "")
	if items := rows(t, list, "items"); len(items) != 1 || items[0]["id"] != id {
		t.Errorf("list by material and status: got %v, want the document", items)
	}
}

// number is a material's number.
func (e env) number(t *testing.T, id string) string {
	t.Helper()
	m, _ := e.Must(200, "GET", "/v1/materials/"+id, "", "")
	return fmt.Sprint(m["number"])
}

// TestCountRefused: what a count document is refused for.
func TestCountRefused(t *testing.T) {
	e := countSetup(t)
	pending := e.material(t, "Pending bracket", "trading_good", false)
	service := e.material(t, "Machining service", "service", true)
	unvalued := e.material(t, "Unvalued bracket", "trading_good", true)
	open := e.count(t, e.bracket)
	cases := []struct {
		name            string
		plant, location uuid.UUID
		ms              []string
		status          int
		code            string
		field           any
	}{
		{"a storage location as the plant", e.s1, e.s1, []string{e.bracket}, 422, "inventory.plant_invalid", "plant_id"},
		{"a storage location of another plant", e.p1, e.s3, []string{e.bracket}, 422, "inventory.storage_location_invalid", "storage_location_id"},
		{"no material", e.p1, e.s2, []string{uuid.NewString()}, 422, "inventory.material_invalid", "items[0].material_id"},
		{"a pending material", e.p1, e.s2, []string{pending}, 422, "inventory.material_not_usable", "items[0].material_id"},
		{"a service", e.p1, e.s2, []string{service}, 422, "inventory.material_not_stocked", "items[0].material_id"},
		{"a material twice", e.p1, e.s2, []string{e.bracket, e.bracket}, 422, "inventory.duplicate_material", "items[1].material_id"},
		{"a material not valued", e.p1, e.s2, []string{unvalued}, 422, "inventory.no_valuation", nil},
		{"a material counted there", e.p1, e.s1, []string{e.bracket}, 409, "inventory.count_open", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, got, _ := e.Do("POST", counts, e.countBody(t, c.plant, c.location, c.ms...), "")
			if status != c.status || got["code"] != c.code || field(got) != c.field {
				t.Errorf("got %d %v, want %d %s at %v", status, got, c.status, c.code, c.field)
			}
		})
	}
	if status, got, _ := e.Do("POST", counts, e.countBody(t, e.p1, e.s1, e.bracket), ""); !strings.Contains(fmt.Sprint(got["detail"]), fmt.Sprint(open["number"])) {
		t.Errorf("a second count: got %d %v, want it to name %v", status, got, open["number"])
	}
}

// TestCountNotExtended: a material not extended to the plant, or not kept
// at the storage location, is not counted there.
func TestCountNotExtended(t *testing.T) {
	e := countSetup(t)
	p4 := e.orgUnit(t, e.DE, "plant", "P4", nil)
	s5 := e.orgUnit(t, e.DE, "storage_location", "S5", &p4)
	s6 := e.orgUnit(t, e.DE, "storage_location", "S6", &e.p1)
	for _, c := range []struct {
		name            string
		plant, location uuid.UUID
		code            string
	}{{"another plant", p4, s5, "inventory.plant_not_extended"}, {"another storage location", e.p1, s6, "inventory.storage_location_not_extended"}} {
		t.Run(c.name, func(t *testing.T) {
			status, got, _ := e.Do("POST", counts, e.countBody(t, c.plant, c.location, e.bracket), "")
			if status != 422 || got["code"] != c.code {
				t.Errorf("got %d %v, want 422 %s", status, got, c.code)
			}
		})
	}
}

// TestCountEntryRefused: what an entry of counts is refused for.
func TestCountEntryRefused(t *testing.T) {
	e := countSetup(t)
	d := e.count(t, e.bracket)
	id := d["id"].(string)
	e.enter(t, id, 200, [3]string{"1", "3", "EA"})
	e2 := e.count(t, mustMaterial(t, e))
	cases := []struct {
		name   string
		doc    string
		entry  [3]string
		status int
		code   string
	}{
		{"a document counted", id, [3]string{"1", "4", "EA"}, 409, "inventory.count_invalid_status"},
		{"no such item", e2["id"].(string), [3]string{"2", "4", "EA"}, 422, "inventory.unknown_item"},
		{"a negative quantity", e2["id"].(string), [3]string{"1", "-1", "EA"}, 422, "inventory.invalid_quantity"},
		{"an unknown unit", e2["id"].(string), [3]string{"1", "1", "KGM"}, 422, "inventory.unknown_unit"},
		{"a zero in an unknown unit", e2["id"].(string), [3]string{"1", "0", "KGM"}, 422, "inventory.unknown_unit"},
		{"an inexact quantity", e2["id"].(string), [3]string{"1", "1", "KG"}, 422, "inventory.inexact_quantity"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.enter(t, c.doc, c.status, c.entry)
			if got["code"] != c.code {
				t.Errorf("got %v, want %s", got, c.code)
			}
		})
	}
	_, tag := e.Must(200, "GET", counts+"/"+e2["id"].(string), "", "")
	twice := `{"items": [{"item_no": 1, "quantity": {"value": "1", "unit": "EA"}}, {"item_no": 1, "quantity": {"value": "1", "unit": "EA"}}]}`
	if status, got, _ := e.Do("POST", counts+"/"+e2["id"].(string)+":enterCount", twice, tag); status != 422 || got["code"] != "inventory.duplicate_item" {
		t.Errorf("an item twice: got %d %v, want 422 inventory.duplicate_item", status, got)
	}
	if status, _, _ := e.Do("POST", counts+"/"+e2["id"].(string)+":enterCount", twice, `"9"`); status != 412 {
		t.Errorf("a stale version: got %d, want 412", status)
	}
	outsider := e.Reader(t, "Outsider", e.US)
	if status, _, _ := e.As(outsider).Do("GET", counts+"/"+id, "", ""); status != 404 {
		t.Errorf("another company's document: got %d, want 404", status)
	}
}

// mustMaterial is a valued material kept at S1, counted nowhere yet.
func mustMaterial(t *testing.T, e env) string {
	t.Helper()
	m := e.material(t, "Another bracket "+uuid.NewString()[:8], "trading_good", true)
	e.value(t, m, e.p1, "moving_average", "1.00")
	return m
}

// TestCountItemOnce: an item counted is not counted again, even by a
// second entry of a document still counting.
func TestCountItemOnce(t *testing.T) {
	e := countSetup(t)
	d := e.count(t, e.bracket, mustMaterial(t, e))
	id := d["id"].(string)
	e.enter(t, id, 200, [3]string{"1", "2", "EA"})
	if got := e.enter(t, id, 409, [3]string{"1", "3", "EA"}); got["code"] != "inventory.item_counted" {
		t.Errorf("a second count: got %v, want inventory.item_counted", got)
	}
}

// TestCountsAtOnce: two count documents of a material at a storage
// location at once: one is created, the other refused.
func TestCountsAtOnce(t *testing.T) {
	e := countSetup(t)
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _, _ = e.Do("POST", counts, e.countBody(t, e.p1, e.s1, e.bracket), "")
		}()
	}
	wg.Wait()
	if fmt.Sprint(statuses) != "[201 409]" && fmt.Sprint(statuses) != "[409 201]" {
		t.Errorf("two counts at once: got %v, want one 201 and one 409", statuses)
	}
}

// TestCountAndIssueAtOnce: a count and an issue of the material at the
// location at once: whichever comes first, the book quantity is the stock
// the other leaves, and the issue is refused after the count.
func TestCountAndIssueAtOnce(t *testing.T) {
	e := countSetup(t)
	e.post(t, "561", e.item("10", "EA", nil))
	var wg sync.WaitGroup
	var issue int
	var created map[string]any
	wg.Add(2)
	go func() {
		defer wg.Done()
		issue, _, _ = e.Do("POST", docs, body(t, "201", e.item("3", "EA", map[string]any{"cost_center_id": e.c1})), "")
	}()
	go func() {
		defer wg.Done()
		_, created, _ = e.Do("POST", counts, e.countBody(t, e.p1, e.s1, e.bracket), "")
	}()
	wg.Wait()
	book := rows(t, created, "items")[0]["book_quantity"].(map[string]any)["value"]
	if got := fmt.Sprintf("%d %v", issue, book); got != "201 7" && got != "409 10" {
		t.Errorf("the issue and the book quantity: got %s, want 201 7 or 409 10", got)
	}
}

// TestCountPages: the documents are listed the latest first, a page at a
// time.
func TestCountPages(t *testing.T) {
	e := countSetup(t)
	first := e.count(t, e.bracket)
	second, _ := e.Must(201, "POST", counts, e.countBody(t, e.p1, e.s2, e.bracket), "")
	page, _ := e.Must(200, "GET", counts+"?page_size=1", "", "")
	next, _ := e.Must(200, "GET", counts+"?page_size=1&page_token="+fmt.Sprint(page["next_page_token"]), "", "")
	got := fmt.Sprint(rows(t, page, "items")[0]["id"], rows(t, next, "items")[0]["id"])
	if want := fmt.Sprint(second["id"], first["id"]); got != want {
		t.Errorf("pages: got %s, want %s", got, want)
	}
}
