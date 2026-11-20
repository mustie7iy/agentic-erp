package einvoicing

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// ublDocument is a UBL 2.1 Invoice or CreditNote as PINT AE's syntax
// binding places the fields Document holds (ADR-0061 §5 C3, C5; the
// paths of BTAE-02, BTAE-03 and the endpoints from memory, for the
// specialist: ADR-0061 As built E2). Elements are matched by local name;
// the root's namespace says which document it is. A credit note's
// CreditNoteTypeCode and CreditNoteLine land in the same fields.
type ublDocument struct {
	XMLName         xml.Name
	Customization   string      `xml:"CustomizationID"`
	Profile         string      `xml:"ProfileID"`
	ProfileExec     string      `xml:"ProfileExecutionID"`
	ID              string      `xml:"ID"`
	IssueDate       string      `xml:"IssueDate"`
	DueDate         string      `xml:"DueDate"`
	InvoiceType     string      `xml:"InvoiceTypeCode"`
	CreditNoteType  string      `xml:"CreditNoteTypeCode"`
	Currency        string      `xml:"DocumentCurrencyCode"`
	TaxCurrency     string      `xml:"TaxCurrencyCode"`
	Discrepancy     *ublReason  `xml:"DiscrepancyResponse"`
	BillingRef      string      `xml:"BillingReference>InvoiceDocumentReference>ID"`
	OrderRef        string      `xml:"OrderReference>ID"`
	Seller          ublParty    `xml:"AccountingSupplierParty>Party"`
	Buyer           ublParty    `xml:"AccountingCustomerParty>Party"`
	PaymentMeans    string      `xml:"PaymentMeans>PaymentMeansCode"`
	TaxTotals       []ublTaxTot `xml:"TaxTotal"`
	Totals          ublTotals   `xml:"LegalMonetaryTotal"`
	InvoiceLines    []ublLine   `xml:"InvoiceLine"`
	CreditNoteLines []ublLine   `xml:"CreditNoteLine"`
}

type ublReason struct {
	Code string `xml:"ResponseCode"`
	Text string `xml:"Description"`
}

type ublParty struct {
	Endpoint ublID   `xml:"EndpointID"`
	Address  ublAddr `xml:"PostalAddress"`
	TRN      string  `xml:"PartyTaxScheme>CompanyID"`
	Name     string  `xml:"PartyLegalEntity>RegistrationName"`
}

type ublID struct {
	Scheme string `xml:"schemeID,attr"`
	Value  string `xml:",chardata"`
}

type ublAddr struct {
	Line1      string `xml:"StreetName"`
	Line2      string `xml:"AdditionalStreetName"`
	City       string `xml:"CityName"`
	PostalCode string `xml:"PostalZone"`
	Subentity  string `xml:"CountrySubentity"`
	Country    string `xml:"Country>IdentificationCode"`
}

type ublAmount struct {
	Currency string `xml:"currencyID,attr"`
	Value    string `xml:",chardata"`
}

type ublTaxTot struct {
	Amount    ublAmount   `xml:"TaxAmount"`
	Subtotals []ublSubtot `xml:"TaxSubtotal"`
}

type ublSubtot struct {
	Taxable  ublAmount `xml:"TaxableAmount"`
	Tax      ublAmount `xml:"TaxAmount"`
	Category string    `xml:"TaxCategory>ID"`
	Percent  string    `xml:"TaxCategory>Percent"`
}

type ublTotals struct {
	LineTotal    ublAmount `xml:"LineExtensionAmount"`
	TaxExclusive ublAmount `xml:"TaxExclusiveAmount"`
	TaxInclusive ublAmount `xml:"TaxInclusiveAmount"`
	Payable      ublAmount `xml:"PayableAmount"`
}

type ublQuantity struct {
	Unit  string `xml:"unitCode,attr"`
	Value string `xml:",chardata"`
}

type ublLine struct {
	ID          string      `xml:"ID"`
	OrderLine   string      `xml:"OrderLineReference>LineID"`
	Invoiced    ublQuantity `xml:"InvoicedQuantity"`
	Credited    ublQuantity `xml:"CreditedQuantity"`
	Net         ublAmount   `xml:"LineExtensionAmount"`
	Description string      `xml:"Item>Description"`
	Name        string      `xml:"Item>Name"`
	Category    string      `xml:"Item>ClassifiedTaxCategory>ID"`
	Percent     string      `xml:"Item>ClassifiedTaxCategory>Percent"`
	Price       ublAmount   `xml:"Price>PriceAmount"`
}

// amounts parses a document's amounts, each in the document's currency,
// with at most exp decimals (fewer are an XML Schema decimal's, padded):
// an amount in another currency or not a decimal is a malformed field, one
// of more decimals a field of too much precision (FieldError.Precision).
type amounts struct {
	currency string
	exp      int
	err      error
}

func (a *amounts) of(v ublAmount, term string) money.Minor {
	if a.err != nil || v.Value == "" {
		return 0
	}
	if v.Currency != a.currency {
		a.err = &FieldError{Term: term, Err: fmt.Errorf("in %q, not the document's %s", v.Currency, a.currency)}
		return 0
	}
	m, err := parseAmount(v.Value, a.exp)
	if err != nil {
		a.err = &FieldError{Term: term, Currency: a.currency, Precision: errors.Is(err, errPrecision), Err: err}
	}
	return m
}

// errPrecision is an amount with more decimals than its currency's minor
// unit, zeros included (ADR-0061 §5: einvoicing.amount_precision).
var errPrecision = errors.New("more decimals than the currency's minor unit")

