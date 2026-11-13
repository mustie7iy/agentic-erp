package grc_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/grc"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// The targets the tests link to: a permission of the catalog, and a
// setting, a list item and a section of the fixture's configuration.
const (
	releasePermission = "payments.run.release"
	settingPath       = "companies[2000].currency"
	itemPath          = "companies[2000]"
	sectionPath       = "payments"
)

// rule inserts a segregation-of-duties rule of the tenant valid from
// today plus from days, to today plus to days (exclusive; nil: no end),
// as identity stores one; the tests date rules in the past, which the
// API refuses.
func (e env) rule(t *testing.T, a, b string, from int, to *int) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, valid_to, created_by)
		VALUES ($1, $2, $3, $4, 'Whoever sets a limit does not read the exposure.', current_date + $5::int, current_date + $6::int, $7)`,
		e.Tenant.ID, id, a, b, from, to, e.Tenant.Human.ID)
	return id
}

func days(n int) *int { return &n }

// permissionLink, ruleLink and pathLink are the JSON of a link.
func permissionLink(code string) string {
	return fmt.Sprintf(`{"kind": "permission", "permission_code": %q}`, code)
}

func ruleLink(id uuid.UUID) string {
	return fmt.Sprintf(`{"kind": "sod_rule", "sod_rule_id": %q}`, id)
}

func pathLink(path string) string {
	return fmt.Sprintf(`{"kind": "configuration_path", "configuration_path": %q}`, path)
}

// link makes a link of the control and returns its id and ETag.
func (e env) link(t *testing.T, control, body string) (string, string) {
	t.Helper()
	l, tag := e.Must(201, "POST", "/v1/controls/"+control+"/links", body, "")
	return apitest.String(t, l, "id"), tag
}

// matrix reads the matrix with the query ("" or "?...").
func (e env) matrix(t *testing.T, query string) map[string]any {
	t.Helper()
	m, _ := e.Must(200, "GET", "/v1/control-matrix"+query, "", "")
	return m
}

// shown is each link of a matrix row as "kind target: holds" or
// "kind target: reason", joined by "; ".
func shown(t *testing.T, row map[string]any) string {
	t.Helper()
	var out []string
	for _, l := range objects(t, row, "links") {
		target := l["permission_code"]
		switch l["kind"] {
		case "sod_rule":
			target = l["sod_rule_id"]
		case "configuration_path":
			target = l["configuration_path"]
		}
		state := "holds"
		if l["holds"] != true {
			state = fmt.Sprint(l["broken_reason"])
		} else if _, has := l["broken_reason"]; has {
			t.Errorf("link %v: got a broken_reason on a link that holds", l["id"])
		}
		out = append(out, fmt.Sprintf("%v %v: %s", l["kind"], target, state))
	}
	return strings.Join(out, "; ")
}

// totalsOf is a matrix's totals as "controls holding/broken/without, links holding/broken".
func totalsOf(t *testing.T, m map[string]any) string {
	t.Helper()
	x := apitest.Object(t, m, "totals")
	return fmt.Sprintf("%v controls %v/%v/%v, %v links %v/%v", x["controls"], x["controls_holding"], x["controls_broken"], x["controls_without_links"],
		x["links"], x["links_holding"], x["links_broken"])
}

// TestLinkAndRead: a control is linked to a permission, a
// segregation-of-duties rule and a configuration path; each link reads
// back with what it names and that it holds, and the audit record and
// ControlChanged name the link (T98b acceptance).
func TestLinkAndRead(t *testing.T) {
	e := setup(t)
	id, tag := e.record(t, "PAY-01", "Payments")
	rule := e.rule(t, "credit.limit.read", "credit.limit.write", -1, nil)
	path := "/v1/controls/" + id + "/links"

	p, ptag := e.Must(201, "POST", path, permissionLink(releasePermission), "")
	wantFields(t, p, map[string]any{"control_id": id, "control_code": "PAY-01", "kind": "permission", "permission_code": releasePermission,
		"permission_description": "Release payment runs", "status": "active", "holds": true, "version": float64(1)})
	if ptag != `"1"` {
		t.Errorf("ETag of the link: got %s, want \"1\"", ptag)
	}
	for _, f := range []string{"sod_rule_id", "sod_rule", "configuration_path", "configuration_version", "broken_reason"} {
		if _, has := p[f]; has {
			t.Errorf("%s on a permission link: got %v, want it absent", f, p[f])
		}
	}
	r, _ := e.Must(201, "POST", path, ruleLink(rule), "")
	wantFields(t, r, map[string]any{"kind": "sod_rule", "sod_rule_id": rule.String(), "holds": true})
	wantFields(t, apitest.Object(t, r, "sod_rule"), map[string]any{"permission_a": "credit.limit.read", "permission_b": "credit.limit.write",
		"description": "Whoever sets a limit does not read the exposure.", "valid_from": day(t, -1)})
	if _, has := apitest.Object(t, r, "sod_rule")["valid_to"]; has {
		t.Error("valid_to of a rule without an end: got one, want it absent")
	}
	c, _ := e.Must(201, "POST", path, pathLink(settingPath), "")
	wantFields(t, c, map[string]any{"kind": "configuration_path", "configuration_path": settingPath, "configuration_version": float64(1), "holds": true})

	read, after := e.Must(200, "GET", "/v1/controls/"+id, "", "")
	if after != tag || read["version"] != float64(1) {
		t.Errorf("the control after its links: got version %v, ETag %s, want it unchanged at 1", read["version"], after)
	}
	a := e.audited(t, id)
	wantAudit := fmt.Sprintf(`grc.control.link {"code": "PAY-01", "kind": "permission", "target": %q, "link_id": %q}`, releasePermission, p["id"])
	if len(a) != 4 || a[1] != wantAudit {
		t.Errorf("audit: got %v, want %s second of four", a, wantAudit)
	}
	events := e.changed(t)
	if len(events) != 4 {
		t.Fatalf("events: got %d, want 4", len(events))
	}
	wantFields(t, events[1], map[string]any{"control_id": id, "change": "linked", "version": float64(1), "test": nil})
	wantFields(t, apitest.Object(t, events[1], "link"), map[string]any{"link_id": p["id"], "kind": "permission", "target": releasePermission})
	if events[0]["link"] != nil {
		t.Errorf("link of the created event: got %v, want null", events[0]["link"])
	}
}

// TestLinkRefusals: a link names exactly the field of its kind and
// something that is there; a control has one link to a target; a
// retired control takes none.
func TestLinkRefusals(t *testing.T) {
	e := setup(t)
	id, tag := e.record(t, "PAY-01", "Payments")
	path := "/v1/controls/" + id + "/links"
	rule := e.rule(t, "credit.limit.read", "credit.limit.write", -1, nil)
	e.link(t, id, permissionLink(releasePermission))
	for _, c := range []struct {
		name, body  string
		status      int
		code, field string
	}{
		{"an unknown kind", `{"kind": "report", "permission_code": "a.b"}`, 422, "request.validation_failed", "kind"},
		{"a permission link without its code", `{"kind": "permission"}`, 422, "grc.invalid_link", "permission_code"},
		{"a permission link with a rule too", fmt.Sprintf(`{"kind": "permission", "permission_code": "a.b", "sod_rule_id": %q}`, rule), 422, "grc.invalid_link", "sod_rule_id"},
		{"a rule link without its rule", `{"kind": "sod_rule"}`, 422, "grc.invalid_link", "sod_rule_id"},
		{"a rule link with a path too", fmt.Sprintf(`{"kind": "sod_rule", "sod_rule_id": %q, "configuration_path": "payments"}`, rule), 422, "grc.invalid_link", "configuration_path"},
		{"a path link without its path", `{"kind": "configuration_path"}`, 422, "grc.invalid_link", "configuration_path"},
		{"a path link with a permission too", `{"kind": "configuration_path", "configuration_path": "payments", "permission_code": "a.b"}`, 422, "grc.invalid_link", "permission_code"},
		{"a path with white space around it", pathLink(" payments"), 422, "grc.invalid_link", "configuration_path"},
		{"a permission the catalog does not hold", permissionLink("payments.run.vanish"), 422, "grc.unknown_permission", "permission_code"},
		{"a rule the tenant does not have", ruleLink(uuid.Must(uuid.NewV7())), 422, "request.reference_not_found", "sod_rule_id"},
		{"a path without a value", pathLink("companies[9999].currency"), 422, "grc.configuration_path_without_value", "configuration_path"},
		{"a path of the extensions", pathLink("extensions"), 422, "grc.configuration_path_without_value", "configuration_path"},
		{"a second link to the permission", permissionLink(releasePermission), 409, "grc.control_link_exists", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := e.in(t).Problem(c.status, c.code, "POST", path, c.body, "")
			if c.field == "" {
				return
			}
			errs := objects(t, m, "errors")
			if len(errs) == 0 || errs[0]["field"] != c.field {
				t.Errorf("field: got %v, want %s", errs, c.field)
			}
		})
	}
	e.Problem(404, "request.not_found", "POST", "/v1/controls/"+uuid.Must(uuid.NewV7()).String()+"/links", permissionLink(releasePermission), "")
	t.Run("another tenant's rule", func(t *testing.T) {
		other := setup(t)
		foreign := other.rule(t, "credit.limit.read", "credit.limit.write", -1, nil)
		e.in(t).Problem(422, "request.reference_not_found", "POST", path, ruleLink(foreign), "")
	})
	t.Run("without the permission to read access data", func(t *testing.T) {
		e.Tenant.Revoke(t, "identity.access.read", e.Tenant.Human)
		defer e.Tenant.Grant(t, "identity.access.read", e.Tenant.Human)
		e.in(t).Problem(403, "identity.permission_missing", "POST", path, pathLink(sectionPath), "")
	})
	t.Run("on a retired control", func(t *testing.T) {
		e := e.in(t)
		e.Must(200, "POST", "/v1/controls/"+id+":retire", "", tag)
		e.Problem(409, "grc.control_retired", "POST", path, pathLink(sectionPath), "")
	})
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM grc.control_link WHERE control_id = $1`, []any{id}, &n)
	if n != 1 {
		t.Errorf("links after the refusals: got %d, want the first alone", n)
	}
}

