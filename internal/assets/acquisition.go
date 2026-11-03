package assets

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Acquisition statuses (ADR-0057 §5).
const (
	AcquisitionPosted   = "posted"
	AcquisitionReversed = "reversed"
)

// documentTypeAA is the journal entries' document type of acquisitions,
// retirements and transfers (number range journal_aa, ADR-0057 §3).
const documentTypeAA = "AA"

// sourceAssets is the source context of the assets context's entries,
// whose lines on a clearing account settle it and are no source.
const sourceAssets = "assets"

// NewAcquisition is an acquisition (ADR-0057 §6) from a posted line
// (Line) or by manual entry against an offset account (OffsetAccountID),
// one of the two. From a line, Amount is in the line's currency, nil for
// its default; against an offset account it is required and positive, in
// AmountCurrency. The dates are nil for their defaults.
type NewAcquisition struct {
	AssetID                uuid.UUID
	Line                   *LineKey
	OffsetAccountID        *uuid.UUID
	Amount                 *money.Minor
	AmountCurrency         string
	ValueDate, PostingDate *time.Time
	Text                   *string
}

// LineKey names a journal line by its entry and number.
type LineKey struct {
	EntryID uuid.UUID
	LineNo  int32
}

// acquiring is what an acquisition is posted from, gathered and checked
// under the asset's lock before anything is written: the source line, or
// for a reversal's line also the line it reverses, or the offset account
// of a manual acquisition, and the amount in the posting's currency and
// in local and group currency (a manual acquisition's are the ledger's,
// known once posted).
type acquiring struct {
	asset                            Asset
	company                          Company
	cost                             uuid.UUID
	clearing                         uuid.UUID
	line                             Line
	reverses                         *LineRef
	offset                           *AccountRef
	use                              clearingUse
	value                            time.Time
	posting                          time.Time
	currency, currencyLC, currencyGC string
	amount                           share
}

// share is what an acquisition takes of its line in each currency.
type share struct{ tc, lc, gc money.Minor }

// postAcquisition capitalises a posted line on the asset's class's
// clearing account onto the asset (ADR-0057 §6): checked under the
// asset's lock and the line's clearing_use lock, posted to every ledger
// the company keeps, with the asset's movements, its status and
// capitalisation date derived again, audited and published.
func (e *Endpoints) postAcquisition(ctx context.Context, in NewAcquisition) (Acquisition, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Acquisition{}, err
	}
	a, err := e.prepareAcquisition(ctx, tx, in)
	if err != nil {
		return Acquisition{}, err
	}
	id := uuid.Must(uuid.NewV7())
	seq := a.asset.LastSeq + 1
	entries, err := e.Ledger.Post(ctx, a.entry(id, in.Text))
	if err != nil {
		return Acquisition{}, fmt.Errorf("assets: post acquisition of asset %s: %w", a.asset.Number, err)
	}
	if err := e.costPosted(ctx, &a, entries); err != nil {
		return Acquisition{}, err
	}
	if err := writeAcquisition(ctx, tx, p, id, seq, a, in.Text, entries); err != nil {
		return Acquisition{}, err
	}
	acq, err := readAcquisition(ctx, tx, id)
	if err != nil {
		return Acquisition{}, err
	}
	return acq, recordAcquisition(ctx, tx, acq)
}

// prepareAcquisition reads and checks what the acquisition is posted from.
func (e *Endpoints) prepareAcquisition(ctx context.Context, tx pgx.Tx, in NewAcquisition) (acquiring, error) {
	asset, err := readAsset(ctx, tx, in.AssetID, true)
	if err != nil {
		return acquiring{}, err
	}
	if err := takesDocuments(asset); err != nil {
		return acquiring{}, err
	}
	a := acquiring{asset: asset}
	doc, err := e.accountsOf(ctx, &a)
	if err != nil {
		return acquiring{}, err
	}
	if in.OffsetAccountID != nil {
		err = e.prepareManual(ctx, tx, doc, &a, in)
	} else {
		err = e.prepareFromLine(ctx, tx, doc, &a, in)
	}
	return a, err
}

// prepareFromLine reads and checks the source line and what the
// acquisition takes of it: from a reversal's line, at most what the asset
// may take off (takeOff).
func (e *Endpoints) prepareFromLine(ctx context.Context, tx pgx.Tx, doc *config.Config, a *acquiring, in NewAcquisition) error {
	if err := e.sourceLine(ctx, a, *in.Line); err != nil {
		return err
	}
	if err := e.datesOf(ctx, tx, doc, a, in); err != nil {
		return err
	}
	var err error
	if a.use, err = lockUse(ctx, tx, a.line, a.reverses); err != nil {
		return err
	}
	amount, currency := in.Amount, in.AmountCurrency
	if a.reverses != nil {
		if amount == nil {
			currency = a.line.Currency // the default is in the line's
		}
		if amount, err = takeOff(ctx, tx, *a, amount); err != nil {
			return err
		}
	}
	if a.amount, err = a.use.take(amount, currency); err != nil {
		return err
	}
	return costStaysPositive(ctx, tx, *a)
}

