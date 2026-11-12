package ledger

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Account assignment on the journal line (ADR-0050 §5, T56d). Every
// posting path ends in postAll, which derives each line's cost center,
// profit center and segment once for the posting group, so every ledger's
// copy carries the same (T52), and validates them before a line is
// written; parking checks an entry the same way without locking. A
// reversal carries its original's assignment and is neither derived nor
// validated again. The database holds the centers' companies by foreign
// key (000155-000156); its commit-time check of §5's table comes with
// T56d3, in the release after (06 §6.4).
//
// Derivation, first hit wins: the cost center as the line gives it, else,
// on a primary cost account with cost center accounting on in the posting's
// fiscal year, the company's default for the account (SAP OKB9); the
// profit center as given, else the cost center's in its period on the
// posting date (a different one given is refused), else, for a P&L line
// with profit center accounting on, the company's default; the segment is
// always the profit center's.
//
// An internal order is the second real cost object (ADR-0064 §2, T60b): a
// primary cost line names a cost center or an order, never both. The order
// master is the controlling context's, so the ledger asks it through
// CostObjects (assignOrders) before the derivation; the line then takes
// its profit center and segment from the order's responsible cost center
// in its period on the posting date, and keeps no cost center of its own.

// CostObjects is what the ledger asks the controlling context about the
// internal orders its lines name (ADR-0064 §2), as it asks the partner
// context through Partners: the order master is controlling's, and the
// journal line names an order without a foreign key.
type CostObjects interface {
	// Orders reads the orders with the ids, keyed by id; an id that is no
	// order has no entry. hold keeps them until the transaction ends, so
	// that a status change waits for a posting under way and a posting
	// after it sees it.
	Orders(ctx context.Context, ids []uuid.UUID, hold bool) (map[uuid.UUID]CostOrder, error)
}

// CostOrder is an internal order as a posting reads it: its company, its
// code, whether it takes postings (released) and its responsible cost
// center.
type CostOrder struct {
	LegalEntityID uuid.UUID
	Code          string
	Postable      bool
	CostCenterID  uuid.UUID
}

func init() {
	// The database's part of the rule (000268): the application refuses
	// the line first (assignOrders).
	problem.RegisterConstraint("journal_line_one_cost_object", func() *problem.Problem {
		return problem.Unprocessable("ledger.internal_order_invalid", "A line names both a cost center and an internal order.",
			problem.FieldError{Field: "lines.internal_order_id", Code: reasonWithCostCenter, Message: reasonTexts[reasonWithCostCenter]})
	})
	problem.RegisterConstraint("journal_line_cost_center_fkey", func() *problem.Problem {
		return problem.Unprocessable("ledger.cost_center_invalid", "A line's cost center is not a cost center of the line's legal entity.",
			problem.FieldError{Field: "lines.cost_center_id", Code: reasonNotInCompany, Message: "a cost center of the legal entity"})
	})
	problem.RegisterConstraint("journal_line_profit_center_fkey", func() *problem.Problem {
		return problem.Unprocessable("ledger.profit_center_invalid", "A line's profit center is not assigned to the line's legal entity.",
			problem.FieldError{Field: "lines.profit_center_id", Code: reasonNotInCompany, Message: "a profit center assigned to the legal entity"})
	})
	problem.RegisterConstraint("journal_line_segment_fkey", segmentMismatch)
	// A parked line's centers are the tenant's (000159); parking checks the
	// rest, except on a date in no fiscal year (parkable).
	problem.RegisterConstraint("parked_entry_line_cost_center_fkey", func() *problem.Problem {
		return problem.Unprocessable("ledger.cost_center_invalid", "A line's cost center is not known.",
			problem.FieldError{Field: "lines.cost_center_id", Code: reasonUnknown, Message: "a cost center of the legal entity"})
	})
	problem.RegisterConstraint("parked_entry_line_profit_center_fkey", func() *problem.Problem {
		return problem.Unprocessable("ledger.profit_center_invalid", "A line's profit center is not known.",
			problem.FieldError{Field: "lines.profit_center_id", Code: reasonUnknown, Message: "a profit center assigned to the legal entity"})
	})
	problem.RegisterConstraint("parked_entry_line_segment_fkey", segmentMismatch)
}

