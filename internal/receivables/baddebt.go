package receivables

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Bad debt relief (ADR-0049 As built, bad debt relief; ADR-0063 As built;
// the tax specialist's confirmation of 2026-11-21, relayed by the product
// owner): the output tax of a customer invoice written off as a bad debt
// is recovered by an adjustment the tax return reports, as a step of its
// own. A plain write-off or a short payment corrects no tax (ADR-0065
// T86d, ADR-0063 T75f), and a dispute's price cut is a credit memo, whose
// tax is the invoice's share. The write-off is a dispute case's (T86d),
// which collections records; receivables reads it through WriteOffs.

// reliefRule is what a country's law asks before the output tax of a bad
// debt is recovered, as the coordinator's research of 2026-11-21 states it
// (memory po-2026-11-21-uae-vat-research; tracker leaf, src
// po:2026-11-21), for the tax specialist to confirm against the law's
// text: the law cited; the direction of the adjustment code the relief is
// posted under, which decides the line of the return that reports it
// (ADR-0049 §11: adjustments are entries of a code of kind adjustment
// mapped to their box); the months that must have passed since the
// supply (0: none; the research says six months and the tracker leaf
// "past the date of supply": counted, by default of this step, from the
// later of the invoice's date and its due date, the stricter reading, and
// only once they have passed in full); and whether the customer must have
// been notified of what was written off.
type reliefRule struct {
	law       string
	direction string
	months    int
	notice    bool
}

// reliefRules are the countries whose companies recover the output tax of
// a bad debt, by country code (the company's, as its tax codes and returns
// are its country's): the UAE under Federal Decree-Law No. 8 of 2017
// Art. 64 (six months, written off in the accounts, the customer
// notified), the adjustment an output code levying the sale's emirate,
// which the VAT 201 reports in box 1's adjustment column of that emirate;
// Canada under the Excise Tax Act s.231 (the account written off as bad),
// the adjustment an input code of the GST/HST, which the GST34 reports on
// line 107, adjustments that decrease the net tax.
var reliefRules = map[string]reliefRule{
	"AE": {law: "Federal Decree-Law No. 8 of 2017 Art. 64", direction: config.TaxOutput, months: 6, notice: true},
	"CA": {law: "Excise Tax Act s.231", direction: config.TaxInput},
}

// WrittenOffKey is the account determination key the relief credits: the
// payment differences account a dispute case's write-off debited
// (collections.WriteOffKey), so the tax recovered reduces the loss the
// write-off booked.
const WrittenOffKey = "payment_difference_expense"

// ReliefDocumentType is the journal document type of a relief: a general
// document, as a dispute case's write-off is.
const ReliefDocumentType = "SA"

// WrittenOff is what a dispute case's write-off (ADR-0065 T86d) wrote off
// of a customer invoice's open item: the case, the write-off's entry by
// fiscal year and document number, the date it posted on, the amount,
// in the invoice's currency and, at the item's booked rate, the
// company's, and the lines of the write-off's entry that credit the
// invoice's items to the customer, by number: open items beside the
// invoice's (T86d), which a payment may apply with it (recharge.go).
type WrittenOff struct {
	DisputeCaseID    uuid.UUID
	EntryYear        int16
	EntryNumber      string
	PostingDate      time.Time
	Amount, AmountLC money.Minor
	CreditLines      []int32
}

// WriteOffs reads what a write-off wrote off of a customer invoice's
// entry (collections context, which receivables does not import: the
// composition root wires it); ok is false when the entry is no write-off
// of that invoice.
type WriteOffs interface {
	WrittenOff(ctx context.Context, writeOffEntry uuid.UUID, invoiceYear int16, invoiceEntry uuid.UUID) (WrittenOff, bool, error)
}

// TaxItems reads an entry's tax items by item number (ledger context).
type TaxItems interface {
	Of(ctx context.Context, entryID uuid.UUID) ([]ledger.TaxItem, error)
}

// ReliefInput is a claim of bad debt relief as a client makes it: the
// invoice, the write-off it relieves, the adjustment code, the date the
// customer was notified of what was written off (nil: not given) and the
// posting date (nil: the company's today).
type ReliefInput struct {
	InvoiceID          uuid.UUID
	WriteOffEntryID    uuid.UUID
	TaxCode            string
	CustomerNotifiedOn *time.Time
	PostingDate        *time.Time
}

// reliefShare is a tax item of the invoice and the part of it recovered,
// in the invoice's currency and in the company's, positive.
type reliefShare struct {
	item    ledger.TaxItem
	tc, lc  money.Minor
	account uuid.UUID // the adjustment component's account it posts to
}

// claim is a relief that may be posted: the company, its rule, the
// invoice, what the write-off wrote off, the posting date and the shares.
type claim struct {
	le       tenancy.Entity
	rule     reliefRule
	invoice  Invoice
	written  WrittenOff
	date     time.Time
	in       ReliefInput
	shares   []reliefShare
	tax, lc  money.Minor
	expensed uuid.UUID
	before   reliefTotals
}

// reliefTotals are what an invoice's reliefs wrote off and recovered, in
// its currency, and the tax in the company's.
type reliefTotals struct {
	written, tax, lc money.Minor
}

// ClaimBadDebtRelief recovers the output tax of a customer invoice a
// dispute case's write-off wrote off, once per write-off of the invoice
// (ADR-0049 As built, bad debt relief): the invoice is read under its row
// lock (ADR-0008), the conditions of its company's country checked
// (reliefRule), the tax share of what was written off posted as an entry
// of the adjustment code, per tax item of the invoice and in the emirate
// each named, against the payment differences account the write-off
// debited, and the relief recorded with its audit record, all in the
// request's transaction (ADR-0007, ADR-0011). A second claim of the same
// write-off waits on the lock and is then refused (409
// receivables.bad_debt_relief_claimed); bad_debt_relief_once holds it too.
func (e *Endpoints) ClaimBadDebtRelief(ctx context.Context, in ReliefInput) (Relief, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Relief{}, err
	}
	v, err := get(ctx, tx, in.InvoiceID, true)
	if err != nil {
		return Relief{}, problem.Reference(err, "customer_invoice_id", "customer invoice", "a posted customer invoice of a company the caller may act in")
	}
	c, err := e.reliefClaim(ctx, tx, v, in)
	if err != nil {
		return Relief{}, err
	}
	id := uuid.Must(uuid.NewV7())
	entry, err := e.postRelief(ctx, &c, id)
	if err != nil {
		return Relief{}, err
	}
	if err := insertRelief(ctx, tx, p, id, c, entry); err != nil {
		return Relief{}, err
	}
	r, err := loadRelief(ctx, tx, id)
	if err != nil {
		return Relief{}, err
	}
	return r, recordRelief(ctx, r)
}

