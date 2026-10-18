package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CompanySettings are an account's settings in one legal entity.
type CompanySettings struct {
	LegalEntityID     uuid.UUID `json:"legal_entity_id"`
	IsPostingBlocked  bool      `json:"is_posting_blocked"`
	IsOpenItemManaged bool      `json:"is_open_item_managed"`
}

// Account is a general ledger account with its settings in the legal
// entities the caller may act in.
type Account struct {
	ID                 uuid.UUID
	ChartID            uuid.UUID
	Number             string
	Name               string
	AccountType        string
	IsBalanceSheet     bool
	ReconciliationKind string
	Status             string
	Settings           []CompanySettings
	Version            int32
	CreatedAt          time.Time
	CreatedBy          uuid.UUID
	UpdatedAt          time.Time
	UpdatedBy          uuid.UUID
}

// accountSelect takes the caller's company access as $1 (all) and $2 (ids).
const accountSelect = `SELECT a.id, a.chart_id, a.number, a.name, a.account_type, a.is_balance_sheet, a.reconciliation_kind, a.status,
		coalesce((SELECT json_agg(json_build_object('legal_entity_id', ca.legal_entity_id, 'is_posting_blocked', ca.is_posting_blocked,
			'is_open_item_managed', ca.is_open_item_managed) ORDER BY ca.legal_entity_id)
		 FROM ledger.company_account ca WHERE ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ($1 OR ca.legal_entity_id = ANY($2))), '[]'),
		a.version, a.created_at, a.created_by, a.updated_at, a.updated_by
	FROM ledger.gl_account a`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := errs.Wrapf(row.Scan(&a.ID, &a.ChartID, &a.Number, &a.Name, &a.AccountType, &a.IsBalanceSheet, &a.ReconciliationKind, &a.Status,
		&a.Settings, &a.Version, &a.CreatedAt, &a.CreatedBy, &a.UpdatedAt, &a.UpdatedBy), "scan account")
	return a, err
}

