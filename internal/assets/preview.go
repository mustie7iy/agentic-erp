package assets

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// previewKey is an asset's place in the preview's order.
type previewKey struct {
	Number string    `json:"n"`
	ID     uuid.UUID `json:"i"`
}

// candidate is an asset a run of the period selects (ADR-0057 §7), with
// whether it comes after the page token.
type candidate struct {
	depreciable
	number, description, class string
	costCenter, profitCenter   *uuid.UUID
	after                      bool
	plannedTotal, difference   money.Minor
	differenceGC               money.Minor // a run's, in group currency (T57f)
}

// selectDepreciable reads the company's assets a run of the period in the
// ledger selects (ADR-0057 §7, PlanDepreciationRun), by asset number:
// those with terms in the ledger that are active with their depreciation
// start on or before the period's last day, or whose accumulated
// depreciation in the ledger is not zero, passing over an asset booked
// through a later period or whose latest assignment is valid from after
// the period's last day; after marks those after the page token. With
// only, just those of the assets named are read: a run's batch re-checks
// its assets' selection under their locks (T57f). The latest assignment
// of each of the company's assets is read in one pass (DISTINCT ON)
// rather than looked up per asset, and with only the terms are read by their key too (the same
// assets, said of both tables): over 100,000 assets the lookups' estimate
// had the planner compile the query (JIT) and a batch's 500 assets
// scanned every asset's terms (T57f2: 860 ms to 430 ms, 86 ms to 9 ms).
func selectDepreciable(ctx context.Context, tx pgx.Tx, le, ledger uuid.UUID, p Period, after *previewKey, only []uuid.UUID) ([]*candidate, error) {
	var number *string
	var id *uuid.UUID
	if after != nil {
		number, id = &after.Number, &after.ID
	}
	rows, _ := tx.Query(ctx, `SELECT a.id, a.asset_number, a.description, a.class_code, a.capitalised_on, c.cost_center_id, c.profit_center_id,
			t.ledger_id, t.method, t.useful_life_months, trim_scale(t.declining_factor)::text, t.residual_value, t.depreciation_start,
			t.booked_through_year, t.booked_through_period, $6::text IS NULL OR (a.asset_number, a.id) > ($6, $7::uuid)
		FROM assets.asset a
		JOIN assets.asset_terms t ON t.tenant_id = a.tenant_id AND t.asset_id = a.id AND t.ledger_id = $2
		JOIN (SELECT DISTINCT ON (r.asset_id) r.tenant_id, r.asset_id, r.cost_center_id, r.profit_center_id, r.valid_from FROM assets.asset_assignment r
			WHERE ($8::uuid[] IS NULL AND r.asset_id IN (SELECT x.id FROM assets.asset x WHERE x.legal_entity_id = $1)) OR r.asset_id = ANY($8)
			ORDER BY r.asset_id, r.seq DESC) c ON c.tenant_id = a.tenant_id AND c.asset_id = a.id
		WHERE a.legal_entity_id = $1
			AND ((a.status = 'active' AND coalesce(t.depreciation_start, a.capitalised_on) <= $3)
				OR (SELECT coalesce(sum(m.accumulated), 0) FROM assets.movement m
					WHERE m.tenant_id = a.tenant_id AND m.asset_id = a.id AND m.ledger_id = t.ledger_id) <> 0)
			AND (t.booked_through_year IS NULL OR (t.booked_through_year, t.booked_through_period) <= ($4::smallint, $5::smallint))
			AND c.valid_from <= $3 AND ($8::uuid[] IS NULL OR (a.id = ANY($8) AND t.asset_id = ANY($8)))
		ORDER BY a.asset_number, a.id`, le, ledger, p.End, p.FiscalYear, p.Number, number, id, only)
	var out []*candidate
	var c candidate
	_, err := pgx.ForEachRow(rows, []any{&c.asset, &c.number, &c.description, &c.class, &c.capitalised, &c.costCenter, &c.profitCenter,
		&c.terms.LedgerID, &c.terms.Method, &c.terms.UsefulLifeMonths, &c.terms.DecliningFactor, &c.terms.Residual, &c.terms.DepreciationStart,
		&c.terms.BookedYear, &c.terms.BookedPeriod, &c.after}, func() error {
		row := c
		out = append(out, &row)
		c = candidate{} // the next row's pointers are scanned into a new value
		return nil
	})
	return out, errs.Wrapf(err, "assets: assets to depreciate of %s in %d/%d", le, p.FiscalYear, p.Number)
}