// reliefClaim checks the claim and works out its shares: an invoice of a
// company whose country recovers a bad debt's tax (reliefable), a posting
// date not before the write-off, the write-off of this invoice
// (writtenOff), the country's conditions (rule.check), not yet relieved
// (relieved), not paid beyond what its write-offs leave (unpaid), the tax
// it bore (reliefShares) and the adjustment code and account it is posted
// to (reliefLines).
func (e *Endpoints) reliefClaim(ctx context.Context, tx pgx.Tx, v Invoice, in ReliefInput) (claim, error) {
	le, _, err := e.LegalEntities.Entity(ctx, v.LegalEntityID) // a stored invoice's legal entity exists
	if err != nil {
		return claim{}, fmt.Errorf("receivables: company of customer invoice %s: %w", v.ID, err)
	}
	c := claim{le: le, invoice: v, in: in}
	if c.rule, err = reliefable(le, v); err != nil {
		return claim{}, err
	}
	// Its payments are read under the customer's hold (RechargeLock).
	if _, err := relievedOf(ctx, tx, le.ID, []uuid.UUID{v.CustomerID}); err != nil {
		return claim{}, err
	}
	if c.date, err = e.reliefDate(ctx, le, in.PostingDate); err != nil {
		return claim{}, err
	}
	if c.written, err = e.writtenOff(ctx, v, in.WriteOffEntryID, c.date); err != nil {
		return claim{}, err
	}
	if err := c.rule.check(v, c.date, in.CustomerNotifiedOn); err != nil {
		return claim{}, err
	}
	if c.before, err = relieved(ctx, tx, v, in.WriteOffEntryID, c.written); err != nil {
		return claim{}, err
	}
	if err := e.unpaid(ctx, tx, c); err != nil {
		return claim{}, err
	}
	if err := e.reliefShares(ctx, &c); err != nil {
		return claim{}, err
	}
	return c, e.reliefAccounts(ctx, &c)
}

