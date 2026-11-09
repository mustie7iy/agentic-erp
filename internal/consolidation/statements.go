package consolidation

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The consolidated statements of a run (ADR-0059 §11).
const (
	BalanceSheet  = "balance_sheet"
	ProfitAndLoss = "profit_and_loss"
)

// statementTypes are the group account types each statement shows.
var statementTypes = map[string][]config.AccountType{
	BalanceSheet:  {config.Asset, config.Liability, config.Equity},
	ProfitAndLoss: {config.Revenue, config.Expense},
}

// statementCursor is where a page of a statement ends: by group account
// number.
type statementCursor struct {
	Number string `json:"n"`
}

// columns are a statement row's or total's amounts in the group
// currency's minor units, debit positive: the companies' translated rows
// summed and the run's eliminations. A run has no eliminations until
// T68f and T68h build its entries (ADR-0059 §8, §9), so eliminations
// stays zero until then.
type columns struct {
	reported, eliminations money.Minor
}

// add sums two columns.
func (f columns) add(g columns) columns {
	return columns{reported: f.reported + g.reported, eliminations: f.eliminations + g.eliminations}
}

// statementRun reports whether a run has figures a statement shows: once
// computed, and final or superseded after (ADR-0059 §11).
func statementRun(status string) bool {
	return status == RunComputed || status == RunFinal || status == RunSuperseded
}

