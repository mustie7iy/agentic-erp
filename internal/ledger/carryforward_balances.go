package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// carriedCursor is where a page of a run's rows ends: by account number
// (byte by byte), currency and kind, then the account's id, which with the
// currency and kind is the row's key in its run: the order is unique even
// if two of a company's accounts shared a number, which only the
// configuration rules out.
type carriedCursor struct {
	Number   string    `json:"n"`
	Currency string    `json:"c"`
	Kind     string    `json:"k"`
	Account  uuid.UUID `json:"a"`
}

// storedRow is a run's row with its account and its currency's minor
// units.
type storedRow struct {
	account        gen.CarryForwardBalanceAccount
	kind, currency string
	exp            int
	tc, lc, gc     money.Minor
}

// ListBalanceCarryForwardBalances implements GET
// /v1/balance-carry-forwards/{id}/balances (ADR-0060 §7): a run's rows by
// account number, currency and kind, each with its account named, and the
// whole run's totals on every page. A run's rows never change, so the
// pages and the totals agree without a snapshot.
func (e *Endpoints) ListBalanceCarryForwardBalances(ctx context.Context, params gen.ListBalanceCarryForwardBalancesParams) (gen.ListBalanceCarryForwardBalancesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	run, err := getCarryForward(ctx, tx, params.ID)
	if err != nil {
		return nil, err
	}
	account := apitypes.Ptr[uuid.UUID](params.AccountID)
	scope := fmt.Sprintf("listBalanceCarryForwardBalances|%s|%v", run.ID, account)
	after := carriedCursor{}
	if params.PageToken.Set {
		if err := e.Pages.Decode(scope, params.PageToken.Value, &after); err != nil {
			return nil, err
		}
	}
	rows, err := carriedPage(ctx, tx, run.ID, account, after, size)
	if err != nil {
		return nil, err
	}
	v, err := e.carryForwardViewOf(ctx, tx, []CarryForwardRun{run})
	if err != nil {
		return nil, err
	}
	lc := v.companies[run.LegalEntityID].LocalCurrency
	f := v.figures(run.ID, lc)
	out := gen.CarryForwardBalancePage{Items: make([]gen.CarryForwardBalance, 0, len(rows)), RunID: run.ID, LocalCurrency: lc, GroupCurrency: v.group,
		Totals: f.totals, ResultLc: f.resultLC, ResultGc: f.resultGC, DebitLc: f.debitLC, CreditLc: f.creditLC, DebitGc: f.debitGC, CreditGc: f.creditGC}
	if len(rows) > size {
		rows = rows[:size]
		last := rows[size-1]
		tok, _ := e.Pages.Encode(scope, carriedCursor{Number: last.account.Number, Currency: last.currency, Kind: last.kind,
			Account: last.account.ID}) // strings and a uuid always encode
		out.NextPageToken.SetTo(tok)
	}
	for _, r := range rows {
		out.Items = append(out.Items, gen.CarryForwardBalance{Account: r.account, Kind: r.kind, Currency: r.currency,
			Amount: apitypes.Money(r.tc, r.currency, r.exp), AmountLc: apitypes.Money(r.lc, lc, v.exps[lc]), AmountGc: apitypes.Money(r.gc, v.group, v.exps[v.group])})
	}
	return &out, nil
}

// carriedPage reads the rows of a run after the cursor, one more than the
// page so the caller can tell whether another follows. The empty cursor
// precedes every row: an account number is never empty.
func carriedPage(ctx context.Context, tx pgx.Tx, run uuid.UUID, account *uuid.UUID, after carriedCursor, size int) ([]storedRow, error) {
	rows, _ := tx.Query(ctx, `SELECT a.id, a.number, a.name, a.account_type, b.kind, b.currency_tc, c.exponent, b.amount_tc, b.amount_lc, b.amount_gc
		FROM ledger.carry_forward_balance b
		JOIN ledger.gl_account a ON a.tenant_id = b.tenant_id AND a.id = b.account_id
		JOIN ref.currency c ON c.code = b.currency_tc
		WHERE b.run_id = $1 AND ($2::uuid IS NULL OR b.account_id = $2)
			AND (a.number COLLATE "C", b.currency_tc COLLATE "C", b.kind COLLATE "C", b.account_id) > ($3 COLLATE "C", $4 COLLATE "C", $5 COLLATE "C", $6)
		ORDER BY a.number COLLATE "C", b.currency_tc COLLATE "C", b.kind COLLATE "C", b.account_id LIMIT $7`,
		pagination.PlanPerCall, run, account, after.Number, after.Currency, after.Kind, after.Account, size+1)
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedRow, error) {
		var s storedRow
		return s, row.Scan(&s.account.ID, &s.account.Number, &s.account.Name, &s.account.Type, &s.kind, &s.currency, &s.exp, &s.tc, &s.lc, &s.gc)
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: rows of carry-forward run %s: %w", run, err)
	}
	return out, nil
}
