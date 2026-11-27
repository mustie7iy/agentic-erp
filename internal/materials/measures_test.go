package materials_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// GTINs with their GS1 check digits (internal/platform/gtin's tests show
// the sums): the bracket's, its GTIN-13 form, the box's and a third.
const (
	eachGTIN  = "036000291452"
	eachGTIN0 = "0036000291452"
	boxGTIN   = "10036000291459"
	otherGTIN = "4006381333931"
)

// eachMeasures are the bracket's measures as a request gives them, a
// trailing zero included.
func eachMeasures() map[string]any {
	return map[string]any{"gtin": eachGTIN, "gross_weight": "0.120", "net_weight": "0.11", "weight_unit": "KG"}
}

// boxMeasures are the box of 20's.
func boxMeasures(gtin string) map[string]any {
	return map[string]any{"gtin": gtin, "gross_weight": "2.5", "weight_unit": "KG", "volume": "0.012", "volume_unit": "M3", "length": "0.3",
		"width": "0.2", "height": "0.20", "dimension_unit": "M"}
}

// box is the box of 20 with its measures, the request's form.
func box(measures map[string]any) []any {
	return []any{map[string]any{"unit": "BOX", "numerator": 20, "denominator": 1, "measures": measures}}
}

// measured is a material's or unit's measures as "field=value" in the
// contract's order, the unit names included.
func measured(m any) string {
	got, _ := m.(map[string]any)
	out := ""
	for _, f := range []string{"gtin", "gross_weight", "net_weight", "weight_unit", "weight_unit_name", "volume", "volume_unit", "volume_unit_name",
		"length", "width", "height", "dimension_unit", "dimension_unit_name"} {
		if v, ok := got[f]; ok {
			out += fmt.Sprintf("%s=%v ", f, v)
		}
	}
	return out
}

const (
	eachRead = "gtin=036000291452 gross_weight=0.12 net_weight=0.11 weight_unit=KG weight_unit_name=Kilogram "
	boxRead  = "gtin=10036000291459 gross_weight=2.5 weight_unit=KG weight_unit_name=Kilogram volume=0.012 volume_unit=M3 " +
		"volume_unit_name=Cubic metre length=0.3 width=0.2 height=0.2 dimension_unit=M dimension_unit_name=Metre "
)

// TestMeasures: a material created with its base unit's and its box's
// dimensions, weights and GTINs gives them back, without trailing zeros
// and with the units' names; its create change, audit and MaterialChanged
// carry them; a list finds it by either GTIN, the GTIN-12 by its GTIN-13
// form too, and by no other.
func TestMeasures(t *testing.T) {
	e := setup(t)
	m := e.create(t, map[string]any{"base_unit_measures": eachMeasures(), "alternative_units": box(boxMeasures(boxGTIN))})
	id := m["id"].(string)
	if got := measured(m["base_unit_measures"]); got != eachRead {
		t.Errorf("base unit: got %q, want %q", got, eachRead)
	}
	if got := measured(rows(t, m, "alternative_units")[0]["measures"]); got != boxRead {
		t.Errorf("box: got %q, want %q", got, boxRead)
	}
	plain := e.create(t, map[string]any{"description": "Copper wire", "alternative_units": nil})
	if got := measured(plain["base_unit_measures"]); got != "" {
		t.Errorf("a material without measures: got %q, want none", got)
	}
	c, _ := e.Must(200, "GET", "/v1/material-changes/"+m["pending_change_id"].(string), "", "")
	req := c["requested"].(map[string]any)
	if got, want := measured(req["base_unit_measures"]), "gtin=036000291452 gross_weight=0.12 net_weight=0.11 weight_unit=KG "; got != want {
		t.Errorf("the create change's base unit: got %q, want %q", got, want)
	}
	if got := measured(rows(t, req, "alternative_units")[0]["measures"]); got == "" {
		t.Error("the create change's box: got no measures, want them")
	}
	audit := e.audits(t, id, "materials.material.create")[0]
	if got, want := fmt.Sprint(audit["measures"].(map[string]any)["EA"]), "map[gross_weight:0.12 gtin:036000291452 net_weight:0.11 weight_unit:KG]"; got != want {
		t.Errorf("audit: got %s, want %s", got, want)
	}
	event := e.events(t, "material_changed", id)[0]
	if got, want := measured(event["base_unit_measures"]), "gtin=036000291452 gross_weight=0.12 net_weight=0.11 weight_unit=KG "; got != want {
		t.Errorf("event: got %q, want %q", got, want)
	}
	for gtin, want := range map[string]int{eachGTIN: 1, eachGTIN0: 1, boxGTIN: 1, otherGTIN: 0} {
		t.Run("listed by "+gtin, func(t *testing.T) {
			page, _ := e.Must(200, "GET", path+"?gtin="+gtin, "", "")
			if got := len(rows(t, page, "items")); got != want {
				t.Errorf("got %d, want %d", got, want)
			}
		})
	}
	e.Problem(400, "request.invalid_parameter", "GET", path+"?gtin=03600029145", "", "")
}

