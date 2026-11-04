package assets

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Transfer kinds and statuses (ADR-0057 §5, §8).
const (
	TransferCenter   = "center"
	TransferAsset    = "asset"
	TransferPosted   = "posted"
	TransferReversed = "reversed"
)

// NewTransfer is a transfer to post (ADR-0057 §8): of kind center, to the
// cost center (its profit center derived) or, for an asset without one,
// the profit center given; of kind asset, of a share of the asset to
// ToAssetID, as a percentage (Percent, in ten-thousandths of a percent)
// or a cost (Cost, in CostCurrency, to be the company's), neither for all
// of it. The dates are nil for their defaults.
type NewTransfer struct {
	AssetID                      uuid.UUID
	Kind                         string
	CostCenterID, ProfitCenterID *uuid.UUID
	ToAssetID                    *uuid.UUID
	Percent                      *int64
	Cost                         *money.Minor
	CostCurrency                 string
	ValueDate, PostingDate       *time.Time
	Text                         *string
}

// transferring is a transfer gathered and checked under its assets'
// locks before anything is posted: the asset (and an asset transfer's
// receiver, to), their company and class accounts, the dates, the posting
// period V and the period before it, a center transfer's new centers, and
// what it moves per ledger the company keeps.
type transferring struct {
	in                   NewTransfer
	asset, to            Asset
	company              Company
	accounts, toAccounts retirementAccounts
	classAccounts        *config.AssetAccounts
	group                string
	value, posting       time.Time
	period               Period
	before               postingPeriod
	costCenter, profit   *uuid.UUID
	ledgers              []*transferLedger
}

// transferLedger is what a transfer moves in one ledger: its catch-up and
// the share taken (an asset transfer) or the balances moved to the new
// profit center (a center transfer, nothing without a profit center
// change) as a retirement's ledger holds them, and whether a center
// transfer's catch-up is an entry of its own (apart) dated catchUpOn, the
// last day of the period before, with that entry once posted.
type transferLedger struct {
	*retiredLedger
	apart        bool
	catchUpOn    time.Time
	catchUpEntry *PostedEntry
}

