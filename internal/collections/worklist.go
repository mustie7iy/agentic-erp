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
// date for a customer none of whose open items was dunned. Disputed is
// what the open dispute cases of its open items dispute, in the
// company's currency, and DisputedCases how many they are (T86b); they
// change neither Open nor Overdue. Promised is what its promises to pay
// open on the key date promise, in the company's currency, OpenPromises
// how many they are and NextPromised the earliest of their dates;
// BrokenPromises how many of its promises are broken on the key date
// (T76f). Number, Name, Level, LastDunned and BrokenPromises are read for
// the customers of a page (describe).
type WorklistCustomer struct {
	CustomerID    uuid.UUID
	Number        string
	Name          string
	Open          money.Minor
	Overdue       money.Minor
	OldestDue     time.Time
	Level         int32
	LastDunned    *time.Time
	Disputed      money.Minor
	DisputedCases int32
	Promised      money.Minor
	OpenPromises  int32
	NextPromised  *time.Time
	// BrokenPromises is read for a page's customers (describe).
	BrokenPromises int32

	items []ledger.ReceivableItem // its open items
}

// Worklist is a company's collection worklist on a key date (ADR-0065
// §9, T76c): its customers with overdue items, the largest overdue amount
// first, and their totals, the disputed amount and open dispute cases of
// every customer listed among them (T86b) and what their open promises to
// pay promise (T76f). Exponent is the minor-unit digits of the company's
// currency, which every amount is in.
type Worklist struct {
	Company       tenancy.Entity
	KeyDate       time.Time
	Exponent      int
	Open          money.Minor
	Overdue       money.Minor
	Disputed      money.Minor
	DisputedCases int32
	Promised      money.Minor
	Customers     []WorklistCustomer

	open map[ledger.ItemRef]bool // the company's open customer items
}

// isOpen is whether the company's customer item is open now.
func (w Worklist) isOpen(ref ledger.ItemRef) bool { return w.open[ref] }

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
// now: nothing is stored. A customer is listed when an item it owes is past
// its net due date on the key date (overdue as a dunning run counts it:
// an item posted without payment terms is due on its document date);
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
	disputes, err := openCases(ctx, tx, le.ID)
	if err != nil {
		return Worklist{}, err
	}
	promises, err := readPromises(ctx, tx, "the promises to pay of company "+le.Code, promiseSelect+
		` WHERE p.legal_entity_id = $1 AND p.promised_date >= $2 ORDER BY p.promised_date, p.id`, le.ID, keyDate)
	if err != nil {
		return Worklist{}, err
	}
	w := Worklist{Company: le, KeyDate: keyDate, Exponent: exp, open: make(map[ledger.ItemRef]bool, len(open))}
	for _, it := range open {
		w.open[it.ItemRef] = true
	}
	if !w.add(open, disputes) || !w.promise(promises) {
		return Worklist{}, errTooLarge()
	}
	slices.SortFunc(w.Customers, inWorklistOrder)
	return w, nil
}

// add gives the worklist its customers and totals from the open items,
// which the ledger returns by customer, and the open dispute cases of
// those items. It reports false when amounts add up to more than an
// amount holds.
func (w *Worklist) add(open []ledger.ReceivableItem, disputes map[ledger.ItemRef]openCase) bool {
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
		var all, due, disputed []money.Minor
		for _, o := range overdue(c.items, w.KeyDate) {
			due = append(due, o.AmountLC)
			if d := o.NetDueDate(); c.OldestDue.IsZero() || d.Before(c.OldestDue) {
				c.OldestDue = d
			}
		}
		for _, it := range c.items {
			all = append(all, it.AmountLC)
			if d, ok := disputes[it.ItemRef]; ok {
				disputed = append(disputed, d.AmountLC)
			}
		}
		open = open[n:]
		if len(due) == 0 {
			continue // nothing overdue: not on the worklist
		}
		c.Open, c.Overdue, c.Disputed, c.DisputedCases = sum(all...), sum(due...), sum(disputed...), int32(len(disputed))
		w.Open, w.Overdue, w.Disputed = sum(w.Open, c.Open), sum(w.Overdue, c.Overdue), sum(w.Disputed, c.Disputed)
		w.DisputedCases += c.DisputedCases
		w.Customers = append(w.Customers, c)
	}
	return fits
}

// promise gives the listed customers what their promises to pay open on
// the key date promise (those dated on or after it, read by date, and not
// kept by the items open now), and the worklist its total. It reports
// false when amounts add up to more than an amount holds.
func (w *Worklist) promise(promises []Promise) bool {
	at := make(map[uuid.UUID]int, len(w.Customers))
	for i, c := range w.Customers {
		at[c.CustomerID] = i
	}
	fits := true
	for _, p := range promises {
		i, listed := at[p.CustomerID]
		if p.stillOpen(w.isOpen); !listed || p.StatusOn(w.KeyDate) != PromiseOpen {
			continue
		}
		c := &w.Customers[i]
		promised, err := money.Sum(c.Promised, p.Amount)
		total, errTotal := money.Sum(w.Promised, p.Amount)
		fits = fits && err == nil && errTotal == nil // money.Sum's only error is the range
		c.Promised, w.Promised, c.OpenPromises = promised, total, c.OpenPromises+1
		if c.NextPromised == nil {
			c.NextPromised = &p.Date // the promises are read by date: the first is the earliest
		}
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
// continues after the last customer's overdue amount and id as they were.
// While nothing changes, nobody is listed twice or left out. A customer
// whose overdue amount rose in between, to before that place, is not read
// again; one whose amount fell, to after it, is listed again with its new
// amount.
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
// with: each one's number and name, the highest dunning level and latest
// dunning date recorded for its open items, and how many of its promises
// to pay are broken on the worklist's key date.
func (e *Endpoints) describe(ctx context.Context, tx pgx.Tx, w Worklist, page []WorklistCustomer) error {
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
	broken, err := w.broken(ctx, tx, ids)
	if err != nil {
		return err
	}
	for i := range page {
		c := &page[i]
		named, ok := names[c.CustomerID]
		if !ok { // the ledger posts customer items to business partners only, and none is deleted
			return fmt.Errorf("collections: customer %s of an open item is no business partner", c.CustomerID)
		}
		c.Number, c.Name, c.BrokenPromises = named.Number, named.Name, broken[c.CustomerID]
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

// broken counts the customers' promises to pay broken on the worklist's
// key date: dated before it and not kept by the items open now. Only a
// page's customers are read (over the index promise_to_pay_by_customer),
// as their names are.
func (w Worklist) broken(ctx context.Context, tx pgx.Tx, customers []uuid.UUID) (map[uuid.UUID]int32, error) {
	promises, err := readPromises(ctx, tx, fmt.Sprintf("the promises to pay of %d customers", len(customers)), promiseSelect+
		` WHERE p.legal_entity_id = $1 AND p.customer_id = ANY($2) AND p.promised_date < $3 ORDER BY p.id`, w.Company.ID, customers, w.KeyDate)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]int32{}
	for _, p := range promises {
		if p.stillOpen(w.isOpen); p.StatusOn(w.KeyDate) == PromiseBroken {
			out[p.CustomerID]++
		}
	}
	return out, nil
}