// TestMeasuresChanged: a change of the base unit's measures and of the
// box's waits for its approval and is audited before and after; approved,
// it replaces them, a GTIN moved from the base unit to the box in one
// change. The same measures written otherwise ("2.50") change nothing; an
// empty object removes the base unit's, and removing the box removes its
// measures, so its GTIN is free for another material.
func TestMeasuresChanged(t *testing.T) {
	e := setup(t)
	id := e.active(t, map[string]any{"base_unit_measures": eachMeasures(), "alternative_units": box(boxMeasures(boxGTIN))})["id"].(string)
	c := e.change(t, id, mustJSON(t, map[string]any{"base_unit_measures": map[string]any{"gross_weight": "0.13", "weight_unit": "KG"},
		"alternative_units": box(boxMeasures(eachGTIN))}))
	audit := e.audits(t, c["id"].(string), "materials.change.request")[0]
	fields(t, audit, map[string]any{"fields": "[alternative_units base_unit_measures]"})
	fields(t, audit["after"].(map[string]any), map[string]any{"base_unit_measures": "map[gross_weight:0.13 weight_unit:KG]"})
	if got := fmt.Sprint(audit["after"].(map[string]any)["alternative_unit_measures"].(map[string]any)["BOX"].(map[string]any)["gtin"]); got != eachGTIN {
		t.Errorf("audit after: box GTIN %s, want %s", got, eachGTIN)
	}
	waiting, _ := e.Must(200, "GET", path+"/"+id, "", "")
	if got := measured(waiting["base_unit_measures"]); got != eachRead {
		t.Errorf("waiting: got %q, want it unchanged", got)
	}
	e.Approve(t, c["id"].(string))
	got, tag := e.Must(200, "GET", path+"/"+id, "", "")
	if g, want := measured(got["base_unit_measures"]), "gross_weight=0.13 weight_unit=KG weight_unit_name=Kilogram "; g != want {
		t.Errorf("approved base unit: got %q, want %q", g, want)
	}
	if g := fmt.Sprint(rows(t, got, "alternative_units")[0]["measures"].(map[string]any)["gtin"]); g != eachGTIN {
		t.Errorf("approved box GTIN: got %s, want %s", g, eachGTIN)
	}
	e.Problem(422, "materials.nothing_to_change", "PATCH", path+"/"+id, mustJSON(t, map[string]any{"base_unit_measures": map[string]any{
		"gross_weight": "0.130", "weight_unit": "KG"}}), tag)
	status, refused, _ := e.Do("PATCH", path+"/"+id, mustJSON(t, map[string]any{"base_unit_measures": map[string]any{"gtin": eachGTIN0}}), tag)
	if status != 422 || refused["code"] != "materials.duplicate_gtin" || field(refused) != "base_unit_measures.gtin" {
		t.Errorf("the box's GTIN for the base unit: got %d %v %v, want 422 materials.duplicate_gtin on base_unit_measures.gtin", status, refused["code"],
			field(refused))
	}
	cleared := e.changed(t, id, `{"base_unit_measures": {}, "alternative_units": []}`)
	if g := measured(cleared["base_unit_measures"]); g != "" {
		t.Errorf("cleared: got %q, want none", g)
	}
	var live int
	pgtest.Scan(t, `SELECT count(*) FROM materials.unit_measure WHERE tenant_id = $1 AND material_id = $2 AND removed_at IS NULL`, []any{e.Tenant.ID, id}, &live)
	if live != 0 {
		t.Errorf("measures left: got %d, want none", live)
	}
	e.create(t, map[string]any{"description": "Copper wire", "base_unit_measures": map[string]any{"gtin": eachGTIN}, "alternative_units": nil})
}