func segmentMismatch() *problem.Problem {
	return problem.Unprocessable("ledger.segment_mismatch", "A line's segment is not its profit center's.",
		problem.FieldError{Field: "lines.segment_id", Code: "mismatch", Message: "the segment of the line's profit center, or none"})
}

// Why a center cannot be posted on a line (ADR-0050 §5), the field
// error's code of ledger.cost_center_invalid and ledger.profit_center_invalid,
// beside reasonUnknown.
const (
	reasonNotInCompany      = "not_in_company"
	reasonNotActive         = "not_active"
	reasonNotValidOnDate    = "not_valid_on_date"
	reasonLockedForCosts    = "locked_for_costs"
	reasonLockedForRevenue  = "locked_for_revenue"
	reasonRevenueNotAllowed = "revenue_not_allowed"
	reasonNotAllowed        = "not_allowed" // a cost center on an account whose cost element is none
	reasonLocked            = "locked"      // a profit center's lock
	// Of ledger.internal_order_invalid (ADR-0064 §2), beside reasonUnknown
	// and reasonNotInCompany.
	reasonNotReleased      = "not_released"
	reasonWithCostCenter   = "with_cost_center"
	reasonNotACostAccount  = "not_a_cost_account"
	reasonCenterNotOnDate  = "cost_center_not_valid_on_date"
	internalOrderInvalid   = "ledger.internal_order_invalid"
	internalOrderFieldName = "internal_order_id"
)

var reasonTexts = map[string]string{
	reasonUnknown:           "is not known",
	reasonNotInCompany:      "is not one of the legal entity's",
	reasonNotActive:         "is not active",
	reasonNotValidOnDate:    "is not valid on the posting date",
	reasonLockedForCosts:    "is locked for actual costs on the posting date",
	reasonLockedForRevenue:  "is locked for revenue on the posting date",
	reasonRevenueNotAllowed: "is of a category that takes no revenue",
	reasonNotAllowed:        "cannot be posted on an account that takes no cost object (cost element none)",
	reasonLocked:            "is locked on the posting date",
	reasonNotReleased:       "is not released for posting",
	reasonWithCostCenter:    "cannot be named beside a cost center: a line carries one real cost object",
	reasonNotACostAccount:   "can be posted only on a primary cost account",
	reasonCenterNotOnDate:   "has a responsible cost center that is not valid on the posting date, so the line's profit center cannot be derived",
}

// assignRules are a company's controlling as a posting in one fiscal year
// reads it (T56c's rules): whether cost and profit center accounting apply
// to the year, the default profit center, and per account of the lines its
// cost element, whether it is a balance sheet account and its active
// default cost center. accounts is nil when nothing is to be assigned.
type assignRules struct {
	costOn, profitOn bool
	defaultPC        *uuid.UUID
	accounts         map[uuid.UUID]controlledAccount
	// ownCostCenter: a primary cost line takes no default; an order item
	// names its own (CheckAssignments).
	ownCostCenter bool
}

type controlledAccount struct {
	element      config.CostElement
	balanceSheet bool
	defaultCC    *uuid.UUID
}

// lineCenters is what derivation found for one line: whether its cost and
// profit centers are a default's (lenient, spanOn).
type lineCenters struct{ ccDefault, pcDefault bool }

// assign derives and validates the assignment of the lines of a posting in
// legal entity le, dated date in fiscal year year, setting each line's
// centers. lock holds the centers and the periods read until the
// transaction ends: a center's key for share, so a change (which takes the
// center for update) waits for the posting and the posting for a change
// under way, and its periods for share (ADR-0050 §5); the periods are
// read after the centers are locked, so a posting that waited reads the
// periods the change wrote. Parking passes false.
func assign(ctx context.Context, tx pgx.Tx, le uuid.UUID, year int16, date time.Time, lines []Line, lock bool) error {
	r, err := readAssignRules(ctx, tx, le, year, lines)
	if err != nil || r.accounts == nil {
		return err
	}
	return r.apply(ctx, tx, le, date, lines, lock)
}

// apply derives and validates the lines' assignment under the rules, as
// assign says.
func (r assignRules) apply(ctx context.Context, tx pgx.Tx, le uuid.UUID, date time.Time, lines []Line, lock bool) error {
	found := make([]lineCenters, len(lines))
	if err := r.deriveCostCenters(lines, found); err != nil {
		return err
	}
	if err := checkLineCostCenters(ctx, tx, le, date, lines, r, found, lock); err != nil {
		return err
	}
	r.deriveProfitCenters(lines, found)
	return checkLineProfitCenters(ctx, tx, le, date, lines, found, lock)
}

