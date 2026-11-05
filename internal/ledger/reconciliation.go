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
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ledgerOrLeading is the ledger asked for, or the leading ledger when none
// is; ok is false for an unknown ledger.
func ledgerOrLeading(ctx context.Context, tx pgx.Tx, asked *uuid.UUID) (id uuid.UUID, ok bool, err error) {
	err = tx.QueryRow(ctx, `SELECT id FROM ledger.ledger WHERE ($1::uuid IS NULL AND is_leading) OR id = $1`, asked).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("ledger: find ledger %v: %w", asked, err)
	}
	return id, true, nil
}

// reconciliation is what both reconciliation reads resolve first: the
// legal entity, the two ledgers and the currencies amounts are shown in.
type reconciliation struct {
	le              tenancy.Entity
	ledger, compare uuid.UUID
	group           string
	exps            currency.Exponents
}

// reconcile resolves a reconciliation of two ledgers of a legal entity
// (ADR-0045, T52e). ok is false when the caller cannot act in the legal
// entity or it does not keep both ledgers: there is nothing to reconcile.
// r.ledger is set whenever the ledger is known, for the empty page.
func (e *Endpoints) reconcile(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, asked *uuid.UUID, compare uuid.UUID) (r reconciliation, ok bool, err error) {
	r.compare = compare
	found, ok, err := ledgerOrLeading(ctx, tx, asked)
	if err != nil || !ok {
		if asked != nil {
			r.ledger = *asked
		}
		return r, false, err
	}
	r.ledger = found
	access, err := authz.Companies(ctx)
	if err != nil {
		return r, false, err
	}
	if r.le, ok, err = e.LegalEntities.Entity(ctx, legalEntity); err != nil || !ok || !access.Allows(r.le.ID) {
		return r, false, err
	}
	if err := tx.QueryRow(ctx, `SELECT ledger.company_keeps($1, $2) AND ledger.company_keeps($1, $3)`, r.le.ID, r.ledger, compare).Scan(&ok); err != nil {
		return r, false, fmt.Errorf("ledger: reconcile %s: ledgers kept: %w", r.le.ID, err)
	}
	if !ok {
		return r, false, nil
	}
	if r.group, err = e.LegalEntities.GroupCurrency(ctx); err != nil {
		return r, false, err
	}
	r.exps, err = exponents(ctx, tx, r.le.LocalCurrency, r.group)
	return r, err == nil, err
}

var differenceSort = pagination.Sort{Default: "account_id", Fields: []pagination.Field{{Name: "account_id", Column: "account_id", Type: "uuid"}}}

// differenceSelect compares the balances of $6 the ledger and $7 the
// compared ledger ($2 holding both) per account and transaction currency,
// keeping the pairs that differ in any currency. A ledger compared with
// itself has no differences: each row counts for both.
const differenceSelect = `SELECT id, account_id, currency_tc, exponent, a_tc, b_tc, a_tc - b_tc, a_lc - b_lc, a_gc - b_gc FROM (
	SELECT md5(account_id::text || '/' || currency_tc)::uuid AS id, account_id, currency_tc, c.exponent,
		coalesce(sum(debit_tc - credit_tc) FILTER (WHERE ledger_id = $6), 0)::bigint AS a_tc,
		coalesce(sum(debit_tc - credit_tc) FILTER (WHERE ledger_id = $7), 0)::bigint AS b_tc,
		coalesce(sum(debit_lc - credit_lc) FILTER (WHERE ledger_id = $6), 0)::bigint AS a_lc,
		coalesce(sum(debit_lc - credit_lc) FILTER (WHERE ledger_id = $7), 0)::bigint AS b_lc,
		coalesce(sum(debit_gc - credit_gc) FILTER (WHERE ledger_id = $6), 0)::bigint AS a_gc,
		coalesce(sum(debit_gc - credit_gc) FILTER (WHERE ledger_id = $7), 0)::bigint AS b_gc
	FROM (` + balanceRows + `) b
	JOIN ref.currency c ON c.code = currency_tc
	GROUP BY account_id, currency_tc, c.exponent) t
WHERE (a_tc, a_lc, a_gc) <> (b_tc, b_lc, b_gc)`