// TestGTINsAcrossMaterials: a GTIN another material's unit carries is
// refused, a blocked material's too, named with that material, on a
// creation and on a change; a material whose creation is rejected frees
// its GTIN; of two changes asking for one GTIN, the second's approval is
// refused once the first is applied, and the material stays as it was.
func TestGTINsAcrossMaterials(t *testing.T) {
	e := setup(t)
	first := e.active(t, map[string]any{"base_unit_measures": map[string]any{"gtin": eachGTIN}, "alternative_units": nil})
	_, tag := e.Must(200, "GET", path+"/"+first["id"].(string), "", "")
	e.Must(200, "PATCH", path+"/"+first["id"].(string), `{"status": "blocked"}`, tag) // a block applies at once
	e.Problem(409, "materials.gtin_taken", "POST", path, e.bracket(t, map[string]any{"description": "Copper wire",
		"base_unit_measures": map[string]any{"gtin": eachGTIN0}}), "")
	rejected := e.create(t, map[string]any{"description": "Copper wire", "base_unit_measures": map[string]any{"gtin": otherGTIN}})
	r := e.Pending(t, rejected["pending_change_id"].(string))
	e.As(e.Approver).Must(200, "POST", "/v1/approval-requests/"+r["id"].(string)+":reject", `{"comment": "Not ours"}`, `"1"`)
	gone, goneTag := e.Must(200, "GET", path+"/"+rejected["id"].(string), "", "")
	if got := measured(gone["base_unit_measures"]); got != "" || gone["version"] != 2.0 || goneTag != `"2"` {
		t.Errorf("the rejected material: got measures %q, version %v, ETag %s, want none at version 2", got, gone["version"], goneTag)
	}
	released := e.events(t, "material_changed", rejected["id"].(string))
	if last := released[len(released)-1]; last["change"] != "changed" || last["version"] != 2.0 || measured(last["base_unit_measures"]) != "" {
		t.Errorf("the rejected material's last event: got %v, want changed at version 2 without measures", last)
	}
	if got := e.audits(t, rejected["id"].(string), "materials.material.release"); len(got) != 1 {
		t.Errorf("the release's audit records: got %d, want 1", len(got))
	}
	second := e.active(t, map[string]any{"description": "Brass wire", "base_unit_measures": map[string]any{"gtin": otherGTIN}})
	third := e.active(t, map[string]any{"description": "Steel wire", "alternative_units": nil})
	_, tag = e.Must(200, "GET", path+"/"+third["id"].(string), "", "")
	e.Problem(409, "materials.gtin_taken", "PATCH", path+"/"+third["id"].(string), mustJSON(t, map[string]any{
		"base_unit_measures": map[string]any{"gtin": otherGTIN}}), tag)
	ask := mustJSON(t, map[string]any{"alternative_units": box(map[string]any{"gtin": boxGTIN})})
	a := e.change(t, second["id"].(string), ask)
	b := e.change(t, third["id"].(string), ask)
	e.Approve(t, a["id"].(string))
	pending := e.Pending(t, b["id"].(string))
	refused := false
	for _, approver := range []pgtest.Actor{e.Approver, e.Approver2} {
		status, got, _ := e.As(approver).Do("POST", "/v1/approval-requests/"+pending["id"].(string)+":approve", `{"comment": "ok"}`,
			fmt.Sprintf(`"%v"`, pending["version"]))
		if status == 409 {
			fields(t, got, map[string]any{"code": "materials.gtin_taken"})
			refused = true
			break
		}
		pending = e.Pending(t, b["id"].(string))
	}
	if !refused {
		t.Error("the second change's approval: got no refusal, want 409 materials.gtin_taken")
	}
	after, _ := e.Must(200, "GET", path+"/"+third["id"].(string), "", "")
	if got := units(t, after); got != "" {
		t.Errorf("the material whose change was refused: got units %q, want none", got)
	}
}

