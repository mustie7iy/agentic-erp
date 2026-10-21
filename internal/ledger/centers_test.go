package ledger_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// centers is a ledger tenant with a controller, who decides center
// changes (ADR-0050 §11); the tenant's human requests them.
type centers struct {
	env
	ctl apitest.Client
	// The standard hierarchies (T56b) and the leaf each has, where the
	// helpers place new centers.
	ccTree, pcTree, ccLeaf, pcLeaf uuid.UUID
}

func setupBareCenters(t *testing.T) centers {
	t.Helper()
	e := withControllerRole(t, setup(t))
	a := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	a.Subject = "human|" + a.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'controller', true, 'active', $2, $2)`, e.ten.ID, a.ID, pgtest.Issuer(t), a.Subject)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, id, current_date - 1, $2 FROM identity.role WHERE tenant_id = $1 AND code IN ('all', $3)`,
		e.ten.ID, a.ID, approvals.DefaultCenterChangeRole)
	ctl := e.Client
	ctl.As = a
	return centers{env: e, ctl: ctl}
}

// setupCenters is setupBareCenters with a standard hierarchy of each kind,
// each with one leaf.
func setupCenters(t *testing.T) centers {
	t.Helper()
	c := setupBareCenters(t)
	c.ccTree, c.ccLeaf = c.standard(t, "cost_center")
	c.pcTree, c.pcLeaf = c.standard(t, "profit_center")
	return c
}

