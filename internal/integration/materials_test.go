package integration_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// materialSetup is a tenant whose people may also read materials and
// read and manage org units, and whose API reads extracts from a
// directory of its own.
func materialSetup(t *testing.T) env {
	t.Helper()
	f := fixture.New(t, append(slices.Clone(perms), "materials.material.read", "materials.material.write", "tenancy.org.read", "tenancy.org.manage")...)
	dir := t.TempDir()
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir })
	return env{Env: f, dir: dir}
}

// withMaterials is tiny with company 2000's plants P1 (storage locations
// S1, S2) and P2 (S3) and its material master (S2o): steel sheet M-1 by
// the kilogram and the tonne at P1/S1, described in German and English;
// shelving M-2, described in German alone, by the box of 4 written with
// decimals, at P1/S2 and P2/S3, flagged for deletion at P1/S1; and materials the load does not create:
// M-3 whose base unit ST is none of the platform's, M-4 with an
// alternative unit CAR that is none and a box of 0, M-5 of type ZZZ and
// group "lower", M-6 flagged for deletion, M-7 flagged at its one plant
// and at a storage location, M-8 without a description and M-9 whose
// base unit converts to itself 2/1 and lists its box twice.
func withMaterials() *sap.Extract {
	x := tiny()
	x.T001W = []sap.T001W{{WERKS: "P2", NAME1: "Plant two", BWKEY: "P2", BUKRS: "2000"}, {WERKS: "P1", NAME1: "Plant one", BWKEY: "P1", BUKRS: "2000"}}
	x.T001L = []sap.T001L{{WERKS: "P1", LGORT: "S2", LGOBE: "Store two"}, {WERKS: "P1", LGORT: "S1", LGOBE: "Store one"}, {WERKS: "P2", LGORT: "S3", LGOBE: "Store three"}}
	material := func(matnr, mtart, matkl, meins string, units [][3]string, plants ...string) {
		x.MARA = append(x.MARA, sap.MARA{MATNR: matnr, MTART: mtart, MATKL: matkl, MEINS: meins})
		x.MARM = append(x.MARM, sap.MARM{MATNR: matnr, MEINH: meins, UMREZ: "1", UMREN: "1"})
		for _, u := range units {
			x.MARM = append(x.MARM, sap.MARM{MATNR: matnr, MEINH: u[0], UMREZ: u[1], UMREN: u[2]})
		}
		for _, p := range plants {
			werks, lgort, _ := strings.Cut(p, "/")
			if !slices.ContainsFunc(x.MARC, func(c sap.MARC) bool { return c.MATNR == matnr && c.WERKS == werks }) {
				x.MARC = append(x.MARC, sap.MARC{MATNR: matnr, WERKS: werks})
			}
			x.MARD = append(x.MARD, sap.MARD{MATNR: matnr, WERKS: werks, LGORT: lgort})
		}
		if matnr != "M-8" {
			x.MAKT = append(x.MAKT, sap.MAKT{MATNR: matnr, SPRAS: "E", MAKTX: "Material " + matnr})
		}
	}
	material("M-1", "ROH", "STEEL", "KG", [][3]string{{"T", "1000", "1"}}, "P1/S1")
	x.MAKT = append(x.MAKT, sap.MAKT{MATNR: "M-1", SPRAS: "D", MAKTX: "Stahlblech"})
	material("M-2", "FERT", "SHELVING", "EA", [][3]string{{"BOX", "4.000", "1"}}, "P2/S3", "P1/S2", "P1/S1")
	x.MAKT[len(x.MAKT)-1].SPRAS = "D"
	x.MARD[len(x.MARD)-1].LVORM = sap.Deleted
	material("M-3", "HAWA", "FASTENERS", "ST", nil, "P2/S3")
	material("M-4", "HAWA", "FASTENERS", "EA", [][3]string{{"CAR", "500", "1"}, {"BOX", "0", "1"}}, "P2/S3")
	material("M-5", "ZZZ", "lower", "EA", nil, "P2/S3")
	material("M-6", "ROH", "RUBBER", "EA", nil, "P1/S1")
	x.MARA[len(x.MARA)-1].LVORM = sap.Deleted
	material("M-7", "ROH", "RUBBER", "EA", nil, "P1/S1")
	x.MARC[len(x.MARC)-1].LVORM = sap.Deleted
	x.MARD[len(x.MARD)-1].LVORM = sap.Deleted
	material("M-8", "ROH", "RUBBER", "EA", nil, "P1/S1")
	material("M-9", "ROH", "RUBBER", "EA", [][3]string{{"BOX", "2", "1"}, {"BOX", "3", "1"}}, "P1/S1")
	x.MARM = append(x.MARM, sap.MARM{MATNR: "M-9", MEINH: "EA", UMREZ: "2", UMREN: "1"})
	x.MARM = slices.DeleteFunc(x.MARM, func(u sap.MARM) bool { return u.MATNR == "M-9" && u.UMREZ == "1" })
	return x
}

