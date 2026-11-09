package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// set is a set of controlling area A1 with its English text, subsets and
// values (sign, option, from, to).
type set struct {
	class, name string
	subsets     []string
	values      [][4]string
}

// withSets adds the sets to the extract, numbering their lines.
func withSets(x *sap.Extract, sets ...set) {
	for _, s := range sets {
		x.SETHEADER = append(x.SETHEADER, sap.SETHEADER{SETCLASS: s.class, SUBCLASS: "A1", SETNAME: s.name})
		x.SETHEADERT = append(x.SETHEADERT, sap.SETHEADERT{SETCLASS: s.class, SUBCLASS: "A1", SETNAME: s.name, LANGU: "E", DESCRIPT: "Group " + s.name})
		for i, sub := range s.subsets {
			x.SETNODE = append(x.SETNODE, sap.SETNODE{SETCLASS: s.class, SUBCLASS: "A1", SETNAME: s.name, LINEID: fmt.Sprint(i + 1), SUBSETCLS: s.class,
				SUBSETSCLS: "A1", SUBSETNAME: sub})
		}
		for i, v := range s.values {
			x.SETLEAF = append(x.SETLEAF, sap.SETLEAF{SETCLASS: s.class, SUBCLASS: "A1", SETNAME: s.name, LINEID: fmt.Sprint(i + 1), VALSIGN: v[0], VALOPTION: v[1],
				VALFROM: v[2], VALTO: v[3]})
		}
	}
}

// gateGroups is gateCenters with cost center C2 and center groups: the
// standard hierarchy STD (TKA01) with leaves ADM, holding C1 by its KHINR,
// and SAL, the range C2 to C9; an alternative hierarchy ALT whose leaf X
// takes every C; the profit centers' standard hierarchy PSTD with leaf PL,
// which P1 names (KHINR).
func gateGroups() *sap.Extract {
	x := gateCenters()
	x.CSKS[0].KHINR = "ADM"
	x.CSKS = append(x.CSKS, sap.CSKS{KOKRS: "A1", KOSTL: "C2", DATAB: "20200101", DATBI: "99991231", BUKRS: "2000", KOSAR: "ADMIN", VERAK: "Bo", WAERS: "EUR"})
	x.CSKT = append(x.CSKT, sap.CSKT{KOKRS: "A1", KOSTL: "C2", DATBI: "99991231", SPRAS: "E", KTEXT: "C2"})
	x.CEPC[0].KHINR = "PL"
	x.TKA01 = []sap.TKA01{{KOKRS: "A1", KHINR: "STD"}}
	withSets(x, set{"0101", "STD", []string{"ADM", "SAL"}, nil}, set{"0101", "ADM", nil, nil}, set{"0101", "SAL", nil, [][4]string{{"I", "BT", "C2", "C9"}}},
		set{"0101", "ALT", []string{"X"}, nil}, set{"0101", "X", nil, [][4]string{{"I", "BT", "C0", "C9"}}},
		set{"0106", "PSTD", []string{"PL"}, nil}, set{"0106", "PL", nil, nil})
	return x
}

// TestGroupPlan: the groups' trees and each center's leaf (ADR-0050 §10,
// T56h2).
func TestGroupPlan(t *testing.T) {
	g := planGroups(gateGroups())
	if len(g.issues) != 0 {
		t.Fatalf("issues: got %v, want none", g.issues)
	}
	var trees []string
	for _, tr := range g.trees {
		var nodes []string
		for _, n := range tr.nodes {
			nodes = append(nodes, fmt.Sprintf("%s<%s@%d", n.name, n.parent, n.level))
		}
		trees = append(trees, fmt.Sprintf("%s %t %v %v", tr.key(), tr.standard, nodes, g.leaves[tr.key()]))
	}
	want := []string{
		"0101/A1/STD true [STD<@1 ADM<STD@2 SAL<STD@2] map[C1:ADM C2:SAL]",
		"0106/A1/PSTD true [PSTD<@1 PL<PSTD@2] map[P1:PL]",
		"0101/A1/ALT false [ALT<@1 X<ALT@2] map[C1:X C2:X]",
	}
	if fmt.Sprint(trees) != fmt.Sprint(want) {
		t.Errorf("trees:\n got %q\nwant %q", trees, want)
	}
}

