package integration_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// centerSetup is a tenant whose configuration has the cost center
// categories ADMIN and SALES and whose API reads centersTiny as
// centers.json; its people may read centers and create segments.
func centerSetup(t *testing.T) env {
	t.Helper()
	f := fixture.NewWith(t, nil, func(doc *config.Config) {
		doc.Controlling.CostCenterCategories = []config.CostCenterCategory{{Code: "ADMIN", Name: "Administration"},
			{Code: "SALES", Name: "Sales", RevenueAllowed: true}}
	}, append(slices.Clone(perms), "ledger.cost_center.read", "ledger.cost_center.change")...)
	dir := t.TempDir()
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir })
	write(t, dir, "centers.json", centersTiny())
	return env{Env: f, dir: dir}
}

// centersTiny is tiny with company 1000 (nothing but the company) and the
// controlling master data of controlling area A1 (ADR-0050 §10): segment
// SEG1; profit center P1 of companies 2000 and 1000, locked from
// 2026-10-01; cost center C1 of 2000 with P1, locked for actual revenues
// until 2026-06-30, and C2 (SALES), without a profit center, named in
// German only.
func centersTiny() *sap.Extract {
	x := tiny()
	x.T001 = append(x.T001, sap.T001{BUKRS: "1000", BUTXT: "Acme Corp US", WAERS: "USD", LAND1: "US"})
	x.FAGL_SEGM = []sap.FAGLSegm{{SEGMENT: "SEG1"}}
	x.FAGL_SEGMT = []sap.FAGLSegmt{{SEGMENT: "SEG1", LANGU: "E", NAME: "Europe"}}
	x.CEPC = []sap.CEPC{{KOKRS: "A1", PRCTR: "P1", DATAB: "20200101", DATBI: "20260930", SEGMENT: "SEG1", VERAK: "Ana"},
		{KOKRS: "A1", PRCTR: "P1", DATAB: "20261001", DATBI: "99991231", SEGMENT: "SEG1", VERAK: "Ana", LOCK_IND: "X"}}
	x.CEPCT = []sap.CEPCT{{KOKRS: "A1", PRCTR: "P1", DATBI: "99991231", SPRAS: "E", KTEXT: "Germany", LTEXT: "German operations"}}
	x.CEPC_BUKRS = []sap.CEPCBukrs{{KOKRS: "A1", PRCTR: "P1", BUKRS: "2000"}, {KOKRS: "A1", PRCTR: "P1", BUKRS: "1000"}}
	x.CSKS = []sap.CSKS{
		{KOKRS: "A1", KOSTL: "C1", DATAB: "20260701", DATBI: "99991231", BUKRS: "2000", KOSAR: "ADMIN", VERAK: "Bo", PRCTR: "P1", WAERS: "EUR"},
		{KOKRS: "A1", KOSTL: "C1", DATAB: "20200101", DATBI: "20260630", BUKRS: "2000", KOSAR: "ADMIN", VERAK: "Bo", PRCTR: "P1", BKZER: "X", WAERS: "EUR"},
		{KOKRS: "A1", KOSTL: "C2", DATAB: "20260101", DATBI: "99991231", BUKRS: "2000", KOSAR: "SALES", VERAK: "Cy"}}
	x.CSKT = []sap.CSKT{{KOKRS: "A1", KOSTL: "C1", DATBI: "99991231", SPRAS: "E", KTEXT: "Finance"},
		{KOKRS: "A1", KOSTL: "C2", DATBI: "99991231", SPRAS: "D", KTEXT: "Vertrieb"}}
	return x
}

func report(got map[string]any) map[string]any { return got["report"].(map[string]any) }

// checkOf is the reconciliation check of a company with the name.
func checkOf(t *testing.T, got map[string]any, company, name string) map[string]any {
	t.Helper()
	for _, c := range report(got)["checks"].([]any) {
		if m := c.(map[string]any); m["company_code"] == company && m["name"] == name {
			return m
		}
	}
	t.Fatalf("check %s %q: not in %v", company, name, report(got)["checks"])
	return nil
}

// TestMigratedCenters: a run loads the scope's segments, profit centers and
// cost centers through the ledger, active at once with their slices as
// periods (ADR-0050 §10, T56h); the reconciliation agrees; a run of the
// same extract again loads none, and a run adding another company assigns
// it the profit center an earlier run loaded.
func TestMigratedCenters(t *testing.T) {
	e := centerSetup(t)
	conn := e.connector(t, "centers.json")
	_, got := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if got["status"] != "loaded" {
		t.Fatalf("status: got %v (%v), want loaded", got["status"], report(got))
	}
	if q := fmt.Sprint(report(got)["quality"]); !strings.Contains(q, "no text in English; it loads named \"Vertrieb\", its text in language D") {
		t.Errorf("quality: got %s, want a warning of C2's German name", q)
	}
	if l := report(got)["loaded"].(map[string]any); l["segments"] != float64(1) || l["profit_centers"] != float64(1) || l["cost_centers"] != float64(2) {
		t.Errorf("loaded: got %v, want 1 segment, 1 profit center, 2 cost centers", l)
	}
	run := got
	_, got = e.act(t, run, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("reconciled: got %v (%v)", got["status"], report(got)["diffs"])
	}
	for name, want := range map[string]string{"Cost centers": "2", "Profit centers": "1", "Cost centers: centers with differences": "0"} {
		if c := checkOf(t, got, "2000", name); c["sap"] != want || c["platform"] != want || c["pass"] != true {
			t.Errorf("check %q: got %v, want %s on both sides", name, c, want)
		}
	}
	page, _ := e.Must(200, "GET", "/v1/cost-centers?legal_entity_id="+e.DE.String(), "", "")
	var codes []string
	for _, it := range page["items"].([]any) {
		m := it.(map[string]any)
		codes = append(codes, fmt.Sprintf("%v %v %v %v", m["code"], m["name"], m["responsible"], m["status"]))
	}
	if fmt.Sprint(codes) != "[C1 Finance Bo active C2 Vertrieb Cy active]" {
		t.Errorf("cost centers: got %v, want C1 and C2 active", codes)
	}
	_, again := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if l := report(again)["loaded"].(map[string]any); l["segments"] != nil || l["profit_centers"] != nil || l["cost_centers"] != nil {
		t.Errorf("loaded again: got %v, want no centers", l)
	}
	_, us := e.act(t, e.run(t, conn, "documents", false, e.DE.String(), e.US.String()), "start")
	if l := report(us)["loaded"].(map[string]any); fmt.Sprint(l) != "map[profit_centers_extended:1]" {
		t.Errorf("loaded for 2000 and 1000: got %v, want P1 assigned to 1000 alone", l)
	}
	_, got = e.act(t, us, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("2000 and 1000 reconciled: got %v, %v", got["status"], report(got)["diffs"])
	}
	for _, company := range []string{"2000", "1000"} {
		if c := checkOf(t, got, company, "Profit centers"); c["platform"] != "1" || c["pass"] != true {
			t.Errorf("profit centers of %s: got %v, want P1 on both sides", company, c)
		}
	}
}

