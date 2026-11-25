package inventory_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
)

// recount sends the document's items for a recount at its version, wanting
// status, and returns the answer.
func (e env) recount(t *testing.T, id string, status int, nos ...int) map[string]any {
	t.Helper()
	return e.recountAs(t, e.Client, id, status, nos...)
}

// recountAs is recount by the client's person.
func (e env) recountAs(t *testing.T, c apitest.Client, id string, status int, nos ...int) map[string]any {
	t.Helper()
	_, tag := c.Must(200, "GET", counts+"/"+id, "", "")
	items := make([]any, len(nos))
	for i, no := range nos {
		items[i] = map[string]any{"item_no": no}
	}
	code, got, _ := c.Do("POST", counts+"/"+id+":recount", mustJSON(t, map[string]any{"items": items}), tag)
	if code != status {
		t.Fatalf("recount: got %d %v, want %d", code, got, status)
	}
	return got
}

// history is a document's items as "no #count_no counted [earlier counts]",
// each earlier count as "#no counted (entered) by recounter".
func history(t *testing.T, d map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range rows(t, d, "items") {
		counted := "-"
		if c, ok := it["counted_quantity"].(map[string]any); ok {
			counted = fmt.Sprint(c["value"])
		}
		var earlier []string
		for _, c := range rows(t, it, "earlier_counts") {
			q, entry := c["counted_quantity"].(map[string]any), c["entry_quantity"].(map[string]any)
			earlier = append(earlier, fmt.Sprintf("#%v %v (%v %v)", c["count_no"], q["value"], entry["value"], entry["unit"]))
		}
		out = append(out, fmt.Sprintf("%v #%v %s [%s]", it["item_no"], it["count_no"], counted, strings.Join(earlier, ", ")))
	}
	return strings.Join(out, "; ")
}

// TestRecount: a counted item sent for a recount keeps its count as an
// earlier count, who counted it and who sent it back with it, and is
// counted again, the document counting until it is; a second recount
// keeps both earlier counts in order; only the latest count is posted;
// each recount is audited and published; a posted document is not
// recounted.
func TestRecount(t *testing.T) {
	e := countSetup(t)
	bolt := mustMaterial(t, e)
	e.post(t, "561", e.item("10", "EA", nil))
	id := e.counted(t, []string{e.bracket, bolt}, "7", "0")
	before, _ := e.Must(200, "GET", counts+"/"+id, "", "")
	counter := rows(t, before, "items")[0]
	ines := e.Reader(t, "Ines Inventory", e.DE)
	got := e.recountAs(t, e.As(ines), id, 200, 1)
	for _, w := range []struct{ name, got, want string }{
		{"history", history(t, got), "1 #2 - [#1 7 (7 EA)]; 2 #1 0 []"},
		{"status", fmt.Sprint(got["status"]), "counting"},
		{"version", fmt.Sprint(got["version"]), "3"},
	} {
		if w.got != w.want {
			t.Errorf("recounted %s: got %q, want %q", w.name, w.got, w.want)
		}
	}
	first := rows(t, rows(t, got, "items")[0], "earlier_counts")[0]
	for _, w := range []struct{ name, got, want string }{
		{"counted_by", fmt.Sprint(first["counted_by"]), e.Tenant.Human.ID.String()},
		{"counted_by_name", fmt.Sprint(first["counted_by_name"]), fmt.Sprint(counter["counted_by_name"])},
		{"counted_at", fmt.Sprint(first["counted_at"]), fmt.Sprint(counter["counted_at"])},
		{"recounted_by", fmt.Sprint(first["recounted_by"]), ines.ID.String()},
		{"recounted_by_name", fmt.Sprint(first["recounted_by_name"]), "Ines Inventory"},
		{"recounted_at given", fmt.Sprint(first["recounted_at"] != nil), "true"},
	} {
		if w.got != w.want {
			t.Errorf("the earlier count's %s: got %q, want %q", w.name, w.got, w.want)
		}
	}
	if name, _ := counter["counted_by_name"].(string); name == "" {
		t.Errorf("the count's counted_by_name: got %v, want the counter's name", counter["counted_by_name"])
	}
	if got := e.enter(t, id, 409, [3]string{"2", "1", "EA"}); got["code"] != "inventory.item_counted" {
		t.Errorf("an item not recounted entered again: got %v, want inventory.item_counted", got)
	}
	if got := e.enter(t, id, 200, [3]string{"1", "0.5", "BOX"}); got["status"] != "counted" {
		t.Fatalf("the recount entered: got %v, want counted", got["status"])
	}
	e.recount(t, id, 200, 1)
	after := e.enter(t, id, 200, [3]string{"1", "9", "EA"})
	if want := "1 #3 9 [#1 7 (7 EA), #2 10 (0.5 BOX)]; 2 #1 0 []"; history(t, after) != want {
		t.Errorf("recounted twice: got %s, want %s", history(t, after), want)
	}
	posted := e.postDiffs(t, id, 200)
	if got, want := diffs(t, posted), "1 -1 -2.00 true; 2 0 0.00 false"; got != want {
		t.Errorf("posted: got %s, want %s, the latest count's", got, want)
	}
	if got := e.stock(t, e.s1, ""); got != "9 EA" {
		t.Errorf("the stock after posting: got %s, want 9 EA, the latest count", got)
	}
	if got := e.recount(t, id, 409, 1); got["code"] != "inventory.count_invalid_status" {
		t.Errorf("a posted document recounted: got %v, want inventory.count_invalid_status", got)
	}
	a := e.audits(t, id, "inventory.count_document.recount")
	if len(a) != 2 {
		t.Fatalf("audits of the recounts: got %d, want 2", len(a))
	}
	if got := fmt.Sprint(rows(t, a[1], "recounted")[0]["count_no"]); got != "2" {
		t.Errorf("the second recount's audit: got count %s set aside, want 2", got)
	}
	e.recountEvents(t, id)
}