// reliefable is the rule of the invoice's company's country: only a posted
// invoice (409 receivables.invalid_status otherwise), not a credit or
// debit memo (422 receivables.bad_debt_not_invoice), of a company whose
// country recovers a bad debt's tax (422
// receivables.bad_debt_relief_not_provided), each naming
// customer_invoice_id.
func reliefable(le tenancy.Entity, v Invoice) (reliefRule, error) {
	rule, ok := reliefRules[le.CountryCode]
	switch {
	case !ok:
		return reliefRule{}, problem.Invalid("receivables.bad_debt_relief_not_provided", "customer_invoice_id", fmt.Sprintf("Company %s, of country %s, "+
			"has no bad debt relief: the output tax of what it writes off stays as invoiced, until its country's rule is confirmed by its tax specialist.",
			le.Code, le.CountryCode))
	case v.Kind != KindInvoice:
		return reliefRule{}, problem.Invalid("receivables.bad_debt_not_invoice", "customer_invoice_id", "Customer "+strings.ReplaceAll(v.Kind, "_", " ")+
			" "+v.DocumentNumber+" is no invoice: bad debt relief recovers the tax of a supply invoiced.")
	case v.Status != Posted:
		return reliefRule{}, problem.Conflict("receivables.invalid_status", "Customer invoice "+v.DocumentNumber+" is "+v.Status+
			"; only a posted invoice is written off and relieved.")
	}
	return rule, nil
}

// reliefDate is the date the relief posts on and its tax date: the
// request's, else the company's today in its time zone.
func (e *Endpoints) reliefDate(ctx context.Context, le tenancy.Entity, requested *time.Time) (time.Time, error) {
	if requested != nil {
		return *requested, nil
	}
	day, err := config.DocumentDay(ctx, e.Settings, le.Code, clock.Now(e.Now))
	if err != nil {
		return time.Time{}, fmt.Errorf("receivables: posting date of a bad debt relief in company %s: %w", le.Code, err)
	}
	return day.Date, nil
}

// writtenOff is what the named write-off wrote off of the invoice: 422
// receivables.not_written_off naming write_off_journal_entry_id when the
// entry is no dispute case's write-off of the invoice's open item, and 422
// receivables.relief_before_write_off naming posting_date when the relief
// would post before it.
func (e *Endpoints) writtenOff(ctx context.Context, v Invoice, entry uuid.UUID, date time.Time) (WrittenOff, error) {
	w, ok, err := e.WriteOffs.WrittenOff(ctx, entry, *v.EntryYear, *v.EntryID) // a posted invoice names its entry (customer_invoice_posted)
	if err != nil {
		return WrittenOff{}, fmt.Errorf("receivables: write-off %s of customer invoice %s: %w", entry, v.DocumentNumber, err)
	}
	if !ok {
		return WrittenOff{}, problem.Invalid("receivables.not_written_off", "write_off_journal_entry_id", "Journal entry "+entry.String()+
			" is no write-off of customer invoice "+v.DocumentNumber+": a bad debt is written off by resolving a dispute case over the invoice "+
			"with a write-off (POST /v1/dispute-cases/{id}:resolve), whose entry the relief names.")
	}
	if date.Before(w.PostingDate) {
		return WrittenOff{}, problem.Invalid("receivables.relief_before_write_off", "posting_date", "The relief posts on or after "+
			w.PostingDate.Format(time.DateOnly)+", the date write-off "+w.EntryNumber+" posted on.")
	}
	return w, nil
}

