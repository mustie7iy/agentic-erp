package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CashPosition is what the cash context reads of payments for the daily
// cash position (ADR-0051 §8, T53c): a legal entity's house banks with
// their latest statement before a day and its reconciliation figures, the
// released payments its clearing accounts still hold open, and the
// payments of runs not released yet. It reads in the request's transaction
// under row-level security; the caller checks company access. The
// composition root wires it behind the cash context's port, so neither
// context imports the other.
type CashPosition struct {
	Accounts Accounts // the main bank account's balance on the statement's date
	Owed     Owed     // the supplier open items the liquidity forecast expects to pay (T53e)
	Payees   Payees   // their suppliers' blocks, net days and payment methods
	Unposted Unposted // the invoices not posted yet (T53e2)
	Requests Requests // the open down payment requests (T53e2)
	Orders   Orders   // the open values of the released purchase orders (T53g)
}

// Orders is what the liquidity forecast reads of the purchase orders: the
// released orders' open values, each as the supplier open item invoicing
// it would give, and the order an invoice was entered on, by which the
// forecast variance knows a payment of the invoice as the order (payables
// context, T53g).
type Orders interface {
	OpenOrders(ctx context.Context, legalEntity uuid.UUID, on time.Time) ([]ledger.PayableItem, error)
	InvoiceOrders(ctx context.Context, invoices []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
}

// Unposted is what the liquidity forecast reads of the invoices parked or
// on hold: each as the supplier open item it would post (payables
// context, T53e2).
type Unposted interface {
	Unposted(ctx context.Context, legalEntity uuid.UUID) ([]ledger.PayableItem, error)
}

// Requests is what the liquidity forecast reads of the open down payment
// requests, in every currency (payables context, T53e2).
type Requests interface {
	Open(ctx context.Context, legalEntity uuid.UUID) ([]DownPayment, error)
}

// Owed is what the liquidity forecast reads of the ledger's supplier open
// items (ledger.OpenItems.Owed).
type Owed interface {
	Owed(ctx context.Context, legalEntity uuid.UUID) ([]ledger.PayableItem, error)
	// Offsetting are the entries' offsetting lines, from which the
	// forecast derives the liquidity items of their items (T53e3).
	Offsetting(ctx context.Context, entries []ledger.EntryRef) (map[ledger.EntryRef][]ledger.OffsettingLine, error)
	// Sources are the documents the entries were posted for in a context:
	// the supplier invoices of the items a payment settled, which the
	// forecast variance matches to the invoices it forecast before they
	// were posted (T53f3).
	Sources(ctx context.Context, sourceContext string, entries []ledger.EntryRef) (map[ledger.EntryRef]uuid.UUID, error)
}

// CashBank is a house bank as the cash position reads it.
type CashBank struct {
	ID                uuid.UUID
	Code, Last4       string
	Currency          string
	Exponent          int
	ClearingAccountID uuid.UUID      // where its payments are open until the bank books them
	Statement         *CashStatement // the latest closing before the day; nil: none
}

// CashStatement is a house bank's statement the position opens at: its
// closing balance, whether a statement up to it is in gap, and its
// reconciliation figures on its closing date.
type CashStatement struct {
	Closing money.Minor
	Gap     bool
	Figures Figures
}

// CashPayment is a payment as the cash position reads it: Amount is what
// it pays, positive, in Currency, from the house bank.
type CashPayment struct {
	ID, RunID   uuid.UUID
	RunNumber   string    // its run's document number
	SupplierID  uuid.UUID // the business partner it pays
	HouseBankID uuid.UUID
	Method      string // "" for a run proposed for its bank
	RunDate     time.Time
	Amount      money.Minor
	Currency    string
	// EntryID is the payment's journal entry, once posted.
	EntryID *uuid.UUID
	// MatchedOn is the closing date of the statement whose line's decided
	// match clears it: the bank has booked it, though the posting may still
	// wait for approval.
	MatchedOn *time.Time
}

// Banks are the legal entity's active house banks by code, each with its
// latest statement closing before the day.
func (c CashPosition) Banks(ctx context.Context, legalEntity uuid.UUID, before time.Time) ([]CashBank, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	type bankRow struct {
		CashBank
		main    *uuid.UUID
		closing *time.Time
		balance *money.Minor
		gap     bool
	}
	rows, _ := tx.Query(ctx, `SELECT h.id, h.code, h.iban_last4, h.currency, c.exponent, h.gl_account_id, h.main_gl_account_id, s.closing_date, s.closing_balance,
			s.closing_date IS NOT NULL AND EXISTS (SELECT FROM payments.bank_statement g WHERE g.tenant_id = h.tenant_id AND g.house_bank_id = h.id
				AND g.status = 'gap' AND g.closing_date <= s.closing_date)
		FROM payments.house_bank h JOIN ref.currency c ON c.code = h.currency
		LEFT JOIN LATERAL (SELECT closing_date, closing_balance FROM payments.bank_statement b WHERE b.tenant_id = h.tenant_id AND b.house_bank_id = h.id
			AND b.closing_date < $2 ORDER BY b.closing_date DESC, b.id DESC LIMIT 1) s ON true
		WHERE h.legal_entity_id = $1 AND h.status = 'active' ORDER BY h.code`, legalEntity, before)
	banks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (bankRow, error) {
		var b bankRow
		err := r.Scan(&b.ID, &b.Code, &b.Last4, &b.Currency, &b.Exponent, &b.ClearingAccountID, &b.main, &b.closing, &b.balance, &b.gap)
		return b, errs.Wrapf(err, "scan house bank")
	})
	if err != nil {
		return nil, fmt.Errorf("payments: house banks of %s for the cash position: %w", legalEntity, err)
	}
	out := make([]CashBank, len(banks))
	for i, b := range banks {
		out[i] = b.CashBank
		if b.closing == nil {
			continue
		}
		figs, err := figures(ctx, tx, c.Accounts, reconBank{ID: b.ID, LegalEntityID: legalEntity, Currency: b.Currency, Exponent: b.Exponent, Main: b.main},
			[]time.Time{*b.closing}, []money.Minor{*b.balance})
		if err != nil {
			return nil, err
		}
		out[i].Statement = &CashStatement{Closing: *b.balance, Gap: b.gap, Figures: figs[0]}
	}
	return out, nil
}