// GetConsolidatedStatement implements GET
// /v1/consolidation-runs/{id}/statements/{statement} (ADR-0059 §11): the
// run's balance sheet or profit and loss, one row per group account of
// the statement's types in the version the run read, by number, with the
// totals over the whole statement. It reads the run's stored rows alone
// (§14).
func (e *Endpoints) GetConsolidatedStatement(ctx context.Context, params gen.GetConsolidatedStatementParams) (gen.GetConsolidatedStatementRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	types, ok := statementTypes[params.Statement]
	if !ok {
		return nil, problem.NotFound("There is no consolidated statement " + params.Statement + "; there are balance_sheet and profit_and_loss.")
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	r, err := readRun(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	if !statementRun(r.Status) {
		return nil, problem.Conflict("consolidation.run_not_computed", fmt.Sprintf("Run %s is %s; a statement is of a computed, final or superseded run.",
			r.ID, r.Status))
	}
	scope := "getConsolidatedStatement|" + r.ID.String() + "|" + params.Statement
	var after statementCursor
	if params.PageToken.Set {
		if err := e.Pages.Decode(scope, params.PageToken.Value, &after); err != nil {
			return nil, err
		}
	}
	doc, err := e.version(ctx, r.ConfigVersion)
	if err != nil {
		return nil, fmt.Errorf("consolidation: configuration version %d of run %s: %w", r.ConfigVersion, r.ID, err)
	}
	sums, err := accountSums(ctx, tx, r)
	if err != nil {
		return nil, err
	}
	exp, err := currency.Exponent(ctx, tx, r.GroupCurrency, nil)
	if err != nil {
		return nil, fmt.Errorf("consolidation: currency %s: %w", r.GroupCurrency, err)
	}
	// The run's configuration version never changes and was activated
	// with its group (validation), so Group is never nil, as runAPI reads it.
	s := statement{chart: doc.Consolidation, sums: sums, group: r.GroupCurrency, exp: exp}
	out := gen.ConsolidatedStatement{RunID: r.ID, GroupCode: r.GroupCode, GroupName: doc.Consolidation.Group(r.GroupCode).Name, FiscalYear: int32(r.FiscalYear),
		Period: int32(r.Period), PeriodEnd: r.PeriodEnd, RunStatus: r.Status, Statement: params.Statement, GroupCurrency: r.GroupCurrency,
		Items: []gen.ConsolidatedStatementRow{}, Totals: s.totals(params.Statement)}
	for _, a := range s.accounts(types) {
		if params.PageToken.Set && a.Number <= after.Number {
			continue
		}
		if len(out.Items) == size {
			tok, _ := e.Pages.Encode(scope, statementCursor{Number: out.Items[size-1].Number}) // a string always encodes
			out.NextPageToken.SetTo(tok)
			break
		}
		out.Items = append(out.Items, s.row(a))
	}
	return &out, nil
}

// accountSums are a run's translated rows summed per group account.
func accountSums(ctx context.Context, tx pgx.Tx, r Run) (map[string]columns, error) {
	rows, _ := tx.Query(ctx, `SELECT group_account, sum(amount_gc)::bigint FROM consolidation.run_balance WHERE run_id = $1 GROUP BY group_account`, r.ID)
	out := map[string]columns{}
	var account string
	var sum money.Minor
	_, err := pgx.ForEachRow(rows, []any{&account, &sum}, func() error {
		out[account] = columns{reported: sum}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("consolidation: statement figures of run %s: %w", r.ID, err)
	}
	return out, nil
}

// statement is what a statement is shown from: the group chart in the
// run's version, its figures per group account and the group currency
// with its minor units.
type statement struct {
	chart config.Consolidation
	sums  map[string]columns
	group string
	exp   int
}

// accounts are the group chart's accounts of the types, by number.
func (s statement) accounts(types []config.AccountType) []config.GroupAccount {
	var out []config.GroupAccount
	for _, a := range s.chart.Accounts {
		if slices.Contains(types, a.Type) {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b config.GroupAccount) int { return strings.Compare(a.Number, b.Number) })
	return out
}

// sum is the figures of the group chart's accounts of the types summed.
func (s statement) sum(types ...config.AccountType) columns {
	var f columns
	for _, a := range s.accounts(types) {
		f = f.add(s.sums[a.Number])
	}
	return f
}

// money is an amount in the group currency.
func (s statement) money(m money.Minor) gen.Money {
	return gen.Money{Amount: m.Plain(s.exp), Currency: s.group}
}

// figuresAPI shows a total's figures, the consolidated their sum.
func (s statement) figuresAPI(f columns) gen.ConsolidatedFigures {
	return gen.ConsolidatedFigures{Reported: s.money(f.reported), Eliminations: s.money(f.eliminations), Consolidated: s.money(f.reported + f.eliminations)}
}

// row shows a group account's row.
func (s statement) row(a config.GroupAccount) gen.ConsolidatedStatementRow {
	f := s.figuresAPI(s.sums[a.Number])
	out := gen.ConsolidatedStatementRow{Number: a.Number, Name: a.Name, Type: string(a.Type), Reported: f.Reported, Eliminations: f.Eliminations,
		Consolidated: f.Consolidated}
	if a.Role != "" {
		out.Role = gen.NewOptString(a.Role)
	}
	return out
}

// totals are the statement's totals over every group account: the
// balance sheet's assets, liabilities and equity, the profit and loss's
// revenue and expenses, and on both the fiscal year's profit through the
// period, which closes the balance sheet to zero.
func (s statement) totals(statement string) gen.ConsolidatedStatementTotals {
	out := gen.ConsolidatedStatementTotals{Profit: s.figuresAPI(s.sum(config.Revenue, config.Expense))}
	if statement == BalanceSheet {
		out.Assets = gen.NewOptConsolidatedFigures(s.figuresAPI(s.sum(config.Asset)))
		out.Liabilities = gen.NewOptConsolidatedFigures(s.figuresAPI(s.sum(config.Liability)))
		out.Equity = gen.NewOptConsolidatedFigures(s.figuresAPI(s.sum(config.Equity)))
		return out
	}
	out.Revenue = gen.NewOptConsolidatedFigures(s.figuresAPI(s.sum(config.Revenue)))
	out.Expenses = gen.NewOptConsolidatedFigures(s.figuresAPI(s.sum(config.Expense)))
	return out
}