// trialBalanceSelect totals each ledger of $2 in its order; a ledger
// without balances totals zero. It runs planned per call
// (pagination.PlanPerCall): PostgreSQL's generic plan for it cannot use
// the period, when one is given, in the balances' index, and from a
// cached statement's sixth run it took that plan (5,000 accounts in two
// ledgers: a period's totals 26 ms to 50 ms, the year's 212 ms to 290 ms).
const trialBalanceSelect = `SELECT u.ledger, coalesce(sum(debit_lc), 0)::bigint, coalesce(sum(credit_lc), 0)::bigint,
		coalesce(sum(debit_gc), 0)::bigint, coalesce(sum(credit_gc), 0)::bigint
	FROM unnest($2::uuid[]) WITH ORDINALITY u(ledger, n)
	LEFT JOIN (` + balanceRows + `) b ON b.ledger_id = u.ledger
	GROUP BY u.ledger, u.n ORDER BY u.n`

// difference is one account and currency whose balances differ.
type difference struct {
	ID                   uuid.UUID // derived from account and currency, for paging
	AccountID            uuid.UUID
	Currency             string
	Exp                  int
	Balance, Compare     money.Minor
	Diff, DiffLC, DiffGC money.Minor
}

// ListLedgerReconciliation implements GET /v1/ledger-reconciliation: the
// differences between two ledgers of a legal entity per account and
// currency, and each ledger's trial balance check (ADR-0045, T52e).
func (e *Endpoints) ListLedgerReconciliation(ctx context.Context, params gen.ListLedgerReconciliationParams) (gen.ListLedgerReconciliationRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	period, account := apitypes.Ptr[int32](params.FiscalPeriod), apitypes.Ptr[uuid.UUID](params.AccountID)
	// The Opt values, not the pointers: a pointer prints its address.
	scope := fmt.Sprintf("listLedgerReconciliation|%s|%v|%s|%d|%v|%v", params.LegalEntityID, params.LedgerID, params.CompareLedgerID,
		params.FiscalYear, params.FiscalPeriod, params.AccountID)
	ks, err := e.Pages.Keyset(differenceSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	r, ok, err := e.reconcile(ctx, tx, params.LegalEntityID, apitypes.Ptr[uuid.UUID](params.LedgerID), params.CompareLedgerID)
	page := gen.LedgerReconciliationPage{LedgerID: r.ledger, CompareLedgerID: r.compare, TrialBalances: []gen.LedgerTrialBalance{}, Items: []gen.LedgerDifference{}}
	if err != nil || !ok {
		return &page, err
	}
	ledgers := []uuid.UUID{r.ledger, r.compare}
	// The trial balances read every balance of both ledgers again: the
	// first page carries them, the next ones need not.
	if !params.PageToken.Set {
		if page.TrialBalances, err = r.trialBalances(ctx, tx, ledgers, params.FiscalYear, period); err != nil {
			return nil, err
		}
	}
	q, args := ks.SQL(differenceSelect, []any{r.le.ID, ledgers, params.FiscalYear, period, account, r.ledger, r.compare})
	rows, _ := tx.Query(ctx, q, args...)
	diffs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[difference])
	if err != nil {
		return nil, fmt.Errorf("ledger: reconcile %s: %s with %s: %w", r.le.ID, r.ledger, r.compare, err)
	}
	diffs, next := pagination.Page(ks, diffs, func(d difference) (string, uuid.UUID) { return d.AccountID.String(), d.ID })
	accounts, err := accountNames(ctx, tx, diffs)
	if err != nil {
		return nil, err
	}
	for _, d := range diffs {
		lc, gc := r.exps[r.le.LocalCurrency], r.exps[r.group]
		a := accounts[d.AccountID]
		page.Items = append(page.Items, gen.LedgerDifference{AccountID: d.AccountID, AccountNumber: a.Number, AccountName: a.Name, Currency: d.Currency,
			Balance:        apitypes.Money(d.Balance, d.Currency, d.Exp),
			CompareBalance: apitypes.Money(d.Compare, d.Currency, d.Exp), Difference: apitypes.Money(d.Diff, d.Currency, d.Exp),
			DifferenceLc: apitypes.Money(d.DiffLC, r.le.LocalCurrency, lc), DifferenceGc: apitypes.Money(d.DiffGC, r.group, gc)})
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &page, nil
}

// accountName is what an account is known by.
type accountName struct{ Number, Name string }

// accountNames are the numbers and names of a page's accounts, by id
// (T73i2): a difference names its account as the GR/IR report does, so a
// reader needs no chart of accounts beside it.
func accountNames(ctx context.Context, tx pgx.Tx, diffs []difference) (map[uuid.UUID]accountName, error) {
	ids := make([]uuid.UUID, len(diffs))
	for i, d := range diffs {
		ids[i] = d.AccountID
	}
	rows, _ := tx.Query(ctx, `SELECT id, number, name FROM ledger.gl_account WHERE id = ANY($1)`, ids)
	out := make(map[uuid.UUID]accountName, len(ids))
	var id uuid.UUID
	var a accountName
	if _, err := pgx.ForEachRow(rows, []any{&id, &a.Number, &a.Name}, func() error { out[id] = a; return nil }); err != nil {
		return nil, fmt.Errorf("ledger: names of %d reconciled accounts: %w", len(ids), err)
	}
	return out, nil
}

// ledgerTotals is one ledger's trial balance totals.
type ledgerTotals struct {
	LedgerID          uuid.UUID
	DebitLC, CreditLC money.Minor
	DebitGC, CreditGC money.Minor
}

// trialBalances checks that each ledger's trial balance nets to zero in
// local and group currency, over the balances the trial balance reads.
func (r reconciliation) trialBalances(ctx context.Context, tx pgx.Tx, ledgers []uuid.UUID, year int32, period *int32) ([]gen.LedgerTrialBalance, error) {
	rows, _ := tx.Query(ctx, trialBalanceSelect, pagination.PlanPerCall, r.le.ID, ledgers, year, period, nil)
	lc, gc := r.exps[r.le.LocalCurrency], r.exps[r.group]
	totals, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ledgerTotals])
	if err != nil {
		return nil, fmt.Errorf("ledger: trial balances of %v in %s: %w", ledgers, r.le.ID, err)
	}
	out := make([]gen.LedgerTrialBalance, len(totals))
	for i, t := range totals {
		out[i] = gen.LedgerTrialBalance{LedgerID: t.LedgerID, DebitLc: apitypes.Money(t.DebitLC, r.le.LocalCurrency, lc),
			CreditLc: apitypes.Money(t.CreditLC, r.le.LocalCurrency, lc), DebitGc: apitypes.Money(t.DebitGC, r.group, gc),
			CreditGc: apitypes.Money(t.CreditGC, r.group, gc), Balanced: t.DebitLC == t.CreditLC && t.DebitGC == t.CreditGC}
	}
	return out, nil
}

