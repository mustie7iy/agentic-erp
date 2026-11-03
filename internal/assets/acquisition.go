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

// NewAcquisition is an acquisition from a posted line (ADR-0057 §6):
// Amount in the line's currency, nil for all that is left of it; the
// dates nil for their defaults.
type NewAcquisition struct {
	AssetID                uuid.UUID
	EntryID                uuid.UUID
	LineNo                 int32
	Amount                 *money.Minor
	AmountCurrency         string
	ValueDate, PostingDate *time.Time
	Text                   *string
}

// acquiring is what an acquisition is posted from, gathered and checked
// under the asset's lock before anything is written.
type acquiring struct {
	asset    Asset
	company  Company
	cost     uuid.UUID
	clearing uuid.UUID
	line     Line
	use      clearingUse
	value    time.Time
	posting  time.Time
	amount   share
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
	if err := writeAcquisition(ctx, tx, p, id, seq, a, in.Text, entries); err != nil {
		return Acquisition{}, err
	}
	acq, err := readAcquisition(ctx, tx, id)
	if err != nil {
		return Acquisition{}, err
	}
	return acq, recordAcquisition(ctx, tx, acq, a)
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
	if err := e.sourceLine(ctx, &a, in); err != nil {
		return acquiring{}, err
	}
	if err := e.datesOf(ctx, tx, doc, &a, in); err != nil {
		return acquiring{}, err
	}
	if a.use, err = lockUse(ctx, tx, a.line); err != nil {
		return acquiring{}, err
	}
	if a.amount, err = a.use.take(in.Amount, in.AmountCurrency); err != nil {
		return acquiring{}, err
	}
	return a, costStaysPositive(ctx, tx, a)
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
// an assets entry (an acquisition's own clearing line), neither
// reversed nor a reversal (T57d takes a reversal's line), and in every
// ledger the company keeps (decided by default 14).
func (e *Endpoints) sourceLine(ctx context.Context, a *acquiring, in NewAcquisition) error {
	l, ok, err := e.Ledger.Line(ctx, in.EntryID, in.LineNo)
	if err != nil {
		return fmt.Errorf("assets: line %d of entry %s: %w", in.LineNo, in.EntryID, err)
	}
	switch {
	case !ok || !l.Leading || l.LegalEntityID != a.asset.LegalEntityID || l.AccountID != a.clearing || l.SourceContext == sourceAssets:
		return invalid("assets.not_clearing_line", fmt.Sprintf("Line %d of entry %s is no line of the leading ledger on the clearing account of class %s in "+
			"the asset's company.", in.LineNo, in.EntryID, a.asset.Class), "source", "a line on the class's clearing account (GET /v1/asset-clearing-items)")
	case l.Reversed || l.Reversal:
		return invalid("assets.source_reversed", fmt.Sprintf("Entry %s was reversed or reverses another; its line cannot be capitalised.", l.DocumentNumber),
			"source", "a line of an entry that is not reversed")
	case !l.InEveryLedger:
		return invalid("assets.source_not_in_every_ledger", fmt.Sprintf("Entry %s was not posted to every ledger the company keeps, so an acquisition, "+
			"which posts to each, cannot take its line.", l.DocumentNumber), "source", "a line of an entry posted to every ledger")
	}
	a.line = l
	return nil
}

// datesOf sets the posting date (the company's date today by default)
// and the value date (the line's posting date by default), refusing a
// value date after the end of the posting date's period (422
// assets.value_date_in_future) and a posting date before the asset's
// latest assignment took effect (409 assets.posting_before_center_transfer,
// ADR-0057 §8).
func (e *Endpoints) datesOf(ctx context.Context, tx pgx.Tx, doc *config.Config, a *acquiring, in NewAcquisition) error {
	a.posting, a.value = a.line.PostingDate, a.line.PostingDate
	if in.PostingDate != nil {
		a.posting = *in.PostingDate
	} else {
		today, err := doc.CompanyDay(a.company.Code, clock.Now(e.Now))
		if err != nil {
			return fmt.Errorf("assets: %w", err)
		}
		a.posting = today.Date
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

// entry is the acquisition's entry (ADR-0057 §6): in the line's
// currency, Dr the class's acquisition cost account on the asset's profit
// center, Cr the clearing account on the line's, each line keeping the
// acquisition's local and group amounts.
func (a acquiring) entry(id uuid.UUID, text *string) Posting {
	header := text
	if header == nil {
		h := "Asset " + a.asset.Number + " " + a.asset.Description
		header = &h
	}
	return Posting{LegalEntityID: a.company.ID, DocumentType: documentTypeAA, PostingDate: a.posting, DocumentDate: a.posting, Currency: a.line.Currency,
		HeaderText: header, DocumentID: id, Lines: []PostingLine{
			{AccountID: a.cost, Amount: a.amount.tc, LC: a.amount.lc, GC: a.amount.gc, ProfitCenterID: a.asset.ProfitCenterID, Text: header},
			{AccountID: a.clearing, Amount: -a.amount.tc, LC: -a.amount.lc, GC: -a.amount.gc, ProfitCenterID: a.line.ProfitCenterID, Text: header},
		}}
}
