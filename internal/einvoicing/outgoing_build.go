package einvoicing

import (
	"fmt"
	"strconv"
	"strings"
)

// The values E2 renders that no ERP field supplies (ADR-0061 As built
// E2, decided by default and listed for the tax specialist):
const (
	// noTransactionFlags is BTAE-02 with all eight flags unset: none of
	// the special cases (free zone, deemed supply, margin scheme, summary
	// or continuous supply, agent billing, e-commerce, exports) is
	// rendered by E2.
	noTransactionFlags = "00000000"
	// creditTransfer is the payment means (IBT-081, UNCL 4461 code 30,
	// credit transfer, from memory): customers pay the company's house
	// bank accounts.
	creditTransfer = "30"
	// oneUnit is each line's quantity and unit (IBT-129, IBT-130; UN/ECE
	// Recommendation 20 C62, "one", from memory): a customer invoice's
	// items carry a net amount but no quantity, and the ERP's units are
	// not mapped to Recommendation 20 (ADR-0061 Gaps).
	oneUnit, oneUnitCode = "1", "C62"
)

// emirates maps the ISO 3166-2 codes of the emirates, which addresses and
// the emirate guard name (AE-DU; ADR-0049 §7 As built, confirmed by the
// tax specialist), to PINT AE's codes (Emirates). An address whose region
// already holds a PINT AE code keeps it.
var emirates = map[string]string{"AE-AZ": "AUH", "AE-DU": "DXB", "AE-SH": "SHJ", "AE-UQ": "UAQ", "AE-FU": "FUJ", "AE-AJ": "AJM", "AE-RK": "RAK"}

func emirate(region string) string {
	if code, ok := emirates[strings.ToUpper(strings.TrimSpace(region))]; ok {
		return code
	}
	return strings.ToUpper(strings.TrimSpace(region))
}

// buildDocument is the PINT AE document of a posted customer document
// (ADR-0061 §9: from its items and tax codes, no second mapping), and
// what the seller or buyer lacks that the Document cannot show.
func buildDocument(s Sale, seller Seller, buyer Buyer, reason *Reason) (Document, []Finding) {
	d := Document{Kind: KindTaxInvoice, TypeCode: TypeTaxInvoice, Number: s.Number, IssueDate: s.IssueDate, DueDate: s.DueDate,
		Currency: s.Currency, Customization: PINTAECustomization, Profile: PINTAEProfile, TransactionType: noTransactionFlags,
		Reason: reason, PaymentMeans: creditTransfer, Tax: s.Tax, LineTotal: s.Net, TaxExclusive: s.Net, TaxInclusive: s.Gross,
		Payable: s.Gross, Exponent: s.Exponent}
	if s.Kind == saleCreditMemo {
		d.Kind, d.TypeCode, d.DueDate = KindTaxCreditNote, TypeTaxCreditNote, nil
	}
	if s.ReferenceNumber != nil {
		d.Reference = *s.ReferenceNumber
	}
	if s.Currency != accountingCurrency {
		lc := s.LocalTax.Abs()
		d.TaxCurrency, d.AccountingTax, d.AccountingCurrency = accountingCurrency, &lc, accountingCurrency
	}
	d.Seller = Party{Name: seller.Name, TRN: seller.TRN, Address: address(seller.Address)}
	d.Buyer = Party{Name: buyer.Name, TRN: buyer.TRN, Address: address(buyer.Address)}
	var found []Finding
	if seller.Member {
		found = append(found, Finding{Rule: "einvoicing.pint_ae.seller", Term: "IBT-034", Message: "the company's VAT TRN is its tax group's, " +
			"so its own TIN, the electronic address, is not recorded"})
	} else {
		d.Seller.EndpointScheme, d.Seller.Endpoint = EndpointScheme, tin(seller.TRN)
	}
	d.Buyer.EndpointScheme, d.Buyer.Endpoint = EndpointScheme, tin(buyer.TRN)
	if buyer.Blocked {
		found = append(found, Finding{Rule: "einvoicing.pint_ae.buyer", Term: "IBG-07", Message: "the customer's personal data is blocked"})
	}
	d.Lines, d.Subtotals = linesOf(s)
	return d, append(found, unrendered(d)...)
}