// previewRun is a preview's company, ledger, period and the assets it
// selects with their differences.
type previewRun struct {
	co     Company
	ledger LedgerRef
	period Period
	assets []*candidate
}

// PreviewDepreciationRun implements POST /v1/depreciation-runs/preview
// (ADR-0057 §7): what a run of the company, ledger and period would post,
// per asset and in total, without posting.
func (e *Endpoints) PreviewDepreciationRun(ctx context.Context, req *gen.DepreciationRunPreviewRequest,
	params gen.PreviewDepreciationRunParams) (gen.PreviewDepreciationRunRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	ledgerID := apitypes.Ptr[uuid.UUID](req.LedgerID)
	scope := fmt.Sprintf("previewDepreciationRun|%s|%s|%d|%d", req.LegalEntityID, ledgerID, req.FiscalYear, req.Period)
	var after *previewKey
	if params.PageToken.Set {
		after = &previewKey{}
		if err := e.Pages.Decode(scope, params.PageToken.Value, after); err != nil {
			return nil, err
		}
	}
	r, err := e.previewOf(ctx, tx, req, ledgerID, after)
	if err != nil {
		return nil, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("assets: configuration: %w", err)
	}
	accounts, err := e.depreciationAccounts(ctx, doc, r)
	if err != nil {
		return nil, err
	}
	out, err := e.previewAPI(ctx, tx, doc, r, accounts, scope, size)
	if err != nil {
		return nil, err
	}
	return &gen.DepreciationRunPreviewHeaders{Response: *out}, nil
}

// previewOf reads the run's target (runTarget) and the assets a run
// selects with their planned, booked and difference amounts.
func (e *Endpoints) previewOf(ctx context.Context, tx pgx.Tx, req *gen.DepreciationRunPreviewRequest, ledgerID *uuid.UUID, after *previewKey) (previewRun, error) {
	co, l, p, err := e.runTarget(ctx, req.LegalEntityID, ledgerID, req.FiscalYear, req.Period)
	if err != nil {
		return previewRun{}, err
	}
	cs, err := selectDepreciable(ctx, tx, co.ID, l.ID, p, after, nil)
	if err != nil {
		return previewRun{}, err
	}
	if err := differences(ctx, tx, l.ID, p, cs); err != nil {
		return previewRun{}, err
	}
	return previewRun{co: co, ledger: l, period: p, assets: cs}, nil
}

// differences reads the selected assets' movements in the ledger and sets
// each one's planned depreciation through the period and its difference
// from what is booked: what a run of the period posts (ADR-0057 §7).
func differences(ctx context.Context, tx pgx.Tx, ledger uuid.UUID, p Period, cs []*candidate) error {
	ds := make(map[uuid.UUID]*depreciable, len(cs))
	for _, c := range cs {
		ds[c.asset] = &c.depreciable
	}
	if err := readMovements(ctx, tx, ledger, ds, false); err != nil {
		return err
	}
	for _, c := range cs {
		var err error
		if _, c.plannedTotal, err = c.planned(MonthOf(p.Start)); err != nil {
			return err
		}
		c.difference = c.plannedTotal - c.booked
	}
	return nil
}

// classAccounts are a class's depreciation and accumulated depreciation
// accounts in a company.
type classAccounts struct{ depreciation, accumulated AccountRef }

// depreciationAccounts reads the depreciation and accumulated
// depreciation accounts of the selected assets' classes in the company
// (ADR-0057 §3), refusing a class the active document no longer gives the
// company (422 assets.class_unknown, assets.class_not_in_company).
func (e *Endpoints) depreciationAccounts(ctx context.Context, doc *config.Config, r previewRun) (map[string]classAccounts, error) {
	accounts := map[string]*config.AssetAccounts{}
	var numbers []string
	for _, c := range r.assets {
		if _, ok := accounts[c.class]; ok {
			continue
		}
		_, a, err := classOf(doc, r.co, c.class)
		if err != nil {
			return nil, err
		}
		accounts[c.class], numbers = a, append(numbers, a.Depreciation, a.AccumulatedDepreciation)
	}
	ids, err := e.Ledger.Accounts(ctx, r.co.ID, numbers)
	if err != nil {
		return nil, fmt.Errorf("assets: accounts of %s: %w", r.co.Code, err)
	}
	chart := doc.Company(r.co.Code) // classOf found the company's asset accounts
	ref := func(number string) AccountRef {
		return AccountRef{ID: ids[number], Number: number, Name: chart.Account(number).Name}
	}
	out := make(map[string]classAccounts, len(accounts))
	for class, a := range accounts {
		out[class] = classAccounts{depreciation: ref(a.Depreciation), accumulated: ref(a.AccumulatedDepreciation)}
	}
	return out, nil
}

