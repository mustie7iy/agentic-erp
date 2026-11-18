package inventory_test

import (
	"fmt"
	"sync"
	"testing"
)

// TestReverse: a reversal is a document of the reversal type moving the
// original's lines back, naming the original, which names it; it is
// audited and published.
func TestReverse(t *testing.T) {
	e := setup(t)
	e.post(t, "561", e.item("10", "EA", nil))
	transfer := e.post(t, "311", e.item("4", "EA", map[string]any{"to_storage_location_id": e.s2}))
	id := transfer["id"].(string)
	_, tag := e.Must(200, "GET", docs+"/"+id, "", "")
	rev, _ := e.Must(200, "POST", docs+"/"+id+":reverse", "", tag)
	orig, _ := e.Must(200, "GET", docs+"/"+id, "", "")
	for _, w := range []struct{ name, got, want string }{
		{"movement type", fmt.Sprint(rev["movement_type"]), "312"},
		{"lines", e.lines(t, rev), "P1:S2 -4 EA (4 EA); P1:S1 4 EA (4 EA)"},
		{"reversal of", fmt.Sprint(rev["reversal_of_number"]), fmt.Sprint(transfer["number"])},
		{"reversed by", fmt.Sprint(orig["reversed_by_id"]), fmt.Sprint(rev["id"])},
		{"S1", e.stock(t, e.s1, ""), "10 EA"},
		{"S2", e.stock(t, e.s2, ""), ""},
	} {
		if w.got != w.want {
			t.Errorf("%s: got %q, want %q", w.name, w.got, w.want)
		}
	}
	if a := e.audits(t, rev["id"].(string), "inventory.material_document.reverse"); len(a) != 1 || a[0]["reversal_of_id"] != id {
		t.Errorf("audit: got %v, want one naming %s", a, id)
	}
	if ev := e.events(t, rev["id"].(string)); len(ev) != 1 || ev[0]["reversal_of_id"] != id {
		t.Errorf("event: got %v, want one naming %s", ev, id)
	}
}

// TestReverseRefused: a stale version, a document reversed already, a
// reversal and a reversal of stock that has gone are refused; another
// company's document is not found.
func TestReverseRefused(t *testing.T) {
	e := setup(t)
	receipt := e.post(t, "561", e.item("10", "EA", nil))["id"].(string)
	issued := e.post(t, "501", e.item("5", "EA", nil))["id"].(string)
	_, tag := e.Must(200, "GET", docs+"/"+issued, "", "")
	rev, _ := e.Must(200, "POST", docs+"/"+issued+":reverse", "", tag)
	e.post(t, "201", e.item("8", "EA", map[string]any{"cost_center_id": e.c1}))
	_, revTag := e.Must(200, "GET", docs+"/"+rev["id"].(string), "", "")
	_, receiptTag := e.Must(200, "GET", docs+"/"+receipt, "", "")
	outsider := e.Reader(t, "Outsider", e.US)
	cases := []struct {
		name, id, tag string
		status        int
		code          string
	}{
		{"a stale version", issued, tag, 412, "request.version_mismatch"},
		{"a reversal", rev["id"].(string), revTag, 409, "inventory.cannot_reverse_reversal"},
		{"a receipt whose stock has gone", receipt, receiptTag, 409, "inventory.insufficient_stock"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, got, _ := e.Do("POST", docs+"/"+c.id+":reverse", "", c.tag)
			if status != c.status || got["code"] != c.code {
				t.Errorf("got %d %v, want %d %s", status, got, c.status, c.code)
			}
		})
	}
	_, current := e.Must(200, "GET", docs+"/"+issued, "", "")
	if status, got, _ := e.Do("POST", docs+"/"+issued+":reverse", "", current); status != 409 || got["code"] != "inventory.already_reversed" {
		t.Errorf("reversed twice: got %d %v, want 409 inventory.already_reversed", status, got)
	}
	if status, _, _ := e.As(outsider).Do("GET", docs+"/"+receipt, "", ""); status != 404 {
		t.Errorf("another company's document: got %d, want 404", status)
	}
}

// TestReverseBlocked: a document of a material blocked since is still
// reversed.
func TestReverseBlocked(t *testing.T) {
	e := setup(t)
	id := e.post(t, "561", e.item("10", "EA", nil))["id"].(string)
	_, mtag := e.Must(200, "GET", "/v1/materials/"+e.bracket, "", "")
	e.Must(200, "PATCH", "/v1/materials/"+e.bracket, `{"status": "blocked"}`, mtag)
	_, tag := e.Must(200, "GET", docs+"/"+id, "", "")
	e.Must(200, "POST", docs+"/"+id+":reverse", "", tag)
	if got := e.stock(t, e.s1, ""); got != "" {
		t.Errorf("stock: got %q, want none", got)
	}
}

// TestReversalsAtOnce: two reversals of one document at once post one.
func TestReversalsAtOnce(t *testing.T) {
	e := setup(t)
	id := e.post(t, "561", e.item("10", "EA", nil))["id"].(string)
	_, tag := e.Must(200, "GET", docs+"/"+id, "", "")
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _, _ = e.Do("POST", docs+"/"+id+":reverse", "", tag)
		}()
	}
	wg.Wait()
	if fmt.Sprint(statuses) != "[200 409]" && fmt.Sprint(statuses) != "[409 200]" && fmt.Sprint(statuses) != "[200 412]" && fmt.Sprint(statuses) != "[412 200]" {
		t.Errorf("statuses: got %v, want one 200 and one refusal", statuses)
	}
	page, _ := e.Must(200, "GET", docs+"?movement_type=562", "", "")
	if n := len(rows(t, page, "items")); n != 1 {
		t.Errorf("reversals: got %d, want 1", n)
	}
}