// withControllerRole gives the tenant the role center changes are
// approved by.
func withControllerRole(t *testing.T, e env) env {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, gen_random_uuid(), $2, $2, 'active', $3, $3)`,
		e.ten.ID, approvals.DefaultCenterChangeRole, e.ten.Human.ID)
	return e
}

// standard creates the standard hierarchy of a kind with one leaf, LEAF,
// and returns their ids.
func (c centers) standard(t *testing.T, kind string) (tree, leaf uuid.UUID) {
	t.Helper()
	h, tag := c.Must(201, "POST", "/v1/hierarchies", fmt.Sprintf(`{"kind": %q, "code": "STANDARD", "name": "Standard", "is_standard": true}`, kind), "")
	ch, _ := c.Must(200, "POST", fmt.Sprintf("/v1/hierarchies/%v/nodes/%v:add", h["id"], h["root_node_id"]), `{"code": "LEAF", "name": "Leaf", "reason": "r"}`, tag)
	c.decide(t, ch["id"], "approve")
	return uuid.MustParse(h["id"].(string)), uuid.MustParse(ch["payload"].(map[string]any)["node_id"].(string))
}

// ccBody and pcBody add the standard leaf to a create request's body.
func (c centers) ccBody(body string) string { return withNode(body, c.ccLeaf) }
func (c centers) pcBody(body string) string { return withNode(body, c.pcLeaf) }

func withNode(body string, node uuid.UUID) string {
	return strings.Replace(body, `"reason"`, fmt.Sprintf(`"standard_node_id": %q, "reason"`, node), 1)
}

func (c centers) in(t *testing.T) centers {
	t.Helper()
	c.env = c.env.in(t)
	c.ctl.T = t
	return c
}

// decide has the controller approve, or reject, the change's pending
// approval request.
func (c centers) decide(t *testing.T, change any, action string) {
	t.Helper()
	page, _ := c.ctl.Must(200, "GET", fmt.Sprintf("/v1/approval-requests?status=pending&object_id=%v", change), "", "")
	items := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("pending approval requests of center change %v: got %v, want one", change, items)
	}
	r := items[0].(map[string]any)
	if r["object_type"] != approvals.CenterChange {
		t.Errorf("object_type: got %v, want %s", r["object_type"], approvals.CenterChange)
	}
	c.ctl.Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":"+action, `{"comment": "ok"}`, fmt.Sprintf(`"%v"`, r["version"]))
}

// segment creates a segment and returns its id.
func (c centers) segment(t *testing.T, code string) string {
	t.Helper()
	s, _ := c.Must(201, "POST", "/v1/segments", fmt.Sprintf(`{"code": %q, "name": "Segment %s"}`, code, code), "")
	return s["id"].(string)
}

// profitCenter creates and approves a profit center in the companies,
// valid from 2026-01-01 to until ("" open-ended), and returns its path.
func (c centers) profitCenter(t *testing.T, code, until string, companies ...uuid.UUID) string {
	t.Helper()
	to := ""
	if until != "" {
		to = fmt.Sprintf(`, "valid_to": %q`, until)
	}
	les := ""
	for i, le := range companies {
		if i > 0 {
			les += ", "
		}
		les += `"` + le.String() + `"`
	}
	pc, _ := c.Must(201, "POST", "/v1/profit-centers", c.pcBody(fmt.Sprintf(`{"code": %q, "name": "PC %s", "responsible": "Jane Doe", "legal_entity_ids": [%s],
		"valid_from": "2026-01-01"%s, "reason": "New region"}`, code, code, les, to)), "")
	c.decide(t, pc["pending_change_id"], "approve")
	return "/v1/profit-centers/" + pc["id"].(string)
}

// costCenter requests a cost center of legal entity 2000 and returns it,
// pending.
func (c centers) costCenter(t *testing.T, code, extra string) map[string]any {
	t.Helper()
	cc, _ := c.Must(201, "POST", "/v1/cost-centers", c.ccBody(fmt.Sprintf(`{"legal_entity_id": %q, "code": %q, "name": "CC %s", "responsible": "Jane Doe",
		"category": "ADMIN", "valid_from": "2026-01-01"%s, "reason": "New office"}`, c.de, code, code, extra)), "")
	return cc
}

// periods are a center's periods as (from, to) pairs and one attribute.
func periods(t *testing.T, center map[string]any, attr string) []string {
	t.Helper()
	var out []string
	for _, p := range center["periods"].([]any) {
		m := p.(map[string]any)
		out = append(out, fmt.Sprintf("%v..%v %v", m["valid_from"], m["valid_to"], m[attr]))
	}
	return out
}

func wantPeriods(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: periods got %v, want %v", what, got, want)
	}
}

// TestSegments: segments are created and changed at once, and published.
func TestSegments(t *testing.T) {
	c := setupCenters(t)
	id := c.segment(t, "RETAIL")
	c.Problem(409, "ledger.segment_exists", "POST", "/v1/segments", `{"code": "RETAIL", "name": "Again"}`, "")
	path := "/v1/segments/" + id
	s, tag := c.Must(200, "GET", path, "", "")
	if s["code"] != "RETAIL" || s["name"] != "Segment RETAIL" {
		t.Errorf("segment: got %v %v, want RETAIL Segment RETAIL", s["code"], s["name"])
	}
	c.Problem(412, "request.version_mismatch", "PATCH", path, `{"name": "Retail"}`, `"9"`)
	c.Problem(422, "ledger.nothing_to_change", "PATCH", path, `{}`, tag)
	s, tag = c.Must(200, "PATCH", path, `{"name": "Retail", "description": "Stores"}`, tag)
	if s["description"] != "Stores" {
		t.Errorf("description: got %v, want Stores", s["description"])
	}
	s, _ = c.Must(200, "PATCH", path, `{"description": null}`, tag)
	if _, ok := s["description"]; ok || s["name"] != "Retail" {
		t.Errorf("after clearing the description: got %v, want name Retail and no description", s)
	}
	c.Problem(404, "request.not_found", "GET", "/v1/segments/"+uuid.NewString(), "", "")
	c.Problem(404, "request.not_found", "PATCH", "/v1/segments/"+uuid.NewString(), `{"name": "X"}`, `"1"`)
	c.segment(t, "ONLINE")
	page, _ := c.Must(200, "GET", "/v1/segments?page_size=1", "", "")
	if items := page["items"].([]any); len(items) != 1 || items[0].(map[string]any)["code"] != "ONLINE" || page["next_page_token"] == nil {
		t.Errorf("first page by code: got %v, want ONLINE and a next page", page)
	}
	page, _ = c.Must(200, "GET", "/v1/segments?code=RETAIL", "", "")
	if items := page["items"].([]any); len(items) != 1 {
		t.Errorf("by code: got %v, want one", items)
	}
	got := apitest.Published(t, c.ten, outbox.Event{Context: "ledger", Name: "segment_changed"})
	if len(got) != 4 || got[0]["change"] != "created" || got[1]["change"] != "updated" {
		t.Errorf("segment_changed: got %v, want created, updated, updated, created", got)
	}
}

// TestProfitCenterLifecycle: a profit center exists once a controller
// approves its creation; every change of its validity, lock, segment or
// companies waits for one, its texts do not.
func TestProfitCenterLifecycle(t *testing.T) {
	c := setupCenters(t)
	retail := c.segment(t, "RETAIL")
	body := fmt.Sprintf(`{"code": "CA-SALES", "name": "Sales", "responsible": "Jane Doe", "segment_id": %q, "legal_entity_ids": [%q],
		"valid_from": "2026-01-01", "reason": "New region"}`, retail, c.us)
	pc, _ := c.Must(201, "POST", "/v1/profit-centers", c.pcBody(body), "")
	path := "/v1/profit-centers/" + pc["id"].(string)
	if pc["status"] != "pending" || len(pc["periods"].([]any)) != 0 || pc["pending_change_id"] == nil {
		t.Errorf("requested: got %v, want pending, no periods, a pending change", pc)
	}
	c.Problem(409, "ledger.profit_center_exists", "POST", "/v1/profit-centers", c.pcBody(body), "")
	_, tag := c.Must(200, "GET", path, "", "")
	c.Problem(409, "ledger.invalid_center_status", "POST", path+":lock", `{"valid_from": "2026-02-01", "locked": true, "reason": "r"}`, tag)
	// The requester cannot decide their own change.
	page, _ := c.Must(200, "GET", fmt.Sprintf("/v1/approval-requests?object_id=%v", pc["pending_change_id"]), "", "")
	r := page["items"].([]any)[0].(map[string]any)
	c.Problem(403, "workflow.own_request", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "mine"}`, fmt.Sprintf(`"%v"`, r["version"]))
	c.decide(t, pc["pending_change_id"], "approve")
	pc, tag = c.Must(200, "GET", path, "", "")
	if pc["status"] != "active" || pc["valid_from"] != "2026-01-01" {
		t.Errorf("approved: got %v, want active from 2026-01-01", pc)
	}
	if les := pc["legal_entity_ids"].([]any); len(les) != 1 || les[0] != c.us.String() {
		t.Errorf("companies: got %v, want [%s]", les, c.us)
	}
	pc, tag = c.Must(200, "PATCH", path, `{"name": "Sales Canada", "responsible": "John Roe"}`, tag)
	if pc["name"] != "Sales Canada" || pc["responsible"] != "John Roe" {
		t.Errorf("texts: got %v, want Sales Canada, John Roe", pc)
	}
	c.profitCenterChanges(t, path, tag)
}