// GetAccount returns an account.
func GetAccount(ctx context.Context, id uuid.UUID) (Account, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Account{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Account{}, err
	}
	a, err := scanAccount(tx.QueryRow(ctx, accountSelect+` WHERE a.id = $3`, access.All, access.IDs, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, problem.NotFoundOf("account")
	}
	if err != nil {
		return Account{}, fmt.Errorf("ledger: read account %s: %w", id, err)
	}
	return a, nil
}

// OpenItem is an uncleared line on a supplier or customer reconciliation
// account.
type OpenItem struct {
	ItemRef
	ID             uuid.UUID // derived from entry and line, for paging
	LegalEntityID  uuid.UUID
	AccountID      uuid.UUID
	PartnerID      *uuid.UUID
	PostingDate    time.Time
	TC, LC         money.Minor
	CurrencyTC     string
	CurrencyLC     string
	ExpTC, ExpLC   int
	DocumentNumber string
	Reference      *string
	Terms          *ItemTerms // the payment terms it was posted with, or nil
	SpecialGL      *string    // A for a down payment (ADR-0037)
}

// openItemSelect takes the caller's company access as $1 and $2.
const openItemSelect = `SELECT * FROM (SELECT l.fiscal_year, l.journal_entry_id, l.line_no, md5(l.journal_entry_id::text || '/' || l.line_no)::uuid AS id,
		l.legal_entity_id, l.account_id, l.partner_id, l.posting_date, l.amount_tc, l.amount_lc, e.currency_tc, e.currency_lc,
		ctc.exponent AS exp_tc, clc.exponent AS exp_lc, e.document_number, e.reference, l.special_gl, ` + termsColumns + `
	FROM ledger.journal_line l
	LEFT JOIN ledger.open_item_terms t ON t.tenant_id = l.tenant_id AND t.fiscal_year = l.fiscal_year AND t.journal_entry_id = l.journal_entry_id
		AND t.line_no = l.line_no
	JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind IN ('supplier', 'customer')
	JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
	JOIN ref.currency ctc ON ctc.code = e.currency_tc
	JOIN ref.currency clc ON clc.code = e.currency_lc
	WHERE ($1 OR l.legal_entity_id = ANY($2)) AND NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id
		AND c.fiscal_year = l.fiscal_year AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)`

func scanOpenItem(row pgx.Row) (OpenItem, error) {
	var o OpenItem
	var t storedTerms
	err := errs.Wrapf(row.Scan(append([]any{&o.FiscalYear, &o.EntryID, &o.LineNo, &o.ID, &o.LegalEntityID, &o.AccountID, &o.PartnerID, &o.PostingDate,
		&o.TC, &o.LC, &o.CurrencyTC, &o.CurrencyLC, &o.ExpTC, &o.ExpLC, &o.DocumentNumber, &o.Reference, &o.SpecialGL}, t.fields()...)...), "scan open item")
	o.Terms = t.terms()
	return o, err
}

// Balance is the period totals of one account and transaction currency.
type Balance struct {
	ID                uuid.UUID // derived from account, period and currency, for paging
	AccountID         uuid.UUID
	FiscalPeriod      int16
	CurrencyTC        string
	ExpTC             int
	DebitTC, CreditTC money.Minor
	DebitLC, CreditLC money.Minor
	DebitGC, CreditGC money.Minor
	UpdatedAt         time.Time
}

// balanceSelect adds the lines of entries the projector has not reached yet
// to the projected balances, so the figures are exact at the moment of the
// read; projected rows are unique per key, so only the pending lines are
// aggregated. $1 legal entity, $2 ledger, $3 fiscal year, $4 period and $5
// account (both optional).
const balanceSelect = `SELECT * FROM (SELECT md5(account_id::text || '/' || fiscal_period || '/' || currency_tc)::uuid AS id,
		account_id, fiscal_period, currency_tc, c.exponent,
		coalesce(pb.debit_tc, 0) + coalesce(d.debit_tc, 0), coalesce(pb.credit_tc, 0) + coalesce(d.credit_tc, 0),
		coalesce(pb.debit_lc, 0) + coalesce(d.debit_lc, 0), coalesce(pb.credit_lc, 0) + coalesce(d.credit_lc, 0),
		coalesce(pb.debit_gc, 0) + coalesce(d.debit_gc, 0), coalesce(pb.credit_gc, 0) + coalesce(d.credit_gc, 0),
		greatest(pb.updated_at, d.updated_at)
	FROM (
		SELECT account_id, fiscal_period, currency_tc, debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc, updated_at
		FROM ledger.period_balance
		WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year = $3 AND ($4::smallint IS NULL OR fiscal_period = $4)
			AND ($5::uuid IS NULL OR account_id = $5)
	) pb FULL JOIN (
		SELECT l.account_id, l.fiscal_period, e.currency_tc, sum(greatest(l.amount_tc, 0))::bigint AS debit_tc, sum(greatest(-l.amount_tc, 0))::bigint AS credit_tc,
			sum(greatest(l.amount_lc, 0))::bigint AS debit_lc, sum(greatest(-l.amount_lc, 0))::bigint AS credit_lc,
			sum(greatest(l.amount_gc, 0))::bigint AS debit_gc, sum(greatest(-l.amount_gc, 0))::bigint AS credit_gc, max(e.posted_at) AS updated_at
		FROM ledger.balance_projection q
		JOIN ledger.journal_entry e ON e.tenant_id = q.tenant_id AND e.fiscal_year = q.fiscal_year AND e.id = q.journal_entry_id
		JOIN ledger.journal_line l ON l.tenant_id = q.tenant_id AND l.fiscal_year = q.fiscal_year AND l.journal_entry_id = q.journal_entry_id
		WHERE q.projected_at IS NULL AND q.fiscal_year = $3 AND e.legal_entity_id = $1 AND e.ledger_id = $2
			AND ($4::smallint IS NULL OR l.fiscal_period = $4) AND ($5::uuid IS NULL OR l.account_id = $5)
		GROUP BY 1, 2, 3
	) d USING (account_id, fiscal_period, currency_tc)
	JOIN ref.currency c ON c.code = currency_tc) t`

func scanBalance(row pgx.Row) (Balance, error) {
	var b Balance
	err := errs.Wrapf(row.Scan(&b.ID, &b.AccountID, &b.FiscalPeriod, &b.CurrencyTC, &b.ExpTC, &b.DebitTC, &b.CreditTC, &b.DebitLC, &b.CreditLC,
		&b.DebitGC, &b.CreditGC, &b.UpdatedAt), "scan balance")
	return b, err
}
