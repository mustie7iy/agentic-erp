package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// What a statement line's decided match clears, as the cash actuals
// classify it (ADR-0051 §4, §11, T53f2).
const (
	CashMatchPayment     = "payment"      // payments of the house bank
	CashMatchPostingRule = "posting_rule" // a posting rule of the company
	CashMatchIncoming    = "incoming"     // the incoming payments clearing account
	CashMatchExcluded    = "excluded"     // nothing: a person excluded it
	CashMatchNone        = "unmatched"    // no decided match yet
)

// CashActualLine is a bank statement line as the cash actuals read it: its
// statement and number, house bank and currency, signed amount, value date
// and status, and what its decided match clears: the payments, in the
// match's order, or the posting rule's transaction code.
type CashActualLine struct {
	StatementID uuid.UUID
	LineNo      int32
	HouseBankID uuid.UUID
	Currency    string
	Amount      money.Minor
	ValueDate   time.Time
	Status      string
	Match       string
	PostingRule string
	Payments    []CashPayment
}

// Actuals are the legal entity's bank statement lines whose value date is
// from from to to, posted or not, by value date, bank, statement and line,
// each with what its decided match (matched or excluded; an open match
// decides nothing) clears. The payments are read as the cash position
// reads them (cashPaymentSelect), so one its run dropped, which no match
// can name, would be left out.
func (c CashPosition) Actuals(ctx context.Context, legalEntity uuid.UUID, from, to time.Time) ([]CashActualLine, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	type lineRow struct {
		CashActualLine
		decided  *string
		incoming bool
		rule     *string
		payments []uuid.UUID
	}
	rows, _ := tx.Query(ctx, `SELECT l.statement_id, l.line_no, s.house_bank_id, s.currency, l.amount, l.value_date, l.status, m.status,
			coalesce(t.incoming, false), t.posting_rule, coalesce(t.payments, '{}')
		FROM payments.bank_statement_line l JOIN payments.bank_statement s ON s.tenant_id = l.tenant_id AND s.id = l.statement_id
		LEFT JOIN payments.statement_match m ON m.tenant_id = l.tenant_id AND m.statement_id = l.statement_id AND m.line_no = l.line_no
			AND m.status IN ('matched', 'excluded')
		LEFT JOIN LATERAL (SELECT bool_or(x.incoming) AS incoming, min(x.posting_rule) AS posting_rule,
				array_agg(x.payment_id ORDER BY x.candidate, x.target_no) FILTER (WHERE x.payment_id IS NOT NULL) AS payments
			FROM payments.statement_match_target x WHERE x.tenant_id = m.tenant_id AND x.match_id = m.id) t ON true
		WHERE s.legal_entity_id = $1 AND l.value_date BETWEEN $2 AND $3
		ORDER BY l.value_date, s.house_bank_id, l.statement_id, l.line_no`, legalEntity, from, to)
	lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (lineRow, error) {
		var l lineRow
		err := r.Scan(&l.StatementID, &l.LineNo, &l.HouseBankID, &l.Currency, &l.Amount, &l.ValueDate, &l.Status, &l.decided, &l.incoming, &l.rule, &l.payments)
		return l, errs.Wrapf(err, "scan statement line")
	})
	if err != nil {
		return nil, fmt.Errorf("payments: statement lines of %s for the cash actuals: %w", legalEntity, err)
	}
	var ids []uuid.UUID
	for _, l := range lines {
		ids = append(ids, l.payments...)
	}
	paid, err := c.payments(ctx, "matched payments", `p.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]CashPayment, len(paid))
	for _, p := range paid {
		byID[p.ID] = p
	}
	out := make([]CashActualLine, len(lines))
	for i, l := range lines {
		out[i] = l.CashActualLine
		out[i].Match = cashMatch(l.decided, l.incoming, l.rule)
		if l.rule != nil {
			out[i].PostingRule = *l.rule
		}
		for _, id := range l.payments {
			if p, ok := byID[id]; ok {
				out[i].Payments = append(out[i].Payments, p)
			}
		}
	}
	return out, nil
}

// cashMatch is what a line's decided match (its status, nil for none)
// clears: a match decided names one target kind (statement_match_target_one).
func cashMatch(decided *string, incoming bool, rule *string) string {
	switch {
	case decided == nil:
		return CashMatchNone
	case *decided == "excluded":
		return CashMatchExcluded
	case incoming:
		return CashMatchIncoming
	case rule != nil:
		return CashMatchPostingRule
	}
	return CashMatchPayment
}