// profitCenterChanges walks an active profit center through each kind of
// change.
func (c centers) profitCenterChanges(t *testing.T, path, tag string) {
	t.Helper()
	lock := `{"valid_from": "2026-06-01", "valid_to": "2026-07-01", "locked": true, "reason": "Audit"}`
	ch, _ := c.Must(200, "POST", path+":lock", lock, tag)
	_, tag = c.Must(200, "GET", path, "", "")
	c.Problem(409, "ledger.center_change_pending", "POST", path+":lock", lock, tag)
	c.decide(t, ch["id"], "approve")
	pc, tag := c.Must(200, "GET", path, "", "")
	wantPeriods(t, "locked June", periods(t, pc, "locked"), "2026-01-01..2026-06-01 false", "2026-06-01..2026-07-01 true", "2026-07-01..<nil> false")
	c.Problem(422, "ledger.nothing_to_change", "POST", path+":lock", lock, tag)
	ch, _ = c.Must(200, "POST", path+":lock", `{"valid_from": "2026-06-01", "valid_to": "2026-07-01", "locked": false, "reason": "Done"}`, tag)
	c.decide(t, ch["id"], "approve")
	pc, tag = c.Must(200, "GET", path, "", "")
	wantPeriods(t, "unlocked", periods(t, pc, "locked"), "2026-01-01..<nil> false")
	ch, _ = c.Must(200, "POST", path+":redate", `{"valid_to": "2027-01-01", "reason": "Region closes"}`, tag)
	c.decide(t, ch["id"], "approve")
	pc, tag = c.Must(200, "GET", path, "", "")
	if pc["valid_to"] != "2027-01-01" {
		t.Errorf("redated: valid_to got %v, want 2027-01-01", pc["valid_to"])
	}
	ch, _ = c.Must(200, "POST", path+":move", `{"segment_id": null, "reason": "No segments yet"}`, tag)
	c.decide(t, ch["id"], "approve")
	pc, tag = c.Must(200, "GET", path, "", "")
	if _, ok := pc["segment_id"]; ok {
		t.Errorf("moved: segment_id got %v, want none", pc["segment_id"])
	}
	c.Problem(422, "ledger.nothing_to_change", "POST", path+":move", `{"segment_id": null, "reason": "Again"}`, tag)
	c.Problem(422, "request.reference_not_found", "POST", path+":move", fmt.Sprintf(`{"segment_id": %q, "reason": "r"}`, uuid.New()), tag)
	c.Problem(422, "ledger.nothing_to_change", "POST", path+":assign", fmt.Sprintf(`{"legal_entity_ids": [%q], "reason": "r"}`, c.us), tag)
	ch, _ = c.Must(200, "POST", path+":assign", fmt.Sprintf(`{"legal_entity_ids": [%q, %q], "reason": "UAE too"}`, c.us, c.de), tag)
	c.decide(t, ch["id"], "approve")
	pc, _ = c.Must(200, "GET", path, "", "")
	if les := pc["legal_entity_ids"].([]any); len(les) != 2 {
		t.Errorf("assigned: companies got %v, want both", les)
	}
	got := apitest.Published(t, c.ten, outbox.Event{Context: "ledger", Name: "profit_center_changed"})
	var changes []any
	for _, e := range got {
		changes = append(changes, e["change"])
	}
	if want := "[created texts locked locked redated moved assigned]"; fmt.Sprint(changes) != want {
		t.Errorf("profit_center_changed: got %v, want %s", changes, want)
	}
	ch, _ = c.Must(200, "GET", fmt.Sprintf("/v1/center-changes/%v", ch["id"]), "", "")
	if ch["status"] != "applied" || ch["applied_at"] == nil || ch["kind"] != "assign" {
		t.Errorf("assign change: got %v, want applied", ch)
	}
}