// previewLine is a line a run would post: an account with its centers
// (uuid.Nil for none) and its amount, debit positive, in local and group
// currency.
type previewLine struct {
	account                  AccountRef
	costCenter, profitCenter uuid.UUID
	amount, gc               money.Minor
}

// previewSums are the totals of a preview's assets.
type previewSums struct {
	planned, booked, difference money.Minor
	lines                       []previewLine
}

// previewTotals sums the assets: their planned, booked and difference
// amounts and the lines a run would post (runLines).
func previewTotals(r previewRun, accounts map[string]classAccounts) previewSums {
	var out previewSums
	for _, c := range r.assets {
		out.planned, out.booked, out.difference = out.planned+c.plannedTotal, out.booked+c.booked, out.difference+c.difference
	}
	out.lines = runLines(r.assets, accounts)
	return out
}

// runLines are the lines a run posts for the assets' differences
// (ADR-0057 §7): the expense lines by depreciation account, cost center
// and profit center, then the credit lines by accumulated depreciation
// account and profit center, each side by account number and centers,
// a key's debits before its credits. Debits and credits are summed apart,
// so no line sums to zero locally while keeping a group amount (assets of
// both signs on one key, their translations rounded apart), which the
// ledger refuses; an asset without a difference adds none.
func runLines(assets []*candidate, accounts map[string]classAccounts) []previewLine {
	var expense, credit []previewLine
	type key struct {
		side                              *[]previewLine
		account, costCenter, profitCenter uuid.UUID
		negative                          bool
	}
	at := map[key]int{}
	add := func(side *[]previewLine, l previewLine) {
		k := key{side, l.account.ID, l.costCenter, l.profitCenter, l.amount < 0}
		if i, ok := at[k]; ok {
			(*side)[i].amount, (*side)[i].gc = (*side)[i].amount+l.amount, (*side)[i].gc+l.gc
			return
		}
		at[k] = len(*side)
		*side = append(*side, l)
	}
	for _, c := range assets {
		if c.difference == 0 {
			continue
		}
		a := accounts[c.class]
		pc := orNil(c.profitCenter)
		add(&expense, previewLine{account: a.depreciation, costCenter: orNil(c.costCenter), profitCenter: pc, amount: c.difference, gc: c.differenceGC})
		add(&credit, previewLine{account: a.accumulated, profitCenter: pc, amount: -c.difference, gc: -c.differenceGC})
	}
	for _, side := range [][]previewLine{expense, credit} {
		slices.SortFunc(side, func(a, b previewLine) int {
			return cmp.Or(cmp.Compare(a.account.Number, b.account.Number), cmp.Compare(a.costCenter.String(), b.costCenter.String()),
				cmp.Compare(a.profitCenter.String(), b.profitCenter.String()), cmp.Compare(b.amount, a.amount))
		})
	}
	return slices.Concat(expense, credit)
}