// TestRemoveLink: a link is removed once with its version; it is kept
// as removed, left out of the matrix, and the target can be linked
// again; a retired control's links stay.
func TestRemoveLink(t *testing.T) {
	e := setup(t)
	id, tag := e.record(t, "PAY-01", "Payments")
	other, _ := e.record(t, "PAY-02", "Payments")
	link, ltag := e.link(t, id, permissionLink(releasePermission))
	kept, ktag := e.link(t, id, pathLink(itemPath))
	remove := "/v1/controls/" + id + "/links/" + link + ":remove"
	e.Problem(412, "request.version_mismatch", "POST", remove, "", `"9"`)
	e.Problem(404, "request.not_found", "POST", "/v1/controls/"+other+"/links/"+link+":remove", "", ltag)
	e.Problem(404, "request.not_found", "POST", "/v1/controls/"+id+"/links/"+uuid.Must(uuid.NewV7()).String()+":remove", "", ltag)
	e.Problem(404, "request.not_found", "POST", "/v1/controls/"+uuid.Must(uuid.NewV7()).String()+"/links/"+link+":remove", "", ltag)

	got, next := e.Must(200, "POST", remove, "", ltag)
	wantFields(t, got, map[string]any{"id": link, "status": "removed", "version": float64(2), "kind": "permission",
		"permission_code": releasePermission, "holds": true})
	if next != `"2"` {
		t.Errorf("ETag of the removed link: got %s, want \"2\"", next)
	}
	e.Problem(409, "grc.control_link_removed", "POST", remove, "", next)
	m := e.matrix(t, "?process=Payments")
	rows := objects(t, m, "items")
	if got, want := shown(t, rows[0]), "configuration_path "+itemPath+": holds"; got != want {
		t.Errorf("the control's links after the removal: got %q, want %q", got, want)
	}
	again, _ := e.link(t, id, permissionLink(releasePermission))
	if again == link {
		t.Errorf("the link made again: got the removed link's id %s, want a new one", link)
	}
	a := e.audited(t, id)
	wantAudit := fmt.Sprintf(`grc.control.unlink {"code": "PAY-01", "kind": "permission", "target": %q, "link_id": %q}`, releasePermission, link)
	if len(a) != 5 || a[3] != wantAudit {
		t.Errorf("audit: got %v, want %s fourth of five", a, wantAudit)
	}
	events := e.changed(t)
	if len(events) != 6 {
		t.Fatalf("events: got %d, want 6", len(events))
	}
	wantFields(t, events[4], map[string]any{"control_id": id, "change": "unlinked"})
	wantFields(t, apitest.Object(t, events[4], "link"), map[string]any{"link_id": link, "kind": "permission", "target": releasePermission})

	e.Must(200, "POST", "/v1/controls/"+id+":retire", "", tag)
	e.Problem(409, "grc.control_retired", "POST", "/v1/controls/"+id+"/links/"+kept+":remove", "", ktag)
}