// TestCenterChangesRejectedAndWithdrawn: a rejected or withdrawn create
// leaves the center rejected, which frees its code; other changes leave
// the center as it was.
func TestCenterChangesRejectedAndWithdrawn(t *testing.T) {
	c := setupCenters(t)
	body := fmt.Sprintf(`{"code": "PC1", "name": "PC", "responsible": "Jane Doe", "legal_entity_ids": [%q], "valid_from": "2026-01-01", "reason": "r"}`, c.de)
	pc, _ := c.Must(201, "POST", "/v1/profit-centers", c.pcBody(body), "")
	c.decide(t, pc["pending_change_id"], "reject")
	pc, tag := c.Must(200, "GET", "/v1/profit-centers/"+pc["id"].(string), "", "")
	if pc["status"] != "rejected" {
		t.Errorf("rejected create: status got %v, want rejected", pc["status"])
	}
	c.Problem(409, "ledger.invalid_center_status", "PATCH", "/v1/profit-centers/"+pc["id"].(string), `{"name": "X"}`, tag)
	pc, _ = c.Must(201, "POST", "/v1/profit-centers", c.pcBody(body), "")
	page, _ := c.Must(200, "GET", fmt.Sprintf("/v1/approval-requests?object_id=%v", pc["pending_change_id"]), "", "")
	r := page["items"].([]any)[0].(map[string]any)
	c.Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":cancel", `{"comment": "Wrong code"}`, fmt.Sprintf(`"%v"`, r["version"]))
	pc, _ = c.Must(200, "GET", "/v1/profit-centers/"+pc["id"].(string), "", "")
	if pc["status"] != "rejected" {
		t.Errorf("withdrawn create: status got %v, want rejected", pc["status"])
	}
	path := c.profitCenter(t, "PC1", "", c.de)
	_, tag = c.Must(200, "GET", path, "", "")
	ch, _ := c.Must(200, "POST", path+":lock", `{"valid_from": "2026-03-01", "locked": true, "reason": "r"}`, tag)
	c.decide(t, ch["id"], "reject")
	pc, _ = c.Must(200, "GET", path, "", "")
	wantPeriods(t, "rejected lock", periods(t, pc, "locked"), "2026-01-01..<nil> false")
	ch, _ = c.Must(200, "GET", fmt.Sprintf("/v1/center-changes/%v", ch["id"]), "", "")
	if ch["status"] != "rejected" {
		t.Errorf("rejected lock: status got %v, want rejected", ch["status"])
	}
	page, _ = c.Must(200, "GET", "/v1/center-changes?status=rejected&profit_center_id="+pc["id"].(string), "", "")
	if items := page["items"].([]any); len(items) != 1 {
		t.Errorf("rejected changes of the profit center: got %v, want one", items)
	}
}

