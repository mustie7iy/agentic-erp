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

// Retirement kinds and statuses (ADR-0057 §5, §8).
const (
	RetirementSale      = "sale"
	RetirementScrapping = "scrapping"
	RetirementPosted    = "posted"
	RetirementReversed  = "reversed"
)

// hundredPercent is 100 % in the ten-thousandths of a percent a share is
// given in.
const hundredPercent = 1_000_000

// NewRetirement is a retirement to post (ADR-0057 §8): a sale with its
// proceeds, or a scrapping; the share of cost retired as a percentage
// (Percent, in ten-thousandths of a percent) or as a cost (Cost), neither
// for all of it; the proceeds and the cost in the currencies given, which
// are to be the company's. The dates are nil for their defaults.
type NewRetirement struct {
	AssetID                        uuid.UUID
	Kind                           string
	Proceeds                       money.Minor
	ProceedsCurrency, CostCurrency string
	ValueDate, PostingDate         *time.Time
	Percent                        *int64
	Cost                           *money.Minor
	Text                           *string
}

// retiring is a retirement gathered and checked under the asset's lock
// before anything is posted: the asset, its company and class accounts,
// the dates, the posting period V and the period before it, and what it
// takes per ledger the company keeps.
type retiring struct {
	in             NewRetirement
	asset          Asset
	company        Company
	accounts       retirementAccounts
	group          string
	value, posting time.Time
	period         Period
	before         postingPeriod
	proceedsGC     money.Minor
	ledgers        []*retiredLedger
}

// retirementAccounts are the class's accounts in the company a retirement
// posts to.
type retirementAccounts struct {
	cost, accumulated, depreciation, gain, loss, saleClearing uuid.UUID
}

// retiredLedger is what a retirement takes in one ledger: the asset's
// terms and values there, the catch-up (the depreciation it posts, an
// expense debit positive) in local and group currency, and the share
// taken of the balances after it (cost positive, accumulated depreciation
// negative as the credit it is, residual value), with their group
// amounts, and the ledger's entry once posted.
type retiredLedger struct {
	ledger                                   LedgerRef
	terms                                    Terms
	values                                   Values
	catchUp, catchUpGC                       money.Minor
	share                                    *big.Rat
	cost, costGC, accumulated, accumulatedGC money.Minor
	residual                                 money.Minor
	entry                                    *PostedEntry
}

// postRetirement retires a share of an asset (ADR-0057 §8): checked under
// the asset's lock, per ledger the company keeps (the leading first, then
// by code) its depreciation caught up through the period before the
// posting period and the share taken off with the proceeds and the gain
// or loss, posted as one ledger-specific entry, with the asset's
// movements, its booked_through raised and its status derived again,
// audited and published.
func (e *Endpoints) postRetirement(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in NewRetirement) (Retirement, error) {
	r, err := e.prepareRetirement(ctx, tx, in)
	if err != nil {
		return Retirement{}, err
	}
	id, seq := uuid.Must(uuid.NewV7()), r.asset.LastSeq+1
	for _, l := range r.ledgers {
		lines := r.lines(l)
		if len(lines) == 0 { // every amount zero: movements without an entry (ADR-0057 §5)
			continue
		}
		entries, err := e.Ledger.Post(ctx, r.entry(id, l, lines))
		if err != nil {
			return Retirement{}, fmt.Errorf("assets: post retirement of asset %s in ledger %s: %w", r.asset.Number, l.ledger.Code, err)
		}
		l.entry = &entries[0] // a posting to one ledger has one entry
	}
	if err := writeRetirement(ctx, tx, p, id, seq, r); err != nil {
		return Retirement{}, err
	}
	out, err := readRetirement(ctx, tx, id)
	if err != nil {
		return Retirement{}, err
	}
	return out, recordRetirement(ctx, tx, out, r.asset.Status)
}

// prepareRetirement reads and checks what the retirement takes.
func (e *Endpoints) prepareRetirement(ctx context.Context, tx pgx.Tx, in NewRetirement) (*retiring, error) {
	asset, err := readAsset(ctx, tx, in.AssetID, true)
	if err != nil {
		return nil, err
	}
	if err := takesDocuments(asset); err != nil {
		return nil, err
	}
	if asset.Status == StatusPlanned {
		return nil, problem.Unprocessable("assets.asset_not_capitalised", fmt.Sprintf("Asset %s is planned: it holds no acquisition, so there is "+
			"nothing to retire.", asset.Number))
	}
	r := &retiring{in: in, asset: asset}
	doc, err := e.retirementAccountsOf(ctx, r)
	if err != nil {
		return nil, err
	}
	if err := e.retirementDates(ctx, doc, r); err != nil {
		return nil, err
	}
	if err := laterValueDate(ctx, tx, r.asset, r.period); err != nil {
		return nil, err
	}
	if err := e.retiredLedgers(ctx, tx, r); err != nil {
		return nil, err
	}
	for _, l := range r.ledgers {
		if err := l.take(in, r.asset.Number); err != nil {
			return nil, err
		}
	}
	return r, e.retirementGroupAmounts(ctx, r)
}

