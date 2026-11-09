package integration

import (
	"bytes"
	"context"
	"maps"
	"math"
	"slices"
	"strconv"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// CenterActuals reads the center reports' actuals, for the reconciliation
// of migrated center totals (ledger.AssignmentBalances, read as GET
// /v1/cost-center-balances and /v1/profit-center-balances read them).
type CenterActuals interface {
	CostCenterActuals(ctx context.Context, q ledger.ActualsQuery) ([]ledger.AssignmentActual, error)
	ProfitCenterActuals(ctx context.Context, q ledger.ActualsQuery) ([]ledger.AssignmentActual, error)
}

// actualsPage is how many rows of actuals the reconciliation reads at a
// time.
var actualsPage = 1000

// centerTotalChecks name the controls of the center totals by kind: its
// differences are keyed "<kind> total <key>".
var centerTotalChecks = [3]struct{ kind, name string }{
	{"cost center", "Cost center totals: centers with differences"},
	{"profit center", "Profit center totals: centers with differences"},
	{"segment", "Segment totals: segments with differences"},
}

// centerTotals compares, in documents mode, the company's migrated totals
// per cost center, profit center and segment (ADR-0050 §10, T56h4), where
// either side has any: SAP's, its lines on profit and loss accounts by
// RCNTR, PRCTR and SEGMENT in company code currency, against the center
// reports' actuals in local currency over the fiscal year of the centers
// runs loaded, the leading ledger's, a segment's the sum of its profit
// centers' (the profit center report's segment rows). The reports hold
// profit and loss accounts only, and a balance sheet line takes no cost
// center (§5). A balances-mode run has no control: its opening entry
// carries the profit and loss balances without centers, so the totals
// cannot agree; only its own entries (an open down payment's, an open
// foreign item's) carry SAP's centers.
func (rc *reconciler) centerTotals(ctx context.Context, s companyScope, st *centerStates) error {
	if rc.t.balances {
		return nil
	}
	cost, profit, segmentOf := map[uuid.UUID]string{}, map[uuid.UUID]string{}, map[string]string{}
	for key, c := range st.cost {
		if c.LegalEntityID == s.le {
			cost[rc.r["CSKS/"+key]] = key
		}
	}
	for key, c := range st.profit {
		if slices.Contains(c.LegalEntityIDs, s.le) {
			profit[rc.r["CEPC/"+key]] = key
			if c.SegmentID != nil {
				segmentOf[key] = st.segments[*c.SegmentID]
			}
		}
	}
	platformCost, err := readActuals(ctx, rc.e.Actuals.CostCenterActuals, s.le, int16(rc.t.year), cost)
	if err != nil {
		return err
	}
	platformProfit, err := readActuals(ctx, rc.e.Actuals.ProfitCenterActuals, s.le, int16(rc.t.year), profit)
	if err != nil {
		return err
	}
	platformSegment := map[string][]money.Minor{}
	for key, terms := range platformProfit {
		if seg := segmentOf[key]; seg != "" {
			platformSegment[seg] = append(platformSegment[seg], terms...)
		}
	}
	sapSide := rc.sapCenterTotals(s)
	for i, platform := range []map[string][]money.Minor{platformCost, platformProfit, platformSegment} {
		c := centerTotalChecks[i]
		rc.compareTotals(s, c.name, c.kind+" total ", sapSide[i], rc.totals("platform", s.cc, platform, func(k string) string { return "the platform's actuals of " + c.kind + " " + k }))
	}
	return nil
}

// compareTotals adds a control of center totals and its differences,
// where either side has any.
func (rc *reconciler) compareTotals(s companyScope, name, prefix string, sapSide, platform map[string]money.Minor) {
	if len(sapSide) == 0 && len(platform) == 0 {
		return
	}
	d := diffs(s.cc, sapSide, platform, s.format)
	for i := range d {
		d[i].Key = prefix + d[i].Key
	}
	rc.rec.Diffs = append(rc.rec.Diffs, d...)
	rc.add(s.cc, name, "0", strconv.Itoa(len(d)), len(d) == 0)
}

// sapCenterTotals are SAP's totals of the company's lines on profit and
// loss accounts per cost center and profit center (by KOKRS/code) and per
// segment. A line naming a center the run does not migrate is refused, as
// the quality gate refuses it, and counts for none.
func (rc *reconciler) sapCenterTotals(s companyScope) [3]map[string]money.Minor {
	x := lineCentersOf(rc.ext)
	terms := [3]map[string][]money.Minor{{}, {}, {}}
	for _, l := range rc.ext.ACDOCA {
		if l.RBUKRS != s.cc {
			continue
		}
		cost, profit, why := x.centers(l)
		for _, w := range why {
			rc.refuse("ACDOCA", documentKey(l)+"/"+l.DOCLN, "%s", w)
		}
		amt, err := rc.t.amount(l.HSL, s.cur)
		if err != nil || !rc.pnl[s.cc+"/"+l.RACCT] {
			continue // a line whose amount does not parse is refused by journal
		}
		for i, key := range []string{cost, profit, l.SEGMENT} {
			if key != "" {
				terms[i][key] = append(terms[i][key], amt)
			}
		}
	}
	var out [3]map[string]money.Minor
	for i, c := range centerTotalChecks {
		out[i] = rc.totals("ACDOCA", s.cc, terms[i], func(k string) string { return "the lines of " + c.kind + " " + k })
	}
	return out
}

// readActuals reads the actuals of the centers, by id with their SAP key,
// over every period of the fiscal year, page by page, and returns each
// key's amounts (one per account).
func readActuals(ctx context.Context, read func(context.Context, ledger.ActualsQuery) ([]ledger.AssignmentActual, error), le uuid.UUID, year int16,
	keys map[uuid.UUID]string) (map[string][]money.Minor, error) {
	out := map[string][]money.Minor{}
	if len(keys) == 0 {
		return out, nil
	}
	ids := slices.SortedFunc(maps.Keys(keys), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	q := ledger.ActualsQuery{LegalEntityID: le, FiscalYear: year, PeriodFrom: 0, PeriodTo: math.MaxInt16, Centers: ids, Limit: actualsPage}
	for {
		page, err := read(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, a := range page {
			out[keys[a.CenterID]] = append(out[keys[a.CenterID]], a.Amount)
		}
		if len(page) < q.Limit {
			return out, nil
		}
		q.After = &page[len(page)-1].CenterAccount
	}
}