// TestMeasureRefusals: values and units a unit cannot carry, a GTIN that
// is none and one given to two units are refused naming the field, on a
// creation and on a change; a value or GTIN of another form is a bad
// request.
func TestMeasureRefusals(t *testing.T) {
	e := setup(t)
	id := e.active(t, nil)["id"].(string)
	for name, c := range map[string]struct {
		base   map[string]any
		box    map[string]any
		status int
		code   string
		field  string
	}{
		"a weight of zero":       {map[string]any{"gross_weight": "0.000", "weight_unit": "KG"}, nil, 422, "materials.invalid_measures", "base_unit_measures.gross_weight"},
		"a weight without unit":  {map[string]any{"net_weight": "1"}, nil, 422, "materials.invalid_measures", "base_unit_measures.weight_unit"},
		"a unit without weight":  {map[string]any{"weight_unit": "KG", "gtin": eachGTIN}, nil, 422, "materials.invalid_measures", "base_unit_measures.weight_unit"},
		"an unknown weight unit": {map[string]any{"gross_weight": "1", "weight_unit": "LB"}, nil, 422, "materials.unknown_unit", "base_unit_measures.weight_unit"},
		"a weight in litres":     {map[string]any{"gross_weight": "1", "weight_unit": "L"}, nil, 422, "materials.invalid_measures", "base_unit_measures.weight_unit"},
		"a volume in metres":     {nil, map[string]any{"volume": "1", "volume_unit": "M"}, 422, "materials.invalid_measures", "alternative_units[0].measures.volume_unit"},
		"a height without unit":  {nil, map[string]any{"height": "1"}, 422, "materials.invalid_measures", "alternative_units[0].measures.dimension_unit"},
		"a length in kilograms":  {nil, map[string]any{"length": "1", "dimension_unit": "KG"}, 422, "materials.invalid_measures", "alternative_units[0].measures.dimension_unit"},
		"a net above the gross":  {map[string]any{"gross_weight": "1", "net_weight": "1.001", "weight_unit": "KG"}, nil, 422, "materials.invalid_measures", "base_unit_measures.net_weight"},
		"a check digit wrong":    {map[string]any{"gtin": "036000291453"}, nil, 422, "materials.invalid_gtin", "base_unit_measures.gtin"},
		"one GTIN on two units":  {map[string]any{"gtin": eachGTIN}, map[string]any{"gtin": eachGTIN0}, 422, "materials.duplicate_gtin", "alternative_units[0].measures.gtin"},
		"four decimals":          {map[string]any{"gross_weight": "1.0001", "weight_unit": "KG"}, nil, 400, "request.invalid_body", ""},
		"a weight as a number":   {map[string]any{"gross_weight": 1, "weight_unit": "KG"}, nil, 400, "request.invalid_body", ""},
		"eleven digits":          {map[string]any{"gtin": "03600029145"}, nil, 400, "request.invalid_body", ""},
		"an unknown field":       {map[string]any{"colour": "red"}, nil, 400, "request.invalid_body", ""},
	} {
		over := map[string]any{"description": "Machined bracket M12", "base_unit_measures": c.base, "alternative_units": box(c.box)}
		change := map[string]any{"base_unit_measures": c.base, "alternative_units": box(c.box)}
		if c.base == nil {
			over["base_unit_measures"] = nil
			delete(change, "base_unit_measures")
		}
		if c.box == nil {
			over["alternative_units"] = nil
			delete(change, "alternative_units")
		}
		t.Run("create "+name, func(t *testing.T) {
			status, got, _ := e.Do("POST", path, e.bracket(t, over), "")
			if status != c.status || got["code"] != c.code {
				t.Fatalf("got %d %v, want %d %s", status, got["code"], c.status, c.code)
			}
			if c.field != "" && field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
		})
		t.Run("change "+name, func(t *testing.T) {
			status, got, _ := e.Do("PATCH", path+"/"+id, mustJSON(t, change), `"2"`)
			if status != c.status || got["code"] != c.code {
				t.Fatalf("got %d %v, want %d %s", status, got["code"], c.status, c.code)
			}
			if c.field != "" && field(got) != c.field {
				t.Errorf("field: got %v, want %s", field(got), c.field)
			}
		})
	}
}

