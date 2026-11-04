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

// CashActuals reads a legal entity's cash actuals in the context's
// transaction, without a company access check: the run reads every
// company of its tenant (the cash context's ExtractActualShares).
type CashActuals interface {
	ExtractActualShares(ctx context.Context, legalEntity uuid.UUID) ([]CashShare, error)
}

// ExtractActor is the principal the run asks the cash context as, in every
// tenant: a system actor, never a token holder.
var ExtractActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:analytics-extract"))

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
	type company struct {
		ID   uuid.UUID
		Code string
	}
	rows, _ := tx.Query(ctx, cashCompaniesSQL)
	companies, err := pgx.CollectRows(rows, pgx.RowToStructByPos[company])
	if err != nil {
		return fmt.Errorf("%s: companies: %w", cashActualsDataset, err)
	}
	ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), reqctx.Principal{ID: ExtractActor, Kind: reqctx.KindSystem, TenantID: r.tenant})
	var all []cashActual
	for _, co := range companies {
		shares, err := r.x.Cash.ExtractActualShares(ctx, co.ID)
		if err != nil {
			return fmt.Errorf("%s of %s: %w", cashActualsDataset, co.Code, err)
		}
		for _, sh := range shares {
			all = append(all, cashRow(r.tenant, co.Code, sh))
		}
	}
	slices.SortFunc(all, func(a, b cashActual) int {
		return cmp.Or(cmp.Compare(a.ValueDate, b.ValueDate), strings.Compare(a.Company, b.Company), bytes.Compare(a.StatementID[:], b.StatementID[:]),
			cmp.Compare(a.LineNo, b.LineNo), strings.Compare(deref(a.LiquidityItem), deref(b.LiquidityItem)))
	})
	months, done := snapshotOf[cashActual](ctx, r, cashActualsDataset)
	for _, a := range all {
		if err = months.add(time.Unix(int64(a.ValueDate)*86400, 0).UTC().Format(monthLayout), a); err != nil {
			break
		}
	}
	return done(err)
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

// deref is a nullable string's value, "" for null.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