var reconEntrySort = pagination.Sort{Default: "posting_date", Fields: []pagination.Field{{Name: "posting_date", Column: "posting_date", Type: "date"}}}

// reconEntrySelect is the lines on $6 the account of $2 the ledger and $3
// the compared ledger whose entry has no copy in the other: an entry
// without a posting group (posted before T52b) has none. The copies,
// in their entry's year, are found by the posting group index. A ledger
// compared with itself has none, as it has no difference.
const reconEntrySelect = `SELECT * FROM (SELECT md5(l.journal_entry_id::text || '/' || l.line_no)::uuid AS id, l.ledger_id, l.journal_entry_id,
		l.fiscal_year, l.line_no, e.document_number, l.posting_date, l.fiscal_period, e.currency_tc, c.exponent, l.amount_tc, l.amount_lc, l.amount_gc
	FROM ledger.journal_line l
	JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
	JOIN ref.currency c ON c.code = e.currency_tc
	WHERE $2::uuid <> $3 AND l.legal_entity_id = $1 AND l.fiscal_year = $4 AND l.account_id = $6 AND l.ledger_id IN ($2, $3)
		AND ($5::smallint IS NULL OR l.fiscal_period = $5) AND ($7::text IS NULL OR e.currency_tc = $7)
		AND NOT EXISTS (SELECT FROM ledger.journal_entry o WHERE o.tenant_id = e.tenant_id AND o.fiscal_year = e.fiscal_year AND o.posting_group_id = e.posting_group_id
			AND o.ledger_id = CASE WHEN e.ledger_id = $2 THEN $3::uuid ELSE $2 END)) t WHERE true`

