package grc_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// stored inserts a link as it is stored, for a target the API would not
// link to: one that was there when the link was made and is gone since.
func (e env) stored(t *testing.T, control, kind, target string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO grc.control_link (tenant_id, id, control_id, kind, target, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $6)`,
		e.Tenant.ID, uuid.Must(uuid.NewV7()), control, kind, target, e.Tenant.Human.ID)
}

// scene is a library for the matrix: GL-01 without a link; PAY-01 with
// a permission, a rule in force and a configuration path, all holding,
// and two framework references; PAY-02 with a permission that holds and
// a rule that ended; PAY-03 with a rule that has not started; REC-01
// with a permission, a rule and a path that are gone; RET-01, retired,
// with a link. ids are the controls by code.
type scene struct {
	env
	ids                      map[string]string
	inForce, ended, upcoming uuid.UUID
	gone                     uuid.UUID // a rule the tenant does not have
}

func matrixScene(t *testing.T) scene {
	t.Helper()
	s := scene{env: setup(t), ids: map[string]string{}, gone: uuid.Must(uuid.NewV7())}
	s.inForce = s.rule(t, "credit.limit.read", "credit.limit.write", -1, nil)
	s.ended = s.rule(t, "sales.price.read", "sales.price.write", -9, days(0))
	s.upcoming = s.rule(t, "procurement.catalog.manage", "procurement.catalog.read", 1, nil)
	for _, c := range []struct{ code, process, refs string }{
		{"REC-01", "Receivables", `[]`}, {"PAY-02", "Payments", `["SOX 404"]`}, {"GL-01", "General ledger", `[]`},
		{"PAY-01", "Payments", `["ICFR 4.2 Payments", "SOX 404"]`}, {"PAY-03", "Payments", `[]`}, {"RET-01", "Payments", `["SOX 404"]`},
	} {
		body := fmt.Sprintf(`{"code": %q, "objective": "x", "process": %q, "owner_id": %q, "frequency": "monthly", "nature": "manual",
			"framework_references": %s}`, c.code, c.process, s.carla.ID, c.refs)
		got, _ := s.Must(201, "POST", "/v1/controls", body, "")
		s.ids[c.code] = apitest.String(t, got, "id")
	}
	s.link(t, s.ids["PAY-01"], permissionLink(releasePermission))
	s.link(t, s.ids["PAY-01"], ruleLink(s.inForce))
	s.link(t, s.ids["PAY-01"], pathLink(settingPath))
	s.link(t, s.ids["PAY-02"], permissionLink(releasePermission))
	s.link(t, s.ids["PAY-02"], ruleLink(s.ended))
	s.link(t, s.ids["PAY-03"], ruleLink(s.upcoming))
	s.stored(t, s.ids["REC-01"], "permission", "payments.run.vanish")
	s.stored(t, s.ids["REC-01"], "sod_rule", s.gone.String())
	s.stored(t, s.ids["REC-01"], "configuration_path", "companies[9999].currency")
	removed, tag := s.link(t, s.ids["GL-01"], pathLink(sectionPath))
	s.Must(200, "POST", "/v1/controls/"+s.ids["GL-01"]+"/links/"+removed+":remove", "", tag)
	s.link(t, s.ids["RET-01"], permissionLink(releasePermission))
	_, tag = s.Must(200, "GET", "/v1/controls/"+s.ids["RET-01"], "", "")
	s.Must(200, "POST", "/v1/controls/"+s.ids["RET-01"]+":retire", "", tag)
	s.Must(201, "POST", "/v1/controls/"+s.ids["PAY-01"]+"/tests", testOf(day(t, -1), "passed", "Seen in run 12."), "")
	return s
}

// rowsOf is each row of a matrix as "code state", joined by ", ".
func rowsOf(t *testing.T, m map[string]any) string {
	t.Helper()
	var out []string
	for _, r := range objects(t, m, "items") {
		out = append(out, fmt.Sprintf("%v %v", apitest.Object(t, r, "control")["code"], r["link_state"]))
	}
	return strings.Join(out, ", ")
}

// TestMatrix: the matrix lists the active controls by code with their
// links and whether each holds: a permission while the catalog has it,
// a rule while it is in force, a path while the active configuration
// has a value there; a broken link is shown, never removed; the totals
// count what the filters select (T98b acceptance).
func TestMatrix(t *testing.T) {
	s := matrixScene(t)
	m := s.matrix(t, "")
	if got, want := rowsOf(t, m), "GL-01 none, PAY-01 holding, PAY-02 broken, PAY-03 broken, REC-01 broken"; got != want {
		t.Errorf("rows: got %q, want %q", got, want)
	}
	if got, want := totalsOf(t, m), "5 controls 1/3/1, 9 links 4/5"; got != want {
		t.Errorf("totals: got %q, want %q", got, want)
	}
	if m["as_of"] != day(t, 0) {
		t.Errorf("as_of: got %v, want %s", m["as_of"], day(t, 0))
	}
	rows := map[string]map[string]any{}
	for _, r := range objects(t, m, "items") {
		rows[fmt.Sprint(apitest.Object(t, r, "control")["code"])] = r
	}
	for code, want := range map[string]string{
		"GL-01":  "",
		"PAY-01": "permission " + releasePermission + ": holds; sod_rule " + s.inForce.String() + ": holds; configuration_path " + settingPath + ": holds",
		"PAY-02": "permission " + releasePermission + ": holds; sod_rule " + s.ended.String() + ": sod_rule_ended",
		"PAY-03": "sod_rule " + s.upcoming.String() + ": sod_rule_not_started",
		"REC-01": "permission payments.run.vanish: permission_not_in_catalog; sod_rule " + s.gone.String() +
			": sod_rule_not_found; configuration_path companies[9999].currency: configuration_path_without_value",
	} {
		t.Run("the links of "+code, func(t *testing.T) {
			if got := shown(t, rows[code]); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
	t.Run("a control with its references, owner and last test", func(t *testing.T) {
		c := apitest.Object(t, rows["PAY-01"], "control")
		wantFields(t, c, map[string]any{"id": s.ids["PAY-01"], "owner_name": "Carla Controller", "process": "Payments", "status": "active"})
		if got := fmt.Sprint(c["framework_references"]); got != "[ICFR 4.2 Payments SOX 404]" {
			t.Errorf("framework_references: got %s, want [ICFR 4.2 Payments SOX 404]", got)
		}
		wantFields(t, apitest.Object(t, c, "last_test"), map[string]any{"result": "passed", "tested_by_name": "human"})
	})
	t.Run("a broken link with what it names", func(t *testing.T) {
		links := objects(t, rows["PAY-02"], "links")
		wantFields(t, links[1], map[string]any{"kind": "sod_rule", "holds": false, "broken_reason": "sod_rule_ended", "status": "active"})
		wantFields(t, apitest.Object(t, links[1], "sod_rule"), map[string]any{"permission_a": "sales.price.read", "permission_b": "sales.price.write",
			"valid_from": day(t, -9), "valid_to": day(t, 0)})
		gone := objects(t, rows["REC-01"], "links")
		for _, f := range []string{"permission_description", "sod_rule"} {
			if _, has := gone[0][f]; has {
				t.Errorf("%s of a permission the catalog lost: got %v, want it absent", f, gone[0][f])
			}
		}
		if _, has := gone[1]["sod_rule"]; has {
			t.Errorf("sod_rule of a rule the tenant does not have: got %v, want it absent", gone[1]["sod_rule"])
		}
		if gone[2]["configuration_version"] != float64(1) {
			t.Errorf("configuration_version of a path without a value: got %v, want 1", gone[2]["configuration_version"])
		}
	})
	for _, c := range []struct{ name, query, rows, totals string }{
		{"one process", "?process=Payments", "PAY-01 holding, PAY-02 broken, PAY-03 broken", "3 controls 1/2/0, 6 links 4/2"},
		{"one framework reference", "?framework_reference=SOX%20404", "PAY-01 holding, PAY-02 broken", "2 controls 1/1/0, 5 links 4/1"},
		{"a reference nothing carries", "?framework_reference=SOX", "", "0 controls 0/0/0, 0 links 0/0"},
		{"the controls with a broken link", "?link_state=broken", "PAY-02 broken, PAY-03 broken, REC-01 broken", "3 controls 0/3/0, 6 links 1/5"},
		{"the controls whose links hold", "?link_state=holding", "PAY-01 holding", "1 controls 1/0/0, 3 links 3/0"},
		{"the controls without a link", "?link_state=none", "GL-01 none", "1 controls 0/0/1, 0 links 0/0"},
		{"a process and a state", "?process=Payments&link_state=broken", "PAY-02 broken, PAY-03 broken", "2 controls 0/2/0, 3 links 1/2"},
		{"a state nothing is in", "?link_state=unknown", "", "0 controls 0/0/0, 0 links 0/0"},
		{"by code, descending", "?order_by=code%20desc", "REC-01 broken, PAY-03 broken, PAY-02 broken, PAY-01 holding, GL-01 none", "5 controls 1/3/1, 9 links 4/5"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := s.in(t).matrix(t, c.query)
			if got := rowsOf(t, m); got != c.rows {
				t.Errorf("rows: got %q, want %q", got, c.rows)
			}
			if got := totalsOf(t, m); got != c.totals {
				t.Errorf("totals: got %q, want %q", got, c.totals)
			}
		})
	}
	t.Run("in pages of two, each with the totals of all", func(t *testing.T) {
		e := s.in(t)
		var pages []string
		token := ""
		for {
			m := e.matrix(t, "?page_size=2"+token)
			pages = append(pages, rowsOf(t, m))
			if got, want := totalsOf(t, m), "5 controls 1/3/1, 9 links 4/5"; got != want {
				t.Errorf("totals of a page: got %q, want %q", got, want)
			}
			next, more := m["next_page_token"].(string)
			if !more {
				break
			}
			token = "&page_token=" + next
		}
		if got, want := strings.Join(pages, " | "), "GL-01 none, PAY-01 holding | PAY-02 broken, PAY-03 broken | REC-01 broken"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	s.Problem(400, "request.invalid_order_by", "GET", "/v1/control-matrix?order_by=process", "", "")
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM grc.control_link WHERE tenant_id = $1 AND status = 'active'`, []any{s.Tenant.ID}, &n)
	if n != 10 {
		t.Errorf("active links after the reads: got %d, want all 10 kept, the broken ones too", n)
	}
	for _, p := range []string{"identity.access.read", "config.version.read", "grc.control.read"} {
		t.Run("without "+p, func(t *testing.T) {
			s.Tenant.Revoke(t, p, s.Tenant.Human)
			defer s.Tenant.Grant(t, p, s.Tenant.Human)
			s.in(t).Problem(403, "identity.permission_missing", "GET", "/v1/control-matrix", "", "")
		})
	}
}

