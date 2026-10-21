package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// tree is a hierarchy as a test reads it.
type tree struct {
	c    centers
	path string
}

// hierarchy creates an alternative cost center hierarchy.
func (c centers) hierarchy(t *testing.T, code string) tree {
	t.Helper()
	h, _ := c.Must(201, "POST", "/v1/hierarchies", fmt.Sprintf(`{"kind": "cost_center", "code": %q, "name": "Hierarchy %s"}`, code, code), "")
	return tree{c: c, path: "/v1/hierarchies/" + h["id"].(string)}
}

// get reads the tree and its ETag.
func (h tree) get(t *testing.T) (map[string]any, string) {
	t.Helper()
	return h.c.Must(200, "GET", h.path, "", "")
}

// request asks for a change of the tree with its current ETag and returns
// the pending change.
func (h tree) request(t *testing.T, op, body string) map[string]any {
	t.Helper()
	_, tag := h.get(t)
	ch, _ := h.c.Must(200, "POST", h.path+op, body, tag)
	if ch["status"] != "pending" {
		t.Errorf("%s: status got %v, want pending", op, ch["status"])
	}
	return ch
}

// refused asks for a change of the tree and expects a problem.
func (h tree) refused(t *testing.T, status int, code, op, body string) map[string]any {
	t.Helper()
	_, tag := h.get(t)
	return h.c.Problem(status, code, "POST", h.path+op, body, tag)
}

// apply requests a change and has the controller approve it; it returns
// the change's payload.
func (h tree) apply(t *testing.T, op, body string) map[string]any {
	t.Helper()
	ch := h.request(t, op, body)
	h.c.decide(t, ch["id"], "approve")
	return ch["payload"].(map[string]any)
}

// add adds an approved node below a node and returns its id.
func (h tree) add(t *testing.T, parent any, code string) string {
	t.Helper()
	return h.apply(t, fmt.Sprintf("/nodes/%v:add", parent), fmt.Sprintf(`{"code": %q, "name": "Node %s", "reason": "r"}`, code, code))["node_id"].(string)
}

// outline is the tree as "code@level[centers]" in tree order.
func outline(t *testing.T, tr map[string]any) string {
	t.Helper()
	var out []string
	for _, n := range tr["nodes"].([]any) {
		m := n.(map[string]any)
		out = append(out, fmt.Sprintf("%v@%v%v", m["code"], m["level"], m["center_ids"]))
	}
	return fmt.Sprint(out)
}

// activeCostCenter creates and approves a cost center in the standard
// leaf and returns its id.
func (c centers) activeCostCenter(t *testing.T, code string) string {
	t.Helper()
	cc := c.costCenter(t, code, "")
	c.decide(t, cc["pending_change_id"], "approve")
	return cc["id"].(string)
}

// paths is a center's paths as "hierarchy:node/node" strings.
func (c centers) paths(t *testing.T, center string) []string {
	t.Helper()
	page, _ := c.Must(200, "GET", "/v1/cost-centers/"+center+"/hierarchy-paths", "", "")
	var out []string
	for _, item := range page["items"].([]any) {
		p := item.(map[string]any)
		s := fmt.Sprint(p["hierarchy_code"], ":")
		for i, n := range p["nodes"].([]any) {
			if i > 0 {
				s += "/"
			}
			s += n.(map[string]any)["code"].(string)
		}
		out = append(out, s)
	}
	return out
}

