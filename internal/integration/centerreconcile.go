package integration

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// centerStates are the centers and segments runs loaded, read back once
// per reconciliation: by SAP key (KOKRS/KOSTL, KOKRS/PRCTR), and the codes
// of profit centers and segments by id, which periods and profit centers
// name.
type centerStates struct {
	cost        map[string]ledger.MigratedCostCenterState
	profit      map[string]ledger.MigratedProfitCenterState
	profitCodes map[uuid.UUID]string
	segments    map[uuid.UUID]string
}

// readCenters reads every center and segment a run of the system loaded.
func (rc *reconciler) readCenters(ctx context.Context) (*centerStates, error) {
	if rc.states != nil {
		return rc.states, nil
	}
	ids := map[string][]uuid.UUID{}
	keyOf := map[uuid.UUID]string{}
	for ref, id := range rc.r {
		object, key, _ := strings.Cut(ref, "/")
		ids[object], keyOf[id] = append(ids[object], id), key
	}
	cost, err := rc.e.Centers.CostCenters(ctx, ids["CSKS"])
	if err != nil {
		return nil, err
	}
	profit, err := rc.e.Centers.ProfitCenters(ctx, ids["CEPC"])
	if err != nil {
		return nil, err
	}
	st := &centerStates{cost: map[string]ledger.MigratedCostCenterState{}, profit: map[string]ledger.MigratedProfitCenterState{},
		profitCodes: map[uuid.UUID]string{}}
	if st.segments, err = rc.e.Centers.Segments(ctx, ids["FAGL_SEGM"]); err != nil {
		return nil, err
	}
	for id, c := range cost {
		st.cost[keyOf[id]] = c
	}
	for id, c := range profit {
		st.profit[keyOf[id]], st.profitCodes[id] = c, c.Code
	}
	rc.states = st
	return st, nil
}

// centers compares the company's cost and profit centers (ADR-0050 §10)
// where either side has any: how many there are, SAP's of the company
// against the platform's active ones that runs loaded in it, and each
// one's periods as SAP's slices give them against the platform's. It
// returns the centers runs loaded, which the center totals read too.
func (rc *reconciler) centers(ctx context.Context, s companyScope) (*centerStates, error) {
	st, err := rc.readCenters(ctx)
	if err != nil {
		return nil, err
	}
	sapCost, sapProfit := map[string]string{}, map[string]string{}
	for _, c := range costCenters(rc.ext) {
		if c.company() == s.cc {
			sapCost[c.key()] = rc.sapCostCenter(c)
		}
	}
	for _, c := range profitCenters(rc.ext) {
		if slices.Contains(c.companies, s.cc) {
			sapProfit[c.key()] = rc.sapProfitCenter(c)
		}
	}
	platformCost, platformProfit := map[string]string{}, map[string]string{}
	for key, c := range st.cost {
		if c.LegalEntityID == s.le && c.Status == ledger.CenterActive {
			platformCost[key] = costCenterText(fromLedger(c.Periods, func(a ledger.CostCenterAttrs) ccAttr {
				return ccAttr{category: a.Category, profitCenter: st.code(a.ProfitCenterID), lockCost: a.LockCost, lockRevenue: a.LockRevenue, lockPlan: a.LockPlan}
			}))
		}
	}
	for key, c := range st.profit {
		if slices.Contains(c.LegalEntityIDs, s.le) && c.Status == ledger.CenterActive {
			segment := ""
			if c.SegmentID != nil {
				segment = st.segments[*c.SegmentID]
			}
			platformProfit[key] = profitCenterText(fromLedger(c.Periods, func(a ledger.ProfitCenterAttrs) pcAttr { return pcAttr{locked: a.Locked} }), segment)
		}
	}
	rc.compareCenters(s.cc, "Cost centers", "cost center ", sapCost, platformCost)
	rc.compareCenters(s.cc, "Profit centers", "profit center ", sapProfit, platformProfit)
	return st, nil
}

// compareCenters adds the count and the differences of one kind of center,
// where either side has any.
func (rc *reconciler) compareCenters(cc, name, prefix string, sapSide, platform map[string]string) {
	if len(sapSide) == 0 && len(platform) == 0 {
		return
	}
	rc.add(cc, name, strconv.Itoa(len(sapSide)), strconv.Itoa(len(platform)), len(sapSide) == len(platform))
	n := 0
	for _, key := range sortedKeys(unionKeys(sapSide, platform)) {
		if sapSide[key] != platform[key] {
			rc.rec.Diffs = append(rc.rec.Diffs, Diff{CompanyCode: cc, Key: prefix + key, SAP: orNone(sapSide[key]), Platform: orNone(platform[key])})
			n++
		}
	}
	rc.add(cc, name+": centers with differences", "0", strconv.Itoa(n), n == 0)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func (st *centerStates) code(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	if c, ok := st.profitCodes[id]; ok {
		return c
	}
	return id.String()
}

// sapCostCenter is a cost center as SAP's slices give it; one whose
// slices do not read is refused, as the quality gate refuses it, and
// compares as having none.
func (rc *reconciler) sapCostCenter(c sapCostCenter) string {
	ps, err := costCenterPeriods(c)
	if err != nil {
		rc.refuse("CSKS", c.key(), "%v", err)
		return "unreadable"
	}
	return costCenterText(ps)
}

func (rc *reconciler) sapProfitCenter(c sapProfitCenter) string {
	ps, err := profitCenterPeriods(c)
	if err != nil {
		rc.refuse("CEPC", c.key(), "%v", err)
		return "unreadable"
	}
	return profitCenterText(ps, c.slices[len(c.slices)-1].SEGMENT)
}

func fromLedger[L, A comparable](ps []ledger.MigratedPeriod[L], attr func(L) A) []period[A] {
	out := make([]period[A], len(ps))
	for i, p := range ps {
		out[i] = period[A]{from: p.From, to: p.To, attr: attr(p.Attr)}
	}
	return out
}

// validityText is a period's validity, its last day included, as a
// difference shows it.
func validityText(from, to time.Time) string {
	if to.IsZero() {
		return "from " + from.Format(time.DateOnly)
	}
	return from.Format(time.DateOnly) + " to " + lastDay(to)
}

func costCenterText(ps []period[ccAttr]) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		t := validityText(p.from, p.to) + ": " + p.attr.category
		if p.attr.profitCenter != "" {
			t += ", profit center " + p.attr.profitCenter
		}
		var locks []string
		for _, l := range []struct {
			on   bool
			name string
		}{{p.attr.lockCost, "actual costs"}, {p.attr.lockRevenue, "actual revenues"}, {p.attr.lockPlan, "plan"}} {
			if l.on {
				locks = append(locks, l.name)
			}
		}
		if len(locks) > 0 {
			t += ", locked for " + strings.Join(locks, ", ")
		}
		parts[i] = t
	}
	return strings.Join(parts, "; ")
}

func profitCenterText(ps []period[pcAttr], segment string) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = validityText(p.from, p.to)
		if p.attr.locked {
			parts[i] += ", locked"
		}
	}
	t := strings.Join(parts, "; ")
	if segment != "" {
		t += "; segment " + segment
	}
	return t
}
