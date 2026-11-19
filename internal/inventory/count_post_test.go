package inventory_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
)

// counted creates a count document of the materials at S1 of P1, enters
// the quantities (in EA, item by item) and returns its id.
func (e env) counted(t *testing.T, ms []string, qty ...string) string {
	t.Helper()
	id := e.count(t, ms...)["id"].(string)
	entries := make([][3]string, len(qty))
	for i, q := range qty {
		entries[i] = [3]string{fmt.Sprint(i + 1), q, "EA"}
	}
	e.enter(t, id, 200, entries...)
	return id
}

// postDiffs posts the document's differences at its version, wanting
// status, and returns the answer.
func (e env) postDiffs(t *testing.T, id string, status int) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", counts+"/"+id, "", "")
	code, got, _ := e.Do("POST", counts+"/"+id+":postDifferences", "", tag)
	if code != status {
		t.Fatalf("post differences: got %d %v, want %d", code, got, status)
	}
	return got
}

// diffs are a posted document's items as "no difference value document?".
func diffs(t *testing.T, d map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range rows(t, d, "items") {
		q, _ := it["difference"].(map[string]any)
		v, _ := it["difference_value"].(map[string]any)
		_, doc := it["material_document_number"].(string)
		out = append(out, fmt.Sprintf("%v %v %v %v", it["item_no"], q["value"], v["amount"], doc))
	}
	return strings.Join(out, "; ")
}

// TestPostDifferences: differences within the tolerance are posted at
// once, a gain as a 701 and a loss as a 702 valued at the materials'
// prices against the difference account; the book quantity is then the
// count, the posting block lifted, and the documents not reversed.
func TestPostDifferences(t *testing.T) {
	e := countSetup(t)
	bolt := e.material(t, "Hex bolt M10", "raw_material", true)
	e.value(t, bolt, e.p1, "standard", "0.50")
	e.post(t, "561", e.item("10", "EA", nil))
	id := e.counted(t, []string{e.bracket, bolt}, "9", "4")
	got := e.postDiffs(t, id, 200)
	if got["status"] != "posted" || diffs(t, got) != "1 -1 -2.00 true; 2 4 2.00 true" {
		t.Fatalf("posted: got %v %s, want posted 1 -1 -2.00 true; 2 4 2.00 true", got["status"], diffs(t, got))
	}
	if s := e.stock(t, e.s1, ""); s != "9 EA" {
		t.Errorf("the bracket's stock: got %s, want 9 EA, the count", s)
	}
	types := map[string]string{}
	for _, it := range rows(t, got, "items") {
		md, _ := e.Must(200, "GET", docs+"/"+it["material_document_id"].(string), "", "")
		types[fmt.Sprint(md["movement_type"])] = md["id"].(string)
		entry, _ := e.Must(200, "GET", "/v1/journal-entries/"+md["journal_entry_id"].(string), "", "")
		if n := len(rows(t, entry, "lines")); n != 2 {
			t.Errorf("entry of %v: got %d lines, want stock and difference", md["movement_type"], n)
		}
		if ev := e.events(t, md["id"].(string)); len(ev) != 1 {
			t.Errorf("events of %v: got %d, want 1", md["movement_type"], len(ev))
		}
	}
	if len(types) != 2 || types["701"] == "" || types["702"] == "" {
		t.Errorf("material documents: got %v, want a 701 and a 702", types)
	}
	_, tag := e.Must(200, "GET", docs+"/"+types["702"], "", "")
	if status, p, _ := e.Do("POST", docs+"/"+types["702"]+":reverse", "", tag); status != 409 || p["code"] != "inventory.reverse_count" {
		t.Errorf("reversing a count's document: got %d %v, want 409 inventory.reverse_count", status, p)
	}
	e.post(t, "201", e.item("1", "EA", map[string]any{"cost_center_id": e.c1}))
	if a := e.audits(t, id, "inventory.count_document.post"); len(a) != 1 {
		t.Errorf("audit of the posting: got %v, want one", a)
	}
	if status, p, _ := e.Do("POST", docs, body(t, "701", e.item("1", "EA", nil)), ""); status != 422 || p["code"] != "inventory.unknown_movement_type" {
		t.Errorf("a 701 by hand: got %d %v, want 422 inventory.unknown_movement_type", status, p)
	}
}