// parseAmount reads an XML Schema decimal amount (digits, a sign, at most
// exp decimals; fewer are padded) into minor units.
func parseAmount(raw string, exp int) (money.Minor, error) {
	s := strings.TrimSpace(raw)
	if strings.HasSuffix(s, "-") {
		return 0, fmt.Errorf("amount %q is not a decimal", raw)
	}
	if _, frac, _ := strings.Cut(s, "."); len(frac) > exp {
		if _, err := money.ParseDecimalRat(s); err != nil {
			return 0, fmt.Errorf("amount %q is not a decimal", raw)
		}
		return 0, fmt.Errorf("amount %q: %w", raw, errPrecision)
	}
	m, err := money.ParseDecimal(s, exp)
	if err != nil {
		return 0, fmt.Errorf("amount %q: %w", raw, err)
	}
	return m, nil
}

// document turns the UBL into a Document.
func (u ublDocument) document(exponent func(string) (int, bool)) (Document, error) {
	exp, ok := exponent(u.Currency)
	if !ok {
		return Document{}, &FieldError{Term: "IBT-005", Err: fmt.Errorf("unknown currency %q", u.Currency)}
	}
	d := Document{Number: u.ID, Currency: u.Currency, TaxCurrency: u.TaxCurrency, Customization: u.Customization, Profile: u.Profile,
		TransactionType: u.ProfileExec, Reference: u.BillingRef, OrderReference: u.OrderRef, Seller: u.Seller.party(), Buyer: u.Buyer.party(),
		PaymentMeans: u.PaymentMeans, Exponent: exp}
	d.Kind, d.TypeCode = KindTaxInvoice, u.InvoiceType
	lines := u.InvoiceLines
	if u.XMLName.Space == nsCreditNote {
		d.Kind, d.TypeCode, lines = KindTaxCreditNote, u.CreditNoteType, u.CreditNoteLines
	}
	if u.Discrepancy != nil {
		d.Reason = &Reason{Code: u.Discrepancy.Code, Text: u.Discrepancy.Text}
	}
	var err error
	if d.IssueDate, d.DueDate, err = u.dates(); err != nil {
		return Document{}, err
	}
	a := &amounts{currency: u.Currency, exp: exp}
	d.Lines = make([]Line, len(lines))
	for i, l := range lines {
		d.Lines[i] = l.line(a, d.Kind)
	}
	if err := u.taxes(&d, a, exponent); err != nil {
		return Document{}, err
	}
	d.LineTotal, d.TaxExclusive = a.of(u.Totals.LineTotal, "IBT-106"), a.of(u.Totals.TaxExclusive, "IBT-109")
	d.TaxInclusive, d.Payable = a.of(u.Totals.TaxInclusive, "IBT-112"), a.of(u.Totals.Payable, "IBT-115")
	return d, a.err
}

// dates parses the issue date and, when given, the due date.
func (u ublDocument) dates() (time.Time, *time.Time, error) {
	issued, err := time.Parse(time.DateOnly, u.IssueDate)
	if err != nil {
		return time.Time{}, nil, &FieldError{Term: "IBT-002", Err: err}
	}
	if u.DueDate == "" {
		return issued, nil, nil
	}
	due, err := time.Parse(time.DateOnly, u.DueDate)
	if err != nil {
		return time.Time{}, nil, &FieldError{Term: "IBT-009", Err: err}
	}
	return issued, &due, nil
}

// taxes reads the tax totals: the one in the document's currency with its
// subtotals (IBT-110, IBG-23), and the one in another currency, the tax
// accounting currency's (IBT-111), when the document is not in it.
func (u ublDocument) taxes(d *Document, a *amounts, exponent func(string) (int, bool)) error {
	for _, t := range u.TaxTotals {
		if t.Amount.Currency == d.Currency {
			d.Tax = a.of(t.Amount, "IBT-110")
			for _, s := range t.Subtotals {
				d.Subtotals = append(d.Subtotals, Subtotal{Category: s.Category, Percent: s.Percent,
					Taxable: a.of(s.Taxable, "IBT-116"), Tax: a.of(s.Tax, "IBT-117")})
			}
			continue
		}
		exp, ok := exponent(t.Amount.Currency)
		if !ok {
			return &FieldError{Term: "IBT-111", Err: fmt.Errorf("unknown currency %q", t.Amount.Currency)}
		}
		other := &amounts{currency: t.Amount.Currency, exp: exp}
		tax := other.of(t.Amount, "IBT-111")
		if other.err != nil {
			return other.err
		}
		d.AccountingTax, d.AccountingCurrency = &tax, t.Amount.Currency
	}
	return nil
}

func (p ublParty) party() Party {
	return Party{EndpointScheme: p.Endpoint.Scheme, Endpoint: p.Endpoint.Value, Name: p.Name, TRN: p.TRN,
		Address: Address{Line1: p.Address.Line1, Line2: p.Address.Line2, City: p.Address.City, PostalCode: p.Address.PostalCode,
			Subentity: p.Address.Subentity, Country: p.Address.Country}}
}

func (l ublLine) line(a *amounts, kind string) Line {
	q := l.Invoiced
	if kind == KindTaxCreditNote {
		q = l.Credited
	}
	return Line{ID: l.ID, OrderLine: l.OrderLine, Quantity: q.Value, Unit: q.Unit, Net: a.of(l.Net, "IBT-131"), Price: a.of(l.Price, "IBT-146"),
		Name: l.Name, Description: l.Description, Category: l.Category, Percent: l.Percent}
}