// TestLinkWaitsForARetirement: making a link locks the control, so a
// link started while the control is being retired waits and is then
// refused.
func TestLinkWaitsForARetirement(t *testing.T) {
	e := setup(t)
	id, _ := e.record(t, "PAY-01", "Payments")
	control := uuid.MustParse(id)
	h := endpoints(t)
	code := releasePermission
	retired, linked := apitest.Serialized(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := grc.Retire(ctx, control, func(int32) error { return nil })
		return err
	}, func(ctx context.Context) error {
		_, err := h.Link(ctx, control, grc.NewLink{Kind: grc.KindPermission, PermissionCode: &code})
		return err
	})
	if retired != nil {
		t.Fatalf("the retirement: got %v, want it done", retired)
	}
	if got := problemCode(linked); got != "409 grc.control_retired" {
		t.Errorf("the link that waited: got %s, want 409 grc.control_retired", got)
	}
}

// TestLinkWithoutAConfiguration: a tenant without a configuration links
// a control to a permission; a configuration path is refused as the
// configuration is: not bootstrapped.
func TestLinkWithoutAConfiguration(t *testing.T) {
	ten := pgtest.NewTenant(t, perms...)
	c := apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: ten.Human}
	got, _ := c.Must(201, "POST", "/v1/controls", controlOf("PAY-01", "Payments", ten.Human.ID), "")
	path := "/v1/controls/" + apitest.String(t, got, "id") + "/links"
	c.Must(201, "POST", path, permissionLink(releasePermission), "")
	c.Problem(409, "config.not_bootstrapped", "POST", path, pathLink(sectionPath), "")
	m, _ := c.Must(200, "GET", "/v1/control-matrix", "", "")
	if got, want := totalsOf(t, m), "1 controls 1/0/0, 1 links 1/0"; got != want {
		t.Errorf("the matrix without a configuration: got %q, want %q", got, want)
	}
}