// TestGroupQualityGate: the gate refuses groups the platform cannot hold
// or that place a center in other than one standard leaf, and warns of
// what places nothing.
func TestGroupQualityGate(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(x *sap.Extract)
		want   string
	}{
		"no standard set": {func(x *sap.Extract) { x.TKA01[0].KHINR = "NONE" }, "error TKA01 A1: standard hierarchy NONE (KHINR) is not a cost center group"},
		"no standard hierarchy": {func(x *sap.Extract) { x.TKA01 = nil },
			"warning SETHEADER 0101/A1: controlling area A1 has groups of class 0101 but no standard hierarchy"},
		"a set code":      {func(x *sap.Extract) { x.SETHEADER[4].SETNAME = "x y"; x.SETNODE[2].SUBSETNAME = "x y" }, `error SETHEADER 0101/A1/x y: set "x y" is not a code`},
		"no set text":     {func(x *sap.Extract) { x.SETHEADERT[1].DESCRIPT = "" }, "error SETHEADER 0101/A1/ADM: no text with a name (SETHEADERT)"},
		"a German text":   {func(x *sap.Extract) { x.SETHEADERT[1].LANGU = "D" }, `warning SETHEADER 0101/A1/ADM: no text in English; it loads named "Group ADM"`},
		"subsets, values": {func(x *sap.Extract) { x.SETLEAF[0].SETNAME = "STD" }, "error SETHEADER 0101/A1/STD: set STD has both subsets and values"},
		"a node of no set": {func(x *sap.Extract) { x.SETNODE[0].SETNAME = "NOPE" },
			"error SETNODE 0101/A1/NOPE/1: set NOPE is not in SETHEADER"},
		"a subset of another class": {func(x *sap.Extract) { x.SETNODE[0].SUBSETCLS = "0106" }, "error SETNODE 0101/A1/STD/1: subset ADM is of class 0106"},
		"an unknown subset":         {func(x *sap.Extract) { x.SETNODE[0].SUBSETNAME = "NOPE" }, "error SETNODE 0101/A1/STD/1: subset NOPE is not in SETHEADER"},
		"a value of no set":         {func(x *sap.Extract) { x.SETLEAF[0].SETNAME = "NOPE" }, "error SETLEAF 0101/A1/NOPE/1: set NOPE is not in SETHEADER"},
		"a sign":                    {func(x *sap.Extract) { x.SETLEAF[0].VALSIGN = "X" }, `error SETLEAF 0101/A1/SAL/1: VALSIGN "X" is neither I`},
		"an option":                 {func(x *sap.Extract) { x.SETLEAF[0].VALOPTION = "CP" }, `error SETLEAF 0101/A1/SAL/1: VALOPTION "CP" is neither EQ`},
		"a range backwards":         {func(x *sap.Extract) { x.SETLEAF[0].VALTO = "C1" }, "error SETLEAF 0101/A1/SAL/1: range C2 to C1 ends before it starts"},
		"a cycle": {func(x *sap.Extract) {
			withSets(x, set{"0101", "Y1", []string{"Y2"}, nil}, set{"0101", "Y2", []string{"Y1"}, nil})
		}, "error SETHEADER 0101/A1/Y1: set Y1 is in no tree: the sets above it include each other"},
		"a cycle below a root": {func(x *sap.Extract) { withSets(x, set{"0101", "ADM", []string{"STD"}, nil}) },
			"error SETHEADER 0101/A1/STD: set STD includes itself through its subsets STD > ADM"},
		"a set twice": {func(x *sap.Extract) { withSets(x, set{"0101", "SAL", []string{"ADM"}, nil}) },
			"error SETHEADER 0101/A1/ADM: set ADM is twice in the hierarchy below STD"},
		"too deep": {func(x *sap.Extract) {
			for i := 1; i <= 12; i++ {
				withSets(x, set{"0101", fmt.Sprint("D", i), []string{fmt.Sprint("D", i+1)}, nil})
			}
			withSets(x, set{"0101", "D13", nil, nil})
		}, "error SETHEADER 0101/A1/D1: the hierarchy below D1 is deeper than the 12 levels the platform holds"},
		"two leaves": {func(x *sap.Extract) { x.SETLEAF[0].VALFROM = "C1" }, "error CSKS A1/C1: it is in leaves SAL, ADM of the hierarchy below STD"},
		"no leaf":    {func(x *sap.Extract) { x.CSKS[0].KHINR = "" }, "error CSKS A1/C1: it is in no leaf of the standard hierarchy below STD"},
		"a KHINR not a leaf": {func(x *sap.Extract) { x.CSKS[0].KHINR = "STD" },
			"error CSKS A1/C1: its standard leaf STD (KHINR) is not a leaf of the standard hierarchy below STD"},
		"two alternative leaves": {func(x *sap.Extract) {
			withSets(x, set{"0101", "ALT", []string{"Z"}, nil}, set{"0101", "Z", nil, [][4]string{{"I", "EQ", "C1", ""}}})
		},
			"error CSKS A1/C1: it is in leaves X, Z of the hierarchy below ALT"},
		"a value matching nothing": {func(x *sap.Extract) { x.SETLEAF[1].VALFROM, x.SETLEAF[1].VALTO = "D0", "D9" },
			"warning SETLEAF 0101/A1/X/1: value D0 to D9 matches no center of controlling area A1 in the extract"},
		"an unknown profit center leaf": {func(x *sap.Extract) { x.CEPC[0].KHINR = "NOPE" },
			"error CEPC 0106/A1: the profit centers' standard leaves (KHINR) NOPE are not in one tree"},
		"one value matching nothing": {func(x *sap.Extract) { withSets(x, set{"0101", "X", nil, [][4]string{{"I", "EQ", "D1", ""}}}) },
			"warning SETLEAF 0101/A1/X/1: value D1 matches no center"},
		"profit center leaves in two trees": {func(x *sap.Extract) {
			x.CEPC = append(x.CEPC, sap.CEPC{KOKRS: "A1", PRCTR: "P2", DATAB: "20200101", DATBI: "99991231", KHINR: "PL2", VERAK: "V"})
			withSets(x, set{"0106", "PSTD2", []string{"PL2"}, nil}, set{"0106", "PL2", nil, nil})
		}, "error CEPC 0106/A1: the profit centers' standard leaves (KHINR) PL, PL2 are not in one tree"},
		"a set in two areas' standard trees": {func(x *sap.Extract) {
			x.TKA01 = append(x.TKA01, sap.TKA01{KOKRS: "A2", KHINR: "STD"})
			x.SETHEADER = append(x.SETHEADER, sap.SETHEADER{SETCLASS: "0101", SUBCLASS: "A2", SETNAME: "STD"})
			x.SETHEADERT = append(x.SETHEADERT, sap.SETHEADERT{SETCLASS: "0101", SUBCLASS: "A2", SETNAME: "STD", LANGU: "E", DESCRIPT: "S"})
		}, "error SETHEADER 0101/STD: set STD of class 0101 is in the standard hierarchies of controlling areas A1, A2"},
		"a root in two areas": {func(x *sap.Extract) {
			x.SETHEADER = append(x.SETHEADER, sap.SETHEADER{SETCLASS: "0101", SUBCLASS: "A2", SETNAME: "ALT"})
			x.SETHEADERT = append(x.SETHEADERT, sap.SETHEADERT{SETCLASS: "0101", SUBCLASS: "A2", SETNAME: "ALT", LANGU: "E", DESCRIPT: "S"})
		}, "error SETHEADER 0101/ALT: set ALT of class 0101 is in the alternative hierarchies' roots of controlling areas A1, A2"},
	} {
		t.Run(name, func(t *testing.T) {
			x := gateGroups()
			tc.change(x)
			var got []string
			for _, i := range gate(x) {
				got = append(got, fmt.Sprintf("%s %s %s: %s", i.Severity, i.Table, i.Key, i.Message))
			}
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Errorf("issues: got %q, want one starting %q", got, tc.want)
			}
		})
	}
}