// TestHierarchyNodes: a tree grows, is renamed, moved and pruned only
// through approved changes, which are planned when requested and again
// when approved; the ETag moves on with every applied change.
func TestHierarchyNodes(t *testing.T) {
	c := setupCenters(t)
	h := c.hierarchy(t, "ALT")
	tr, tag := h.get(t)
	root := tr["root_node_id"]
	if got := outline(t, tr); got != "[ALT@1[]]" {
		t.Errorf("new tree: got %s, want the root alone", got)
	}
	ch := h.request(t, fmt.Sprintf("/nodes/%v:add", root), `{"code": "A", "name": "Node A", "reason": "r"}`)
	if tr, now := h.get(t); now != tag || outline(t, tr) != "[ALT@1[]]" {
		t.Errorf("before approval: got %s %s, want the tree unchanged at %s", outline(t, tr), now, tag)
	}
	c.decide(t, ch["id"], "approve")
	a := ch["payload"].(map[string]any)["node_id"]
	b := h.add(t, a, "B")
	cNode := h.add(t, root, "C")
	h.apply(t, fmt.Sprintf("/nodes/%v:add", root), `{"code": "FIRST", "name": "First", "position": 0, "reason": "r"}`)
	tr, now := h.get(t)
	if got, want := outline(t, tr), "[ALT@1[] FIRST@2[] A@2[] B@3[] C@2[]]"; got != want {
		t.Errorf("tree: got %s, want %s", got, want)
	}
	if now == tag {
		t.Errorf("ETag after four applied changes: got %s, want it moved on", now)
	}
	h.apply(t, fmt.Sprintf("/nodes/%s:rename", b), `{"name": "Bee", "reason": "r"}`)
	h.refused(t, 422, "ledger.nothing_to_change", fmt.Sprintf("/nodes/%s:rename", b), `{"name": "Bee", "reason": "r"}`)
	h.apply(t, fmt.Sprintf("/nodes/%s:move", b), fmt.Sprintf(`{"parent_id": %q, "reason": "r"}`, cNode))
	h.refused(t, 422, "ledger.nothing_to_change", fmt.Sprintf("/nodes/%s:move", b), fmt.Sprintf(`{"parent_id": %q, "reason": "r"}`, cNode))
	h.apply(t, fmt.Sprintf("/nodes/%s:remove", a), `{"reason": "r"}`)
	sub, _ := c.Must(200, "GET", fmt.Sprintf("%s/nodes/%s", h.path, cNode), "", "")
	if got, want := outline(t, sub), "[C@2[] B@3[]]"; got != want {
		t.Errorf("subtree of C: got %s, want %s", got, want)
	}
	if n := sub["nodes"].([]any)[1].(map[string]any); n["name"] != "Bee" || n["parent_id"] != cNode {
		t.Errorf("B: got %v, want Bee below C", n)
	}
	c.Problem(404, "request.not_found", "GET", fmt.Sprintf("%s/nodes/%v", h.path, a), "", "")
	c.Problem(404, "request.not_found", "GET", "/v1/hierarchies/"+uuid.NewString(), "", "")
	_, tag = h.get(t)
	c.Problem(412, "request.version_mismatch", "POST", fmt.Sprintf("%s/nodes/%s:rename", h.path, b), `{"name": "X", "reason": "r"}`, `"1"`)
	c.Problem(404, "request.not_found", "POST", fmt.Sprintf("/v1/hierarchies/%s/nodes/%s:rename", uuid.New(), b), `{"name": "X", "reason": "r"}`, tag)
	got := apitest.Published(t, c.ten, outbox.Event{Context: "ledger", Name: "hierarchy_changed"})
	var changes []any
	for _, e := range got {
		if e["code"] == "ALT" {
			changes = append(changes, e["change"])
		}
	}
	if want := "[create node_create node_create node_create node_create node_rename node_move node_remove]"; fmt.Sprint(changes) != want {
		t.Errorf("hierarchy_changed: got %v, want %s", changes, want)
	}
}

