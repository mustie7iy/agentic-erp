package integration

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Center groups (ADR-0050 §10, T56h2): SAP's sets of class 0101 (cost
// center groups) and 0106 (profit center groups) of a controlling area
// become the ledger's hierarchies. The area's standard hierarchy is the
// tree below the set TKA01-KHINR names for cost centers; for profit
// centers, the one tree holding the sets their CEPC-KHINR name (the
// extract reads no field naming it). Each other set no set includes tops
// an alternative hierarchy. A set's values (SETLEAF) are ranges, expanded
// against the extract's centers of its area; a cost or profit center
// sits at the standard groups its ranges or its KHINR name, which must be
// exactly one, and at most once in an alternative hierarchy. A set may hold
// values beside its subsets, and a KHINR may name a set that has subsets
// (the SAP specialist, po:2026-11-09, A10(d) [M]), so a center sits at
// that inner node, which the ledger allows (ADR-0050 §3 as amended).

// nodeCode is the form of a hierarchy's and a node's code (ADR-0050 §3 as
// built in T56b): up to 24 characters, as SETHEADER-SETNAME holds (the SAP
// specialist, po:2026-11-09, A11(c); migration 000248).
var nodeCode = regexp.MustCompile(`^[0-9A-Z_-]{1,24}$`)

// setKinds are the hierarchy kinds by set class.
var setKinds = map[string]string{sap.SetClassCostCenters: ledger.HierarchyCostCenters, sap.SetClassProfitCenters: ledger.HierarchyProfitCenters}

// maxSetLevels is the deepest SAP tree an alternative hierarchy takes
// (ledger: 12 levels, its root at 1); a standard one sits below the
// standard hierarchy's root, so takes one level less.
const maxSetLevels = 12

// sapSet is a set of the extract with its subsets and values in line
// order.
type sapSet struct {
	class, area, name string
	subsets           []string
	values            []sap.SETLEAF
}

func setKey(class, area, name string) string { return class + "/" + area + "/" + name }

func (s *sapSet) key() string { return setKey(s.class, s.area, s.name) }

// includes reports whether the set's values take the center: a value
// including it and none excluding it.
func (s *sapSet) includes(code string) bool {
	in := false
	for _, v := range s.values {
		if valueMatches(v, code) {
			if v.VALSIGN == "E" {
				return false
			}
			in = true
		}
	}
	return in
}

// valueMatches reports whether a value (EQ: VALFROM, BT: VALFROM to VALTO,
// both included) holds the code, compared as SAP keeps them (alphaIn).
func valueMatches(v sap.SETLEAF, code string) bool {
	c, from := alphaIn(code), alphaIn(v.VALFROM)
	if v.VALOPTION == "EQ" {
		return c == from
	}
	return from <= c && c <= alphaIn(v.VALTO)
}

// alphaIn is a center code in SAP's internal form, as sets compare it: a
// code of digits alone padded with leading zeros to the field's ten
// characters (the ALPHA conversion), any other as it is.
func alphaIn(code string) string {
	if code == "" || len(code) > 10 || strings.Trim(code, "0123456789") != "" {
		return code
	}
	return strings.Repeat("0", 10-len(code)) + code
}

// lineOrder orders a set's lines by LINEID, a number.
func lineOrder(a, b string) int { return cmp.Compare(alphaIn(a), alphaIn(b)) }

// groupNode is a set's place in a tree: below its parent ("" for the
// root), at level (the root's 1).
type groupNode struct {
	name, parent string
	level        int
}

// groupTree is a hierarchy SAP's sets make: its root set and the sets
// below it, each after its parent, siblings in line order.
type groupTree struct {
	class, area, root string
	standard          bool
	nodes             []groupNode
}

func (t groupTree) key() string  { return setKey(t.class, t.area, t.root) }
func (t groupTree) kind() string { return setKinds[t.class] }

// has reports whether the set is a node of the tree.
func (t groupTree) has(name string) bool {
	return slices.ContainsFunc(t.nodes, func(n groupNode) bool { return n.name == name })
}

// groupPlan is what the extract's sets say: the sets, their texts, the
// trees and, by tree key, the group of each center (area/code) that is in
// exactly one group of it; issues are the quality gate's.
type groupPlan struct {
	sets   map[string]*sapSet
	texts  map[string][]sapText
	trees  []groupTree
	leaves map[string]map[string]string
	issues []Issue
}

// add records an issue once: a set in several trees is walked in each.
func (g *groupPlan) add(sev, table, key, format string, args ...any) {
	i := Issue{Severity: sev, Table: table, Key: key, Message: fmt.Sprintf(format, args...)}
	if !slices.Contains(g.issues, i) {
		g.issues = append(g.issues, i)
	}
}

// planGroups reads the extract's center groups. The quality gate reports
// its issues; the load, once the gate passed, builds its trees.
func planGroups(e *sap.Extract) *groupPlan {
	g := &groupPlan{sets: map[string]*sapSet{}, texts: map[string][]sapText{}, leaves: map[string]map[string]string{}}
	g.readSets(e)
	centers := groupCenters(e)
	standard, reached := g.standardRoots(e, centers), map[string]bool{}
	for _, k := range sortedKeys(standard) {
		class, area, _ := strings.Cut(k, "/")
		g.trees = append(g.trees, g.walk(class, area, standard[k], true, reached))
	}
	g.alternativeTrees(standard, reached)
	g.collisions()
	for _, t := range g.trees {
		g.members(t, centers[t.class+"/"+t.area])
	}
	g.unmatched(centers)
	return g
}