// TestGroupValues: values compare as SAP keeps codes, digits alone padded
// to ten; an exclusion wins; a set of another class is not a group.
func TestGroupValues(t *testing.T) {
	s := &sapSet{values: []sap.SETLEAF{{VALSIGN: "I", VALOPTION: "BT", VALFROM: "900", VALTO: "1000"}, {VALSIGN: "E", VALOPTION: "EQ", VALFROM: "0000000950"}}}
	for code, want := range map[string]bool{"900": true, "0000001000": true, "950": false, "9000": false, "A900": false} {
		t.Run(code, func(t *testing.T) {
			if got := s.includes(code); got != want {
				t.Errorf("got %t, want %t", got, want)
			}
		})
	}
	if got, want := alphaIn("ABCDEFGHIJK"), "ABCDEFGHIJK"; got != want {
		t.Errorf("a code longer than ten: got %s, want %s", got, want)
	}
	x := gateGroups()
	x.SETHEADER = append(x.SETHEADER, sap.SETHEADER{SETCLASS: "0102", SUBCLASS: "A1", SETNAME: "ORDERS"})
	x.SETNODE = append(x.SETNODE, sap.SETNODE{SETCLASS: "0102", SUBCLASS: "A1", SETNAME: "ORDERS", SUBSETNAME: "O1"})
	x.SETLEAF = append(x.SETLEAF, sap.SETLEAF{SETCLASS: "0102", SUBCLASS: "A1", SETNAME: "ORDERS", VALSIGN: "I", VALOPTION: "EQ", VALFROM: "1"})
	if g := planGroups(x); len(g.issues) != 0 || len(g.trees) != 3 {
		t.Errorf("a set of class 0102: got %v and %d trees, want it left out", g.issues, len(g.trees))
	}
}
