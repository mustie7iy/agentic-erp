package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
// validated again. The database holds the table of §5 at commit
// (ledger.check_line_assignment, 000155) and the centers' companies by
// foreign key.
//
// Derivation, first hit wins: the cost center as the line gives it, else,
// on a primary cost account with cost center accounting on in the posting's
// fiscal year, the company's default for the account (SAP OKB9); the
// profit center as given, else the cost center's in its period on the
// posting date (a different one given is refused), else, for a P&L line
// with profit center accounting on, the company's default; the segment is
// always the profit center's.

func init() {
	problem.RegisterConstraint("journal_line_cost_center_fkey", func() *problem.Problem {
		return problem.Unprocessable("ledger.cost_center_invalid", "A line's cost center is not a cost center of the line's legal entity.",
			problem.FieldError{Field: "lines.cost_center_id", Code: reasonNotInCompany, Message: "a cost center of the legal entity"})
	})
	problem.RegisterConstraint("journal_line_profit_center_fkey", func() *problem.Problem {
		return problem.Unprocessable("ledger.profit_center_invalid", "A line's profit center is not assigned to the line's legal entity.",
			problem.FieldError{Field: "lines.profit_center_id", Code: reasonNotInCompany, Message: "a profit center assigned to the legal entity"})
	})
	problem.RegisterConstraint("journal_line_segment_fkey", segmentMismatch)
	problem.RegisterConstraint("journal_line_segment_of_profit_center", segmentMismatch)
	problem.RegisterConstraint("journal_line_cost_center_required", func() *problem.Problem {
		return problem.Unprocessable("ledger.cost_center_required", "A line on a primary cost account needs a cost center.",
			problem.FieldError{Field: "lines.cost_center_id", Code: "required", Message: "a cost center of the legal entity"})
	})
	problem.RegisterConstraint("journal_line_cost_center_not_allowed", func() *problem.Problem {
		return problem.Unprocessable("ledger.cost_center_invalid", "A line on an account that takes no cost object has a cost center.",
			problem.FieldError{Field: "lines.cost_center_id", Code: reasonNotAllowed, Message: "none on this account"})
	})
	problem.RegisterConstraint("journal_line_cost_center_revenue", func() *problem.Problem {
		return problem.Unprocessable("ledger.cost_center_invalid", "A revenue line's cost center is of a category that takes no revenue.",
			problem.FieldError{Field: "lines.cost_center_id", Code: reasonRevenueNotAllowed, Message: "a cost center whose category allows revenue"})
	})
	problem.RegisterConstraint("journal_line_profit_center_required", func() *problem.Problem {
		return problem.Unprocessable("ledger.profit_center_required", "A P&L line needs a profit center.",
			problem.FieldError{Field: "lines.profit_center_id", Code: "required", Message: "a profit center assigned to the legal entity"})
	})
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

// entered reports whether the line gives a center of its own.
func entered(l Line) bool {
	return l.CostCenterID != nil || l.ProfitCenterID != nil || l.SegmentID != nil
}

// deriveCostCenters refuses a cost center on an account that takes none
// and gives a primary cost line without one the account's default, while
// cost center accounting applies; without a default the line is refused.
func (r assignRules) deriveCostCenters(lines []Line, found []lineCenters) error {
	for i := range lines {
		l, a := &lines[i], r.accounts[lines[i].AccountID]
		switch {
		case l.CostCenterID != nil && a.element == config.CostElementNone:
			return centerRefused("ledger.cost_center_invalid", i, "cost_center_id", "The cost center", reasonNotAllowed)
		case l.CostCenterID != nil || a.element != config.CostElementPrimaryCost || !r.costOn:
		case a.defaultCC == nil:
			return problem.Unprocessable("ledger.cost_center_required", fmt.Sprintf("Line %d posts to a primary cost account and needs a cost center; "+
				"the account has no default cost center in the legal entity.", i+1),
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
// (validCostCenter) and gives a line without a profit center its cost
// center's; a different one given is refused (422
// ledger.profit_center_mismatch), as the cost center's profit center wins
// (SAP).
func checkLineCostCenters(ctx context.Context, tx pgx.Tx, le uuid.UUID, date time.Time, lines []Line, r assignRules, found []lineCenters, lock bool) error {
	centers, err := readCostCenters(ctx, tx, centerIDs(lines, func(l Line) *uuid.UUID { return l.CostCenterID }), lock)
	if err != nil {
		return err
	}
	revenue, err := revenueAllowed(ctx, tx, lines, r, centers)
	if err != nil {
		return err
	}
	for i := range lines {
		l := &lines[i]
		if l.CostCenterID == nil {
			continue
		}
		attr, err := validCostCenter(i, centers[*l.CostCenterID], le, date, r.accounts[l.AccountID].element, found[i].ccDefault, revenue)
		if err != nil {
			return err
		}
		switch {
		case attr.ProfitCenterID == uuid.Nil:
		case l.ProfitCenterID == nil:
			pc := attr.ProfitCenterID
			l.ProfitCenterID, found[i].pcDefault = &pc, found[i].ccDefault
		case *l.ProfitCenterID != attr.ProfitCenterID:
			return problem.Unprocessable("ledger.profit_center_mismatch", fmt.Sprintf("Line %d: the profit center differs from its cost center's "+
				"on the posting date, which the line takes.", i+1),
				problem.FieldError{Field: fmt.Sprintf("lines[%d].profit_center_id", i), Code: "mismatch", Message: "the cost center's profit center, or none"})
		}
	}
	return nil
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
