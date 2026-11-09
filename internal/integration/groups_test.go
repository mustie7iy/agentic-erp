package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// group is a center group of controlling area A1: its subsets and values
// (sign, option, from, to).
type group struct {
	class, name string
	subsets     []string
	values      [][4]string
}

func addGroups(x *sap.Extract, groups ...group) {
	for _, g := range groups {
		x.SETHEADER = append(x.SETHEADER, sap.SETHEADER{SETCLASS: g.class, SUBCLASS: "A1", SETNAME: g.name})
		x.SETHEADERT = append(x.SETHEADERT, sap.SETHEADERT{SETCLASS: g.class, SUBCLASS: "A1", SETNAME: g.name, LANGU: "E", DESCRIPT: "Group " + g.name})
		for i, sub := range g.subsets {
			x.SETNODE = append(x.SETNODE, sap.SETNODE{SETCLASS: g.class, SUBCLASS: "A1", SETNAME: g.name, LINEID: fmt.Sprintf("%010d", i+1), SUBSETCLS: g.class,
				SUBSETSCLS: "A1", SUBSETNAME: sub})
		}
		for i, v := range g.values {
			x.SETLEAF = append(x.SETLEAF, sap.SETLEAF{SETCLASS: g.class, SUBCLASS: "A1", SETNAME: g.name, LINEID: fmt.Sprintf("%010d", i+1), VALSIGN: v[0],
				VALOPTION: v[1], VALFROM: v[2], VALTO: v[3]})
		}
	}
}

// centersGrouped is centersTiny with its centers' groups (T56h2): the
// standard hierarchy STD of cost centers (TKA01) with leaves ADM, which C1
// names (KHINR), and SAL, the range C2 to C9; an alternative hierarchy ALT
// whose leaf ALL takes C0 to C9; and the profit centers' standard
// hierarchy PSTD with leaf PL, which P1 names.
func centersGrouped() *sap.Extract {
	x := centersTiny()
	x.TKA01 = []sap.TKA01{{KOKRS: "A1", KHINR: "STD"}}
	for i := range x.CSKS {
		if x.CSKS[i].KOSTL == "C1" {
			x.CSKS[i].KHINR = "ADM"
		}
	}
	for i := range x.CEPC {
		x.CEPC[i].KHINR = "PL"
	}
	addGroups(x, group{"0101", "STD", []string{"ADM", "SAL"}, nil}, group{"0101", "ADM", nil, nil}, group{"0101", "SAL", nil, [][4]string{{"I", "BT", "C2", "C9"}}},
		group{"0101", "ALT", []string{"ALL"}, nil}, group{"0101", "ALL", nil, [][4]string{{"I", "BT", "C0", "C9"}}},
		group{"0106", "PSTD", []string{"PL"}, nil}, group{"0106", "PL", nil, nil})
	return x
}

// paths is a center's hierarchy paths as "hierarchy:node/node".
func (e env) paths(t *testing.T, kind, id string) []string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/"+kind+"/"+id+"/hierarchy-paths", "", "")
	var out []string
	for _, it := range page["items"].([]any) {
		p := it.(map[string]any)
		s := fmt.Sprint(p["hierarchy_code"], ":")
		for i, n := range p["nodes"].([]any) {
			if i > 0 {
				s += "/"
			}
			s += n.(map[string]any)["code"].(string)
		}
		out = append(out, s)
	}
	return out
}

// centerID is the id of the cost center of company 2000 with the code.
func (e env) centerID(t *testing.T, code string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/cost-centers?legal_entity_id="+e.DE.String()+"&code="+code, "", "")
	return page["items"].([]any)[0].(map[string]any)["id"].(string)
}