// TestCostCenterLifecycle: a cost center's periods split and join as its
// locks, profit center and validity change, each approved; its profit
// center must be one the company may post to, valid over the period.
func TestCostCenterLifecycle(t *testing.T) {
	c := setupCenters(t)
	sales := c.profitCenter(t, "SALES", "", c.de)
	salesID := sales[len("/v1/profit-centers/"):]
	cc := c.costCenter(t, "1000", fmt.Sprintf(`, "profit_center_id": %q`, salesID))
	path := "/v1/cost-centers/" + cc["id"].(string)
	c.Problem(409, "ledger.cost_center_exists", "POST", "/v1/cost-centers", c.ccBody(fmt.Sprintf(`{"legal_entity_id": %q, "code": "1000", "name": "Again",
		"responsible": "J", "category": "ADMIN", "valid_from": "2026-01-01", "reason": "r"}`, c.de)), "")
	c.decide(t, cc["pending_change_id"], "approve")
	cc, tag := c.Must(200, "GET", path, "", "")
	wantPeriods(t, "created", periods(t, cc, "profit_center_id"), "2026-01-01..<nil> "+salesID)
	ch, _ := c.Must(200, "POST", path+":lock", `{"valid_from": "2026-04-01", "lock_actual_cost": true, "reason": "Winding down"}`, tag)
	c.decide(t, ch["id"], "approve")
	cc, tag = c.Must(200, "GET", path, "", "")
	wantPeriods(t, "locked from April", periods(t, cc, "lock_actual_cost"), "2026-01-01..2026-04-01 false", "2026-04-01..<nil> true")
	other := c.profitCenter(t, "OTHER", "", c.de)
	ch, _ = c.Must(200, "POST", path+":move", fmt.Sprintf(`{"valid_from": "2026-03-01", "profit_center_id": %q, "category": "SALES", "reason": "Reorg"}`,
		other[len("/v1/profit-centers/"):]), tag)
	c.decide(t, ch["id"], "approve")
	cc, tag = c.Must(200, "GET", path, "", "")
	wantPeriods(t, "moved from March", periods(t, cc, "category"), "2026-01-01..2026-03-01 ADMIN", "2026-03-01..2026-04-01 SALES", "2026-04-01..<nil> SALES")
	ch, _ = c.Must(200, "POST", path+":move", `{"valid_from": "2026-03-01", "profit_center_id": null, "reason": "None"}`, tag)
	c.decide(t, ch["id"], "approve")
	cc, tag = c.Must(200, "GET", path, "", "")
	wantPeriods(t, "no profit center", periods(t, cc, "profit_center_id"), "2026-01-01..2026-03-01 "+salesID, "2026-03-01..2026-04-01 <nil>", "2026-04-01..<nil> <nil>")
	ch, _ = c.Must(200, "POST", path+":redate", `{"valid_from": "2026-02-01", "valid_to": "2026-12-01", "reason": "Office dates"}`, tag)
	c.decide(t, ch["id"], "approve")
	cc, tag = c.Must(200, "GET", path, "", "")
	wantPeriods(t, "redated", periods(t, cc, "category"), "2026-02-01..2026-03-01 ADMIN", "2026-03-01..2026-04-01 SALES", "2026-04-01..2026-12-01 SALES")
	ch, _ = c.Must(200, "POST", path+":redate", `{"valid_to": null, "reason": "Stays"}`, tag)
	c.decide(t, ch["id"], "approve")
	cc, tag = c.Must(200, "GET", path, "", "")
	if _, ok := cc["valid_to"]; ok || cc["valid_from"] != "2026-02-01" {
		t.Errorf("open again: got %v..%v, want 2026-02-01 open-ended", cc["valid_from"], cc["valid_to"])
	}
	cc, _ = c.Must(200, "PATCH", path, `{"description": "Head office"}`, tag)
	if cc["description"] != "Head office" {
		t.Errorf("description: got %v, want Head office", cc["description"])
	}
	got := apitest.Published(t, c.ten, outbox.Event{Context: "ledger", Name: "cost_center_changed"})
	var changes []any
	for _, e := range got {
		changes = append(changes, e["change"])
	}
	if want := "[created locked moved moved redated redated texts]"; fmt.Sprint(changes) != want {
		t.Errorf("cost_center_changed: got %v, want %s", changes, want)
	}
	if _, ok := got[0]["responsible"]; ok {
		t.Errorf("cost_center_changed carries the person responsible: %v", got[0])
	}
	// Each request and each decision is in the audit trail; a change of
	// texts names the texts, not their values.
	var actions []string
	pgtest.Scan(t, `SELECT array_agg(action || coalesce(' ' || (detail -> 'fields')::text, '') ORDER BY seq) FROM audit.audit_event
		WHERE tenant_id = $1 AND object_id = $2`, []any{c.ten.ID, cc["id"]}, &actions)
	want := `[ledger.cost_center.request ledger.cost_center.create ledger.cost_center.request ledger.cost_center.lock ledger.cost_center.request ` +
		`ledger.cost_center.move ledger.cost_center.request ledger.cost_center.move ledger.cost_center.request ledger.cost_center.redate ` +
		`ledger.cost_center.request ledger.cost_center.redate ledger.cost_center.texts ["description"]]`
	if fmt.Sprint(actions) != want {
		t.Errorf("audit trail: got %v, want %s", actions, want)
	}
}

