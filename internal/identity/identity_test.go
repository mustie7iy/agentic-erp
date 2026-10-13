package identity_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"identity.access.read", "identity.access.manage"}

func admin(t *testing.T, extra ...string) (apitest.Client, pgtest.Tenant) {
	t.Helper()
	ten := pgtest.NewTenant(t, append(perms, extra...)...)
	return apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: ten.Human}, ten
}

func id(m map[string]any) string { return m["id"].(string) }

func ids(page map[string]any) []string {
	var out []string
	for _, it := range page["items"].([]any) {
		out = append(out, it.(map[string]any)["id"].(string))
	}
	return out
}

func events(t *testing.T, ten pgtest.Tenant, name string) []map[string]any {
	t.Helper()
	return apitest.Published(t, ten, outbox.Event{Context: "identity", Name: name})
}

func day(offset int) string { return time.Now().UTC().AddDate(0, 0, offset).Format(time.DateOnly) }

func TestPrincipals(t *testing.T) {
	c, ten := admin(t)
	human, tag := c.Must(201, "POST", "/v1/principals", `{"kind": "human", "external_subject": "auth0|jonas", "display_name": "Jonas Weber",
		"email": "jonas@example.com"}`, "")
	if human["status"] != "active" {
		t.Errorf("human status: got %v, want active", human["status"])
	}
	if human["all_companies"] != false {
		t.Errorf("human all companies: got %v, want false", human["all_companies"])
	}
	if human["agent_profile"] != nil {
		t.Errorf("human agent profile: got %v, want none", human["agent_profile"])
	}
	if tag != `"1"` {
		t.Errorf("human ETag: got %s, want \"1\"", tag)
	}
	agent, _ := c.Must(201, "POST", "/v1/principals", `{"kind": "agent", "display_name": "AP agent", "all_companies": true,
		"agent_profile": {"autonomy_level": 1, "model": "claude-opus-5-5", "description": "Invoice exceptions",
		"monthly_budget": {"amount": "5000.00", "currency": "EUR"}}}`, "")
	profile := agent["agent_profile"].(map[string]any)
	if agent["all_companies"] != true {
		t.Errorf("agent all companies: got %v, want true", agent["all_companies"])
	}
	if agent["external_subject"] != nil {
		t.Errorf("agent external subject: got %v, want none", agent["external_subject"])
	}
	if got := profile["monthly_budget"].(map[string]any)["amount"]; got != "5000.00" {
		t.Errorf("agent monthly budget: got %v, want 5000.00", got)
	}
	bridge, _ := c.Must(201, "POST", "/v1/principals", `{"kind": "service", "external_subject": "client|sap-bridge", "display_name": "SAP bridge"}`, "")

	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"unknown kind":     {`{"kind": "robot", "display_name": "R", "external_subject": "s1"}`, 422, "identity.unknown_principal_kind"},
		"no subject":       {`{"kind": "human", "display_name": "H"}`, 422, "identity.subject_required"},
		"agent no profile": {`{"kind": "agent", "display_name": "A"}`, 422, "identity.agent_profile"},
		"human profile":    {`{"kind": "human", "display_name": "H", "external_subject": "s2", "agent_profile": {"autonomy_level": 0, "model": "m", "description": "d"}}`, 422, "identity.agent_profile"},
		"subject taken":    {`{"kind": "human", "display_name": "H", "external_subject": "auth0|jonas"}`, 409, "identity.subject_taken"},
		"bad budget":       {`{"kind": "agent", "display_name": "A", "agent_profile": {"autonomy_level": 0, "model": "m", "description": "d", "monthly_budget": {"amount": "5.001", "currency": "EUR"}}}`, 422, "request.invalid_amount"},
		"unknown currency": {`{"kind": "agent", "display_name": "A", "agent_profile": {"autonomy_level": 0, "model": "m", "description": "d", "monthly_budget": {"amount": "5.00", "currency": "XXQ"}}}`, 422, "identity.unknown_currency"},
	} {
		t.Run(name, func(t *testing.T) {
			api := c
			api.T = t
			code, m, _ := api.Do("POST", "/v1/principals", tc.body, "")
			if code != tc.status {
				t.Errorf("status: got %d, want %d: %v", code, tc.status, m)
			}
			if m["code"] != tc.code {
				t.Errorf("problem code: got %v, want %s", m["code"], tc.code)
			}
		})
	}

	got, tag := c.Must(200, "GET", "/v1/principals/"+id(human), "", "")
	if got["email"] != "jonas@example.com" {
		t.Errorf("email: got %v, want jonas@example.com", got["email"])
	}
	if tag != `"1"` {
		t.Fatalf("ETag: got %s, want \"1\"", tag)
	}
	c.Problem(404, "request.not_found", "GET", "/v1/principals/"+uuid.NewString(), "", "")

	// Merge patch: the e-mail is cleared, company access widened.
	got, tag = c.Must(200, "PATCH", "/v1/principals/"+id(human), `{"display_name": "Jonas M. Weber", "email": null, "all_companies": true}`, `"1"`)
	if got["display_name"] != "Jonas M. Weber" {
		t.Errorf("updated display name: got %v, want Jonas M. Weber", got["display_name"])
	}
	if got["email"] != nil {
		t.Errorf("updated email: got %v, want none", got["email"])
	}
	if got["all_companies"] != true {
		t.Errorf("updated all companies: got %v, want true", got["all_companies"])
	}
	if tag != `"2"` {
		t.Fatalf("updated ETag: got %s, want \"2\"", tag)
	}
	got, _ = c.Must(200, "PATCH", "/v1/principals/"+id(human), `{"email": "jonas.weber@example.com"}`, `"2"`)
	if got["email"] != "jonas.weber@example.com" {
		t.Fatalf("email: %v", got)
	}
	c.Problem(412, "request.version_mismatch", "PATCH", "/v1/principals/"+id(human), `{}`, `"1"`)
	c.Problem(422, "identity.agent_profile", "PATCH", "/v1/principals/"+id(human), `{"agent_profile": {"autonomy_level": 0, "model": "m", "description": "d"}}`, `"3"`)
	c.Problem(404, "request.not_found", "PATCH", "/v1/principals/"+uuid.NewString(), `{}`, `"1"`)
	got, _ = c.Must(200, "PATCH", "/v1/principals/"+id(agent), `{"agent_profile": {"autonomy_level": 2, "model": "claude-opus-5-5", "description": "Invoice exceptions"}}`, `"1"`)
	p := got["agent_profile"].(map[string]any)
	if p["autonomy_level"] != float64(2) {
		t.Errorf("profile autonomy level: got %v, want 2", p["autonomy_level"])
	}
	if p["monthly_budget"] != nil {
		t.Errorf("profile monthly budget: got %v, want none", p["monthly_budget"])
	}
	// The same profile again changes nothing in it.
	c.Must(200, "PATCH", "/v1/principals/"+id(agent), `{"agent_profile": {"autonomy_level": 2, "model": "claude-opus-5-5", "description": "Invoice exceptions"}}`, `"2"`)
	changes := events(t, ten, "agent_profile_changed")
	if len(changes) != 2 {
		t.Fatalf("profile events: got %d, want 2: %v", len(changes), changes)
	}
	if changes[0]["change"] != "created" {
		t.Errorf("first profile event: got %v, want created", changes[0]["change"])
	}
	if changes[1]["change"] != "updated" {
		t.Errorf("second profile event: got %v, want updated", changes[1]["change"])
	}
	if got := fmt.Sprint(changes[1]["changed_fields"]); got != "[autonomy_level monthly_budget]" {
		t.Errorf("second profile event changed fields: got %s, want [autonomy_level monthly_budget]", got)
	}
	if changes[1]["monthly_budget"] != nil {
		t.Errorf("second profile event monthly budget: got %v, want none", changes[1]["monthly_budget"])
	}

	// Blocking: never yourself; a blocked principal's token is refused.
	c.Problem(409, "identity.cannot_block_self", "POST", "/v1/principals/"+ten.Human.ID.String()+":block", `{"reason": "r"}`, `"1"`)
	c.Problem(412, "request.version_mismatch", "POST", "/v1/principals/"+ten.Service.ID.String()+":block", `{"reason": "r"}`, `"9"`)
	got, _ = c.Must(200, "POST", "/v1/principals/"+ten.Service.ID.String()+":block", `{"reason": "Key leaked"}`, `"1"`)
	if got["status"] != "blocked" {
		t.Fatal(got)
	}
	c.Problem(409, "identity.invalid_status", "POST", "/v1/principals/"+ten.Service.ID.String()+":block", `{"reason": "r"}`, `"2"`)
	c.Problem(404, "request.not_found", "POST", "/v1/principals/"+uuid.NewString()+":block", `{"reason": "r"}`, `"1"`)
	service := apitest.Client{T: t, API: c.API, Tenant: ten, As: ten.Service}
	service.Problem(403, "identity.principal_blocked", "GET", "/v1/principals", "", "")

	created := events(t, ten, "principal_created")
	blocked := events(t, ten, "principal_blocked")
	if len(created) != 3 {
		t.Fatalf("created events: got %d, want 3: %v", len(created), created)
	}
	if created[1]["kind"] != "agent" {
		t.Errorf("second created event kind: got %v, want agent", created[1]["kind"])
	}
	if created[1]["all_companies"] != true {
		t.Errorf("second created event all companies: got %v, want true", created[1]["all_companies"])
	}
	if len(blocked) != 1 {
		t.Fatalf("blocked events: got %d, want 1: %v", len(blocked), blocked)
	}
	if blocked[0]["previous_status"] != "active" {
		t.Errorf("blocked event previous status: got %v, want active", blocked[0]["previous_status"])
	}
	if blocked[0]["kind"] != "service" {
		t.Errorf("blocked event kind: got %v, want service", blocked[0]["kind"])
	}

	// Lists: by kind and status, by name (default) or creation time.
	list := func(q string) map[string]any { m, _ := c.Must(200, "GET", "/v1/principals"+q, "", ""); return m }
	if got := ids(list("?kind=agent")); len(got) != 2 { // the fixture's agent and the new one
		t.Fatalf("agents: %v", got)
	}
	if got := ids(list("?status=blocked")); len(got) != 1 || got[0] != ten.Service.ID.String() {
		t.Fatalf("blocked: %v", got)
	}
	first := list("?order_by=created_at%20desc&page_size=2")
	second := list("?order_by=created_at%20desc&page_size=2&page_token=" + first["next_page_token"].(string))
	if got := ids(first); got[0] != id(bridge) || got[1] != id(agent) { // newest first
		t.Fatalf("by creation: %v", got)
	}
	got2 := ids(second)
	if got2[0] != id(human) {
		t.Errorf("by creation, page 2 first: got %s, want %s", got2[0], id(human))
	}
	if len(got2) != 2 {
		t.Errorf("by creation, page 2: got %d principals, want 2", len(got2))
	}
	byName := list("?page_size=1")
	if next := ids(list("?page_size=1&page_token=" + byName["next_page_token"].(string))); next[0] == ids(byName)[0] {
		t.Fatalf("by name: %v then %v", byName, next)
	}
	c.Problem(400, "request.invalid_order_by", "GET", "/v1/principals?order_by=email", "", "")
}

