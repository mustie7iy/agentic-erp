package receivables

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Kinds of customer invoice (ADR-0063 §2, §7), as payables' kinds are for
// supplier documents: the kind gives the direction of amounts that are
// never negative.
const (
	KindInvoice    = "invoice"
	KindCreditMemo = "credit_memo" // T75d
)

// CreditDocumentType is the journal document type a customer credit memo
// posts as (SAP's DG, customer credit memo); its entries draw their
// numbers from the journal_dg number range.
const CreditDocumentType = "DG"

// signed is an amount of a document with its kind's direction, as what
// the customer owes: negative for a credit memo. Events carry amounts so,
// as payables' do, and the posting's lines follow it.
func signed(v Invoice, amount money.Minor) money.Minor {
	if v.Kind == KindCreditMemo {
		return -amount
	}
	return amount
}

// documentKind checks the kind entered ("": an invoice) and the invoice a
// credit memo names: a known kind (422 receivables.unknown_kind); a
// reference only on a credit memo (422 receivables.reference_not_allowed),
// to a posted invoice of the same company and customer in the memo's
// currency (422 receivables.invalid_reference_invoice), each naming the
// request's field. An invoice of a company the caller may not act in is
// refused as one that does not exist: the memo's company is the caller's
// (parties), so such an invoice is never the same company's.
func documentKind(ctx context.Context, tx pgx.Tx, in Input) (string, error) {
	kind := in.Kind
	if kind == "" {
		kind = KindInvoice
	}
	if kind != KindInvoice && kind != KindCreditMemo {
		return "", problem.Invalid("receivables.unknown_kind", "kind", "The kind is invoice or credit_memo.")
	}
	if in.ReferenceInvoiceID == nil {
		return kind, nil
	}
	if kind == KindInvoice {
		return "", problem.Invalid("receivables.reference_not_allowed", "reference_invoice_id", "An invoice credits none; a credit memo names the invoice it credits.")
	}
	ref, err := load(ctx, tx, *in.ReferenceInvoiceID, false)
	var p *problem.Problem
	switch {
	case errors.As(err, &p) && p.Status == 404:
	case err != nil:
		return "", err
	case ref.Kind == KindInvoice && ref.Status == Posted && ref.CustomerID == in.CustomerID && ref.LegalEntityID == in.LegalEntityID &&
		ref.Currency == in.Currency:
		return kind, nil
	}
	return "", problem.Invalid("receivables.invalid_reference_invoice", "reference_invoice_id",
		"The reference is a posted invoice of this customer and company in the credit memo's currency.")
}

// creditedInvoice locks the invoice a credit memo names and refuses the
// memo when, with the posted credit memos naming the same invoice, it
// would credit more than that invoice's gross amount (ADR-0063 §7, plan
// default 5: the product owner's rule for supplier credit memos of
// 2026-11-10, mirrored; 422 receivables.credit_exceeds_invoice with the
// figures, the refusal payables gives: problem.CreditExceedsInvoice). It
// returns the invoice, nil for a memo naming none, which has no limit,
// and for an invoice.
//
// The invoice's row is read FOR UPDATE (ADR-0008), so another credit memo
// naming it waits for this posting to commit and then counts this one:
// the sum read here holds until then. The memo's own row is locked before
// its invoice's and nothing locks them the other way round, so the two
// never wait for each other in a cycle. Only posted memos count: a draft
// is checked when it posts; whether the invoice is paid plays no part,
// since the limit is its gross amount. The invoice is posted and stays so
// (it was when the memo was entered, and a posted invoice never changes).
// The amounts are in one currency (documentKind) and none is negative, so
// the comparison subtracts and cannot overflow.
func creditedInvoice(ctx context.Context, tx pgx.Tx, v Invoice) (*Invoice, error) {
	if v.ReferenceInvoiceID == nil {
		return nil, nil
	}
	ref, err := load(ctx, tx, *v.ReferenceInvoiceID, true)
	if err != nil {
		return nil, fmt.Errorf("receivables: lock invoice %s that %s credits: %w", *v.ReferenceInvoiceID, v.DocumentNumber, err)
	}
	var credited money.Minor
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(gross_amount), 0)::bigint FROM receivables.customer_invoice
		WHERE reference_invoice_id = $1 AND status = 'posted' AND kind = 'credit_memo'`, ref.ID).Scan(&credited); err != nil {
		return nil, fmt.Errorf("receivables: read the credits posted against invoice %s: %w", ref.ID, err)
	}
	if v.Gross <= ref.Gross-credited {
		return &ref, nil
	}
	return nil, problem.CreditExceedsInvoice("receivables.credit_exceeds_invoice", ref.ID, ref.DocumentNumber, v.DocumentNumber,
		problem.CreditFigures{Gross: ref.Gross, Credited: credited, Attempted: v.Gross, Currency: v.Currency, Exponent: v.Exponent})
}