// recountEvents checks the document's PhysicalInventoryRecountRequested
// payloads, which conform to the event contract: one per recount, each
// naming the count it set aside.
func (e env) recountEvents(t *testing.T, id string) {
	t.Helper()
	var got []string
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "inventory", Name: "physical_inventory_recount_requested"}) {
		if p["count_document_id"] != id {
			continue
		}
		it := rows(t, p, "items")[0]
		q := it["counted_quantity"].(map[string]any)
		got = append(got, fmt.Sprintf("%v %v item %v #%v %v %v", p["status"], p["version"], it["item_no"], it["count_no"], q["value"], q["unit"]))
	}
	if want := "counting 3 item 1 #1 7 EA, counting 5 item 1 #2 10 EA"; strings.Join(got, ", ") != want {
		t.Errorf("events: got %v, want %s", got, want)
	}
}

// TestRecountSeveral: items of a document still counting are recounted
// together, in the request's order, each keeping its own count; the
// document stays counting.
func TestRecountSeveral(t *testing.T) {
	e := countSetup(t)
	id := e.count(t, e.bracket, mustMaterial(t, e), mustMaterial(t, e))["id"].(string)
	e.enter(t, id, 200, [3]string{"1", "4", "EA"}, [3]string{"2", "5", "EA"})
	got := e.recount(t, id, 200, 2, 1)
	if want := "1 #2 - [#1 4 (4 EA)]; 2 #2 - [#1 5 (5 EA)]; 3 #1 - []"; history(t, got) != want {
		t.Errorf("recounted: got %s, want %s", history(t, got), want)
	}
	if got["status"] != "counting" {
		t.Errorf("the document: got %v, want counting", got["status"])
	}
	var nos []string
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "inventory", Name: "physical_inventory_recount_requested"}) {
		if p["count_document_id"] == id {
			for _, it := range rows(t, p, "items") {
				nos = append(nos, fmt.Sprint(it["item_no"]))
			}
		}
	}
	if got := strings.Join(nos, " "); got != "2 1" {
		t.Errorf("the event's items: got %s, want 2 1, the request's order", got)
	}
}

// TestRecountRefused: what a recount is refused for.
func TestRecountRefused(t *testing.T) {
	e := countSetup(t)
	e.post(t, "561", e.item("10", "EA", nil))
	counting := e.count(t, e.bracket, mustMaterial(t, e))["id"].(string)
	e.enter(t, counting, 200, [3]string{"1", "10", "EA"})
	pending := e.counted(t, []string{mustMaterial(t, e)}, "50")
	e.postDiffs(t, pending, 200)
	cases := []struct {
		name   string
		doc    string
		nos    []int
		status int
		code   string
		field  any
	}{
		{"no such item", counting, []int{3}, 422, "inventory.unknown_item", "items[0].item_no"},
		{"an item twice", counting, []int{1, 1}, 422, "inventory.duplicate_item", "items[1].item_no"},
		{"an item not counted", counting, []int{2}, 409, "inventory.item_not_counted", nil},
		{"a document pending approval", pending, []int{1}, 409, "inventory.count_invalid_status", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.recount(t, c.doc, c.status, c.nos...)
			if got["code"] != c.code || field(got) != c.field {
				t.Errorf("got %v, want %s at %v", got, c.code, c.field)
			}
		})
	}
	body := `{"items": [{"item_no": 1}]}`
	if status, _, _ := e.Do("POST", counts+"/"+counting+":recount", body, `"9"`); status != 412 {
		t.Errorf("a stale version: got %d, want 412", status)
	}
	outsider := e.Reader(t, "Outsider", e.US)
	if status, _, _ := e.As(outsider).Do("POST", counts+"/"+counting+":recount", body, `"2"`); status != 404 {
		t.Errorf("another company's document: got %d, want 404", status)
	}
	if d, _ := e.Must(200, "GET", counts+"/"+counting, "", ""); history(t, d) != "1 #1 10 []; 2 #1 - []" || d["version"] != float64(2) {
		t.Errorf("after the refusals: got version %v %s, want version 2 unchanged", d["version"], history(t, d))
	}
}

// TestRecountAndPostAtOnce: a recount and the posting of a counted
// document at its version at once: the document's row lock orders them,
// so one is done and the other refused at the version it read.
func TestRecountAndPostAtOnce(t *testing.T) {
	e := countSetup(t)
	e.post(t, "561", e.item("10", "EA", nil))
	id := e.counted(t, []string{e.bracket}, "9")
	_, tag := e.Must(200, "GET", counts+"/"+id, "", "")
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i, path := range []string{":recount", ":postDifferences"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := ""
			if path == ":recount" {
				body = `{"items": [{"item_no": 1}]}`
			}
			statuses[i], _, _ = e.Do("POST", counts+"/"+id+path, body, tag)
		}()
	}
	wg.Wait()
	d, _ := e.Must(200, "GET", counts+"/"+id, "", "")
	switch got := fmt.Sprint(statuses, " ", d["status"]); got {
	case "[200 412] counting", "[412 200] posted":
	default:
		t.Errorf("a recount and a posting at once: got %s, want [200 412] counting or [412 200] posted", got)
	}
}
