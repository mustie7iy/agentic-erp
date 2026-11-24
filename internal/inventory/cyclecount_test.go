package inventory_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

const (
	indicators     = "/v1/cycle-count-indicators"
	cycleMaterials = "/v1/cycle-count-materials"
	countsDue      = "/v1/cycle-counts-due"
)

// cycleSetup is setup with the permissions to count and to plan counts.
func cycleSetup(t *testing.T) env {
	t.Helper()
	return setupWith(t, append([]string{"inventory.count_plan.write"}, countPerms...)...)
}

// indicator creates an indicator of the plant and returns it.
func (e env) indicator(t *testing.T, plant uuid.UUID, code string, days int) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", indicators, mustJSON(t, map[string]any{"plant_id": plant, "code": code, "interval_days": days}), "")
	return got
}

// assign gives the material the indicator and returns the material's
// indicator.
func (e env) assign(t *testing.T, material string, indicator map[string]any) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", cycleMaterials, mustJSON(t, map[string]any{"material_id": material, "indicator_id": indicator["id"]}), "")
	return got
}

// refused wants the call refused with the status and code, naming
// wantField when it is not "".
func (e env) refused(t *testing.T, method, path, body, ifMatch string, status int, code, wantField string) {
	t.Helper()
	got, answer, _ := e.Do(method, path, body, ifMatch)
	if got != status || answer["code"] != code {
		t.Errorf("%s %s: got %d %v, want %d %s", method, path, got, answer["code"], status, code)
	}
	if named := fmt.Sprint(field(answer)); wantField != "" && named != wantField {
		t.Errorf("%s %s: field %s, want %s", method, path, named, wantField)
	}
}

// TestCycleCountIndicators: a plant's indicator is created with its code
// and interval, once per code at a plant, read, listed by plant and code,
// and its interval changed at its version; each step audited.
func TestCycleCountIndicators(t *testing.T) {
	e := cycleSetup(t)
	a := e.indicator(t, e.p1, "A", 90)
	if got, want := fmt.Sprintf("%v %v/%v %v %v %v", a["company_code"], a["plant_code"], a["plant_name"], a["code"], a["interval_days"], a["version"]),
		"2000 P1/Unit P1 A 90 1"; got != want {
		t.Errorf("created: got %q, want %q", got, want)
	}
	id := a["id"].(string)
	if au := e.audits(t, id, "inventory.cycle_count_indicator.create"); len(au) != 1 || au[0]["code"] != "A" || au[0]["plant_code"] != "P1" {
		t.Errorf("audit of create: got %v, want one naming A at P1", au)
	}
	e.refused(t, "POST", indicators, mustJSON(t, map[string]any{"plant_id": e.p1, "code": "A", "interval_days": 30}), "", 409,
		"inventory.cycle_count_indicator_exists", "")
	e.indicator(t, e.p2, "A", 30)
	e.indicator(t, e.p1, "Z9", 7)
	for name, c := range map[string]struct {
		body   map[string]any
		status int
		code   string
	}{
		"a storage location as the plant": {map[string]any{"plant_id": e.s1, "code": "B", "interval_days": 30}, 422, "inventory.plant_invalid"},
		"a small letter":                  {map[string]any{"plant_id": e.p1, "code": "b", "interval_days": 30}, 400, "request.invalid"},
		"a code of five":                  {map[string]any{"plant_id": e.p1, "code": "ABCDE", "interval_days": 30}, 400, "request.invalid"},
		"no day":                          {map[string]any{"plant_id": e.p1, "code": "B", "interval_days": 0}, 400, "request.invalid"},
		"past a year":                     {map[string]any{"plant_id": e.p1, "code": "B", "interval_days": 367}, 400, "request.invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			code, got, _ := e.Do("POST", indicators, mustJSON(t, c.body), "")
			if code != c.status || (c.status == 422 && got["code"] != c.code) {
				t.Errorf("got %d %v, want %d %s", code, got["code"], c.status, c.code)
			}
		})
	}
	_, tag := e.Must(200, "GET", indicators+"/"+id, "", "")
	changed, newTag := e.Must(200, "PATCH", indicators+"/"+id, `{"interval_days": 30}`, tag)
	if got, want := fmt.Sprintf("%v %v", changed["interval_days"], changed["version"]), "30 2"; got != want {
		t.Errorf("changed: got %q, want %q", got, want)
	}
	if au := e.audits(t, id, "inventory.cycle_count_indicator.change"); len(au) != 1 || fmt.Sprint(au[0]["before"], au[0]["after"]) !=
		"map[interval_days:90] map[interval_days:30]" {
		t.Errorf("audit of change: got %v, want 90 before and 30 after", au)
	}
	e.refused(t, "PATCH", indicators+"/"+id, `{"interval_days": 30}`, newTag, 422, "inventory.nothing_to_change", "")
	e.refused(t, "PATCH", indicators+"/"+id, `{"interval_days": 60}`, tag, 412, "request.version_mismatch", "")
	e.refused(t, "GET", indicators+"/"+uuid.NewString(), "", "", 404, "request.not_found", "")
	page, _ := e.Must(200, "GET", indicators+"?plant_id="+e.p1.String()+"&code=A", "", "")
	if items := rows(t, page, "items"); len(items) != 1 || items[0]["id"] != id {
		t.Errorf("list by plant and code: got %v, want indicator A of P1", items)
	}
	page, _ = e.Must(200, "GET", indicators+"?page_size=2", "", "")
	if items := rows(t, page, "items"); len(items) != 2 || page["next_page_token"] == nil || items[0]["id"] != id {
		t.Errorf("first page: got %v and token %v, want two, A of P1 first, and a next page", items, page["next_page_token"])
	}
}

