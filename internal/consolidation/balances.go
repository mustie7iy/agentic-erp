package consolidation

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// balanceCursor is where a page of a run's translated balances ends: by
// company code and line (ADR-0059 §5).
type balanceCursor struct {
	Code string `json:"c"`
	Line int32  `json:"l"`
}

// storedBalance is a translated row as stored, with its company's code,
// local currency and closing rate.
type storedBalance struct {
	legalEntity        uuid.UUID
	code               string
	line               int32
	groupAccount, kind string
	account, partner   *uuid.UUID
	lc, booked         *money.Minor
	gc                 money.Minor
	currency, rate     string
}

// balanceFilter is the filter of a run's translated balances ($2-$6 of
// balanceWhere).
func balanceFilter(params gen.ListConsolidationBalancesParams) []any {
	return []any{apitypes.Ptr[uuid.UUID](params.LegalEntityID), params.GroupAccount.Value, apitypes.Ptr[uuid.UUID](params.AccountID),
		apitypes.Ptr[uuid.UUID](params.TradingPartnerID), params.Kind.Value}
}

// balanceWhere selects a run's ($1) translated balances by the filter.
const balanceWhere = `FROM consolidation.run_balance b
	JOIN consolidation.run_member m ON m.tenant_id = b.tenant_id AND m.run_id = b.run_id AND m.legal_entity_id = b.legal_entity_id
	WHERE b.run_id = $1 AND ($2::uuid IS NULL OR b.legal_entity_id = $2) AND ($3 = '' OR b.group_account = $3)
		AND ($4::uuid IS NULL OR b.account_id = $4) AND ($5::uuid IS NULL OR b.trading_partner_id = $5) AND ($6 = '' OR b.kind = $6)`

// ListConsolidationBalances implements GET
// /v1/consolidation-runs/{id}/balances (ADR-0059 §7): a run's translated
// rows by company code and line, filtered, with the totals over the
// filter, each named: the company, the group account in the version the
// run read, the company account and the trading partner.
func (e *Endpoints) ListConsolidationBalances(ctx context.Context, params gen.ListConsolidationBalancesParams) (gen.ListConsolidationBalancesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	r, err := readRun(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	filter := balanceFilter(params)
	scope := fmt.Sprintf("listConsolidationBalances|%s|%t %s|%s|%t %s|%t %s|%s", r.ID, params.LegalEntityID.Set, params.LegalEntityID.Value,
		params.GroupAccount.Value, params.AccountID.Set, params.AccountID.Value, params.TradingPartnerID.Set, params.TradingPartnerID.Value, params.Kind.Value)
	after := balanceCursor{}
	if params.PageToken.Set {
		if err := e.Pages.Decode(scope, params.PageToken.Value, &after); err != nil {
			return nil, err
		}
	}
	args := append(append([]any{r.ID}, filter...), after.Code, after.Line, size+1)
	rows, _ := tx.Query(ctx, `SELECT b.legal_entity_id, m.company_code, b.line_no, b.group_account, b.kind, b.account_id, b.trading_partner_id,
			b.amount_lc, b.amount_gc_booked, b.amount_gc, m.local_currency, m.closing_rate::text `+balanceWhere+`
			AND (m.company_code, b.line_no) > ($7, $8)
		ORDER BY m.company_code, b.line_no LIMIT $9`, args...)
	page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedBalance, error) {
		var s storedBalance
		return s, row.Scan(&s.legalEntity, &s.code, &s.line, &s.groupAccount, &s.kind, &s.account, &s.partner, &s.lc, &s.booked, &s.gc, &s.currency,
			&s.rate)
	})
	if err != nil {
		return nil, fmt.Errorf("consolidation: translated balances of run %s: %w", r.ID, err)
	}
	out := gen.ConsolidationBalancePage{Items: []gen.ConsolidationBalance{}}
	if len(page) > size {
		page = page[:size]
		tok, _ := e.Pages.Encode(scope, balanceCursor{Code: page[size-1].code, Line: page[size-1].line}) // a string and a number always encode
		out.NextPageToken.SetTo(tok)
	}
	n, err := e.namesOf(ctx, tx, r, page)
	if err != nil {
		return nil, err
	}
	for _, s := range page {
		out.Items = append(out.Items, n.balanceAPI(s))
	}
	out.Totals, err = n.totals(ctx, tx, r, filter)
	return &out, err
}

// names are what a page of translated balances is shown with: the group
// chart in the run's version, the companies by id, the accounts of the
// page's companies and the currencies' minor units.
type names struct {
	chart     config.Consolidation
	companies map[uuid.UUID]Company
	accounts  map[uuid.UUID]Account
	exps      map[string]int
	group     string
}