// readAssignRules reads the company's controlling for the year and, unless
// it applies none and no line gives a center, the lines' accounts. The
// switches need no lock of their own: turning one on locks the company's
// period statuses for update (applyControlling), which the posting holds
// for share (postingPeriod).
func readAssignRules(ctx context.Context, tx pgx.Tx, le uuid.UUID, year int16, lines []Line) (assignRules, error) {
	var r assignRules
	err := tx.QueryRow(ctx, `SELECT coalesce(cost_center_accounting_from <= $2, false), coalesce(profit_center_accounting_from <= $2, false),
			default_profit_center_id FROM ledger.company_controlling WHERE legal_entity_id = $1`, le, year).Scan(&r.costOn, &r.profitOn, &r.defaultPC)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("ledger: controlling of %s in %d: %w", le, year, err)
	}
	if !r.costOn && !r.profitOn && !slices.ContainsFunc(lines, entered) {
		return r, nil
	}
	accounts := make([]uuid.UUID, len(lines))
	for i, l := range lines {
		accounts[i] = l.AccountID
	}
	rows, _ := tx.Query(ctx, `SELECT a.id, a.cost_element, a.is_balance_sheet, (SELECT d.cost_center_id FROM ledger.default_assignment d
			WHERE d.tenant_id = a.tenant_id AND d.legal_entity_id = $2 AND d.account_id = a.id AND d.is_active)
		FROM ledger.gl_account a WHERE a.id = ANY($1)`, accounts, le)
	r.accounts = map[uuid.UUID]controlledAccount{}
	var id uuid.UUID
	var a controlledAccount
	_, err = pgx.ForEachRow(rows, []any{&id, &a.element, &a.balanceSheet, &a.defaultCC}, func() error { r.accounts[id] = a; return nil })
	return r, errs.Wrapf(err, "ledger: controlled accounts of %s", le)
}

// entered reports whether the line gives a center or an order of its own.
func entered(l Line) bool {
	return l.CostCenterID != nil || l.ProfitCenterID != nil || l.SegmentID != nil || l.InternalOrderID != nil
}

// assignOrders checks the internal orders the lines name with the
// controlling context (ADR-0064 §2) and notes each one's responsible cost
// center on its line for the derivation: an order must be known, of the
// posting's company and released, and its line names no cost center (422
// ledger.internal_order_invalid, naming the line and why). The orders are
// held until the transaction ends. A posting whose lines name none asks
// nothing.
func (e *Endpoints) assignOrders(ctx context.Context, le uuid.UUID, lines []Line) error {
	ids := centerIDs(lines, func(l Line) *uuid.UUID { return l.InternalOrderID })
	if len(ids) == 0 {
		return nil
	}
	orders, err := e.costOrders(ctx, ids, true)
	if err != nil {
		return err
	}
	for i := range lines {
		l := &lines[i]
		if l.InternalOrderID == nil {
			continue
		}
		o, known := orders[*l.InternalOrderID]
		if reason := orderState(known, o.LegalEntityID == le, o.Postable, l.CostCenterID != nil); reason != "" {
			return orderRefused(i, reason)
		}
		l.orderCenter = &o.CostCenterID
	}
	return nil
}

// costOrders asks the controlling context for the orders. A posting path
// wired without the port (another context's, whose lines name no order)
// is an error rather than a line posted unchecked.
func (e *Endpoints) costOrders(ctx context.Context, ids []uuid.UUID, hold bool) (map[uuid.UUID]CostOrder, error) {
	if e.CostObjects == nil {
		return nil, errors.New("ledger: a line names an internal order where the cost objects port is not wired")
	}
	orders, err := e.CostObjects.Orders(ctx, ids, hold)
	return orders, errs.Wrapf(err, "ledger: internal orders of the lines")
}

// orderState is why an order cannot be posted on a line: unknown, not the
// company's, not released, or named beside a cost center; "" when it can.
func orderState(known, inCompany, postable, withCostCenter bool) string {
	switch {
	case !known:
		return reasonUnknown
	case !inCompany:
		return reasonNotInCompany
	case !postable:
		return reasonNotReleased
	case withCostCenter:
		return reasonWithCostCenter
	}
	return ""
}