// TestCycleCountMaterials: a material is given an indicator of a plant it
// is counted at, one per plant, and another of the plant at its version;
// what a count does not take, and an indicator of another plant, are
// refused.
func TestCycleCountMaterials(t *testing.T) {
	e := cycleSetup(t)
	a, b, other := e.indicator(t, e.p1, "A", 90), e.indicator(t, e.p1, "B", 180), e.indicator(t, e.p2, "A", 90)
	far := e.indicator(t, e.p3, "C", 365)
	got := e.assign(t, e.bracket, a)
	id := got["id"].(string)
	if want := fmt.Sprintf("P1 %s Machined bracket M10 A 90 1", e.number(t, e.bracket)); fmt.Sprintf("%v %v %v %v %v %v", got["plant_code"],
		got["material_number"], got["material_description"], got["indicator_code"], got["interval_days"], got["version"]) != want {
		t.Errorf("created: got %v, want %s", got, want)
	}
	if au := e.audits(t, id, "inventory.cycle_count_material.create"); len(au) != 1 || au[0]["indicator_code"] != "A" {
		t.Errorf("audit of create: got %v, want one naming A", au)
	}
	e.assign(t, e.bracket, other)
	create := func(material string, ind map[string]any) string {
		return mustJSON(t, map[string]any{"material_id": material, "indicator_id": ind["id"]})
	}
	e.refused(t, "POST", cycleMaterials, create(e.bracket, b), "", 409, "inventory.cycle_count_material_exists", "")
	for name, c := range map[string]struct {
		body        string
		code, field string
	}{
		"no indicator":       {mustJSON(t, map[string]any{"material_id": e.bracket, "indicator_id": uuid.New()}), "inventory.indicator_invalid", "indicator_id"},
		"no material":        {create(uuid.NewString(), a), "inventory.material_invalid", "material_id"},
		"a pending material": {create(e.material(t, "Pending bracket", "trading_good", false), a), "inventory.material_not_usable", "material_id"},
		"a service":          {create(e.material(t, "Machining service", "service", true), a), "inventory.material_not_stocked", "material_id"},
		"not at the plant":   {create(e.bracket, far), "inventory.plant_not_extended", "material_id"},
		"in batches":         {create(e.batchManaged(t, "Sealant"), a), "inventory.batch_not_supported", "material_id"},
		"by serial number":   {create(e.serialManaged(t, "Gauge"), a), "inventory.serial_not_supported", "material_id"},
	} {
		t.Run(name, func(t *testing.T) { e.refused(t, "POST", cycleMaterials, c.body, "", 422, c.code, c.field) })
	}
	_, tag := e.Must(200, "GET", cycleMaterials+"/"+id, "", "")
	e.refused(t, "PATCH", cycleMaterials+"/"+id, fmt.Sprintf(`{"indicator_id": %q}`, other["id"]), tag, 422, "inventory.indicator_invalid", "indicator_id")
	e.refused(t, "PATCH", cycleMaterials+"/"+id, fmt.Sprintf(`{"indicator_id": %q}`, a["id"]), tag, 422, "inventory.nothing_to_change", "")
	changed, _ := e.Must(200, "PATCH", cycleMaterials+"/"+id, fmt.Sprintf(`{"indicator_id": %q}`, b["id"]), tag)
	if got, want := fmt.Sprintf("%v %v %v", changed["indicator_code"], changed["interval_days"], changed["version"]), "B 180 2"; got != want {
		t.Errorf("changed: got %q, want %q", got, want)
	}
	if au := e.audits(t, id, "inventory.cycle_count_material.change"); len(au) != 1 || fmt.Sprintf("%v %v", au[0]["before"].(map[string]any)["indicator_code"],
		au[0]["after"].(map[string]any)["indicator_code"]) != "A B" {
		t.Errorf("audit of change: got %v, want A before and B after", au)
	}
	e.refused(t, "PATCH", cycleMaterials+"/"+id, fmt.Sprintf(`{"indicator_id": %q}`, a["id"]), tag, 412, "request.version_mismatch", "")
	e.refused(t, "GET", cycleMaterials+"/"+uuid.NewString(), "", "", 404, "request.not_found", "")
	page, _ := e.Must(200, "GET", cycleMaterials+"?plant_id="+e.p1.String()+"&material_id="+e.bracket+"&indicator_id="+fmt.Sprint(b["id"]), "", "")
	if items := rows(t, page, "items"); len(items) != 1 || items[0]["id"] != id {
		t.Errorf("list by plant, material and indicator: got %v, want the bracket's at P1", items)
	}
}

