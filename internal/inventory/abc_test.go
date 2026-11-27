package inventory_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

const (
	abcAnalysis     = "/v1/cycle-count-abc-analysis"
	abcApplications = "/v1/cycle-count-abc-applications"
)

// abcPerms are the permissions to post, count, plan counts and keep
// batches.
var abcPerms = append([]string{"inventory.count_plan.write", "inventory.batch.write"}, countPerms...)

// abcScene is env with four materials at S1 of P1 and a batch-managed
// sealant, each issued to a cost center this month: the bracket 70.00
// (35 at 2.00), the second 20.00, the third 6.00 (6 issued, and 3 more
// issued and reversed), the fourth nothing, the sealant 4.00; 100.00 in all.
type abcScene struct {
	env
	second, third, fourth, sealant string
	a, b, c                        map[string]any
}

func abcSetup(t *testing.T, configure func(c *config.Config)) abcScene {
	t.Helper()
	s := abcScene{env: setupConfigured(t, configure, abcPerms...)}
	s.second, s.third, s.fourth = mustMaterial(t, s.env), mustMaterial(t, s.env), mustMaterial(t, s.env)
	s.sealant = s.batchMaterial(t, "Thread sealant")
	lot := s.batch(t, s.sealant, "L1", "2026-11-01", "")["id"].(string)
	stocked := func(m, qty string) map[string]any { return s.item(qty, "EA", map[string]any{"material_id": m}) }
	issued := func(m, qty string) map[string]any {
		return s.item(qty, "EA", map[string]any{"material_id": m, "cost_center_id": s.c1})
	}
	sealant := map[string]any{"material_id": s.sealant, "batch_id": lot}
	s.post(t, "561", stocked(s.bracket, "100"), stocked(s.second, "100"), stocked(s.third, "100"), stocked(s.fourth, "100"),
		s.item("50", "EA", sealant))
	s.post(t, "201", issued(s.bracket, "35"), issued(s.second, "20"), issued(s.third, "6"),
		s.item("4", "EA", map[string]any{"material_id": s.sealant, "batch_id": lot, "cost_center_id": s.c1}))
	back := s.post(t, "201", issued(s.third, "3"))
	_, tag := s.Must(200, "GET", docs+"/"+back["id"].(string), "", "")
	s.Must(200, "POST", docs+"/"+back["id"].(string)+":reverse", "", tag)
	return s
}

// window is this month's analysis query of P1: the 30 days to today, the
// test companies' day being UTC's.
func (s abcScene) window() string {
	today := time.Now().UTC()
	return fmt.Sprintf("?plant_id=%s&from=%s&to=%s", s.p1, today.AddDate(0, 0, -29).Format(time.DateOnly), today.Format(time.DateOnly))
}

// applyBody is the apply of this month's analysis with the token.
func (s abcScene) applyBody(t *testing.T, token any) string {
	t.Helper()
	today := time.Now().UTC()
	return mustJSON(t, map[string]any{"plant_id": s.p1, "from": today.AddDate(0, 0, -29).Format(time.DateOnly), "to": today.Format(time.DateOnly),
		"preview_token": token})
}

// abcLines are an analysis's lines as "material class indicator change
// consumption" each.
func (s abcScene) abcLines(t *testing.T, a map[string]any) string {
	t.Helper()
	names := map[any]string{s.bracket: "bracket", s.second: "second", s.third: "third", s.fourth: "fourth"}
	var out []string
	for _, l := range rows(t, a, "lines") {
		out = append(out, fmt.Sprintf("%s %v %v %v %v", names[l["material_id"]], l["class"], l["indicator_code"], l["change"],
			l["consumption"].(map[string]any)["amount"]))
	}
	return strings.Join(out, "; ")
}

// otherHex is the token with its first character replaced by another hex
// digit, so it differs from the token whatever it holds.
func otherHex(token string) string {
	if token[0] == '0' {
		return "1" + token[1:]
	}
	return "0" + token[1:]
}