// TestHierarchyRefusals: what a tree does not allow, refused when asked
// for, with the reason.
func TestHierarchyRefusals(t *testing.T) {
	c := setupCenters(t)
	h := c.hierarchy(t, "ALT")
	tr, _ := h.get(t)
	root := tr["root_node_id"]
	a := h.add(t, root, "A")
	b := h.add(t, a, "B")
	sibling := h.add(t, root, "C")
	cc := c.activeCostCenter(t, "1000")
	h.apply(t, ":place", fmt.Sprintf(`{"center_id": %q, "node_id": %q, "reason": "r"}`, cc, b))
	for name, tc := range map[string]struct{ op, body, field, reason string }{
		"a node below one holding centers": {fmt.Sprintf("/nodes/%s:add", b), `{"code": "X", "name": "X", "reason": "r"}`, "node_id", "has_centers"},
		"a node below an unknown node":     {fmt.Sprintf("/nodes/%s:add", uuid.New()), `{"code": "X", "name": "X", "reason": "r"}`, "node_id", "unknown"},
		"a node moved below itself":        {fmt.Sprintf("/nodes/%s:move", a), fmt.Sprintf(`{"parent_id": %q, "reason": "r"}`, a), "parent_id", "cycle"},
		"a node moved below its child":     {fmt.Sprintf("/nodes/%s:move", a), fmt.Sprintf(`{"parent_id": %q, "reason": "r"}`, b), "parent_id", "cycle"},
		"a node moved below an unknown node": {fmt.Sprintf("/nodes/%s:move", a), fmt.Sprintf(`{"parent_id": %q, "reason": "r"}`, uuid.New()),
			"parent_id", "unknown"},
		"an unknown node moved": {fmt.Sprintf("/nodes/%s:move", uuid.New()), fmt.Sprintf(`{"parent_id": %q, "reason": "r"}`, a), "node_id", "unknown"},
		"a node moved below one holding centers": {fmt.Sprintf("/nodes/%s:move", sibling), fmt.Sprintf(`{"parent_id": %q, "reason": "r"}`, b),
			"parent_id", "has_centers"},
		"the root moved":                 {fmt.Sprintf("/nodes/%v:move", root), fmt.Sprintf(`{"parent_id": %q, "reason": "r"}`, a), "node_id", "root"},
		"an unknown node renamed":        {fmt.Sprintf("/nodes/%s:rename", uuid.New()), `{"name": "X", "reason": "r"}`, "node_id", "unknown"},
		"the root removed":               {fmt.Sprintf("/nodes/%v:remove", root), `{"reason": "r"}`, "node_id", "root"},
		"a node with children removed":   {fmt.Sprintf("/nodes/%s:remove", a), `{"reason": "r"}`, "node_id", "has_children"},
		"a node holding centers removed": {fmt.Sprintf("/nodes/%s:remove", b), `{"reason": "r"}`, "node_id", "has_centers"},
		"an unknown node removed":        {fmt.Sprintf("/nodes/%s:remove", uuid.New()), `{"reason": "r"}`, "node_id", "unknown"},
		"a center at a node with children": {":place", fmt.Sprintf(`{"center_id": %q, "node_id": %q, "reason": "r"}`, cc, a), "node_id",
			"has_children"},
		"a center at an unknown node": {":place", fmt.Sprintf(`{"center_id": %q, "node_id": %q, "reason": "r"}`, cc, uuid.New()), "node_id", "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			h := tree{c: c.in(t), path: h.path}
			p := h.refused(t, 422, "ledger.hierarchy_change_invalid", tc.op, tc.body)
			errs, _ := p["errors"].([]any)
			if len(errs) != 1 {
				t.Fatalf("errors: got %v, want one", p["errors"])
			}
			if got := errs[0].(map[string]any)["field"]; got != tc.field {
				t.Errorf("field: got %v, want %s", got, tc.field)
			}
			if got := errs[0].(map[string]any)["code"]; got != tc.reason {
				t.Errorf("code: got %v, want %s", got, tc.reason)
			}
		})
	}
	h.refused(t, 409, "ledger.hierarchy_node_exists", fmt.Sprintf("/nodes/%s:add", a), `{"code": "B", "name": "X", "reason": "r"}`)
	h.refused(t, 422, "ledger.nothing_to_change", ":place", fmt.Sprintf(`{"center_id": %q, "node_id": %q, "reason": "r"}`, cc, b))
	// A depth beyond 12 levels: the root and A, B make three.
	deep := b
	h.apply(t, ":place", fmt.Sprintf(`{"center_id": %q, "node_id": null, "reason": "r"}`, cc))
	for i := 4; i <= 12; i++ {
		deep = h.add(t, deep, fmt.Sprintf("L%d", i))
	}
	p := h.refused(t, 422, "ledger.hierarchy_change_invalid", fmt.Sprintf("/nodes/%s:add", deep), `{"code": "L13", "name": "X", "reason": "r"}`)
	if errs := p["errors"].([]any); errs[0].(map[string]any)["code"] != "too_deep" {
		t.Errorf("a thirteenth level: got %v, want too_deep", errs)
	}
	p = h.refused(t, 422, "ledger.hierarchy_change_invalid", fmt.Sprintf("/nodes/%s:move", sibling), fmt.Sprintf(`{"parent_id": %q, "reason": "r"}`, deep))
	if errs := p["errors"].([]any); errs[0].(map[string]any)["code"] != "too_deep" {
		t.Errorf("a node moved to a thirteenth level: got %v, want too_deep", errs)
	}
	c.Problem(409, "ledger.hierarchy_exists", "POST", "/v1/hierarchies", `{"kind": "cost_center", "code": "ALT", "name": "Again"}`, "")
	c.Problem(422, "request.validation_failed", "POST", "/v1/hierarchies", `{"kind": "order", "code": "ORD", "name": "Orders"}`, "")
	c.Problem(409, "ledger.standard_hierarchy_exists", "POST", "/v1/hierarchies", `{"kind": "cost_center", "code": "STD2", "name": "S", "is_standard": true}`, "")
}

