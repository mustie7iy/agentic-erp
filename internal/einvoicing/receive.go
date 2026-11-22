package einvoicing

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// PINTAERelease is the release of PINT AE Billing whose fields a received
// document is validated against (ADR-0061 §5 C2, As built T65c): 1.0.4, as
// the coordinator's plan read OpenPeppol's release on 2026-11-19; each
// processed document records it beside its own IBT-024, so a later
// release's rules can tell what was validated by which.
const PINTAERelease = "PINT AE Billing 1.0.4"

// The refusals of a received document (ADR-0061 §5), each recorded as the
// document's problem code.
const (
	RefusedUnreadable    = "einvoicing.unreadable"
	RefusedUnsupported   = "einvoicing.unsupported_document"
	RefusedMissingField  = "einvoicing.missing_field"
	RefusedPrecision     = "einvoicing.amount_precision"
	RefusedInconsistent  = "einvoicing.totals_inconsistent"
	RefusedNotAddressed  = "einvoicing.not_addressed_to_company"
	maxNamedMissingTerms = 20 // a detail names at most these, then how many more
)

// Refusal is why a received document is refused: its problem code and the
// detail a person reads (no TRN in clear, ADR-0061 §12).
type Refusal struct{ Code, Detail string }

// Received reads a received document as PINT AE and checks what ADR-0061
// §5 asks of it without master data, in this order: well-formed XML with
// no DTD or entity (unreadable); a UBL Invoice of type 380 or CreditNote
// of type 381 following PINT AE (unsupported_document); every field the
// ERP reads present and well-formed (missing_field, naming each by its
// business term, C3, C5); no amount with more decimals than its currency's
// minor unit (amount_precision); and the arithmetic that involves no tax
// rule (totals_inconsistent). Whether it is addressed to the company is
// the caller's check (addressed), as it needs the connector and the FTA
// registration. It answers what it read and the refusal, nil when none.
func Received(content []byte, exponent func(string) (int, bool)) (Document, *Refusal) {
	u, err := decodeUBL(content)
	if err != nil {
		return Document{}, &Refusal{Code: RefusedUnreadable, Detail: "The document is not well-formed XML, or declares a DTD or an entity, " +
			"which is never expanded: " + clip(err.Error()) + "."}
	}
	if r := u.supported(); r != nil {
		return Document{}, r
	}
	if missing := u.missing(); len(missing) > 0 {
		return Document{}, missingFields(missing)
	}
	d, err := u.document(exponent)
	if err != nil {
		fe := &FieldError{Term: "a field", Err: err}
		errors.As(err, &fe) // document answers *FieldError; another error it may answer is named as a field
		if fe.Precision {
			return Document{}, &Refusal{Code: RefusedPrecision, Detail: fmt.Sprintf("%s has more decimals than %s's minor unit: %s.", fe.Term,
				fe.Currency, clip(fe.Err.Error()))}
		}
		return Document{}, missingFields([]string{fe.Term + " (" + clip(fe.Err.Error()) + ")"})
	}
	return d, inconsistent(d)
}

// supported refuses a document that is not a PINT AE tax invoice (380, a
// UBL Invoice) or tax credit note (381, a UBL CreditNote) (§5 C4).
func (u ublDocument) supported() *Refusal {
	typeCode := map[string]string{nsInvoice + " Invoice": u.InvoiceType, nsCreditNote + " CreditNote": u.CreditNoteType}
	code, ok := typeCode[u.XMLName.Space+" "+u.XMLName.Local]
	want := map[string]string{"Invoice": TypeTaxInvoice, "CreditNote": TypeTaxCreditNote}[u.XMLName.Local]
	switch {
	case !ok:
		return &Refusal{Code: RefusedUnsupported, Detail: fmt.Sprintf("The document is a %q of namespace %q, not a UBL 2.1 Invoice or CreditNote.",
			clip(u.XMLName.Local), clip(u.XMLName.Space))}
	case code != "" && code != want:
		return &Refusal{Code: RefusedUnsupported, Detail: fmt.Sprintf("Its type code (IBT-003) is %q: only tax invoices (380) and tax credit notes "+
			"(381) are read.", clip(code))}
	case u.Customization != "" && u.Customization != PINTAECustomization:
		return &Refusal{Code: RefusedUnsupported, Detail: fmt.Sprintf("It follows %q (IBT-024), not PINT AE (%s).", clip(u.Customization), PINTAECustomization)}
	}
	return nil
}