// materialList is the tenant's materials as "number description type
// group base unit status, plants, units", by description.
func (e env) materialList(t *testing.T) []string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/materials?page_size=50", "", "")
	var out []string
	for _, it := range page["items"].([]any) {
		m := it.(map[string]any)
		var plants, units []string
		for _, p := range m["plants"].([]any) {
			pm := p.(map[string]any)
			var codes []string
			for _, s := range pm["storage_locations"].([]any) {
				codes = append(codes, s.(map[string]any)["code"].(string))
			}
			plants = append(plants, fmt.Sprintf("%v(%s)", pm["plant_code"], strings.Join(codes, ",")))
		}
		for _, u := range m["alternative_units"].([]any) {
			um := u.(map[string]any)
			units = append(units, fmt.Sprintf("%v %v/%v", um["unit"], um["numerator"], um["denominator"]))
		}
		out = append(out, fmt.Sprintf("%v %v %v %v %v, %s, %s", m["description"], m["material_type"], m["material_group"], m["base_unit"], m["status"],
			strings.Join(plants, " "), strings.Join(units, " ")))
	}
	slices.Sort(out)
	return out
}

// quality is the run's quality findings as "severity table key: message".
func quality(got map[string]any) string {
	var out []string
	for _, q := range report(got)["quality"].([]any) {
		m := q.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %v: %v", m["severity"], m["table"], m["key"], m["message"]))
	}
	return strings.Join(out, "\n")
}

// expectFields checks each field of want in got, one message each.
func expectFields(t *testing.T, what string, got, want map[string]any) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s %s: got %v, want %v", what, k, got[k], w)
		}
	}
}

// loadIssues is the run's load refusals as "table key: message".
func loadIssues(got map[string]any) []string {
	var out []string
	for _, i := range report(got)["load"].([]any) {
		m := i.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v: %v", m["table"], m["key"], m["message"]))
	}
	return out
}

// materialID is the id of the tenant's material with the description.
func (e env) materialID(t *testing.T, description string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/materials?page_size=50", "", "")
	for _, it := range page["items"].([]any) {
		if m := it.(map[string]any); m["description"] == description {
			return m["id"].(string)
		}
	}
	t.Fatalf("material %q: not in %v", description, page["items"])
	return ""
}

