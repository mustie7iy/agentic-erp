package integration_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// materialSetup is a tenant whose people may also read materials and
// read and manage org units, and whose API reads extracts from a
// directory of its own.
func materialSetup(t *testing.T) env {
	t.Helper()
	f := fixture.New(t, append(slices.Clone(perms), "materials.material.read", "tenancy.org.read", "tenancy.org.manage")...)
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

// TestMigratedMaterials: a run creates the scope's plants and storage
// locations active and its materials active at once, each with a create
// change applied that names the run and the SAP material and has no
// approval request (S2o); materials the platform cannot hold are held
// back with their reasons as warnings, and the reconciliation agrees. A
// run of the same extract loads nothing again; one whose extract has a
// plant more extends a material to it.
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
	if l := report(got)["loaded"].(map[string]any); l["materials"] != float64(2) || l["plants"] != float64(2) || l["storage_locations"] != float64(3) {
		t.Errorf("loaded: got %v, want 2 plants, 3 storage locations, 2 materials", l)
	}
	want := []string{"Material M-1 raw_material STEEL KG active, P1(S1), T 1000/1", "Material M-2 finished_good SHELVING EA active, P1(S2) P2(S3), BOX 4/1"}
	if got := e.materialList(t); !slices.Equal(got, want) {
		t.Errorf("materials: got %v, want %v", got, want)
	}
	page, _ := e.Must(200, "GET", "/v1/materials?page_size=1", "", "")
	id := page["items"].([]any)[0].(map[string]any)["id"].(string)
	changes, _ := e.Must(200, "GET", "/v1/material-changes?material_id="+id, "", "")
	c := changes["items"].([]any)[0].(map[string]any)
	if c["kind"] != "create" || c["status"] != "applied" || c["workflow_request_id"] != nil || !strings.Contains(c["reason"].(string), ": SAP material M-") {
		t.Errorf("the migrated material's change: got %v, want an applied create naming the SAP material, without an approval request", c)
	}
	run := got
	_, got = e.act(t, run, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("reconciled: got %v (%v %v)", got["status"], report(got)["diffs"], report(got)["issues"])
	}
	for name, want := range map[string]string{"Plants": "2", "Storage locations": "3", "Materials": "2", "Materials: materials with differences": "0",
		"Plants: plants with differences": "0", "Storage locations: storage locations with differences": "0"} {
		if c := checkOf(t, got, "2000", name); c["sap"] != want || c["platform"] != want || c["pass"] != true {
			t.Errorf("check %q: got %v, want %s on both sides", name, c, want)
		}
	}
	_, again := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if l := report(again)["loaded"].(map[string]any); l["materials"] != nil || l["plants"] != nil || l["storage_locations"] != nil || l["materials_extended"] != nil {
		t.Errorf("loaded again: got %v, want no plants, storage locations or materials", l)
	}
	more := withMaterials()
	more.T001W = append(more.T001W, sap.T001W{WERKS: "P3", NAME1: "Plant three", BUKRS: "2000"})
	more.T001L = append(more.T001L, sap.T001L{WERKS: "P3", LGORT: "S4", LGOBE: "Store four"})
	more.MARC = append(more.MARC, sap.MARC{MATNR: "M-1", WERKS: "P3"})
	more.MARD = append(more.MARD, sap.MARD{MATNR: "M-1", WERKS: "P3", LGORT: "S4"}, sap.MARD{MATNR: "M-1", WERKS: "P1", LGORT: "S2"})
	write(t, e.dir, "more.json", more)
	_, extended := e.act(t, e.run(t, e.connector(t, "more.json"), "documents", false, e.DE.String()), "start")
	if l := report(extended)["loaded"].(map[string]any); l["materials_extended"] != float64(1) || l["plants"] != float64(1) || l["storage_locations"] != float64(1) {
		t.Errorf("loaded with plant P3: got %v, want P3 and S4 created and M-1 extended", l)
	}
	if got := e.materialList(t)[0]; got != "Material M-1 raw_material STEEL KG active, P1(S1,S2) P3(S4), T 1000/1" {
		t.Errorf("M-1 extended: got %s, want it at P1's S1 and S2 and at P3's S4", got)
	}
	_, got = e.act(t, extended, "reconcile")
	if got["status"] != "reconciled" {
		t.Errorf("reconciled after the extension: got %v (%v)", got["status"], report(got)["diffs"])
	}
}

