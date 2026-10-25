package ledger

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The in-use refusals of center changes (ADR-0050 §3, T56d2). A posted
// line read its cost center's attributes in force on its posting date, and
// its profit center's segment; a change may not contradict what posted
// lines read. It is planned at the request and again at the approval
// (planCostCenter, planProfitCenter), each time with the center held for
// update: a posting under way holds it for key share (assign), so the
// change waits for it and then sees its lines, and a posting after waits
// for the change and reads what it wrote.

// dateRange is a range of posting dates [From, To), To zero open-ended.
type dateRange struct{ From, To time.Time }

// readByPostings is what a posted line read of its cost center's
// attributes: the plan lock is read by plans, never by an actual posting.
func readByPostings(a CostCenterAttrs) CostCenterAttrs {
	a.LockPlan = false
	return a
}

// changedRanges are the posting dates on which what a line read of the
// center (read) differs between the periods old and after a change, or on
// which the center is no longer valid: dates within old's validity, and
// dates before it, on which a posting took the first period's attributes
// from a default (spanOn, lenient).
func changedRanges[A comparable](old, after []span[A], read func(A) A) []dateRange {
	first, _, _ := validity(old) // none: nothing was posted on old
	cuts := []time.Time{{}}      // the earliest date
	for _, s := range slices.Concat(old, after) {
		cuts = append(cuts, s.From)
		if !s.To.IsZero() {
			cuts = append(cuts, s.To)
		}
	}
	slices.SortFunc(cuts, time.Time.Compare)
	cuts = slices.CompactFunc(cuts, time.Time.Equal)
	var out []dateRange
	for i, from := range cuts {
		var to time.Time
		if i+1 < len(cuts) {
			to = cuts[i+1]
		}
		lenient := from.Before(first)
		was, posted := spanOn(old, from, lenient)
		now, still := spanOn(after, from, lenient)
		if !posted || (still && read(was) == read(now)) {
			continue
		}
		if n := len(out); n > 0 && out[n-1].To.Equal(from) {
			out[n-1].To = to
			continue
		}
		out = append(out, dateRange{From: from, To: to})
	}
	return out
}

// costCenterInUse refuses a change of the cost center's periods that its
// posted lines contradict: a validity ending on or before a posting (409
// ledger.cost_center_posted_after), or attributes in force on a posted
// date that differ after the change (409 ledger.cost_center_period_in_use);
// the change adds a period from a later date instead.
func costCenterInUse(ctx context.Context, tx pgx.Tx, c CostCenter, after []span[CostCenterAttrs]) error {
	_, oldEnd, _ := validity(c.Periods)
	if _, end, _ := validity(after); !end.IsZero() && end.Before(endOf(oldEnd)) {
		last, err := postedOnCostCenter(ctx, tx, c, []dateRange{{From: end}})
		if err != nil {
			return err
		}
		if last != nil {
			return problem.Conflict("ledger.cost_center_posted_after", fmt.Sprintf("Lines are posted on the cost center on %s, on or after the end "+
				"of validity the change gives it (%s); its validity may end only after its last posting.", dateText(*last), dateText(end)))
		}
	}
	ranges := changedRanges(c.Periods, after, readByPostings)
	if len(ranges) == 0 {
		return nil
	}
	first, err := postedOnCostCenter(ctx, tx, c, ranges)
	if err != nil {
		return err
	}
	if first != nil {
		return problem.Conflict("ledger.cost_center_period_in_use", fmt.Sprintf("Lines posted on the cost center on %s read the attributes the "+
			"change would alter; change them from a date after the last posting instead.", dateText(*first)))
	}
	return nil
}

// postedOnCostCenter is the earliest posting date within the ranges of a
// line carrying the cost center, nil when there is none.
func postedOnCostCenter(ctx context.Context, tx pgx.Tx, c CostCenter, ranges []dateRange) (*time.Time, error) {
	froms, tos := make([]time.Time, len(ranges)), make([]*time.Time, len(ranges))
	for i, r := range ranges {
		froms[i], tos[i] = r.From, nullDate(r.To)
	}
	var first *time.Time
	if err := tx.QueryRow(ctx, `SELECT min(l.posting_date) FROM ledger.journal_line l
		JOIN unnest($3::date[], $4::date[]) AS r(f, t) ON l.posting_date >= r.f AND (r.t IS NULL OR l.posting_date < r.t)
		WHERE l.legal_entity_id = $1 AND l.cost_center_id = $2`, c.LegalEntityID, c.ID, froms, tos).Scan(&first); err != nil {
		return nil, fmt.Errorf("ledger: lines posted on cost center %s: %w", c.ID, err)
	}
	return first, nil
}

// segmentInUse refuses another segment for a profit center lines are
// posted on (409 ledger.segment_in_use): each carries the segment it had,
// so a segment move is a reorganisation with reposting, not an attribute
// change (SAP). Lines carry a profit center only in companies it is
// assigned to (journal_line_profit_center_fkey).
func segmentInUse(ctx context.Context, tx pgx.Tx, c ProfitCenter) error {
	var posted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.journal_line WHERE legal_entity_id = ANY($1) AND profit_center_id = $2)`,
		c.LegalEntityIDs, c.ID).Scan(&posted); err != nil {
		return fmt.Errorf("ledger: lines posted on profit center %s: %w", c.ID, err)
	}
	if posted {
		return problem.Conflict("ledger.segment_in_use", "Lines are posted on the profit center with its segment; a segment move is a "+
			"reorganisation with reposting, not a change of the profit center.")
	}
	return nil
}
