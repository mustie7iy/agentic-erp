package integration_test

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestMaterialListsAfterARun: material types and groups configured after
// a run hold back only materials no run has loaded (ADR-0082 As built
// S1a4). Run 1 loads M-1 (raw material, STEEL) and M-2 (finished good,
// SHELVING) without lists; the tenant then lists raw materials and STEEL
// alone; run 2 still extends M-2 to a new plant and reconciles, and holds
// back the new M-10, a finished good of SHELVING, naming both reasons.
func TestMaterialListsAfterARun(t *testing.T) {
	e := materialSetup(t)
	write(t, e.dir, "materials.json", withMaterials())
	if _, got := e.act(t, e.run(t, e.connector(t, "materials.json"), "documents", false, e.DE.String()), "start"); got["status"] != "loaded" {
		t.Fatalf("run 1: got %v (%v), want loaded", got["status"], quality(got))
	}
	pgtest.Exec(t, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, restored_from, activated_by)
		SELECT v.tenant_id, gen_random_uuid(), v.number + 1, v.document || jsonb_build_object('materials', $2::jsonb),
			v.hash, 'rollback', v.number, v.activated_by
		FROM config.config_version v JOIN config.active_version a ON a.tenant_id = v.tenant_id AND a.number = v.number WHERE v.tenant_id = $1`,
		e.Tenant.ID, `{"material_types": [{"code": "raw_material", "name": "Raw materials"}], "material_groups": [{"code": "STEEL", "name": "Steel"}]}`)
	pgtest.Exec(t, `UPDATE config.active_version SET number = number + 1, version = version + 1 WHERE tenant_id = $1`, e.Tenant.ID)
	more := withMaterials()
	more.T001W = append(more.T001W, sap.T001W{WERKS: "P3", NAME1: "Plant three", BUKRS: "2000"})
	more.T001L = append(more.T001L, sap.T001L{WERKS: "P3", LGORT: "S4", LGOBE: "Store four"})
	more.MARC = append(more.MARC, sap.MARC{MATNR: "M-2", WERKS: "P3"})
	more.MARD = append(more.MARD, sap.MARD{MATNR: "M-2", WERKS: "P3", LGORT: "S4"})
	more.MARA = append(more.MARA, sap.MARA{MATNR: "M-10", MTART: "FERT", MATKL: "SHELVING", MEINS: "EA"})
	more.MARM = append(more.MARM, sap.MARM{MATNR: "M-10", MEINH: "EA", UMREZ: "1", UMREN: "1"})
	more.MAKT = append(more.MAKT, sap.MAKT{MATNR: "M-10", SPRAS: "E", MAKTX: "Material M-10"})
	more.MARC = append(more.MARC, sap.MARC{MATNR: "M-10", WERKS: "P3"})
	write(t, e.dir, "more.json", more)
	_, run := e.act(t, e.run(t, e.connector(t, "more.json"), "documents", false, e.DE.String()), "start")
	if run["status"] != "loaded" {
		t.Fatalf("run 2: got %v (%v), want loaded", run["status"], quality(run))
	}
	want := `warning MARA M-10: held back: material type "FERT" (finished_good) is not one of the configuration's material types; ` +
		`material group "SHELVING" is not one of the configuration's material groups`
	if q := quality(run); !strings.Contains(q, want) || strings.Contains(q, "MARA M-2: held back") {
		t.Errorf("run 2's quality: got\n%s\nwant it to hold %s and not M-2", q, want)
	}
	expectFields(t, "run 2 loaded", report(run)["loaded"].(map[string]any), map[string]any{"materials": nil, "materials_extended": 1.0})
	if got := e.materialList(t)[1]; got != "Material M-2 finished_good SHELVING EA active, P1(S2) P2(S3) P3(S4), BOX 4/1" {
		t.Errorf("M-2 extended: got %s, want it at P3's S4 too", got)
	}
	_, got := e.act(t, run, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("run 2 reconciled: got %v (%v %v)", got["status"], report(got)["diffs"], report(got)["issues"])
	}
	expectFields(t, "materials", checkOf(t, got, "2000", "Materials"), map[string]any{"sap": "2", "platform": "2", "pass": true})
}
