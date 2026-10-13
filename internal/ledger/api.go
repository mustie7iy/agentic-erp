package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func tag[T any](v T) gen.OptString { return gen.NewOptString(etag.Of(&v)) }

func optUUID(v *uuid.UUID) gen.OptUUID {
	if v == nil {
		return gen.OptUUID{}
	}
	return gen.NewOptUUID(*v)
}

func optString(v *string) gen.OptString {
	if v == nil {
		return gen.OptString{}
	}
	return gen.NewOptString(*v)
}

func ptr[T any](o interface{ Get() (T, bool) }) *T {
	if v, ok := o.Get(); ok {
		return &v
	}
	return nil
}

func money2(minor int64, currency string, exp int) gen.Money {
	return gen.Money{Amount: money.Minor(minor).Plain(exp), Currency: currency}
}

func paging(size gen.OptInt32, token gen.OptString, order gen.OptString) pagination.Request {
	return pagination.Request{OrderBy: order.Value, PageSize: size.Value, PageSizeSet: size.Set, PageToken: token.Value, PageTokenSet: token.Set}
}

func entryToAPI(e Entry) gen.JournalEntry {
	out := gen.JournalEntry{
		ID: e.ID, LegalEntityID: e.LegalEntityID, LedgerID: e.LedgerID, FiscalYear: int32(e.FiscalYear), FiscalPeriod: int32(e.FiscalPeriod),
		DocumentType: e.DocumentType, DocumentNumber: e.DocumentNumber, DocumentDate: e.DocumentDate, PostingDate: e.PostingDate,
		Currency: e.CurrencyTC, LocalCurrency: e.CurrencyLC, GroupCurrency: e.CurrencyGC, RateTcLc: gen.Decimal(e.RateTCLC),
		RateLcGc: gen.Decimal(e.RateLCGC), Reference: optString(e.Reference), HeaderText: optString(e.HeaderText),
		SourceContext: e.SourceContext, SourceDocumentID: optUUID(e.SourceDocumentID), ReversalOfID: optUUID(e.ReversalOfID),
		ReversedByID: optUUID(e.ReversedByID), PostedAt: e.PostedAt, PostedBy: e.PostedBy, Lines: make([]gen.JournalLine, len(e.Lines)),
	}
	for i, l := range e.Lines {
		line := gen.JournalLine{LineNo: l.LineNo, AccountID: l.AccountID, Amount: money2(l.TC, e.CurrencyTC, e.ExpTC),
			AmountLc: money2(l.LC, e.CurrencyLC, e.ExpLC), AmountGc: money2(l.GC, e.CurrencyGC, e.ExpGC), PartnerID: optUUID(l.PartnerID),
			TaxCode: optString(l.TaxCode), LineText: optString(l.Text)}
		if l.Quantity != nil {
			line.Quantity.SetTo(gen.Quantity{Value: *l.Quantity, Unit: *l.Unit})
		}
		out.Lines[i] = line
	}
	return out
}

func periodToAPI(p Period) gen.FiscalPeriod {
	return gen.FiscalPeriod{ID: p.ID, LegalEntityID: p.LegalEntityID, LedgerID: p.LedgerID, CalendarID: p.CalendarID, FiscalYear: int32(p.FiscalYear),
		Period: int32(p.Period), StartDate: p.StartDate, EndDate: p.EndDate, IsSpecial: p.IsSpecial, Status: p.Status, Version: p.Version,
		UpdatedAt: p.UpdatedAt, UpdatedBy: p.UpdatedBy}
}

func accountToAPI(a Account) gen.Account {
	out := gen.Account{ID: a.ID, Version: a.Version, CreatedAt: a.CreatedAt, CreatedBy: a.CreatedBy, UpdatedAt: a.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(a.UpdatedBy), ChartID: a.ChartID, Number: a.Number, Name: a.Name, AccountType: a.AccountType,
		IsBalanceSheet: a.IsBalanceSheet, ReconciliationKind: a.ReconciliationKind, Status: a.Status,
		CompanySettings: make([]gen.CompanyAccountSettings, len(a.Settings))}
	for i, s := range a.Settings {
		out.CompanySettings[i] = gen.CompanyAccountSettings{LegalEntityID: s.LegalEntityID, IsPostingBlocked: s.IsPostingBlocked,
			IsOpenItemManaged: s.IsOpenItemManaged}
	}
	return out
}

