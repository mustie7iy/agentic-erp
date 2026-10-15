package identity_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// role creates a role holding the permissions from today.
func role(t *testing.T, c apitest.Client, code string, permissions ...string) string {
	t.Helper()
	r, _ := c.Must(201, "POST", "/v1/roles", fmt.Sprintf(`{"code": %q, "name": %q}`, code, code), "")
	for _, p := range permissions {
		c.Must(201, "POST", "/v1/roles/"+id(r)+"/permissions", fmt.Sprintf(`{"permission_code": %q}`, p), "")
	}
	return id(r)
}

func assign(c apitest.Client, principal uuid.UUID, role, extra string) (int, map[string]any, string) {
	return c.Do("POST", "/v1/role-assignments", fmt.Sprintf(`{"principal_id": %q, "role_id": %q%s}`, principal, role, extra), "")
}

func TestRoleAssignments(t *testing.T) {
	c, ten := admin(t)
	clerk := role(t, c, "CLERK", "payables.invoice.read")
	code, a, tag := assign(c, ten.Agent.ID, clerk, "")
	if code != 201 {
		t.Fatalf("assign status: got %d, want 201: %v", code, a)
	}
	if a["principal_id"] != ten.Agent.ID.String() {
		t.Errorf("assign principal: got %v, want %v", a["principal_id"], ten.Agent.ID)
	}
	if a["valid_from"] != day(0) {
		t.Errorf("assign valid from: got %v, want %s", a["valid_from"], day(0))
	}
	if tag == "" {
		t.Fatal("assign ETag: got none, want one")
	}
	// The agent now passes authorization for reading invoices.
	agent := apitest.Client{T: t, API: c.API, Tenant: ten, As: ten.Agent}
	agent.Must(200, "GET", "/v1/supplier-invoices", "", "")
	c.Problem(409, "identity.grant_overlaps", "POST", "/v1/role-assignments", fmt.Sprintf(`{"principal_id": %q, "role_id": %q}`, ten.Agent.ID, clerk), "")
	for name, body := range map[string]string{
		"unknown principal": fmt.Sprintf(`{"principal_id": %q, "role_id": %q}`, uuid.New(), clerk),
		"unknown role":      fmt.Sprintf(`{"principal_id": %q, "role_id": %q}`, ten.Agent.ID, uuid.New()),
	} {
		t.Run(name, func(t *testing.T) {
			api := c
			api.T = t
			api.Problem(422, "request.reference_not_found", "POST", "/v1/role-assignments", body, "")
		})
	}
	c.Problem(422, "identity.backdated_grant", "POST", "/v1/role-assignments",
		fmt.Sprintf(`{"principal_id": %q, "role_id": %q, "valid_from": %q}`, ten.Service.ID, clerk, day(-3)), "")
	retired := role(t, c, "RETIRED")
	pgtest.Exec(t, `UPDATE identity.role SET status = 'inactive', version = version + 1 WHERE id = $1`, uuid.MustParse(retired))
	c.Problem(409, "identity.role_inactive", "POST", "/v1/role-assignments", fmt.Sprintf(`{"principal_id": %q, "role_id": %q}`, ten.Agent.ID, retired), "")

	path := "/v1/role-assignments/" + id(a)
	if _, got := c.Must(200, "GET", path, "", ""); got != tag {
		t.Fatalf("etag %s, created with %s", got, tag)
	}
	c.Problem(404, "request.not_found", "GET", "/v1/role-assignments/"+uuid.NewString(), "", "")
	c.Problem(412, "request.version_mismatch", "POST", path+":end", fmt.Sprintf(`{"valid_to": %q}`, day(0)), `"x"`)
	c.Problem(422, "identity.invalid_end", "POST", path+":end", fmt.Sprintf(`{"valid_to": %q}`, day(-1)), tag)
	c.Problem(404, "request.not_found", "POST", "/v1/role-assignments/"+uuid.NewString()+":end", fmt.Sprintf(`{"valid_to": %q}`, day(0)), tag)
	c.Problem(422, "identity.invalid_end", "POST", path+":end", fmt.Sprintf(`{"valid_to": %q}`, day(0)), tag) // made today: runs until tomorrow at least
	ended, _ := c.Must(200, "POST", path+":end", fmt.Sprintf(`{"valid_to": %q}`, day(1)), tag)
	if ended["valid_to"] != day(1) {
		t.Fatal(ended)
	}
	// An assignment from an earlier day ended today is gone today.
	old := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1, $2, $3, $4, current_date - 3, $5)`,
		ten.ID, old, ten.Service.ID, uuid.MustParse(clerk), ten.Human.ID)
	service := apitest.Client{T: t, API: c.API, Tenant: ten, As: ten.Service}
	service.Must(200, "GET", "/v1/supplier-invoices", "", "")
	_, otag := c.Must(200, "GET", "/v1/role-assignments/"+old.String(), "", "")
	c.Must(200, "POST", "/v1/role-assignments/"+old.String()+":end", fmt.Sprintf(`{"valid_to": %q}`, day(0)), otag)
	service.Problem(403, "identity.permission_missing", "GET", "/v1/supplier-invoices", "", "")

	future := role(t, c, "FUTURE")
	_, later, _ := assign(c, ten.Service.ID, future, fmt.Sprintf(`, "valid_from": %q, "valid_to": %q`, day(2), day(9)))
	list := func(q string) []string { m, _ := c.Must(200, "GET", "/v1/role-assignments"+q, "", ""); return ids(m) }
	if got := list("?principal_id=" + ten.Agent.ID.String() + "&role_id=" + clerk); len(got) != 1 || got[0] != id(a) {
		t.Fatalf("by principal: %v", got)
	}
	if got := list("?role_id=" + future); len(got) != 1 || got[0] != id(later) {
		t.Fatalf("by role: %v", got)
	}
	if got := list("?role_id=" + future + "&valid_on=" + day(1)); len(got) != 0 {
		t.Fatalf("not yet valid: %v", got)
	}
	if got := list("?role_id=" + future + "&valid_on=" + day(3)); len(got) != 1 || got[0] != id(later) {
		t.Fatalf("valid on: %v", got)
	}
	page, _ := c.Must(200, "GET", "/v1/role-assignments?page_size=1", "", "")
	c.Must(200, "GET", "/v1/role-assignments?page_size=1&page_token="+page["next_page_token"].(string), "", "")

	assigned := events(t, ten, "role_assigned")
	revoked := events(t, ten, "access_revoked")
	if len(assigned) != 2 {
		t.Fatalf("assigned events: got %d, want 2: %v", len(assigned), assigned)
	}
	if assigned[0]["principal_kind"] != "agent" {
		t.Errorf("first assigned event principal kind: got %v, want agent", assigned[0]["principal_kind"])
	}
	if assigned[0]["role_code"] != "CLERK" {
		t.Errorf("first assigned event role: got %v, want CLERK", assigned[0]["role_code"])
	}
	if assigned[1]["valid_to"] != day(9) {
		t.Errorf("second assigned event valid to: got %v, want %s", assigned[1]["valid_to"], day(9))
	}
	if len(revoked) != 2 {
		t.Fatalf("revoked events: got %d, want 2: %v", len(revoked), revoked)
	}
	if revoked[0]["assignment_kind"] != "role_assignment" {
		t.Errorf("first revoked event kind: got %v, want role_assignment", revoked[0]["assignment_kind"])
	}
	if revoked[0]["role_id"] != clerk {
		t.Errorf("first revoked event role: got %v, want %s", revoked[0]["role_id"], clerk)
	}
	if revoked[0]["legal_entity_id"] != nil {
		t.Errorf("first revoked event legal entity: got %v, want none", revoked[0]["legal_entity_id"])
	}
	if revoked[1]["valid_from"] != day(-3) {
		t.Errorf("second revoked event valid from: got %v, want %s", revoked[1]["valid_from"], day(-3))
	}
}

func TestCompanyAccessGrants(t *testing.T) {
	c, ten := admin(t)
	le := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, 'DE01', 'Muster GmbH', 'DE', 'EUR', gen_random_uuid(), 'active', $3, $3)`, ten.ID, le, ten.Human.ID)
	g, tag := c.Must(201, "POST", "/v1/company-access", fmt.Sprintf(`{"principal_id": %q, "legal_entity_id": %q}`, ten.Agent.ID, le), "")
	if g["legal_entity_id"] != le.String() {
		t.Errorf("grant legal entity: got %v, want %v", g["legal_entity_id"], le)
	}
	if g["valid_to"] != nil {
		t.Errorf("grant valid to: got %v, want none", g["valid_to"])
	}
	for name, body := range map[string]string{
		"unknown principal":    fmt.Sprintf(`{"principal_id": %q, "legal_entity_id": %q}`, uuid.New(), le),
		"unknown legal entity": fmt.Sprintf(`{"principal_id": %q, "legal_entity_id": %q}`, ten.Agent.ID, uuid.New()),
	} {
		t.Run(name, func(t *testing.T) {
			api := c
			api.T = t
			api.Problem(422, "request.reference_not_found", "POST", "/v1/company-access", body, "")
		})
	}
	c.Problem(409, "identity.grant_overlaps", "POST", "/v1/company-access", fmt.Sprintf(`{"principal_id": %q, "legal_entity_id": %q}`, ten.Agent.ID, le), "")
	c.Problem(422, "identity.invalid_validity", "POST", "/v1/company-access",
		fmt.Sprintf(`{"principal_id": %q, "legal_entity_id": %q, "valid_from": %q, "valid_to": %q}`, ten.Service.ID, le, day(4), day(4)), "")
	c.Must(201, "POST", "/v1/company-access", fmt.Sprintf(`{"principal_id": %q, "legal_entity_id": %q, "valid_from": %q}`, ten.Service.ID, le, day(1)), "")

	path := "/v1/company-access/" + id(g)
	if _, got := c.Must(200, "GET", path, "", ""); got != tag {
		t.Fatal(got, tag)
	}
	c.Problem(404, "request.not_found", "GET", "/v1/company-access/"+uuid.NewString(), "", "")
	c.Problem(412, "request.version_mismatch", "POST", path+":end", fmt.Sprintf(`{"valid_to": %q}`, day(1)), `"x"`)
	c.Problem(422, "identity.invalid_end", "POST", path+":end", fmt.Sprintf(`{"valid_to": %q}`, day(-1)), tag)
	c.Problem(404, "request.not_found", "POST", "/v1/company-access/"+uuid.NewString()+":end", fmt.Sprintf(`{"valid_to": %q}`, day(1)), tag)
	c.Must(200, "POST", path+":end", fmt.Sprintf(`{"valid_to": %q}`, day(1)), tag)

	list := func(q string) []string { m, _ := c.Must(200, "GET", "/v1/company-access"+q, "", ""); return ids(m) }
	if got := list("?principal_id=" + ten.Agent.ID.String() + "&legal_entity_id=" + le.String()); len(got) != 1 {
		t.Fatalf("by principal: %v", got)
	}
	if got := list("?valid_on=" + day(2)); len(got) != 1 || got[0] == id(g) {
		t.Fatalf("valid on: %v", got)
	}
	page, _ := c.Must(200, "GET", "/v1/company-access?page_size=1", "", "")
	c.Must(200, "GET", "/v1/company-access?page_size=1&page_token="+page["next_page_token"].(string), "", "")

	granted := events(t, ten, "company_access_granted")
	revoked := events(t, ten, "access_revoked")
	if len(granted) != 2 {
		t.Fatalf("granted events: got %d, want 2: %v", len(granted), granted)
	}
	if granted[1]["valid_from"] != day(1) {
		t.Errorf("second granted event valid from: got %v, want %s", granted[1]["valid_from"], day(1))
	}
	if len(revoked) != 1 {
		t.Fatalf("revoked events: got %d, want 1: %v", len(revoked), revoked)
	}
	if revoked[0]["assignment_kind"] != "company_access" {
		t.Errorf("revoked event kind: got %v, want company_access", revoked[0]["assignment_kind"])
	}
	if revoked[0]["legal_entity_id"] != le.String() {
		t.Errorf("revoked event legal entity: got %v, want %v", revoked[0]["legal_entity_id"], le)
	}
	if revoked[0]["role_id"] != nil {
		t.Errorf("revoked event role: got %v, want none", revoked[0]["role_id"])
	}
}