// withCheck is the 12-digit payload with its GS1 check digit, a GTIN-13:
// from the right, the digits weighted 3, 1, 3, …
func withCheck(payload string) string {
	sum := 0
	for i := len(payload) - 1; i >= 0; i-- {
		d := int(payload[i] - '0')
		if (len(payload)-1-i)%2 == 0 {
			d *= 3
		}
		sum += d
	}
	return fmt.Sprintf("%s%d", payload, (10-sum%10)%10)
}

// TestGTINsInOppositeOrder (ADR-0008): of two transactions writing GTINs
// X and Y in opposite orders, neither waits for a GTIN while it holds the
// other. The first applies material C's change taking X and stays open;
// the second applies B's taking Y and X and waits; the first then applies
// D's taking Y, which the second, waiting for X's lock, holds nothing of:
// the first commits, and the second is refused 409 materials.gtin_taken.
// Writing Y before waiting for X, the second would hold Y while the first
// waits for it: a deadlock.
func TestGTINsInOppositeOrder(t *testing.T) {
	e := setup(t)
	decide := materials.Approvals{Endpoints: endpoints(t)}
	x, y := withCheck("400000000001"), withCheck("400000000002") // X before Y in key order
	ask := func(description, base, box string) uuid.UUID {
		over := map[string]any{"base_unit_measures": map[string]any{"gtin": base}}
		if box != "" {
			over["alternative_units"] = []any{map[string]any{"unit": "BOX", "numerator": 20, "denominator": 1, "measures": map[string]any{"gtin": box}}}
		}
		id := e.active(t, map[string]any{"description": description})["id"].(string)
		return uuid.MustParse(e.change(t, id, mustJSON(t, over))["id"].(string))
	}
	c, b, d := ask("Washer", x, ""), ask("Spacer", y, x), ask("Shim", y, "")
	ctx := context.Background()
	begin := func() pgx.Tx {
		tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app", TenantID: e.Tenant.ID})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) }) // after a commit, a no-op
		return tx
	}
	p := reqctx.Principal{ID: e.Tenant.Human.ID, Kind: e.Tenant.Human.Kind, TenantID: e.Tenant.ID}
	first, second := begin(), begin()
	var pid int32
	if err := second.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	applied, waiting := make(chan struct{}), make(chan struct{})
	run := func(tx pgx.Tx, fn func(ctx context.Context) error) chan error {
		done := make(chan error, 1)
		go func() {
			err := uow.Act(ctx, tx, p, nil, fn)
			if err == nil {
				err = tx.Commit(ctx)
			}
			done <- err
		}()
		return done
	}
	firstDone := run(first, func(ctx context.Context) error {
		if err := decide.Approved(ctx, c, uuid.Nil); err != nil {
			return err
		}
		close(applied)
		<-waiting
		return decide.Approved(ctx, d, uuid.Nil)
	})
	<-applied
	secondDone := run(second, func(ctx context.Context) error { return decide.Approved(ctx, b, uuid.Nil) })
	pgtest.Eventually(t, 30*time.Second, func() bool {
		var w bool
		pgtest.Scan(t, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock')`, []any{pid}, &w)
		return w
	}, "the second transaction did not wait for the first's GTIN")
	close(waiting)
	if err := <-firstDone; err != nil {
		t.Errorf("the first transaction: got %v, want C's and D's changes applied", err)
	}
	if err := <-secondDone; problem.From(err).Code != "materials.gtin_taken" {
		t.Errorf("the second transaction: got %v, want 409 materials.gtin_taken", err)
	}
}
