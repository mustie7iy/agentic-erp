package cash

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The actuals as the analytical store holds them (ADR-0044, ADR-0051 §6,
// T53j1): each share of a legal entity's statement lines by liquidity
// item, of every value date, classified as the cash actuals classify them
// (§11). The store's cash_actuals dataset and the semantic layer's
// database path both read them here, so the classification has one
// mechanism, this context's; the analytics context reaches them through
// ports it declares, which the composition root wires, and imports
// nothing of cash.

// ActualShare is a statement line's share of a liquidity item: the line
// by its statement and number, its value date, its house bank's code (a
// bank is never shown by its account here, §15), its currency and that
// currency's exponent, what classified it (MatchPayment, MatchPostingRule,
// MatchIncoming, MatchExcluded or MatchNone), the item's code and name (""
// for a share of no item) and the share's signed amount: negative is money
// out.
type ActualShare struct {
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

// The range of value dates the shares cover: every one a date can hold.
var (
	firstValueDate = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	lastValueDate  = time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)
)

// ActualShares are the shares of every statement line of the legal entity,
// read as the caller in a consistent read of their own (Consistent): one
// database state, as /v1/cash-actuals reads it (x-snapshot). It is the
// semantic layer's database path, whose endpoint has checked
// cash.forecast.read. A legal entity the caller cannot act in is not
// found.
func (e *Endpoints) ActualShares(ctx context.Context, legalEntity uuid.UUID) ([]ActualShare, error) {
	return asCaller(ctx, e, legalEntity, e.shares)
}

// ExtractActualShares are the shares of every statement line of the legal
// entity, read in the context's transaction: the analytical store's
// extract, which reads every company of its tenant, as a system principal
// under the tenant's row-level security, in its one REPEATABLE READ
// transaction (ADR-0044). A legal entity the directory does not know has
// none.
func (e *Endpoints) ExtractActualShares(ctx context.Context, legalEntity uuid.UUID) ([]ActualShare, error) {
	return asExtract(ctx, e, legalEntity, e.shares)
}

// asCaller is what read gives of the legal entity, read as the caller in a
// consistent read of its own (Consistent): the semantic layer's database
// path. A legal entity the caller cannot act in is not found.
func asCaller[T any](ctx context.Context, e *Endpoints, legalEntity uuid.UUID, read func(context.Context, pgx.Tx, Company, uuid.UUID) ([]T, error)) ([]T, error) {
	if _, _, err := reqctx.Request(ctx); err != nil {
		return nil, err
	}
	co, err := e.company(ctx, legalEntity)
	if err != nil {
		return nil, err
	}
	if e.Consistent == nil {
		return nil, errors.New("cash: the analytical store's reads need a consistent read, which the composition root wires")
	}
	rctx, rtx, err := e.Consistent.Read(ctx)
	if err != nil {
		return nil, err
	}
	// Read only: nothing to keep, and the rollback's error changes nothing read.
	defer func() { _ = rtx.Rollback(context.WithoutCancel(ctx)) }()
	return read(rctx, rtx, co, legalEntity)
}

// asExtract is what read gives of the legal entity, read in the context's
// transaction without a company access check: the analytical store's
// extract (ADR-0044). A legal entity the directory does not know has
// nothing.
func asExtract[T any](ctx context.Context, e *Endpoints, legalEntity uuid.UUID, read func(context.Context, pgx.Tx, Company, uuid.UUID) ([]T, error)) ([]T, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	co, ok, err := e.LegalEntities.Company(ctx, legalEntity)
	if err != nil {
		return nil, fmt.Errorf("cash: legal entity %s: %w", legalEntity, err)
	}
	if !ok {
		return nil, nil
	}
	return read(ctx, tx, co, legalEntity)
}

// shares reads, in tx, and classifies the company's statement lines of
// every value date and lists each line's shares, in the order the lines
// are read.
func (e *Endpoints) shares(ctx context.Context, tx pgx.Tx, co Company, legalEntity uuid.UUID) ([]ActualShare, error) {
	ar, err := e.classified(ctx, tx, co, legalEntity, firstValueDate, lastValueDate)
	if err != nil {
		return nil, err
	}
	return ar.shares(ctx)
}

// shares are the classified lines' shares; a classification whose sums
// went out of the range of an amount is refused, never given saturated.
func (ar actualsRead) shares(ctx context.Context) ([]ActualShare, error) {
	if ar.r.sums.overflow {
		return nil, errActualsTooLarge()
	}
	names := map[string]string{}
	for _, it := range ar.r.doc.Cash.LiquidityItems {
		names[it.Code] = it.Name
	}
	exponents := map[string]int{}
	var out []ActualShare
	for _, l := range ar.lines {
		exp, seen := exponents[l.Currency]
		if !seen {
			var err error
			if exp, err = currency.Exponent(ctx, ar.tx, l.Currency, fmt.Errorf("cash: currency %s of a statement is not known", l.Currency)); err != nil {
				return nil, err
			}
			exponents[l.Currency] = exp
		}
		for _, sh := range l.shares {
			out = append(out, ActualShare{StatementID: l.StatementID, LineNo: l.LineNo, ValueDate: l.ValueDate, HouseBankCode: l.bankCode, Currency: l.Currency,
				Exponent: exp, Match: l.Match, Item: sh.Item, ItemName: names[sh.Item], Amount: sh.Amount})
		}
	}
	return out, nil
}