// postTransfer posts a transfer (ADR-0057 §8): checked under its assets'
// locks, per ledger the company keeps (the leading first, then by code)
// its catch-up and what it moves posted as ledger-specific entries, with
// the movements, the asset's booked_through raised, a center transfer's
// assignment row and the assets' status derived again, audited and
// published.
func (e *Endpoints) postTransfer(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in NewTransfer) (Transfer, error) {
	t, err := e.prepareTransfer(ctx, tx, in)
	if err != nil {
		return Transfer{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if err := e.postTransferEntries(ctx, id, t); err != nil {
		return Transfer{}, err
	}
	if err := writeTransfer(ctx, tx, p, id, t); err != nil {
		return Transfer{}, err
	}
	out, err := readTransfer(ctx, tx, id)
	if err != nil {
		return Transfer{}, err
	}
	return out, e.recordTransfer(ctx, tx, out, t.asset.Status)
}

// prepareTransfer reads and checks what the transfer moves.
func (e *Endpoints) prepareTransfer(ctx context.Context, tx pgx.Tx, in NewTransfer) (*transferring, error) {
	asset, to, err := lockTransferred(ctx, tx, in)
	if err != nil {
		return nil, err
	}
	t := &transferring{in: in, asset: asset, to: to}
	doc, err := e.transferAccountsOf(ctx, t)
	if err != nil {
		return nil, err
	}
	if t.posting, t.period, t.before, err = e.postingDates(ctx, doc, t.company, in.PostingDate); err != nil {
		return nil, err
	}
	if in.Kind == TransferCenter {
		return t, e.prepareCenterTransfer(ctx, tx, t)
	}
	return t, e.prepareAssetTransfer(ctx, tx, t)
}

// lockTransferred reads the asset and an asset transfer's receiver FOR
// UPDATE, in id order so that two transfers between the same assets never
// deadlock (ADR-0057 §5), and refuses an asset that takes no documents
// (ADR-0057 §5), the asset as its own receiver (422
// assets.transfer_same_asset), a receiver of another company (422
// assets.transfer_other_company) and a planned asset, which has nothing
// to transfer to another asset (422 assets.asset_not_capitalised).
func lockTransferred(ctx context.Context, tx pgx.Tx, in NewTransfer) (Asset, Asset, error) {
	if in.ToAssetID == nil {
		a, err := readAsset(ctx, tx, in.AssetID, true)
		if err == nil {
			err = takesDocuments(a)
		}
		return a, Asset{}, err
	}
	if *in.ToAssetID == in.AssetID {
		return Asset{}, Asset{}, invalid("assets.transfer_same_asset", "An asset is not transferred to itself.", "to_asset_id", "another asset")
	}
	locked, err := lockAssets(ctx, tx, in.AssetID, *in.ToAssetID)
	if err != nil {
		return Asset{}, Asset{}, err
	}
	a, to := locked[in.AssetID], locked[*in.ToAssetID]
	switch {
	case to.LegalEntityID != a.LegalEntityID:
		return Asset{}, Asset{}, invalid("assets.transfer_other_company", fmt.Sprintf("Asset %s belongs to another company than asset %s; a transfer "+
			"between companies is not offered.", to.Number, a.Number), "to_asset_id", "an asset of the same company")
	case a.Status == StatusPlanned:
		return Asset{}, Asset{}, problem.Unprocessable("assets.asset_not_capitalised", fmt.Sprintf("Asset %s is planned: it holds no acquisition, so "+
			"there is nothing to transfer.", a.Number))
	}
	return a, to, nil
}

// lockAssets reads the assets FOR UPDATE in id order and refuses one that
// takes no documents.
func lockAssets(ctx context.Context, tx pgx.Tx, ids ...uuid.UUID) (map[uuid.UUID]Asset, error) {
	out := map[uuid.UUID]Asset{}
	for _, id := range sortedIDs(ids) {
		a, err := readAsset(ctx, tx, id, true)
		if err != nil {
			return nil, err
		}
		if err := takesDocuments(a); err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, nil
}

// sortedIDs are the ids in their order, the order assets are locked in.
func sortedIDs(ids []uuid.UUID) []uuid.UUID {
	return slices.SortedFunc(slices.Values(ids), func(a, b uuid.UUID) int { return cmp.Compare(a.String(), b.String()) })
}

// transferAccountsOf reads the asset's company and its class's accounts
// there (and the receiver's class's), the tenant's group currency, and
// returns the active document.
func (e *Endpoints) transferAccountsOf(ctx context.Context, t *transferring) (*config.Config, error) {
	co, err := e.company(ctx, t.asset.LegalEntityID)
	if err != nil {
		return nil, err
	}
	t.company = co
	if c := t.in.CostCurrency; c != "" && c != co.Currency {
		return nil, validation("share.cost.currency", "the company's currency, "+co.Currency)
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("assets: configuration: %w", err)
	}
	if t.classAccounts, t.accounts, err = e.assetAccounts(ctx, doc, co, t.asset.Class); err != nil {
		return nil, err
	}
	if t.to.ID != uuid.Nil {
		if _, t.toAccounts, err = e.assetAccounts(ctx, doc, co, t.to.Class); err != nil {
			return nil, err
		}
	}
	if t.group, err = e.Companies.GroupCurrency(ctx); err != nil {
		return nil, fmt.Errorf("assets: group currency: %w", err)
	}
	return doc, nil
}

// assetAccounts are a class's accounts in the company: as configured and
// by id.
func (e *Endpoints) assetAccounts(ctx context.Context, doc *config.Config, co Company, class string) (*config.AssetAccounts, retirementAccounts, error) {
	_, a, err := classOf(doc, co, class)
	if err != nil {
		return nil, retirementAccounts{}, err
	}
	// Activation sets up every account the asset accounts name in the
	// company (ADR-0057 §3), so each is found.
	ids, err := e.Ledger.Accounts(ctx, co.ID, []string{a.AcquisitionCost, a.AccumulatedDepreciation, a.Depreciation})
	if err != nil {
		return nil, retirementAccounts{}, fmt.Errorf("assets: accounts of class %s in %s: %w", class, co.Code, err)
	}
	return a, retirementAccounts{cost: ids[a.AcquisitionCost], accumulated: ids[a.AccumulatedDepreciation], depreciation: ids[a.Depreciation]}, nil
}

// prepareAssetTransfer checks an asset transfer (ADR-0057 §6, §8): valued
// in its posting period on or before its posting date (the posting date
// by default) and not before the asset's capitalisation date; neither
// asset with anything valued or booked in a later period (409
// assets.later_value_date); posted on or after the receiver's latest
// assignment (409 assets.posting_before_center_transfer); the receiver
// with terms in every ledger; then per ledger the asset's catch-up, the
// share taken of its balances and their group amounts, as a retirement's.
func (e *Endpoints) prepareAssetTransfer(ctx context.Context, tx pgx.Tx, t *transferring) error {
	t.value = t.posting
	if t.in.ValueDate != nil {
		t.value = *t.in.ValueDate
	}
	if err := valuedInPeriod("transfer", t.value, t.posting, t.period); err != nil {
		return err
	}
	if err := notBeforeCapitalisation(t.asset, t.value); err != nil {
		return err
	}
	from, err := latestAssignment(ctx, tx, t.to.ID)
	if err != nil {
		return err
	}
	if t.posting.Before(from) { // before later_value_date, which a row valid from a later period also meets
		return problem.Conflict("assets.posting_before_center_transfer", fmt.Sprintf("Asset %s is on its current centers from %s; a transfer to it is "+
			"posted on or after that day.", t.to.Number, from.Format(time.DateOnly)))
	}
	for _, a := range []Asset{t.asset, t.to} {
		if err := laterValueDate(ctx, tx, a, t.period); err != nil {
			return err
		}
	}
	ls, err := e.catchUps(ctx, tx, t.asset, t.company, t.period)
	if err != nil {
		return err
	}
	kept := make([]LedgerRef, len(ls))
	for i, l := range ls {
		kept[i] = l.ledger
		if err := l.take(t.in.Percent, t.in.Cost, t.asset.Number); err != nil {
			return err
		}
		t.ledgers = append(t.ledgers, &transferLedger{retiredLedger: l})
	}
	if err := termsInEvery(t.to, kept, "to_asset_id"); err != nil {
		return err
	}
	if _, err := e.sharedGroupAmounts(ctx, t.company.Currency, t.group, t.posting, ls); err != nil {
		return fmt.Errorf("assets: group amounts of the transfer of asset %s: %w", t.asset.Number, err)
	}
	return nil
}

// postTransferEntries posts, per ledger in ledger order, a center
// transfer's catch-up entry where it is apart, then the main entry where
// it has lines (none: no entry), each to that ledger alone.
func (e *Endpoints) postTransferEntries(ctx context.Context, id uuid.UUID, t *transferring) error {
	for _, l := range t.ledgers {
		if l.apart {
			entries, err := e.Ledger.Post(ctx, t.entry(id, l, t.catchUpLines(l), l.catchUpOn))
			if err != nil {
				return fmt.Errorf("assets: post the catch-up of the transfer of asset %s in ledger %s: %w", t.asset.Number, l.ledger.Code, err)
			}
			l.catchUpEntry = &entries[0] // a posting to one ledger has one entry
		}
		lines := t.mainLines(l)
		if len(lines) == 0 {
			continue
		}
		entries, err := e.Ledger.Post(ctx, t.entry(id, l, lines, t.posting))
		if err != nil {
			return fmt.Errorf("assets: post the transfer of asset %s in ledger %s: %w", t.asset.Number, l.ledger.Code, err)
		}
		l.entry = &entries[0]
	}
	return nil
}

// lines builds an entry's lines, debit positive in local currency with
// their group amounts, leaving out a line with neither amount.
type lines []PostingLine

func (ls *lines) add(account uuid.UUID, amount, group money.Minor, costCenter, profitCenter *uuid.UUID) {
	if amount == 0 && group == 0 {
		return
	}
	*ls = append(*ls, PostingLine{AccountID: account, Amount: amount, LC: &amount, GC: &group, CostCenterID: costCenter, ProfitCenterID: profitCenter})
}

// catchUpLines are the catch-up's lines on the asset's centers before the
// transfer (ADR-0057 §8): Dr depreciation on its cost and profit center /
// Cr accumulated depreciation on its profit center, the other way round
// when negative.
func (t *transferring) catchUpLines(l *transferLedger) lines {
	var out lines
	out.add(t.accounts.depreciation, l.catchUp, l.catchUpGC, t.asset.CostCenterID, t.asset.ProfitCenterID)
	out.add(t.accounts.accumulated, -l.catchUp, -l.catchUpGC, nil, t.asset.ProfitCenterID)
	return out
}

// mainLines are the main entry's lines (ADR-0057 §8): the catch-up when it
// is not apart, then a center transfer's balances moved from the previous
// profit center to the new one on the same accounts, or an asset
// transfer's cost and accumulated depreciation taken off the asset and
// brought onto the receiver, each side on its own asset's profit center,
// at their local and group amounts.
func (t *transferring) mainLines(l *transferLedger) lines {
	var out lines
	if !l.apart {
		out = t.catchUpLines(l)
	}
	from, to, toPC := t.accounts, t.accounts, t.profit
	if t.in.Kind == TransferAsset {
		to, toPC = t.toAccounts, t.to.ProfitCenterID
	}
	out.add(from.cost, -l.cost, -l.costGC, nil, t.asset.ProfitCenterID)
	out.add(to.cost, l.cost, l.costGC, nil, toPC)
	out.add(from.accumulated, -l.accumulated, -l.accumulatedGC, nil, t.asset.ProfitCenterID)
	out.add(to.accumulated, l.accumulated, l.accumulatedGC, nil, toPC)
	return out
}

// entry is one of the transfer's entries in a ledger: in local currency,
// to that ledger alone (Posting.LedgerID, ADR-0057 §4), document type AA,
// the transfer as its source document, dated on.
func (t *transferring) entry(id uuid.UUID, l *transferLedger, ls lines, on time.Time) Posting {
	header := t.in.Text
	if header == nil {
		h := "Transfer of asset " + t.asset.Number + " " + t.asset.Description
		header = &h
	}
	for i := range ls {
		ls[i].Text = header
	}
	return Posting{LegalEntityID: t.company.ID, LedgerID: &l.ledger.ID, DocumentType: documentTypeAA, PostingDate: on, DocumentDate: on,
		Currency: t.company.Currency, HeaderText: header, DocumentID: id, Lines: ls}
}
