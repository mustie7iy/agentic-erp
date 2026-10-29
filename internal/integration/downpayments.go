package integration

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// Open SAP down payments (T72c, ADR-0037). A vendor line with special G/L
// indicator A that SAP has not cleared is a down payment the supplier
// still owes an invoice for. It loads as the platform's own down payments
// are posted (ledger.Postings.PayDownPayment): in an entry of its own, its
// SAP document, the down payment on the down payment account as the
// supplier's special G/L item A, net of tax, on its tax code, and its
// input tax on that code; and payables records it as a paid down payment
// request naming the entry (TakeOverDownPayment). A payment run then never
// nets it, and the final invoice clears it and reverses its input tax as
// the platform's own (T46d).

// specialDownPayment is SAP's special G/L indicator of down payments.
const specialDownPayment = "A"

// downPaymentDocs are the extract's documents that hold an open down
// payment (an uncleared vendor line with special G/L indicator A), with
// their lines, by document key.
func downPaymentDocs(e *sap.Extract) map[string][]sap.ACDOCA {
	keys := map[string]bool{}
	for _, l := range e.ACDOCA {
		if l.UMSKZ == specialDownPayment && l.AUGBL == "" {
			keys[documentKey(l)] = true
		}
	}
	docs := map[string][]sap.ACDOCA{}
	for _, l := range e.ACDOCA {
		if keys[documentKey(l)] {
			docs[documentKey(l)] = append(docs[documentKey(l)], l)
		}
	}
	return docs
}

// openDownPayments are a document's open down payment lines.
func openDownPayments(lines []sap.ACDOCA) []sap.ACDOCA {
	var out []sap.ACDOCA
	for _, l := range lines {
		if l.UMSKZ == specialDownPayment && l.AUGBL == "" {
			out = append(out, l)
		}
	}
	return out
}

// downPaymentTaxLines are the lines of a down payment's document that
// carry its tax code, the down payment's own left out: its tax lines.
func downPaymentTaxLines(dp sap.ACDOCA, lines []sap.ACDOCA) []sap.ACDOCA {
	var out []sap.ACDOCA
	for _, l := range lines {
		if dp.MWSKZ != "" && l.MWSKZ == dp.MWSKZ && l.DOCLN != dp.DOCLN {
			out = append(out, l)
		}
	}
	return out
}

// specialGL checks each journal line's special G/L indicator against the
// account it is on: only A (down payments) is migrated, a line with A is
// on the legal entity's down_payment account and a line on that account
// carries A, which is never inferred from the account. A vendor line
// without an indicator on a vendor reconciliation account that T074 names
// as a special G/L account, or that no vendor of the company has as its
// reconciliation account (LFB1-AKONT), is refused too: in SAP such an
// account is an alternative reconciliation account, which special G/L
// items post to (SAP Help, "Special G/L Accounts"), so the line may be a
// down payment whose indicator the extract does not carry (the OData
// mapping does not yet), and loaded as an ordinary payable a payment run
// would net it. SAP's clearing of a down payment against its invoice is not taken over,
// so a cleared one loads in balances mode only, where cleared items are
// left out.
func (q *qualityCheck) specialGL(l sap.ACDOCA, key string) {
	onAccount := q.t.charts[l.RBUKRS][sap.Alpha(l.RACCT)].DownPayment
	switch {
	case l.UMSKZ != "" && l.UMSKZ != specialDownPayment:
		q.add(SevError, "ACDOCA", key, "special G/L indicator %q is not migrated; only down payments (A) are", l.UMSKZ)
	case l.UMSKZ == "" && !onAccount && q.specialAccount(l):
		q.add(SevError, "ACDOCA", key, "vendor line without a special G/L indicator (UMSKZ) on account %s, which %s: it may be a special G/L item "+
			"such as a down payment, which is not loaded as an ordinary payable; extract UMSKZ, or have an SAP specialist confirm the account",
			sap.Alpha(l.RACCT), q.specialWhy(l))
	case l.UMSKZ == specialDownPayment && !onAccount:
		q.add(SevError, "ACDOCA", key, "down payment (special G/L A) on account %s, which is not the legal entity's down_payment account", sap.Alpha(l.RACCT))
	case l.UMSKZ != specialDownPayment && onAccount:
		q.add(SevError, "ACDOCA", key, "line on the down payment account %s without special G/L indicator A (UMSKZ); the indicator is not inferred from the account",
			sap.Alpha(l.RACCT))
	case l.UMSKZ == specialDownPayment && l.AUGBL != "" && !q.t.balances:
		q.add(SevError, "ACDOCA", key, "down payment cleared in SAP by %s: its clearing is not migrated in documents mode; load the year in balances mode", l.AUGBL)
	}
}

