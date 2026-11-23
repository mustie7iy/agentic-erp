package receivables

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Bad debt relief charged again (ADR-0049 As built batch 91; tracker leaf,
// src po:2026-11-21): what the customer later pays of an invoice beyond
// what its write-offs left owed is a recovery of the bad debt, and the
// share of the tax its reliefs recovered is charged again, by an
// adjustment the return reports, in the transaction that applies the
// payment; undoing that application (T75i) charges back less. The
// research of 2026-11-21 names Art. 64 and ETA s.231 for the relief and
// the tracker leaf the charge again; how and where the return reports it
// is this step's default (the relief's codes and lines, reversed), for
// the tax specialist.
//
// What is charged again is a pure function of the invoice's state, so
// each change of it posts the difference to what was charged so far:
//
//   - paid is what the incoming payments in force applied of the invoice:
//     of its entry's items and of the residual items they left, net of
//     residuals, write-offs and cash discounts, with what they applied of
//     its reliefs' write-off credits (negative): a payment of the
//     invoice's item with the write-off's credit pays only what was left;
//   - recovered is paid beyond the gross less what the reliefs wrote
//     off, from nothing to what they wrote off;
//   - charged is the reliefs' tax times recovered over written off, in
//     the invoice's currency and the company's, each rounded once half
//     away from zero, so a full recovery charges exactly what the reliefs
//     recovered.
//
// A relief and a payment of one customer run one after the other
// (RechargeLock): a relief is refused once the invoice is paid beyond
// what its write-offs leave (unpaid), and a payment after a relief
// charges its tax again.

// RechargeKey is the account determination key a charge again debits:
// the payment differences account the relief credited (WrittenOffKey).
const RechargeKey = WrittenOffKey

// PaymentRecharge is a row of receivables.bad_debt_recharge as
// paymentSelect reads it: the invoice, the application (0: the payment's
// own) whose applying or undoing (Reset) moved what was recovered, that
// amount as it then stood, the tax charged again (negative: charged back
// less), in the invoice's currency, which is the payment's
// (relievedOtherCurrency), and the company's, and its entry.
type PaymentRecharge struct {
	ID            uuid.UUID   `json:"id"`
	InvoiceID     uuid.UUID   `json:"customer_invoice_id"`
	InvoiceNumber string      `json:"customer_invoice_number"`
	ApplicationNo int32       `json:"application_no"`
	Reset         bool        `json:"reset"`
	Currency      string      `json:"currency"`
	Exponent      int         `json:"exponent"`
	LocalCurrency string      `json:"local_currency"`
	LocalExponent int         `json:"local_exponent"`
	Recovered     money.Minor `json:"recovered_amount"`
	Tax           money.Minor `json:"tax_amount"`
	TaxLC         money.Minor `json:"tax_amount_lc"`
	PostingDate   string      `json:"posting_date"`
	EntryYear     int16       `json:"journal_entry_year"`
	EntryID       uuid.UUID   `json:"journal_entry_id"`
	EntryNumber   string      `json:"journal_entry_number"`
	CreatedAt     time.Time   `json:"created_at"`
	CreatedBy     uuid.UUID   `json:"created_by"`
}

// rechargeSelect is paymentSelect's column of a payment's charges again,
// in the order they were posted.
const rechargeSelect = `coalesce((SELECT json_agg(json_build_object('id', b.id, 'customer_invoice_id', b.customer_invoice_id,
			'customer_invoice_number', v.document_number, 'application_no', b.application_no, 'reset', b.reset, 'currency', b.currency,
			'exponent', (SELECT x.exponent FROM ref.currency x WHERE x.code = b.currency), 'local_currency', b.local_currency,
			'local_exponent', (SELECT x.exponent FROM ref.currency x WHERE x.code = b.local_currency), 'recovered_amount', b.recovered_amount,
			'tax_amount', b.tax_amount, 'tax_amount_lc', b.tax_amount_lc, 'posting_date', b.posting_date, 'journal_entry_year', b.journal_entry_year,
			'journal_entry_id', b.journal_entry_id, 'journal_entry_number', b.journal_entry_number, 'created_at', b.created_at, 'created_by', b.created_by)
			ORDER BY b.created_at, b.id)
		 FROM receivables.bad_debt_recharge b JOIN receivables.customer_invoice v ON v.tenant_id = b.tenant_id AND v.id = b.customer_invoice_id
		 WHERE b.tenant_id = p.tenant_id AND b.incoming_payment_id = p.id), '[]')`

// RechargeLock is the advisory lock (database.LockTenant) a customer's
// payments, their undoings and the reliefs of its invoices take, by
// company and customer: what a payment pays of an invoice and whether a
// relief may be claimed for it are read under it, so a relief and a
// payment of one customer run one after the other and neither misses
// the other (no write skew), even before the invoice has a relief.
const RechargeLock = "receivables.bad_debt"

// relievedOf holds the customers of the company (RechargeLock, by id, so
// two holders never wait on each other in a cycle) and locks their
// invoices a bad debt relief relieved, by id, returning them. Every
// caller takes them before any invoice row and any ledger entry lock of
// its own: an application or undoing before the ledger's entries, a
// relief before its invoice's row. A charge again inserts rows naming the
// invoice, whose foreign key takes it FOR KEY SHARE after the ledger's
// locks; holding the row from before them, as an invoice's reversal
// takes it (row, then entries), keeps that wait out of a cycle.
func relievedOf(ctx context.Context, tx pgx.Tx, company uuid.UUID, customers []uuid.UUID) ([]uuid.UUID, error) {
	held := slices.Clone(customers)
	slices.SortFunc(held, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	for _, c := range slices.Compact(held) {
		if err := database.LockTenant(ctx, tx, RechargeLock, company.String(), c.String()); err != nil {
			return nil, fmt.Errorf("receivables: hold customer %s of company %s: %w", c, company, err)
		}
	}
	rows, _ := tx.Query(ctx, `SELECT v.id FROM receivables.customer_invoice v
		WHERE v.id IN (SELECT r.customer_invoice_id FROM receivables.bad_debt_relief r WHERE r.legal_entity_id = $1) AND v.customer_id = ANY($2)
		ORDER BY v.id FOR UPDATE OF v`, company, customers)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("receivables: the relieved invoices of %d customers: %w", len(customers), err)
	}
	return ids, nil
}

// recharging is the change a charge again follows: the payment, its
// application (0: its own), whether it undoes it, and the date it posts.
type recharging struct {
	payment     uuid.UUID
	application int32
	reset       bool
	date        time.Time
}

// recharge charges again what the change moved of each relieved invoice
// (rechargeInvoice), in the order relievedOf read them.
func (e *Endpoints) recharge(ctx context.Context, tx pgx.Tx, p reqctx.Principal, invoices []uuid.UUID, r recharging) error {
	for _, id := range invoices {
		if err := e.rechargeInvoice(ctx, tx, p, id, r); err != nil {
			return err
		}
	}
	return nil
}

// reliefState is what an invoice's reliefs wrote off and recovered and
// what has been charged again, in its currency and the company's, with
// the reliefs' entries and the write-offs' credits.
type reliefState struct {
	written, tax, lc   money.Minor
	charged, chargedLC money.Minor
	entries            []uuid.UUID
	credits            []ledger.ItemRef
}

// reliefStateOf reads the invoice's reliefs and charges again, and the
// write-offs' credits to the customer through WriteOffs.
func (e *Endpoints) reliefStateOf(ctx context.Context, tx pgx.Tx, v Invoice) (reliefState, error) {
	var s reliefState
	var years []int16
	var writeOffs []uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(written_off_amount), 0)::bigint, coalesce(sum(tax_amount), 0)::bigint,
			coalesce(sum(tax_amount_lc), 0)::bigint, coalesce(array_agg(journal_entry_id ORDER BY posting_date, id), '{}'),
			coalesce(array_agg(write_off_entry_year ORDER BY posting_date, id), '{}'), coalesce(array_agg(write_off_entry_id ORDER BY posting_date, id), '{}'),
			(SELECT coalesce(sum(b.tax_amount), 0)::bigint FROM receivables.bad_debt_recharge b WHERE b.customer_invoice_id = $1),
			(SELECT coalesce(sum(b.tax_amount_lc), 0)::bigint FROM receivables.bad_debt_recharge b WHERE b.customer_invoice_id = $1)
		FROM receivables.bad_debt_relief WHERE customer_invoice_id = $1`, v.ID).
		Scan(&s.written, &s.tax, &s.lc, &s.entries, &years, &writeOffs, &s.charged, &s.chargedLC); err != nil {
		return reliefState{}, fmt.Errorf("receivables: the bad debt reliefs of customer invoice %s: %w", v.DocumentNumber, err)
	}
	for i, entry := range writeOffs {
		w, _, err := e.WriteOffs.WrittenOff(ctx, entry, *v.EntryYear, *v.EntryID) // a relief names a write-off of its invoice
		if err != nil {
			return reliefState{}, fmt.Errorf("receivables: write-off %s of customer invoice %s: %w", entry, v.DocumentNumber, err)
		}
		for _, line := range w.CreditLines {
			s.credits = append(s.credits, ledger.ItemRef{FiscalYear: years[i], EntryID: entry, LineNo: line})
		}
	}
	return s, nil
}

// paidOf is what the incoming payments in force applied of the invoice
// (see above), and whether one applied an item of it in another currency
// than the item's, which paidOf cannot count in the invoice's currency.
// The residual items are followed from application to application: a
// residual item is a line of the clearing entry of the application that
// left it. An underpayment an application wrote off within the company's
// tolerance (its difference, T75f) was not paid: its share of what the
// application applied of the invoice's debits, in proportion to all the
// debits it applied (so never more than the difference, whatever credits
// it applied beside them), rounded half away from zero (partOf), is not
// counted; an overpayment kept as revenue adds nothing to what the items
// were paid.
func paidOf(ctx context.Context, tx pgx.Tx, v Invoice, credits []ledger.ItemRef) (money.Minor, bool, error) {
	years, entries, lines := ledger.ItemColumns(credits)
	rows, _ := tx.Query(ctx, `WITH RECURSIVE applied AS (
			SELECT t.incoming_payment_id, t.application_no, t.item_currency, t.residual_line_no, `+itemPaid+` AS paid,
				coalesce(a.clearing_entry_year, p.clearing_entry_year) AS cy, coalesce(a.clearing_entry_id, p.clearing_entry_id) AS ce,
				coalesce(a.difference, p.difference) AS difference
			FROM receivables.incoming_payment_item t
			JOIN receivables.incoming_payment p ON p.tenant_id = t.tenant_id AND p.id = t.incoming_payment_id
			LEFT JOIN receivables.incoming_payment_application a ON a.tenant_id = t.tenant_id AND a.incoming_payment_id = t.incoming_payment_id
				AND a.application_no = t.application_no
			WHERE (t.journal_entry_id = $1 AND t.fiscal_year = $2)
				OR (t.journal_entry_id, t.line_no, t.fiscal_year) IN (SELECT * FROM unnest($4::uuid[], $5::int[], $3::smallint[]))
			UNION ALL
			SELECT t.incoming_payment_id, t.application_no, t.item_currency, t.residual_line_no, `+itemPaid+`,
				coalesce(a.clearing_entry_year, p.clearing_entry_year), coalesce(a.clearing_entry_id, p.clearing_entry_id),
				coalesce(a.difference, p.difference)
			FROM applied l
			JOIN receivables.incoming_payment_item t ON t.journal_entry_id = l.ce AND t.line_no = l.residual_line_no AND t.fiscal_year = l.cy
			JOIN receivables.incoming_payment p ON p.tenant_id = t.tenant_id AND p.id = t.incoming_payment_id
			LEFT JOIN receivables.incoming_payment_application a ON a.tenant_id = t.tenant_id AND a.incoming_payment_id = t.incoming_payment_id
				AND a.application_no = t.application_no),
		per AS (
			SELECT l.incoming_payment_id, l.application_no, sum(l.paid) AS paid, coalesce(sum(l.paid) FILTER (WHERE l.paid > 0), 0) AS debits,
				max(l.difference) AS difference, bool_or(l.item_currency IS NOT NULL) AS translated,
				(SELECT coalesce(sum(`+itemPaid+`) FILTER (WHERE t.amount > 0), 0) FROM receivables.incoming_payment_item t
				 WHERE t.incoming_payment_id = l.incoming_payment_id AND t.application_no = l.application_no) AS whole
			FROM applied l
			WHERE NOT EXISTS (SELECT FROM receivables.incoming_payment_reset r WHERE r.incoming_payment_id = l.incoming_payment_id
				AND coalesce(r.application_no, 0) = l.application_no)
			GROUP BY l.incoming_payment_id, l.application_no)
		SELECT paid::bigint, debits::bigint, difference, whole::bigint, translated FROM per ORDER BY incoming_payment_id, application_no`,
		*v.EntryID, *v.EntryYear, years, entries, lines)
	var paid, debits, difference, whole, total money.Minor
	var translated, foreign bool
	_, err := pgx.ForEachRow(rows, []any{&paid, &debits, &difference, &whole, &translated}, func() error {
		// Applied amounts of one invoice's items and the debits beside them: in range.
		total += paid
		if difference > 0 && whole > 0 {
			total -= partOf(difference, debits, whole) // debits are some of whole: at most the difference
		}
		foreign = foreign || translated
		return nil
	})
	if err != nil {
		return 0, false, fmt.Errorf("receivables: what was paid of customer invoice %s: %w", v.DocumentNumber, err)
	}
	return total, foreign, nil
}

// itemPaid is what an incoming payment item t paid of its item: its
// amount less the residual it left, what its reason wrote off and its
// cash discount.
const itemPaid = `t.amount - coalesce(t.residual_amount, 0) - coalesce(t.write_off_amount, 0) - coalesce(t.cash_discount, 0)`

// relievedOtherCurrency refuses an invoice with a bad debt relief whose
// item a payment applied in another currency: what is paid of it is
// counted in its own currency (422 receivables.relieved_invoice_other_currency).
func relievedOtherCurrency(v Invoice) error {
	return problem.Unprocessable("receivables.relieved_invoice_other_currency", "Customer invoice "+v.DocumentNumber+" has a bad debt relief, "+
		"whose tax what the customer pays of it charges again: its items and their write-offs' credits are applied with a receipt in "+v.Currency+".")
}

// recovered is what is paid of the invoice beyond its gross less what its
// reliefs wrote off, from nothing to that.
func recovered(v Invoice, s reliefState, paid money.Minor) money.Minor {
	// Paid amounts are applied amounts of one invoice's items: in range.
	return max(0, min(paid-(v.Gross-s.written), s.written))
}

// partOf is total × part ÷ whole, rounded half away from zero; whole is
// above zero and part at most it, so the result is at most total.
func partOf(total, part, whole money.Minor) money.Minor {
	out, _ := money.Round(new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(int64(total)), big.NewInt(int64(part))), big.NewInt(int64(whole))), 0)
	return out
}

// rechargeInvoice charges again, under the reliefs' codes, the difference
// between what the invoice's recovery charges and what was charged so far
// (see above); nothing when there is none.
func (e *Endpoints) rechargeInvoice(ctx context.Context, tx pgx.Tx, p reqctx.Principal, id uuid.UUID, r recharging) error {
	v, err := get(ctx, tx, id, false) // locked by relievedOf
	if err != nil {
		return fmt.Errorf("receivables: relieved customer invoice %s: %w", id, err)
	}
	s, err := e.reliefStateOf(ctx, tx, v)
	if err != nil {
		return err
	}
	paid, foreign, err := paidOf(ctx, tx, v, s.credits)
	switch {
	case err != nil:
		return err
	case foreign:
		return relievedOtherCurrency(v)
	}
	back := recovered(v, s, paid)
	// The local tax follows the tax charged, so a full recovery is exact in
	// both currencies and no change of the tax leaves none of the local
	// tax; each part is at most the reliefs' tax: in range.
	cumulative := partOf(s.tax, back, s.written)
	tax, lc := cumulative-s.charged, partOf(s.lc, cumulative, s.tax)-s.chargedLC
	if tax == 0 { // the local tax is the earlier charges' too
		return nil
	}
	c := charge{invoice: v, state: s, recovered: back, tax: tax, lc: lc, recharging: r}
	if err := e.postCharge(ctx, &c); err != nil {
		return err
	}
	return c.record(ctx, tx, p)
}
