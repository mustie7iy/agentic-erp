package ledger

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The in-use refusals of center changes (ADR-0050 §3, T56d2, T56d4). A
// posted line read its centers' attributes in force on its posting date,
// and its profit center's segment; a change may not contradict what posted
// lines read. It is planned at the request and again at the approval
// (planCostCenter, planProfitCenter), each time with the center held for
// update: a posting under way holds it for key share (assign), so the
// change waits for it and then sees its lines, and a posting after waits
// for the change and reads what it wrote.

// lineKinds are the posted lines that read a changed attribute (product
// owner 2026-10-25: a lock is compared with the lines of its kind only).
// The posting reads a cost center's actual cost lock on primary_cost lines
// and its revenue lock on revenue lines (validCostCenter). An account's
// cost_element is primary_cost only on expense accounts and revenue only on
// revenue accounts (gl_account_cost_element), a line on a none account
// takes no cost center, and the configuration may change cost_element
// later while no path changes account_type: so the lines on expense
// accounts are the cost lines, those on revenue accounts the revenue
// lines, as they were posted.
type lineKinds uint8

const (
	costLines    lineKinds = 1 << iota // lines on expense accounts
	revenueLines                       // lines on revenue accounts
	everyLine                          // every line carrying the center, whatever its account
)

// dateRange is a range of posting dates [From, To), To zero open-ended,
// and the lines on them that read what the change alters.
type dateRange struct {
	From, To time.Time
	Lines    lineKinds
}

// costCenterReaders are the lines that read, on a date, what differs
// between the cost center's attributes was and now: a cost line the actual
// cost lock, a revenue line the revenue lock, no actual posting the plan
// lock, and every line all the rest (its category and profit center, and
// any attribute added later, so a new one is checked until it is placed).
func costCenterReaders(was, now CostCenterAttrs) lineKinds {
	unlocked := func(a CostCenterAttrs) CostCenterAttrs {
		a.LockCost, a.LockRevenue, a.LockPlan = false, false, false
		return a
	}
	if unlocked(was) != unlocked(now) {
		return everyLine
	}
	var k lineKinds
	if was.LockCost != now.LockCost {
		k |= costLines
	}
	if was.LockRevenue != now.LockRevenue {
		k |= revenueLines
	}
	return k
}

// profitCenterReaders: a profit center has one lock, and every line
// carrying the profit center reads it (profitCenterState).
func profitCenterReaders(was, now ProfitCenterAttrs) lineKinds {
	if was != now {
		return everyLine
	}
	return 0
}

// changedRanges are the posting dates on which what a line read of the
// center differs between the periods old and after a change (readers), or
// on which the center is no longer valid (every line): dates within old's
// validity, and dates before it, on which a posting took the first
// period's attributes from a default (spanOn, lenient).
func changedRanges[A comparable](old, after []span[A], readers func(was, now A) lineKinds) []dateRange {
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
		if !posted {
			continue
		}
		lines := everyLine
		if now, still := spanOn(after, from, lenient); still {
			lines = readers(was, now)
		}
		if lines == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].To.Equal(from) && out[n-1].Lines == lines {
			out[n-1].To = to
			continue
		}
		out = append(out, dateRange{From: from, To: to, Lines: lines})
	}
	return out
}

// shortenedEnd is the end of validity the periods after a change give the
// center when it is earlier than its end before; zero when it is not.
func shortenedEnd[A comparable](old, after []span[A]) time.Time {
	_, oldEnd, _ := validity(old)
	if _, end, _ := validity(after); !end.IsZero() && end.Before(endOf(oldEnd)) {
		return end
	}
	return time.Time{}
}

// centerLines are the journal lines carrying a center: in its column of
// journal_line, in the companies it may be posted in.
type centerLines struct {
	column, what string // cost_center_id, "cost center"
	id           uuid.UUID
	companies    []uuid.UUID
}