// TestHierarchyPlaces: centers are placed, moved and taken out of
// alternative hierarchies, and moved but never taken out of the standard
// one; their paths show where they sit.
func TestHierarchyPlaces(t *testing.T) {
	c := setupCenters(t)
	cc := c.activeCostCenter(t, "1000")
	if got := c.paths(t, cc); fmt.Sprint(got) != "[STANDARD:STANDARD/LEAF]" {
		t.Errorf("a new center's paths: got %v, want the standard leaf", got)
	}
	h := c.hierarchy(t, "ALT")
	tr, _ := h.get(t)
	a := h.add(t, tr["root_node_id"], "A")
	place := func(node any) string { return fmt.Sprintf(`{"center_id": %q, "node_id": %v, "reason": "r"}`, cc, node) }
	ch := h.request(t, ":place", place(fmt.Sprintf("%q", a)))
	_, ccTag := c.Must(200, "GET", "/v1/cost-centers/"+cc, "", "")
	c.Problem(409, "ledger.center_change_pending", "POST", "/v1/cost-centers/"+cc+":lock", `{"valid_from": "2026-02-01", "lock_plan": true, "reason": "r"}`,
		ccTag)
	_, pendingTag := c.Must(200, "GET", "/v1/cost-centers/"+cc, "", "")
	c.decide(t, ch["id"], "approve")
	if center, tag := c.Must(200, "GET", "/v1/cost-centers/"+cc, "", ""); tag == pendingTag || center["pending_change_id"] != nil {
		t.Errorf("center after the place is applied: ETag %s (was %s), pending %v; want a new ETag and no pending change", tag, pendingTag, center["pending_change_id"])
	}
	if got := c.paths(t, cc); fmt.Sprint(got) != "[ALT:ALT/A STANDARD:STANDARD/LEAF]" {
		t.Errorf("paths: got %v, want ALT/A and the standard leaf", got)
	}
	page, _ := c.Must(200, "GET", "/v1/cost-centers/"+cc+"/hierarchy-paths?page_size=1", "", "")
	page, _ = c.Must(200, "GET", "/v1/cost-centers/"+cc+"/hierarchy-paths?page_size=1&page_token="+page["next_page_token"].(string), "", "")
	if items := page["items"].([]any); len(items) != 1 || items[0].(map[string]any)["is_standard"] != true {
		t.Errorf("second page of paths: got %v, want the standard hierarchy", items)
	}
	h.apply(t, ":place", place("null"))
	h.refused(t, 422, "ledger.nothing_to_change", ":place", place("null"))
	std := tree{c: c, path: "/v1/hierarchies/" + c.ccTree.String()}
	p := std.refused(t, 422, "ledger.hierarchy_change_invalid", ":place", place("null"))
	if errs := p["errors"].([]any); errs[0].(map[string]any)["code"] != "standard" {
		t.Errorf("out of the standard hierarchy: got %v, want standard", errs)
	}
	tr, _ = std.get(t)
	other := std.add(t, tr["root_node_id"], "OTHER")
	std.apply(t, ":place", place(fmt.Sprintf("%q", other)))
	if got := c.paths(t, cc); fmt.Sprint(got) != "[STANDARD:STANDARD/OTHER]" {
		t.Errorf("paths after the move: got %v, want the standard OTHER only", got)
	}
	pending := c.costCenter(t, "2000", "")
	pc := c.profitCenter(t, "PC", "", c.de)
	for name, body := range map[string]string{
		"a pending center":                    fmt.Sprintf(`{"center_id": %q, "node_id": %q, "reason": "r"}`, pending["id"], a),
		"a profit center in a cost hierarchy": fmt.Sprintf(`{"center_id": %q, "node_id": %q, "reason": "r"}`, pc[len("/v1/profit-centers/"):], a),
	} {
		t.Run(name, func(t *testing.T) {
			h := tree{c: c.in(t), path: h.path}
			want := map[string]string{"a pending center": "ledger.invalid_center_status"}[name]
			if want == "" {
				h.refused(t, 422, "request.reference_not_found", ":place", body)
				return
			}
			h.refused(t, 409, want, ":place", body)
		})
	}
	c.Problem(404, "request.not_found", "GET", "/v1/cost-centers/"+uuid.NewString()+"/hierarchy-paths", "", "")
	c.Problem(404, "request.not_found", "GET", "/v1/profit-centers/"+uuid.NewString()+"/hierarchy-paths", "", "")
	page, _ = c.Must(200, "GET", pc+"/hierarchy-paths", "", "")
	if items := page["items"].([]any); len(items) != 1 {
		t.Errorf("a profit center's paths: got %v, want the standard one", items)
	}
	var actions []string
	pgtest.Scan(t, `SELECT array_agg(action ORDER BY seq) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2`,
		[]any{c.ten.ID, uuid.MustParse(h.path[len("/v1/hierarchies/"):])}, &actions)
	want := "[ledger.hierarchy.create ledger.hierarchy.request ledger.hierarchy.node_create ledger.hierarchy.request ledger.hierarchy.place " +
		"ledger.hierarchy.request ledger.hierarchy.place]"
	if fmt.Sprint(actions) != want {
		t.Errorf("audit trail: got %v, want %s", actions, want)
	}
}