// TestCostCenterRefusals: what a cost center change may not ask for.
func TestCostCenterRefusals(t *testing.T) {
	c := setupCenters(t)
	us := c.profitCenter(t, "US", "", c.us)
	short := c.profitCenter(t, "SHORT", "2026-06-01", c.de)
	pending, _ := c.Must(201, "POST", "/v1/profit-centers", c.pcBody(fmt.Sprintf(`{"code": "PEND", "name": "P", "responsible": "J", "legal_entity_ids": [%q],
		"valid_from": "2026-01-01", "reason": "r"}`, c.de)), "")
	for name, tc := range map[string]struct{ pc, reason string }{
		"unknown":             {uuid.NewString(), "unknown"},
		"pending":             {pending["id"].(string), "not_active"},
		"another company's":   {us[len("/v1/profit-centers/"):], "not_in_company"},
		"valid for too short": {short[len("/v1/profit-centers/"):], "not_valid_on_date"},
	} {
		t.Run(name, func(t *testing.T) {
			c := c.in(t)
			p := c.Problem(422, "ledger.profit_center_invalid", "POST", "/v1/cost-centers", c.ccBody(fmt.Sprintf(`{"legal_entity_id": %q, "code": "X1", "name": "X",
				"responsible": "J", "category": "ADMIN", "profit_center_id": %q, "valid_from": "2026-01-01", "reason": "r"}`, c.de, tc.pc)), "")
			if errs, _ := p["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["code"] != tc.reason {
				t.Errorf("errors: got %v, want %s", p["errors"], tc.reason)
			}
		})
	}
	c.Problem(422, "ledger.invalid_validity", "POST", "/v1/cost-centers", c.ccBody(fmt.Sprintf(`{"legal_entity_id": %q, "code": "X2", "name": "X",
		"responsible": "J", "category": "ADMIN", "valid_from": "2026-05-01", "valid_to": "2026-05-01", "reason": "r"}`, c.de)), "")
	c.Problem(422, "request.reference_not_found", "POST", "/v1/cost-centers", c.ccBody(fmt.Sprintf(`{"legal_entity_id": %q, "code": "X3", "name": "X",
		"responsible": "J", "category": "ADMIN", "valid_from": "2026-05-01", "reason": "r"}`, uuid.New())), "")
	cc := c.costCenter(t, "2000", `, "valid_to": "2027-01-01"`)
	c.decide(t, cc["pending_change_id"], "approve")
	path := "/v1/cost-centers/" + cc["id"].(string)
	_, tag := c.Must(200, "GET", path, "", "")
	for name, tc := range map[string]struct{ op, body, code string }{
		"lock outside the validity": {":lock", `{"valid_from": "2025-12-01", "lock_plan": true, "reason": "r"}`, "ledger.invalid_validity"},
		"lock ending before it starts": {":lock", `{"valid_from": "2026-05-01", "valid_to": "2026-04-01", "lock_plan": true, "reason": "r"}`,
			"ledger.invalid_validity"},
		"lock without a flag":            {":lock", `{"valid_from": "2026-05-01", "reason": "r"}`, "ledger.nothing_to_change"},
		"move outside the validity":      {":move", `{"valid_from": "2027-01-01", "category": "SALES", "reason": "r"}`, "ledger.invalid_validity"},
		"move to the same":               {":move", `{"valid_from": "2026-05-01", "category": "ADMIN", "reason": "r"}`, "ledger.nothing_to_change"},
		"redate ending before it starts": {":redate", `{"valid_from": "2027-02-01", "reason": "r"}`, "ledger.invalid_validity"},
		"redate past the validity":       {":redate", `{"valid_from": "2027-01-01", "valid_to": "2027-03-01", "reason": "r"}`, "ledger.invalid_validity"},
		"redate to the same":             {":redate", `{"valid_from": "2026-01-01", "reason": "r"}`, "ledger.nothing_to_change"},
		"move to another company's": {":move", fmt.Sprintf(`{"valid_from": "2026-05-01", "profit_center_id": %q, "reason": "r"}`, us[len("/v1/profit-centers/"):]),
			"ledger.profit_center_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			c.in(t).Problem(422, tc.code, "POST", path+tc.op, tc.body, tag)
		})
	}
	c.Problem(412, "request.version_mismatch", "POST", path+":lock", `{"valid_from": "2026-05-01", "lock_plan": true, "reason": "r"}`, `"99"`)
	c.Problem(404, "request.not_found", "POST", "/v1/cost-centers/"+uuid.NewString()+":lock", `{"valid_from": "2026-05-01", "lock_plan": true, "reason": "r"}`, tag)
	// Locking is a person's decision; an agent may propose the rest.
	c.ten.AllCompanies(t, c.ten.Agent)
	agent := c.Client
	agent.As = c.ten.Agent
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+":lock", `{"valid_from": "2026-05-01", "lock_plan": true, "reason": "r"}`, tag)
	ch, _ := agent.Must(200, "POST", path+":move", `{"valid_from": "2026-05-01", "category": "SALES", "reason": "Agent proposal"}`, tag)
	if ch["requested_by"] != c.ten.Agent.ID.String() {
		t.Errorf("requested_by: got %v, want the agent", ch["requested_by"])
	}
	c.decide(t, ch["id"], "approve")
}