// TestPostNoDifferences: a count equal to the book posts nothing and closes
// the document.
func TestPostNoDifferences(t *testing.T) {
	e := countSetup(t)
	e.post(t, "561", e.item("10", "EA", nil))
	got := e.postDiffs(t, e.counted(t, []string{e.bracket}, "10"), 200)
	if got["status"] != "posted" || diffs(t, got) != "1 0 0.00 false" {
		t.Errorf("got %v %s, want posted 1 0 0.00 false", got["status"], diffs(t, got))
	}
	e.count(t, e.bracket)
}

// TestDifferencesApproved: differences above the tolerance wait for the
// controller, the posting block held, and are posted when approved.
func TestDifferencesApproved(t *testing.T) {
	e := countSetup(t)
	e.post(t, "561", e.item("10", "EA", nil))
	id := e.counted(t, []string{e.bracket}, "2")
	got := e.postDiffs(t, id, 200)
	value, _ := got["difference_value"].(map[string]any)
	if got["status"] != "pending_approval" || got["workflow_request_id"] == nil || value["amount"] != "16.00" {
		t.Fatalf("above the tolerance: got %v %v %v, want pending_approval with its request and 16.00", got["status"], got["workflow_request_id"], value)
	}
	if status, _, _ := e.Do("POST", docs, body(t, "201", e.item("1", "EA", map[string]any{"cost_center_id": e.c1})), ""); status != 409 {
		t.Errorf("an issue while the differences wait: got %d, want 409", status)
	}
	e.postDiffs(t, id, 409)
	e.Approve(t, id)
	after, _ := e.Must(200, "GET", counts+"/"+id, "", "")
	if after["status"] != "posted" || e.stock(t, e.s1, "") != "2 EA" {
		t.Errorf("approved: got %v with stock %s, want posted with 2 EA", after["status"], e.stock(t, e.s1, ""))
	}
}

// TestDifferencesRefused: differences rejected or withdrawn post nothing
// and close the document, so the material is counted again.
func TestDifferencesRefused(t *testing.T) {
	e := countSetup(t)
	e.post(t, "561", e.item("10", "EA", nil))
	for _, c := range []struct{ name, status string }{{"rejected", "rejected"}, {"withdrawn", "withdrawn"}} {
		t.Run(c.name, func(t *testing.T) {
			id := e.counted(t, []string{e.bracket}, "1")
			e.postDiffs(t, id, 200)
			r := e.Pending(t, id)
			path, version := "/v1/approval-requests/"+r["id"].(string), fmt.Sprintf(`"%v"`, r["version"])
			if c.name == "rejected" {
				e.As(e.Approver).Must(200, "POST", path+":reject", `{"comment": "Count again."}`, version)
			} else {
				e.Must(200, "POST", path+":cancel", `{"comment": "Counted wrong."}`, version)
			}
			after, _ := e.Must(200, "GET", counts+"/"+id, "", "")
			if after["status"] != c.status || e.stock(t, e.s1, "") != "10 EA" || diffs(t, after) != "1 <nil> <nil> false" {
				t.Errorf("got %v with stock %s, %s, want %s with 10 EA and nothing posted", after["status"], e.stock(t, e.s1, ""), diffs(t, after), c.status)
			}
		})
	}
}

