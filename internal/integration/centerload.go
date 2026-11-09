package integration

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Centers creates the controlling master data a run loads and reads it
// back for the reconciliation (ledger.CenterMigration).
type Centers interface {
	CreateSegment(ctx context.Context, in ledger.NewSegment) (uuid.UUID, error)
	CreateProfitCenter(ctx context.Context, in ledger.MigratedProfitCenter) (uuid.UUID, error)
	AssignProfitCenter(ctx context.Context, id uuid.UUID, legalEntities []uuid.UUID, reason string) error
	LockProfitCenters(ctx context.Context, ids []uuid.UUID) error
	CreateCostCenter(ctx context.Context, in ledger.MigratedCostCenter) (uuid.UUID, error)
	StandardHierarchy(ctx context.Context, kind string) (hierarchy, root uuid.UUID, err error)
	CreateHierarchy(ctx context.Context, in ledger.NewHierarchy) (hierarchy, root uuid.UUID, err error)
	AddNodes(ctx context.Context, hierarchy uuid.UUID, nodes []ledger.MigratedNode, reason string) error
	PlaceCenters(ctx context.Context, hierarchy uuid.UUID, places []ledger.MigratedPlace, reason string) error
	CostCenters(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ledger.MigratedCostCenterState, error)
	ProfitCenters(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ledger.MigratedProfitCenterState, error)
	Segments(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// centers loads the segments, the center groups, the profit centers and
// the cost centers, in that order, as each names the ones before (ADR-0050
// §10), and last places the centers in the alternative hierarchies. What
// an earlier run loaded is kept as it is: a profit center is only assigned
// the run's companies it is not yet assigned to, and a difference SAP made
// since shows in the reconciliation.
//
// Creating a center locks its standard hierarchy, and the API locks a
// center before its hierarchy, so the load first locks every profit center
// an earlier run loaded that this one assigns or names (lockLoaded).
func (l *loader) centers(ctx context.Context) error {
	if err := l.lockLoaded(ctx); err != nil {
		return err
	}
	texts := centerTexts(l.ext)
	for _, s := range l.ext.FAGL_SEGM {
		if err := l.segment(ctx, s.SEGMENT, texts); err != nil {
			return err
		}
	}
	if err := l.groups(ctx); err != nil {
		return err
	}
	for _, pc := range profitCenters(l.ext) {
		if err := l.profitCenter(ctx, pc, texts); err != nil {
			return err
		}
	}
	for _, cc := range costCenters(l.ext) {
		if err := l.costCenter(ctx, cc, texts); err != nil {
			return err
		}
	}
	return l.groupPlaces(ctx)
}

// lockLoaded locks the profit centers earlier runs loaded that the
// extract's profit centers and cost centers name.
func (l *loader) lockLoaded(ctx context.Context) error {
	var ids []uuid.UUID
	for _, r := range l.ext.CEPC {
		if id, ok := l.refs["CEPC/"+r.KOKRS+"/"+r.PRCTR]; ok {
			ids = append(ids, id)
		}
	}
	for _, r := range l.ext.CSKS {
		if id, ok := l.refs["CEPC/"+r.KOKRS+"/"+r.PRCTR]; ok && r.PRCTR != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return l.e.Centers.LockProfitCenters(ctx, ids)
}

func (l *loader) segment(ctx context.Context, code string, texts map[string][]sapText) error {
	if _, ok := l.refs["FAGL_SEGM/"+code]; ok {
		return nil
	}
	t, _, _ := pickText(texts["FAGL_SEGM/"+code]) // the quality gate refused a segment without a name
	done, err := l.step(ctx, "FAGL_SEGM", code, func() error {
		id, err := l.e.Centers.CreateSegment(ctx, ledger.NewSegment{Code: code, Name: t.name})
		if err != nil {
			return err
		}
		return l.addRef(ctx, "ledger.segment", id, "FAGL_SEGM", code)
	})
	if done {
		l.counts.Segments++
	}
	return err
}

// centerTextsOf are a center's texts as the platform holds them, from its
// text (pickText; the quality gate refused a center without a name) and
// its last slice's person responsible.
func centerTextsOf(t sapText, responsible string) ledger.Texts {
	out := ledger.Texts{Name: t.name, Responsible: responsible}
	if t.description != "" {
		out.Description = &t.description
	}
	return out
}

// scopeEntities are the legal entities of the run's scope among the
// company codes, in their order.
func (l *loader) scopeEntities(codes []string) []uuid.UUID {
	var out []uuid.UUID
	for _, c := range codes {
		if le, ok := l.t.entities[c]; ok {
			out = append(out, le.ID)
		}
	}
	return out
}

func (l *loader) profitCenter(ctx context.Context, pc sapProfitCenter, texts map[string][]sapText) error {
	key, les := pc.key(), l.scopeEntities(pc.companies)
	reason := "Migration run " + l.run.ID.String() + ": SAP profit center " + key
	if id, ok := l.refs["CEPC/"+key]; ok {
		return l.assignProfitCenter(ctx, key, id, les, reason)
	}
	last := pc.slices[len(pc.slices)-1]
	in := ledger.MigratedProfitCenter{Code: pc.code, LegalEntityIDs: les, Reason: reason,
		StandardNodeID: l.standardNode(sap.SetClassProfitCenters, pc.area, pc.code)}
	if last.SEGMENT != "" {
		id, ok := l.refs["FAGL_SEGM/"+last.SEGMENT]
		if !ok {
			l.issue("CEPC", key, "segment "+last.SEGMENT+" was not loaded")
			return nil
		}
		in.SegmentID = &id
	}
	t, _, _ := pickText(texts["CEPC/"+key])
	in.Texts = centerTextsOf(t, last.VERAK)
	ps, _ := profitCenterPeriods(pc) // the quality gate refused slices that do not read
	for _, p := range ps {
		in.Periods = append(in.Periods, ledger.MigratedPeriod[ledger.ProfitCenterAttrs]{From: p.from, To: p.to, Attr: ledger.ProfitCenterAttrs{Locked: p.attr.locked}})
	}
	var id uuid.UUID
	done, err := l.step(ctx, "CEPC", key, func() error {
		var err error
		if id, err = l.e.Centers.CreateProfitCenter(ctx, in); err != nil {
			return err
		}
		return l.addRef(ctx, "ledger.profit_center", id, "CEPC", key)
	})
	if done {
		l.counts.ProfitCenters++
		l.createdCenter(sap.SetClassProfitCenters+"/"+key, id, in.StandardNodeID)
	}
	return err
}

// assignProfitCenter assigns a profit center an earlier run loaded to the
// run's companies it is not yet assigned to.
func (l *loader) assignProfitCenter(ctx context.Context, key string, id uuid.UUID, les []uuid.UUID, reason string) error {
	states, err := l.e.Centers.ProfitCenters(ctx, []uuid.UUID{id})
	if err != nil {
		return err
	}
	missing := slices.DeleteFunc(slices.Clone(les), func(le uuid.UUID) bool { return slices.Contains(states[id].LegalEntityIDs, le) })
	if len(missing) == 0 {
		return nil
	}
	done, err := l.step(ctx, "CEPC_BUKRS", key, func() error { return l.e.Centers.AssignProfitCenter(ctx, id, missing, reason) })
	if done {
		l.counts.ProfitCentersExtended++
	}
	return err
}

func (l *loader) costCenter(ctx context.Context, cc sapCostCenter, texts map[string][]sapText) error {
	key := cc.key()
	if _, ok := l.refs["CSKS/"+key]; ok {
		return nil
	}
	last := cc.slices[len(cc.slices)-1]
	t, _, _ := pickText(texts["CSKS/"+key])
	in := ledger.MigratedCostCenter{LegalEntityID: l.t.entities[cc.company()].ID, Code: cc.code, Texts: centerTextsOf(t, last.VERAK),
		StandardNodeID: l.standardNode(sap.SetClassCostCenters, cc.area, cc.code), Reason: "Migration run " + l.run.ID.String() + ": SAP cost center " + key}
	ps, _ := costCenterPeriods(cc) // the quality gate refused slices that do not read
	for _, p := range ps {
		a := ledger.CostCenterAttrs{Category: p.attr.category, LockCost: p.attr.lockCost, LockRevenue: p.attr.lockRevenue, LockPlan: p.attr.lockPlan}
		if p.attr.profitCenter != "" {
			id, ok := l.refs["CEPC/"+cc.area+"/"+p.attr.profitCenter]
			if !ok {
				l.issue("CSKS", key, "profit center "+p.attr.profitCenter+" was not loaded")
				return nil
			}
			a.ProfitCenterID = id
		}
		in.Periods = append(in.Periods, ledger.MigratedPeriod[ledger.CostCenterAttrs]{From: p.from, To: p.to, Attr: a})
	}
	var id uuid.UUID
	done, err := l.step(ctx, "CSKS", key, func() error {
		var err error
		if id, err = l.e.Centers.CreateCostCenter(ctx, in); err != nil {
			return err
		}
		return l.addRef(ctx, "ledger.cost_center", id, "CSKS", key)
	})
	if done {
		l.counts.CostCenters++
		l.createdCenter(sap.SetClassCostCenters+"/"+key, id, in.StandardNodeID)
	}
	return err
}

// createdCenter notes a center the run created, by set class/area/code,
// and counts its place at a standard leaf of SAP's groups.
func (l *loader) createdCenter(key string, id uuid.UUID, standardNode *uuid.UUID) {
	l.created[key] = id
	if standardNode != nil {
		l.counts.HierarchyPlaces++
	}
}
