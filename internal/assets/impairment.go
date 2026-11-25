package assets

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Impairment kinds (ADR-0057 As built, impairment; IAS 36.59, 36.114).
const (
	ImpairmentLoss     = "loss"
	ImpairmentReversal = "reversal"
)

// NewImpairment is an impairment loss on an asset, or the reversal of
// earlier ones, to post in one ledger (nil: the leading ledger): the
// amount, above zero, in the currency given, which is to be the
// company's. The dates are nil for their defaults.
type NewImpairment struct {
	LedgerID               *uuid.UUID
	Kind                   string
	Amount                 money.Minor
	Currency               string
	ValueDate, PostingDate *time.Time
	Text                   *string
}

// impairing is an impairment gathered and checked under the asset's lock
// before anything is posted: the asset, its company, the ledger and the
// asset's terms there, the accounts its entry posts to, the dates and
// posting period, the carrying amounts it is measured against and its
// group amount.
type impairing struct {
	in                   NewImpairment
	asset                Asset
	company              Company
	ledger               LedgerRef
	terms                Terms
	accumulated, account uuid.UUID // the other: the loss's expense, or the reversal's revenue
	group                string
	value, posting       time.Time
	period               Period
	carrying, unimpaired money.Minor
	amountGC             money.Minor
}

// impairAsset posts an impairment loss on an asset, or the reversal of
// earlier ones, in one ledger (ADR-0057 As built, impairment): checked
// under the asset's lock against its If-Match, the carrying amounts of the
// schedule at the end of the value date's period, one ledger-specific
// entry, the asset's movement, its next seq and its year totals, audited
// and published.
func (e *Endpoints) impairAsset(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, ifMatch func(int32) error, in NewImpairment) (Impairment, error) {
	r, err := e.prepareImpairment(ctx, tx, id, ifMatch, in)
	if err != nil {
		return Impairment{}, err
	}
	doc := uuid.Must(uuid.NewV7())
	entries, err := e.Ledger.Post(ctx, r.entry(doc))
	if err != nil {
		return Impairment{}, fmt.Errorf("assets: post impairment of asset %s in ledger %s: %w", r.asset.Number, r.ledger.Code, err)
	}
	if err := writeImpairment(ctx, tx, p, doc, r, entries[0]); err != nil { // a posting to one ledger has one entry
		return Impairment{}, err
	}
	out, err := readImpairment(ctx, tx, doc)
	if err != nil {
		return Impairment{}, err
	}
	return out, e.recordImpairment(ctx, tx, out)
}

