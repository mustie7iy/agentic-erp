package payables

import (
	"cmp"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Kinds of supplier invoice (T34, 03-data-model §3.9), as SAP's invoice
// verification transactions: the kind gives the direction of amounts that
// are never negative.
const (
	KindInvoice          = "invoice"
	KindCreditMemo       = "credit_memo"
	KindSubsequentDebit  = "subsequent_debit"
	KindSubsequentCredit = "subsequent_credit"
)

var kinds = map[string]bool{KindInvoice: true, KindCreditMemo: true, KindSubsequentDebit: true, KindSubsequentCredit: true}

// subsequent says whether the kind revalues order items without quantity.
func subsequent(kind string) bool { return kind == KindSubsequentDebit || kind == KindSubsequentCredit }

// credits says whether the kind reduces what is owed the supplier: its
// amounts are negative in events, and it posts Dr supplier as KG.
func credits(kind string) bool { return kind == KindCreditMemo || kind == KindSubsequentCredit }

// signed is an amount of a document with its kind's direction: negative
// for credit memos and subsequent credits.
func signed(v Invoice, amount money.Minor) money.Minor {
	if credits(v.Kind) {
		return -amount
	}
	return amount
}

// kind checks the document's kind and the invoice it corrects: a known
// kind; a reference only on a correction, to a posted invoice of the same
// supplier, legal entity and currency. A subsequent debit or credit is on
// a purchase order because each of its items is an order item (item).
func (v *invoiceCheck) kind() error {
	v.c.kind = cmp.Or(v.in.Kind, KindInvoice)
	if !kinds[v.c.kind] {
		return invalid("payables.unknown_kind", "kind", "The kind is invoice, credit_memo, subsequent_debit or subsequent_credit.")
	}
	if v.in.ReferenceInvoiceID == nil {
		return nil
	}
	if v.c.kind == KindInvoice {
		return invalid("payables.reference_not_allowed", "reference_invoice_id", "An invoice corrects none; a credit memo or subsequent debit/credit names the invoice it corrects.")
	}
	return v.reference(*v.in.ReferenceInvoiceID)
}

// reference checks the invoice a correction names: a posted invoice of
// the same supplier, legal entity and currency.
func (v *invoiceCheck) reference(id uuid.UUID) error {
	ref, err := load(v.ctx, v.tx, id, false)
	var p *problem.Problem
	switch {
	case errors.As(err, &p) && p.Status == 404:
	case err != nil:
		return err
	case ref.Kind == KindInvoice && ref.Status == Posted && ref.SupplierID == v.sup.ID && ref.LegalEntityID == v.le.ID && ref.Currency == v.c.currency:
		return nil
	}
	return invalid("payables.invalid_reference_invoice", "reference_invoice_id",
		"The reference is a posted invoice of this supplier and legal entity in the document's currency.")
}

// quantity checks an item's quantity: 0 on a subsequent debit or credit,
// which revalues without quantity, a positive one otherwise.
func (v *invoiceCheck) quantity(field, quantity string) error {
	if !subsequent(v.c.kind) {
		if money.PositiveQuantity(quantity) != nil {
			return invalid("payables.invalid_quantity", field+".quantity.value", fmt.Sprintf("A quantity is a positive plain decimal of at most %d integer digits and %d decimals.",
				money.QuantityIntDigits, money.QuantityDecimals))
		}
		return nil
	}
	if q, err := money.ParsePlain(quantity); err != nil || q.Sign() != 0 {
		return invalid("payables.quantity_not_zero", field+".quantity.value", "A subsequent debit or credit moves no quantity: its items' quantity is 0.")
	}
	return nil
}