// TestMigratedGroups: a run loads SAP's center groups as hierarchies
// (ADR-0050 §10, T56h2): the standard ones below the standard hierarchy's
// root, each center created at its leaf, an alternative one with its
// centers; a run of the same extract loads nothing, and one with a group
// and a center SAP added adds them alone.
func TestMigratedGroups(t *testing.T) {
	e := centerSetup(t)
	write(t, e.dir, "groups.json", centersGrouped())
	conn := e.connector(t, "groups.json")
	_, got := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if got["status"] != "loaded" {
		t.Fatalf("status: got %v (%v), want loaded", got["status"], report(got))
	}
	l := report(got)["loaded"].(map[string]any)
	if got, want := fmt.Sprint(l["hierarchies"], l["hierarchy_nodes"], l["hierarchy_places"]), "1 7 5"; got != want {
		t.Errorf("hierarchies, nodes and places: got %s, want %s", got, want)
	}
	for _, x := range []struct{ code, want string }{{"C1", "[ALT:ALT/ALL STANDARD:STANDARD/STD/ADM]"}, {"C2", "[ALT:ALT/ALL STANDARD:STANDARD/STD/SAL]"}} {
		if got := fmt.Sprint(e.paths(t, "cost-centers", e.centerID(t, x.code))); got != x.want {
			t.Errorf("%s's paths: got %s, want %s", x.code, got, x.want)
		}
	}
	pcs, _ := e.Must(200, "GET", "/v1/profit-centers?code=P1", "", "")
	if got, want := fmt.Sprint(e.paths(t, "profit-centers", pcs["items"].([]any)[0].(map[string]any)["id"].(string))), "[STANDARD:STANDARD/PSTD/PL]"; got != want {
		t.Errorf("P1's paths: got %s, want %s", got, want)
	}
	_, again := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if l := report(again)["loaded"].(map[string]any); l["hierarchies"] != nil || l["hierarchy_nodes"] != nil || l["hierarchy_places"] != nil {
		t.Errorf("loaded again: got %v, want no groups", l)
	}
	write(t, e.dir, "groups.json", grownGroups())
	_, grown := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if l := report(grown)["loaded"].(map[string]any); fmt.Sprint(l) != "map[cost_centers:1 hierarchies:1 hierarchy_nodes:3 hierarchy_places:3]" {
		t.Errorf("loaded with a new group and center: got %v, want NEW, MORE, LONE, C3 and its three places", l)
	}
	if got, want := fmt.Sprint(e.paths(t, "cost-centers", e.centerID(t, "C3"))), "[ALT:ALT/ALL LONE:LONE STANDARD:STANDARD/STD/NEW]"; got != want {
		t.Errorf("C3's paths: got %s, want %s", got, want)
	}
}

// grownGroups is centersGrouped with a group NEW below STD, MORE below the
// loaded ALT, a lone group LONE (an alternative hierarchy of its root
// alone) taking C3, and a cost center C3 that names NEW.
func grownGroups() *sap.Extract {
	x := centersGrouped()
	addGroups(x, group{"0101", "NEW", nil, nil}, group{"0101", "MORE", nil, nil}, group{"0101", "LONE", nil, [][4]string{{"I", "EQ", "C3", ""}}})
	x.SETNODE = append(x.SETNODE, sap.SETNODE{SETCLASS: "0101", SUBCLASS: "A1", SETNAME: "STD", LINEID: "0000000003", SUBSETCLS: "0101", SUBSETSCLS: "A1",
		SUBSETNAME: "NEW"}, sap.SETNODE{SETCLASS: "0101", SUBCLASS: "A1", SETNAME: "ALT", LINEID: "0000000002", SUBSETCLS: "0101", SUBSETSCLS: "A1",
		SUBSETNAME: "MORE"})
	x.SETLEAF[0].VALTO = "C2"
	x.CSKS = append(x.CSKS, sap.CSKS{KOKRS: "A1", KOSTL: "C3", DATAB: "20260101", DATBI: "99991231", BUKRS: "2000", KOSAR: "ADMIN", VERAK: "Di", KHINR: "NEW"})
	x.CSKT = append(x.CSKT, sap.CSKT{KOKRS: "A1", KOSTL: "C3", DATBI: "99991231", SPRAS: "E", KTEXT: "Three"})
	return x
}

// TestMigratedGroupRefusals: an alternative hierarchy whose code the
// tenant already has is refused by the load, as are its places, and the
// run fails.
func TestMigratedGroupRefusals(t *testing.T) {
	e := centerSetup(t)
	e.Must(201, "POST", "/v1/hierarchies", `{"kind": "cost_center", "code": "ALT", "name": "Taken"}`, "")
	write(t, e.dir, "groups.json", centersGrouped())
	_, got := e.act(t, e.run(t, e.connector(t, "groups.json"), "documents", false, e.DE.String()), "start")
	if got["status"] != "failed" {
		t.Fatalf("status: got %v, want failed", got["status"])
	}
	if load := fmt.Sprint(report(got)["load"]); !contains(load, "SETHEADER", "0101/A1/ALT", "ledger.hierarchy_exists") {
		t.Errorf("load refusals: got %s, want ALT refused", load)
	}
	if l := report(got)["loaded"].(map[string]any); l["hierarchies"] != nil {
		t.Errorf("loaded: got %v, want no alternative hierarchy", l)
	}
}

func contains(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// TestMigratedGroupDatabaseFailures: a group load, and one adding a group
// and placing a center, fail with each of their statements in turn.
func TestMigratedGroupDatabaseFailures(t *testing.T) {
	e := centerSetup(t)
	write(t, e.dir, "groups.json", centersGrouped())
	h := endpoints(e.dir)
	conn := e.connector(t, "groups.json")
	yes := func(int32) error { return nil }
	first := uuid.MustParse(e.run(t, conn, "documents", false, e.DE.String())["id"].(string))
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "groups", func(ctx context.Context) error {
		_, err := h.StartRun(ctx, first, yes)
		return err
	})
	e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	write(t, e.dir, "groups.json", grownGroups())
	grown := uuid.MustParse(e.run(t, conn, "documents", false, e.DE.String())["id"].(string))
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "grown", func(ctx context.Context) error {
		_, err := h.StartRun(ctx, grown, yes)
		return err
	})
}