func TestIdentityAdministrationIsHumanOnly(t *testing.T) {
	c, ten := admin(t)
	agent := apitest.Client{T: t, API: c.API, Tenant: ten, As: ten.Agent}
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/roles", `{"code": "X", "name": "X"}`, "")
	// Reading is allowed for agents with the read permission.
	agent.Must(200, "GET", "/v1/roles", "", "")
}

func TestRolesAndPermissions(t *testing.T) {
	c, ten := admin(t)
	role, tag := c.Must(201, "POST", "/v1/roles", `{"code": "AP_CLERK", "name": "AP clerk", "description": "Captures invoices"}`, "")
	if role["status"] != "active" {
		t.Errorf("role status: got %v, want active", role["status"])
	}
	if codes := role["permission_codes"].([]any); len(codes) != 0 {
		t.Errorf("role permission codes: got %v, want none", codes)
	}
	if tag != `"1"` {
		t.Errorf("role ETag: got %s, want \"1\"", tag)
	}
	c.Problem(409, "identity.role_code_taken", "POST", "/v1/roles", `{"code": "AP_CLERK", "name": "Again"}`, "")
	path := "/v1/roles/" + id(role)
	got, _ := c.Must(200, "PATCH", path, `{"name": "Accounts payable clerk", "description": null}`, `"1"`)
	if got["name"] != "Accounts payable clerk" {
		t.Errorf("updated name: got %v, want Accounts payable clerk", got["name"])
	}
	if got["description"] != nil {
		t.Errorf("updated description: got %v, want none", got["description"])
	}
	c.Problem(412, "request.version_mismatch", "PATCH", path, `{}`, `"1"`)
	c.Problem(404, "request.not_found", "PATCH", "/v1/roles/"+uuid.NewString(), `{}`, `"1"`)

	grant, gtag := c.Must(201, "POST", path+"/permissions", `{"permission_code": "payables.invoice.capture"}`, "")
	if grant["valid_from"] != day(0) {
		t.Errorf("grant valid from: got %v, want %s", grant["valid_from"], day(0))
	}
	if grant["valid_to"] != nil {
		t.Errorf("grant valid to: got %v, want none", grant["valid_to"])
	}
	if len(gtag) < 10 {
		t.Fatalf("grant ETag: got %q, want at least 10 characters", gtag)
	}
	future, _ := c.Must(201, "POST", path+"/permissions", fmt.Sprintf(`{"permission_code": "payables.invoice.read", "valid_from": %q, "valid_to": %q}`, day(5), day(30)), "")
	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"unknown permission": {`{"permission_code": "payables.invoice.dance"}`, 422, "identity.unknown_permission"},
		"backdated":          {fmt.Sprintf(`{"permission_code": "payables.invoice.hold", "valid_from": %q}`, day(-1)), 422, "identity.backdated_grant"},
		"ends before start":  {fmt.Sprintf(`{"permission_code": "payables.invoice.hold", "valid_to": %q}`, day(0)), 422, "identity.invalid_validity"},
		"overlaps":           {`{"permission_code": "payables.invoice.capture"}`, 409, "identity.grant_overlaps"},
	} {
		t.Run(name, func(t *testing.T) {
			api := c
			api.T = t
			code, m, _ := api.Do("POST", path+"/permissions", tc.body, "")
			if code != tc.status {
				t.Errorf("status: got %d, want %d: %v", code, tc.status, m)
			}
			if m["code"] != tc.code {
				t.Errorf("problem code: got %v, want %s", m["code"], tc.code)
			}
		})
	}
	c.Problem(404, "request.not_found", "POST", "/v1/roles/"+uuid.NewString()+"/permissions", `{"permission_code": "payables.invoice.read"}`, "")
	got, _ = c.Must(200, "GET", path, "", "")
	if fmt.Sprint(got["permission_codes"]) != "[payables.invoice.capture]" { // the future grant is not in force yet
		t.Fatalf("permission codes: %v", got["permission_codes"])
	}

	gpath := path + "/permissions/" + id(grant)
	_, tag = c.Must(200, "GET", gpath, "", "")
	if tag != gtag {
		t.Fatalf("etag %s, created with %s", tag, gtag)
	}
	c.Problem(404, "request.not_found", "GET", "/v1/roles/"+uuid.NewString()+"/permissions/"+id(grant), "", "")
	end := func(path, to, ifMatch string) (int, map[string]any, string) {
		return c.Do("POST", path+":end", fmt.Sprintf(`{"valid_to": %q}`, to), ifMatch)
	}
	if code, m, _ := end(gpath, day(10), `"stale"`); code != 412 {
		t.Fatalf("stale: %d %v", code, m)
	}
	if code, m, _ := end(gpath, day(-1), gtag); code != 422 || m["errors"].([]any)[0].(map[string]any)["code"] != "in_past" {
		t.Fatalf("past: %d %v", code, m)
	}
	code, ended, etag2 := end(gpath, day(10), gtag)
	if code != 200 {
		t.Fatalf("end status: got %d, want 200: %v", code, ended)
	}
	if ended["valid_to"] != day(10) {
		t.Errorf("ended valid to: got %v, want %s", ended["valid_to"], day(10))
	}
	if ended["ended_by"] != ten.Human.ID.String() {
		t.Errorf("ended by: got %v, want %v", ended["ended_by"], ten.Human.ID)
	}
	if ended["ended_at"] == nil {
		t.Errorf("ended at: got none, want a time")
	}
	if etag2 == gtag {
		t.Fatalf("ended ETag: got %s unchanged, want a new one", etag2)
	}
	if code, m, _ := end(gpath, day(10), etag2); code != 422 || m["errors"].([]any)[0].(map[string]any)["code"] != "not_earlier" {
		t.Fatalf("not earlier: %d %v", code, m)
	}
	fpath := path + "/permissions/" + id(future)
	_, ftag := c.Must(200, "GET", fpath, "", "")
	if code, m, _ := end(fpath, day(5), ftag); code != 422 || m["errors"].([]any)[0].(map[string]any)["code"] != "before_start" {
		t.Fatalf("before start: %d %v", code, m)
	}
	c.Problem(404, "request.not_found", "POST", path+"/permissions/"+uuid.NewString()+":end", fmt.Sprintf(`{"valid_to": %q}`, day(1)), `"x"`)
	// Grants are day-granular: one made today runs at least until tomorrow. A
	// grant from an earlier day can be ended today, which removes it today.
	old := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
		VALUES ($1, $2, $3, 'payables.invoice.hold', current_date - 7, $4)`, ten.ID, old, uuid.MustParse(id(role)), ten.Human.ID)
	if got, _ := c.Must(200, "GET", path, "", ""); fmt.Sprint(got["permission_codes"]) != "[payables.invoice.capture payables.invoice.hold]" {
		t.Fatalf("before: %v", got["permission_codes"])
	}
	_, otag := c.Must(200, "GET", path+"/permissions/"+old.String(), "", "")
	c.Must(200, "POST", path+"/permissions/"+old.String()+":end", fmt.Sprintf(`{"valid_to": %q}`, day(0)), otag)
	if got, _ := c.Must(200, "GET", path, "", ""); fmt.Sprint(got["permission_codes"]) != "[payables.invoice.capture]" {
		t.Fatalf("ended today: %v", got["permission_codes"])
	}

	all, _ := c.Must(200, "GET", path+"/permissions", "", "")
	if len(ids(all)) != 3 {
		t.Fatalf("history: %v", all)
	}
	page, _ := c.Must(200, "GET", path+"/permissions?page_size=1", "", "")
	c.Must(200, "GET", path+"/permissions?page_size=1&page_token="+page["next_page_token"].(string), "", "")
	if on, _ := c.Must(200, "GET", path+"/permissions?valid_on="+day(12), "", ""); fmt.Sprint(ids(on)) != "["+id(future)+"]" {
		t.Fatalf("valid on: %v", on)
	}
	c.Problem(404, "request.not_found", "GET", "/v1/roles/"+uuid.NewString()+"/permissions", "", "")

	granted := events(t, ten, "role_permission_granted")
	endedEvents := events(t, ten, "role_permission_ended")
	roles := events(t, ten, "role_created")
	if len(granted) != 2 {
		t.Fatalf("granted events: got %d, want 2: %v", len(granted), granted)
	}
	if granted[1]["valid_to"] != day(30) {
		t.Errorf("second granted event valid to: got %v, want %s", granted[1]["valid_to"], day(30))
	}
	if len(endedEvents) != 2 {
		t.Fatalf("ended events: got %d, want 2: %v", len(endedEvents), endedEvents)
	}
	if endedEvents[1]["valid_to"] != day(0) {
		t.Errorf("second ended event valid to: got %v, want %s", endedEvents[1]["valid_to"], day(0))
	}
	if endedEvents[1]["valid_from"] != day(-7) {
		t.Errorf("second ended event valid from: got %v, want %s", endedEvents[1]["valid_from"], day(-7))
	}
	if len(roles) != 1 {
		t.Fatalf("role events: got %d, want 1: %v", len(roles), roles)
	}
	if roles[0]["code"] != "AP_CLERK" {
		t.Errorf("role event code: got %v, want AP_CLERK", roles[0]["code"])
	}

	// Lists of roles.
	c.Must(201, "POST", "/v1/roles", `{"code": "AA_FIRST", "name": "Zeta"}`, "")
	byCode, _ := c.Must(200, "GET", "/v1/roles?page_size=1", "", "")
	if byCode["items"].([]any)[0].(map[string]any)["code"] != "AA_FIRST" {
		t.Fatalf("by code: %v", byCode)
	}
	c.Must(200, "GET", "/v1/roles?page_size=1&page_token="+byCode["next_page_token"].(string), "", "")
	byName, _ := c.Must(200, "GET", "/v1/roles?order_by=name&status=active", "", "")
	if byName["items"].([]any)[0].(map[string]any)["name"] != "Accounts payable clerk" {
		t.Fatalf("by name: %v", byName)
	}
	byName, _ = c.Must(200, "GET", "/v1/roles?order_by=name&page_size=1", "", "")
	c.Must(200, "GET", "/v1/roles?order_by=name&page_size=1&page_token="+byName["next_page_token"].(string), "", "")
	c.Problem(400, "request.invalid_order_by", "GET", "/v1/roles?order_by=status", "", "")
	c.Problem(404, "request.not_found", "GET", "/v1/roles/"+uuid.NewString(), "", "")
}

func TestAgentsNeverHoldApprovals(t *testing.T) {
	c, ten := admin(t)
	approver, _ := c.Must(201, "POST", "/v1/roles", `{"code": "APPROVER", "name": "Approver"}`, "")
	c.Must(201, "POST", "/v1/roles/"+id(approver)+"/permissions", `{"permission_code": "payables.invoice.approve"}`, "")
	c.Problem(409, "identity.agent_approval_permission", "POST", "/v1/role-assignments",
		fmt.Sprintf(`{"principal_id": %q, "role_id": %q}`, ten.Agent.ID, id(approver)), "")
	// A role an agent holds cannot gain an approval permission, not even a future one.
	helper, _ := c.Must(201, "POST", "/v1/roles", `{"code": "HELPER", "name": "Helper"}`, "")
	c.Must(201, "POST", "/v1/role-assignments", fmt.Sprintf(`{"principal_id": %q, "role_id": %q}`, ten.Agent.ID, id(helper)), "")
	c.Problem(409, "identity.agent_approval_permission", "POST", "/v1/roles/"+id(helper)+"/permissions",
		fmt.Sprintf(`{"permission_code": "payments.run.release", "valid_from": %q}`, day(3)), "")
	// People may hold them.
	c.Must(201, "POST", "/v1/role-assignments", fmt.Sprintf(`{"principal_id": %q, "role_id": %q}`, ten.Human.ID, id(approver)), "")
}

func TestOutsideAUnitOfWork(t *testing.T) {
	for name, call := range endpointCalls(nil) {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("got nil error, want an error outside a unit of work")
			}
		})
	}
}
