package assets

import (
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
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// used is what acquisitions used of a capitalised line in its currency,
// local and group currency, whether all of it, and whether a posted
// acquisition holds some.
type used struct {
	share
	all, held bool
}

// usesOf reads what acquisitions used of the company's lines of a fiscal
// year, by line.
func usesOf(ctx context.Context, tx pgx.Tx, le uuid.UUID, year int16) (map[LineRef]used, error) {
	rows, _ := tx.Query(ctx, `SELECT u.entry_year, u.entry_id, u.line_no, u.used, u.used_lc, u.used_gc, u.used = u.line_amount,
			EXISTS (SELECT FROM assets.acquisition q WHERE q.tenant_id = u.tenant_id AND q.source_entry_id = u.entry_id AND q.source_line_no = u.line_no
				AND q.status = 'posted')
		FROM assets.clearing_use u WHERE u.legal_entity_id = $1 AND u.entry_year = $2`, le, year)
	out := map[LineRef]used{}
	var r LineRef
	var u used
	_, err := pgx.ForEachRow(rows, []any{&r.FiscalYear, &r.EntryID, &r.LineNo, &u.tc, &u.lc, &u.gc, &u.all, &u.held}, func() error {
		out[r] = u
		return nil
	})
	return out, errs.Wrapf(err, "assets: used lines of %s in %d", le, year)
}

// clearingAcquisition is an acquisition as the worklist shows it under its
// line.
type clearingAcquisition struct {
	id, asset   uuid.UUID
	assetNumber string
	amount      money.Minor
	status      string
}

// acquisitionsFrom reads the acquisitions made from the lines, by line, in
// posting order.
func acquisitionsFrom(ctx context.Context, tx pgx.Tx, lines []Line) (map[LineRef][]clearingAcquisition, error) {
	entries, nos := make([]uuid.UUID, len(lines)), make([]int32, len(lines))
	for i, l := range lines {
		entries[i], nos[i] = l.EntryID, l.LineNo
	}
	rows, _ := tx.Query(ctx, `SELECT q.source_entry_year, q.source_entry_id, q.source_line_no, q.id, q.asset_id, a.asset_number, q.amount, q.status
		FROM assets.acquisition q JOIN assets.asset a ON a.tenant_id = q.tenant_id AND a.id = q.asset_id
		JOIN unnest($1::uuid[], $2::integer[]) AS x(e, n) ON q.source_entry_id = x.e AND q.source_line_no = x.n
		ORDER BY q.created_at, q.id`, entries, nos)
	out := map[LineRef][]clearingAcquisition{}
	var r LineRef
	var c clearingAcquisition
	_, err := pgx.ForEachRow(rows, []any{&r.FiscalYear, &r.EntryID, &r.LineNo, &c.id, &c.asset, &c.assetNumber, &c.amount, &c.status}, func() error {
		out[r] = append(out[r], c)
		return nil
	})
	return out, errs.Wrapf(err, "assets: acquisitions from %d lines", len(lines))
}

// clearingAccounts are the company's asset clearing accounts by id, from
// its asset accounts in the active document.
func (e *Endpoints) clearingAccounts(ctx context.Context, doc *config.Config, co Company) (map[uuid.UUID]*config.Account, error) {
	c := doc.Company(co.Code)
	if c == nil {
		return nil, nil
	}
	var numbers []string
	for _, a := range c.AssetAccounts {
		if !slices.Contains(numbers, a.Clearing) {
			numbers = append(numbers, a.Clearing)
		}
	}
	ids, err := e.Ledger.Accounts(ctx, co.ID, numbers)
	if err != nil {
		return nil, fmt.Errorf("assets: clearing accounts of %s: %w", co.Code, err)
	}
	out := make(map[uuid.UUID]*config.Account, len(ids))
	for number, id := range ids {
		out[id] = c.Account(number)
	}
	return out, nil
}

// worklist is a request for the worklist.
type worklist struct {
	co        Company
	accounts  map[uuid.UUID]*config.Account
	year      int16
	remaining bool
	after     *ClearingKey
	size      int
}

// ListAssetClearingItems implements GET /v1/asset-clearing-items
// (ADR-0057 §6): the leading ledger's lines on the company's clearing
// accounts in a fiscal year with what acquisitions used of them.
func (e *Endpoints) ListAssetClearingItems(ctx context.Context, params gen.ListAssetClearingItemsParams) (gen.ListAssetClearingItemsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("listAssetClearingItems|%s|%d|%t", params.LegalEntityID, params.FiscalYear, params.Remaining.Value)
	w := worklist{year: int16(params.FiscalYear), remaining: params.Remaining.Value, size: size}
	if params.PageToken.Set {
		w.after = &ClearingKey{}
		if err := e.Pages.Decode(scope, params.PageToken.Value, w.after); err != nil {
			return nil, err
		}
	}
	page := &gen.AssetClearingItemPage{Items: []gen.AssetClearingItem{}}
	ok, err := e.worklistOf(ctx, params.LegalEntityID, &w)
	if err != nil || !ok {
		return page, err
	}
	lines, err := e.clearingLines(ctx, tx, w)
	if err != nil {
		return nil, err
	}
	if len(lines.lines) > size {
		lines.lines = lines.lines[:size]
		last := lines.lines[size-1]
		tok, _ := e.Pages.Encode(scope, ClearingKey{PostingDate: last.PostingDate, EntryID: last.EntryID, LineNo: last.LineNo}) // a date, a uuid and an int always encode
		page.NextPageToken = gen.NewOptString(tok)
	}
	if page.Items, err = e.clearingItems(ctx, tx, w, lines); err != nil {
		return nil, err
	}
	return page, nil
}

// worklistOf reads the company the caller may act in and its clearing
// accounts; ok false: it has none to list.
func (e *Endpoints) worklistOf(ctx context.Context, le uuid.UUID, w *worklist) (bool, error) {
	access, err := authz.Companies(ctx)
	if err != nil || !access.Allows(le) {
		return false, err
	}
	co, ok, err := e.Companies.Company(ctx, le)
	if err != nil || !ok {
		return false, errs.Wrapf(err, "assets: legal entity %s", le)
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return false, fmt.Errorf("assets: configuration: %w", err)
	}
	w.co = co
	if w.accounts, err = e.clearingAccounts(ctx, doc, co); err != nil {
		return false, err
	}
	return len(w.accounts) > 0, nil
}

// clearingPage is a page of lines with what was used of the year's lines.
type clearingPage struct {
	lines []Line
	uses  map[LineRef]used
}

// clearingLines reads the page's lines and one more, leaving out the
// clearing lines of the acquisitions' own entries. With remaining, the
// lines used in full and every reversed line are left out; otherwise a
// reversed line is kept while a posted acquisition holds some of it.
func (e *Endpoints) clearingLines(ctx context.Context, tx pgx.Tx, w worklist) (clearingPage, error) {
	uses, err := usesOf(ctx, tx, w.co.ID, w.year)
	if err != nil {
		return clearingPage{}, err
	}
	q := ClearingQuery{LegalEntityID: w.co.ID, ExceptContext: sourceAssets, FiscalYear: w.year, After: w.after, Limit: w.size + 1}
	for id := range w.accounts {
		q.Accounts = append(q.Accounts, id)
	}
	for r, u := range uses {
		switch {
		case w.remaining && u.all:
			q.Skip = append(q.Skip, r)
		case !w.remaining && u.held:
			q.Keep = append(q.Keep, r)
		}
	}
	lines, err := e.Ledger.ClearingLines(ctx, q)
	if err != nil {
		return clearingPage{}, fmt.Errorf("assets: clearing lines of %s in %d: %w", w.co.Code, w.year, err)
	}
	return clearingPage{lines: lines, uses: uses}, nil
}

// clearingItems shows the page's lines with their acquisitions and the
// supplier invoices they came from.
func (e *Endpoints) clearingItems(ctx context.Context, tx pgx.Tx, w worklist, p clearingPage) ([]gen.AssetClearingItem, error) {
	acqs, err := acquisitionsFrom(ctx, tx, p.lines)
	if err != nil {
		return nil, err
	}
	var currencies []string
	var invoices []uuid.UUID
	for _, l := range p.lines {
		currencies = append(currencies, l.Currency, l.CurrencyLC, l.CurrencyGC)
		if id, ok := invoiceOf(l.SourceContext, l.SourceDocumentID); ok {
			invoices = append(invoices, id)
		}
	}
	v, err := e.docViewOf(ctx, tx, []uuid.UUID{w.co.ID}, currencies, invoices)
	if err != nil {
		return nil, err
	}
	items := make([]gen.AssetClearingItem, len(p.lines))
	for i, l := range p.lines {
		items[i] = v.clearingItem(l, w.accounts[l.AccountID], p.uses[l.LineRef], acqs[l.LineRef])
	}
	return items, nil
}

// clearingItem shows a line with what was used of it and its acquisitions.
func (v docView) clearingItem(l Line, account *config.Account, u used, acqs []clearingAcquisition) gen.AssetClearingItem {
	it := gen.AssetClearingItem{FiscalYear: int32(l.FiscalYear), EntryID: l.EntryID, LineNo: l.LineNo, PostingDate: l.PostingDate,
		DocumentNumber: l.DocumentNumber, SourceContext: l.SourceContext, SourceDocumentID: apitypes.OptUUID(l.SourceDocumentID),
		SupplierInvoice: v.supplierInvoice(l.SourceContext, l.SourceDocumentID), AccountID: l.AccountID, AccountNumber: account.Number,
		AccountName: account.Name, Amount: v.money(l.Amount, l.Currency), AmountLc: v.money(l.LC, l.CurrencyLC), AmountGc: v.money(l.GC, l.CurrencyGC),
		Used: v.money(u.tc, l.Currency), UsedLc: v.money(u.lc, l.CurrencyLC), UsedGc: v.money(u.gc, l.CurrencyGC), Remaining: v.money(l.Amount-u.tc, l.Currency), RemainingLc: v.money(l.LC-u.lc, l.CurrencyLC),
		RemainingGc: v.money(l.GC-u.gc, l.CurrencyGC), SourceReversed: l.Reversed || l.Reversal, Acquisitions: []gen.AssetClearingAcquisition{}}
	for _, a := range acqs {
		it.Acquisitions = append(it.Acquisitions, gen.AssetClearingAcquisition{ID: a.id, AssetID: a.asset, AssetNumber: a.assetNumber,
			Amount: v.money(a.amount, l.Currency), Status: a.status})
	}
	return it
}