// TestCentersCreatedInTheStandardHierarchy: a new center names a leaf of
// the standard hierarchy of its kind, checked when it is requested and
// again when it is approved.
func TestCentersCreatedInTheStandardHierarchy(t *testing.T) {
	c := setupCenters(t)
	std := tree{c: c, path: "/v1/hierarchies/" + c.ccTree.String()}
	tr, _ := std.get(t)
	body := func(code string, node any) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "code": %q, "name": "N", "responsible": "J", "category": "ADMIN", "valid_from": "2026-01-01",
			"standard_node_id": %q, "reason": "r"}`, c.de, code, node)
	}
	for name, tc := range map[string]struct {
		node   any
		reason string
	}{
		"the root, which has children": {tr["root_node_id"], "has_children"},
		"a node of no hierarchy":       {uuid.NewString(), "unknown"},
		"a node of another hierarchy":  {c.pcLeaf, "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			c := c.in(t)
			p := c.Problem(422, "ledger.hierarchy_change_invalid", "POST", "/v1/cost-centers", body("X", tc.node), "")
			if errs := p["errors"].([]any); errs[0].(map[string]any)["field"] != "standard_node_id" || errs[0].(map[string]any)["code"] != tc.reason {
				t.Errorf("errors: got %v, want standard_node_id %s", errs, tc.reason)
			}
		})
	}
	// The leaf gains a child before the create is approved: the approval
	// is refused, as the center could not sit there.
	cc, _ := c.Must(201, "POST", "/v1/cost-centers", body("1000", c.ccLeaf), "")
	std.add(t, c.ccLeaf, "BELOW")
	page, _ := c.ctl.Must(200, "GET", fmt.Sprintf("/v1/approval-requests?status=pending&object_id=%v", cc["pending_change_id"]), "", "")
	r := page["items"].([]any)[0].(map[string]any)
	c.ctl.Problem(422, "ledger.hierarchy_change_invalid", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`,
		fmt.Sprintf(`"%v"`, r["version"]))
	// A tenant without a standard hierarchy cannot create centers yet.
	e := setup(t)
	p := e.Problem(422, "ledger.hierarchy_change_invalid", "POST", "/v1/profit-centers", fmt.Sprintf(`{"code": "PC", "name": "P", "responsible": "J",
		"legal_entity_ids": [%q], "valid_from": "2026-01-01", "standard_node_id": %q, "reason": "r"}`, e.de, uuid.New()), "")
	if errs := p["errors"].([]any); errs[0].(map[string]any)["code"] != "unknown" {
		t.Errorf("no standard hierarchy: got %v, want unknown", errs)
	}
}

// TestCentersCreatedUnassigned: a new center that names no leaf is placed,
// when its create is approved, in the standard hierarchy's UNASSIGNED leaf,
// for a controller to place later (product owner, 2026-10-21): proposing it
// changes no tree; the leaf is added below the root the first time and
// taken again after, and created with the standard hierarchy when the
// tenant has none; an UNASSIGNED node with child nodes, or another
// hierarchy coded STANDARD where the tenant has no standard one, refuses
// the approval.
func TestCentersCreatedUnassigned(t *testing.T) {
	c := setupCenters(t)
	std := tree{c: c, path: "/v1/hierarchies/" + c.ccTree.String()}
	outlined := func(h tree) string {
		t.Helper()
		tr, _ := h.get(t)
		return fmt.Sprint(outline(t, tr))
	}
	first, _ := c.Must(201, "POST", "/v1/cost-centers", ccBodyWithout(c.de, "U1"), "")
	if got := outlined(std); got != "[STANDARD@1[] LEAF@2[]]" {
		t.Errorf("tree while the create is pending: got %s, want it unchanged", got)
	}
	c.decide(t, first["pending_change_id"], "approve")
	second, _ := c.Must(201, "POST", "/v1/cost-centers", ccBodyWithout(c.de, "U2"), "")
	c.decide(t, second["pending_change_id"], "approve")
	if got, want := outlined(std), fmt.Sprintf("[STANDARD@1[] LEAF@2[] UNASSIGNED@2[%v %v]]", first["id"], second["id"]); got != want {
		t.Errorf("standard tree: got %s, want %s", got, want)
	}
	if got := c.paths(t, first["id"].(string)); fmt.Sprint(got) != "[STANDARD:STANDARD/UNASSIGNED]" {
		t.Errorf("paths: got %v, want [STANDARD:STANDARD/UNASSIGNED]", got)
	}

	pcs := tree{c: c, path: "/v1/hierarchies/" + c.pcTree.String()}
	pr, _ := pcs.get(t)
	pcs.add(t, pcs.add(t, pr["root_node_id"], "UNASSIGNED"), "BELOW")
	pu, _ := c.Must(201, "POST", "/v1/profit-centers", pcBodyWithout(c.de, "PU"), "")
	if code := c.approveRefused(t, pu["pending_change_id"]); code != "has_children" {
		t.Errorf("an UNASSIGNED node with children: got %s, want has_children", code)
	}

	bare := setupBareCenters(t)
	pc, _ := bare.Must(201, "POST", "/v1/profit-centers", pcBodyWithout(bare.de, "PC"), "")
	bare.decide(t, pc["pending_change_id"], "approve")
	if got := standardNodes(t, bare, "profit_center"); got != "STANDARD/STANDARD STANDARD/UNASSIGNED[1]" {
		t.Errorf("a tenant's first center: got %q, want STANDARD with its UNASSIGNED leaf holding it", got)
	}
	bare.Must(201, "POST", "/v1/hierarchies", `{"kind": "cost_center", "code": "STANDARD", "name": "Not standard"}`, "")
	cc, _ := bare.Must(201, "POST", "/v1/cost-centers", ccBodyWithout(bare.de, "CC"), "")
	if code := bare.approveRefused(t, cc["pending_change_id"]); code != "unknown" {
		t.Errorf("STANDARD taken by an alternative hierarchy: got %s, want unknown", code)
	}
}