// TestProfitCenterInUse: a profit center's validity keeps covering the
// cost center periods that name it.
func TestProfitCenterInUse(t *testing.T) {
	c := setupCenters(t)
	path := c.profitCenter(t, "SALES", "", c.de)
	cc := c.costCenter(t, "1000", fmt.Sprintf(`, "profit_center_id": %q, "valid_to": "2026-07-01"`, path[len("/v1/profit-centers/"):]))
	c.decide(t, cc["pending_change_id"], "approve")
	_, tag := c.Must(200, "GET", path, "", "")
	c.Problem(409, "ledger.profit_center_in_use", "POST", path+":redate", `{"valid_to": "2026-06-01", "reason": "r"}`, tag)
	c.Problem(409, "ledger.profit_center_in_use", "POST", path+":redate", `{"valid_from": "2026-02-01", "reason": "r"}`, tag)
	ch, _ := c.Must(200, "POST", path+":redate", `{"valid_to": "2026-07-01", "reason": "Fits"}`, tag)
	c.decide(t, ch["id"], "approve")
	_, tag = c.Must(200, "GET", path, "", "")
	c.Problem(422, "ledger.invalid_validity", "POST", path+":redate", `{"valid_from": "2026-08-01", "valid_to": "2026-09-01", "reason": "r"}`, tag)
}