// namesOf reads the names of a page of a run's translated balances.
func (e *Endpoints) namesOf(ctx context.Context, tx pgx.Tx, r Run, page []storedBalance) (names, error) {
	doc, err := e.Settings.Version(ctx, r.ConfigVersion)
	if err != nil {
		return names{}, fmt.Errorf("consolidation: configuration version %d of run %s: %w", r.ConfigVersion, r.ID, err)
	}
	n := names{chart: doc.Consolidation, companies: map[uuid.UUID]Company{}, accounts: map[uuid.UUID]Account{}, exps: map[string]int{},
		group: r.GroupCurrency}
	entities, err := e.Companies.Entities(ctx)
	if err != nil {
		return names{}, fmt.Errorf("consolidation: companies: %w", err)
	}
	for _, c := range entities {
		n.companies[c.ID] = c
	}
	rows, _ := tx.Query(ctx, `SELECT DISTINCT local_currency FROM consolidation.run_member WHERE run_id = $1 ORDER BY 1`, r.ID)
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return names{}, fmt.Errorf("consolidation: currencies of run %s: %w", r.ID, err)
	}
	for _, code := range append(codes, r.GroupCurrency) {
		if n.exps[code], err = currency.Exponent(ctx, tx, code, nil); err != nil {
			return names{}, fmt.Errorf("consolidation: currency %s: %w", code, err)
		}
	}
	read := map[uuid.UUID]bool{}
	for _, s := range page {
		if read[s.legalEntity] {
			continue
		}
		read[s.legalEntity] = true
		accounts, err := e.Ledger.Accounts(ctx, s.legalEntity)
		if err != nil {
			return names{}, fmt.Errorf("consolidation: accounts of %s: %w", s.code, err)
		}
		for _, a := range accounts {
			n.accounts[a.ID] = a
		}
	}
	return n, nil
}

// money is an amount in the group currency or a run member's local
// currency, whose minor units namesOf read.
func (n names) money(m money.Minor, code string) gen.Money {
	return gen.Money{Amount: m.Plain(n.exps[code]), Currency: code}
}

// balanceAPI shows a translated row with its names.
func (n names) balanceAPI(s storedBalance) gen.ConsolidationBalance {
	co := n.companies[s.legalEntity]
	out := gen.ConsolidationBalance{LegalEntityID: s.legalEntity, CompanyCode: s.code, CompanyName: co.Name, LineNo: s.line, Kind: s.kind,
		GroupAccount: gen.GroupAccountRef{Number: s.groupAccount}, AccountID: apitypes.OptUUID(s.account), AmountGc: n.money(s.gc, n.group)}
	if g := n.chart.GroupAccount(s.groupAccount); g != nil {
		out.GroupAccount.Name = g.Name
	}
	if s.account != nil {
		a := n.accounts[*s.account]
		out.Account = gen.NewOptAssetAccountRef(gen.AssetAccountRef{Number: a.Number, Name: a.Name})
		if a.Type == config.Asset || a.Type == config.Liability {
			out.ClosingRate = gen.NewOptString(plainRate(s.rate))
		}
	}
	if s.partner != nil {
		p := n.companies[*s.partner]
		out.TradingPartner = gen.NewOptTradingPartner(gen.TradingPartner{ID: *s.partner, Code: p.Code, Name: p.Name})
	}
	if s.lc != nil {
		out.AmountLc, out.AmountGcBooked = gen.NewOptMoney(n.money(*s.lc, s.currency)), gen.NewOptMoney(n.money(*s.booked, n.group))
	}
	return out
}

// totals are the sums over every row of the run the filter selects: the
// local amounts per local currency by code, and the booked and translated
// group amounts.
func (n names) totals(ctx context.Context, tx pgx.Tx, r Run, filter []any) (gen.ConsolidationBalanceTotals, error) {
	rows, _ := tx.Query(ctx, `SELECT m.local_currency, count(b.amount_lc) > 0, coalesce(sum(b.amount_lc), 0)::bigint, coalesce(sum(b.amount_gc_booked), 0)::bigint,
			coalesce(sum(b.amount_gc), 0)::bigint `+balanceWhere+`
		GROUP BY m.local_currency ORDER BY m.local_currency`, append([]any{r.ID}, filter...)...)
	type total struct {
		code           string
		local          bool // some row has a local amount
		lc, booked, gc money.Minor
	}
	sums, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (total, error) {
		var t total
		return t, row.Scan(&t.code, &t.local, &t.lc, &t.booked, &t.gc)
	})
	if err != nil {
		return gen.ConsolidationBalanceTotals{}, fmt.Errorf("consolidation: totals of run %s: %w", r.ID, err)
	}
	out := gen.ConsolidationBalanceTotals{Local: []gen.Money{}}
	var booked, gc money.Minor
	for _, t := range sums {
		if t.local {
			out.Local = append(out.Local, n.money(t.lc, t.code))
		}
		booked, gc = booked+t.booked, gc+t.gc
	}
	out.GroupBooked, out.Group = n.money(booked, n.group), n.money(gc, n.group)
	return out, nil
}