// TestABCAnalysis (S2c3): a plant's materials are classed by their
// consumption value over the window, reversals given back and the
// materials a count does not take excluded but in the total; each is
// proposed the plant's indicator of its class, and the apply makes the
// changes the preview showed, audited, once: a stale preview is refused.
func TestABCAnalysis(t *testing.T) {
	s := abcSetup(t, valued)
	s.a, s.b = s.indicator(t, s.p1, "A", 90), s.indicator(t, s.p1, "B", 180)
	s.refused(t, "GET", abcAnalysis+s.window(), "", "", 422, "inventory.abc_indicator_missing", "plant_id")
	s.c = s.indicator(t, s.p1, "C", 365)
	s.assign(t, s.second, s.b)
	kept := s.assign(t, s.third, s.b)
	preview, _ := s.Must(200, "GET", abcAnalysis+s.window(), "", "")
	if got, want := s.abcLines(t, preview), "bracket A A create 70.00; second A A change 20.00; third B B none 6.00; fourth C C create 0.00"; got != want {
		t.Errorf("lines: got %q, want %q", got, want)
	}
	if got, want := fmt.Sprintf("%v %v %v %v %v %v %v", preview["plant_code"], preview["company_code"], preview["share_a"], preview["share_b"],
		preview["total_consumption"], preview["excluded_materials"], preview["changes"]), "P1 2000 80 15 map[amount:100.00 currency:EUR] 1 3"; got != want {
		t.Errorf("analysis: got %q, want %q", got, want)
	}
	var classes []string
	for _, c := range rows(t, preview, "classes") {
		classes = append(classes, fmt.Sprintf("%v %v %v %v", c["class"], c["materials"], c["consumption"].(map[string]any)["amount"], c["interval_days"]))
	}
	if got, want := strings.Join(classes, "; ")+" excluded "+preview["excluded_consumption"].(map[string]any)["amount"].(string),
		"A 2 90.00 90; B 1 6.00 180; C 1 0.00 365 excluded 4.00"; got != want {
		t.Errorf("classes: got %q, want %q", got, want)
	}
	if third := rows(t, preview, "lines")[2]; third["cycle_count_material_id"] != kept["id"] || third["before_indicator_code"] != "B" {
		t.Errorf("third's line: got %v, want its indicator B named", third)
	}
	token := preview["preview_token"].(string)
	s.refused(t, "POST", abcApplications, s.applyBody(t, otherHex(token)), "", 409, "inventory.abc_preview_stale", "")
	applied, _ := s.Must(201, "POST", abcApplications, s.applyBody(t, token), "")
	s.appliedAs(t, applied, token)
	s.refused(t, "POST", abcApplications, s.applyBody(t, token), "", 409, "inventory.abc_preview_stale", "")
	again, _ := s.Must(200, "GET", abcAnalysis+s.window(), "", "")
	if got := again["changes"]; got != float64(0) {
		t.Errorf("once applied: got %v changes, want none", got)
	}
	got, _ := s.Must(200, "GET", abcApplications+"/"+applied["id"].(string), "", "")
	if fmt.Sprint(got["preview_token"], len(rows(t, got, "changes"))) != token+"3" {
		t.Errorf("the application read: got %v, want its three changes", got)
	}
	// Applied as it is now, nothing to change: refused, nothing kept.
	s.refused(t, "POST", abcApplications, s.applyBody(t, again["preview_token"]), "", 422, "inventory.abc_nothing_to_apply", "")
	// The bracket moved to B by hand is moved back by the next one.
	bracket := rows(t, applied, "changes")[0]["cycle_count_material_id"].(string)
	_, tag := s.Must(200, "GET", cycleMaterials+"/"+bracket, "", "")
	s.Must(200, "PATCH", cycleMaterials+"/"+bracket, mustJSON(t, map[string]any{"indicator_id": s.b["id"]}), tag)
	moved, _ := s.Must(200, "GET", abcAnalysis+s.window(), "", "")
	if got, want := strings.Split(s.abcLines(t, moved), "; ")[0], "bracket A A change 70.00"; got != want {
		t.Errorf("the bracket moved to B: got %q, want %q", got, want)
	}
	movedBack, _ := s.Must(201, "POST", abcApplications, s.applyBody(t, moved["preview_token"]), "")
	if got := len(rows(t, movedBack, "changes")); got != 1 {
		t.Errorf("the bracket moved back: got %d changes, want 1", got)
	}
	page, _ := s.Must(200, "GET", abcApplications+"?page_size=1&plant_id="+s.p1.String(), "", "")
	if items := rows(t, page, "items"); len(items) != 1 || items[0]["id"] != movedBack["id"] || page["next_page_token"] == nil {
		t.Fatalf("the applications at P1: got %v, want the latest first and a next page", page)
	}
	if item := rows(t, page, "items")[0]; item["change_count"] != float64(1) || item["changes"] != nil {
		t.Errorf("the latest listed: got change_count %v and changes %v, want 1 counted, none listed", item["change_count"], item["changes"])
	}
	page, _ = s.Must(200, "GET", abcApplications+"?page_size=1&plant_id="+s.p1.String()+"&page_token="+page["next_page_token"].(string), "", "")
	if items := rows(t, page, "items"); len(items) != 1 || items[0]["id"] != applied["id"] {
		t.Errorf("the applications at P1, second page: got %v, want the first applied", items)
	}
	page, _ = s.Must(200, "GET", abcApplications+"?plant_id="+s.p2.String(), "", "")
	if items := rows(t, page, "items"); len(items) != 0 {
		t.Errorf("the applications at P2: got %v, want none", items)
	}
	s.refused(t, "GET", abcApplications+"/"+uuid.NewString(), "", "", 404, "request.not_found", "")
}