// check holds the claim to the rule's conditions: a relief dated after
// the months have passed since the later of the invoice's date (its tax
// date, the supply's) and its due date (422
// receivables.bad_debt_relief_too_early naming posting_date),
// and the customer's notice where the rule asks for one (422
// receivables.customer_not_notified naming customer_notified_on). A notice
// given is dated from the invoice's date to the relief's (422
// receivables.invalid_notice_date).
func (r reliefRule) check(v Invoice, date time.Time, notified *time.Time) error {
	since := v.InvoiceDate
	if v.NetDueDate != nil && v.NetDueDate.After(since) { // a posted invoice names its due date (customer_invoice_posted)
		since = *v.NetDueDate
	}
	if passed := since.AddDate(0, r.months, 0); r.months > 0 && !date.After(passed) {
		return problem.Invalid("receivables.bad_debt_relief_too_early", "posting_date", fmt.Sprintf("Under %s the relief is claimed once %d months "+
			"have passed since the supply (counted, by default, from the later of the invoice's date and its due date, %s for customer invoice %s): "+
			"it posts after %s.", r.law, r.months, since.Format(time.DateOnly), v.DocumentNumber, passed.Format(time.DateOnly)))
	}
	if notified == nil {
		if r.notice {
			return problem.Invalid("receivables.customer_not_notified", "customer_notified_on", "Under "+r.law+" the customer is notified of "+
				"what was written off before its tax is recovered: give the date it was notified.")
		}
		return nil
	}
	if notified.Before(v.InvoiceDate) || notified.After(date) {
		return problem.Invalid("receivables.invalid_notice_date", "customer_notified_on", "The customer is notified from "+
			v.InvoiceDate.Format(time.DateOnly)+", the invoice's date, to "+date.Format(time.DateOnly)+", the relief's.")
	}
	return nil
}

// relieved are the invoice's reliefs so far, read under its row lock,
// which orders two claims of it (bad_debt_relief_once refuses the second
// if both got this far): a write-off of the invoice relieved already is
// refused, 409 receivables.bad_debt_relief_claimed, and so is one that,
// with what earlier reliefs relieved, would relieve more than the
// invoice's gross amount, 422 receivables.bad_debt_relief_exceeds_invoice
// naming write_off_journal_entry_id (a write-off leaves the invoice's item
// open, so a second case may write it off again).
func relieved(ctx context.Context, tx pgx.Tx, v Invoice, entry uuid.UUID, w WrittenOff) (reliefTotals, error) {
	var claimed bool
	var t reliefTotals
	if err := tx.QueryRow(ctx, `SELECT coalesce(bool_or(write_off_entry_id = $2), false), coalesce(sum(written_off_amount), 0)::bigint,
			coalesce(sum(tax_amount), 0)::bigint, coalesce(sum(tax_amount_lc), 0)::bigint
		FROM receivables.bad_debt_relief WHERE customer_invoice_id = $1`, v.ID, entry).Scan(&claimed, &t.written, &t.tax, &t.lc); err != nil {
		return reliefTotals{}, fmt.Errorf("receivables: the bad debt reliefs of customer invoice %s: %w", v.DocumentNumber, err)
	}
	if claimed {
		return reliefTotals{}, claimedTwice(v.DocumentNumber, w.EntryNumber)
	}
	// Recorded reliefs are each less than the gross: their sum and this write-off's are in range.
	if t.written+w.Amount > v.Gross {
		return reliefTotals{}, problem.Invalid("receivables.bad_debt_relief_exceeds_invoice", "write_off_journal_entry_id", fmt.Sprintf("Write-off %s "+
			"wrote off %s of customer invoice %s, whose reliefs so far relieved %s of its gross amount of %s: its tax is recovered once.", w.EntryNumber,
			w.Amount.Plain(v.Exponent), v.DocumentNumber, t.written.Plain(v.Exponent), v.Gross.Plain(v.Exponent)))
	}
	return t, nil
}