// TestMigratedMaterials: a run creates the scope's plants and storage
// locations active and its materials active at once, each with a create
// change applied that names the run and the SAP material and has no
// approval request (S2o); materials the platform cannot hold are held
// back with their reasons as warnings, and the reconciliation agrees. A
// run of the same extract loads nothing again; one whose extract has a
// plant more extends a material to it, also a plant of another company.
func TestMigratedMaterials(t *testing.T) {
	e := materialSetup(t)
	write(t, e.dir, "materials.json", withMaterials())
	conn := e.connector(t, "materials.json")
	_, got := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if got["status"] != "loaded" {
		t.Fatalf("status: got %v (%v), want loaded", got["status"], quality(got))
	}
	q := quality(got)
	for _, want := range []string{
		`warning MAKT M-2: no description in English; it loads described "Material M-2", in language D`,
		`warning MARA M-3: held back: base unit "ST" is no unit of measure of the platform`,
		`warning MARA M-4: held back: the conversion of BOX, 0/1 base units, is not whole numbers from 1 to 1,000,000; alternative unit "CAR" is no unit of measure of the platform`,
		`warning MARA M-5: held back: material type "ZZZ" is none of ROH, HALB, FERT, HAWA, NLAG and DIEN, which the platform's types are; material group "lower" is not 1 to 9 capitals, digits, - or _`,
		`warning MARA M-6: flagged for deletion in SAP (LVORM); not migrated`,
		`warning MARD M-2/P1/S1: flagged for deletion at the storage location (LVORM); the material is not kept there`,
		`warning MARC M-7/P1: flagged for deletion at the plant (LVORM); the material is not extended to it`,
		`warning MARA M-7: every plant of the scope flags it for deletion (MARC LVORM); not migrated`,
		`warning MARA M-8: held back: it has no description (MAKT)`,
		`warning MARA M-9: held back: unit "BOX" is listed twice (MARM); the base unit's own conversion is 2/1, not 1/1`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("quality: got\n%s\nwant it to hold %s", q, want)
		}
	}
	expectFields(t, "loaded", report(got)["loaded"].(map[string]any), map[string]any{"materials": 2.0, "plants": 2.0, "storage_locations": 3.0})
	want := []string{"Material M-1 raw_material STEEL KG active, P1(S1), T 1000/1", "Material M-2 finished_good SHELVING EA active, P1(S2) P2(S3), BOX 4/1"}
	if got := e.materialList(t); !slices.Equal(got, want) {
		t.Errorf("materials: got %v, want %v", got, want)
	}
	changes, _ := e.Must(200, "GET", "/v1/material-changes?material_id="+e.materialID(t, "Material M-1"), "", "")
	c := changes["items"].([]any)[0].(map[string]any)
	expectFields(t, "the migrated material's change", c, map[string]any{"kind": "create", "status": "applied", "workflow_request_id": nil})
	if reason := c["reason"].(string); !strings.HasSuffix(reason, ": SAP material M-1") {
		t.Errorf("the migrated material's change reason: got %q, want it to name SAP material M-1", reason)
	}
	run := got
	_, got = e.act(t, run, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("reconciled: got %v (%v %v)", got["status"], report(got)["diffs"], report(got)["issues"])
	}
	for name, want := range map[string]string{"Plants": "2", "Storage locations": "3", "Materials": "2", "Materials: materials with differences": "0",
		"Plants: plants with differences": "0", "Storage locations: storage locations with differences": "0"} {
		expectFields(t, "check "+name, checkOf(t, got, "2000", name), map[string]any{"sap": want, "platform": want, "pass": true})
	}
	_, again := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	expectFields(t, "loaded again", report(again)["loaded"].(map[string]any), map[string]any{"materials": nil, "plants": nil, "storage_locations": nil,
		"materials_extended": nil})
	more := withMaterials()
	more.T001W = append(more.T001W, sap.T001W{WERKS: "P3", NAME1: "Plant three", BUKRS: "2000"})
	more.T001L = append(more.T001L, sap.T001L{WERKS: "P3", LGORT: "S4", LGOBE: "Store four"})
	more.MARC = append(more.MARC, sap.MARC{MATNR: "M-1", WERKS: "P3"})
	more.MARD = append(more.MARD, sap.MARD{MATNR: "M-1", WERKS: "P3", LGORT: "S4"}, sap.MARD{MATNR: "M-1", WERKS: "P1", LGORT: "S2"})
	write(t, e.dir, "more.json", more)
	_, extended := e.act(t, e.run(t, e.connector(t, "more.json"), "documents", false, e.DE.String()), "start")
	expectFields(t, "loaded with plant P3", report(extended)["loaded"].(map[string]any), map[string]any{"materials_extended": 1.0, "plants": 1.0,
		"storage_locations": 1.0})
	if got := e.materialList(t)[0]; got != "Material M-1 raw_material STEEL KG active, P1(S1,S2) P3(S4), T 1000/1" {
		t.Errorf("M-1 extended: got %s, want it at P1's S1 and S2 and at P3's S4", got)
	}
	_, got = e.act(t, extended, "reconcile")
	if got["status"] != "reconciled" {
		t.Errorf("reconciled after the extension: got %v (%v)", got["status"], report(got)["diffs"])
	}
	e.otherCompany(t, more)
}

