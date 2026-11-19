package integration_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestMaterialLoadAndRequestDoNotDeadlock runs a migration run that
// creates a material and extends another against a material request in
// the same company (S2o's lock order, ADR-0082 As built S2o; sapmm's
// review round 2, batch 83). A request takes the company's material range,
// writes its material and then takes the tenant's material change
// numbers; the run takes the materials it extends, then the range, and
// the change numbers last, so it never holds the change numbers while it
// waits for the range. The interleaving is forced without sleeps: a third
// transaction holds the request's base unit (the material's foreign key
// takes it) until the request waits for it holding the range, the run
// starts and waits for the request, and then lets go; both finish, one
// after the other. A run that took the change numbers before the range
// would deadlock with the request here.
func TestMaterialLoadAndRequestDoNotDeadlock(t *testing.T) {
	e := materialSetup(t)
	write(t, e.dir, "materials.json", withMaterials())
	if _, got := e.act(t, e.run(t, e.connector(t, "materials.json"), "documents", false, e.DE.String()), "start"); got["status"] != "loaded" {
		t.Fatalf("first run: got %v, want loaded: %v", got["status"], quality(got))
	}
	more := withMaterials()
	more.MARA = append(more.MARA, sap.MARA{MATNR: "M-10", MTART: "ROH", MATKL: "STEEL", MEINS: "KG"})
	more.MARM = append(more.MARM, sap.MARM{MATNR: "M-10", MEINH: "KG", UMREZ: "1", UMREN: "1"})
	more.MAKT = append(more.MAKT, sap.MAKT{MATNR: "M-10", SPRAS: "E", MAKTX: "Material M-10"})
	more.MARC = append(more.MARC, sap.MARC{MATNR: "M-10", WERKS: "P1"}, sap.MARC{MATNR: "M-1", WERKS: "P2"})
	more.MARD = append(more.MARD, sap.MARD{MATNR: "M-10", WERKS: "P1", LGORT: "S1"}, sap.MARD{MATNR: "M-1", WERKS: "P2", LGORT: "S3"})
	write(t, e.dir, "more.json", more)
	run := e.run(t, e.connector(t, "more.json"), "documents", false, e.DE.String())
	_, tag := e.Must(200, "GET", "/v1/migration-runs/"+run["id"].(string), "", "")
	// The request's plant is none the run takes, and its base unit, the
	// litre, none the run's materials have.
	plant := e.unit(t, "plant", "P9", "", true)
	location := e.unit(t, "storage_location", "S9", plant, true)

	gate, letGo := holdUnit(t, "L")
	defer letGo()
	request := e.async(apitest.Req{Method: "POST", Path: "/v1/materials", Body: fmt.Sprintf(`{"legal_entity_id": %q, "description": "Cutting oil",
		"base_unit": "L", "material_type": "trading_good", "material_group": "OILS", "reason": "New oil",
		"plants": [{"plant_id": %q, "storage_location_ids": [%q]}]}`, e.DE, plant, location)})
	var requestPID int32
	pgtest.Eventually(t, 10*time.Second, func() bool { requestPID = blockedBy(t, gate); return requestPID != 0 },
		"the material request never waited for its base unit")
	load := e.async(apitest.Req{Method: "POST", Path: "/v1/migration-runs/" + run["id"].(string) + ":start", IfMatch: tag})
	var loaded *httptest.ResponseRecorder
	pgtest.Eventually(t, 10*time.Second, func() bool {
		select {
		case loaded = <-load:
			return true
		default:
			return blockedBy(t, requestPID) != 0
		}
	}, "the run neither finished nor waited for the request")
	letGo()
	if loaded == nil {
		loaded = <-load
	}
	if r := <-request; r.Code != 201 {
		t.Errorf("the material request: got %d, want 201: %s", r.Code, r.Body)
	}
	if loaded.Code != 202 {
		t.Errorf("the run: got %d, want 202: %s", loaded.Code, loaded.Body)
	}
	got, _ := e.Must(200, "GET", "/v1/migration-runs/"+run["id"].(string), "", "")
	if got["status"] != "loaded" {
		t.Fatalf("the run: got %v, want loaded: %v", got["status"], quality(got))
	}
	expectFields(t, "the run's load", report(got)["loaded"].(map[string]any), map[string]any{"materials": 1.0, "materials_extended": 1.0})
}

// holdUnit locks a unit of measure's row in a transaction of its own, so
// a material written in it waits at its foreign key, and holds it until
// letGo, returning that transaction's backend. The row is shared by every
// tenant; the transaction changes nothing and is rolled back.
func holdUnit(t *testing.T, code string) (int32, func()) {
	t.Helper()
	ctx := context.Background()
	conn := pgtest.Admin(t)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid() FROM ref.unit_of_measure WHERE code = $1 FOR UPDATE`, code).Scan(&pid); err != nil {
		t.Fatalf("lock unit %s: %v", code, err)
	}
	done := false
	return pid, func() {
		if done {
			return
		}
		done = true
		if err := tx.Rollback(ctx); err != nil {
			t.Errorf("let go of unit %s: %v", code, err)
		}
	}
}
