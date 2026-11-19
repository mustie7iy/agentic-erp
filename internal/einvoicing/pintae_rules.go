package einvoicing

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Finding is a mandatory field a rule found missing or malformed: the
// rule's code, the business term it checks and what is wrong.
type Finding struct {
	Rule    string `json:"rule"`
	Term    string `json:"term"`
	Message string `json:"message"`
}

// rule is one check of a Document (ADR-0061 §5's rule machinery, As built
// E2): the mandatory fields of an electronic tax invoice and credit note,
// as the UAE tax specialist listed them (po:2026-11-09, C3-C6), and the
// arithmetic that involves no tax rule. Each finds what is wrong, or
// nothing.
type rule struct {
	code, term string
	check      func(Document) []string
}

// Emirates are the PINT AE codes of the seven emirates, mandatory as a UAE
// address's country subdivision (IBT-039, IBT-054; IBR-128-AE, fatal, as
// the coordinator's plan records it from PINT AE Billing 1.0.4).
var Emirates = []string{"AUH", "DXB", "SHJ", "UAQ", "FUJ", "AJM", "RAK"}

// Categories are the VAT categories the format renders (UN/CEFACT 5305):
// standard, zero-rated, exempt, out of scope and reverse charge.
var Categories = []string{"S", "Z", "E", "O", "AE"}

// The forms of a TRN (15 digits; ADR-0061 §5 C6), of the endpoint
// identifier (scheme 0235 and the TIN, the first ten digits of the TRN)
// and of BTAE-02's eight transaction type flags.
var (
	trnForm   = regexp.MustCompile(`^[0-9]{15}$`)
	tinForm   = regexp.MustCompile(`^[0-9]{10}$`)
	flagsForm = regexp.MustCompile(`^[01]{8}$`)
)

// EndpointScheme is the scheme of a UAE business's electronic address (C6).
const EndpointScheme = "0235"

func required(value, what string) []string {
	if value == "" {
		return []string{what + " is missing"}
	}
	return nil
}

var rules = []rule{
	{"einvoicing.pint_ae.specification", "IBT-024", func(d Document) []string {
		return mismatch(d.Customization, PINTAECustomization, "the specification identifier")
	}},
	{"einvoicing.pint_ae.profile", "IBT-023", func(d Document) []string {
		return mismatch(d.Profile, PINTAEProfile, "the business process")
	}},
	{"einvoicing.pint_ae.type_code", "IBT-003", typeCode},
	{"einvoicing.pint_ae.number", "IBT-001", func(d Document) []string { return required(d.Number, "the document number") }},
	{"einvoicing.pint_ae.transaction_type", "BTAE-02", func(d Document) []string {
		if !flagsForm.MatchString(d.TransactionType) {
			return []string{"the transaction type is not eight flags of 0 or 1"}
		}
		return nil
	}},
	{"einvoicing.pint_ae.tax_currency", "IBT-006", taxCurrency},
	{"einvoicing.pint_ae.credited_invoice", "IBT-025", func(d Document) []string {
		if d.Kind == KindTaxCreditNote {
			return required(d.Reference, "the invoice the credit note corrects")
		}
		return nil
	}},
	{"einvoicing.pint_ae.credit_reason", "BTAE-03", creditReason},
	{"einvoicing.pint_ae.seller", "IBG-04", func(d Document) []string { return party(d.Seller, "the seller's", true) }},
	{"einvoicing.pint_ae.buyer", "IBG-07", func(d Document) []string { return party(d.Buyer, "the buyer's", false) }},
	{"einvoicing.pint_ae.payment_means", "IBT-081", func(d Document) []string { return required(d.PaymentMeans, "the payment means") }},
	{"einvoicing.pint_ae.lines", "IBG-25", lines},
	{"einvoicing.pint_ae.totals", "IBG-22", totals},
	{"einvoicing.pint_ae.vat_breakdown", "IBG-23", breakdown},
}

// Check runs every rule on the Document, in order, and returns what they
// found; none: the document may be rendered as valid.
func Check(d Document) []Finding {
	var out []Finding
	for _, r := range rules {
		for _, m := range r.check(d) {
			out = append(out, Finding{Rule: r.code, Term: r.term, Message: m})
		}
	}
	return out
}

func mismatch(got, want, what string) []string {
	if got != want {
		return []string{fmt.Sprintf("%s is %q, not %s", what, got, want)}
	}
	return nil
}

func typeCode(d Document) []string {
	want := map[string]string{KindTaxInvoice: TypeTaxInvoice, KindTaxCreditNote: TypeTaxCreditNote}[d.Kind]
	if want == "" {
		return []string{"the document is neither a tax invoice nor a tax credit note"}
	}
	return mismatch(d.TypeCode, want, "the type code")
}

func taxCurrency(d Document) []string {
	if d.Currency == accountingCurrency {
		if d.TaxCurrency != "" || d.AccountingTax != nil {
			return []string{"a document in AED states no separate tax accounting currency"}
		}
		return nil
	}
	if d.TaxCurrency != accountingCurrency || d.AccountingTax == nil || d.AccountingCurrency != accountingCurrency {
		return []string{"a document in " + d.Currency + " states its tax accounting currency AED and its VAT in AED (IBT-111)"}
	}
	return nil
}