// otherCompany extends M-1 to plant P5 of company 1000 by a run over both
// companies, each reconciled with its own plants and materials.
func (e env) otherCompany(t *testing.T, x *sap.Extract) {
	t.Helper()
	x.T001 = append(x.T001, sap.T001{BUKRS: "1000", BUTXT: "Acme Corp US", WAERS: "USD", LAND1: "US"})
	x.T001W = append(x.T001W, sap.T001W{WERKS: "P5", NAME1: "Plant five", BUKRS: "1000"})
	x.T001L = append(x.T001L, sap.T001L{WERKS: "P5", LGORT: "S6", LGOBE: "Store six"})
	x.MARC = append(x.MARC, sap.MARC{MATNR: "M-1", WERKS: "P5"})
	x.MARD = append(x.MARD, sap.MARD{MATNR: "M-1", WERKS: "P5", LGORT: "S6"})
	write(t, e.dir, "both.json", x)
	_, run := e.act(t, e.run(t, e.connector(t, "both.json"), "documents", false, e.DE.String(), e.US.String()), "start")
	expectFields(t, "loaded with company 1000", report(run)["loaded"].(map[string]any), map[string]any{"materials_extended": 1.0, "plants": 1.0,
		"storage_locations": 1.0})
	_, got := e.act(t, run, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("both companies reconciled: got %v (%v %v)", got["status"], report(got)["diffs"], report(got)["issues"])
	}
	for company, want := range map[string]string{"1000": "1", "2000": "3"} {
		expectFields(t, company+" plants", checkOf(t, got, company, "Plants"), map[string]any{"sap": want, "platform": want})
	}
	expectFields(t, "1000 materials", checkOf(t, got, "1000", "Materials"), map[string]any{"sap": "1", "platform": "1"})
}

// TestMigratedMaterialPendingChange: a material with a pending change is
// not extended by a run, which reports why; applying the change would
// replace its plants.
func TestMigratedMaterialPendingChange(t *testing.T) {
	e := materialSetup(t)
	write(t, e.dir, "materials.json", withMaterials())
	e.act(t, e.run(t, e.connector(t, "materials.json"), "documents", false, e.DE.String()), "start")
	id := e.materialID(t, "Material M-1")
	_, tag := e.Must(200, "GET", "/v1/materials/"+id, "", "")
	e.Must(200, "PATCH", "/v1/materials/"+id, `{"description": "Material M-1, revised", "reason": "Revised"}`, tag)
	more := withMaterials()
	more.MARC = append(more.MARC, sap.MARC{MATNR: "M-1", WERKS: "P2"})
	more.MARD = append(more.MARD, sap.MARD{MATNR: "M-1", WERKS: "P2", LGORT: "S3"})
	write(t, e.dir, "more.json", more)
	_, got := e.act(t, e.run(t, e.connector(t, "more.json"), "documents", false, e.DE.String()), "start")
	if load := strings.Join(loadIssues(got), "\n"); !strings.Contains(load, "MARC M-1: materials.change_pending: Material ") {
		t.Errorf("load: got\n%s\nwant M-1's extension refused for its pending change", load)
	}
	if got["status"] != "failed" {
		t.Errorf("status: got %v, want failed", got["status"])
	}
}

// TestMigratedMaterialWithoutRange: a company without a material range
// refuses each material to create, naming the range.
func TestMigratedMaterialWithoutRange(t *testing.T) {
	f := fixture.NewWith(t, nil, func(doc *config.Config) {
		doc.NumberRanges = slices.DeleteFunc(doc.NumberRanges, func(r config.NumberRange) bool { return r.DocumentType == "material" })
	}, append(slices.Clone(perms), "materials.material.read")...)
	dir := t.TempDir()
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir })
	e := env{Env: f, dir: dir}
	write(t, dir, "materials.json", withMaterials())
	_, got := e.act(t, e.run(t, e.connector(t, "materials.json"), "documents", false, e.DE.String()), "start")
	if load := strings.Join(loadIssues(got), "\n"); !strings.Contains(load, "MARA M-1: config.no_number_range: ") {
		t.Errorf("load: got\n%s\nwant M-1 refused for want of a material range", load)
	}
	if got["status"] != "failed" {
		t.Errorf("status: got %v, want failed", got["status"])
	}
}