// cashPaymentSelect reads payments with their paying bank, their run and
// the statement that booked them; $1 is the filter's argument.
const cashPaymentSelect = `SELECT p.id, p.payment_run_id, r.document_number, p.supplier_id, coalesce(p.house_bank_id, r.house_bank_id), coalesce(p.payment_method, ''), r.run_date, p.amount,
		p.currency, p.journal_entry_id,
		(SELECT max(s.closing_date) FROM payments.statement_match_target t
			JOIN payments.statement_match m ON m.tenant_id = t.tenant_id AND m.id = t.match_id AND m.status = 'matched'
			JOIN payments.bank_statement s ON s.tenant_id = m.tenant_id AND s.id = m.statement_id
			WHERE t.tenant_id = p.tenant_id AND t.payment_id = p.id)
	FROM payments.payment p JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
	WHERE coalesce(p.house_bank_id, r.house_bank_id) IS NOT NULL AND NOT ` + dropped

func (CashPosition) payments(ctx context.Context, what, filter string, arg any) ([]CashPayment, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, cashPaymentSelect+` AND `+filter+` ORDER BY r.run_date, p.id`, arg)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CashPayment, error) {
		var p CashPayment
		err := r.Scan(&p.ID, &p.RunID, &p.RunNumber, &p.SupplierID, &p.HouseBankID, &p.Method, &p.RunDate, &p.Amount, &p.Currency, &p.EntryID, &p.MatchedOn)
		return p, errs.Wrapf(err, "scan payment")
	})
	if err != nil {
		return nil, fmt.Errorf("payments: %s for the cash position: %w", what, err)
	}
	return out, nil
}

// Released are the posted payments among the ids, whatever their run's
// status since: what a clearing account's open lines name, the line being
// what says the money has not left yet.
func (c CashPosition) Released(ctx context.Context, ids []uuid.UUID) ([]CashPayment, error) {
	return c.payments(ctx, "released payments", `p.id = ANY($1) AND p.journal_entry_id IS NOT NULL`, ids)
}