// readSets indexes the sets of classes 0101 and 0106 with their texts,
// subsets and values, refusing lines of a set the extract does not have.
func (g *groupPlan) readSets(e *sap.Extract) {
	for _, h := range e.SETHEADER {
		if setKinds[h.SETCLASS] != "" && g.sets[setKey(h.SETCLASS, h.SUBCLASS, h.SETNAME)] == nil {
			g.sets[setKey(h.SETCLASS, h.SUBCLASS, h.SETNAME)] = &sapSet{class: h.SETCLASS, area: h.SUBCLASS, name: h.SETNAME}
		}
	}
	for _, t := range e.SETHEADERT {
		k := setKey(t.SETCLASS, t.SUBCLASS, t.SETNAME)
		g.texts[k] = append(g.texts[k], sapText{lang: t.LANGU, name: t.DESCRIPT})
	}
	nodes := slices.SortedStableFunc(slices.Values(e.SETNODE), func(a, b sap.SETNODE) int { return lineOrder(a.LINEID, b.LINEID) })
	for _, n := range nodes {
		g.subset(n)
	}
	leaves := slices.SortedStableFunc(slices.Values(e.SETLEAF), func(a, b sap.SETLEAF) int { return lineOrder(a.LINEID, b.LINEID) })
	for _, v := range leaves {
		g.value(v)
	}
	for _, k := range sortedKeys(g.sets) {
		g.checkSet(g.sets[k])
	}
}

func (g *groupPlan) subset(n sap.SETNODE) {
	k := setKey(n.SETCLASS, n.SUBCLASS, n.SETNAME)
	s := g.sets[k]
	switch {
	case setKinds[n.SETCLASS] == "":
		return
	case s == nil:
		g.add(SevError, "SETNODE", k+"/"+n.LINEID, "set %s is not in SETHEADER", n.SETNAME)
	case n.SUBSETCLS != n.SETCLASS || n.SUBSETSCLS != n.SUBCLASS:
		g.add(SevError, "SETNODE", k+"/"+n.LINEID, "subset %s is of class %s and subclass %s; a set of class %s and controlling area %s takes its own class and area's",
			n.SUBSETNAME, n.SUBSETCLS, n.SUBSETSCLS, n.SETCLASS, n.SUBCLASS)
	case g.sets[setKey(n.SETCLASS, n.SUBCLASS, n.SUBSETNAME)] == nil:
		g.add(SevError, "SETNODE", k+"/"+n.LINEID, "subset %s is not in SETHEADER", n.SUBSETNAME)
	default:
		s.subsets = append(s.subsets, n.SUBSETNAME)
	}
}

func (g *groupPlan) value(v sap.SETLEAF) {
	k := setKey(v.SETCLASS, v.SUBCLASS, v.SETNAME)
	s := g.sets[k]
	switch {
	case setKinds[v.SETCLASS] == "":
		return
	case s == nil:
		g.add(SevError, "SETLEAF", k+"/"+v.LINEID, "set %s is not in SETHEADER", v.SETNAME)
	case v.VALSIGN != "I" && v.VALSIGN != "E":
		g.add(SevError, "SETLEAF", k+"/"+v.LINEID, "VALSIGN %q is neither I (include) nor E (exclude)", v.VALSIGN)
	case v.VALOPTION != "EQ" && v.VALOPTION != "BT":
		g.add(SevError, "SETLEAF", k+"/"+v.LINEID, "VALOPTION %q is neither EQ (one value) nor BT (a range); the platform expands no other", v.VALOPTION)
	case v.VALOPTION == "BT" && alphaIn(v.VALTO) < alphaIn(v.VALFROM):
		g.add(SevError, "SETLEAF", k+"/"+v.LINEID, "range %s to %s ends before it starts", v.VALFROM, v.VALTO)
	default:
		s.values = append(s.values, v)
	}
}

// checkSet refuses a set the platform cannot hold as a node: a code that
// is not one, or no name.
func (g *groupPlan) checkSet(s *sapSet) {
	if !nodeCode.MatchString(s.name) {
		g.add(SevError, "SETHEADER", s.key(), "set %q is not a code the platform holds (up to 24 of 0-9, A-Z, _ and -)", s.name)
	}
	t, isEnglish, ok := pickText(g.texts[s.key()])
	switch {
	case !ok:
		g.add(SevError, "SETHEADER", s.key(), "no text with a name (SETHEADERT); the platform needs a name")
	case !isEnglish:
		g.add(SevWarning, "SETHEADER", s.key(), "no text in English; it loads named %q, its text in language %s", t.name, t.lang)
	}
}

// areaCenters are a controlling area's centers of one class: their codes
// and the standard group each names (KHINR of its latest slice).
type areaCenters struct {
	codes []string
	khinr map[string]string
}

// groupCenters are the extract's centers by set class and area.
func groupCenters(e *sap.Extract) map[string]*areaCenters {
	out := map[string]*areaCenters{}
	add := func(class, area, code, khinr string) {
		k := class + "/" + area
		if out[k] == nil {
			out[k] = &areaCenters{khinr: map[string]string{}}
		}
		out[k].codes = append(out[k].codes, code)
		out[k].khinr[code] = khinr
	}
	for _, c := range costCenters(e) {
		add(sap.SetClassCostCenters, c.area, c.code, c.slices[len(c.slices)-1].KHINR)
	}
	for _, c := range profitCenters(e) {
		add(sap.SetClassProfitCenters, c.area, c.code, c.slices[len(c.slices)-1].KHINR)
	}
	return out
}