// TestMigratedMaterialDifferences: a material, plant or storage location
// SAP changed after the load, or that the platform lacks or holds
// otherwise, is a difference by its SAP key; an extract the gate would
// refuse fails the reconciliation; a rehearsal leaves nothing behind.
func TestMigratedMaterialDifferences(t *testing.T) {
	e := materialSetup(t)
	write(t, e.dir, "materials.json", withMaterials())
	conn := e.connector(t, "materials.json")
	_, rehearsal := e.act(t, e.run(t, conn, "documents", true, e.DE.String()), "start")
	if rehearsal["status"] != "reconciled" {
		t.Fatalf("rehearsal: got %v, want reconciled", rehearsal["status"])
	}
	if kept := e.materialList(t); len(kept) != 0 {
		t.Fatalf("materials after the rehearsal: got %v, want none", kept)
	}
	_, run := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	units, _ := e.Must(200, "GET", "/v1/org-units?kind=plant&page_size=50", "", "")
	for _, u := range units["items"].([]any) {
		if m := u.(map[string]any); m["code"] == "P2" {
			_, tag := e.Must(200, "GET", "/v1/org-units/"+m["id"].(string), "", "")
			e.Must(200, "POST", "/v1/org-units/"+m["id"].(string)+":block", `{"reason": "Closed"}`, tag)
		}
	}
	changed := withMaterials()
	changed.MAKT[0].MAKTX = "Steel sheet, 3 mm"
	changed.T001W = append(changed.T001W, sap.T001W{WERKS: "P4", NAME1: "Plant four", BUKRS: "2000"})
	changed.T001L = append(changed.T001L, sap.T001L{WERKS: "P4", LGORT: "S5", LGOBE: "Store five"})
	changed.MARC = append(changed.MARC, sap.MARC{MATNR: "M-X", WERKS: "P1"})
	write(t, e.dir, "materials.json", changed)
	_, got := e.act(t, run, "reconcile")
	if got["status"] != "failed" {
		t.Fatalf("status: got %v, want failed", got["status"])
	}
	var diffs []string
	for _, d := range report(got)["diffs"].([]any) {
		m := d.(map[string]any)
		diffs = append(diffs, fmt.Sprintf("%v: %v | %v", m["key"], m["sap"], m["platform"]))
	}
	want := []string{"plant P2: P2 | P2 (blocked)", "plant P4: P4 | none", "storage location P4/S5: P4/S5 | none",
		`material M-1: raw_material, group STEEL, base unit KG, active: "Steel sheet, 3 mm"; plant P1 (S1); unit T 1000/1 | ` +
			`raw_material, group STEEL, base unit KG, active: "Material M-1"; plant P1 (S1); unit T 1000/1`}
	if !slices.Equal(diffs, want) {
		t.Errorf("diffs: got\n%s\nwant\n%s", strings.Join(diffs, "\n"), strings.Join(want, "\n"))
	}
	if issues := fmt.Sprint(report(got)["issues"]); !strings.Contains(issues, "material M-X is not in MARA") {
		t.Errorf("reconciliation issues: got %s, want M-X's plant data refused", issues)
	}
	_, got = e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if q := quality(got); !strings.Contains(q, "error MARC M-X/P1: material M-X is not in MARA") {
		t.Errorf("an extract with plant data of no material: got\n%s\nwant it refused by the gate", q)
	}
}

// TestMaterialQualityGate: rows that name a material, plant data or
// storage location the extract does not have, keys that are no org unit
// codes, plants and storage locations without a name or listed twice, a
// description twice in a language, and a storage location code at two
// plants of one company refuse the run.
func TestMaterialQualityGate(t *testing.T) {
	e := materialSetup(t)
	x := withMaterials()
	x.T001W = append(x.T001W, sap.T001W{WERKS: "P1", NAME1: "Plant one again", BUKRS: "2000"}, sap.T001W{WERKS: "p-5", BUKRS: "2000"})
	x.T001L = append(x.T001L, sap.T001L{WERKS: "P1", LGORT: "S1", LGOBE: "Again"},
		sap.T001L{WERKS: "P2", LGORT: "S1", LGOBE: "Shared"})
	x.MARA = append(x.MARA, sap.MARA{MATNR: "M-1", MTART: "ROH", MATKL: "STEEL", MEINS: "KG"})
	x.MAKT = append(x.MAKT, sap.MAKT{MATNR: "M-1", SPRAS: "E", MAKTX: "Steel sheet, again"})
	x.MARC = append(x.MARC, sap.MARC{MATNR: "M-1", WERKS: "P1"})
	x.MARD = append(x.MARD, sap.MARD{MATNR: "M-2", WERKS: "P2", LGORT: "S8"}, sap.MARD{MATNR: "M-3", WERKS: "P1", LGORT: "S1"})
	write(t, e.dir, "bad.json", x)
	_, got := e.act(t, e.run(t, e.connector(t, "bad.json"), "documents", false, e.DE.String()), "start")
	if got["status"] != "failed" {
		t.Fatalf("status: got %v, want failed", got["status"])
	}
	q := quality(got)
	for _, want := range []string{
		"error T001W P1: the plant is listed twice",
		`error T001W p-5: the plant's code "p-5" is not 1 to 10 capitals, digits or _, as an org unit's is`,
		"error T001W p-5: the plant has no name",
		"error T001L P1/S1: the storage location is listed twice",
		"error T001L P2/S1: storage location S1 is also at plant P1 of company code 2000; the platform keeps a storage location's code unique in its company, where SAP keys it by its plant",
		"error MAKT M-1/E: the material's description in language E is listed twice",
		"error MARA M-1: the material is listed twice",
		"error MARC M-1/P1: the material's plant data is listed twice",
		"error MARD M-3/P1/S1: material M-3 has no plant data (MARC) at plant P1",
		"error MARD M-2/P2/S8: storage location S8 of plant P2 is not in T001L",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("quality: got\n%s\nwant it to hold %s", q, want)
		}
	}
}