// clip bounds a value the document gives that a detail quotes: the
// detail is kept (at most 2,000 characters) and shown, so a document
// cannot make it longer than that.
func clip(s string) string {
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}

func missingFields(terms []string) *Refusal {
	return &Refusal{Code: RefusedMissingField, Detail: "Missing or malformed: " + atMost(terms) + "."}
}

// atMost joins the items a detail names, at most maxNamedMissingTerms of
// them and then how many more, so a detail stays bounded.
func atMost(items []string) string {
	named := items
	if len(named) > maxNamedMissingTerms {
		named = append(named[:maxNamedMissingTerms:maxNamedMissingTerms], fmt.Sprintf("and %d more", len(items)-maxNamedMissingTerms))
	}
	return strings.Join(named, ", ")
}

// missing names the fields the ERP reads that the document lacks or holds
// malformed (§5 C3, C5), by business term.
func (u ublDocument) missing() []string {
	var out []string
	need := func(ok bool, term string) {
		if !ok {
			out = append(out, term)
		}
	}
	code := u.InvoiceType + u.CreditNoteType
	need(u.Customization != "", "IBT-024 (the specification identifier)")
	need(code != "", "IBT-003 (the type code)")
	need(u.ID != "" && len(u.ID) <= 200, "IBT-001 (the number, at most 200 characters)")
	need(u.IssueDate != "", "IBT-002 (the issue date)")
	need(u.Currency != "", "IBT-005 (the currency)")
	need(trnForm.MatchString(u.Seller.TRN), "IBT-031 (the seller's TRN, 15 digits)")
	need(trnForm.MatchString(u.Buyer.TRN), "IBT-048 (the buyer's TRN, 15 digits)")
	need(u.Buyer.Endpoint.Scheme != "" && u.Buyer.Endpoint.Value != "", "IBT-049 (the buyer's electronic address and its scheme)")
	out = append(out, u.missingTotals()...)
	lines := u.InvoiceLines
	if u.XMLName.Local == "CreditNote" {
		lines = u.CreditNoteLines
	}
	need(len(lines) > 0, "IBG-25 (a line)")
	for i, l := range lines {
		out = append(out, l.missing(fmt.Sprintf("line %d ", i+1))...)
	}
	return out
}

// missingTotals names the totals, the VAT breakdown and, for a document
// not in AED, its VAT in AED that the document lacks.
func (u ublDocument) missingTotals() []string {
	var out []string
	need := func(ok bool, term string) {
		if !ok {
			out = append(out, term)
		}
	}
	t := u.Totals
	need(t.LineTotal.Value != "", "IBT-106 (the sum of the lines' net amounts)")
	need(t.TaxExclusive.Value != "", "IBT-109 (the total without VAT)")
	need(t.TaxInclusive.Value != "", "IBT-112 (the total with VAT)")
	need(t.Payable.Value != "", "IBT-115 (the amount payable)")
	var own, aed bool
	for _, tt := range u.TaxTotals {
		switch tt.Amount.Currency {
		case u.Currency:
			own = tt.Amount.Value != ""
			need(len(tt.Subtotals) > 0, "IBG-23 (the VAT breakdown)")
			for i, s := range tt.Subtotals {
				at := fmt.Sprintf("VAT subtotal %d ", i+1)
				need(s.Taxable.Value != "", at+"IBT-116 (its taxable amount)")
				need(s.Tax.Value != "", at+"IBT-117 (its VAT)")
				need(s.Category != "", at+"IBT-118 (its category)")
				need(decimal(s.Percent), at+"IBT-119 (its rate)")
			}
		case accountingCurrency:
			aed = tt.Amount.Value != ""
		}
	}
	need(own, "IBT-110 (the VAT total in the document's currency)")
	if u.Currency != "" && u.Currency != accountingCurrency {
		need(u.TaxCurrency == accountingCurrency, "IBT-006 (the tax accounting currency, AED)")
		need(aed, "IBT-111 (the VAT total in AED)")
	}
	return out
}

// missing names what a line lacks: its item's name and description, its
// quantity and unit, its net amount, and its VAT category and rate.
func (l ublLine) missing(at string) []string {
	q := l.Invoiced
	if q.Value == "" && q.Unit == "" {
		q = l.Credited
	}
	var out []string
	for _, f := range []struct {
		ok   bool
		term string
	}{
		{l.Name != "", "IBT-153 (the item's name)"}, {l.Description != "", "IBT-154 (the item's description)"},
		{decimal(q.Value), "IBT-129 (the quantity)"}, {q.Unit != "", "IBT-130 (the unit)"},
		{l.Net.Value != "", "IBT-131 (the net amount)"}, {l.Category != "", "IBT-151 (the VAT category)"},
		{decimal(l.Percent), "IBT-152 (the VAT rate)"},
	} {
		if !f.ok {
			out = append(out, at+f.term)
		}
	}
	return out
}

