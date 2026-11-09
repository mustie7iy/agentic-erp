package integration

import (
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// standardRoots are the root sets of the areas' standard hierarchies by
// class/area: TKA01-KHINR for cost centers, and for profit centers the
// one root above every set their KHINR names. An area with centers and
// groups of their class but no standard hierarchy is warned of: its
// centers go to the standard hierarchy's UNASSIGNED leaf, as without
// groups.
func (g *groupPlan) standardRoots(e *sap.Extract, centers map[string]*areaCenters) map[string]string {
	out := map[string]string{}
	for _, r := range e.TKA01 {
		if g.sets[setKey(sap.SetClassCostCenters, r.KOKRS, r.KHINR)] == nil {
			g.add(SevError, "TKA01", r.KOKRS, "standard hierarchy %s (KHINR) is not a cost center group (set of class 0101) of controlling area %s", r.KHINR, r.KOKRS)
			continue
		}
		out[sap.SetClassCostCenters+"/"+r.KOKRS] = r.KHINR
	}
	for _, k := range sortedKeys(centers) {
		class, area, _ := strings.Cut(k, "/")
		if class == sap.SetClassProfitCenters {
			if root, ok := g.profitCenterRoot(area, centers[k]); ok {
				out[k] = root
			}
		}
		if out[k] == "" && g.hasSets(class, area) {
			g.add(SevWarning, "SETHEADER", k, "controlling area %s has groups of class %s but no standard hierarchy (TKA01-KHINR, CEPC-KHINR); its centers load at the standard hierarchy's UNASSIGNED leaf",
				area, class)
		}
	}
	return out
}

func (g *groupPlan) hasSets(class, area string) bool {
	for _, s := range g.sets {
		if s.class == class && s.area == area {
			return true
		}
	}
	return false
}

// profitCenterRoot is the root of the one tree holding every set the
// area's profit centers name as their standard leaf (CEPC-KHINR); ok is
// false when they name none, or no single tree holds them (an error).
func (g *groupPlan) profitCenterRoot(area string, c *areaCenters) (string, bool) {
	var named []string
	for _, code := range c.codes {
		if kh := c.khinr[code]; kh != "" && !slices.Contains(named, kh) {
			named = append(named, kh)
		}
	}
	if len(named) == 0 {
		return "", false
	}
	slices.Sort(named)
	var common []string
	for i, name := range named {
		roots := g.rootsOf(sap.SetClassProfitCenters, area, name, nil)
		if i == 0 {
			common = roots
		}
		common = slices.DeleteFunc(common, func(r string) bool { return !slices.Contains(roots, r) })
	}
	if len(common) != 1 {
		g.add(SevError, "CEPC", sap.SetClassProfitCenters+"/"+area,
			"the profit centers' standard leaves (KHINR) %s are not in one tree of profit center groups of controlling area %s", strings.Join(named, ", "), area)
		return "", false
	}
	return common[0], true
}

// rootsOf are the sets without a parent above a set (itself if it has
// none), sorted; a set not in SETHEADER has none. path guards against a
// cycle, which unreached reports.
func (g *groupPlan) rootsOf(class, area, name string, path []string) []string {
	if g.sets[setKey(class, area, name)] == nil || slices.Contains(path, name) {
		return nil
	}
	parents := g.parents(class, area, name)
	if len(parents) == 0 {
		return []string{name}
	}
	var out []string
	for _, p := range parents {
		for _, r := range g.rootsOf(class, area, p, append(path, name)) {
			if !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	slices.Sort(out)
	return out
}

// parents are the sets of the class and area that include the set.
func (g *groupPlan) parents(class, area, name string) []string {
	var out []string
	for _, k := range sortedKeys(g.sets) {
		if s := g.sets[k]; s.class == class && s.area == area && slices.Contains(s.subsets, name) {
			out = append(out, s.name)
		}
	}
	return out
}

// walk is the tree below a root set: a set that includes itself through
// its subsets, or is twice in the tree, or a tree deeper than the
// platform holds is refused, and the walk does not go below it.
func (g *groupPlan) walk(class, area, root string, standard bool, reached map[string]bool) groupTree {
	t := groupTree{class: class, area: area, root: root, standard: standard}
	limit := maxSetLevels
	if standard {
		limit-- // below the platform's standard root
	}
	var visit func(name, parent string, path []string)
	visit = func(name, parent string, path []string) {
		k := setKey(class, area, name)
		switch {
		case slices.Contains(path, name):
			g.add(SevError, "SETHEADER", k, "set %s includes itself through its subsets %s", name, strings.Join(path, " > "))
			return
		case slices.ContainsFunc(t.nodes, func(n groupNode) bool { return n.name == name }):
			g.add(SevError, "SETHEADER", k, "set %s is twice in the hierarchy below %s; a node is once in a tree", name, root)
			return
		case len(path) >= limit:
			g.add(SevError, "SETHEADER", setKey(class, area, root), "the hierarchy below %s is deeper than the %d levels the platform holds", root, limit)
			return
		}
		reached[k] = true
		t.nodes = append(t.nodes, groupNode{name: name, parent: parent, level: len(path) + 1})
		for _, sub := range g.sets[k].subsets {
			visit(sub, name, append(slices.Clone(path), name))
		}
	}
	visit(root, "", nil)
	return t
}

// alternativeTrees walks every set no set includes, other than a standard
// root, as an alternative hierarchy, and refuses a set no tree reaches,
// which only a cycle of subsets leaves.
func (g *groupPlan) alternativeTrees(standard map[string]string, reached map[string]bool) {
	for _, k := range sortedKeys(g.sets) {
		s := g.sets[k]
		if standard[s.class+"/"+s.area] != s.name && len(g.parents(s.class, s.area, s.name)) == 0 {
			g.trees = append(g.trees, g.walk(s.class, s.area, s.name, false, reached))
		}
	}
	for _, k := range sortedKeys(g.sets) {
		if !reached[k] {
			g.add(SevError, "SETHEADER", k, "set %s is in no tree: the sets above it include each other", g.sets[k].name)
		}
	}
}

// collisions refuses what one tenant cannot hold for several controlling
// areas: the standard hierarchies of a kind are one tree, so a set name in
// two of them would be two nodes of one code, and alternative hierarchies
// of a kind are coded by their root set.
func (g *groupPlan) collisions() {
	nodes, roots := map[string][]string{}, map[string][]string{}
	for _, t := range g.trees {
		if !t.standard {
			roots[t.class+"/"+t.root] = append(roots[t.class+"/"+t.root], t.area)
			continue
		}
		for _, n := range t.nodes {
			nodes[t.class+"/"+n.name] = append(nodes[t.class+"/"+n.name], t.area)
		}
	}
	for _, c := range []struct {
		what string
		m    map[string][]string
	}{{"standard hierarchies", nodes}, {"alternative hierarchies' roots", roots}} {
		what, m := c.what, c.m
		for _, k := range sortedKeys(m) {
			if areas := m[k]; len(areas) > 1 {
				class, name, _ := strings.Cut(k, "/")
				g.add(SevError, "SETHEADER", k, "set %s of class %s is in the %s of controlling areas %s; the tenant holds one of a code per kind",
					name, class, what, strings.Join(areas, ", "))
			}
		}
	}
}

// members places each center of the tree's area at its leaf (leavesOf).
// A center in more than one leaf, or in none of the standard tree, is
// refused.
func (g *groupPlan) members(t groupTree, c *areaCenters) {
	if c == nil {
		return
	}
	table := map[string]string{sap.SetClassCostCenters: "CSKS", sap.SetClassProfitCenters: "CEPC"}[t.class]
	in := g.leavesOf(t, c, table)
	g.leaves[t.key()] = map[string]string{}
	for _, code := range c.codes {
		switch leaves := in[code]; {
		case len(leaves) == 1:
			g.leaves[t.key()][code] = leaves[0]
		case len(leaves) > 1:
			g.add(SevError, table, t.area+"/"+code, "it is in leaves %s of the hierarchy below %s; a center sits at one leaf of a hierarchy",
				strings.Join(leaves, ", "), t.root)
		case t.standard:
			g.add(SevError, table, t.area+"/"+code, "it is in no leaf of the standard hierarchy below %s (its KHINR, or a value of a group)", t.root)
		}
	}
}

// leavesOf are the leaves of the tree each center sits at: those whose
// values take it and, in a standard tree, the leaf its KHINR names, which
// must be one.
func (g *groupPlan) leavesOf(t groupTree, c *areaCenters, table string) map[string][]string {
	in := map[string][]string{}
	for _, n := range t.nodes {
		if s := g.sets[setKey(t.class, t.area, n.name)]; len(s.subsets) == 0 {
			for _, code := range c.codes {
				if s.includes(code) {
					in[code] = append(in[code], n.name)
				}
			}
		}
	}
	if !t.standard {
		return in
	}
	for _, code := range c.codes {
		switch kh := c.khinr[code]; {
		case kh == "" || slices.Contains(in[code], kh):
		case !t.leaf(g.sets, kh):
			g.add(SevError, table, t.area+"/"+code, "its standard leaf %s (KHINR) is not a leaf of the standard hierarchy below %s", kh, t.root)
		default:
			in[code] = append(in[code], kh)
		}
	}
	return in
}

// unmatched warns of a value including no center of the extract: a group
// may hold a range for centers not yet created, which places nothing.
func (g *groupPlan) unmatched(centers map[string]*areaCenters) {
	for _, k := range sortedKeys(g.sets) {
		s := g.sets[k]
		var codes []string
		if c := centers[s.class+"/"+s.area]; c != nil {
			codes = c.codes
		}
		for _, v := range s.values {
			if v.VALSIGN == "I" && !slices.ContainsFunc(codes, func(code string) bool { return valueMatches(v, code) }) {
				g.add(SevWarning, "SETLEAF", k+"/"+v.LINEID, "value %s matches no center of controlling area %s in the extract; it places nothing", valueText(v), s.area)
			}
		}
	}
}

func valueText(v sap.SETLEAF) string {
	if v.VALOPTION == "EQ" {
		return v.VALFROM
	}
	return v.VALFROM + " to " + v.VALTO
}
