package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TaxReturns tells the ledger which tax return holds a tax date (the tax
// filing context; ADR-0049 P6, §5 As built). A tax date chosen for an
// entry, a manual entry's or a person's on a supplier invoice, is held to
// periods no return reports yet: once a return of the period is prepared
// its items are claimed, and a further item dated there would have to
// wait for the next return or an amendment.
type TaxReturns interface {
	// Holding returns the prepared, approved or filed return of a
	// registration covering the legal entity whose period holds the date;
	// ok is false when no return holds it. It locks the registrations
	// covering the legal entity FOR SHARE for the rest of the
	// transaction: a return being prepared locks its registration FOR
	// UPDATE before it claims items, so either it waits for the posting
	// and claims its items, or the posting waits for it and sees it.
	Holding(ctx context.Context, legalEntity uuid.UUID, taxDate time.Time) (HeldReturn, bool, error)
}

// HeldReturn is the return that holds a tax date: its registration's
// code, its period, kind (original or amendment) and status (prepared,
// approved or filed).
type HeldReturn struct {
	ID                     uuid.UUID
	Registration           string
	PeriodStart, PeriodEnd time.Time
	Kind, Status           string
}

// errNoTaxReturns is a ledger wired without the tax filing context, which
// must not post a held entry as though no return existed.
var errNoTaxReturns = errors.New("ledger: no tax returns port to hold the tax date to")

// heldTaxDate reports whether the entry's tax date is held to the open
// returns: a manual entry's (the ledger's own, given or its posting
// date), or one a posting context says a person chose (Posting.TaxDateChosen).
func (in NewEntry) heldTaxDate() bool {
	return in.SourceContext == "" || in.SourceContext == "ledger" || in.taxDateChosen
}

// holdTaxDate refuses an entry with a tax code whose tax date, day, is
// held and lies in a period a return of a registration covering its company
// already reports (ADR-0049 P6): 409 ledger.tax_date_in_return. Entries
// without tax, and those whose tax date the ledger or a document decides
// (a reversal's, an invoice's invoice date), are not held: a late item
// goes to the next return or the period's amendment (P7).
func holdTaxDate(ctx context.Context, returns TaxReturns, le uuid.UUID, held bool, day time.Time, lines []Line) error {
	if !held || !taxed(lines) {
		return nil
	}
	if returns == nil {
		return errNoTaxReturns
	}
	r, ok, err := returns.Holding(ctx, le, day)
	if err != nil || !ok {
		return err
	}
	return problem.Conflict("ledger.tax_date_in_return", fmt.Sprintf("The tax date %s lies in period %s to %s of tax registration %s, "+
		"whose %s return %s is %s: a tax date is chosen in a period no return reports yet (ADR-0049 P6). Date the entry in an open "+
		"tax period; the period's own late items go to its amendment or the next return.", day.Format(time.DateOnly),
		r.PeriodStart.Format(time.DateOnly), r.PeriodEnd.Format(time.DateOnly), r.Registration, r.Kind, r.ID, r.Status))
}