// orNil is the id, or uuid.Nil for none.
func orNil(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

// notNil is the id as an optional value: none for uuid.Nil.
func notNil(id uuid.UUID) gen.OptUUID {
	if id == uuid.Nil {
		return gen.OptUUID{}
	}
	return gen.NewOptUUID(id)
}

// previewAPI shows a page of the preview's assets, after the page token,
// with the totals of all of them and their centers named to a caller who
// may read cost centers.
func (e *Endpoints) previewAPI(ctx context.Context, tx pgx.Tx, doc *config.Config, r previewRun, accounts map[string]classAccounts, scope string,
	size int) (*gen.DepreciationRunPreview, error) {
	page := slices.DeleteFunc(slices.Clone(r.assets), func(c *candidate) bool { return !c.after })
	var next gen.OptString
	if len(page) > size {
		page = page[:size]
		last := page[size-1]
		tok, _ := e.Pages.Encode(scope, previewKey{Number: last.number, ID: last.asset}) // a string and a uuid always encode
		next = gen.NewOptString(tok)
	}
	sums := previewTotals(r, accounts)
	names, err := e.previewCenters(ctx, page, sums.lines)
	if err != nil {
		return nil, err
	}
	exp, err := currency.Exponent(ctx, tx, r.co.Currency, nil)
	if err != nil {
		return nil, fmt.Errorf("assets: currency %s: %w", r.co.Currency, err)
	}
	lc := func(m money.Minor) gen.Money { return apitypes.Money(m, r.co.Currency, exp) }
	out := &gen.DepreciationRunPreview{LegalEntityID: r.co.ID, CompanyCode: r.co.Code, LedgerID: r.ledger.ID, LedgerCode: r.ledger.Code,
		LedgerName: r.ledger.Name, FiscalYear: int32(r.period.FiscalYear), Period: int32(r.period.Number), PeriodStart: r.period.Start,
		PeriodEnd: r.period.End, Currency: r.co.Currency, Items: make([]gen.DepreciationPreviewItem, len(page)), NextPageToken: next,
		Totals: gen.DepreciationPreviewTotals{Assets: int32(len(r.assets)), Planned: lc(sums.planned), Booked: lc(sums.booked),
			Difference: lc(sums.difference), Lines: make([]gen.DepreciationPreviewLine, len(sums.lines))}}
	for i, c := range page {
		a := accounts[c.class]
		it := gen.DepreciationPreviewItem{AssetID: c.asset, AssetNumber: c.number, Description: c.description, ClassCode: c.class,
			ClassName: doc.AssetClass(c.class).Name, Method: c.terms.Method, CostCenterID: apitypes.OptUUID(c.costCenter),
			ProfitCenterID: apitypes.OptUUID(c.profitCenter), DepreciationAccount: accountAPI(a.depreciation), AccumulatedAccount: accountAPI(a.accumulated),
			Planned: lc(c.plannedTotal), Booked: lc(c.booked), Difference: lc(c.difference)}
		it.CostCenterCode, it.CostCenterName = names.of(orNil(c.costCenter))
		it.ProfitCenterCode, it.ProfitCenterName = names.of(orNil(c.profitCenter))
		out.Items[i] = it
	}
	for i, l := range sums.lines {
		line := gen.DepreciationPreviewLine{AccountID: l.account.ID, AccountNumber: l.account.Number, AccountName: l.account.Name,
			CostCenterID: notNil(l.costCenter), ProfitCenterID: notNil(l.profitCenter), Amount: lc(l.amount)}
		line.CostCenterCode, line.CostCenterName = names.of(l.costCenter)
		line.ProfitCenterCode, line.ProfitCenterName = names.of(l.profitCenter)
		out.Totals.Lines[i] = line
	}
	return out, nil
}

// accountAPI shows an account by its number and name.
func accountAPI(a AccountRef) gen.AssetOffsetAccount {
	return gen.AssetOffsetAccount{ID: a.ID, Number: a.Number, Name: a.Name}
}

// centerNames are centers' codes and names by id; nil when the caller may
// not read centers.
type centerNames map[uuid.UUID]CenterName

// of is a center's code and name, when known.
func (n centerNames) of(id uuid.UUID) (gen.OptString, gen.OptString) {
	c, ok := n[id]
	if !ok {
		return gen.OptString{}, gen.OptString{}
	}
	return gen.NewOptString(c.Code), gen.NewOptString(c.Name)
}

// previewCenters names the centers of the page's assets and of the lines,
// to a caller who may read cost centers (as an asset names them).
func (e *Endpoints) previewCenters(ctx context.Context, page []*candidate, lines []previewLine) (centerNames, error) {
	may, err := authz.MayUse(ctx, centerReadPermission)
	if err != nil || !may {
		return nil, err
	}
	var ids []uuid.UUID
	for _, id := range slices.Concat(centerIDs(page), lineCenters(lines)) {
		if id != uuid.Nil && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	names, err := e.Centers.Names(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("assets: names of %d centers: %w", len(ids), err)
	}
	return names, nil
}

func centerIDs(page []*candidate) []uuid.UUID {
	var out []uuid.UUID
	for _, c := range page {
		out = append(out, orNil(c.costCenter), orNil(c.profitCenter))
	}
	return out
}

func lineCenters(lines []previewLine) []uuid.UUID {
	var out []uuid.UUID
	for _, l := range lines {
		out = append(out, l.costCenter, l.profitCenter)
	}
	return out
}