// prepareManual checks the offset account of a manual acquisition
// (ADR-0057 §6): set up in the company and postable there, no
// reconciliation account of any kind nor the GR/IR account (the ledger's
// OffsetAccount), and no asset clearing account of the company, whose
// lines the worklist settles (422 assets.offset_account_invalid).
func (e *Endpoints) prepareManual(ctx context.Context, tx pgx.Tx, doc *config.Config, a *acquiring, in NewAcquisition) error {
	off, ok, err := e.Ledger.OffsetAccount(ctx, a.company.ID, *in.OffsetAccountID)
	if err != nil {
		return fmt.Errorf("assets: offset account %s: %w", *in.OffsetAccountID, err)
	}
	clearing, err := e.clearingAccounts(ctx, doc, a.company)
	if err != nil {
		return err
	}
	if _, isClearing := clearing[off.ID]; !ok || isClearing {
		return invalid("assets.offset_account_invalid", fmt.Sprintf("Account %s cannot be the offset of an acquisition in company %s: it is not "+
			"postable there, is a reconciliation or GR/IR account, or is an asset clearing account.", *in.OffsetAccountID, a.company.Code),
			"offset_account_id", "a postable G/L account of the company, no reconciliation and no asset clearing account")
	}
	a.offset, a.currency, a.amount = &off, in.AmountCurrency, share{tc: *in.Amount}
	return e.datesOf(ctx, tx, doc, a, in)
}

// costPosted sets a manual acquisition's local and group amounts and
// currencies to its cost line's as the ledger posted them, converted at
// the posting date's rate; the copies in other ledgers carry the leading
// entry's amounts. An acquisition from a line keeps the line's.
func (e *Endpoints) costPosted(ctx context.Context, a *acquiring, entries []PostedEntry) error {
	if a.offset == nil {
		return nil
	}
	// The entry was just posted with its cost line first, so the line is found.
	l, _, err := e.Ledger.Line(ctx, entries[0].ID, 1)
	if err != nil {
		return fmt.Errorf("assets: cost line of entry %s: %w", entries[0].DocumentNumber, err)
	}
	a.amount, a.currencyLC, a.currencyGC = share{tc: l.Amount, lc: l.LC, gc: l.GC}, l.CurrencyLC, l.CurrencyGC
	return nil
}

// takesDocuments refuses an asset that takes no documents (ADR-0057 §5).
func takesDocuments(a Asset) error {
	switch a.Status {
	case StatusCancelled:
		return problem.Unprocessable("assets.asset_cancelled", fmt.Sprintf("Asset %s is cancelled; it takes no documents.", a.Number))
	case StatusRetired:
		return problem.Unprocessable("assets.asset_retired", fmt.Sprintf("Asset %s is retired; it takes no documents.", a.Number))
	}
	return nil
}

// accountsOf reads the asset's company and its class's acquisition cost
// and clearing accounts there, and returns the active document.
func (e *Endpoints) accountsOf(ctx context.Context, a *acquiring) (*config.Config, error) {
	co, err := e.company(ctx, a.asset.LegalEntityID)
	if err != nil {
		return nil, err
	}
	a.company = co
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("assets: configuration: %w", err)
	}
	_, accounts, err := classOf(doc, co, a.asset.Class)
	if err != nil {
		return nil, err
	}
	// Activation sets up every account the asset accounts name in the
	// company (ADR-0057 §3), so both are found.
	ids, err := e.Ledger.Accounts(ctx, co.ID, []string{accounts.AcquisitionCost, accounts.Clearing})
	if err != nil {
		return nil, fmt.Errorf("assets: accounts of class %s in %s: %w", a.asset.Class, co.Code, err)
	}
	a.cost, a.clearing = ids[accounts.AcquisitionCost], ids[accounts.Clearing]
	return doc, nil
}

