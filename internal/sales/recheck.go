package sales

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// recheck holds items entered on an earlier date to the company's
// configuration on a later one (ADR-0067 §4; As built, the emirate on a
// sales item): an item's account must still be postable in the company,
// its tax code, when it has one, an output tax code of the company with a
// rate in force on the date, and its tax jurisdiction one that code
// levies then (none where the code levies its items' subdivision, or one
// it does not levy). An item was checked when it was entered, and the
// configuration may have changed since: a billing document checks its
// order's items on the billing date (billableOn), an order from a
// quotation the quotation's items on the order's date (orderableOn). Each
// caller refuses an item that fails in its own terms; doc names the
// document in a wrapped error. The ledger is asked about each account and
// tax code once.
type recheck struct {
	e        *Endpoints
	le       uuid.UUID
	date     time.Time
	doc      string
	accounts map[uuid.UUID]bool        // postable in the company
	codes    map[string]ledger.TaxCode // output tax codes in force on the date
}

// newRecheck is a recheck of a document of the company on the date.
func (e *Endpoints) newRecheck(le uuid.UUID, date time.Time, doc string) recheck {
	return recheck{e: e, le: le, date: date, doc: doc, accounts: map[uuid.UUID]bool{}, codes: map[string]ledger.TaxCode{}}
}

// item rechecks it, an item the document names by no (an order item's
// number, for a billing document's item): why it fails, "" when it
// passes.
func (c recheck) item(ctx context.Context, no int32, it Item) (string, error) {
	if why, err := c.account(ctx, no, it.AccountID); why != "" || err != nil {
		return why, err
	}
	return c.taxCode(ctx, no, it.TaxCode, it.TaxJurisdiction)
}

// account requires item no's account to be postable in the company
// still.
func (c recheck) account(ctx context.Context, no int32, id uuid.UUID) (string, error) {
	if c.accounts[id] {
		return "", nil
	}
	ok, err := c.e.Accounts.IsPostable(ctx, c.le, id)
	if err != nil {
		return "", fmt.Errorf("sales: account of item %d of %s: %w", no, c.doc, err)
	}
	if !ok {
		return "its account can no longer be posted in the legal entity", nil
	}
	c.accounts[id] = true
	return "", nil
}

// taxCode requires item no's tax code, when it has one, to be an output
// tax code of the company with a rate in force on the date, and its tax
// jurisdiction to be one that code levies then
// (ledger.TaxCode.CheckJurisdiction).
func (c recheck) taxCode(ctx context.Context, no int32, code, jurisdiction *string) (string, error) {
	if code == nil {
		return "", nil // an item names a jurisdiction only with a code (sales_order_item_tax_jurisdiction, quotation_item_tax_jurisdiction)
	}
	t, ok, err := c.e.outputCodeOn(ctx, c.codes, c.le, *code, c.date)
	if err != nil {
		return "", fmt.Errorf("sales: tax code %s of item %d of %s: %w", *code, no, c.doc, err)
	}
	if !ok {
		return "its tax code " + *code + " is not an output tax code of the legal entity with a rate on that date", nil
	}
	switch fault, _ := t.CheckJurisdiction(jurisdiction); fault {
	case ledger.JurisdictionOK:
		return "", nil
	case ledger.JurisdictionRequired:
		return "its tax code " + *code + " levies the tax of the subdivision each item names, and the item names none", nil
	default:
		return "its tax code " + *code + " does not levy its tax jurisdiction " + *jurisdiction + " on that date", nil
	}
}
