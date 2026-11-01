package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The payments a person may match a statement's line to by hand (T73g2,
// ADR-0047): those manualTargets accepts, posted, from the statement's
// house bank, in its currency and matched to no line, so a payment the
// rules did not find can be chosen. A payment names its house bank, or
// leaves it null for its run's (000093): one branch each, so each reads
// an index (payment_posted_by_bank, 000191, and the run's payments).

// PaymentSearch narrows the payments a statement's lines may be matched
// to: the amount paid, a decimal in the statement's currency ("" for no
// bound), and the run's date (nil for none).
type PaymentSearch struct {
	AmountMin, AmountMax string
	RunFrom, RunTo       *time.Time
}

// statementPaymentSort is the search's one order: the end-to-end id, whose
// run document number follows the runs, latest first.
var statementPaymentSort = pagination.Sort{Default: "end_to_end_id", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "end_to_end_id", Column: "end_to_end_id", Type: "text"}}}

// matchablePayments is the search: $1 the house bank, $2 the currency, $3
// and $4 the amount's bounds, $5 and $6 the run date's.
const matchablePayments = `JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
	WHERE p.currency = $2 AND p.journal_entry_id IS NOT NULL
	AND ($3::bigint IS NULL OR p.amount >= $3) AND ($4::bigint IS NULL OR p.amount <= $4)
	AND ($5::date IS NULL OR r.run_date >= $5) AND ($6::date IS NULL OR r.run_date <= $6)
	AND NOT EXISTS (SELECT FROM payments.statement_match_target t JOIN payments.statement_match sm ON sm.tenant_id = t.tenant_id AND sm.id = t.match_id
		WHERE t.tenant_id = p.tenant_id AND t.payment_id = p.id AND sm.status = 'matched')`

// StatementPayments returns a page of the payments the lines of a
// statement the caller may access may be matched to, and the statement.
func StatementPayments(ctx context.Context, ks *pagination.Keyset, id uuid.UUID, f PaymentSearch) (Statement, []Payment, error) {
	s, tx, err := visibleStatement(ctx, id)
	if err != nil {
		return Statement{}, nil, err
	}
	// The bounds are in the statement's currency, known once it is read.
	least, err := amountBound("amount_min", f.AmountMin, s.Exponent)
	if err != nil {
		return Statement{}, nil, err
	}
	most, err := amountBound("amount_max", f.AmountMax, s.Exponent)
	if err != nil {
		return Statement{}, nil, err
	}
	// The page is chosen by id and end-to-end id alone, and only its rows
	// are read whole: paymentSelect's items and exclusion, read for every
	// payment before the sort, took 120 ms at 20,000 (the budget test).
	page, args := ks.SQL(`SELECT * FROM (SELECT p.id, p.end_to_end_id FROM payments.payment p `+matchablePayments+` AND p.house_bank_id = $1
		UNION ALL SELECT p.id, p.end_to_end_id FROM payments.payment p `+matchablePayments+` AND p.house_bank_id IS NULL AND r.house_bank_id = $1) p WHERE true`,
		[]any{s.HouseBankID, s.Currency, least, most, f.RunFrom, f.RunTo})
	// The search has one order, latest first (statementPaymentSort).
	rows, _ := tx.Query(ctx, paymentSelect+` JOIN (`+page+`) page ON page.id = p.id ORDER BY p.end_to_end_id DESC, p.id DESC`, args...)
	pays, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Payment, error) { return scanPayment(r) })
	if err != nil {
		return Statement{}, nil, fmt.Errorf("payments: search payments for statement %s: %w", id, err)
	}
	return s, pays, nil
}

// ListBankStatementPayments implements GET /v1/bank-statements/{id}/payments.
func (e *Endpoints) ListBankStatementPayments(ctx context.Context, params gen.ListBankStatementPaymentsParams) (gen.ListBankStatementPaymentsRes, error) {
	f := PaymentSearch{AmountMin: params.AmountMin.Value, AmountMax: params.AmountMax.Value, RunFrom: apitypes.Ptr[time.Time](params.RunDateFrom),
		RunTo: apitypes.Ptr[time.Time](params.RunDateTo)}
	scope := fmt.Sprintf("listBankStatementPayments|%s|%s|%s|%s|%s", params.ID, f.AmountMin, f.AmountMax, dateText(f.RunFrom), dateText(f.RunTo))
	ks, err := e.Pages.Keyset(statementPaymentSort, scope, apitypes.Page(gen.OptString{}, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	s, pays, err := StatementPayments(ctx, ks, params.ID, f)
	if err != nil {
		return nil, err
	}
	page, next := pagination.Page(ks, pays, func(p Payment) (string, uuid.UUID) { return p.EndToEndID, p.ID })
	run := Run{Currency: s.Currency, Exponent: s.Exponent, HouseBankID: &s.HouseBankID}
	refs, err := e.paymentRefs(ctx, run, page)
	if err != nil {
		return nil, fmt.Errorf("payments: search payments for statement %s: %w", s.ID, err)
	}
	out := gen.PaymentPage{Items: make([]gen.Payment, len(page))}
	for i, p := range page {
		out.Items[i] = paymentToAPI(p, run, refs)
	}
	if next != "" {
		out.NextPageToken.SetTo(next)
	}
	return &out, nil
}

// amountBound is an amount filter in minor units, nil when absent; an
// amount without exactly the currency's digits is refused (ADR-0005).
func amountBound(name, v string, exponent int) (*money.Minor, error) {
	if v == "" {
		return nil, nil
	}
	m, err := money.ParseAmount(v, exponent)
	if err != nil {
		return nil, problem.BadRequest("request.invalid_parameter", fmt.Sprintf("Parameter %s is an amount with exactly %d decimals.", name, exponent),
			problem.FieldError{Field: name, Code: "invalid", Message: "an amount in the statement's currency"})
	}
	return &m, nil
}

// dateText is an optional date as the page token's scope names it.
func dateText(d *time.Time) string {
	if d == nil {
		return ""
	}
	return d.Format(time.DateOnly)
}