// decimal says whether s is an XML Schema decimal (a sign, digits, a
// point), as parseAmount reads amounts.
func decimal(s string) bool {
	_, err := xsdDecimal(s)
	return err == nil
}

// inconsistent checks the arithmetic that involves no tax rule (§5): the
// lines' net amounts sum to their total (IBT-106), the total without VAT
// and the VAT to the total with VAT (IBT-109 + IBT-110 = IBT-112), and the
// subtotals' VAT to the VAT total (IBT-117 to IBT-110). Allowances and
// charges on the document (IBT-107, IBT-108) and amounts paid (IBT-113)
// are not read, so IBT-109 is not compared with IBT-106 nor IBT-115 with
// IBT-112; whether each subtotal's VAT is its rate's is P5's at posting.
func inconsistent(d Document) *Refusal {
	f := func(m money.Minor) string { return m.Format(d.Exponent) + " " + d.Currency }
	nets := make([]money.Minor, len(d.Lines))
	for i, l := range d.Lines {
		nets[i] = l.Net
	}
	taxes := make([]money.Minor, len(d.Subtotals))
	for i, s := range d.Subtotals {
		taxes[i] = s.Tax
	}
	lines, err1 := money.Sum(nets...)
	gross, err2 := money.Sum(d.TaxExclusive, d.Tax)
	subtotals, err3 := money.Sum(taxes...)
	var found []string
	switch {
	case errors.Join(err1, err2, err3) != nil:
		found = append(found, "an amount is out of range")
	default:
		if lines != d.LineTotal {
			found = append(found, fmt.Sprintf("the lines' net amounts sum to %s, not the %s stated (IBT-106)", f(lines), f(d.LineTotal)))
		}
		if gross != d.TaxInclusive {
			found = append(found, fmt.Sprintf("the total without VAT and the VAT sum to %s, not the %s stated (IBT-112)", f(gross), f(d.TaxInclusive)))
		}
		if subtotals != d.Tax {
			found = append(found, fmt.Sprintf("the VAT subtotals sum to %s, not the %s stated (IBT-110)", f(subtotals), f(d.Tax)))
		}
		found = append(found, inconsistentInAED(d)...)
	}
	if len(found) == 0 {
		return nil
	}
	return &Refusal{Code: RefusedInconsistent, Detail: strings.ToUpper(found[0][:1]) + strings.Join(found, "; ")[1:] + "."}
}

// LineTaxInAED is the VAT in AED a document states per line (BTAE-08),
// summed, and whether every line states it: what a supplier invoice keeps
// as its tax in AED for the VAT 201 (ADR-0061 §5 C5, As built batch 88;
// po:2026-11-21). An exempt line (E) states none (IBR-163-AE, as the
// coordinator's research read the AE rules), so it is not counted as
// missing.
func LineTaxInAED(d Document) (money.Minor, bool, error) {
	var taxes []money.Minor
	for _, l := range d.Lines {
		switch {
		case l.AccountingTax != nil:
			taxes = append(taxes, *l.AccountingTax)
		case l.Category != "E":
			return 0, false, nil
		}
	}
	sum, err := money.Sum(taxes...)
	if err != nil {
		return 0, false, fmt.Errorf("einvoicing: the lines' VAT in AED: %w", err)
	}
	return sum, true, nil
}

// inconsistentInAED names a document whose lines' VAT in AED (BTAE-08),
// when every line states it, does not sum to its VAT in AED: IBT-111 for
// a document in another currency, its VAT (IBT-110) for one in AED.
func inconsistentInAED(d Document) []string {
	sum, all, err := LineTaxInAED(d)
	switch {
	case err != nil:
		return []string{"the lines' VAT in AED (BTAE-08) is out of range"}
	case !all:
		return nil
	}
	want, term := d.Tax, "IBT-110"
	if d.Currency != accountingCurrency {
		if d.AccountingTax == nil {
			return nil // missingTotals names a document not in AED without IBT-111
		}
		want, term = *d.AccountingTax, "IBT-111"
	}
	if sum == want {
		return nil
	}
	return []string{fmt.Sprintf("the lines' VAT in AED (BTAE-08) sums to %s AED, not the %s AED stated (%s)", sum.Format(aedExponent),
		want.Format(aedExponent), term)}
}
