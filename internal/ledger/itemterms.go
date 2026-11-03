package ledger

import (
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The due-date and cash discount rules of a supplier open item, shared by
// the payment proposer (T36) and the liquidity forecast (ADR-0051 §5,
// T53e): the forecast expects an item when the payment run would pay it,
// so both read one rule rather than a copy each.

// TierOn is the index of the cash discount tier the terms grant for a
// payment on the date (the first whose last day is on or after it), or -1
// for none: no terms, or every tier has lapsed. Tiers give less the later
// they are (T36a), so a later index is a smaller discount.
func (t *ItemTerms) TierOn(on time.Time) int {
	if t == nil {
		return -1
	}
	for i, d := range t.Discounts {
		if !on.After(t.Until(d)) {
			return i
		}
	}
	return -1
}

// LosesDiscount says whether paying the item on the due-by date rather
// than on the run date would earn less discount: such an item is paid in
// a run although it is not yet due, as SAP's payment run pays an item
// whose discount would lapse before the next run.
func (t *ItemTerms) LosesDiscount(runDate, dueBy time.Time) bool {
	now, later := t.TierOn(runDate), t.TierOn(dueBy)
	return now >= 0 && (later < 0 || later > now)
}

// DiscountOn is the cash discount on an item paid (in the sign it is paid
// in) on the date: its tier's percent of the amount, rounded half away
// from zero to the currency's minor unit, item by item. A discount that
// would round to the whole amount is one minor unit less, so an item
// always pays something (payment_item_discount_within).
func (t *ItemTerms) DiscountOn(paid money.Minor, on time.Time) (money.Minor, error) {
	i := t.TierOn(on)
	if i < 0 || paid == 0 { // a zero item has nothing to take a discount off
		return 0, nil
	}
	d, err := money.Scale(paid, t.Discounts[i].Percent, 100)
	if err != nil {
		return 0, err
	}
	switch {
	case d.Abs() < paid.Abs():
		return d, nil
	case paid > 0:
		return paid - 1, nil
	}
	return paid + 1, nil
}

// DueDate is when the open item is due: an invoice (a credit on the
// supplier account) its supplier's net days after its document date; every
// debit on the supplier account (a credit memo, whether or not it names an
// invoice, a subsequent credit, a manual or migrated debit) on its document
// date, so it nets with the next run that pays the supplier (T34d). SAP
// has a credit memo without an invoice reference due at once and gives one
// naming an invoice that invoice's due date; netting earlier never pays
// more than is owed. A credit posted with payment terms (T36b) is due on
// its net due date by the terms it was posted with, so a later change to
// the term does not move it; one posted without them (before T36, manual,
// migrated) by its supplier's net days now, supplierNetDays.
func (p PayableItem) DueDate(supplierNetDays int) time.Time {
	switch {
	case p.Amount > 0:
		return p.DocumentDate
	case p.Terms != nil:
		return p.Terms.NetDueDate()
	}
	return p.DocumentDate.AddDate(0, 0, supplierNetDays)
}