// retirementAccountsOf reads the asset's company and its class's accounts
// there, and returns the active document.
func (e *Endpoints) retirementAccountsOf(ctx context.Context, r *retiring) (*config.Config, error) {
	co, err := e.company(ctx, r.asset.LegalEntityID)
	if err != nil {
		return nil, err
	}
	r.company = co
	for _, c := range [][2]string{{"proceeds.currency", r.in.ProceedsCurrency}, {"share.cost.currency", r.in.CostCurrency}} {
		if c[1] != "" && c[1] != co.Currency {
			return nil, validation(c[0], "the company's currency, "+co.Currency)
		}
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("assets: configuration: %w", err)
	}
	_, a, err := classOf(doc, co, r.asset.Class)
	if err != nil {
		return nil, err
	}
	// Activation sets up every account the asset accounts name in the
	// company (ADR-0057 §3), so each is found.
	ids, err := e.Ledger.Accounts(ctx, co.ID, []string{a.AcquisitionCost, a.AccumulatedDepreciation, a.Depreciation, a.Gain, a.Loss, a.SaleClearing})
	if err != nil {
		return nil, fmt.Errorf("assets: accounts of class %s in %s: %w", r.asset.Class, co.Code, err)
	}
	r.accounts = retirementAccounts{cost: ids[a.AcquisitionCost], accumulated: ids[a.AccumulatedDepreciation], depreciation: ids[a.Depreciation],
		gain: ids[a.Gain], loss: ids[a.Loss], saleClearing: ids[a.SaleClearing]}
	if r.group, err = e.Companies.GroupCurrency(ctx); err != nil {
		return nil, fmt.Errorf("assets: group currency: %w", err)
	}
	return doc, nil
}

// retirementDates sets the posting date (the company's date today by
// default), the value date (the posting date by default) and the posting
// period V with the regular period before it (the previous fiscal year's
// last for period 1: twelve regular periods a year, ADR-0057 as built
// T57e). The value date lies in V on or before the posting date (422
// assets.value_date_outside_period, decided by default 13) and not before
// the asset's capitalisation date (422
// assets.value_date_before_capitalisation).
func (e *Endpoints) retirementDates(ctx context.Context, doc *config.Config, r *retiring) error {
	if r.in.PostingDate != nil {
		r.posting = *r.in.PostingDate
	} else {
		today, err := doc.CompanyDay(r.company.Code, clock.Now(e.Now))
		if err != nil {
			return fmt.Errorf("assets: %w", err)
		}
		r.posting = today.Date
	}
	r.value = r.posting
	if r.in.ValueDate != nil {
		r.value = *r.in.ValueDate
	}
	per, err := e.Ledger.Period(ctx, r.company.ID, r.posting)
	if err != nil {
		return fmt.Errorf("assets: period of %s: %w", r.posting.Format(time.DateOnly), err)
	}
	r.period, r.before = per, postingPeriod{per.FiscalYear, per.Number - 1}
	if per.Number == 1 {
		r.before = postingPeriod{per.FiscalYear - 1, 12}
	}
	if r.value.Before(per.Start) || r.value.After(r.posting) {
		return invalid("assets.value_date_outside_period", fmt.Sprintf("A retirement is valued in its posting period, %s to its posting date %s; "+
			"it is not backdated.", per.Start.Format(time.DateOnly), r.posting.Format(time.DateOnly)), "value_date",
			"a date from "+per.Start.Format(time.DateOnly)+" to "+r.posting.Format(time.DateOnly))
	}
	if on := r.asset.CapitalisedOn; on != nil && r.value.Before(*on) { // an active asset has its date (§5)
		return invalid("assets.value_date_before_capitalisation", fmt.Sprintf("Asset %s was capitalised on %s; nothing is retired before.",
			r.asset.Number, on.Format(time.DateOnly)), "value_date", "on or after "+on.Format(time.DateOnly))
	}
	return nil
}