// TestCenterAccess: cost centers and their changes are seen in the
// companies one may act in, as the rest of the ledger.
func TestCenterAccess(t *testing.T) {
	c := setupCenters(t)
	cc := c.costCenter(t, "1000", "")
	other := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	other.Subject = "human|" + other.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'us only', 'active', $2, $2)`, c.ten.ID, other.ID, pgtest.Issuer(t), other.Subject)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, id, current_date - 1, $2 FROM identity.role WHERE tenant_id = $1 AND code = 'all'`, c.ten.ID, other.ID)
	pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`, c.ten.ID, other.ID, c.us)
	us := c.Client
	us.As = other
	us.Problem(404, "request.not_found", "GET", "/v1/cost-centers/"+cc["id"].(string), "", "")
	us.Problem(404, "request.not_found", "GET", fmt.Sprintf("/v1/center-changes/%v", cc["pending_change_id"]), "", "")
	// Changes of hierarchies' nodes name no company, so are listed to all.
	for _, list := range []string{"/v1/cost-centers", "/v1/center-changes?status=pending"} {
		page, _ := us.Must(200, "GET", list, "", "")
		if items := page["items"].([]any); len(items) != 0 {
			t.Errorf("%s outside access: got %v, want none", list, items)
		}
	}
	us.Problem(422, "request.reference_not_found", "POST", "/v1/profit-centers", c.pcBody(fmt.Sprintf(`{"code": "PC", "name": "P", "responsible": "J",
		"legal_entity_ids": [%q], "valid_from": "2026-01-01", "reason": "r"}`, c.de)), "")
	page, _ := c.Must(200, "GET", fmt.Sprintf("/v1/cost-centers?legal_entity_id=%s&code=1000&status=pending", c.de), "", "")
	if items := page["items"].([]any); len(items) != 1 {
		t.Errorf("own cost centers: got %v, want one", items)
	}
	page, _ = c.Must(200, "GET", fmt.Sprintf("/v1/center-changes?cost_center_id=%v&status=pending", cc["id"]), "", "")
	if items := page["items"].([]any); len(items) != 1 {
		t.Errorf("own center changes: got %v, want one", items)
	}
	// A tree shows the cost centers of one's companies only (T56b).
	c.decide(t, cc["pending_change_id"], "approve")
	leafCenters := func(cl apitest.Client) any {
		tr, _ := cl.Must(200, "GET", "/v1/hierarchies/"+c.ccTree.String(), "", "")
		return tr["nodes"].([]any)[1].(map[string]any)["center_ids"]
	}
	if got := fmt.Sprint(leafCenters(c.Client)); got != fmt.Sprintf("[%v]", cc["id"]) {
		t.Errorf("the leaf in one's company: got %v, want the cost center", got)
	}
	if got := fmt.Sprint(leafCenters(us)); got != "[]" {
		t.Errorf("the leaf outside access: got %v, want no centers", got)
	}
	us.Problem(404, "request.not_found", "GET", fmt.Sprintf("/v1/cost-centers/%v/hierarchy-paths", cc["id"]), "", "")
}

// TestCenterRequestChecks: the listings' paging and order, and the checks
// of requests the lifecycle tests do not reach.
func TestCenterRequestChecks(t *testing.T) {
	c := setupCenters(t)
	pc := c.profitCenter(t, "PC1", "2027-01-01", c.de)
	c.profitCenter(t, "PC2", "", c.de)
	for _, code := range []string{"1000", "2000"} {
		c.costCenter(t, code, "")
	}
	for _, list := range []string{"/v1/cost-centers", "/v1/profit-centers", "/v1/segments", "/v1/center-changes"} {
		t.Run(list, func(t *testing.T) {
			c := c.in(t)
			c.Problem(400, "request.invalid_order_by", "GET", list+"?order_by=bogus", "", "")
		})
	}
	for _, list := range []string{"/v1/cost-centers", "/v1/profit-centers", "/v1/center-changes"} {
		page, _ := c.Must(200, "GET", list+"?page_size=1", "", "")
		if page["next_page_token"] == nil {
			t.Errorf("%s: first of two pages has no next page token", list)
		}
		page, _ = c.Must(200, "GET", list+"?page_size=1&page_token="+page["next_page_token"].(string), "", "")
		if items := page["items"].([]any); len(items) != 1 {
			t.Errorf("%s: second page got %v, want one", list, items)
		}
	}
	page, _ := c.Must(200, "GET", fmt.Sprintf("/v1/profit-centers?legal_entity_id=%s&code=PC1&status=active", c.de), "", "")
	if items := page["items"].([]any); len(items) != 1 {
		t.Errorf("profit centers by company and code: got %v, want one", items)
	}
	c.Problem(422, "ledger.invalid_validity", "POST", "/v1/profit-centers", c.pcBody(fmt.Sprintf(`{"code": "PC3", "name": "P", "responsible": "J",
		"legal_entity_ids": [%q], "valid_from": "2026-05-01", "valid_to": "2026-04-01", "reason": "r"}`, c.de)), "")
	_, tag := c.Must(200, "GET", pc, "", "")
	for name, tc := range map[string]struct{ op, body string }{
		"lock ending before it starts":   {":lock", `{"valid_from": "2026-05-01", "valid_to": "2026-04-01", "locked": true, "reason": "r"}`},
		"lock after the validity":        {":lock", `{"valid_from": "2027-02-01", "valid_to": "2027-03-01", "locked": true, "reason": "r"}`},
		"redate ending before it starts": {":redate", `{"valid_from": "2027-02-01", "reason": "r"}`},
	} {
		t.Run(name, func(t *testing.T) {
			c.in(t).Problem(422, "ledger.invalid_validity", "POST", pc+tc.op, tc.body, tag)
		})
	}
	c.Problem(412, "request.version_mismatch", "PATCH", pc, `{"name": "X"}`, `"99"`)
	c.Problem(422, "ledger.nothing_to_change", "PATCH", pc, `{}`, tag)
	// An agent never decides a center change.
	c.ten.AllCompanies(t, c.ten.Agent)
	agent := c.Client
	agent.As = c.ten.Agent
	cc := c.costCenter(t, "3000", "")
	page, _ = c.ctl.Must(200, "GET", fmt.Sprintf("/v1/approval-requests?status=pending&object_id=%v", cc["pending_change_id"]), "", "")
	r := page["items"].([]any)[0].(map[string]any)
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`, fmt.Sprintf(`"%v"`, r["version"]))
}