func creditReason(d Document) []string {
	if d.Kind != KindTaxCreditNote {
		if d.Reason != nil {
			return []string{"a tax invoice carries no credit note reason"}
		}
		return nil
	}
	if d.Reason == nil || d.Reason.Code == "" || d.Reason.Text == "" {
		return []string{"the credit note's reason, its code and words, is missing"}
	}
	return nil
}

// party checks a party's mandatory fields: its name, TRN, electronic
// address (scheme 0235 and a ten-digit TIN; the seller's the first ten
// digits of its TRN), address line 1, city, country and, in the UAE, the
// emirate. A buyer outside the UAE (exports) is not rendered by E2.
func party(p Party, whose string, seller bool) []string {
	var out []string
	out = append(out, required(p.Name, whose+" name")...)
	if !trnForm.MatchString(p.TRN) {
		out = append(out, whose+" TRN is missing or not 15 digits")
	}
	switch {
	case p.EndpointScheme != EndpointScheme || !tinForm.MatchString(p.Endpoint):
		out = append(out, whose+" electronic address is not scheme 0235 with a ten-digit TIN")
	case seller && trnForm.MatchString(p.TRN) && p.Endpoint != p.TRN[:10]:
		out = append(out, whose+" electronic address is not the first ten digits of its TRN")
	}
	out = append(out, required(p.Address.Line1, whose+" address line 1")...)
	out = append(out, required(p.Address.City, whose+" city")...)
	if p.Address.Country != "AE" {
		out = append(out, whose+" country is "+quoted(p.Address.Country)+", not AE")
	} else if !slices.Contains(Emirates, p.Address.Subentity) {
		out = append(out, whose+" emirate is "+quoted(p.Address.Subentity)+", not one of AUH, DXB, SHJ, UAQ, FUJ, AJM, RAK")
	}
	return out
}

func quoted(s string) string {
	if s == "" {
		return "missing"
	}
	return fmt.Sprintf("%q", s)
}

// lines checks each line: an id, a positive quantity with its unit, a name
// and description, a category the format renders with its rate (above
// zero exactly for S), and amounts not below zero.
func lines(d Document) []string {
	if len(d.Lines) == 0 {
		return []string{"the document has no line"}
	}
	var out []string
	for i, l := range d.Lines {
		at := fmt.Sprintf("line %d: ", i+1)
		if l.ID == "" || l.Unit == "" || money.PositiveQuantity(l.Quantity) != nil {
			out = append(out, at+"its id, quantity or unit is missing")
		}
		if l.Name == "" || l.Description == "" {
			out = append(out, at+"its item name or description is missing")
		}
		if !slices.Contains(Categories, l.Category) {
			out = append(out, at+"its VAT category is "+quoted(l.Category)+", not one of S, Z, E, O, AE")
		} else if !fits(l.Category, l.Percent) {
			out = append(out, at+"its VAT rate "+quoted(l.Percent)+" does not fit category "+l.Category)
		}
		if l.Net < 0 || l.Price < 0 {
			out = append(out, at+"an amount is below zero")
		}
	}
	return out
}

// fits reports whether a rate fits its category: a plain decimal, above
// zero for S (standard) and zero for the others.
func fits(category, percent string) bool {
	r, err := money.ParseDecimalRat(percent)
	return err == nil && (r.Sign() > 0) == (category == "S") && r.Sign() >= 0
}

// totals checks the arithmetic that involves no tax rule: the lines sum to
// the net total, net and VAT to the gross, which is what is payable.
func totals(d Document) []string {
	nets := make([]money.Minor, len(d.Lines))
	for i, l := range d.Lines {
		nets[i] = l.Net
	}
	sum, err := money.Sum(nets...)
	if err != nil || sum != d.LineTotal || d.LineTotal != d.TaxExclusive {
		return []string{"the lines' net amounts do not add up to the net total"}
	}
	gross, err := money.Sum(d.TaxExclusive, d.Tax)
	if err != nil || gross != d.TaxInclusive || d.TaxInclusive != d.Payable {
		return []string{"the net total and the VAT do not add up to the gross total payable"}
	}
	return nil
}

// breakdown checks the VAT per category and rate: one subtotal for each
// category and rate the lines name, its taxable amount theirs, and the
// subtotals' VAT the document's. Whether each subtotal's VAT is the rate's
// is not checked here (ADR-0061 §5: P5 at posting, rounding to confirm).
func breakdown(d Document) []string {
	taxable := map[[2]string][]money.Minor{}
	for _, l := range d.Lines {
		k := [2]string{l.Category, l.Percent}
		taxable[k] = append(taxable[k], l.Net)
	}
	taxes := make([]money.Minor, len(d.Subtotals))
	for i, s := range d.Subtotals {
		taxes[i] = s.Tax
		k := [2]string{s.Category, s.Percent}
		want, ok := taxable[k]
		got, err := money.Sum(want...)
		if !ok || err != nil || got != s.Taxable {
			return []string{fmt.Sprintf("the subtotal of category %s at %s is not its lines' net amounts", s.Category, s.Percent)}
		}
		delete(taxable, k)
	}
	if len(taxable) > 0 {
		return []string{"a category and rate of the lines has no subtotal"}
	}
	if total, err := money.Sum(taxes...); err != nil || total != d.Tax {
		return []string{"the subtotals' VAT does not add up to the document's"}
	}
	return nil
}