// laterValueDate refuses a retirement or transfer while the asset has a
// movement or an assignment valued after its period, or is booked
// through a later period in a ledger (409 assets.later_value_date,
// ADR-0057 §6): a run of a later period, an acquisition valued later, a
// transfer from a later date. A reversed movement and its reversal cancel
// out (§7) and do not count.
func laterValueDate(ctx context.Context, tx pgx.Tx, a Asset, v Period) error {
	var later bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM assets.movement m WHERE m.asset_id = $1 AND m.value_date > $2 AND m.reverses_id IS NULL
				AND NOT EXISTS (SELECT FROM assets.movement r WHERE r.tenant_id = m.tenant_id AND r.reverses_id = m.id))
			OR EXISTS (SELECT FROM assets.asset_assignment WHERE asset_id = $1 AND valid_from > $2)
			OR EXISTS (SELECT FROM assets.asset_terms WHERE asset_id = $1 AND (booked_through_year, booked_through_period) > ($3::smallint, $4::smallint))`,
		a.ID, v.End, v.FiscalYear, v.Number).Scan(&later)
	if err != nil {
		return fmt.Errorf("assets: later values of asset %s: %w", a.Number, err)
	}
	if later {
		return problem.Conflict("assets.later_value_date", fmt.Sprintf("Asset %s has a movement or center assignment valued after %d/%02d, or "+
			"depreciation booked through a later period; a retirement or transfer is valued in its posting period and not before them.",
			a.Number, v.FiscalYear, v.Number))
	}
	return nil
}

// retiredLedgers reads, per ledger the company keeps, the asset's terms
// (422 assets.ledger_terms_missing in a ledger it has none in: give them
// by PATCH first) and values there, and its catch-up: planned(V-1) less
// booked (ADR-0057 §7), negative when V's run already posted.
func (e *Endpoints) retiredLedgers(ctx context.Context, tx pgx.Tx, r *retiring) error {
	kept, err := e.Ledger.Ledgers(ctx, &r.company.ID)
	if err != nil {
		return fmt.Errorf("assets: ledgers of %s: %w", r.company.Code, err)
	}
	for _, l := range kept {
		t, ok := termsIn(r.asset, l.ID)
		if !ok {
			return invalid("assets.ledger_terms_missing", fmt.Sprintf("Asset %s has no depreciation terms in ledger %s, which its company keeps; "+
				"give them (PATCH /v1/assets/{id}) before it is retired.", r.asset.Number, l.Code), "asset_id", "an asset with terms in every ledger")
		}
		d := &depreciable{asset: r.asset.ID, terms: t, capitalised: r.asset.CapitalisedOn, bookedIn: map[postingPeriod]money.Minor{}}
		if err := readMovements(ctx, tx, l.ID, map[uuid.UUID]*depreciable{r.asset.ID: d}); err != nil {
			return err
		}
		_, planned, err := d.planned(MonthOf(r.period.Start) - 1)
		if err != nil {
			return err
		}
		r.ledgers = append(r.ledgers, &retiredLedger{ledger: l, terms: t, values: r.asset.Values[l.ID], catchUp: planned - d.booked})
	}
	return nil
}

// termsIn is the asset's terms in the ledger.
func termsIn(a Asset, ledger uuid.UUID) (Terms, bool) {
	for _, t := range a.Terms {
		if t.LedgerID == ledger {
			return t, true
		}
	}
	return Terms{}, false
}

// take sets the share of the ledger's balances after the catch-up the
// retirement takes (ADR-0057 §8): a share given as cost is that cost over
// the base cost (above it 422 assets.share_exceeds), one given as a
// percentage that fraction; the cost, accumulated depreciation and
// residual value taken are the share of each rounded half away from zero,
// all of them for a full share. With no base cost only a full share is
// accepted (422 assets.share_of_zero_cost).
func (l *retiredLedger) take(in NewRetirement, number string) error {
	cost, accumulated, residual := l.values.Cost, l.values.Accumulated-l.catchUp, l.terms.Residual+l.values.Residual
	num, den := int64(1), int64(1) // all of it
	switch {
	case in.Cost != nil:
		num, den = int64(*in.Cost), int64(cost)
	case in.Percent != nil:
		num, den = *in.Percent, hundredPercent
	}
	if cost == 0 && num != den {
		return invalid("assets.share_of_zero_cost", fmt.Sprintf("Asset %s has no cost left in ledger %s; only a full retirement takes what is "+
			"left of it.", number, l.ledger.Code), "share", "all of it (no share, or 100 percent)")
	}
	if num > den {
		return invalid("assets.share_exceeds", fmt.Sprintf("Asset %s's cost in ledger %s is %d minor units; a retirement takes at most that.",
			number, l.ledger.Code, int64(cost)), "share.cost", "at most the asset's cost")
	}
	l.share = big.NewRat(num, den)
	if num == den {
		l.cost, l.accumulated, l.residual = cost, accumulated, residual
		return nil
	}
	l.cost = proportion(cost, money.Minor(num), money.Minor(den))
	l.accumulated = proportion(accumulated, money.Minor(num), money.Minor(den))
	l.residual = proportion(residual, money.Minor(num), money.Minor(den))
	return nil
}

// retirementGroupAmounts translates the proceeds and each ledger's
// catch-up at the posting date's rate (ADR-0057 §7, §8), and sets the
// group amounts of the cost and accumulated depreciation taken (groupTaken).
func (e *Endpoints) retirementGroupAmounts(ctx context.Context, r *retiring) error {
	local := []money.Minor{r.in.Proceeds}
	for _, l := range r.ledgers {
		local = append(local, l.catchUp, l.cost, l.accumulated)
	}
	gc, err := e.Ledger.GroupAmounts(ctx, r.company.Currency, r.group, r.posting, local)
	if err != nil {
		return fmt.Errorf("assets: group amounts of the retirement of asset %s: %w", r.asset.Number, err)
	}
	r.proceedsGC = gc[0]
	for i, l := range r.ledgers {
		at := gc[1+3*i:]
		l.catchUpGC = at[0]
		l.costGC = groupTaken(l.cost, l.values.Cost, l.values.CostGC, at[1])
		l.accumulatedGC = groupTaken(l.accumulated, l.values.Accumulated-l.catchUp, l.values.AccumulatedGC-l.catchUpGC, at[2])
	}
	return nil
}

// groupTaken is the group amount of taken, a share of a booked balance
// with its booked group amount, by the first of ADR-0057 §8's rules that
// applies (decided by default 18): all of the booked group amount when
// nothing is left locally, so the account clears in group currency; the
// local amount translated at the posting date's rate when the booked
// group amount has the other sign (a zero booked local amount leaves
// nothing after any share, so the first rule takes it); otherwise taken
// times booked group over booked local, rounded half away from zero.
func groupTaken(taken, booked, bookedGC, translated money.Minor) money.Minor {
	switch {
	case booked == taken:
		return bookedGC
	case bookedGC != 0 && (bookedGC > 0) != (booked > 0):
		return translated
	}
	return proportion(bookedGC, taken, booked)
}

// lines are the ledger's entry lines (ADR-0057 §8), debit positive in
// local currency with their group amounts: the catch-up (Dr depreciation
// / Cr accumulated depreciation, the other way round when negative), Dr
// accumulated depreciation for what is taken, Cr acquisition cost for the
// cost taken, Dr sale clearing for the proceeds, and the gain (Cr gain)
// or loss (Dr loss) taking the difference in both currencies, decided by
// its local amount (by its group amount when that is zero). A line with
// neither amount is left out; expense lines take the asset's cost and
// profit center, the others its profit center (§8).
func (r *retiring) lines(l *retiredLedger) []PostingLine {
	cc, pc := r.asset.CostCenterID, r.asset.ProfitCenterID
	var out []PostingLine
	var lc, gc money.Minor
	add := func(account uuid.UUID, amount, group money.Minor, costCenter *uuid.UUID) {
		if amount == 0 && group == 0 {
			return
		}
		lc, gc = lc+amount, gc+group
		out = append(out, PostingLine{AccountID: account, Amount: amount, LC: &amount, GC: &group, CostCenterID: costCenter, ProfitCenterID: pc})
	}
	add(r.accounts.depreciation, l.catchUp, l.catchUpGC, cc)
	add(r.accounts.accumulated, -l.catchUp, -l.catchUpGC, nil)
	add(r.accounts.accumulated, -l.accumulated, -l.accumulatedGC, nil)
	add(r.accounts.cost, -l.cost, -l.costGC, nil)
	add(r.accounts.saleClearing, r.in.Proceeds, r.proceedsGC, nil)
	if -lc > 0 || (lc == 0 && -gc > 0) {
		add(r.accounts.loss, -lc, -gc, cc)
	} else {
		add(r.accounts.gain, -lc, -gc, nil)
	}
	return out
}

// entry is the ledger's entry of the retirement: in local currency, to
// that ledger alone (Posting.LedgerID, ADR-0057 §4), document type AA, the
// retirement as its source document.
func (r *retiring) entry(id uuid.UUID, l *retiredLedger, lines []PostingLine) Posting {
	header := r.in.Text
	if header == nil {
		h := "Retirement of asset " + r.asset.Number + " " + r.asset.Description
		header = &h
	}
	for i := range lines {
		lines[i].Text = header
	}
	return Posting{LegalEntityID: r.company.ID, LedgerID: &l.ledger.ID, DocumentType: documentTypeAA, PostingDate: r.posting, DocumentDate: r.posting,
		Currency: r.company.Currency, HeaderText: header, DocumentID: id, Lines: lines}
}