// dueOn lists the counts due at P1 with the clock days on (through the
// handler, as the API wires it) and returns them as "location material
// code last due" each.
func (e env) dueOn(t *testing.T, days int, params gen.ListCycleCountsDueParams) string {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Inventory(app.Config{Pages: pages, Now: func() time.Time { return time.Now().AddDate(0, 0, days) }})
	params.PlantID = e.p1
	var out []string
	err = e.inRequest(t, func(ctx context.Context) error {
		res, err := h.ListCycleCountsDue(ctx, params)
		if err != nil {
			return err
		}
		for _, d := range res.(*gen.CycleCountDuePage).Items {
			last, due := "never", "now"
			if v, ok := d.LastCountNumber.Get(); ok {
				last = v
			}
			if v, ok := d.DueOn.Get(); ok {
				due = v.Format(time.DateOnly)
			}
			out = append(out, fmt.Sprintf("%s %s %s %s %s", d.StorageLocationCode, d.MaterialNumber, d.IndicatorCode, last, due))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "; ")
}

// TestCycleCountsDue: a material with an indicator is due at each storage
// location it has stock at (none left, none due) where it was never counted, not while a count
// holds it, and again once its interval has passed since its posted
// count; never counted first; a material without one is never due; the
// list pages and filters.
func TestCycleCountsDue(t *testing.T) {
	e := cycleSetup(t)
	bolt := mustMaterial(t, e)
	e.post(t, "561", e.item("10", "EA", nil), e.item("4", "EA", map[string]any{"storage_location_id": e.s2}),
		e.item("5", "EA", map[string]any{"material_id": bolt}))
	a := e.indicator(t, e.p1, "A", 30)
	e.assign(t, e.bracket, a)
	n := e.number(t, e.bracket)
	// Never counted, both sort first, by their stock rows: S1's moved first.
	if got, want := e.dueOn(t, 0, gen.ListCycleCountsDueParams{}), fmt.Sprintf("S1 %[1]s A never now; S2 %[1]s A never now", n); got != want {
		t.Errorf("never counted: got %q, want %q", got, want)
	}
	doc := e.counted(t, []string{e.bracket}, "10")
	if got, want := e.dueOn(t, 0, gen.ListCycleCountsDueParams{}), "S2 "+n+" A never now"; got != want {
		t.Errorf("while a count holds S1: got %q, want %q", got, want)
	}
	posted := e.postDiffs(t, doc, 200)
	counted, err := time.Parse(time.DateOnly, fmt.Sprint(posted["document_date"]))
	if err != nil {
		t.Fatal(err)
	}
	s1 := fmt.Sprintf("S1 %s A %v %s", n, posted["number"], counted.AddDate(0, 0, 30).Format(time.DateOnly))
	for _, c := range []struct {
		days int
		want string
	}{
		{0, "S2 " + n + " A never now"},
		{29, "S2 " + n + " A never now"},
		{30, "S2 " + n + " A never now; " + s1},
	} {
		t.Run(fmt.Sprint(c.days, " days on"), func(t *testing.T) {
			if got := e.dueOn(t, c.days, gen.ListCycleCountsDueParams{}); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	t.Run("filters", func(t *testing.T) {
		if got := e.dueOn(t, 30, gen.ListCycleCountsDueParams{StorageLocationID: gen.NewOptUUID(e.s1)}); got != s1 {
			t.Errorf("at S1: got %q, want %q", got, s1)
		}
		other := e.indicator(t, e.p1, "B", 90)
		if got := e.dueOn(t, 30, gen.ListCycleCountsDueParams{IndicatorID: gen.NewOptUUID(uuid.MustParse(other["id"].(string)))}); got != "" {
			t.Errorf("of another indicator: got %q, want none", got)
		}
	})
	t.Run("through the API", func(t *testing.T) {
		page, _ := e.Must(200, "GET", countsDue+"?plant_id="+e.p1.String(), "", "")
		items := rows(t, page, "items")
		if len(items) != 1 {
			t.Fatalf("today: got %v, want S2's alone", items)
		}
		got := fmt.Sprintf("%v %v/%v %v %v %v %v %v", items[0]["company_code"], items[0]["plant_code"], items[0]["storage_location_code"],
			items[0]["storage_location_name"], items[0]["material_description"], items[0]["interval_days"], items[0]["last_count_id"], items[0]["due_on"])
		if want := "2000 P1/S2 Unit S2 Machined bracket M10 30 <nil> <nil>"; got != want {
			t.Errorf("today: got %q, want %q", got, want)
		}
	})
	t.Run("an interval changed", func(t *testing.T) {
		_, tag := e.Must(200, "GET", indicators+"/"+a["id"].(string), "", "")
		e.Must(200, "PATCH", indicators+"/"+a["id"].(string), `{"interval_days": 60}`, tag)
		if got, want := e.dueOn(t, 30, gen.ListCycleCountsDueParams{}), "S2 "+n+" A never now"; got != want {
			t.Errorf("30 days on at 60: got %q, want %q", got, want)
		}
	})
	t.Run("no stock left", func(t *testing.T) {
		e.post(t, "201", e.item("4", "EA", map[string]any{"storage_location_id": e.s2, "cost_center_id": e.c1}))
		if got := e.dueOn(t, 30, gen.ListCycleCountsDueParams{}); got != "" {
			t.Errorf("S2 emptied: got %q, want none", got)
		}
	})
	e.refused(t, "GET", countsDue+"?plant_id="+e.s1.String(), "", "", 422, "inventory.plant_invalid", "plant_id")
}

// TestCycleCountsDueLastCount: the last count is the latest posted one
// by its date, and a count whose differences were rejected is none.
func TestCycleCountsDueLastCount(t *testing.T) {
	e := cycleSetup(t)
	h := endpoints(t)
	bolt := mustMaterial(t, e)
	e.post(t, "561", e.item("10", "EA", nil), e.item("50", "EA", map[string]any{"material_id": bolt}))
	a := e.indicator(t, e.p1, "A", 30)
	e.assign(t, e.bracket, a)
	e.assign(t, bolt, a)
	today := time.Now()
	dated := func(id string, days int) {
		t.Helper()
		c := pgtest.Admin(t)
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), `UPDATE inventory.count_document SET document_date = $2, version = version + 1 WHERE id = $1`, id,
			today.AddDate(0, 0, -days).Format(time.DateOnly)); err != nil {
			t.Fatal(err)
		}
	}
	older := e.counted(t, []string{e.bracket}, "10")
	e.postDiffs(t, older, 200)
	dated(older, 40)
	later := e.counted(t, []string{e.bracket}, "10")
	posted := e.postDiffs(t, later, 200)
	dated(later, 5)
	// The bolt's 50 missing at 1.00 are above the tolerance of 10.00; rejected, nothing is posted.
	rejected := e.counted(t, []string{bolt}, "0")
	if got := e.postDiffs(t, rejected, 200); got["status"] != "pending_approval" {
		t.Fatalf("the bolt's count: got %v, want pending_approval", got["status"])
	}
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return inventory.Approvals{Endpoints: h}.Rejected(ctx, uuid.MustParse(rejected), uuid.Nil, "")
	}); err != nil {
		t.Fatal(err)
	}
	b := e.number(t, bolt)
	if got, want := e.dueOn(t, 0, gen.ListCycleCountsDueParams{}), "S1 "+b+" A never now"; got != want {
		t.Errorf("today: got %q, want %q: the bracket was counted 5 days ago, the bolt's count rejected", got, want)
	}
	due := today.AddDate(0, 0, 25).Format(time.DateOnly)
	if got, want := e.dueOn(t, 25, gen.ListCycleCountsDueParams{}), fmt.Sprintf("S1 %s A never now; S1 %s A %v %s", b, e.number(t, e.bracket),
		posted["number"], due); got != want {
		t.Errorf("25 days on: got %q, want %q", got, want)
	}
}

// TestCycleCountsDuePages: the counts due page by the day they fell due,
// never counted first, across a page boundary of each kind.
func TestCycleCountsDuePages(t *testing.T) {
	e := cycleSetup(t)
	a := e.indicator(t, e.p1, "A", 1)
	bolt := mustMaterial(t, e)
	e.post(t, "561", e.item("10", "EA", nil), e.item("5", "EA", map[string]any{"material_id": bolt}),
		e.item("4", "EA", map[string]any{"storage_location_id": e.s2}))
	e.assign(t, e.bracket, a)
	e.assign(t, bolt, a)
	e.postDiffs(t, e.counted(t, []string{e.bracket, bolt}, "10", "5"), 200)
	var seen []string
	for token := ""; ; {
		params := gen.ListCycleCountsDueParams{PlantID: e.p1, PageSize: gen.NewOptInt32(1)}
		if token != "" {
			params.PageToken = gen.NewOptString(token)
		}
		pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
		if err != nil {
			t.Fatal(err)
		}
		h := app.Inventory(app.Config{Pages: pages, Now: func() time.Time { return time.Now().AddDate(0, 0, 2) }})
		var page *gen.CycleCountDuePage
		if err := e.inRequest(t, func(ctx context.Context) error {
			res, err := h.ListCycleCountsDue(ctx, params)
			page, _ = res.(*gen.CycleCountDuePage)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, d := range page.Items {
			seen = append(seen, fmt.Sprintf("%s %v", d.StorageLocationCode, d.LastCountID.Set))
		}
		if !page.NextPageToken.Set {
			break
		}
		token = page.NextPageToken.Value
	}
	if got, want := strings.Join(seen, ", "), "S2 false, S1 true, S1 true"; got != want {
		t.Errorf("pages: got %q, want %q", got, want)
	}
}

// TestCycleCountLocks: a change of an indicator's interval holds its row
// FOR NO KEY UPDATE, and a material given the indicator or moved to it
// takes its key share through the foreign key: neither waits for the
// other, in either order.
func TestCycleCountLocks(t *testing.T) {
	e := cycleSetup(t)
	h := endpoints(t)
	a, b := e.indicator(t, e.p1, "A", 90), e.indicator(t, e.p1, "B", 30)
	ia, ib := uuid.MustParse(a["id"].(string)), uuid.MustParse(b["id"].(string))
	moved := e.assign(t, mustMaterial(t, e), a)
	interval := func(days int32) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.ChangeIndicator(ctx, ib, func(int32) error { return nil }, days)
			return err
		}
	}
	give := func(ctx context.Context) error {
		_, err := h.CreateCycleMaterial(ctx, inventory.CycleMaterialInput{MaterialID: uuid.MustParse(e.bracket), IndicatorID: ib})
		return err
	}
	move := func(to uuid.UUID) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.ChangeCycleMaterial(ctx, uuid.MustParse(moved["id"].(string)), func(int32) error { return nil }, to)
			return err
		}
	}
	for _, c := range []struct {
		name          string
		first, second func(ctx context.Context) error
	}{
		{"an interval changed, then a material given it", interval(45), give},
		{"a material moved to it, then its interval changed", move(ib), interval(60)},
		{"an interval changed, then a material moved off it", interval(15), move(ia)},
	} {
		t.Run(c.name, func(t *testing.T) { e.notWaiting(t, c.first, c.second) })
	}
}