// TestMigratedOrgUnitsFound: a plant or storage location the company has
// with SAP's code is taken rather than created; one that is not active,
// or a storage location under another plant, refuses its key and the
// load.
func TestMigratedOrgUnitsFound(t *testing.T) {
	e := materialSetup(t)
	p1 := e.unit(t, "plant", "P1", "", true)
	e.unit(t, "storage_location", "S1", p1, true)
	x := withMaterials()
	x.T001W, x.T001L = x.T001W[1:], x.T001L[:2] // P1 with S1 and S2 alone
	x.MARA, x.MAKT, x.MARM, x.MARC, x.MARD = x.MARA[:1], x.MAKT[:2], x.MARM[:2], x.MARC[:1], x.MARD[:1]
	write(t, e.dir, "found.json", x)
	_, got := e.act(t, e.run(t, e.connector(t, "found.json"), "documents", false, e.DE.String()), "start")
	if got["status"] != "loaded" {
		t.Fatalf("found P1 and S1: got %v (%s), want loaded", got["status"], quality(got))
	}
	expectFields(t, "loaded", report(got)["loaded"].(map[string]any), map[string]any{"plants": 1.0, "storage_locations": 2.0})
	units, _ := e.Must(200, "GET", "/v1/org-units?page_size=50", "", "")
	if n := len(units["items"].([]any)); n != 3 {
		t.Errorf("org units: got %d, want P1 and S1 taken and S2 created", n)
	}
	other := materialSetup(t)
	p2 := other.unit(t, "plant", "P2", "", false)
	o1 := other.unit(t, "plant", "P1", "", true)
	other.unit(t, "storage_location", "S1", p2, true)
	other.unit(t, "storage_location", "S2", o1, false)
	y := withMaterials()
	y.MARA = append(y.MARA, sap.MARA{MATNR: "M-10", MTART: "ROH", MATKL: "STEEL", MEINS: "KG"})
	y.MAKT = append(y.MAKT, sap.MAKT{MATNR: "M-10", SPRAS: "E", MAKTX: "Material M-10"})
	y.MARC = append(y.MARC, sap.MARC{MATNR: "M-10", WERKS: "P2"})
	write(t, other.dir, "found.json", y)
	_, got = other.act(t, other.run(t, other.connector(t, "found.json"), "documents", false, other.DE.String()), "start")
	load := loadIssues(got)
	for _, want := range []string{"T001W P2: the company's plant P2 is inactive; activate it, or give SAP's another code",
		"T001L P1/S1: the company's storage location S1 is under another plant",
		"T001L P1/S2: the company's storage location S2 is inactive; activate it, or give SAP's another code",
		"MARC M-10/P2: plant P2 was not loaded", "MARD M-1/P1/S1: storage location S1 of plant P1 was not loaded"} {
		if !slices.Contains(load, want) {
			t.Errorf("load: got %v, want it to hold %s", load, want)
		}
	}
	if got["status"] != "failed" {
		t.Errorf("status: got %v, want failed", got["status"])
	}
}

// unit creates an org unit of company 2000 through the API, active or
// left inactive, under the parent when one is given.
func (e env) unit(t *testing.T, kind, code, parent string, active bool) string {
	t.Helper()
	body := fmt.Sprintf(`{"legal_entity_id": %q, "kind": %q, "code": %q, "name": "Unit %s"`, e.DE, kind, code, code)
	if parent != "" {
		body += fmt.Sprintf(`, "parent_id": %q`, parent)
	}
	u, _ := e.Must(201, "POST", "/v1/org-units", body+"}", "")
	if active {
		e.Must(200, "POST", "/v1/org-units/"+u["id"].(string)+":activate", "", `"1"`)
	}
	return u["id"].(string)
}
