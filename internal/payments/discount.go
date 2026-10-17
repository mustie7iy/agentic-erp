package payments

import (
	"time"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// tierOn is the index of the cash discount tier the terms grant for a
// payment on the date (the first whose last day is on or after it), or -1
// for none: no terms, or every tier has lapsed. Tiers give less the later
// they are (T36a), so a later index is a smaller discount.
func tierOn(t *ledger.ItemTerms, on time.Time) int {
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

// losesDiscount says whether paying the item on the due-by date rather
// than on the run date would earn less discount: such an item is paid in
// this run although it is not yet due, as SAP's payment run pays an item
// whose discount would lapse before the next run.
func losesDiscount(t *ledger.ItemTerms, runDate, dueBy time.Time) bool {
	now := tierOn(t, runDate)
	return now >= 0 && tierOn(t, dueBy) != now
}

// discountOn is the cash discount on an item paid (in the sign it is paid
// in) on the date: its tier's percent of the amount, rounded half away
// from zero to the currency's minor unit, item by item. A discount that
// would round to the whole amount is one minor unit less, so an item
// always pays something (payment_item_discount_within).
func discountOn(t *ledger.ItemTerms, paid money.Minor, on time.Time) (money.Minor, error) {
	i := tierOn(t, on)
	if i < 0 {
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