// sourceLine reads the source line and checks it is the leading ledger's
// line on the class's clearing account in the asset's company, not one of
// an assets entry (an acquisition's own clearing line), not reversed, and
// in every ledger the company keeps (decided by default 14). A reversal's
// line names the line it reverses, its own number in the reversed entry.
func (e *Endpoints) sourceLine(ctx context.Context, a *acquiring, in LineKey) error {
	l, ok, err := e.Ledger.Line(ctx, in.EntryID, in.LineNo)
	if err != nil {
		return fmt.Errorf("assets: line %d of entry %s: %w", in.LineNo, in.EntryID, err)
	}
	switch {
	case !ok || !l.Leading || l.LegalEntityID != a.asset.LegalEntityID || l.AccountID != a.clearing || l.SourceContext == sourceAssets:
		return invalid("assets.not_clearing_line", fmt.Sprintf("Line %d of entry %s is no line of the leading ledger on the clearing account of class %s in "+
			"the asset's company.", in.LineNo, in.EntryID, a.asset.Class), "source", "a line on the class's clearing account (GET /v1/asset-clearing-items)")
	case l.ReversedBy != nil:
		return invalid("assets.source_reversed", fmt.Sprintf("Entry %s was reversed by entry %s; its line cannot be capitalised, and its reversal's "+
			"line takes the cost off the assets holding it.", l.DocumentNumber, l.ReversedBy.DocumentNumber), "source", "a line of an entry that is not reversed")
	case !l.InEveryLedger:
		return invalid("assets.source_not_in_every_ledger", fmt.Sprintf("Entry %s was not posted to every ledger the company keeps, so an acquisition, "+
			"which posts to each, cannot take its line.", l.DocumentNumber), "source", "a line of an entry posted to every ledger")
	}
	a.line, a.currency, a.currencyLC, a.currencyGC = l, l.Currency, l.CurrencyLC, l.CurrencyGC
	if l.Reverses != nil {
		a.reverses = &LineRef{FiscalYear: l.Reverses.FiscalYear, EntryID: l.Reverses.EntryID, LineNo: l.LineNo}
	}
	return nil
}

// takeOff is the amount an acquisition from a reversal's line takes
// (ADR-0057 §6, decided by default 23): of the reversal line's sign, at
// most what the asset holds in posted acquisitions from the line it
// reverses less what it already took from the reversal's line, all of
// that by default. An asset holding nothing from that line is refused (422
// assets.source_reversed), as is the reversal of a line nothing was
// capitalised from; more than the bound is 422 assets.amount_exceeds_line.
func takeOff(ctx context.Context, tx pgx.Tx, a acquiring, amount *money.Minor) (*money.Minor, error) {
	var held, taken money.Minor
	err := tx.QueryRow(ctx, `SELECT coalesce(sum(amount) FILTER (WHERE source_entry_id = $2), 0), coalesce(sum(amount) FILTER (WHERE source_entry_id = $3), 0)
		FROM assets.acquisition WHERE asset_id = $1 AND status = 'posted' AND source_entry_id IN ($2, $3) AND source_line_no = $4`,
		a.asset.ID, a.reverses.EntryID, a.line.EntryID, a.line.LineNo).Scan(&held, &taken)
	if err != nil {
		return nil, fmt.Errorf("assets: what asset %s holds of line %d of entry %s: %w", a.asset.Number, a.line.LineNo, a.reverses.EntryID, err)
	}
	if held == 0 {
		return nil, invalid("assets.source_reversed", fmt.Sprintf("Entry %s reverses an entry whose line %d asset %s holds nothing of; its line "+
			"takes cost off only the assets holding the reversed line.", a.line.DocumentNumber, a.line.LineNo, a.asset.Number),
			"source", "a reversal's line, onto an asset holding the line it reverses")
	}
	bound := -(held + taken) // in the reversal line's sign
	if amount == nil {
		amount = &bound
	}
	// An amount of the other sign is take's to refuse.
	if bound == 0 || (bound > 0 && *amount > bound) || (bound < 0 && *amount < bound) {
		return nil, invalid("assets.amount_exceeds_line", fmt.Sprintf("Asset %s may take %d minor units of %s off through the reversal's line: what it "+
			"holds from the line reversed less what it took through this one.", a.asset.Number, int64(bound), a.line.Currency),
			"amount", "at most what the asset may take off")
	}
	return amount, nil
}

