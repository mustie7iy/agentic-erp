package procurement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Account assignment of requisition and order items (ADR-0050 §6, T56e):
// SAP's account assignment category, none or a cost center (K), with the
// cost center. An item's assignment is checked against the ledger, as its
// postings will be, when its document is released: a requisition as it
// is submitted, an order as it is released (and again as its release is
// approved), the items an order change adds as it is submitted and
// applied. A released order's items keep their assignment (000161), so the
// receipts, their reversals and returns, and the invoice's price
// difference post to one center.
const (
	AssignNone       = "none"
	AssignCostCenter = "cost_center"
)

func init() {
	problem.RegisterConstraint("requisition_item_account_assignment", invalidAssignment)
	problem.RegisterConstraint("purchase_order_item_account_assignment", invalidAssignment)
}

func invalidAssignment() *problem.Problem {
	return problem.Unprocessable("procurement.invalid_account_assignment", "An item assigned to a cost center names it, and only such an item names one.",
		problem.FieldError{Field: "items", Code: "invalid", Message: "account_assignment cost_center with a cost_center_id, or none without"})
}

// itemAssignment is the assignment an item input gives: as sent, else a
// cost center's when it names one, else none. An item assigned to a cost
// center names it, and only such an item names one.
func itemAssignment(field string, it ItemInput) (string, *uuid.UUID, error) {
	a := AssignNone
	if it.CostCenterID != nil {
		a = AssignCostCenter
	}
	if it.AccountAssignment != nil {
		a = *it.AccountAssignment
	}
	if (a != AssignNone && a != AssignCostCenter) || (a == AssignCostCenter) != (it.CostCenterID != nil) {
		return "", nil, invalid("procurement.invalid_account_assignment", field+".account_assignment",
			"cost_center with the item's cost_center_id, or none without one")
	}
	return a, it.CostCenterID, nil
}

// checkAssignments checks items' assignments against the ledger as a
// posting in legal entity le on date checks them (ledger
// CheckAssignments): the cost center one of the legal entity, active,
// valid on the date and not locked for costs, and named on a primary cost
// account while cost center accounting applies, without the account's
// default.
func (e *Endpoints) checkAssignments(ctx context.Context, le uuid.UUID, date time.Time, items []ledger.ItemAssignment) error {
	if len(items) == 0 {
		return nil
	}
	return e.Accounts.CheckAssignments(ctx, le, date, items, false)
}

// checkSubmitted checks a requisition's assignments as it is submitted
// for approval (T56e), on its company's date in its time zone: a center
// change takes effect from the company's date, not UTC's (product owner
// 2026-10-31, T72n4), so a Toronto requisition submitted at 22:00 is
// judged on that day. A refusal names the date it was judged on and the
// zone, so a consumer told a center is not valid yet sees which day it was.
func (e *Endpoints) checkSubmitted(ctx context.Context, r Requisition) error {
	le, found, err := e.LegalEntities.Entity(ctx, r.LegalEntityID)
	if err == nil && !found {
		err = errors.New("no such legal entity")
	}
	if err != nil {
		return fmt.Errorf("procurement: legal entity %s of requisition %s: %w", r.LegalEntityID, r.ID, err)
	}
	day, loc, err := e.today(ctx, le.Code)
	if err != nil {
		return err
	}
	err = e.checkAssignments(ctx, r.LegalEntityID, day, assignments(r.Items, 0))
	var p *problem.Problem
	if errors.As(err, &p) {
		// A copy: the ledger's refusal is not changed in place.
		q := *p
		q.Detail += " A requisition's assignments are checked on the company's date in its time zone as it is submitted: " + day.Format(time.DateOnly) + " in " + loc.String() + "."
		return &q
	}
	return err
}

// assignments are the items from position from on as the ledger checks
// them.
func assignments(items []Item, from int) []ledger.ItemAssignment {
	var out []ledger.ItemAssignment
	for i := from; i < len(items); i++ {
		out = append(out, ledger.ItemAssignment{Index: i, AccountID: items[i].AccountID, CostCenterID: items[i].CostCenterID})
	}
	return out
}