// unrendered names the mandatory terms a document needs that the format
// does not render yet (ADR-0061 As built E2, questions for the tax
// specialist), so such a document stays invalid rather than be kept as a
// valid, final tax invoice the provider would reject: a document not in
// AED needs each line's VAT in AED (BTAE-08) and its amount in AED
// (BTAE-10; the Ministry's field list #48 and #49, C5), one finding each
// as each is a term of its own, and an exempt, out-of-scope or
// reverse-charge line its exemption reason, as text or as a code (IBT-120
// or IBT-121, one finding: either term carries it).
func unrendered(d Document) []Finding {
	var out []Finding
	if d.Currency != accountingCurrency {
		out = append(out, Finding{Rule: "einvoicing.pint_ae.not_rendered", Term: "BTAE-08",
			Message: "a document not in AED needs each line's VAT in AED (BTAE-08), which the ERP does not render yet"},
			Finding{Rule: "einvoicing.pint_ae.not_rendered", Term: "BTAE-10",
				Message: "a document not in AED needs each line's amount in AED (BTAE-10), which the ERP does not render yet"})
	}
	for i, l := range d.Lines {
		if l.Category == "E" || l.Category == "O" || l.Category == "AE" {
			out = append(out, Finding{Rule: "einvoicing.pint_ae.not_rendered", Term: "IBT-120",
				Message: fmt.Sprintf("line %d: a line of category %s needs its exemption reason, as text or as a code (IBT-120 or IBT-121), which the ERP does not render yet", i+1, l.Category)})
		}
	}
	return out
}

// tin is the first ten digits of a TRN, the TIN of the electronic address
// (ADR-0061 §5 C6); "" for no TRN.
func tin(trn string) string {
	if len(trn) < 10 {
		return ""
	}
	return trn[:10]
}

func address(a *Address) Address {
	if a == nil {
		return Address{}
	}
	out := *a
	if out.Country == "AE" {
		out.Subentity = emirate(out.Subentity)
	}
	return out
}

// linesOf is the document's lines, one per item at its net amount, and
// its VAT per category and rate in the order the lines first name them,
// each the items' net amounts and the tax receivables gave them. The sums
// are bounded: a posted document's items add up to its net and its tax
// (receivables' totals, customer_invoice's bigint columns), so a part of
// them cannot overflow.
func linesOf(s Sale) ([]Line, []Subtotal) {
	lines := make([]Line, len(s.Lines))
	var subtotals []Subtotal
	at := map[[2]string]int{}
	for i, it := range s.Lines {
		percent := plainPercent(it.Percent)
		lines[i] = Line{ID: strconv.Itoa(int(it.No)), Quantity: oneUnit, Unit: oneUnitCode, Net: it.Net, Price: it.Net,
			Name: it.Description, Description: it.Description, Category: it.Category, Percent: percent}
		k := [2]string{it.Category, percent}
		j, ok := at[k]
		if !ok {
			j = len(subtotals)
			at[k] = j
			subtotals = append(subtotals, Subtotal{Category: it.Category, Percent: percent})
		}
		subtotals[j].Taxable += it.Net // bounded by the document's net (above)
		subtotals[j].Tax += it.Tax     // bounded by the document's tax
	}
	return lines, subtotals
}

// plainPercent writes a rate without trailing zeros: "5.000000" is "5",
// "2.500000" is "2.5"; "" stays "".
func plainPercent(p string) string {
	if !strings.Contains(p, ".") {
		return p
	}
	return strings.TrimSuffix(strings.TrimRight(p, "0"), ".")
}