// specialAccount reports whether a vendor line is on a vendor
// reconciliation account that is a special G/L account of T074 or no
// vendor's reconciliation account in the company.
func (q *qualityCheck) specialAccount(l sap.ACDOCA) bool {
	key := l.RBUKRS + "/" + sap.Alpha(l.RACCT)
	return l.LIFNR != "" && q.account[l.RBUKRS+"/"+l.RACCT].MITKZ == "K" && (q.specialAccounts[key] || !q.vendorAccounts[key])
}

func (q *qualityCheck) specialWhy(l sap.ACDOCA) string {
	if q.specialAccounts[l.RBUKRS+"/"+sap.Alpha(l.RACCT)] {
		return "T074 names as a special G/L account"
	}
	return "no vendor of the company has as its reconciliation account (LFB1-AKONT)"
}

// downPayments checks each document holding an open down payment: one
// open down payment, a debit, its tax as the platform posts a down
// payment's, and in balances mode, where the document loads as an entry of
// its own beside the opening entry, no cleared vendor line and a number
// range for its document type.
func (q *qualityCheck) downPayments() {
	docs := downPaymentDocs(q.e)
	for _, key := range sortedKeys(docs) {
		lines := docs[key]
		open := openDownPayments(lines)
		if len(open) > 1 {
			q.add(SevError, "ACDOCA", key, "document holds %d open down payments; the load takes one per document, as the platform posts them", len(open))
			continue
		}
		dp := open[0]
		cur := q.currency[dp.RBUKRS]
		amt, err := q.t.amount(dp.HSL, cur)
		switch {
		case err != nil:
			continue // reported by journalLine
		case amt <= 0:
			q.add(SevError, "ACDOCA", key+"/"+dp.DOCLN, "the open down payment is not a debit (%s %s); only a down payment made to the supplier is migrated", dp.HSL, cur)
			continue
		}
		q.downPaymentTax(key, dp, amt, lines)
		if !q.t.hasRange(dp.RBUKRS, payables.RequestNumbers) {
			q.add(SevError, "ACDOCA", key, "the configuration has no %s number range in %d for the down payment's request", payables.RequestNumbers, q.t.year)
		}
		if q.t.balances {
			q.downPaymentDocument(key, lines)
		}
	}
}

// downPaymentDocument checks, in balances mode, that a down payment's
// document can load whole beside the opening entry: no cleared vendor
// line, whose clearing partner the opening entry leaves out, and a number
// range for its document type.
func (q *qualityCheck) downPaymentDocument(key string, lines []sap.ACDOCA) {
	for _, l := range lines {
		if l.AUGBL != "" && q.account[l.RBUKRS+"/"+l.RACCT].MITKZ == "K" {
			q.add(SevError, "ACDOCA", key+"/"+l.DOCLN, "the down payment's document has a vendor line cleared by %s; load the year in documents mode", l.AUGBL)
		}
	}
	first := lines[0]
	if documentType := "journal_" + strings.ToLower(first.BLART); !q.t.hasRange(first.RBUKRS, documentType) {
		q.add(SevError, "ACDOCA", key, "the configuration has no %s number range for the down payment's document in %d", documentType, q.t.year)
	}
}

// downPaymentTax checks a down payment's tax: none, or an input tax code
// of the company with one deductible component on an account, as the
// platform's down payments post (T46c), whose tax lines are all on that
// account and add up to the code's rate of the down payment. The platform
// keeps a down payment net of its tax, which is SAP's net display; SAP's
// gross display, where the down payment includes the tax and a clearing
// account offsets it, is refused rather than loaded wrong.
func (q *qualityCheck) downPaymentTax(key string, dp sap.ACDOCA, amt money.Minor, lines []sap.ACDOCA) {
	if dp.MWSKZ == "" {
		return
	}
	date, err := sapDate(dp.BUDAT)
	if err != nil {
		return // reported by journalLine
	}
	part, rate, why := downPaymentCode(q.t.taxes[dp.RBUKRS][dp.MWSKZ], dp.MWSKZ, dp.RBUKRS, date)
	if why != "" {
		q.add(SevError, "ACDOCA", key+"/"+dp.DOCLN, "%s", why)
		return
	}
	var taxes []money.Minor
	for _, l := range downPaymentTaxLines(dp, lines) {
		if sap.Alpha(l.RACCT) != part.Account {
			q.add(SevError, "ACDOCA", key+"/"+l.DOCLN, "line on account %s carries the down payment's tax code %s, whose tax account is %s; "+
				"only SAP's net display is migrated", sap.Alpha(l.RACCT), dp.MWSKZ, part.Account)
			return
		}
		tax, err := q.t.amount(l.HSL, q.currency[l.RBUKRS])
		if err != nil {
			return // reported by journalLine
		}
		taxes = append(taxes, tax)
	}
	tax, errSum := money.Sum(taxes...)
	want, errScale := money.Scale(amt, rate, 100)
	if errSum != nil || errScale != nil {
		q.add(SevError, "ACDOCA", key, "the down payment's tax on %s cannot be checked at %s%%: its tax lines or the rate are out of range", dp.MWSKZ, rate)
		return
	}
	if tax != want {
		q.add(SevError, "ACDOCA", key, "the down payment's tax on %s is %s, not %s%% of the down payment, %s: only SAP's net display, the down payment "+
			"without its tax, is migrated", dp.MWSKZ, tax.Plain(q.t.exponents[q.currency[dp.RBUKRS]]), rate, want.Plain(q.t.exponents[q.currency[dp.RBUKRS]]))
	}
}