// TestMatrixFollowsARule: a link to a rule holds while the rule is in
// force and is broken from the day the rule ends, with nothing changed
// on the link.
func TestMatrixFollowsARule(t *testing.T) {
	e := setup(t)
	id, _ := e.record(t, "PAY-01", "Payments")
	rule := e.rule(t, "credit.limit.read", "credit.limit.write", -3, days(1))
	link, _ := e.link(t, id, ruleLink(rule))
	if got, want := shown(t, objects(t, e.matrix(t, ""), "items")[0]), "sod_rule "+rule.String()+": holds"; got != want {
		t.Errorf("on the rule's last day: got %q, want %q", got, want)
	}
	// The rule ended yesterday, as identity's end of it would have it a day on.
	pgtest.Unguarded(t, `UPDATE identity.sod_rule SET valid_to = current_date WHERE id = $1`, rule)
	m := e.matrix(t, "")
	if got, want := shown(t, objects(t, m, "items")[0]), "sod_rule "+rule.String()+": sod_rule_ended"; got != want {
		t.Errorf("once the rule ended: got %q, want %q", got, want)
	}
	if got, want := totalsOf(t, m), "1 controls 0/1/0, 1 links 0/1"; got != want {
		t.Errorf("totals: got %q, want %q", got, want)
	}
	var status string
	var version int
	pgtest.Scan(t, `SELECT status, version FROM grc.control_link WHERE id = $1`, []any{link}, &status, &version)
	if status != "active" || version != 1 {
		t.Errorf("the broken link: got %s at version %d, want it active at version 1", status, version)
	}
}