// costCenterInUse refuses a change of the cost center's periods that its
// posted lines contradict (linesInUse).
func costCenterInUse(ctx context.Context, tx pgx.Tx, c CostCenter, after []span[CostCenterAttrs]) error {
	on := centerLines{column: "cost_center_id", what: "cost center", id: c.ID, companies: []uuid.UUID{c.LegalEntityID}}
	return linesInUse(ctx, tx, on, shortenedEnd(c.Periods, after), changedRanges(c.Periods, after, costCenterReaders),
		"ledger.cost_center_posted_after", "ledger.cost_center_period_in_use")
}

// profitCenterInUse refuses a change of the profit center's periods that
// the lines carrying it contradict (linesInUse), as a cost center's (T56d4,
// product owner 2026-10-25). Lines carry a profit center only in companies
// it is assigned to (journal_line_profit_center_fkey).
func profitCenterInUse(ctx context.Context, tx pgx.Tx, c ProfitCenter, after []span[ProfitCenterAttrs]) error {
	on := centerLines{column: "profit_center_id", what: "profit center", id: c.ID, companies: c.LegalEntityIDs}
	return linesInUse(ctx, tx, on, shortenedEnd(c.Periods, after), changedRanges(c.Periods, after, profitCenterReaders),
		"ledger.profit_center_posted_after", "ledger.profit_center_period_in_use")
}

// linesInUse refuses a change that the center's posted lines contradict: a
// validity ending (end, zero when not shortened) on or before the posting
// date of a line, a reversal included (409 postedAfter), or what a line
// read on its posting date differing after it (ranges, 409 periodInUse);
// the change is made from a later date instead.
func linesInUse(ctx context.Context, tx pgx.Tx, on centerLines, end time.Time, ranges []dateRange, postedAfter, periodInUse string) error {
	if !end.IsZero() {
		last, err := postedOn(ctx, tx, on, []dateRange{{From: end, Lines: everyLine}})
		if err != nil {
			return err
		}
		if last != nil {
			return problem.Conflict(postedAfter, fmt.Sprintf("Lines are posted on the %s on %s, on or after the end of validity the change "+
				"gives it (%s); its validity may end only after its last posting.", on.what, dateText(*last), dateText(end)))
		}
	}
	if len(ranges) == 0 {
		return nil
	}
	first, err := postedOn(ctx, tx, on, ranges)
	if err != nil {
		return err
	}
	if first != nil {
		return problem.Conflict(periodInUse, fmt.Sprintf("Lines posted on the %s on %s read what the change would alter; change it from "+
			"a date after the last posting instead.", on.what, dateText(*first)))
	}
	return nil
}

// postedOn is the earliest posting date of a line carrying the center
// within the ranges, among the lines each range names (lineKinds), nil
// when there is none.
func postedOn(ctx context.Context, tx pgx.Tx, on centerLines, ranges []dateRange) (*time.Time, error) {
	n := len(ranges)
	froms, tos := make([]time.Time, n), make([]*time.Time, n)
	every, costs, revenues := make([]bool, n), make([]bool, n), make([]bool, n)
	for i, r := range ranges {
		froms[i], tos[i] = r.From, nullDate(r.To)
		every[i], costs[i], revenues[i] = r.Lines&everyLine != 0, r.Lines&costLines != 0, r.Lines&revenueLines != 0
	}
	var first *time.Time
	if err := tx.QueryRow(ctx, `SELECT min(l.posting_date) FROM ledger.journal_line l
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		JOIN unnest($3::date[], $4::date[], $5::bool[], $6::bool[], $7::bool[]) AS r(f, t, every_line, costs, revenues)
			ON l.posting_date >= r.f AND (r.t IS NULL OR l.posting_date < r.t)
			AND (r.every_line OR (r.costs AND a.account_type = 'expense') OR (r.revenues AND a.account_type = 'revenue'))
		WHERE l.legal_entity_id = ANY($1) AND l.`+pgx.Identifier{on.column}.Sanitize()+` = $2`,
		on.companies, on.id, froms, tos, every, costs, revenues).Scan(&first); err != nil {
		return nil, fmt.Errorf("ledger: lines posted on %s %s: %w", on.what, on.id, err)
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