// prepareImpairment reads and checks what the impairment posts: the asset
// locked, at the caller's version, active; its ledger, accounts and dates;
// and the amount within its limit.
func (e *Endpoints) prepareImpairment(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error, in NewImpairment) (*impairing, error) {
	asset, err := readAsset(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if err := ifMatch(asset.Version); err != nil {
		return nil, err
	}
	if err := takesDocuments(asset); err != nil {
		return nil, err
	}
	if asset.Status == StatusPlanned {
		return nil, problem.Unprocessable("assets.asset_not_capitalised", fmt.Sprintf("Asset %s is planned: it holds no acquisition, so it has no "+
			"carrying amount to impair.", asset.Number))
	}
	r := &impairing{in: in, asset: asset}
	if err := e.impairmentLedger(ctx, r); err != nil {
		return nil, err
	}
	doc, err := e.impairmentAccounts(ctx, r)
	if err != nil {
		return nil, err
	}
	if err := e.impairmentDates(ctx, doc, r); err != nil {
		return nil, err
	}
	if err := laterValues(ctx, tx, r.asset, r.period, false); err != nil {
		return nil, err
	}
	if err := r.carryingAmounts(ctx, tx); err != nil {
		return nil, err
	}
	if err := r.withinLimit(ctx, tx); err != nil {
		return nil, err
	}
	gc, err := e.Ledger.GroupAmounts(ctx, r.company.Currency, r.group, r.posting, []money.Minor{in.Amount})
	if err != nil {
		return nil, fmt.Errorf("assets: group amount of the impairment of asset %s: %w", asset.Number, err)
	}
	r.amountGC = gc[0]
	return r, nil
}

// impairmentLedger reads the asset's company (the amount in its currency,
// 422 request.validation_failed otherwise), the ledger (422
// ledger.ledger_not_kept for one the company does not keep) and the
// asset's terms there (422 assets.ledger_terms_missing).
func (e *Endpoints) impairmentLedger(ctx context.Context, r *impairing) error {
	co, err := e.company(ctx, r.asset.LegalEntityID)
	if err != nil {
		return err
	}
	r.company = co
	if r.in.Currency != co.Currency {
		return validation("amount.currency", "the company's currency, "+co.Currency)
	}
	l, ok, err := e.ledgerOf(ctx, co.ID, r.in.LedgerID)
	if err != nil {
		return err
	}
	if !ok {
		return problem.InvalidDetail("ledger.ledger_not_kept", fmt.Sprintf("Company %s does not keep this ledger.", co.Code), "ledger_id",
			"a ledger the company keeps")
	}
	r.ledger = l
	if r.terms, ok = termsIn(r.asset, l.ID); !ok {
		return problem.InvalidDetail("assets.ledger_terms_missing", fmt.Sprintf("Asset %s has no depreciation terms in ledger %s; give them "+
			"(PATCH /v1/assets/{id}) before it is impaired there.", r.asset.Number, l.Code), "ledger_id", "a ledger the asset has terms in")
	}
	return nil
}

// impairmentAccounts reads the accounts of the asset's class in its
// company the entry posts to: the accumulated depreciation, and the
// impairment loss for a loss or the impairment reversal for a reversal
// (422 assets.impairment_account_missing when the company names none);
// and the group currency. It returns the active document.
func (e *Endpoints) impairmentAccounts(ctx context.Context, r *impairing) (*config.Config, error) {
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("assets: configuration: %w", err)
	}
	_, a, err := classOf(doc, r.company, r.asset.Class)
	if err != nil {
		return nil, err
	}
	field, number := "impairment_loss", a.ImpairmentLoss
	if r.in.Kind == ImpairmentReversal {
		field, number = "impairment_reversal", a.ImpairmentReversal
	}
	if number == "" {
		return nil, problem.InvalidDetail("assets.impairment_account_missing", fmt.Sprintf("Company %s names no %s account for asset class %s "+
			"(companies[].asset_accounts[].%s); add one before its assets are impaired.", r.company.Code, field, r.asset.Class, field), "kind",
			"a kind the class has an account for")
	}
	// Activation sets up every account the asset accounts name in the
	// company (ADR-0057 §3), so both are found.
	ids, err := e.Ledger.Accounts(ctx, r.company.ID, []string{a.AccumulatedDepreciation, number})
	if err != nil {
		return nil, fmt.Errorf("assets: accounts of class %s in %s: %w", r.asset.Class, r.company.Code, err)
	}
	r.accumulated, r.account = ids[a.AccumulatedDepreciation], ids[number]
	if r.group, err = e.Companies.GroupCurrency(ctx); err != nil {
		return nil, fmt.Errorf("assets: group currency: %w", err)
	}
	return doc, nil
}

// impairmentDates sets the posting date, the value date (the posting
// date by default) and the posting period, as a retirement's
// (postingDates): valued in the period on or before the posting date and
// not before the capitalisation date.
func (e *Endpoints) impairmentDates(ctx context.Context, doc *config.Config, r *impairing) error {
	var err error
	if r.posting, r.period, _, err = e.postingDates(ctx, doc, r.company, r.in.PostingDate); err != nil {
		return err
	}
	r.value = r.posting
	if r.in.ValueDate != nil {
		r.value = *r.in.ValueDate
	}
	if err := valuedInPeriod("impairment", r.value, r.posting, r.period); err != nil {
		return err
	}
	return notBeforeCapitalisation(r.asset, r.value)
}

// carryingAmounts walks the asset's schedule in the ledger through the
// posting period V (ADR-0057 §7): its net book value at V's end, the
// impairments posted so far applied (carrying), and without any of them
// (unimpaired: the carrying amount the asset would have had, net of the
// depreciation it would have taken, IAS 36.117). Nothing on the asset is
// valued after V (laterValues), so V's end is where the impairment takes
// effect.
func (r *impairing) carryingAmounts(ctx context.Context, tx pgx.Tx) error {
	d := &depreciable{asset: r.asset.ID, terms: r.terms, capitalised: r.asset.CapitalisedOn}
	if err := readMoves(ctx, tx, r.ledger.ID, map[uuid.UUID]*depreciable{r.asset.ID: d}); err != nil {
		return err
	}
	v := MonthOf(r.period.Start)
	var err error
	if r.carrying, err = bookValueAt(d, v); err != nil {
		return err
	}
	var unimpaired []Move
	for _, m := range d.moves {
		if !m.Impairment {
			unimpaired = append(unimpaired, m)
		}
	}
	d.moves = unimpaired
	r.unimpaired, err = bookValueAt(d, v)
	return err
}

// bookValueAt is the asset's planned net book value at the end of the
// period: zero when its schedule has no step by then.
func bookValueAt(d *depreciable, through Month) (money.Minor, error) {
	steps, err := d.schedule(through)
	if err != nil || len(steps) == 0 {
		return 0, err
	}
	return steps[len(steps)-1].BookValue, nil
}

