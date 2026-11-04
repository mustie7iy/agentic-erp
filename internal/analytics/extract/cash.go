package extract

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The cash actuals (dataset cash_actuals v1, ADR-0051 §6, §11; T53j1): each
// bank statement line's share of a liquidity item. Unlike the other
// datasets they are not rows of a table: the cash context classifies the
// lines from the active configuration's derivation rules and what each
// line's decided match clears, so the run asks it, through CashActuals, in
// the run's own transaction, and copies no classification into SQL.

const cashActualsDataset = "cash_actuals"

// CashShare is a statement line's share of a liquidity item as the cash
// context classifies it: the line by its statement and number, its value
// date, its house bank's code, its currency and that currency's exponent,
// what classified it, the item's code and name ("" for no item) and the
// signed amount in minor units.
type CashShare struct {
	StatementID   uuid.UUID
	LineNo        int32
	ValueDate     time.Time
	HouseBankCode string
	Currency      string
	Exponent      int
	Match         string
	Item          string
	ItemName      string
	Amount        money.Minor
}

// Cash reads a legal entity's cash actuals, forecast and the accuracy's
// cells in the context's transaction, without a company access check: the
// run reads every company of its tenant (the cash context's
// ExtractActualShares, ExtractForecastShares and ExtractAccuracyCells).
type Cash interface {
	ExtractActualShares(ctx context.Context, legalEntity uuid.UUID) ([]CashShare, error)
	ExtractForecastShares(ctx context.Context, legalEntity uuid.UUID) ([]ForecastShare, error)
	ExtractAccuracyCells(ctx context.Context, legalEntity uuid.UUID) ([]AccuracyCell, error)
}

// ExtractActor is the principal the run asks the cash context as, in every
// tenant: a system actor, never a token holder.
func ExtractActor() uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:analytics-extract"))
}

// cashActual is a row of dataset cash_actuals v1
// (analytics/datasets/v1/cash_actuals.yaml).
type cashActual struct {
	TenantID          uuid.UUID   `parquet:"tenant_id,uuid"`
	StatementID       uuid.UUID   `parquet:"statement_id,uuid"`
	LineNo            int32       `parquet:"line_no"`
	Company           string      `parquet:"company"`
	ValueDate         int32       `parquet:"value_date,date"`
	HouseBank         string      `parquet:"house_bank"`
	Match             string      `parquet:"match"`
	LiquidityItem     *string     `parquet:"liquidity_item,optional"`
	LiquidityItemName *string     `parquet:"liquidity_item_name,optional"`
	AmountMinor       money.Minor `parquet:"amount_minor"`
	Currency          string      `parquet:"currency"`
	Exponent          int32       `parquet:"exponent"`
}

// cashCompaniesSQL lists the legal entities that have bank statements, by
// company code: the others have no actuals to classify.
const cashCompaniesSQL = `SELECT le.id, le.code FROM tenancy.legal_entity le
	WHERE EXISTS (SELECT FROM payments.bank_statement s WHERE s.tenant_id = le.tenant_id AND s.legal_entity_id = le.id) ORDER BY le.code`

// cashActuals reads the cash actuals of every legal entity with
// statements, ordered by value date, company, statement, line and item,
// one file per month of value dates. Without a port it reads none.
func (r *run) cashActuals(ctx context.Context, tx pgx.Tx) error {
	if r.x.Cash == nil {
		return nil
	}
	all, err := cashOf(ctx, tx, r, cashActualsDataset, cashCompaniesSQL, r.x.Cash.ExtractActualShares)
	if err != nil {
		return err
	}
	slices.SortFunc(all, func(a, b ofCompany[CashShare]) int {
		return cmp.Or(a.v.ValueDate.Compare(b.v.ValueDate), strings.Compare(a.company, b.company), bytes.Compare(a.v.StatementID[:], b.v.StatementID[:]),
			cmp.Compare(a.v.LineNo, b.v.LineNo), strings.Compare(a.v.Item, b.v.Item))
	})
	months, done := snapshotOf[cashActual](ctx, r, cashActualsDataset)
	for _, sh := range all {
		if err = months.add(sh.v.ValueDate.Format(monthLayout), cashRow(r.tenant, sh.company, sh.v)); err != nil {
			break
		}
	}
	return done(err)
}

// ofCompany is what the cash context gave of a company, with its code.
type ofCompany[T any] struct {
	company string
	v       T
}

// cashOf asks the cash context, as ExtractActor in the run's transaction,
// for what read gives of each legal entity companiesSQL lists (its id and
// company code), each with its company's code.
func cashOf[T any](ctx context.Context, tx pgx.Tx, r *run, dataset, companiesSQL string, read func(context.Context, uuid.UUID) ([]T, error)) ([]ofCompany[T], error) {
	type company struct {
		ID   uuid.UUID
		Code string
	}
	rows, _ := tx.Query(ctx, companiesSQL)
	companies, err := pgx.CollectRows(rows, pgx.RowToStructByPos[company])
	if err != nil {
		return nil, fmt.Errorf("%s: companies: %w", dataset, err)
	}
	ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), reqctx.Principal{ID: ExtractActor(), Kind: reqctx.KindSystem, TenantID: r.tenant})
	var all []ofCompany[T]
	for _, co := range companies {
		vs, err := read(ctx, co.ID)
		if err != nil {
			return nil, fmt.Errorf("%s of %s: %w", dataset, co.Code, err)
		}
		for _, v := range vs {
			all = append(all, ofCompany[T]{co.Code, v})
		}
	}
	return all, nil
}

// cashRow is a share as the dataset holds it: a share of no item has a
// null item and name.
func cashRow(tenant uuid.UUID, company string, sh CashShare) cashActual {
	a := cashActual{TenantID: tenant, StatementID: sh.StatementID, LineNo: sh.LineNo, Company: company, ValueDate: days(sh.ValueDate),
		HouseBank: sh.HouseBankCode, Match: sh.Match, AmountMinor: sh.Amount, Currency: sh.Currency, Exponent: int32(sh.Exponent)} //nolint:gosec // a currency's exponent is 0 to 4 (ref.currency)
	if sh.Item != "" {
		item, name := sh.Item, sh.ItemName
		a.LiquidityItem, a.LiquidityItemName = &item, &name
	}
	return a
}