// appliedAs checks what the apply made and kept: the bracket given
// indicator A, the second moved to it from B and the fourth given C, each
// audited naming the analysis, and the application's own record.
func (s abcScene) appliedAs(t *testing.T, applied map[string]any, token string) {
	t.Helper()
	names := map[any]string{s.bracket: "bracket", s.second: "second", s.third: "third", s.fourth: "fourth"}
	var changes []string
	for _, c := range rows(t, applied, "changes") {
		changes = append(changes, fmt.Sprintf("%v %s %v %v %v>%v %v", c["line_no"], names[c["material_id"]], c["class"], c["change"],
			c["before_indicator_code"], c["indicator_code"], c["consumption"].(map[string]any)["amount"]))
	}
	if got, want := strings.Join(changes, "; "), "1 bracket A create <nil>>A 70.00; 2 second A change B>A 20.00; 3 fourth C create <nil>>C 0.00"; got != want {
		t.Errorf("changes: got %q, want %q", got, want)
	}
	if got, want := fmt.Sprintf("%v %v %v %v %v %v", applied["preview_token"] == token, applied["plant_code"], applied["share_a"], applied["share_b"],
		applied["total_consumption"], applied["created_by_name"] != ""), "true P1 80 15 map[amount:100.00 currency:EUR] true"; got != want {
		t.Errorf("the application: got %q, want %q", got, want)
	}
	first := rows(t, applied, "changes")[0]
	bracket, _ := s.Must(200, "GET", cycleMaterials+"/"+first["cycle_count_material_id"].(string), "", "")
	if got, want := fmt.Sprint(bracket["indicator_code"], bracket["status"]), "Aactive"; got != want {
		t.Errorf("the bracket's indicator: got %q, want %q", got, want)
	}
	au := s.audits(t, bracket["id"].(string), "inventory.cycle_count_material.create")
	if len(au) != 1 {
		t.Fatalf("audit of the bracket's indicator: got %v, want one", au)
	}
	if from, _ := au[0]["abc_analysis"].(map[string]any); from["preview_token"] != token || from["class"] != "A" || from["consumption"] != "70.00" ||
		from["application_id"] != applied["id"] {
		t.Errorf("audit of the bracket's indicator: got %v, want the analysis named", au[0])
	}
	second := rows(t, applied, "changes")[1]["cycle_count_material_id"].(string)
	if au := s.audits(t, second, "inventory.cycle_count_material.change"); len(au) != 1 || fmt.Sprint(au[0]["after"].(map[string]any)["indicator_code"]) != "A" ||
		au[0]["abc_analysis"] == nil {
		t.Errorf("audit of the second's change: got %v, want B to A by the analysis", au)
	}
	if au := s.audits(t, applied["id"].(string), "inventory.cycle_count_abc_application.create"); len(au) != 1 || au[0]["changes"] != float64(3) {
		t.Errorf("audit of the application: got %v, want one of three changes", au)
	}
}