func rateToAPI(r Rate) gen.ExchangeRate {
	return gen.ExchangeRate{ID: r.ID, RateType: r.RateType, FromCurrency: r.FromCurrency, ToCurrency: r.ToCurrency, ValidFrom: r.ValidFrom,
		Rate: gen.Decimal(r.Rate), Source: r.Source, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy}
}

func clearingToAPI(c Clearing) gen.Clearing {
	out := gen.Clearing{ID: c.ID, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, LegalEntityID: c.LegalEntityID, ClearingDate: c.ClearingDate,
		ClearingEntryID: c.EntryID, Items: make([]gen.ClearingItemRef, len(c.Items))}
	for i, it := range c.Items {
		out.Items[i] = gen.ClearingItemRef{FiscalYear: int32(it.FiscalYear), JournalEntryID: it.EntryID, LineNo: it.LineNo}
	}
	return out
}

// GetAccount implements GET /v1/accounts/{id}.
func (e *Endpoints) GetAccount(ctx context.Context, params gen.GetAccountParams) (gen.GetAccountRes, error) {
	a, err := GetAccount(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.AccountHeaders{ETag: gen.NewOptString(etag.Format(a.Version)), Response: accountToAPI(a)}, nil
}

var accountSort = pagination.Sort{Default: "number", Fields: []pagination.Field{{Name: "number", Column: "a.number", Type: "text"}}}

// ListAccounts implements GET /v1/accounts.
func (e *Endpoints) ListAccounts(ctx context.Context, params gen.ListAccountsParams) (gen.ListAccountsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("listAccounts|%s|%s|%s|%s", ptr[uuid.UUID](params.LegalEntityID), ptr[uuid.UUID](params.ChartID), params.Number.Value, params.Status.Value)
	ks, err := e.Pages.Keyset(accountSort, scope, paging(params.PageSize, params.PageToken, params.OrderBy))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(accountSelect+` WHERE ($3::uuid IS NULL OR EXISTS (SELECT FROM ledger.company_account ca WHERE ca.tenant_id = a.tenant_id
			AND ca.account_id = a.id AND ca.legal_entity_id = $3 AND ($1 OR $3 = ANY($2))))
		AND ($4::uuid IS NULL OR a.chart_id = $4) AND ($5 = '' OR a.number = $5) AND ($6 = '' OR a.status = $6)`,
		[]any{access.All, access.IDs, ptr[uuid.UUID](params.LegalEntityID), ptr[uuid.UUID](params.ChartID), params.Number.Value, params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	accounts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Account, error) { return scanAccount(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list accounts: %w", err)
	}
	accounts, next := pagination.Page(ks, accounts, func(a Account) (string, uuid.UUID) { return a.Number, a.ID })
	page := gen.AccountPage{Items: make([]gen.Account, len(accounts))}
	for i, a := range accounts {
		page.Items[i] = accountToAPI(a)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.AccountPageHeaders{Response: page}, nil
}

// GetFiscalPeriod implements GET /v1/fiscal-periods/{id}.
func (e *Endpoints) GetFiscalPeriod(ctx context.Context, params gen.GetFiscalPeriodParams) (gen.GetFiscalPeriodRes, error) {
	p, err := GetPeriod(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.FiscalPeriodHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: periodToAPI(p)}, nil
}

var periodSort = pagination.Sort{Default: "fiscal_year", Fields: []pagination.Field{
	{Name: "fiscal_year", Column: "fiscal_year", Type: "smallint"}, {Name: "period", Column: "period", Type: "smallint"},
}}

// ListFiscalPeriods implements GET /v1/fiscal-periods.
func (e *Endpoints) ListFiscalPeriods(ctx context.Context, params gen.ListFiscalPeriodsParams) (gen.ListFiscalPeriodsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("listFiscalPeriods|%s|%s|%d|%s", ptr[uuid.UUID](params.LegalEntityID), ptr[uuid.UUID](params.LedgerID),
		params.FiscalYear.Value, params.Status.Value)
	ks, err := e.Pages.Keyset(periodSort, scope, paging(params.PageSize, params.PageToken, params.OrderBy))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(periodSelect+` WHERE ($1 OR legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR legal_entity_id = $3)
		AND ($4::uuid IS NULL OR ledger_id = $4) AND ($5::smallint IS NULL OR fiscal_year = $5) AND ($6 = '' OR status = $6)`,
		[]any{access.All, access.IDs, ptr[uuid.UUID](params.LegalEntityID), ptr[uuid.UUID](params.LedgerID), ptr[int32](params.FiscalYear), params.Status.Value})
	rows, _ := tx.Query(ctx, q, args...)
	periods, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Period, error) { return scanPeriod(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list fiscal periods: %w", err)
	}
	periods, next := pagination.Page(ks, periods, func(p Period) (string, uuid.UUID) {
		if ks.Field() == "period" {
			return fmt.Sprint(p.Period), p.ID
		}
		return fmt.Sprint(p.FiscalYear), p.ID
	})
	page := gen.FiscalPeriodPage{Items: make([]gen.FiscalPeriod, len(periods))}
	for i, p := range periods {
		page.Items[i] = periodToAPI(p)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.FiscalPeriodPageHeaders{Response: page}, nil
}

// OpenFiscalPeriod implements POST /v1/fiscal-periods/{id}:open.
func (e *Endpoints) OpenFiscalPeriod(ctx context.Context, params gen.OpenFiscalPeriodParams) (gen.OpenFiscalPeriodRes, error) {
	p, err := OpenPeriod(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.CloseFiscalPeriodOKHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: periodToAPI(p)}, nil
}

// CloseFiscalPeriod implements POST /v1/fiscal-periods/{id}:close.
func (e *Endpoints) CloseFiscalPeriod(ctx context.Context, params gen.CloseFiscalPeriodParams) (gen.CloseFiscalPeriodRes, error) {
	p, err := ClosePeriod(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.CloseFiscalPeriodOKHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: periodToAPI(p)}, nil
}

// PostJournalEntry implements POST /v1/journal-entries.
func (e *Endpoints) PostJournalEntry(ctx context.Context, req *gen.JournalEntryCreate, _ gen.PostJournalEntryParams) (gen.PostJournalEntryRes, error) {
	in := NewEntry{LegalEntityID: req.LegalEntityID, LedgerID: ptr[uuid.UUID](req.LedgerID), DocumentType: req.DocumentType.Or("SA"),
		DocumentDate: req.DocumentDate, PostingDate: req.PostingDate, Currency: req.Currency, Reference: ptr[string](req.Reference),
		HeaderText: ptr[string](req.HeaderText), Lines: make([]NewLine, len(req.Lines))}
	for i, l := range req.Lines {
		line := NewLine{AccountID: l.AccountID, Amount: l.Amount.Amount, Currency: l.Amount.Currency, PartnerID: ptr[uuid.UUID](l.PartnerID),
			TaxCode: ptr[string](l.TaxCode), Text: ptr[string](l.LineText)}
		if q, ok := l.Quantity.Get(); ok {
			line.Quantity, line.Unit = &q.Value, &q.Unit
		}
		in.Lines[i] = line
	}
	je, err := e.postJournalEntry(ctx, in)
	if err != nil {
		return nil, err
	}
	rep := entryToAPI(je)
	return &gen.JournalEntryHeaders{ETag: tag(rep), Location: gen.NewOptString("/v1/journal-entries/" + je.ID.String()), Response: rep}, nil
}

// GetJournalEntry implements GET /v1/journal-entries/{id}.
func (e *Endpoints) GetJournalEntry(ctx context.Context, params gen.GetJournalEntryParams) (gen.GetJournalEntryRes, error) {
	je, err := GetEntry(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	rep := entryToAPI(je)
	return &gen.GetJournalEntryOKHeaders{ETag: tag(rep), Response: rep}, nil
}

var entrySort = pagination.Sort{Default: "posting_date", Fields: []pagination.Field{
	{Name: "posting_date", Column: "e.posting_date", Type: "date"}, {Name: "document_number", Column: "e.document_number", Type: "text"},
}}

// ListJournalEntries implements GET /v1/journal-entries.
func (e *Endpoints) ListJournalEntries(ctx context.Context, params gen.ListJournalEntriesParams) (gen.ListJournalEntriesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	from, to := ptr[time.Time](params.PostingDateFrom), ptr[time.Time](params.PostingDateTo)
	scope := fmt.Sprintf("listJournalEntries|%s|%s|%v|%v|%d|%s|%s", ptr[uuid.UUID](params.LegalEntityID), params.DocumentNumber.Value, from, to,
		params.FiscalYear.Value, params.SourceContext.Value, ptr[uuid.UUID](params.SourceDocumentID))
	ks, err := e.Pages.Keyset(entrySort, scope, paging(params.PageSize, params.PageToken, params.OrderBy))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(entrySelect+` WHERE ($1 OR e.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR e.legal_entity_id = $3)
		AND ($4 = '' OR e.document_number = $4) AND ($5::date IS NULL OR e.posting_date >= $5) AND ($6::date IS NULL OR e.posting_date <= $6)
		AND ($7::smallint IS NULL OR e.fiscal_year = $7) AND ($8 = '' OR e.source_context = $8) AND ($9::uuid IS NULL OR e.source_document_id = $9)`,
		[]any{access.All, access.IDs, ptr[uuid.UUID](params.LegalEntityID), params.DocumentNumber.Value, from, to, ptr[int32](params.FiscalYear),
			params.SourceContext.Value, ptr[uuid.UUID](params.SourceDocumentID)})
	rows, _ := tx.Query(ctx, q, args...)
	entries, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Entry, error) { return scanEntry(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list journal entries: %w", err)
	}
	entries, next := pagination.Page(ks, entries, func(je Entry) (string, uuid.UUID) {
		if ks.Field() == "document_number" {
			return je.DocumentNumber, je.ID
		}
		return je.PostingDate.Format(time.DateOnly), je.ID
	})
	page := gen.JournalEntryPage{Items: make([]gen.JournalEntry, len(entries))}
	for i, je := range entries {
		page.Items[i] = entryToAPI(je)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.JournalEntryPageHeaders{Response: page}, nil
}

// ReverseJournalEntry implements POST /v1/journal-entries/{id}:reverse.
func (e *Endpoints) ReverseJournalEntry(ctx context.Context, req gen.OptJournalEntryReverse, params gen.ReverseJournalEntryParams) (gen.ReverseJournalEntryRes, error) {
	check := func(orig Entry) error { return etag.Match(params.IfMatch, etag.Of(ptrTo(entryToAPI(orig)))) }
	rev, err := e.reverseJournalEntry(ctx, params.ID, check, ptr[time.Time](req.Value.PostingDate), ptr[string](req.Value.HeaderText))
	if err != nil {
		return nil, err
	}
	rep := entryToAPI(rev)
	return &gen.ReverseJournalEntryOKHeaders{ETag: tag(rep), Response: rep}, nil
}

func ptrTo[T any](v T) *T { return &v }

var openItemSort = pagination.Sort{Default: "posting_date", Fields: []pagination.Field{{Name: "posting_date", Column: "posting_date", Type: "date"}}}

// ListOpenItems implements GET /v1/open-items.
func (e *Endpoints) ListOpenItems(ctx context.Context, params gen.ListOpenItemsParams) (gen.ListOpenItemsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	to := ptr[time.Time](params.PostingDateTo)
	scope := fmt.Sprintf("listOpenItems|%s|%s|%s|%v", ptr[uuid.UUID](params.LegalEntityID), ptr[uuid.UUID](params.PartnerID),
		ptr[uuid.UUID](params.AccountID), to)
	ks, err := e.Pages.Keyset(openItemSort, scope, paging(params.PageSize, params.PageToken, params.OrderBy))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(openItemSelect+` AND ($3::uuid IS NULL OR l.legal_entity_id = $3) AND ($4::uuid IS NULL OR l.partner_id = $4)
		AND ($5::uuid IS NULL OR l.account_id = $5) AND ($6::date IS NULL OR l.posting_date <= $6)) o WHERE true`,
		[]any{access.All, access.IDs, ptr[uuid.UUID](params.LegalEntityID), ptr[uuid.UUID](params.PartnerID), ptr[uuid.UUID](params.AccountID), to})
	rows, _ := tx.Query(ctx, q, args...)
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (OpenItem, error) { return scanOpenItem(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list open items: %w", err)
	}
	items, next := pagination.Page(ks, items, func(o OpenItem) (string, uuid.UUID) { return o.PostingDate.Format(time.DateOnly), o.ID })
	page := gen.OpenItemPage{Items: make([]gen.OpenItem, len(items))}
	for i, o := range items {
		page.Items[i] = gen.OpenItem{FiscalYear: int32(o.FiscalYear), JournalEntryID: o.EntryID, LineNo: o.LineNo, LegalEntityID: o.LegalEntityID,
			AccountID: o.AccountID, PartnerID: optUUID(o.PartnerID), PostingDate: o.PostingDate, Amount: money2(o.TC, o.CurrencyTC, o.ExpTC),
			AmountLc: money2(o.LC, o.CurrencyLC, o.ExpLC), DocumentNumber: o.DocumentNumber, Reference: optString(o.Reference)}
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.OpenItemPageHeaders{Response: page}, nil
}

var balanceSort = pagination.Sort{Default: "account_id", Fields: []pagination.Field{
	{Name: "account_id", Column: "account_id", Type: "uuid"}, {Name: "fiscal_period", Column: "fiscal_period", Type: "smallint"},
}}

// ListBalances implements GET /v1/balances: the trial balance of a legal
// entity and fiscal year, per account, period and transaction currency.
// A legal entity the caller cannot act in, or without a ledger, has none.
func (e *Endpoints) ListBalances(ctx context.Context, params gen.ListBalancesParams) (gen.ListBalancesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	period, account := ptr[int32](params.FiscalPeriod), ptr[uuid.UUID](params.AccountID)
	scope := fmt.Sprintf("listBalances|%s|%s|%d|%v|%s", params.LegalEntityID, ptr[uuid.UUID](params.LedgerID), params.FiscalYear, params.FiscalPeriod, account)
	ks, err := e.Pages.Keyset(balanceSort, scope, paging(params.PageSize, params.PageToken, params.OrderBy))
	if err != nil {
		return nil, err
	}
	empty := &gen.BalancePageHeaders{Response: gen.BalancePage{Items: []gen.Balance{}}}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	le, ok, err := e.LegalEntities.Entity(ctx, params.LegalEntityID)
	if err != nil {
		return nil, err
	}
	if !ok || !access.Allows(le.ID) {
		return empty, nil
	}
	var ledger uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM ledger.ledger WHERE ($1::uuid IS NULL AND is_leading) OR id = $1`, ptr[uuid.UUID](params.LedgerID)).Scan(&ledger)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: list balances %s: find ledger: %w", le.ID, err)
	}
	group, err := e.LegalEntities.GroupCurrency(ctx)
	if err != nil {
		return nil, err
	}
	exps, err := exponents(ctx, tx, le.LocalCurrency, group)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(balanceSelect+` WHERE true`, []any{le.ID, ledger, params.FiscalYear, period, account})
	rows, _ := tx.Query(ctx, q, args...)
	balances, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Balance, error) { return scanBalance(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list balances %s: %w", le.ID, err)
	}
	balances, next := pagination.Page(ks, balances, func(b Balance) (string, uuid.UUID) {
		if ks.Field() == "fiscal_period" {
			return fmt.Sprint(b.FiscalPeriod), b.ID
		}
		return b.AccountID.String(), b.ID
	})
	page := gen.BalancePage{Items: make([]gen.Balance, len(balances))}
	for i, b := range balances {
		lc, gc := exps[le.LocalCurrency], exps[group]
		page.Items[i] = gen.Balance{LedgerID: ledger, LegalEntityID: le.ID, AccountID: b.AccountID, FiscalYear: params.FiscalYear,
			FiscalPeriod: int32(b.FiscalPeriod), Currency: b.CurrencyTC, Debit: money2(b.DebitTC, b.CurrencyTC, b.ExpTC),
			Credit: money2(b.CreditTC, b.CurrencyTC, b.ExpTC), DebitLc: money2(b.DebitLC, le.LocalCurrency, lc),
			CreditLc: money2(b.CreditLC, le.LocalCurrency, lc), DebitGc: money2(b.DebitGC, group, gc), CreditGc: money2(b.CreditGC, group, gc),
			UpdatedAt: b.UpdatedAt}
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.BalancePageHeaders{Response: page}, nil
}

// CreateExchangeRate implements POST /v1/exchange-rates.
func (e *Endpoints) CreateExchangeRate(ctx context.Context, req *gen.ExchangeRateCreate, _ gen.CreateExchangeRateParams) (gen.CreateExchangeRateRes, error) {
	r, err := CreateRate(ctx, NewRate{RateType: req.RateType, FromCurrency: req.FromCurrency, ToCurrency: req.ToCurrency, ValidFrom: req.ValidFrom,
		Rate: string(req.Rate), Source: req.Source})
	if err != nil {
		return nil, err
	}
	rep := rateToAPI(r)
	return &gen.ExchangeRateHeaders{ETag: tag(rep), Location: gen.NewOptString("/v1/exchange-rates/" + r.ID.String()), Response: rep}, nil
}

// GetExchangeRate implements GET /v1/exchange-rates/{id}.
func (e *Endpoints) GetExchangeRate(ctx context.Context, params gen.GetExchangeRateParams) (gen.GetExchangeRateRes, error) {
	r, err := GetRate(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	rep := rateToAPI(r)
	return &gen.GetExchangeRateOKHeaders{ETag: tag(rep), Response: rep}, nil
}

var rateSort = pagination.Sort{Default: "valid_from", Fields: []pagination.Field{{Name: "valid_from", Column: "valid_from", Type: "date"}}}

// ListExchangeRates implements GET /v1/exchange-rates. With valid_on, each
// rate type and pair contributes the rate in effect on that date.
func (e *Endpoints) ListExchangeRates(ctx context.Context, params gen.ListExchangeRatesParams) (gen.ListExchangeRatesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	on := ptr[time.Time](params.ValidOn)
	scope := fmt.Sprintf("listExchangeRates|%s|%s|%s|%v", params.RateType.Value, params.FromCurrency.Value, params.ToCurrency.Value, on)
	ks, err := e.Pages.Keyset(rateSort, scope, paging(params.PageSize, params.PageToken, params.OrderBy))
	if err != nil {
		return nil, err
	}
	distinct, order := "", ""
	if on != nil {
		distinct, order = "DISTINCT ON (rate_type, from_currency, to_currency)", "ORDER BY rate_type, from_currency, to_currency, valid_from DESC"
	}
	q, args := ks.SQL(`SELECT * FROM (SELECT `+distinct+` `+rateColumns+` FROM ledger.exchange_rate
		WHERE ($1 = '' OR rate_type = $1) AND ($2 = '' OR from_currency = $2) AND ($3 = '' OR to_currency = $3) AND ($4::date IS NULL OR valid_from <= $4)
		`+order+`) r WHERE true`, []any{params.RateType.Value, params.FromCurrency.Value, params.ToCurrency.Value, on})
	rows, _ := tx.Query(ctx, q, args...)
	rates, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Rate, error) { return scanRate(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list exchange rates: %w", err)
	}
	rates, next := pagination.Page(ks, rates, func(r Rate) (string, uuid.UUID) { return r.ValidFrom.Format(time.DateOnly), r.ID })
	page := gen.ExchangeRatePage{Items: make([]gen.ExchangeRate, len(rates))}
	for i, r := range rates {
		page.Items[i] = rateToAPI(r)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.ExchangeRatePageHeaders{Response: page}, nil
}

// CreateClearing implements POST /v1/clearings.
func (e *Endpoints) CreateClearing(ctx context.Context, req *gen.ClearingCreate, _ gen.CreateClearingParams) (gen.CreateClearingRes, error) {
	refs := make([]ItemRef, len(req.Items))
	for i, it := range req.Items {
		refs[i] = ItemRef{FiscalYear: int16(it.FiscalYear), EntryID: it.JournalEntryID, LineNo: it.LineNo}
	}
	c, err := e.createClearing(ctx, req.LegalEntityID, req.ClearingDate, refs)
	if err != nil {
		return nil, err
	}
	rep := clearingToAPI(c)
	return &gen.ClearingHeaders{ETag: tag(rep), Location: gen.NewOptString("/v1/clearings/" + c.ID.String()), Response: rep}, nil
}

// GetClearing implements GET /v1/clearings/{id}.
func (e *Endpoints) GetClearing(ctx context.Context, params gen.GetClearingParams) (gen.GetClearingRes, error) {
	c, err := GetClearing(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	rep := clearingToAPI(c)
	return &gen.GetClearingOKHeaders{ETag: tag(rep), Response: rep}, nil
}

var clearingSort = pagination.Sort{Default: "clearing_date", Fields: []pagination.Field{{Name: "clearing_date", Column: "c.clearing_date", Type: "date"}}}

// ListClearings implements GET /v1/clearings.
func (e *Endpoints) ListClearings(ctx context.Context, params gen.ListClearingsParams) (gen.ListClearingsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(clearingSort, fmt.Sprintf("listClearings|%s", ptr[uuid.UUID](params.LegalEntityID)),
		paging(params.PageSize, params.PageToken, params.OrderBy))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(clearingSelect+` WHERE ($1 OR c.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR c.legal_entity_id = $3)`,
		[]any{access.All, access.IDs, ptr[uuid.UUID](params.LegalEntityID)})
	rows, _ := tx.Query(ctx, q, args...)
	clearings, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Clearing, error) { return scanClearing(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: list clearings: %w", err)
	}
	clearings, next := pagination.Page(ks, clearings, func(c Clearing) (string, uuid.UUID) { return c.ClearingDate.Format(time.DateOnly), c.ID })
	page := gen.ClearingPage{Items: make([]gen.Clearing, len(clearings))}
	for i, c := range clearings {
		page.Items[i] = clearingToAPI(c)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.ClearingPageHeaders{Response: page}, nil
}
