package ui

import (
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// DownPaymentsVerb is the route, under a posted invoice's path, of the
// screen that clears the supplier's down payments against it (T73e4,
// ADR-0037, SAP F-54).
const DownPaymentsVerb = "down-payments"

// FieldDownPayment is a down payment chosen (its fiscal year, journal
// entry and line, DownPaymentValue); FieldPage the page of them the form
// lists, so a refused form lists them again.
const (
	FieldDownPayment = "down_payment"
	FieldPage        = "page"
)

// DownPaymentChoice is one of the supplier's open down payments: its
// value in the form ("" when it is not offered, Why saying why), its
// request's number, its payment entry's number, posting date and
// reference, what was paid and the input tax it includes (the request's,
// the amounts the clearing weighs against the invoice's gross amount;
// nil without a request), and whether the person chose it.
type DownPaymentChoice struct {
	Value     string
	Request   string
	Entry     string
	Reference string
	Posting   time.Time
	Amount    *Money
	Tax       *Money
	Why       i18n.Key
	Chosen    bool
}

// DownPaymentsPage is the clearing of the supplier's down payments
// against a posted invoice: the invoice's supplier and gross amount, a
// page of the supplier's open down payments to choose from (the next
// page's path), the posting date of the transfer, and the form's ETag and
// idempotency key; the API's refusal to list them or to clear them.
type DownPaymentsPage struct {
	Back, Number string
	Supplier     Supplier
	Gross        Money
	Closed       bool // not a posted invoice: nothing is cleared against it
	Action       string
	CSRF         string
	ETag         string
	Key          string
	Page         string
	Date         string
	DateError    i18n.Key
	ChooseError  i18n.Key
	Items        []DownPaymentChoice
	ItemsRefusal *Refusal
	More         string
	Refusal      *Refusal
}

// downPaymentRows are the invoice's facts the clearing is weighed against.
func downPaymentRows(v DownPaymentsPage) []Row {
	return []Row{{Key: i18n.InvoiceSupplier, Value: SupplierName(v.Supplier)}, {Key: i18n.InvoiceGross, Value: Amount(v.Gross)}}
}

// downPaymentID is a down payment's box's id, by its place in the page.
func downPaymentID(i int) string { return "down-payment-" + strconv.Itoa(i+1) }

// Offered reports whether any down payment of the page may be chosen.
func (v DownPaymentsPage) Offered() bool {
	for _, c := range v.Items {
		if c.Value != "" {
			return true
		}
	}
	return false
}

// downPaymentBox is a down payment's box, checked when it was chosen,
// described by the list's error when there is one.
func downPaymentBox(i int, c DownPaymentChoice, err i18n.Key) templ.Attributes {
	a := templ.Attributes{"class": "checkbox__input", "type": "checkbox", "id": downPaymentID(i), "name": FieldDownPayment, "value": c.Value}
	if c.Chosen {
		a["checked"] = true
	}
	if err != "" {
		a["aria-describedby"] = "down-payments-error"
	}
	return a
}