// datesOf sets the posting date (the company's date today by default)
// and the value date (by default the line's posting date, or a manual
// acquisition's posting date), refusing a
// value date after the end of the posting date's period (422
// assets.value_date_in_future) and a posting date before the asset's
// latest assignment took effect (409 assets.posting_before_center_transfer,
// ADR-0057 §8).
func (e *Endpoints) datesOf(ctx context.Context, tx pgx.Tx, doc *config.Config, a *acquiring, in NewAcquisition) error {
	if in.PostingDate != nil {
		a.posting = *in.PostingDate
	} else {
		today, err := doc.CompanyDay(a.company.Code, clock.Now(e.Now))
		if err != nil {
			return fmt.Errorf("assets: %w", err)
		}
		a.posting = today.Date
	}
	a.value = a.line.PostingDate
	if a.offset != nil {
		a.value = a.posting
	}
	if in.ValueDate != nil {
		a.value = *in.ValueDate
	}
	per, err := e.Ledger.Period(ctx, a.company.ID, a.posting)
	if err != nil {
		return fmt.Errorf("assets: period of %s: %w", a.posting.Format(time.DateOnly), err)
	}
	if a.value.After(per.End) {
		return invalid("assets.value_date_in_future", fmt.Sprintf("The value date %s is after the end of the posting date's period, %s.",
			a.value.Format(time.DateOnly), per.End.Format(time.DateOnly)), "value_date", "a date on or before "+per.End.Format(time.DateOnly))
	}
	from, err := latestAssignment(ctx, tx, a.asset.ID)
	if err != nil {
		return err
	}
	if a.posting.Before(from) {
		return problem.Conflict("assets.posting_before_center_transfer", fmt.Sprintf("Asset %s is on its current centers from %s; an acquisition is "+
			"posted on or after that day, with an earlier value date if need be.", a.asset.Number, from.Format(time.DateOnly)))
	}
	return nil
}

// take is what an acquisition of amount (in currency; nil: the rest)
// takes of the line in each currency. The amount has the line's sign
// and is at most what is left (422 assets.amount_exceeds_line). Local
// and group amounts are the line's in proportion to what is used after
// it, rounded half away from zero, less what was used before, so what is
// used never exceeds the line in any currency and the acquisition taking
// the rest of the line takes the rest of its local and group amounts.
func (u clearingUse) take(amount *money.Minor, currency string) (share, error) {
	if amount != nil && currency != u.currency {
		return share{}, validation("amount.currency", "the line's currency, "+u.currency)
	}
	left := u.line.tc - u.used.tc
	tc := left
	if amount != nil {
		tc = *amount
	}
	if tc == 0 || (tc > 0) != (u.line.tc > 0) || (tc > 0 && tc > left) || (tc < 0 && tc < left) {
		return share{}, invalid("assets.amount_exceeds_line", fmt.Sprintf("The line has %d minor units of %s left; an acquisition takes an amount of "+
			"the line's sign up to that.", left, u.currency), "amount", "an amount of the line's sign, at most what is left")
	}
	used := u.used.tc + tc
	lc, gc := proportion(u.line.lc, used, u.line.tc), proportion(u.line.gc, used, u.line.tc)
	return share{tc: tc, lc: lc - u.used.lc, gc: gc - u.used.gc}, nil
}

// proportion is amount × num / den rounded half away from zero; den is
// never zero (clearing_use's line_amount <> 0).
func proportion(amount, num, den money.Minor) money.Minor {
	v := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(int64(amount)), big.NewInt(int64(num))), big.NewInt(int64(den)))
	m, _ := money.Round(v, 0) // never out of range: |num| ≤ |den|, so the result is at most amount in size
	return m
}

// entry is the acquisition's entry (ADR-0057 §6), Dr the class's
// acquisition cost account on the asset's profit center: from a line, in
// its currency, Cr the clearing account on the line's profit center, each
// line keeping the acquisition's local and group amounts; against an
// offset account, Cr that account on the asset's profit center (decided
// by default), both converted at the posting date's rate.
func (a acquiring) entry(id uuid.UUID, text *string) Posting {
	header := text
	if header == nil {
		h := "Asset " + a.asset.Number + " " + a.asset.Description
		header = &h
	}
	cost := PostingLine{AccountID: a.cost, Amount: a.amount.tc, ProfitCenterID: a.asset.ProfitCenterID, Text: header}
	other := PostingLine{AccountID: a.clearing, Amount: -a.amount.tc, ProfitCenterID: a.line.ProfitCenterID, Text: header}
	if a.offset != nil {
		other.AccountID, other.ProfitCenterID = a.offset.ID, a.asset.ProfitCenterID
	} else {
		lc, gc, offLC, offGC := a.amount.lc, a.amount.gc, -a.amount.lc, -a.amount.gc
		cost.LC, cost.GC, other.LC, other.GC = &lc, &gc, &offLC, &offGC
	}
	return Posting{LegalEntityID: a.company.ID, DocumentType: documentTypeAA, PostingDate: a.posting, DocumentDate: a.posting, Currency: a.currency,
		HeaderText: header, DocumentID: id, Lines: []PostingLine{cost, other}}
}