// TestMigratedMaterialDifferences: a material, plant or storage location
// SAP changed after the load, or that the platform lacks, is a difference
// by its SAP key; a rehearsal leaves nothing behind.
func TestMigratedMaterialDifferences(t *testing.T) {
	e := materialSetup(t)
	write(t, e.dir, "materials.json", withMaterials())
	conn := e.connector(t, "materials.json")
	_, rehearsal := e.act(t, e.run(t, conn, "documents", true, e.DE.String()), "start")
	if rehearsal["status"] != "reconciled" || len(e.materialList(t)) != 0 {
		t.Fatalf("rehearsal: got %v and materials %v, want reconciled and none kept", rehearsal["status"], e.materialList(t))
	}
	_, run := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	changed := withMaterials()
	changed.MAKT[0].MAKTX = "Steel sheet, 3 mm"
	changed.T001W = append(changed.T001W, sap.T001W{WERKS: "P4", NAME1: "Plant four", BUKRS: "2000"})
	changed.T001L = append(changed.T001L, sap.T001L{WERKS: "P4", LGORT: "S5", LGOBE: "Store five"})
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
	want := []string{"plant P4: P4 | none", "storage location P4/S5: P4/S5 | none",
		`material M-1: raw_material, group STEEL, base unit KG, active: "Steel sheet, 3 mm"; plant P1 (S1); unit T 1000/1 | ` +
			`raw_material, group STEEL, base unit KG, active: "Material M-1"; plant P1 (S1); unit T 1000/1`}
	if !slices.Equal(diffs, want) {
		t.Errorf("diffs: got\n%s\nwant\n%s", strings.Join(diffs, "\n"), strings.Join(want, "\n"))
	}
	broken := withMaterials()
	broken.MARC = append(broken.MARC, sap.MARC{MATNR: "M-X", WERKS: "P1"})
	write(t, e.dir, "materials.json", broken)
	_, got = e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if got["status"] != "failed" || !strings.Contains(quality(got), "error MARC M-X/P1: material M-X is not in MARA") {
		t.Errorf("an extract with plant data of no material: got %v (%s), want refused by the gate", got["status"], quality(got))
	}
}

// TestMaterialQualityGate: rows that name a material, plant data or
// storage location the extract does not have, keys that are no org unit codes,
// plants and storage locations without a name or listed twice, and a
// storage location code at two plants of one company refuse the run.
func TestMaterialQualityGate(t *testing.T) {
	e := materialSetup(t)
	x := withMaterials()
	x.T001W = append(x.T001W, sap.T001W{WERKS: "P1", NAME1: "Plant one again", BUKRS: "2000"}, sap.T001W{WERKS: "p-5", BUKRS: "2000"})
	x.T001L = append(x.T001L, sap.T001L{WERKS: "P1", LGORT: "S1", LGOBE: "Again"},
		sap.T001L{WERKS: "P2", LGORT: "S1", LGOBE: "Shared"})
	x.MARA = append(x.MARA, sap.MARA{MATNR: "M-1", MTART: "ROH", MATKL: "STEEL", MEINS: "KG"})
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
	unit := func(kind, code, parent string, active bool) string {
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
	p1 := unit("plant", "P1", "", true)
	unit("storage_location", "S1", p1, true)
	x := withMaterials()
	x.T001W, x.T001L = x.T001W[1:], x.T001L[:2] // P1 with S1 and S2 alone
	x.MARA, x.MAKT, x.MARM, x.MARC, x.MARD = x.MARA[:1], x.MAKT[:2], x.MARM[:2], x.MARC[:1], x.MARD[:1]
	write(t, e.dir, "found.json", x)
	conn := e.connector(t, "found.json")
	_, got := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if l := report(got)["loaded"].(map[string]any); got["status"] != "loaded" || l["plants"] != float64(1) || l["storage_locations"] != float64(2) {
		t.Fatalf("found P1 and S1: got %v %v (%s), want loaded with P1, S1 and S2 recorded", got["status"], l, quality(got))
	}
	units, _ := e.Must(200, "GET", "/v1/org-units?legal_entity_id="+e.DE.String()+"&page_size=50", "", "")
	if n := len(units["items"].([]any)); n != 3 {
		t.Errorf("org units: got %d, want P1 and S1 taken and S2 created", n)
	}
	other := materialSetup(t)
	p2 := func() string {
		u, _ := other.Must(201, "POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "plant", "code": "P2", "name": "Two"}`, other.DE), "")
		return u["id"].(string)
	}()
	o1, _ := other.Must(201, "POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "plant", "code": "P1", "name": "One"}`, other.DE), "")
	other.Must(200, "POST", "/v1/org-units/"+o1["id"].(string)+":activate", "", `"1"`)
	s3, _ := other.Must(201, "POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "storage_location", "code": "S1", "name": "One", "parent_id": %q}`,
		other.DE, p2), "")
	other.Must(200, "POST", "/v1/org-units/"+s3["id"].(string)+":activate", "", `"1"`)
	write(t, other.dir, "found.json", withMaterials())
	_, got = other.act(t, other.run(t, other.connector(t, "found.json"), "documents", false, other.DE.String()), "start")
	var load []string
	for _, i := range report(got)["load"].([]any) {
		m := i.(map[string]any)
		load = append(load, fmt.Sprintf("%v %v: %v", m["table"], m["key"], m["message"]))
	}
	for _, want := range []string{"T001W P2: the company's plant P2 is inactive; activate it, or give SAP's another code",
		"T001L P1/S1: the company's storage location S1 is under another plant", "MARC M-2/P2: plant P2 was not loaded",
		"MARD M-1/P1/S1: storage location S1 of plant P1 was not loaded"} {
		if !slices.Contains(load, want) {
			t.Errorf("load: got %v, want it to hold %s", load, want)
		}
	}
	if got["status"] != "failed" {
		t.Errorf("status: got %v, want failed", got["status"])
	}
}