// unpaid refuses relieving a write-off of an invoice the customer has
// paid beyond what its write-offs, this one with the earlier ones', leave
// owed (paidOf, recharge.go): what was paid is no bad debt, and a later
// payment would charge the relief's tax straight back (422
// receivables.bad_debt_paid naming write_off_journal_entry_id). The
// customer's hold orders it with its payments (RechargeLock).
func (e *Endpoints) unpaid(ctx context.Context, tx pgx.Tx, c claim) error {
	s, err := e.reliefStateOf(ctx, tx, c.invoice)
	if err != nil {
		return err
	}
	for _, line := range c.written.CreditLines {
		s.credits = append(s.credits, ledger.ItemRef{FiscalYear: c.written.EntryYear, EntryID: c.in.WriteOffEntryID, LineNo: line})
	}
	paid, foreign, err := paidOf(ctx, tx, c.invoice, s.credits)
	switch {
	case err != nil:
		return err
	case foreign:
		return relievedOtherCurrency(c.invoice)
	}
	// What was written off is at most the gross (relieved): in range.
	if owed := c.invoice.Gross - c.before.written - c.written.Amount; paid > owed {
		return problem.Invalid("receivables.bad_debt_paid", "write_off_journal_entry_id", fmt.Sprintf("The customer has paid %s of customer "+
			"invoice %s, more than the %s its write-offs leave owed: what was paid is no bad debt.", paid.Plain(c.invoice.Exponent),
			c.invoice.DocumentNumber, owed.Plain(c.invoice.Exponent)))
	}
	return nil
}

func init() {
	problem.RegisterConstraint("bad_debt_relief_once", func() *problem.Problem {
		return problem.Conflict("receivables.bad_debt_relief_claimed", "The write-off of the invoice was relieved by another request in the meantime.")
	})
}

// claimedTwice is the refusal of a write-off of an invoice relieved once.
func claimedTwice(invoice, writeOff string) *problem.Problem {
	return problem.Conflict("receivables.bad_debt_relief_claimed", "Write-off "+writeOff+" of customer invoice "+invoice+
		" is relieved already: a write-off's tax is recovered once.")
}

// reliefShares is the tax share of what was written off of each of the
// invoice's output tax items. Counted cumulatively, as sales credits a
// billing item's tax (ADR-0067): the invoice's output tax times the part
// of its gross its reliefs, this one with the earlier, wrote off, in its
// currency and at its booked rate in the company's, each rounded once half
// away from zero, less what the earlier reliefs recovered, so partial
// write-offs never recover more than the invoice bore. The total is shared
// out over the items by their tax (money.ShareOut), so every item keeps its
// code, component and emirate, and the local total over the items whose
// share is not nothing, by their local tax, so the lines add up to it. An
// invoice that bore no output tax, or whose share rounds to nothing, is
// refused (422 receivables.no_output_tax naming customer_invoice_id).
func (e *Endpoints) reliefShares(ctx context.Context, c *claim) error {
	items, err := e.TaxItems.Of(ctx, *c.invoice.EntryID)
	if err != nil {
		return fmt.Errorf("receivables: tax items of customer invoice %s: %w", c.invoice.DocumentNumber, err)
	}
	var taxes, taxesLC []money.Minor
	var total, totalLC money.Minor
	for _, it := range items {
		if it.Direction != config.TaxOutput || it.Tax == 0 {
			continue
		}
		// An invoice's output tax is credited: posted amounts, in range negated and summed.
		c.shares = append(c.shares, reliefShare{item: it})
		taxes, taxesLC = append(taxes, -it.Tax), append(taxesLC, -it.TaxLC)
		total, totalLC = total-it.Tax, totalLC-it.TaxLC
	}
	part := big.NewRat(int64(c.before.written+c.written.Amount), int64(c.invoice.Gross))    // at most the gross (relieved): at most 1
	tax, _ := money.Round(new(big.Rat).Mul(part, new(big.Rat).SetInt64(int64(total))), 0)   // at most the tax: in range
	lcs, _ := money.Round(new(big.Rat).Mul(part, new(big.Rat).SetInt64(int64(totalLC))), 0) // at most the tax: in range
	c.tax, c.lc = tax-c.before.tax, lcs-c.before.lc                                         // what the earlier reliefs recovered is part of the cumulative share
	if c.tax <= 0 {
		return problem.Invalid("receivables.no_output_tax", "customer_invoice_id", "Customer invoice "+c.invoice.DocumentNumber+" bore no output tax "+
			"that what write-off "+c.written.EntryNumber+" wrote off would recover.")
	}
	tc := money.ShareOut(c.tax, taxes)
	for i := range c.shares {
		if c.shares[i].tc = tc[i]; tc[i] == 0 {
			taxesLC[i] = 0 // no line takes it (reliefLines)
		}
	}
	// nil when no item with a share carries local tax: none is shared, and
	// reliefLines records the local tax the lines post.
	for i, lc := range money.ShareOut(c.lc, taxesLC) {
		c.shares[i].lc = lc
	}
	return nil
}