// reconEntry is a journal line of an entry in one of the two ledgers only.
type reconEntry struct {
	ID             uuid.UUID // derived from entry and line, for paging
	LedgerID       uuid.UUID
	JournalEntryID uuid.UUID
	FiscalYear     int16
	LineNo         int32
	DocumentNumber string
	PostingDate    time.Time
	FiscalPeriod   int16
	Currency       string
	Exp            int
	TC, LC, GC     money.Minor
}

// ListLedgerReconciliationEntries implements GET
// /v1/ledger-reconciliation/entries: the drill-down from a difference to
// the ledger-specific lines behind it (ADR-0045, T52e).
func (e *Endpoints) ListLedgerReconciliationEntries(ctx context.Context, params gen.ListLedgerReconciliationEntriesParams) (gen.ListLedgerReconciliationEntriesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	period, cur := apitypes.Ptr[int32](params.FiscalPeriod), apitypes.Ptr[string](params.Currency)
	scope := fmt.Sprintf("listLedgerReconciliationEntries|%s|%v|%s|%d|%v|%s|%v", params.LegalEntityID, params.LedgerID,
		params.CompareLedgerID, params.FiscalYear, params.FiscalPeriod, params.AccountID, params.Currency)
	ks, err := e.Pages.Keyset(reconEntrySort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	page := gen.LedgerReconciliationEntryPage{Items: []gen.LedgerReconciliationEntry{}}
	r, ok, err := e.reconcile(ctx, tx, params.LegalEntityID, apitypes.Ptr[uuid.UUID](params.LedgerID), params.CompareLedgerID)
	if err != nil || !ok {
		return &page, err
	}
	q, args := ks.SQL(reconEntrySelect, []any{r.le.ID, r.ledger, r.compare, params.FiscalYear, period, params.AccountID, cur})
	rows, _ := tx.Query(ctx, q, args...)
	lines, err := pgx.CollectRows(rows, pgx.RowToStructByPos[reconEntry])
	if err != nil {
		return nil, fmt.Errorf("ledger: reconciliation entries %s: %s with %s: %w", r.le.ID, r.ledger, r.compare, err)
	}
	lines, next := pagination.Page(ks, lines, func(l reconEntry) (string, uuid.UUID) { return l.PostingDate.Format(time.DateOnly), l.ID })
	lc, gc := r.exps[r.le.LocalCurrency], r.exps[r.group]
	for _, l := range lines {
		page.Items = append(page.Items, gen.LedgerReconciliationEntry{LedgerID: l.LedgerID, JournalEntryID: l.JournalEntryID, FiscalYear: int32(l.FiscalYear),
			LineNo: l.LineNo, DocumentNumber: l.DocumentNumber, PostingDate: l.PostingDate, FiscalPeriod: int32(l.FiscalPeriod),
			Amount: apitypes.Money(l.TC, l.Currency, l.Exp), AmountLc: apitypes.Money(l.LC, r.le.LocalCurrency, lc), AmountGc: apitypes.Money(l.GC, r.group, gc)})
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &page, nil
}