// notWaiting runs first in a transaction it holds open while second runs
// in another, and wants second to finish without waiting for it.
func (e env) notWaiting(t *testing.T, first, second func(ctx context.Context) error) {
	t.Helper()
	held, release := make(chan struct{}), make(chan struct{})
	letGo := sync.OnceFunc(func() { close(release) })
	defer letGo()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			if err := first(ctx); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-firstDone:
		t.Fatalf("first: got %v, want it held open", err)
	}
	_, secondDone := apitest.Started(t, e.Tenant, e.Tenant.Human, second)
	var secondErr error
	pgtest.Eventually(t, 10*time.Second, func() bool {
		select {
		case secondErr = <-secondDone:
			return true
		default:
			return false
		}
	}, "the second transaction waited for the first")
	letGo()
	if err := <-firstDone; err != nil || secondErr != nil {
		t.Errorf("got %v and %v, want both done", err, secondErr)
	}
}

// TestCycleCountDatabaseFailures: every database error of the cycle
// counting operations is answered as an error, none outside a unit of
// work.
func TestCycleCountDatabaseFailures(t *testing.T) {
	e := cycleSetup(t)
	h := endpoints(t)
	e.post(t, "561", e.item("10", "EA", nil))
	a := e.indicator(t, e.p1, "A", 90)
	ind := uuid.MustParse(a["id"].(string))
	_, indTag := e.Must(200, "GET", indicators+"/"+ind.String(), "", "")
	b := e.indicator(t, e.p1, "B", 30)
	cm := e.assign(t, e.bracket, a)
	cmID := uuid.MustParse(cm["id"].(string))
	_, cmTag := e.Must(200, "GET", cycleMaterials+"/"+cmID.String(), "", "")
	other := mustMaterial(t, e)
	calls := map[string]func(ctx context.Context) error{
		"create an indicator": func(ctx context.Context) error {
			_, err := h.CreateCycleCountIndicator(ctx, &gen.CycleCountIndicatorCreate{PlantID: e.p1, Code: "C", IntervalDays: 365},
				gen.CreateCycleCountIndicatorParams{})
			return err
		},
		"get an indicator": func(ctx context.Context) error {
			_, err := h.GetCycleCountIndicator(ctx, gen.GetCycleCountIndicatorParams{ID: ind})
			return err
		},
		"change an indicator": func(ctx context.Context) error {
			_, err := h.UpdateCycleCountIndicator(ctx, &gen.CycleCountIndicatorUpdate{IntervalDays: gen.NewOptInt32(60)},
				gen.UpdateCycleCountIndicatorParams{ID: ind, IfMatch: indTag})
			return err
		},
		"list indicators": func(ctx context.Context) error {
			_, err := h.ListCycleCountIndicators(ctx, gen.ListCycleCountIndicatorsParams{})
			return err
		},
		"give a material an indicator": func(ctx context.Context) error {
			_, err := h.CreateCycleCountMaterial(ctx, &gen.CycleCountMaterialCreate{MaterialID: uuid.MustParse(other), IndicatorID: ind},
				gen.CreateCycleCountMaterialParams{})
			return err
		},
		"get a material's indicator": func(ctx context.Context) error {
			_, err := h.GetCycleCountMaterial(ctx, gen.GetCycleCountMaterialParams{ID: cmID})
			return err
		},
		"change a material's indicator": func(ctx context.Context) error {
			_, err := h.UpdateCycleCountMaterial(ctx, &gen.CycleCountMaterialUpdate{IndicatorID: gen.NewOptUUID(uuid.MustParse(b["id"].(string)))},
				gen.UpdateCycleCountMaterialParams{ID: cmID, IfMatch: cmTag})
			return err
		},
		"list materials' indicators": func(ctx context.Context) error {
			_, err := h.ListCycleCountMaterials(ctx, gen.ListCycleCountMaterialsParams{})
			return err
		},
		"list the counts due": func(ctx context.Context) error {
			_, err := h.ListCycleCountsDue(ctx, gen.ListCycleCountsDueParams{PlantID: e.p1})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	for name, call := range map[string]func(ctx context.Context) error{
		"indicators": func(ctx context.Context) error {
			_, err := h.ListCycleCountIndicators(ctx, gen.ListCycleCountIndicatorsParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"materials": func(ctx context.Context) error {
			_, err := h.ListCycleCountMaterials(ctx, gen.ListCycleCountMaterialsParams{PageSize: gen.NewOptInt32(0)})
			return err
		},
		"due": func(ctx context.Context) error {
			_, err := h.ListCycleCountsDue(ctx, gen.ListCycleCountsDueParams{PlantID: e.p1, PageSize: gen.NewOptInt32(0)})
			return err
		},
	} {
		t.Run("page size of "+name, func(t *testing.T) {
			if p := problem.From(e.inRequest(t, call)); p.Code != "request.invalid_page_size" {
				t.Errorf("got %v, want request.invalid_page_size", p)
			}
		})
	}
}