// reliefAccounts finds the account each share posts to, its component's
// in the adjustment code (reliefCode), and the payment differences
// account the relief credits (422 receivables.no_write_off_account
// without one).
func (e *Endpoints) reliefAccounts(ctx context.Context, c *claim) error {
	code, err := e.reliefCode(ctx, c)
	if err != nil {
		return err
	}
	for i, sh := range c.shares {
		comp, err := reliefComponent(code, sh.item)
		if err != nil {
			return err
		}
		c.shares[i].account = *comp.AccountID // reliefComponent returns one with its account
	}
	account, ok, err := e.Accounts.Determination(ctx, c.le.ID, WrittenOffKey)
	if err != nil {
		return fmt.Errorf("receivables: the payment differences account of company %s: %w", c.le.Code, err)
	}
	if !ok {
		return problem.Unprocessable("receivables.no_write_off_account", "The relief is credited to the company's "+WrittenOffKey+
			" account, which the write-off debited, and company "+c.le.Code+" has none in its account determination.")
	}
	c.expensed = account
	return nil
}

// reliefCode is the adjustment code the relief posts under: a tax code of
// the company in force on its date, of kind adjustment and the direction
// the country's return reports the relief by (reliefRule), else 422
// receivables.invalid_relief_tax_code naming tax_code.
func (e *Endpoints) reliefCode(ctx context.Context, c *claim) (ledger.TaxCode, error) {
	code, ok, err := e.Accounts.TaxCode(ctx, c.le.ID, c.in.TaxCode, c.date)
	if err != nil {
		return ledger.TaxCode{}, fmt.Errorf("receivables: tax code %s of company %s: %w", c.in.TaxCode, c.le.Code, err)
	}
	if !ok || code.Kind != config.TaxAdjustment || code.Direction != c.rule.direction {
		return ledger.TaxCode{}, invalidReliefCode(fmt.Sprintf("Tax code %q is no %s adjustment code of company %s in force on %s: under %s the "+
			"relief is an adjustment of kind adjustment, direction %s, which the return reports (ADR-0049 §11).", c.in.TaxCode, c.rule.direction,
			c.le.Code, c.date.Format(time.DateOnly), c.rule.law, c.rule.direction))
	}
	return code, nil
}

// reliefComponent is the adjustment code's component a tax item's share
// posts under: of the item's component key, with an account, levying the
// line's subdivision exactly where the item named one (an emirate; a
// jurisdiction of the subdivision form, ADR-0049 §7), so the return
// reports the relief where the sale was; else 422
// receivables.invalid_relief_tax_code.
func reliefComponent(code ledger.TaxCode, it ledger.TaxItem) (ledger.TaxComponent, error) {
	sub := it.Jurisdiction != nil && strings.Contains(*it.Jurisdiction, "-")
	for _, comp := range code.Components {
		if comp.Key == it.Component && comp.AccountID != nil && comp.JurisdictionOnLine == sub {
			return comp, nil
		}
	}
	where := ""
	if sub {
		where = ", levying the subdivision its lines name (" + *it.Jurisdiction + ")"
	}
	return ledger.TaxComponent{}, invalidReliefCode(fmt.Sprintf("Tax code %s has no component %s with an account%s, which the invoice's tax of "+
		"code %s is recovered under.", code.Code, it.Component, where, it.Code))
}

// invalidReliefCode is the refusal of the adjustment code named.
func invalidReliefCode(detail string) *problem.Problem {
	return problem.Invalid("receivables.invalid_relief_tax_code", "tax_code", detail)
}