// TestFirstCentersApprovedAtOnce: two first centers approved at the same
// time create one standard hierarchy with one UNASSIGNED leaf: the second
// approval waits for the first and finds what it created.
func TestFirstCentersApprovedAtOnce(t *testing.T) {
	c := setupBareCenters(t)
	h := endpoints()
	var pending []gen.OptUUID
	for _, code := range []string{"P1", "P2"} {
		pc, _ := c.Must(201, "POST", "/v1/profit-centers", pcBodyWithout(c.de, code), "")
		pending = append(pending, gen.NewOptUUID(uuid.MustParse(pc["pending_change_id"].(string))))
	}
	approve := func(p gen.OptUUID) func(ctx context.Context) error {
		return func(ctx context.Context) error { return created(ctx, h, p, "approve") }
	}
	first, second := apitest.Serialized(t, c.ten, c.ten.Human, approve(pending[0]), approve(pending[1]))
	if first != nil || second != nil {
		t.Fatalf("approvals: got %v and %v, want both", first, second)
	}
	if got := standardNodes(t, c, "profit_center"); got != "STANDARD/STANDARD STANDARD/UNASSIGNED[2]" {
		t.Errorf("after two first approvals: got %q, want one STANDARD with one UNASSIGNED leaf holding both", got)
	}
}

// ccBodyWithout and pcBodyWithout are create requests naming no leaf.
func ccBodyWithout(le uuid.UUID, code string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "code": %q, "name": "N", "responsible": "J", "category": "ADMIN", "valid_from": "2026-01-01",
		"reason": "r"}`, le, code)
}

func pcBodyWithout(le uuid.UUID, code string) string {
	return fmt.Sprintf(`{"code": %q, "name": "P", "responsible": "J", "legal_entity_ids": [%q], "valid_from": "2026-01-01", "reason": "r"}`, code, le)
}

// approveRefused has the controller approve the change and returns the
// reason of the 422 refusal of standard_node_id.
func (c centers) approveRefused(t *testing.T, change any) string {
	t.Helper()
	page, _ := c.ctl.Must(200, "GET", fmt.Sprintf("/v1/approval-requests?status=pending&object_id=%v", change), "", "")
	r := page["items"].([]any)[0].(map[string]any)
	p := c.ctl.Problem(422, "ledger.hierarchy_change_invalid", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`,
		fmt.Sprintf(`"%v"`, r["version"]))
	e := p["errors"].([]any)[0].(map[string]any)
	if e["field"] != "standard_node_id" {
		t.Errorf("refused field: got %v, want standard_node_id", e["field"])
	}
	return fmt.Sprint(e["code"])
}

// standardNodes is the tenant's standard hierarchy of the kind as
// "hierarchy/node" with the number of centers at each leaf that has any.
func standardNodes(t *testing.T, c centers, kind string) string {
	t.Helper()
	var nodes string
	pgtest.Scan(t, `SELECT string_agg(h.code || '/' || n.code || CASE WHEN m.n > 0 THEN '[' || m.n || ']' ELSE '' END, ' ' ORDER BY n.parent_id NULLS FIRST, n.code)
		FROM ledger.hierarchy h JOIN ledger.hierarchy_node n ON n.tenant_id = h.tenant_id AND n.hierarchy_id = h.id
		LEFT JOIN LATERAL (SELECT count(*) AS n FROM ledger.hierarchy_member x WHERE x.tenant_id = n.tenant_id AND x.node_id = n.id) m ON true
		WHERE h.tenant_id = $1 AND h.kind = $2 AND h.is_standard`, []any{c.ten.ID, kind}, &nodes)
	return nodes
}