// TestABCShares (S2c3): the company's shares class the materials: with A
// at 50 percent and B at 40 more, the second is B and the third C.
func TestABCShares(t *testing.T) {
	s := abcSetup(t, func(c *config.Config) {
		valued(c)
		c.Company("2000").CycleCountABCShares = &config.ABCShares{A: 50, B: 40}
	})
	s.indicator(t, s.p1, "A", 90)
	s.indicator(t, s.p1, "B", 180)
	s.indicator(t, s.p1, "C", 365)
	preview, _ := s.Must(200, "GET", abcAnalysis+s.window(), "", "")
	if got, want := s.abcLines(t, preview), "bracket A A create 70.00; second B B create 20.00; third C C create 6.00; fourth C C create 0.00"; got != want {
		t.Errorf("lines: got %q, want %q", got, want)
	}
	if got := fmt.Sprint(preview["share_a"], preview["share_b"]); got != "50 40" {
		t.Errorf("shares: got %s, want 50 40", got)
	}
}

// TestABCRefusals (S2c3): the window ends on or before the company's day
// and spans at most 366 days; the plant is an active plant.
func TestABCRefusals(t *testing.T) {
	e := cycleSetup(t)
	today := time.Now().UTC()
	day := func(days int) string { return today.AddDate(0, 0, days).Format(time.DateOnly) }
	for name, c := range map[string]struct {
		plant    uuid.UUID
		from, to string
		code     string
		field    string
	}{
		"a window ending tomorrow":         {e.p1, day(-10), day(1), "inventory.abc_window_invalid", "to"},
		"a window ending before it starts": {e.p1, day(-1), day(-2), "inventory.abc_window_invalid", "to"},
		"a window of 367 days":             {e.p1, day(-366), day(0), "inventory.abc_window_invalid", "from"},
		"a storage location":               {e.s1, day(-10), day(0), "inventory.plant_invalid", "plant_id"},
	} {
		t.Run(name, func(t *testing.T) {
			e.refused(t, "GET", fmt.Sprintf("%s?plant_id=%s&from=%s&to=%s", abcAnalysis, c.plant, c.from, c.to), "", "", 422, c.code, c.field)
		})
	}
	t.Run("a window of 366 days", func(t *testing.T) {
		got, _ := e.Must(200, "GET", fmt.Sprintf("%s?plant_id=%s&from=%s&to=%s", abcAnalysis, e.p1, day(-365), day(0)), "", "")
		if lines := rows(t, got, "lines"); len(lines) != 0 || got["changes"] != float64(0) {
			t.Errorf("a plant with nothing to class: got %v, want no lines", got)
		}
	})
}

// TestABCTooMany (S2c3): an analysis lists at most so many materials
// (1,000, lowered here to none).
func TestABCTooMany(t *testing.T) {
	e := cycleSetup(t)
	e.post(t, "561", e.item("1", "EA", nil))
	inventory.SetMaxABCLines(t, 0)
	err := e.inRequest(t, func(ctx context.Context) error {
		_, err := endpoints(t).ABC(ctx, inventory.ABCInput{PlantID: e.p1, From: time.Now().UTC(), To: time.Now().UTC()})
		return err
	})
	if p := problem.From(err); p.Code != "inventory.abc_too_many_materials" {
		t.Errorf("more materials than an analysis lists: got %v, want inventory.abc_too_many_materials", err)
	}
}