// orderRefused is the refusal of line i's internal order for the reason.
func orderRefused(i int, reason string) error {
	return centerRefused(internalOrderInvalid, i, internalOrderFieldName, "The internal order", reason)
}

// nameLines names what the entries' lines refer to by id alone, as it is
// now: their trading partners (nameTradingPartners) and their internal
// orders, by code (ADR-0064 §2). The controlling context is asked only
// when a line names an order.
func (e *Endpoints) nameLines(ctx context.Context, reps ...*gen.JournalEntry) error {
	if err := e.nameTradingPartners(ctx, entryTradingPartners(reps...)); err != nil {
		return err
	}
	var ids []uuid.UUID
	for _, rep := range reps {
		for _, l := range rep.Lines {
			if id, ok := l.InternalOrderID.Get(); ok && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	orders, err := e.costOrders(ctx, ids, false)
	if err != nil {
		return err
	}
	for _, rep := range reps {
		for i := range rep.Lines {
			if o, ok := orders[rep.Lines[i].InternalOrderID.Value]; ok {
				rep.Lines[i].InternalOrderCode = gen.NewOptString(o.Code)
			}
		}
	}
	return nil
}

// deriveCostCenters refuses a cost center on an account that takes none
// and an internal order on an account that is not a primary cost account,
// and gives a primary cost line without either the account's default
// cost center, while cost center accounting applies; without a default
// the line is refused.
func (r assignRules) deriveCostCenters(lines []Line, found []lineCenters) error {
	for i := range lines {
		l, a := &lines[i], r.accounts[lines[i].AccountID]
		switch {
		case l.InternalOrderID != nil:
			// The order is the line's cost object: it takes no default
			// cost center, and an order collects costs only.
			if a.element != config.CostElementPrimaryCost {
				return orderRefused(i, reasonNotACostAccount)
			}
		case l.CostCenterID != nil && a.element == config.CostElementNone:
			return centerRefused("ledger.cost_center_invalid", i, "cost_center_id", "The cost center", reasonNotAllowed)
		case l.CostCenterID != nil || a.element != config.CostElementPrimaryCost || !r.costOn:
		case r.ownCostCenter || a.defaultCC == nil:
			why := "the account has no default cost center in the legal entity"
			if r.ownCostCenter {
				why = "while cost center accounting applies, a requisition or order item names its own"
			}
			return problem.Unprocessable("ledger.cost_center_required", fmt.Sprintf("Line %d posts to a primary cost account and needs a cost center; "+
				"%s.", i+1, why),
				problem.FieldError{Field: fmt.Sprintf("lines[%d].cost_center_id", i), Code: "required", Message: "a cost center of the legal entity"})
		default:
			l.CostCenterID, found[i].ccDefault = a.defaultCC, true
		}
	}
	return nil
}

// assignedCostCenter is a cost center as a posting reads it.
type assignedCostCenter struct {
	legalEntity uuid.UUID
	status      string
	spans       []span[CostCenterAttrs]
}

// checkLineCostCenters validates each line's cost center on the posting date
// (validCostCenter), or reads its internal order's responsible one
// (orderCostCenter), and gives a line without a profit center that cost
// center's; a different one given is refused (422
// ledger.profit_center_mismatch), as the cost center's profit center wins
// (SAP).
func checkLineCostCenters(ctx context.Context, tx pgx.Tx, le uuid.UUID, date time.Time, lines []Line, r assignRules, found []lineCenters, lock bool) error {
	centers, err := readCostCenters(ctx, tx, centerIDs(lines, derivingCenter), lock)
	if err != nil {
		return err
	}
	revenue, err := revenueAllowed(ctx, tx, lines, r, centers)
	if err != nil {
		return err
	}
	for i := range lines {
		l, f := &lines[i], &found[i]
		cc := derivingCenter(*l)
		if cc == nil {
			continue
		}
		attr, err := derivingAttrs(i, *l, centers[*cc], le, date, r.accounts[l.AccountID].element, f.ccDefault, revenue)
		if err != nil {
			return err
		}
		switch {
		case attr.ProfitCenterID == uuid.Nil:
		case l.ProfitCenterID == nil:
			pc := attr.ProfitCenterID
			l.ProfitCenterID, f.pcDefault = &pc, f.ccDefault
		case *l.ProfitCenterID != attr.ProfitCenterID:
			return problem.Unprocessable("ledger.profit_center_mismatch", fmt.Sprintf("Line %d: the profit center differs from its cost center's "+
				"on the posting date, which the line takes.", i+1),
				problem.FieldError{Field: fmt.Sprintf("lines[%d].profit_center_id", i), Code: "mismatch", Message: "the cost center's profit center, or none"})
		}
	}
	return nil
}

// derivingCenter is the cost center a line's profit center derives from:
// its own, or its internal order's responsible one (a line has one or the
// other, assignOrders).
func derivingCenter(l Line) *uuid.UUID { return cmp.Or(l.CostCenterID, l.orderCenter) }

// derivingAttrs are the attributes, on the posting date, of the cost
// center line i's profit center derives from: its internal order's
// responsible one (orderCostCenter), or its own, validated for the line
// (validCostCenter).
func derivingAttrs(i int, l Line, c *assignedCostCenter, le uuid.UUID, date time.Time, element config.CostElement, fromDefault bool,
	revenue map[string]bool) (CostCenterAttrs, error) {
	if l.orderCenter != nil {
		return orderCostCenter(i, c, date)
	}
	return validCostCenter(i, c, le, date, element, fromDefault, revenue)
}

// orderCostCenter returns the attributes, on the posting date, of the
// responsible cost center of line i's internal order, for the line's
// profit center. The center's locks are its own postings': the order is
// the line's cost object, so only a center with no period on the date
// (ended, not begun, or unknown to the ledger) refuses the line, which
// would have no profit center to take.
func orderCostCenter(i int, c *assignedCostCenter, date time.Time) (CostCenterAttrs, error) {
	var spans []span[CostCenterAttrs]
	if c != nil {
		spans = c.spans
	}
	attr, ok := spanOn(spans, date, false)
	if !ok {
		return CostCenterAttrs{}, orderRefused(i, reasonCenterNotOnDate)
	}
	return attr, nil
}

// validCostCenter returns the attributes of the cost center in force on
// the posting date, refusing it (422 ledger.cost_center_invalid with the
// reason) when it is unknown, of another company, not active, not valid on
// the date or locked for the line's kind, or of a category that takes no
// revenue on a revenue line.
func validCostCenter(i int, c *assignedCostCenter, le uuid.UUID, date time.Time, element config.CostElement, fromDefault bool,
	revenue map[string]bool) (CostCenterAttrs, error) {
	what := "The cost center"
	if fromDefault {
		what = "The account's default cost center"
	}
	refuse := func(reason string) (CostCenterAttrs, error) {
		return CostCenterAttrs{}, centerRefused("ledger.cost_center_invalid", i, "cost_center_id", what, reason)
	}
	if reason := centerState(c != nil, c != nil && c.legalEntity == le, c != nil && c.status == CenterActive); reason != "" {
		return refuse(reason)
	}
	attr, ok := spanOn(c.spans, date, fromDefault)
	switch {
	case !ok:
		return refuse(reasonNotValidOnDate)
	case element == config.CostElementRevenue && !revenue[attr.Category]:
		return refuse(reasonRevenueNotAllowed)
	case element == config.CostElementRevenue && attr.LockRevenue:
		return refuse(reasonLockedForRevenue)
	case element == config.CostElementPrimaryCost && attr.LockCost:
		return refuse(reasonLockedForCosts)
	}
	return attr, nil
}

// centerState is why a center read for a line cannot be posted: unknown,
// not the company's, not active; "" when it can.
func centerState(known, inCompany, active bool) string {
	switch {
	case !known:
		return reasonUnknown
	case !inCompany:
		return reasonNotInCompany
	case !active:
		return reasonNotActive
	}
	return ""
}

// spanOn is the attributes of the span in force on date. A default center
// is valid from the day it was set with no end (T56c), so an automatic
// posting dated before that, such as a revaluation at a past period end,
// precedes its validity: lenient takes its first span then.
func spanOn[A comparable](spans []span[A], date time.Time, lenient bool) (A, bool) {
	for _, s := range spans {
		if !date.Before(s.From) && date.Before(endOf(s.To)) {
			return s.Attr, true
		}
	}
	if lenient && len(spans) > 0 && date.Before(spans[0].From) {
		return spans[0].Attr, true
	}
	var none A
	return none, false
}

// readCostCenters reads the cost centers with their active periods, locked
// as assign says when lock is set.
func readCostCenters(ctx context.Context, tx pgx.Tx, ids []uuid.UUID, lock bool) (map[uuid.UUID]*assignedCostCenter, error) {
	out := map[uuid.UUID]*assignedCostCenter{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, _ := tx.Query(ctx, `SELECT id, legal_entity_id, status FROM ledger.cost_center WHERE id = ANY($1) ORDER BY id`+keyShare(lock), ids)
	var id uuid.UUID
	var c assignedCostCenter
	if _, err := pgx.ForEachRow(rows, []any{&id, &c.legalEntity, &c.status}, func() error { cc := c; out[id] = &cc; return nil }); err != nil {
		return nil, fmt.Errorf("ledger: cost centers of the lines: %w", err)
	}
	periods, err := costCenterPeriods(ctx, tx, ids, periodLock(lock))
	if err != nil {
		return nil, err
	}
	for id, c := range out {
		c.spans = periods[id]
	}
	return out, nil
}

// revenueAllowed reads, when a revenue line has a cost center, whether
// each category of those centers allows revenue (controlling.
// cost_center_categories). Unlocked: an activation changing a category
// applies to the postings after it, which read it anew.
func revenueAllowed(ctx context.Context, tx pgx.Tx, lines []Line, r assignRules, centers map[uuid.UUID]*assignedCostCenter) (map[string]bool, error) {
	var codes []string
	for _, l := range lines {
		c := centers[ptrOr(l.CostCenterID)]
		if c == nil || r.accounts[l.AccountID].element != config.CostElementRevenue {
			continue
		}
		for _, s := range c.spans {
			codes = append(codes, s.Attr.Category)
		}
	}
	out := map[string]bool{}
	if len(codes) == 0 {
		return out, nil
	}
	rows, _ := tx.Query(ctx, `SELECT code, revenue_allowed FROM ledger.cost_center_category WHERE code = ANY($1)`, codes)
	var code string
	var allowed bool
	_, err := pgx.ForEachRow(rows, []any{&code, &allowed}, func() error { out[code] = allowed; return nil })
	return out, errs.Wrapf(err, "ledger: cost center categories of revenue lines")
}

// deriveProfitCenters gives a P&L line without a profit center the
// company's default while profit center accounting applies (decision P2).
// A company with profit center accounting has one
// (company_controlling_needs_default).
func (r assignRules) deriveProfitCenters(lines []Line, found []lineCenters) {
	for i := range lines {
		l := &lines[i]
		if l.ProfitCenterID == nil && r.profitOn && !r.accounts[l.AccountID].balanceSheet {
			l.ProfitCenterID, found[i].pcDefault = r.defaultPC, true
		}
	}
}

// assignedProfitCenter is a profit center as a posting reads it.
type assignedProfitCenter struct {
	status    string
	segment   *uuid.UUID
	inCompany bool
	spans     []span[ProfitCenterAttrs]
}

// checkLineProfitCenters validates each line's profit center on the posting
// date, as validCostCenter does cost centers, and gives the line the
// profit center's segment: a segment given is refused unless it is that
// one (422 ledger.segment_mismatch).
func checkLineProfitCenters(ctx context.Context, tx pgx.Tx, le uuid.UUID, date time.Time, lines []Line, found []lineCenters, lock bool) error {
	centers, err := readProfitCenters(ctx, tx, le, centerIDs(lines, func(l Line) *uuid.UUID { return l.ProfitCenterID }), lock)
	if err != nil {
		return err
	}
	for i := range lines {
		l := &lines[i]
		if l.ProfitCenterID == nil {
			if l.SegmentID != nil {
				return segmentRefused(i)
			}
			continue
		}
		c := centers[*l.ProfitCenterID]
		what := "The profit center"
		if found[i].pcDefault {
			what = "The derived profit center"
		}
		if reason := profitCenterState(c, date, found[i].pcDefault); reason != "" {
			return centerRefused("ledger.profit_center_invalid", i, "profit_center_id", what, reason)
		}
		if l.SegmentID != nil && !equalIDs(l.SegmentID, c.segment) {
			return segmentRefused(i)
		}
		l.SegmentID = c.segment
	}
	return nil
}

// profitCenterState is why the profit center cannot be posted on the
// date: unknown, not assigned to the company, not active, not valid or
// locked then; "" when it can.
func profitCenterState(c *assignedProfitCenter, date time.Time, lenient bool) string {
	if reason := centerState(c != nil, c != nil && c.inCompany, c != nil && c.status == CenterActive); reason != "" {
		return reason
	}
	attr, ok := spanOn(c.spans, date, lenient)
	switch {
	case !ok:
		return reasonNotValidOnDate
	case attr.Locked:
		return reasonLocked
	}
	return ""
}

// readProfitCenters reads the profit centers with their active periods and
// whether each is assigned to the legal entity, locked as assign says.
func readProfitCenters(ctx context.Context, tx pgx.Tx, le uuid.UUID, ids []uuid.UUID, lock bool) (map[uuid.UUID]*assignedProfitCenter, error) {
	out := map[uuid.UUID]*assignedProfitCenter{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, _ := tx.Query(ctx, `SELECT c.id, c.status, c.segment_id, EXISTS (SELECT FROM ledger.profit_center_company k
			WHERE k.tenant_id = c.tenant_id AND k.profit_center_id = c.id AND k.legal_entity_id = $2)
		FROM ledger.profit_center c WHERE c.id = ANY($1) ORDER BY c.id`+keyShare(lock), ids, le)
	var id uuid.UUID
	var c assignedProfitCenter
	if _, err := pgx.ForEachRow(rows, []any{&id, &c.status, &c.segment, &c.inCompany}, func() error { pc := c; out[id] = &pc; return nil }); err != nil {
		return nil, fmt.Errorf("ledger: profit centers of the lines: %w", err)
	}
	periods, err := profitCenterPeriods(ctx, tx, ids, periodLock(lock))
	if err != nil {
		return nil, err
	}
	for id, c := range out {
		c.spans = periods[id]
	}
	return out, nil
}

func segmentRefused(i int) error {
	return problem.Unprocessable("ledger.segment_mismatch", fmt.Sprintf("Line %d: the segment is not its profit center's; the segment is derived "+
		"from the profit center.", i+1),
		problem.FieldError{Field: fmt.Sprintf("lines[%d].segment_id", i), Code: "mismatch", Message: "the segment of the line's profit center, or none"})
}

// centerRefused is the refusal of line i's center for the reason.
func centerRefused(code string, i int, field, what, reason string) error {
	return problem.Unprocessable(code, fmt.Sprintf("Line %d: %s %s.", i+1, what, reasonTexts[reason]),
		problem.FieldError{Field: fmt.Sprintf("lines[%d].%s", i, field), Code: reason, Message: reasonTexts[reason]})
}

// centerIDs are the distinct centers the lines name, by the field given.
func centerIDs(lines []Line, field func(Line) *uuid.UUID) []uuid.UUID {
	var out []uuid.UUID
	for _, l := range lines {
		if id := field(l); id != nil && !slices.Contains(out, *id) {
			out = append(out, *id)
		}
	}
	return out
}

func ptrOr(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

// keyShare is the lock a posting takes on the centers it reads (assign).
func keyShare(lock bool) string {
	if lock {
		return " FOR KEY SHARE"
	}
	return noLock
}

// periodLock is the lock a posting takes on the center periods it reads.
func periodLock(lock bool) string {
	if lock {
		return periodsForShare
	}
	return noLock
}

// refuseParkedOrders refuses a parked entry or an entry template whose
// line names an internal order (ADR-0064 §2, As built): a kept line has no
// column for one in the first version, so the order would be lost when the
// entry is posted. An order is named on an entry that is posted at once.
func refuseParkedOrders(lines []Line) error {
	for i, l := range lines {
		if l.InternalOrderID != nil {
			return problem.Unprocessable("ledger.internal_order_not_parkable", fmt.Sprintf("Line %d names an internal order, which a parked entry "+
				"or an entry template cannot keep yet; post the entry directly.", i+1),
				problem.FieldError{Field: fmt.Sprintf("lines[%d].internal_order_id", i), Code: "not_parkable", Message: "no internal order on a parked entry or a template"})
		}
	}
	return nil
}
