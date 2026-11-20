package einvoicing

import (
	"bytes"
	"encoding/xml"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// accountingCurrency is the UAE's tax accounting currency (IBT-006): a
// document in another currency states its VAT in it too (IBT-111;
// ADR-0061 §5 C5).
const accountingCurrency = "AED"

// RenderPINTAE writes the Document as UBL 2.1 XML in PINT AE's element
// order (ADR-0061 §9, As built E2): an Invoice for a tax invoice, a
// CreditNote for a tax credit note. The XML carries no time of rendering,
// so the same Document renders the same bytes. Only a Document that
// passes Check is rendered for keeping; RenderPINTAE itself checks
// nothing.
func RenderPINTAE(d Document) []byte {
	w := &ublWriter{exp: d.Exponent}
	root, ns, typeTag, lineTag, qtyTag := "Invoice", nsInvoice, "cbc:InvoiceTypeCode", "cac:InvoiceLine", "cbc:InvoicedQuantity"
	if d.Kind == KindTaxCreditNote {
		root, ns, typeTag, lineTag, qtyTag = "CreditNote", nsCreditNote, "cbc:CreditNoteTypeCode", "cac:CreditNoteLine", "cbc:CreditedQuantity"
	}
	w.buf.WriteString(xml.Header)
	w.open(root, "xmlns", ns, "xmlns:cac", nsCAC, "xmlns:cbc", nsCBC)
	w.text("cbc:CustomizationID", d.Customization)
	w.text("cbc:ProfileID", d.Profile)
	w.text("cbc:ProfileExecutionID", d.TransactionType)
	w.text("cbc:ID", d.Number)
	w.text("cbc:IssueDate", d.IssueDate.Format(time.DateOnly))
	if d.DueDate != nil && d.Kind == KindTaxInvoice {
		w.text("cbc:DueDate", d.DueDate.Format(time.DateOnly))
	}
	w.text(typeTag, d.TypeCode)
	w.text("cbc:DocumentCurrencyCode", d.Currency)
	w.optional("cbc:TaxCurrencyCode", d.TaxCurrency)
	if d.Reason != nil {
		w.open("cac:DiscrepancyResponse")
		w.text("cbc:ResponseCode", d.Reason.Code)
		w.text("cbc:Description", d.Reason.Text)
		w.close("cac:DiscrepancyResponse")
	}
	if d.Reference != "" {
		w.open("cac:BillingReference")
		w.open("cac:InvoiceDocumentReference")
		w.text("cbc:ID", d.Reference)
		w.close("cac:InvoiceDocumentReference")
		w.close("cac:BillingReference")
	}
	w.party("cac:AccountingSupplierParty", d.Seller)
	w.party("cac:AccountingCustomerParty", d.Buyer)
	w.open("cac:PaymentMeans")
	w.text("cbc:PaymentMeansCode", d.PaymentMeans)
	w.close("cac:PaymentMeans")
	w.taxTotals(d)
	w.totals(d)
	for _, l := range d.Lines {
		w.line(lineTag, qtyTag, d.Currency, l)
	}
	w.close(root)
	return w.buf.Bytes()
}

// ublWriter writes elements in order, escaping their text.
type ublWriter struct {
	buf bytes.Buffer
	exp int
}

func (w *ublWriter) open(tag string, attrs ...string) {
	w.buf.WriteString("<" + tag)
	for i := 0; i+1 < len(attrs); i += 2 {
		w.buf.WriteString(" " + attrs[i] + `="`)
		w.escape(attrs[i+1])
		w.buf.WriteString(`"`)
	}
	w.buf.WriteString(">")
}

func (w *ublWriter) close(tag string) { w.buf.WriteString("</" + tag + ">") }

// escape writes s as XML text; writing to a bytes.Buffer never fails.
func (w *ublWriter) escape(s string) { _ = xml.EscapeText(&w.buf, []byte(s)) }

func (w *ublWriter) text(tag, value string, attrs ...string) {
	w.open(tag, attrs...)
	w.escape(value)
	w.close(tag)
}

// optional writes the element only when it has a value.
func (w *ublWriter) optional(tag, value string) {
	if value != "" {
		w.text(tag, value)
	}
}

func (w *ublWriter) amount(tag string, m money.Minor, currency string) {
	w.text(tag, m.Plain(w.exp), "currencyID", currency)
}

func (w *ublWriter) party(tag string, p Party) {
	w.open(tag)
	w.open("cac:Party")
	w.text("cbc:EndpointID", p.Endpoint, "schemeID", p.EndpointScheme)
	w.open("cac:PostalAddress")
	w.optional("cbc:StreetName", p.Address.Line1)
	w.optional("cbc:AdditionalStreetName", p.Address.Line2)
	w.optional("cbc:CityName", p.Address.City)
	w.optional("cbc:PostalZone", p.Address.PostalCode)
	w.optional("cbc:CountrySubentity", p.Address.Subentity)
	w.open("cac:Country")
	w.text("cbc:IdentificationCode", p.Address.Country)
	w.close("cac:Country")
	w.close("cac:PostalAddress")
	if p.TRN != "" {
		w.open("cac:PartyTaxScheme")
		w.text("cbc:CompanyID", p.TRN)
		w.taxScheme()
		w.close("cac:PartyTaxScheme")
	}
	w.open("cac:PartyLegalEntity")
	w.text("cbc:RegistrationName", p.Name)
	w.close("cac:PartyLegalEntity")
	w.close("cac:Party")
	w.close(tag)
}

func (w *ublWriter) taxScheme() {
	w.open("cac:TaxScheme")
	w.text("cbc:ID", "VAT")
	w.close("cac:TaxScheme")
}

// taxCategory writes a category with its rate (IBT-118/IBT-119, or a
// line's IBT-151/IBT-152 under tag).
func (w *ublWriter) taxCategory(tag, category, percent string) {
	w.open(tag)
	w.text("cbc:ID", category)
	w.text("cbc:Percent", percent)
	w.taxScheme()
	w.close(tag)
}

func (w *ublWriter) taxTotals(d Document) {
	w.open("cac:TaxTotal")
	w.amount("cbc:TaxAmount", d.Tax, d.Currency)
	for _, s := range d.Subtotals {
		w.open("cac:TaxSubtotal")
		w.amount("cbc:TaxableAmount", s.Taxable, d.Currency)
		w.amount("cbc:TaxAmount", s.Tax, d.Currency)
		w.taxCategory("cac:TaxCategory", s.Category, s.Percent)
		w.close("cac:TaxSubtotal")
	}
	w.close("cac:TaxTotal")
	if d.AccountingTax != nil {
		w.open("cac:TaxTotal")
		w.amount("cbc:TaxAmount", *d.AccountingTax, d.AccountingCurrency)
		w.close("cac:TaxTotal")
	}
}

func (w *ublWriter) totals(d Document) {
	w.open("cac:LegalMonetaryTotal")
	w.amount("cbc:LineExtensionAmount", d.LineTotal, d.Currency)
	w.amount("cbc:TaxExclusiveAmount", d.TaxExclusive, d.Currency)
	w.amount("cbc:TaxInclusiveAmount", d.TaxInclusive, d.Currency)
	w.amount("cbc:PayableAmount", d.Payable, d.Currency)
	w.close("cac:LegalMonetaryTotal")
}

func (w *ublWriter) line(tag, quantityTag, currency string, l Line) {
	w.open(tag)
	w.text("cbc:ID", l.ID)
	w.text(quantityTag, l.Quantity, "unitCode", l.Unit)
	w.amount("cbc:LineExtensionAmount", l.Net, currency)
	w.open("cac:Item")
	w.text("cbc:Description", l.Description)
	w.text("cbc:Name", l.Name)
	w.taxCategory("cac:ClassifiedTaxCategory", l.Category, l.Percent)
	w.close("cac:Item")
	w.open("cac:Price")
	w.amount("cbc:PriceAmount", l.Price, currency)
	w.close("cac:Price")
	w.close(tag)
}