// TestHierarchyChangesReplanned: changes planned against the same tree
// are each planned again when approved, so the second of two that clash
// is refused, also when both are approved at once.
func TestHierarchyChangesReplanned(t *testing.T) {
	c := setupCenters(t)
	h := c.hierarchy(t, "ALT")
	tr, _ := h.get(t)
	a := h.add(t, tr["root_node_id"], "A")
	b := h.add(t, tr["root_node_id"], "B")
	t.Run("the same code twice", func(t *testing.T) {
		first := h.request(t, fmt.Sprintf("/nodes/%s:add", a), `{"code": "X", "name": "X", "reason": "r"}`)
		second := h.request(t, fmt.Sprintf("/nodes/%s:add", b), `{"code": "X", "name": "X", "reason": "r"}`)
		c.decide(t, first["id"], "approve")
		page, _ := c.ctl.Must(200, "GET", fmt.Sprintf("/v1/approval-requests?status=pending&object_id=%v", second["id"]), "", "")
		r := page["items"].([]any)[0].(map[string]any)
		c.ctl.Problem(409, "ledger.hierarchy_node_exists", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`,
			fmt.Sprintf(`"%v"`, r["version"]))
		c.decide(t, second["id"], "reject")
	})
	t.Run("two moves that would make a cycle", func(t *testing.T) {
		aUnderB := h.request(t, fmt.Sprintf("/nodes/%s:move", a), fmt.Sprintf(`{"parent_id": %q, "reason": "r"}`, b))
		bUnderA := h.request(t, fmt.Sprintf("/nodes/%s:move", b), fmt.Sprintf(`{"parent_id": %q, "reason": "r"}`, a))
		approve := func(ch map[string]any) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				return ledger.CenterChanges{}.Approved(ctx, uuid.MustParse(ch["id"].(string)), uuid.MustParse(ch["workflow_request_id"].(string)))
			}
		}
		first, second := serializeOn(t, c.ten, "the hierarchy", rowLockWaits(t), approve(aUnderB), approve(bUnderA))
		if first != nil {
			t.Fatalf("first move: got %v, want it committed", first)
		}
		if got := problem.From(second); got.Code != "ledger.hierarchy_change_invalid" || got.Errors[0].Code != "cycle" {
			t.Errorf("second move: got %v, want a cycle refused", second)
		}
	})
	t.Run("a withdrawn change", func(t *testing.T) {
		ch := h.request(t, fmt.Sprintf("/nodes/%s:rename", a), `{"name": "Y", "reason": "r"}`)
		c.decide(t, ch["id"], "reject")
		page, _ := c.Must(200, "GET", fmt.Sprintf("/v1/center-changes?hierarchy_id=%s&status=rejected", h.path[len("/v1/hierarchies/"):]), "", "")
		if items := page["items"].([]any); len(items) != 2 {
			t.Errorf("rejected changes of the hierarchy: got %v, want two", items)
		}
	})
}

// TestHierarchyListing: hierarchies are listed by code, filtered and
// paged.
func TestHierarchyListing(t *testing.T) {
	c := setupCenters(t)
	c.hierarchy(t, "ALT")
	for query, want := range map[string]string{
		"":                                  "[ALT STANDARD STANDARD]",
		"?kind=cost_center":                 "[ALT STANDARD]",
		"?is_standard=false":                "[ALT]",
		"?code=STANDARD&kind=profit_center": "[STANDARD]",
	} {
		t.Run(query, func(t *testing.T) {
			c := c.in(t)
			page, _ := c.Must(200, "GET", "/v1/hierarchies"+query, "", "")
			var codes []any
			for _, item := range page["items"].([]any) {
				codes = append(codes, item.(map[string]any)["code"])
			}
			if fmt.Sprint(codes) != want {
				t.Errorf("hierarchies: got %v, want %s", codes, want)
			}
		})
	}
	page, _ := c.Must(200, "GET", "/v1/hierarchies?page_size=2", "", "")
	page, _ = c.Must(200, "GET", "/v1/hierarchies?page_size=2&page_token="+page["next_page_token"].(string), "", "")
	if items := page["items"].([]any); len(items) != 1 {
		t.Errorf("second page: got %v, want one", items)
	}
	c.Problem(400, "request.invalid_order_by", "GET", "/v1/hierarchies?order_by=bogus", "", "")
	c.Problem(400, "request.invalid_order_by", "GET", "/v1/cost-centers/"+c.activeCostCenter(t, "1000")+"/hierarchy-paths?order_by=bogus", "", "")
}

// trees are the hierarchies TestCenterDatabaseFailures changes: an empty
// node SPARE in the standard cost center tree, an alternative cost
// center tree ALT with a leaf, and an alternative profit center tree
// ALTPC holding the profit center.
type trees struct {
	spare, alt, altLeaf, altPC uuid.UUID
}

func (c centers) trees(t *testing.T, pc uuid.UUID) trees {
	t.Helper()
	std := tree{c: c, path: "/v1/hierarchies/" + c.ccTree.String()}
	tr, _ := std.get(t)
	spare := std.add(t, tr["root_node_id"], "SPARE")
	alt := c.hierarchy(t, "ALT")
	tr, _ = alt.get(t)
	altLeaf := alt.add(t, tr["root_node_id"], "AL")
	h, _ := c.Must(201, "POST", "/v1/hierarchies", `{"kind": "profit_center", "code": "ALTPC", "name": "Alternative"}`, "")
	altPC := tree{c: c, path: "/v1/hierarchies/" + h["id"].(string)}
	leaf := altPC.add(t, h["root_node_id"], "PL")
	altPC.apply(t, ":place", fmt.Sprintf(`{"center_id": %q, "node_id": %q, "reason": "r"}`, pc, leaf))
	return trees{spare: uuid.MustParse(spare), alt: uuid.MustParse(alt.path[len("/v1/hierarchies/"):]), altLeaf: uuid.MustParse(altLeaf),
		altPC: uuid.MustParse(h["id"].(string))}
}

// hierarchyCalls are the hierarchy handlers for TestCenterDatabaseFailures.
func hierarchyCalls(h *ledger.Endpoints, c centers, ts trees, pc, cc uuid.UUID) map[string]func(ctx context.Context) error {
	place := func(tree, center uuid.UUID, node gen.NilUUID, outcome string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			res, err := h.PlaceInHierarchy(ctx, &gen.HierarchyPlace{CenterID: center, NodeID: node, Reason: "r"}, gen.PlaceInHierarchyParams{ID: tree, IfMatch: "*"})
			return decided(ctx, res, err, outcome)
		}
	}
	return map[string]func(ctx context.Context) error{
		"create a hierarchy": func(ctx context.Context) error {
			_, err := h.CreateHierarchy(ctx, &gen.HierarchyCreate{Kind: "cost_center", Code: "NEW", Name: "N"}, gen.CreateHierarchyParams{})
			return err
		},
		"list hierarchies": func(ctx context.Context) error {
			_, err := h.ListHierarchies(ctx, gen.ListHierarchiesParams{})
			return err
		},
		"get a hierarchy": func(ctx context.Context) error {
			_, err := h.GetHierarchy(ctx, gen.GetHierarchyParams{ID: c.ccTree})
			return err
		},
		"get a hierarchy node": func(ctx context.Context) error {
			_, err := h.GetHierarchyNode(ctx, gen.GetHierarchyNodeParams{ID: c.ccTree, NodeID: c.ccLeaf})
			return err
		},
		"add a node and approve": func(ctx context.Context) error {
			res, err := h.AddHierarchyNode(ctx, &gen.HierarchyNodeAdd{Code: "NEW", Name: "N", Reason: "r"},
				gen.AddHierarchyNodeParams{ID: ts.alt, NodeID: ts.altLeaf, IfMatch: "*"})
			return decided(ctx, res, err, "approve")
		},
		"rename a node and approve": func(ctx context.Context) error {
			res, err := h.RenameHierarchyNode(ctx, &gen.HierarchyNodeRename{Name: "M", Reason: "r"},
				gen.RenameHierarchyNodeParams{ID: c.ccTree, NodeID: c.ccLeaf, IfMatch: "*"})
			return decided(ctx, res, err, "approve")
		},
		"move a node and approve": func(ctx context.Context) error {
			res, err := h.MoveHierarchyNode(ctx, &gen.HierarchyNodeMove{ParentID: ts.spare, Reason: "r"},
				gen.MoveHierarchyNodeParams{ID: c.ccTree, NodeID: c.ccLeaf, IfMatch: "*"})
			return decided(ctx, res, err, "approve")
		},
		"remove a node and approve": func(ctx context.Context) error {
			res, err := h.RemoveHierarchyNode(ctx, &gen.HierarchyNodeRemove{Reason: "r"}, gen.RemoveHierarchyNodeParams{ID: c.ccTree, NodeID: ts.spare, IfMatch: "*"})
			return decided(ctx, res, err, "approve")
		},
		"rename a node and reject": func(ctx context.Context) error {
			res, err := h.RenameHierarchyNode(ctx, &gen.HierarchyNodeRename{Name: "M", Reason: "r"},
				gen.RenameHierarchyNodeParams{ID: ts.alt, NodeID: ts.altLeaf, IfMatch: "*"})
			return decided(ctx, res, err, "reject")
		},
		"place a cost center in an alternative hierarchy and approve": place(ts.alt, cc, gen.NilUUID{Value: ts.altLeaf}, "approve"),
		"move a cost center in the standard hierarchy and approve":    place(c.ccTree, cc, gen.NilUUID{Value: ts.spare}, "approve"),
		"move a cost center in the standard hierarchy, withdrawn":     place(c.ccTree, cc, gen.NilUUID{Value: ts.spare}, "cancel"),
		"take a profit center out of an alternative hierarchy":        place(ts.altPC, pc, gen.NilUUID{Null: true}, "approve"),
		"cost center paths": func(ctx context.Context) error {
			_, err := h.ListCostCenterPaths(ctx, gen.ListCostCenterPathsParams{ID: cc})
			return err
		},
		"profit center paths": func(ctx context.Context) error {
			_, err := h.ListProfitCenterPaths(ctx, gen.ListProfitCenterPathsParams{ID: pc})
			return err
		},
	}
}