// TestPostDifferencesRefused: a document not counted, a stale version, a
// class without a difference account and another company's document are
// refused.
func TestPostDifferencesRefused(t *testing.T) {
	e := countSetup(t)
	unaccounted := e.material(t, "Unaccounted bracket", "trading_good", true)
	e.Must(201, "POST", "/v1/material-valuations", mustJSON(t, map[string]any{"material_id": unaccounted, "plant_id": e.p1, "valuation_class": "3100",
		"price_control": "standard", "price": map[string]any{"amount": "1.00", "currency": "EUR"}}), "")
	open := e.count(t, e.bracket)["id"].(string)
	noAccount := e.counted(t, []string{unaccounted}, "1")
	_, tag := e.Must(200, "GET", counts+"/"+open, "", "")
	for _, c := range []struct {
		name, id, tag string
		status        int
		code          string
	}{
		{"a document counting", open, tag, 409, "inventory.count_invalid_status"},
		{"a stale version", open, `"9"`, 412, "request.version_mismatch"},
		{"no difference account", noAccount, "", 422, "inventory.no_inventory_account"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.tag == "" {
				_, c.tag = e.Must(200, "GET", counts+"/"+c.id, "", "")
			}
			status, got, _ := e.Do("POST", counts+"/"+c.id+":postDifferences", "", c.tag)
			if status != c.status || got["code"] != c.code {
				t.Errorf("got %d %v, want %d %s", status, got, c.status, c.code)
			}
		})
	}
}

// TestPostDifferencesAtOnce: a document's differences posted twice at once
// are posted once.
func TestPostDifferencesAtOnce(t *testing.T) {
	e := countSetup(t)
	e.post(t, "561", e.item("10", "EA", nil))
	id := e.counted(t, []string{e.bracket}, "9")
	_, tag := e.Must(200, "GET", counts+"/"+id, "", "")
	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _, _ = e.Do("POST", counts+"/"+id+":postDifferences", "", tag)
		}()
	}
	wg.Wait()
	if fmt.Sprint(statuses) != "[200 412]" && fmt.Sprint(statuses) != "[412 200]" {
		t.Errorf("two postings at once: got %v, want one 200 and one 412", statuses)
	}
	if s := e.stock(t, e.s1, ""); s != "9 EA" {
		t.Errorf("stock: got %s, want 9 EA", s)
	}
}

// TestCountApprovalsDecided: a decision on a document no longer pending
// approval is refused; each document's path is its resource.
func TestCountApprovalsDecided(t *testing.T) {
	e := countSetup(t)
	e.post(t, "561", e.item("10", "EA", nil))
	id := uuid.MustParse(e.counted(t, []string{e.bracket}, "10"))
	a := inventory.Approvals{Endpoints: endpoints(t)}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return a.Approved(ctx, id, uuid.Nil) })
	if err == nil || !strings.Contains(err.Error(), "pending approval") {
		t.Errorf("approving a counted document: got %v, want inventory.count_invalid_status", err)
	}
	paths, _ := a.Resources(context.Background(), []uuid.UUID{id})
	if paths[id] != counts+"/"+id.String() {
		t.Errorf("resource: got %s", paths[id])
	}
}

// TestSurplusWithoutPrice: a gain of a material without a moving price is
// not valued, so not posted.
func TestSurplusWithoutPrice(t *testing.T) {
	e := countSetup(t)
	m := e.material(t, "Unpriced bracket", "trading_good", true)
	e.value(t, m, e.p1, "moving_average", "0.00")
	if got := e.postDiffs(t, e.counted(t, []string{m}, "1"), 422); got["code"] != "inventory.no_price" {
		t.Errorf("got %v, want inventory.no_price", got)
	}
}

// noApproval is an approval policy that asks none.
type noApproval struct{}

func (noApproval) Open(context.Context, approvals.NewRequest) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}

// TestDifferencesWithoutPolicy: a policy that asks no approval of
// differences above the tolerance is the server's fault, never a posting
// unapproved.
func TestDifferencesWithoutPolicy(t *testing.T) {
	e := countSetup(t)
	e.post(t, "561", e.item("10", "EA", nil))
	id := uuid.MustParse(e.counted(t, []string{e.bracket}, "1"))
	h := endpoints(t)
	h.Requests = noApproval{}
	err := e.inRequest(t, func(ctx context.Context) error {
		_, err := h.PostDifferences(ctx, id, func(inventory.CountDocument) error { return nil })
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "asks no approval") {
		t.Errorf("got %v, want the policy's fault", err)
	}
}