// TestFrameworkReferences: a control carries the tenant's references to
// a framework, free text in the tenant's order, set when it is recorded
// and replaced by a change.
func TestFrameworkReferences(t *testing.T) {
	e := setup(t)
	body := func(refs string) string {
		return fmt.Sprintf(`{"code": "PAY-01", "objective": "x", "process": "Payments", "owner_id": %q, "frequency": "monthly", "nature": "manual",
			"framework_references": %s}`, e.carla.ID, refs)
	}
	for _, c := range []struct{ name, refs, field string }{
		{"a reference of white space", `["SOX 404", "  "]`, "framework_references[1]"},
		{"a reference with white space around it", `[" SOX 404"]`, "framework_references[0]"},
		{"a reference given twice", `["SOX 404", "ICFR 4.2", "SOX 404"]`, "framework_references[2]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := e.in(t).Problem(422, "grc.invalid_framework_reference", "POST", "/v1/controls", body(c.refs), "")
			errs := objects(t, m, "errors")
			if len(errs) == 0 || errs[0]["field"] != c.field {
				t.Errorf("field: got %v, want %s", errs, c.field)
			}
		})
	}
	got, tag := e.Must(201, "POST", "/v1/controls", body(`["SOX 404", "ICFR 4.2 Payments"]`), "")
	path := "/v1/controls/" + apitest.String(t, got, "id")
	if refs := fmt.Sprint(got["framework_references"]); refs != "[SOX 404 ICFR 4.2 Payments]" {
		t.Errorf("framework_references: got %s, want them in the order given", refs)
	}
	e.Problem(422, "grc.nothing_to_change", "PATCH", path, `{"framework_references": ["SOX 404", "ICFR 4.2 Payments"]}`, tag)
	e.Problem(422, "grc.invalid_framework_reference", "PATCH", path, `{"framework_references": ["a", "a"]}`, tag)
	got, tag = e.Must(200, "PATCH", path, `{"framework_references": ["ICFR 4.2 Payments"]}`, tag)
	if refs := fmt.Sprint(got["framework_references"]); refs != "[ICFR 4.2 Payments]" || got["version"] != float64(2) {
		t.Errorf("after the change: got %s at version %v, want [ICFR 4.2 Payments] at 2", refs, got["version"])
	}
	a := e.audited(t, apitest.String(t, got, "id"))
	if want := `grc.control.change {"code": "PAY-01", "fields": ["framework_references"]}`; len(a) != 2 || a[1] != want {
		t.Errorf("audit: got %v, want %s second", a, want)
	}
	got, _ = e.Must(200, "PATCH", path, `{"framework_references": []}`, tag)
	if refs := fmt.Sprint(got["framework_references"]); refs != "[]" {
		t.Errorf("after clearing them: got %s, want none", refs)
	}
	plain, _ := e.Must(201, "POST", "/v1/controls", controlOf("PAY-02", "Payments", e.carla.ID), "")
	if refs, ok := plain["framework_references"].([]any); !ok || len(refs) != 0 {
		t.Errorf("a control recorded without references: got %v, want an empty list", plain["framework_references"])
	}
}