// withinLimit refuses an amount beyond what the kind may take (IAS
// 36.60, 36.117): a loss above the carrying amount (422
// assets.impairment_exceeds_carrying_amount); a reversal above the
// carrying amount without impairment less the carrying amount (422
// assets.reversal_exceeds_impairment), or once a retirement or asset
// transfer took a share after the asset's first impairment in the ledger
// (409 assets.impairment_reversal_after_retirement): its movement
// recorded the accumulated depreciation it took, impairment with it, and
// that part is not known apart, so the carrying amount without
// impairment cannot be told.
func (r *impairing) withinLimit(ctx context.Context, tx pgx.Tx) error {
	n, c := r.asset.Number, r.ledger.Code
	if r.in.Kind == ImpairmentLoss {
		if r.in.Amount > r.carrying {
			return problem.InvalidDetail("assets.impairment_exceeds_carrying_amount", fmt.Sprintf("Asset %s's carrying amount in ledger %s at the "+
				"end of %d/%02d is %d minor units; an impairment loss takes at most that.", n, c, r.period.FiscalYear, r.period.Number, int64(r.carrying)),
				"amount", "at most the carrying amount")
		}
		return nil
	}
	var shared bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM assets.movement m WHERE m.asset_id = $1 AND m.ledger_id = $2
			AND m.kind IN ('retirement', 'transfer_out') AND m.reverses_id IS NULL
			AND NOT EXISTS (SELECT FROM assets.movement x WHERE x.tenant_id = m.tenant_id AND x.reverses_id = m.id)
			AND m.seq > (SELECT min(i.seq) FROM assets.movement i WHERE i.asset_id = $1 AND i.ledger_id = $2 AND i.kind = 'impairment'))`,
		r.asset.ID, r.ledger.ID).Scan(&shared); err != nil {
		return fmt.Errorf("assets: shares taken of asset %s after its impairment: %w", n, err)
	}
	if shared {
		return problem.Conflict("assets.impairment_reversal_after_retirement", fmt.Sprintf("A retirement or asset transfer took a share of asset %s "+
			"in ledger %s after it was impaired there, so the carrying amount it would have had without the impairment is not known; "+
			"its impairment is not reversed.", n, c))
	}
	if limit := r.unimpaired - r.carrying; r.in.Amount > limit {
		return problem.InvalidDetail("assets.reversal_exceeds_impairment", fmt.Sprintf("Asset %s's carrying amount in ledger %s at the end of "+
			"%d/%02d is %d minor units, and would be %d without its impairments; a reversal restores at most the %d between them.", n, c,
			r.period.FiscalYear, r.period.Number, int64(r.carrying), int64(r.unimpaired), int64(max(0, limit))), "amount",
			"at most what the impairments took, net of depreciation")
	}
	return nil
}

// lines are the entry's lines, debit positive in local currency with
// their group amounts (ADR-0057 §8's centers): a loss Dr impairment loss
// on the asset's cost and profit center / Cr accumulated depreciation on
// its profit center; a reversal Dr accumulated depreciation / Cr
// impairment reversal, both on its profit center (a revenue takes no
// cost center, ADR-0050 §4).
func (r *impairing) lines() []PostingLine {
	amount, gc := r.in.Amount, r.amountGC
	line := func(account uuid.UUID, lc, gc money.Minor, cc *uuid.UUID) PostingLine {
		return PostingLine{AccountID: account, Amount: lc, LC: &lc, GC: &gc, CostCenterID: cc, ProfitCenterID: r.asset.ProfitCenterID}
	}
	if r.in.Kind == ImpairmentLoss {
		return []PostingLine{line(r.account, amount, gc, r.asset.CostCenterID), line(r.accumulated, -amount, -gc, nil)}
	}
	return []PostingLine{line(r.accumulated, amount, gc, nil), line(r.account, -amount, -gc, nil)}
}

// entry is the impairment's entry: in local currency, to its ledger alone
// (Posting.LedgerID, ADR-0057 §4), document type AA, the impairment as its
// source document.
func (r *impairing) entry(id uuid.UUID) Posting {
	header := r.in.Text
	if header == nil {
		h := "Impairment loss on asset " + r.asset.Number + " " + r.asset.Description
		if r.in.Kind == ImpairmentReversal {
			h = "Impairment reversal on asset " + r.asset.Number + " " + r.asset.Description
		}
		header = &h
	}
	lines := r.lines()
	for i := range lines {
		lines[i].Text = header
	}
	return Posting{LegalEntityID: r.company.ID, LedgerID: &r.ledger.ID, DocumentType: documentTypeAA, PostingDate: r.posting, DocumentDate: r.posting,
		Currency: r.company.Currency, HeaderText: header, DocumentID: id, Lines: lines}
}