func TestSegregationOfDuties(t *testing.T) {
	c, ten := admin(t)
	capture := role(t, c, "CAPTURE", "payables.invoice.capture")
	post := role(t, c, "POST", "payables.invoice.post")
	rule, tag := c.Must(201, "POST", "/v1/sod-rules", `{"permission_a": "payables.invoice.post", "permission_b": "payables.invoice.capture",
		"description": "Who captures an invoice does not post it"}`, "")
	// The permissions are stored in canonical order.
	if rule["permission_a"] != "payables.invoice.capture" {
		t.Errorf("rule permission a: got %v, want payables.invoice.capture", rule["permission_a"])
	}
	if rule["permission_b"] != "payables.invoice.post" {
		t.Errorf("rule permission b: got %v, want payables.invoice.post", rule["permission_b"])
	}
	for name, tc := range map[string]struct {
		body   string
		status int
		code   string
	}{
		"same permission":    {`{"permission_a": "payables.invoice.post", "permission_b": "payables.invoice.post", "description": "d"}`, 422, "identity.invalid_sod_rule"},
		"unknown permission": {`{"permission_a": "payables.invoice.post", "permission_b": "payables.invoice.dance", "description": "d"}`, 422, "identity.unknown_permission"},
		"overlapping rule":   {`{"permission_a": "payables.invoice.capture", "permission_b": "payables.invoice.post", "description": "d"}`, 409, "identity.grant_overlaps"},
		"backdated":          {fmt.Sprintf(`{"permission_a": "payables.invoice.read", "permission_b": "payables.invoice.post", "description": "d", "valid_from": %q}`, day(-1)), 422, "identity.backdated_grant"},
	} {
		t.Run(name, func(t *testing.T) {
			api := c
			api.T = t
			code, m, _ := api.Do("POST", "/v1/sod-rules", tc.body, "")
			if code != tc.status {
				t.Errorf("status: got %d, want %d: %v", code, tc.status, m)
			}
			if m["code"] != tc.code {
				t.Errorf("problem code: got %v, want %s", m["code"], tc.code)
			}
		})
	}

	// Assignments that would combine the two permissions are refused, even when one starts later.
	if code, m, _ := assign(c, ten.Agent.ID, capture, ""); code != 201 {
		t.Fatalf("capture: %d %v", code, m)
	}
	if code, m, _ := assign(c, ten.Agent.ID, post, fmt.Sprintf(`, "valid_from": %q`, day(30))); code != 409 || m["code"] != "identity.sod_conflict" {
		t.Fatalf("conflicting assignment: %d %v", code, m)
	}
	// ... and so are permissions added to a role that would combine them.
	c.Problem(409, "identity.sod_conflict", "POST", "/v1/roles/"+capture+"/permissions", `{"permission_code": "payables.invoice.post"}`, "")
	// Periods that do not overlap are fine.
	c.Must(201, "POST", "/v1/role-assignments", fmt.Sprintf(`{"principal_id": %q, "role_id": %q, "valid_to": %q}`, ten.Service.ID, capture, day(5)), "")
	c.Must(201, "POST", "/v1/role-assignments", fmt.Sprintf(`{"principal_id": %q, "role_id": %q, "valid_from": %q}`, ten.Service.ID, post, day(5)), "")

	// A rule is refused while someone would break it.
	c.Must(201, "POST", "/v1/role-assignments", fmt.Sprintf(`{"principal_id": %q, "role_id": %q}`, ten.Service.ID, role(t, c, "READ", "payables.invoice.read")), "")
	c.Problem(409, "identity.sod_conflict", "POST", "/v1/sod-rules", `{"permission_a": "payables.invoice.read", "permission_b": "payables.invoice.post",
		"description": "d", "valid_from": "`+day(6)+`"}`, "")

	// Conflicts that exist anyway (made outside the API) block both permissions at runtime.
	agent := apitest.Client{T: t, API: c.API, Tenant: ten, As: ten.Agent}
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, $3, current_date, $4)`,
		ten.ID, ten.Agent.ID, uuid.MustParse(post), ten.Human.ID)
	postInvoice := "/v1/supplier-invoices/" + uuid.NewString() + ":post" // needs payables.invoice.post
	agent.Problem(403, "identity.sod_conflict", "POST", postInvoice, "", "*")

	path := "/v1/sod-rules/" + id(rule)
	if _, got := c.Must(200, "GET", path, "", ""); got != tag {
		t.Fatal(got, tag)
	}
	c.Problem(404, "request.not_found", "GET", "/v1/sod-rules/"+uuid.NewString(), "", "")
	c.Problem(412, "request.version_mismatch", "POST", path+":end", fmt.Sprintf(`{"valid_to": %q}`, day(0)), `"x"`)
	c.Problem(422, "identity.invalid_end", "POST", path+":end", fmt.Sprintf(`{"valid_to": %q}`, day(-1)), tag)
	c.Problem(404, "request.not_found", "POST", "/v1/sod-rules/"+uuid.NewString()+":end", fmt.Sprintf(`{"valid_to": %q}`, day(1)), tag)
	c.Must(200, "POST", path+":end", fmt.Sprintf(`{"valid_to": %q}`, day(1)), tag)
	agent.Problem(403, "identity.sod_conflict", "POST", postInvoice, "", "*") // the rule is still in force today
	// A rule that is no longer in force blocks nothing.
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, valid_to, created_by)
		VALUES ($1, gen_random_uuid(), 'payables.invoice.hold', 'payables.invoice.post', 'old', current_date - 9, current_date - 2, $2)`, ten.ID, ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, 'payables.invoice.hold', current_date, $3)`,
		ten.ID, uuid.MustParse(post), ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, $3, current_date, $2)`,
		ten.ID, ten.Human.ID, uuid.MustParse(post))
	c.Problem(404, "request.not_found", "POST", postInvoice, "", "*") // authorized; there is no such invoice

	c.Must(201, "POST", "/v1/sod-rules", fmt.Sprintf(`{"permission_a": "partner.bank.change", "permission_b": "payments.run.propose", "description": "d", "valid_from": %q}`, day(3)), "")
	list := func(q string) []string { m, _ := c.Must(200, "GET", "/v1/sod-rules"+q, "", ""); return ids(m) }
	if got := list(""); len(got) != 3 || got[0] != id(rule) {
		t.Fatalf("all: %v", got)
	}
	if got := list("?valid_on=" + day(1)); len(got) != 0 {
		t.Fatalf("valid today: %v", got)
	}
	page, _ := c.Must(200, "GET", "/v1/sod-rules?page_size=1", "", "")
	c.Must(200, "GET", "/v1/sod-rules?page_size=1&page_token="+page["next_page_token"].(string), "", "")

	changes := events(t, ten, "sod_rule_changed")
	if len(changes) != 3 {
		t.Fatalf("rule events: got %d, want 3: %v", len(changes), changes)
	}
	if changes[0]["change"] != "created" {
		t.Errorf("first rule event: got %v, want created", changes[0]["change"])
	}
	if changes[0]["ended_at"] != nil {
		t.Errorf("first rule event ended at: got %v, want none", changes[0]["ended_at"])
	}
	if changes[1]["change"] != "ended" {
		t.Errorf("second rule event: got %v, want ended", changes[1]["change"])
	}
	if changes[1]["ended_at"] == nil {
		t.Errorf("second rule event ended at: got none, want a time")
	}
	if changes[1]["valid_to"] != day(1) {
		t.Errorf("second rule event valid to: got %v, want %s", changes[1]["valid_to"], day(1))
	}
}
