package einvoicing

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Outgoing e-invoices (ADR-0061 §8, §9, As built E2): a posted customer
// invoice or customer credit memo of a UAE company rendered as a PINT AE
// tax invoice (380) or tax credit note (381), once per customer document.

// Statuses of an outgoing e-invoice: valid (its XML kept, final) or
// invalid (its problems listed, rendered again once corrected).
const (
	StatusValid   = "valid"
	StatusInvalid = "invalid"
)

// The kinds of a customer document the format renders (receivables'
// kinds); a debit memo is not one (a specialist question: PINT AE has no
// debit note type, the coordinator's plan default 6).
const (
	saleInvoice    = "invoice"
	saleCreditMemo = "credit_memo"
	salePosted     = "posted"
)

// Sale is a customer invoice or credit memo as rendering reads it
// (receivables and the ledger): its company and customer, number, kind,
// status, date, due date, currency with its minor-unit digits, amounts,
// the invoice a credit memo credits, the document it posted from, its
// items and, once posted, the VAT its entry's output tax items carry in
// the company's currency (LocalTax).
type Sale struct {
	ID                      uuid.UUID
	Number, Kind, Status    string
	CompanyID, CustomerID   uuid.UUID
	IssueDate               time.Time
	DueDate                 *time.Time
	Currency                string
	Exponent                int
	Net, Tax, Gross         money.Minor
	ReferenceNumber         *string
	SourceID                *uuid.UUID
	SourceNumber            *string
	Lines                   []SaleLine
	LocalTax                money.Minor
	LocalCurrency, Location string // the company's currency and country
}

// SaleLine is an item: its number, description, net amount and tax, and
// its tax code with the code's UN/CEFACT category and rate on the
// document's date (ADR-0049's boundary: the code's own, no second
// mapping); "" for an item without a code.
type SaleLine struct {
	No                         int32
	Description                string
	Net, Tax                   money.Minor
	TaxCode, Category, Percent string
}

// Seller is the company as its tax invoice names it: code and name, its
// postal address in the configuration (nil when it has none) and the
// number of its FTA registration on the day ("" for none; Member when the
// registration is a tax group's it is a member of, whose TRN is not its
// own TIN).
type Seller struct {
	Company
	Address *Address
	TRN     string
	Member  bool
}

// Buyer is the customer as a tax invoice names it: its number and name,
// its legal address on the day (nil for none), its UAE TRN ("" for
// none), and whether its personal data is blocked (ADR-0079), which keeps
// it off a new document.
type Buyer struct {
	Number, Name string
	Address      *Address
	TRN          string
	Blocked      bool
}

// Sales reads what an outgoing e-invoice renders (receivables, ledger,
// tenancy, configuration, tax filing and partner contexts) in the
// request's transaction: Sale a customer document of a company the
// caller may act in (found false otherwise), Seller its company on the
// day, Buyer its customer on the day (found false for none), Customers
// customers' numbers and names by id. The TRNs are opened from their
// sealed columns, as a tax invoice prints them.
type Sales interface {
	Sale(ctx context.Context, id uuid.UUID) (Sale, bool, error)
	Seller(ctx context.Context, company uuid.UUID, day time.Time) (Seller, error)
	Buyer(ctx context.Context, customer uuid.UUID, day time.Time) (Buyer, bool, error)
	Customers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]CustomerName, error)
}

// CustomerName is a customer's number and name, as an e-invoice names it
// beside its id (the name masked for a customer whose personal data is
// blocked, ADR-0079 §11).
type CustomerName struct{ Number, Name string }

// OutgoingEInvoice is a row of einvoicing.outgoing_einvoice.
type OutgoingEInvoice struct {
	ID                    uuid.UUID
	LegalEntityID         uuid.UUID
	CustomerInvoiceID     uuid.UUID
	CustomerInvoiceNumber string
	SourceDocumentID      *uuid.UUID
	SourceDocumentNumber  *string
	CustomerID            uuid.UUID
	Kind, TypeCode        string
	IssueDate             time.Time
	Currency              string
	Exponent              int
	Net, Tax, Gross       money.Minor
	ReferenceNumber       *string
	ReasonCode            *string
	ReasonText            *string
	Specification         string
	Status                string
	Problems              []Finding
	SHA256                []byte
	Size                  *int64
	ContentRef            *string
	RenderedAt            time.Time
	Version               int32
}

const outgoingColumns = `o.id, o.legal_entity_id, o.customer_invoice_id, o.customer_invoice_number, o.source_document_id, o.source_document_number,
	o.customer_id, o.kind, o.type_code, o.issue_date, o.currency, c.exponent, o.net_amount, o.tax_amount, o.gross_amount, o.reference_invoice_number,
	o.credit_reason_code, o.credit_reason_text, o.specification, o.status, o.problems, o.sha256, o.size, o.content_ref, o.rendered_at, o.version`

// outgoingFrom names the table with its currency's digits.
const outgoingFrom = ` FROM einvoicing.outgoing_einvoice o JOIN ref.currency c ON c.code = o.currency`

func (o *OutgoingEInvoice) fields() []any {
	return []any{&o.ID, &o.LegalEntityID, &o.CustomerInvoiceID, &o.CustomerInvoiceNumber, &o.SourceDocumentID, &o.SourceDocumentNumber,
		&o.CustomerID, &o.Kind, &o.TypeCode, &o.IssueDate, &o.Currency, &o.Exponent, &o.Net, &o.Tax, &o.Gross, &o.ReferenceNumber,
		&o.ReasonCode, &o.ReasonText, &o.Specification, &o.Status, &o.Problems, &o.SHA256, &o.Size, &o.ContentRef, &o.RenderedAt, &o.Version}
}

// event is the OutgoingEInvoiceRendered payload and its audit record's
// details (api/asyncapi.yaml): no content and no TRN (ADR-0061 §12).
func (o OutgoingEInvoice) event(company Company, customerNumber string, before *string) map[string]any {
	rules := make([]string, len(o.Problems))
	for i, p := range o.Problems {
		rules[i] = p.Rule
	}
	var sum any
	if o.SHA256 != nil {
		sum = hex.EncodeToString(o.SHA256)
	}
	return map[string]any{"outgoing_einvoice_id": o.ID, "customer_invoice_id": o.CustomerInvoiceID, "customer_invoice_number": o.CustomerInvoiceNumber,
		"legal_entity_id": o.LegalEntityID, "legal_entity_code": company.Code, "customer_id": o.CustomerID, "customer_number": customerNumber,
		"kind": o.Kind, "type_code": o.TypeCode, "status": o.Status, "status_before": before, "problem_rules": rules, "sha256": sum,
		"version": o.Version}
}

// problemsJSON is the findings as the problems column keeps them: an
// empty array for none.
func problemsJSON(f []Finding) ([]byte, error) {
	if f == nil {
		f = []Finding{}
	}
	b, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("einvoicing: problems: %w", err)
	}
	return b, nil
}