// TestMigratedCenterDifferences: a center SAP changed after the load, or
// one the platform lacks, is a difference by its SAP key.
func TestMigratedCenterDifferences(t *testing.T) {
	e := centerSetup(t)
	conn := e.connector(t, "centers.json")
	_, run := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	x := centersTiny()
	x.CSKS[0].PKZKP = "X"
	x.CSKS = append(x.CSKS, sap.CSKS{KOKRS: "A1", KOSTL: "C3", DATAB: "20260101", DATBI: "99991231", BUKRS: "2000", KOSAR: "ADMIN", VERAK: "Di"})
	x.CEPC[0].LOCK_IND = "X"
	write(t, e.dir, "centers.json", x)
	_, got := e.act(t, run, "reconcile")
	if got["status"] != "failed" {
		t.Fatalf("status: got %v, want failed", got["status"])
	}
	want := []string{
		"cost center A1/C1: 2020-01-01 to 2026-06-30: ADMIN, profit center P1, locked for actual revenues; from 2026-07-01: ADMIN, profit center P1, locked for plan" +
			" | 2020-01-01 to 2026-06-30: ADMIN, profit center P1, locked for actual revenues; from 2026-07-01: ADMIN, profit center P1",
		"cost center A1/C3: from 2026-01-01: ADMIN | none",
		"profit center A1/P1: from 2020-01-01, locked; segment SEG1 | 2020-01-01 to 2026-09-30; from 2026-10-01, locked; segment SEG1",
	}
	var diffs []string
	for _, d := range report(got)["diffs"].([]any) {
		m := d.(map[string]any)
		diffs = append(diffs, fmt.Sprintf("%v: %v | %v", m["key"], m["sap"], m["platform"]))
	}
	if fmt.Sprint(diffs) != fmt.Sprint(want) {
		t.Errorf("diffs:\n got %q\nwant %q", diffs, want)
	}
	if c := checkOf(t, got, "2000", "Cost centers"); c["sap"] != "3" || c["platform"] != "2" || c["pass"] != false {
		t.Errorf("cost centers: got %v, want 3 against 2", c)
	}
}

// TestMigratedCenterRefusals: a center whose code the tenant already has,
// created through the API, is refused by the load, and what names it is
// not loaded; the run fails and nothing stays.
func TestMigratedCenterRefusals(t *testing.T) {
	e := centerSetup(t)
	e.Must(201, "POST", "/v1/segments", `{"code": "SEG1", "name": "Taken"}`, "")
	conn := e.connector(t, "centers.json")
	_, got := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if got["status"] != "failed" {
		t.Fatalf("status: got %v, want failed", got["status"])
	}
	load := fmt.Sprint(report(got)["load"])
	for _, want := range []string{"FAGL_SEGM SEG1 ledger.segment_exists", "CEPC A1/P1 segment SEG1 was not loaded", "CSKS A1/C1 profit center P1 was not loaded"} {
		parts := strings.SplitN(want, " ", 3)
		if !strings.Contains(load, parts[0]) || !strings.Contains(load, parts[1]) || !strings.Contains(load, parts[2]) {
			t.Errorf("load refusals: got %s, want %s", load, want)
		}
	}
	if page, _ := e.Must(200, "GET", "/v1/cost-centers?legal_entity_id="+e.DE.String(), "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("cost centers after a failed run: got %v, want none", page["items"])
	}
}

// TestMigratedCenterDatabaseFailures: a center load, its reconciliation and
// a later run assigning a loaded profit center fail with each of their
// statements in turn.
func TestMigratedCenterDatabaseFailures(t *testing.T) {
	e := centerSetup(t)
	h := endpoints(e.dir)
	conn := e.connector(t, "centers.json")
	queued := uuid.MustParse(e.run(t, conn, "documents", false, e.DE.String())["id"].(string))
	yes := func(int32) error { return nil }
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "load", func(ctx context.Context) error {
		_, err := h.StartRun(ctx, queued, yes)
		return err
	})
	_, loaded := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	id := uuid.MustParse(loaded["id"].(string))
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "reconcile", func(ctx context.Context) error {
		_, err := h.ReconcileRun(ctx, id, yes)
		return err
	})
	us := uuid.MustParse(e.run(t, conn, "documents", false, e.US.String())["id"].(string))
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "assign", func(ctx context.Context) error {
		_, err := h.StartRun(ctx, us, yes)
		return err
	})
}
