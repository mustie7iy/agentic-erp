package collections

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// WorklistCustomer is a customer on a company's collection worklist: one
// with items overdue on the key date. Open is what its open items add up
// to, credits included; Overdue what its overdue items do, which credits
// do not reduce (as a dunning notice's amount, ADR-0065 §4); both are
// minor units of the company's currency, as the items were booked.
// OldestDue is the earliest net due date among its overdue items. Level
// and LastDunned are the highest dunning level and the latest dunning
// date the confirmed runs recorded for its open items: level 0 and no
// date for a customer none of whose open items was dunned. Number, Name,
// Level and LastDunned are read for the customers of a page (describe).
type WorklistCustomer struct {
	CustomerID uuid.UUID
	Number     string
	Name       string
	Open       money.Minor
	Overdue    money.Minor
	OldestDue  time.Time
	Level      int32
	LastDunned *time.Time

	items []ledger.ReceivableItem // its open items
}

// Worklist is a company's collection worklist on a key date (ADR-0065
// §9, T76c): its customers with overdue items, the largest overdue amount
// first, and their totals. Exponent is the minor-unit digits of the
// company's currency, which every amount is in.
type Worklist struct {
	Company   tenancy.Entity
	KeyDate   time.Time
	Exponent  int
	Open      money.Minor
	Overdue   money.Minor
	Customers []WorklistCustomer
}

// inWorklistOrder orders the worklist: the largest overdue amount first
// (plan default 7), customers with the same amount by id, so the order is
// total and a page continues where the last one ended.
func inWorklistOrder(a, b WorklistCustomer) int {
	if c := cmp.Compare(b.Overdue, a.Overdue); c != 0 {
		return c
	}
	return bytes.Compare(a.CustomerID[:], b.CustomerID[:])
}

// WorklistOf computes the collection worklist of a company the caller may
// act in on a key date, from the ledger's open customer items as they are
// now: nothing is stored. A customer is listed when an item it owes, posted
// with payment terms, is past its net due date on the key date (overdue);
// a customer with nothing overdue is not listed, whatever it owes. The
// customers' numbers, names and dunning are not read here: describe reads
// them for a page.
func (e *Endpoints) WorklistOf(ctx context.Context, legalEntity uuid.UUID, keyDate time.Time) (Worklist, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Worklist{}, err
	}
	if err := checkKeyDate(keyDate); err != nil {
		return Worklist{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Worklist{}, fmt.Errorf("collections: company access for the collection worklist: %w", err)
	}
	le, ok, err := e.LegalEntities.Entity(ctx, legalEntity)
	if err != nil {
		return Worklist{}, fmt.Errorf("collections: company %s of the collection worklist: %w", legalEntity, err)
	}
	if !ok || !access.Allows(legalEntity) {
		return Worklist{}, problem.NotFoundOf("legal entity") // another company's is not found, so ids cannot be probed
	}
	exp, err := currency.Exponent(ctx, tx, le.LocalCurrency, nil) // a legal entity's currency exists
	if err != nil {
		return Worklist{}, fmt.Errorf("collections: currency of company %s: %w", le.Code, err)
	}
	open, err := e.OpenItems.Receivable(ctx, le.ID, nil)
	if err != nil {
		return Worklist{}, fmt.Errorf("collections: open customer items of company %s: %w", le.Code, err)
	}
	w := Worklist{Company: le, KeyDate: keyDate, Exponent: exp}
	if !w.add(open) {
		return Worklist{}, errTooLarge()
	}
	slices.SortFunc(w.Customers, inWorklistOrder)
	return w, nil
}

// add gives the worklist its customers and totals from the open items,
// which the ledger returns by customer. It reports false when amounts add
// up to more than an amount holds.
func (w *Worklist) add(open []ledger.ReceivableItem) bool {
	fits := true
	sum := func(amounts ...money.Minor) money.Minor {
		s, err := money.Sum(amounts...) // money.Sum's only error is the range
		fits = fits && err == nil
		return s
	}
	for len(open) > 0 {
		n := 1
		for n < len(open) && open[n].PartnerID == open[0].PartnerID {
			n++
		}
		c := WorklistCustomer{CustomerID: open[0].PartnerID, items: open[:n]}
		var all, due []money.Minor
		for _, o := range overdue(c.items, w.KeyDate) {
			due = append(due, o.AmountLC)
			if d := o.Terms.NetDueDate(); c.OldestDue.IsZero() || d.Before(c.OldestDue) {
				c.OldestDue = d
			}
		}
		for _, it := range c.items {
			all = append(all, it.AmountLC)
		}
		open = open[n:]
		if len(due) == 0 {
			continue // nothing overdue: not on the worklist
		}
		c.Open, c.Overdue = sum(all...), sum(due...)
		w.Open, w.Overdue = sum(w.Open, c.Open), sum(w.Overdue, c.Overdue)
		w.Customers = append(w.Customers, c)
	}
	return fits
}

// worklistCursor is where a page of the worklist ended: its last
// customer's place in the worklist's order.
type worklistCursor struct {
	Overdue  money.Minor `json:"o"`
	Customer uuid.UUID   `json:"c"`
}

// page cuts a page of size customers from the worklist, after the
// customer a page token names, and returns the token of the next page, or
// none on the last. The token is bound to the company and the key date
// (scope). The worklist is computed again for every page, so a page
// continues after the last customer's overdue amount and id as they were:
// customers are never repeated, and one whose amount changed in between
// is read at its new place or not again.
func (e *Endpoints) page(w Worklist, scope string, size int, token string, hasToken bool) ([]WorklistCustomer, string, error) {
	rest := w.Customers
	if hasToken {
		var c worklistCursor
		if err := e.Pages.Decode(scope, token, &c); err != nil {
			return nil, "", err
		}
		after := WorklistCustomer{CustomerID: c.Customer, Overdue: c.Overdue}
		rest = rest[sort.Search(len(rest), func(i int) bool { return inWorklistOrder(rest[i], after) > 0 }):]
	}
	if len(rest) <= size {
		return rest, "", nil
	}
	last := rest[size-1]
	next, _ := e.Pages.Encode(scope, worklistCursor{Overdue: last.Overdue, Customer: last.CustomerID}) // an amount and a uuid always encode
	return rest[:size], next, nil
}

// describe completes the customers of a page with what they are shown
// with: each one's number and name, and the highest dunning level and
// latest dunning date recorded for its open items.
func (e *Endpoints) describe(ctx context.Context, tx pgx.Tx, page []WorklistCustomer) error {
	ids := make([]uuid.UUID, len(page))
	var refs []ledger.ItemRef
	for i, c := range page {
		ids[i] = c.CustomerID
		for _, it := range c.items {
			refs = append(refs, it.ItemRef)
		}
	}
	names, err := e.Partners.Names(ctx, ids)
	if err != nil {
		return fmt.Errorf("collections: customers of %d worklist rows: %w", len(page), err)
	}
	levels, err := recorded(ctx, tx, refs)
	if err != nil {
		return err
	}
	for i := range page {
		c := &page[i]
		named, ok := names[c.CustomerID]
		if !ok { // the ledger posts customer items to business partners only, and none is deleted
			return fmt.Errorf("collections: customer %s of an open item is no business partner", c.CustomerID)
		}
		c.Number, c.Name = named.Number, named.Name
		for _, it := range c.items {
			d, ok := levels[it.ItemRef]
			if !ok {
				continue
			}
			c.Level = max(c.Level, d.Level)
			if c.LastDunned == nil || d.Date.After(*c.LastDunned) {
				c.LastDunned = &d.Date
			}
		}
	}
	return nil
}