// downPaymentCode is the one component of a down payment's tax code and
// its rate on the date, or why the code cannot carry a migrated down
// payment.
func downPaymentCode(code config.TaxCode, mwskz, company string, date time.Time) (config.TaxComponent, string, string) {
	if code.Code == "" {
		return config.TaxComponent{}, "", fmt.Sprintf("the down payment's tax code %s is not a tax code of company %s in the configuration", mwskz, company)
	}
	parts := code.Parts()
	if code.DirectionOrInput() != config.TaxInput || len(parts) != 1 || parts[0].AccountKey != config.KeyInput || parts[0].Account == "" ||
		parts[0].Recoverable() != "100" {
		return config.TaxComponent{}, "", fmt.Sprintf("the down payment's tax code %s is not an input tax code with one deductible component, "+
			"which is all a down payment posts", mwskz)
	}
	rate, ok := parts[0].RateOn(date)
	if !ok {
		return config.TaxComponent{}, "", fmt.Sprintf("the down payment's tax code %s has no rate on %s", mwskz, date.Format(time.DateOnly))
	}
	return parts[0], rate, ""
}

// downPaymentPosting marks a document's lines as the platform posts a down
// payment: special G/L A on the down payment, and in a document holding an
// open down payment its tax code on the down payment and its tax lines,
// which the clearing against the final invoice reverses (T46d). Other
// migrated lines keep no tax code: SAP accounted for their tax.
func downPaymentPosting(pl *ledger.PostingLine, line sap.ACDOCA, dp *sap.ACDOCA) {
	if line.UMSKZ == specialDownPayment {
		special := ledger.SpecialDownPayment
		pl.SpecialGL = &special
	}
	if dp != nil && dp.MWSKZ != "" && line.MWSKZ == dp.MWSKZ {
		code := dp.MWSKZ
		pl.TaxCode = &code
	}
}

// takeOverDownPayment records the open down payment of a document the load
// just posted as the entry as a paid down payment request (payables).
func (l *loader) takeOverDownPayment(ctx context.Context, key string, lines []sap.ACDOCA, entry ledger.Entry) error {
	dp := openDownPayments(lines)[0]
	cur := l.t.entities[dp.RBUKRS].LocalCurrency
	net, _ := l.t.amount(dp.HSL, cur) // checked by the quality gate (journal)
	taxes := []money.Minor{}
	for _, t := range downPaymentTaxLines(dp, lines) {
		tax, _ := l.t.amount(t.HSL, cur) // checked by the quality gate (journal)
		taxes = append(taxes, tax)
	}
	tax, _ := money.Sum(taxes...)   // bounded by the company's lines (journal)
	gross, _ := money.Sum(net, tax) // likewise
	date, _ := sapDate(dp.BUDAT)    // checked by the quality gate
	// The reference names the source system and document, or the document
	// alone when both do not fit the request's reference.
	in := payables.MigratedDownPayment{LegalEntityID: l.t.entities[dp.RBUKRS].ID, SupplierID: l.vendors[dp.LIFNR], Currency: cur, Amount: gross,
		Tax: tax, PostingDate: date, Reference: l.system + " " + key, EntryYear: entry.FiscalYear, EntryID: entry.ID}
	if dp.MWSKZ != "" {
		in.TaxCode = &dp.MWSKZ
	}
	if len(in.Reference) > payables.ReferenceLength {
		in.Reference = key // the system is kept with the external reference
	}
	r, err := l.e.Invoices.TakeOverDownPayment(ctx, in)
	if err != nil {
		return err
	}
	return l.addRef(ctx, "payables.down_payment_request", r.ID, "BSIK", key+"/"+dp.DOCLN)
}
