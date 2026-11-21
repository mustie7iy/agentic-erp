package einvoicing

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The PINT AE Billing identifiers the format renders and checks (ADR-0061
// §5 C1-C2, As built E2): the specification (IBT-024) and the business
// process (IBT-023), as the coordinator's plan records them from
// OpenPeppol's PINT AE Billing 1.0.4 (einvoicing-evatra.md, Facts).
const (
	PINTAECustomization = "urn:peppol:pint:billing-1@ae-1"
	PINTAEProfile       = "urn:peppol:bis:billing"
)

// The UBL 2.1 namespaces of the documents (ADR-0061 §5 C2: UBL 2.1 XML).
const (
	nsInvoice    = "urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"
	nsCreditNote = "urn:oasis:names:specification:ubl:schema:xsd:CreditNote-2" // #nosec G101 -- a UBL namespace, not a credential
	nsCAC        = "urn:oasis:names:specification:ubl:schema:xsd:CommonAggregateComponents-2"
	nsCBC        = "urn:oasis:names:specification:ubl:schema:xsd:CommonBasicComponents-2"
)

// Kinds of an outgoing e-invoice and their PINT AE type codes (IBT-003;
// ADR-0061 §5 C4: 380 an invoice, 381 a credit note sent as a UBL
// CreditNote).
const (
	KindTaxInvoice    = "invoice"
	KindTaxCreditNote = "credit_note"
	TypeTaxInvoice    = "380"
	TypeTaxCreditNote = "381"
)

// Document is an e-invoice as PINT AE's semantic model names it: the
// fields the ERP renders and the fields §5 reads of a received one, each
// commented with its business term. Amounts are minor units of Currency,
// whose minor-unit digits are Exponent; AccountingTax (IBT-111) is the
// VAT in the tax accounting currency (AED), nil when Currency is AED.
type Document struct {
	Kind            string
	TypeCode        string     // IBT-003
	Number          string     // IBT-001
	IssueDate       time.Time  // IBT-002
	DueDate         *time.Time // IBT-009, an invoice's
	Currency        string     // IBT-005
	TaxCurrency     string     // IBT-006, "" when Currency is AED
	Customization   string     // IBT-024
	Profile         string     // IBT-023
	TransactionType string     // BTAE-02, eight flags
	Reference       string     // IBT-025, the invoice a credit note corrects
	Cited           []string   // IBT-025 of each IBG-03 a received document states, Reference first
	OrderReference  string     // IBT-013, a received document's purchase order
	Reason          *Reason    // BTAE-03, a credit note's
	Seller, Buyer   Party
	PaymentMeans    string // IBT-081
	Lines           []Line
	Subtotals       []Subtotal   // IBG-23
	Tax             money.Minor  // IBT-110
	AccountingTax   *money.Minor // IBT-111, in AccountingCurrency
	// AccountingCurrency is the currency IBT-111 is stated in, "" without it.
	AccountingCurrency string
	LineTotal          money.Minor // IBT-106
	TaxExclusive       money.Minor // IBT-109
	TaxInclusive       money.Minor // IBT-112
	Payable            money.Minor // IBT-115
	Exponent           int
}

// Reason is a credit note's reason (BTAE-03): its code and its words.
type Reason struct{ Code, Text string }

// Party is the seller (IBG-04) or the buyer (IBG-07): its electronic
// address (IBT-034/IBT-049, with its scheme), name (IBT-027/IBT-044), VAT
// TRN (IBT-031/IBT-048) and postal address.
type Party struct {
	EndpointScheme, Endpoint string
	Name                     string
	TRN                      string
	Address                  Address
}

// Address is a party's postal address (IBG-05/IBG-08): line 1 and 2, city,
// post code, country subdivision (the emirate's code, IBT-039/IBT-054) and
// country code.
type Address struct {
	Line1, Line2, City, PostalCode, Subentity, Country string
}

// Line is an invoice line (IBG-25): its id (IBT-126), quantity and unit
// (IBT-129, IBT-130), net amount (IBT-131), net price (IBT-146), item name
// and description (IBT-153, IBT-154) and VAT category and rate (IBT-151,
// IBT-152); a received line names its order line (IBT-132) if any.
type Line struct {
	ID                string
	OrderLine         string // IBT-132, a received line's order line
	Quantity, Unit    string
	Net, Price        money.Minor
	Name, Description string
	Category, Percent string
}

// Subtotal is the VAT of one category and rate (IBG-23): taxable amount
// (IBT-116), tax (IBT-117), category (IBT-118) and rate (IBT-119).
type Subtotal struct {
	Category, Percent string
	Taxable, Tax      money.Minor
}

// errDTD refuses a document declaring a DTD: no entity is ever expanded
// (ADR-0061 §5).
var errDTD = errors.New("the document declares a DTD")

// ParsePINTAE reads a PINT AE invoice or credit note into a Document
// (ADR-0061 §5): parsed strictly, refusing any DTD, so no entity is
// expanded, its amounts in the minor units of its currency, whose digits
// exponent gives (false: an unknown currency). It reads what the format
// renders; what is missing stays empty for the rules (Check) to name. A
// field it cannot read is a *FieldError naming its business term.
func ParsePINTAE(content []byte, exponent func(string) (int, bool)) (Document, error) {
	u, err := decodeUBL(content)
	if err != nil {
		return Document{}, err
	}
	return u.document(exponent)
}

// FieldError is a field of a PINT AE document that could not be read: its
// business term, and whether it is an amount with more decimals than its
// currency's minor unit (Precision) rather than malformed (ADR-0061 §5:
// einvoicing.amount_precision and einvoicing.missing_field).
type FieldError struct {
	Term      string
	Currency  string // an amount's currency, for Precision
	Precision bool
	Err       error
}

func (e *FieldError) Error() string { return "einvoicing: PINT AE " + e.Term + ": " + e.Err.Error() }

func (e *FieldError) Unwrap() error { return e.Err }

// decodeUBL reads the XML into its UBL binding, refusing a DTD first.
func decodeUBL(content []byte) (ublDocument, error) {
	if err := noDirectives(content); err != nil {
		return ublDocument{}, err
	}
	var u ublDocument
	d := xml.NewDecoder(bytes.NewReader(content))
	d.Strict = true
	if err := d.Decode(&u); err != nil {
		return ublDocument{}, fmt.Errorf("einvoicing: parse PINT AE: %w", err)
	}
	return u, nil
}

// noDirectives refuses a document with a directive (<!DOCTYPE ...>) before
// it is decoded.
func noDirectives(content []byte) error {
	d := xml.NewDecoder(bytes.NewReader(content))
	d.Strict = true
	for {
		t, err := d.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("einvoicing: parse PINT AE: %w", err)
		}
		if _, ok := t.(xml.Directive); ok {
			return errDTD
		}
	}
}