// Pending are the legal entity's payments of runs proposed or being
// released that are not posted yet: confirmed, not yet in the ledger.
func (c CashPosition) Pending(ctx context.Context, legalEntity uuid.UUID) ([]CashPayment, error) {
	return c.payments(ctx, "pending payments", `r.legal_entity_id = $1 AND r.status IN ('proposed', 'releasing') AND p.journal_entry_id IS NULL`, legalEntity)
}

// CashHouseBank is an active house bank as the cash context's memo
// records name it.
type CashHouseBank struct {
	ID             uuid.UUID
	Code, Currency string
}

// HouseBanks are the legal entity's active house banks by code: the banks
// a memo record may be expected on (ADR-0051 §7, T53d).
func (CashPosition) HouseBanks(ctx context.Context, legalEntity uuid.UUID) ([]CashHouseBank, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, code, currency FROM payments.house_bank WHERE legal_entity_id = $1 AND status = 'active' ORDER BY code`, legalEntity)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CashHouseBank, error) {
		var b CashHouseBank
		err := r.Scan(&b.ID, &b.Code, &b.Currency)
		return b, errs.Wrapf(err, "scan house bank")
	})
	if err != nil {
		return nil, fmt.Errorf("payments: house banks of %s for memo records: %w", legalEntity, err)
	}
	return out, nil
}

// HouseBankCodes are the codes of the house banks among the ids, active or
// not, keyed by id: what a memo record shows beside its house bank's id
// (ADR-0051 §7). An id that names no house bank is left out.
func (CashPosition) HouseBankCodes(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return houseBankCodes(ctx, ids)
}

// CashBankAccount is a house bank's code and the last four characters of
// its account, as the treasury export names it (ADR-0051 §14, §15).
type CashBankAccount struct {
	Code, Last4 string
}

// HouseBankAccounts are the codes and account endings of the house banks
// among the ids, active or not, keyed by id. An id that names no house
// bank is left out.
func (CashPosition) HouseBankAccounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]CashBankAccount, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, code, iban_last4 FROM payments.house_bank WHERE id = ANY($1)`, ids)
	out := map[uuid.UUID]CashBankAccount{}
	var id uuid.UUID
	var a CashBankAccount
	if _, err = pgx.ForEachRow(rows, []any{&id, &a.Code, &a.Last4}, func() error { out[id] = a; return nil }); err != nil {
		return nil, fmt.Errorf("payments: accounts of %d house banks: %w", len(ids), err)
	}
	return out, nil
}

// CashStatementLine is a bank statement line as a memo record's
// realisation or an agent's evidence reads it: its statement's legal
// entity, house bank and currency, its signed amount and value date.
type CashStatementLine struct {
	LegalEntityID, HouseBankID uuid.UUID
	Currency                   string
	Amount                     money.Minor
	ValueDate                  time.Time
}

// StatementLine is a line of a bank statement; found is false when there
// is none. The caller checks the line's legal entity against the record's.
func (CashPosition) StatementLine(ctx context.Context, statement uuid.UUID, lineNo int32) (CashStatementLine, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return CashStatementLine{}, false, err
	}
	rows, _ := tx.Query(ctx, `SELECT s.legal_entity_id, s.house_bank_id, s.currency, l.amount, l.value_date
		FROM payments.bank_statement_line l JOIN payments.bank_statement s ON s.tenant_id = l.tenant_id AND s.id = l.statement_id
		WHERE l.statement_id = $1 AND l.line_no = $2`, statement, lineNo)
	lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CashStatementLine, error) {
		var l CashStatementLine
		err := r.Scan(&l.LegalEntityID, &l.HouseBankID, &l.Currency, &l.Amount, &l.ValueDate)
		return l, errs.Wrapf(err, "scan statement line")
	})
	if err != nil || len(lines) == 0 {
		return CashStatementLine{}, false, errs.Wrapf(err, "payments: line %d of statement %s", lineNo, statement)
	}
	return lines[0], true, nil
}