// TestABCLocks (S2c3): applies of one plant wait for each other, the
// second finding the first's changes and refused as stale; an apply and
// a removal of a listed material's indicator wait for each other in
// either order, the apply that comes second refused as stale.
func TestABCLocks(t *testing.T) {
	s := abcSetup(t, valued)
	s.a, s.b, s.c = s.indicator(t, s.p1, "A", 90), s.indicator(t, s.p1, "B", 180), s.indicator(t, s.p1, "C", 365)
	h := endpoints(t)
	today := time.Now().UTC()
	in := inventory.ABCInput{PlantID: s.p1, From: today.AddDate(0, 0, -29), To: today}
	token := func() string {
		t.Helper()
		var out string
		if err := s.inRequest(t, func(ctx context.Context) error {
			a, err := h.ABC(ctx, in)
			out = a.Token
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	apply := func(tok string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.ApplyABC(ctx, in, tok)
			return err
		}
	}
	tok := token()
	first, second := apitest.Serialized(t, s.Tenant, s.Tenant.Human, apply(tok), apply(tok))
	if p := problem.From(second); first != nil || p.Code != "inventory.abc_preview_stale" {
		t.Errorf("two applies: got %v and %v, want the second refused as stale", first, second)
	}
	page, _ := s.Must(200, "GET", cycleMaterials+"?plant_id="+s.p1.String()+"&material_id="+s.second, "", "")
	second2 := uuid.MustParse(rows(t, page, "items")[0]["id"].(string))
	remove := func(ctx context.Context) error {
		_, err := h.RemoveCycleMaterial(ctx, second2, func(int32) error { return nil })
		return err
	}
	tok = token()
	first, second = apitest.Serialized(t, s.Tenant, s.Tenant.Human, remove, apply(tok))
	if p := problem.From(second); first != nil || p.Code != "inventory.abc_preview_stale" {
		t.Errorf("a removal, then an apply: got %v and %v, want the apply refused as stale", first, second)
	}
	// The fourth moved to A by hand, which the apply moves back to C
	// while a removal of it waits.
	page, _ = s.Must(200, "GET", cycleMaterials+"?plant_id="+s.p1.String()+"&material_id="+s.fourth+"&status=active", "", "")
	fourth := rows(t, page, "items")[0]["id"].(string)
	_, tag := s.Must(200, "GET", cycleMaterials+"/"+fourth, "", "")
	s.Must(200, "PATCH", cycleMaterials+"/"+fourth, mustJSON(t, map[string]any{"indicator_id": s.a["id"]}), tag)
	tok = token()
	first, second = apitest.Serialized(t, s.Tenant, s.Tenant.Human, apply(tok), func(ctx context.Context) error {
		_, err := h.RemoveCycleMaterial(ctx, uuid.MustParse(fourth), func(int32) error { return nil })
		return err
	})
	if first != nil || second != nil {
		t.Errorf("an apply, then a removal: got %v and %v, want both done", first, second)
	}
	if got, _ := s.Must(200, "GET", cycleMaterials+"/"+fourth, "", ""); fmt.Sprint(got["indicator_code"], got["status"], got["version"]) != "Cremoved4" {
		t.Errorf("the fourth's indicator: got %v, want C, moved back by the apply and then removed", got)
	}
}

// TestCycleCountRemove (S2c3): a material's indicator is taken off at its
// version, once, kept as removed and never changed again; the material is
// then not due by it and may be given an indicator again; the list
// filters by status.
func TestCycleCountRemove(t *testing.T) {
	e := cycleSetup(t)
	e.post(t, "561", e.item("10", "EA", nil))
	a, b := e.indicator(t, e.p1, "A", 30), e.indicator(t, e.p1, "B", 90)
	given := e.assign(t, e.bracket, a)
	id := given["id"].(string)
	if given["status"] != "active" {
		t.Errorf("given: got status %v, want active", given["status"])
	}
	_, tag := e.Must(200, "GET", cycleMaterials+"/"+id, "", "")
	e.refused(t, "POST", cycleMaterials+"/"+id+":remove", "", `"9"`, 412, "request.version_mismatch", "")
	removed, newTag := e.Must(200, "POST", cycleMaterials+"/"+id+":remove", "", tag)
	if got, want := fmt.Sprintf("%v %v %v", removed["status"], removed["version"], removed["indicator_code"]), "removed 2 A"; got != want {
		t.Errorf("removed: got %q, want %q", got, want)
	}
	if au := e.audits(t, id, "inventory.cycle_count_material.remove"); len(au) != 1 || au[0]["indicator_code"] != "A" {
		t.Errorf("audit of the removal: got %v, want one naming A", au)
	}
	e.refused(t, "POST", cycleMaterials+"/"+id+":remove", "", newTag, 409, "inventory.cycle_count_material_removed", "")
	e.refused(t, "PATCH", cycleMaterials+"/"+id, mustJSON(t, map[string]any{"indicator_id": b["id"]}), newTag, 409, "inventory.cycle_count_material_removed", "")
	if got := e.dueOn(t, 0, gen.ListCycleCountsDueParams{}); got != "" {
		t.Errorf("due once removed: got %q, want none", got)
	}
	again := e.assign(t, e.bracket, b)
	for status, want := range map[string]string{"active": again["id"].(string), "removed": id} {
		page, _ := e.Must(200, "GET", cycleMaterials+"?material_id="+e.bracket+"&status="+status, "", "")
		if items := rows(t, page, "items"); len(items) != 1 || items[0]["id"] != want {
			t.Errorf("%s: got %v, want %s alone", status, items, want)
		}
	}
	page, _ := e.Must(200, "GET", cycleMaterials+"?material_id="+e.bracket, "", "")
	if items := rows(t, page, "items"); len(items) != 2 {
		t.Errorf("every status: got %v, want both", items)
	}
	if got, want := e.dueOn(t, 0, gen.ListCycleCountsDueParams{}), "S1 "+e.number(t, e.bracket)+" B never now"; got != want {
		t.Errorf("due by the indicator given again: got %q, want %q", got, want)
	}
}

// TestABCDatabaseFailures (S2c3): every database error of the analysis,
// its apply and a removal is answered as an error, none outside a unit
// of work.
func TestABCDatabaseFailures(t *testing.T) {
	s := abcSetup(t, valued)
	s.a, s.b, s.c = s.indicator(t, s.p1, "A", 90), s.indicator(t, s.p1, "B", 180), s.indicator(t, s.p1, "C", 365)
	s.assign(t, s.second, s.b)
	third := s.assign(t, s.third, s.b)
	h := endpoints(t)
	today := time.Now().UTC()
	from := today.AddDate(0, 0, -29)
	preview, _ := s.Must(200, "GET", abcAnalysis+s.window(), "", "")
	applied, _ := s.Must(201, "POST", abcApplications, s.applyBody(t, preview["preview_token"]), "")
	app := uuid.MustParse(applied["id"].(string))
	// The third moved to A by hand, which the apply below moves back to B.
	id := uuid.MustParse(third["id"].(string))
	_, tag := s.Must(200, "GET", cycleMaterials+"/"+id.String(), "", "")
	_, tag = s.Must(200, "PATCH", cycleMaterials+"/"+id.String(), mustJSON(t, map[string]any{"indicator_id": s.a["id"]}), tag)
	preview, _ = s.Must(200, "GET", abcAnalysis+s.window(), "", "")
	for name, call := range map[string]func(ctx context.Context) error{
		"get an application": func(ctx context.Context) error {
			_, err := h.GetCycleCountAbcApplication(ctx, gen.GetCycleCountAbcApplicationParams{ID: app})
			return err
		},
		"list applications": func(ctx context.Context) error {
			_, err := h.ListCycleCountAbcApplications(ctx, gen.ListCycleCountAbcApplicationsParams{})
			return err
		},
		"preview": func(ctx context.Context) error {
			_, err := h.GetCycleCountAbcAnalysis(ctx, gen.GetCycleCountAbcAnalysisParams{PlantID: s.p1, From: from, To: today})
			return err
		},
		"apply": func(ctx context.Context) error {
			_, err := h.CreateCycleCountAbcApplication(ctx, &gen.CycleCountAbcApply{PlantID: s.p1, From: from, To: today,
				PreviewToken: preview["preview_token"].(string)}, gen.CreateCycleCountAbcApplicationParams{})
			return err
		},
		"remove": func(ctx context.Context) error {
			_, err := h.RemoveCycleCountMaterial(ctx, gen.RemoveCycleCountMaterialParams{ID: id, IfMatch: tag})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, s.Tenant, s.Tenant.Human, name, call)
		})
	}
	t.Run("page size", func(t *testing.T) {
		err := s.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListCycleCountAbcApplications(ctx, gen.ListCycleCountAbcApplicationsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
}
