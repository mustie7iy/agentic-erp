// Package credit is the credit management context (ADR-0066;
// 02-architecture §2.2, 03-data-model §3.28): a customer's credit limit
// in a company, one amount in the company's currency, and the check of an
// amount against it. What the customer owes is not kept here: the
// exposure is the customer's open items, read from the ledger, and its
// confirmed sales orders not yet billed, read from the sales context,
// when it is asked for (ADR-0066 §3, As built T77b). The check answers
// and does nothing else: it changes nothing and blocks nothing
// (ADR-0066 §4); the sales context asks it when an order is confirmed
// and blocks the order itself (Check).
//
// The context is a leaf (ADR-0066 §1): it reads the ledger's customer
// open items, the partner context's customers, the sales context's open
// orders and tenancy through the ports below, posts nothing, and no
// context imports it.
package credit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func init() {
	// A customer's second limit in a company, whether it is found by a
	// request after the first or by one racing it (ADR-0066 §2).
	problem.RegisterConstraint("credit_limit_once", func() *problem.Problem {
		return problem.Conflict("credit.limit_exists", "The customer already has a credit limit in this company; change that one.")
	})
}

// LegalEntities is what credit reads about legal entities (tenancy
// context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// OpenItems reads a company's open customer items of the customers named
// (ledger context, T75b): every currency, debit positive, each with its
// amount in the company's currency as booked. Company access is the
// caller's to check.
type OpenItems interface {
	Receivable(ctx context.Context, legalEntity uuid.UUID, customers []uuid.UUID) ([]ledger.ReceivableItem, error)
}

// Customers says what a business partner is as a customer of a legal
// entity: whether it plays the role and has customer data there (partner
// context, T74a).
type Customers interface {
	Customer(ctx context.Context, partnerID, legalEntity uuid.UUID) (partner.Customer, bool, error)
}

// Partners names business partners by number and name (partner context).
type Partners interface {
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.Named, error)
}

// OpenOrders reads what a customer's confirmed sales orders not yet
// billed come to in a company (sales context, T77b): how many orders
// have a part not yet posted as a customer invoice, those blocked for
// credit left out, and those parts' gross value, net plus output tax, in
// the company's currency. The sales context owns the orders and their
// valuation; the port takes and gives plain values, so credit imports
// nothing of it. Company access is the caller's to check.
type OpenOrders interface {
	OpenOrders(ctx context.Context, legalEntity, customer uuid.UUID) (orders int32, gross money.Minor, err error)
}

// Endpoints implements the credit part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	OpenItems     OpenItems
	Customers     Customers
	Partners      Partners
	Orders        OpenOrders
}

// Limit is a row of credit.credit_limit: the most a customer may owe a
// company. Amount is minor units of Currency, the company's, whose
// minor-unit digits are Exponent.
type Limit struct {
	ID             uuid.UUID
	LegalEntityID  uuid.UUID
	CustomerID     uuid.UUID
	CustomerNumber string
	Currency       string
	Exponent       int
	Amount         money.Minor
	Version        int32
	CreatedAt      time.Time
	CreatedBy      uuid.UUID
	UpdatedAt      time.Time
	UpdatedBy      uuid.UUID
}

const limitSelect = `SELECT l.id, l.legal_entity_id, l.customer_id, l.customer_number, l.currency, c.exponent, l.amount,
		l.version, l.created_at, l.created_by, l.updated_at, l.updated_by
	FROM credit.credit_limit l JOIN ref.currency c ON c.code = l.currency`

func scanLimit(row pgx.Row) (Limit, error) {
	var l Limit
	err := row.Scan(&l.ID, &l.LegalEntityID, &l.CustomerID, &l.CustomerNumber, &l.Currency, &l.Exponent, &l.Amount,
		&l.Version, &l.CreatedAt, &l.CreatedBy, &l.UpdatedAt, &l.UpdatedBy)
	return l, errs.Wrapf(err, "scan credit limit")
}

// limit reads a credit limit of a legal entity the caller may act in,
// locked for a change when forUpdate; another's is not found, so ids
// cannot be probed (04 §4.9).
func limit(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Limit, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Limit{}, fmt.Errorf("credit: company access for credit limit %s: %w", id, err)
	}
	q := limitSelect + ` WHERE l.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF l`
	}
	l, err := scanLimit(tx.QueryRow(ctx, q, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(l.LegalEntityID)):
		return Limit{}, problem.NotFoundOf("credit limit")
	case err != nil:
		return Limit{}, fmt.Errorf("credit: read credit limit %s: %w", id, err)
	}
	return l, nil
}

// GetLimit returns a credit limit of a legal entity the caller may act
// in.
func GetLimit(ctx context.Context, id uuid.UUID) (Limit, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Limit{}, err
	}
	return limit(ctx, tx, id, false)
}

// limitOf reads the customer's limit in the legal entity, held under its
// row lock until the transaction ends when forUpdate; ok is false when
// none is set.
func limitOf(ctx context.Context, tx pgx.Tx, legalEntity, customer uuid.UUID, forUpdate bool) (Limit, bool, error) {
	q := limitSelect + ` WHERE l.legal_entity_id = $1 AND l.customer_id = $2`
	if forUpdate {
		q += ` FOR UPDATE OF l`
	}
	l, err := scanLimit(tx.QueryRow(ctx, q, legalEntity, customer))
	if errors.Is(err, pgx.ErrNoRows) {
		return Limit{}, false, nil
	}
	if err != nil {
		return Limit{}, false, fmt.Errorf("credit: read the credit limit of customer %s in legal entity %s: %w", customer, legalEntity, err)
	}
	return l, true, nil
}

// hold takes the customer's key in the company until the transaction
// ends (ADR-0008): the checks of a customer's orders and the setting of
// its limit run one after the other. The limit's row cannot serve while
// there is none: a check that found no limit would not stop one being
// set beside it, and the next order would be checked against an
// exposure that does not hold the first yet.
func hold(ctx context.Context, tx pgx.Tx, legalEntity, customer uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('credit.customer:' || platform.require_tenant() || ':' || $1::text
		|| ':' || $2::text, 0))`, legalEntity, customer); err != nil {
		return fmt.Errorf("credit: hold customer %s in legal entity %s: %w", customer, legalEntity, err)
	}
	return nil
}

// errTooLarge refuses an exposure whose amounts are out of range.
func errTooLarge() *problem.Problem {
	return problem.Unprocessable("credit.amount_too_large", "The customer's open items, its open orders and the amount add up to more than an amount can hold.")
}

// amountOf reads an amount a request gives in the currency code with exp
// minor-unit digits: zero or more. field is the request's field of the
// amount.
func amountOf(value, code string, exp int, field string) (money.Minor, error) {
	m, err := currency.ParseAmount(value, code, exp, field)
	if err != nil {
		return 0, err
	}
	if m < 0 {
		return 0, problem.Invalid("credit.invalid_amount", field, "The amount is zero or more.")
	}
	return m, nil
}
