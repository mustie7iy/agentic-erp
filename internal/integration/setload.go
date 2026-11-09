package integration

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// setRef is the external reference of a set loaded as a node of a tree
// (object SETHEADER, key class/area/root/set), and of an alternative
// hierarchy (key class/area/root): one set may be a node of several trees.
func setRef(t groupTree, name string) string { return "SETHEADER/" + t.key() + "/" + name }

func (l *loader) groupReason(t groupTree) string {
	return "Migration run " + l.run.ID.String() + ": SAP " + t.kind() + " group " + t.key()
}

// groups loads the extract's center groups before the centers, which are
// created at their standard leaf (standardNode): each tree's sets not yet
// loaded are added as nodes below their parents, an alternative
// hierarchy created with its root set (ADR-0050 §10, T56h2). What an
// earlier run loaded is kept: a set it loaded is not renamed or moved,
// and a center it loaded keeps its places.
func (l *loader) groups(ctx context.Context) error {
	l.plan = planGroups(l.ext)
	for _, t := range l.plan.trees {
		var added []string
		done, err := l.step(ctx, "SETHEADER", t.key(), func() error { return l.groupTree(ctx, t, &added) })
		if !done {
			for _, k := range added { // rolled back with the step
				delete(l.refs, k)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// groupTree loads one tree: its hierarchy, and its sets not yet loaded as
// nodes, each in the order of the tree.
func (l *loader) groupTree(ctx context.Context, t groupTree, added *[]string) error {
	if l.loadedTree(t) {
		return nil
	}
	var h, root uuid.UUID
	var err error
	if t.standard {
		h, root, err = l.e.Centers.StandardHierarchy(ctx, t.kind())
	} else {
		h, root, err = l.alternative(ctx, t, added)
	}
	if err != nil {
		return err
	}
	var nodes []ledger.MigratedNode
	for _, n := range t.nodes {
		if _, ok := l.refs[setRef(t, n.name)]; ok {
			continue
		}
		parent := root
		if n.parent != "" {
			parent = l.refs[setRef(t, n.parent)]
		}
		id := uuid.Must(uuid.NewV7())
		nodes = append(nodes, ledger.MigratedNode{ID: id, Parent: parent, Code: n.name, Name: l.setName(t, n.name)})
		if err := l.addRef(ctx, "ledger.hierarchy_node", id, "SETHEADER", t.key()+"/"+n.name); err != nil {
			return err
		}
		*added = append(*added, setRef(t, n.name))
	}
	if len(nodes) == 0 {
		return nil
	}
	if err := l.e.Centers.AddNodes(ctx, h, nodes, l.groupReason(t)); err != nil {
		return err
	}
	l.counts.HierarchyNodes += len(nodes)
	return nil
}

// loadedTree reports whether every set of the tree is loaded.
func (l *loader) loadedTree(t groupTree) bool {
	for _, n := range t.nodes {
		if _, ok := l.refs[setRef(t, n.name)]; !ok {
			return false
		}
	}
	return true
}

// alternative is an alternative tree's hierarchy and root node: an
// earlier run's, or created with its root set's code and name.
func (l *loader) alternative(ctx context.Context, t groupTree, added *[]string) (h, root uuid.UUID, err error) {
	if h, ok := l.refs["SETHEADER/"+t.key()]; ok {
		return h, l.refs[setRef(t, t.root)], nil
	}
	h, root, err = l.e.Centers.CreateHierarchy(ctx, ledger.NewHierarchy{Kind: t.kind(), Code: t.root, Name: l.setName(t, t.root)})
	if err != nil {
		return h, root, err
	}
	if err := l.addRef(ctx, "ledger.hierarchy", h, "SETHEADER", t.key()); err != nil {
		return h, root, err
	}
	if err := l.addRef(ctx, "ledger.hierarchy_node", root, "SETHEADER", t.key()+"/"+t.root); err != nil {
		return h, root, err
	}
	*added = append(*added, "SETHEADER/"+t.key(), setRef(t, t.root))
	l.counts.Hierarchies++
	l.counts.HierarchyNodes++
	return h, root, nil
}

// setName is a set's name (pickText; the quality gate refused a set
// without one).
func (l *loader) setName(t groupTree, name string) string {
	text, _, _ := pickText(l.plan.texts[setKey(t.class, t.area, name)])
	return text.name
}

// standardNode is the node of the center's standard leaf, nil when its
// area has no standard tree: the UNASSIGNED leaf takes it.
func (l *loader) standardNode(class, area, code string) *uuid.UUID {
	for _, t := range l.plan.trees {
		if !t.standard || t.class != class || t.area != area {
			continue
		}
		if id, ok := l.refs[setRef(t, l.plan.leaves[t.key()][code])]; ok {
			return &id
		}
	}
	return nil
}

// groupPlaces places the centers this run created at their leaves of the
// alternative hierarchies, after the centers.
func (l *loader) groupPlaces(ctx context.Context) error {
	for _, t := range l.plan.trees {
		h, ok := l.refs["SETHEADER/"+t.key()]
		if t.standard || !ok {
			continue
		}
		var places []ledger.MigratedPlace
		for _, code := range sortedKeys(l.plan.leaves[t.key()]) {
			if center, ok := l.created[t.class+"/"+t.area+"/"+code]; ok {
				places = append(places, ledger.MigratedPlace{Center: center, Node: l.refs[setRef(t, l.plan.leaves[t.key()][code])]})
			}
		}
		if len(places) == 0 {
			continue
		}
		done, err := l.step(ctx, "SETLEAF", t.key(), func() error { return l.e.Centers.PlaceCenters(ctx, h, places, l.groupReason(t)) })
		if done {
			l.counts.HierarchyPlaces += len(places)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
