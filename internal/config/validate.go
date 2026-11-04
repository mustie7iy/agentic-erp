package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/bankfile"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/cabank"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

type Severity string

const (
	SevError   Severity = "error"   // blocks activation
	SevWarning Severity = "warning" // allowed, shown to approvers
)

// Finding is one result of deterministic validation.
type Finding struct {
	Severity Severity `json:"severity"`
	Path     string   `json:"path"`
	Message  string   `json:"message"`
}

func (f Finding) String() string { return fmt.Sprintf("[%s] %s: %s", f.Severity, f.Path, f.Message) }

// Note explains a generated setting reviewers should look at: what a
// draft document generated from SAP (delivery/sapconfig) or a country pack
// (countrypack) chose and why.
type Note struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (n Note) String() string { return n.Path + ": " + n.Message }

func HasErrors(fs []Finding) bool {
	return slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError })
}

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	wthCodeRe  = regexp.MustCompile(`^[A-Z0-9]{1,4}$`)
	methodRe   = regexp.MustCompile(`^[A-Z0-9]$`)
	countryRe  = regexp.MustCompile(`^[A-Z]{2}$`)
	bicRe      = regexp.MustCompile(`^[A-Z]{4}[A-Z]{2}[A-Z0-9]{2}([A-Z0-9]{3})?$`)
	accountRe  = regexp.MustCompile(`^[0-9A-Za-z]{1,10}$`)
	termRe     = regexp.MustCompile(`^[A-Z0-9_/]{1,10}$`)
)

// Validate runs every deterministic check. The configuration may only be
// activated when no finding has error severity. exponents are the
// currencies of ref.currency with their minor-unit digits (ADR-0005):
// currency.Load in a transaction, currency.Seeded offline.
func Validate(c *Config, exponents currency.Exponents) []Finding {
	v := &validator{exponents: exponents}
	if c.Version != 1 {
		v.err("version", "unsupported version %d (expected 1)", c.Version)
	}
	if len(c.Companies) == 0 {
		v.err("companies", "at least one company is required")
	}
	seen := map[string]bool{}
	for _, co := range c.Companies {
		seen[co.Code] = co.Code != ""
	}
	registrations := map[string]map[string]taxRegistrationDates{}
	for i, co := range c.Companies {
		p := fmt.Sprintf("companies[%s]", co.Code)
		if co.Code == "" {
			p = fmt.Sprintf("companies[%d]", i)
			v.err(p+".code", "company code is required")
		}
		if slices.IndexFunc(c.Companies, func(o Company) bool { return o.Code == co.Code }) < i {
			v.err(p, "duplicate company code")
		}
		v.company(p, &co)
		registrations[co.Code] = v.taxRegistrations(p, &co, seen)
	}
	v.taxRegistrationsAcross(c, registrations)
	v.paymentTerms(c)
	v.purchasing(c)
	v.payments(c)
	v.numberRanges(c)
	v.journalRanges(c)
	v.ledgers(c)
	v.extensions(c)
	v.rules(c)
	v.packs(c)
	v.calendars(c)
	v.controlling(c)
	v.cash(c)
	v.assetClasses(c)
	v.closeTemplate(c)
	switch c.Output.PurchaseOrderChannel {
	case "email", "edi", "print":
	default:
		v.err("output.purchase_order_channel", "must be email, edi or print")
	}
	return v.out
}

var (
	documentTypeRe = regexp.MustCompile(`^[a-z_]+$`)
	prefixRe       = regexp.MustCompile(`^[A-Z0-9-]{0,10}$`)
)

// numberRanges checks each interval on its own; that a range belongs to an
// existing legal entity and only grows is checked against the tenant's data
// when a change is submitted.
func (v *validator) numberRanges(c *Config) {
	seen := map[string]bool{}
	for _, n := range c.NumberRanges {
		p := "number_ranges[" + n.Key() + "]"
		if seen[n.Key()] {
			v.err(p, "duplicate number range")
		}
		seen[n.Key()] = true
		if strings.TrimSpace(n.Company) == "" {
			v.err(p+".company", "company is required")
		}
		if !documentTypeRe.MatchString(n.DocumentType) {
			v.err(p+".document_type", "document type %q must be lower-case letters and underscores", n.DocumentType)
		}
		if n.FiscalYear < 1900 || n.FiscalYear > 2999 {
			v.err(p+".fiscal_year", "fiscal year %d is out of range", n.FiscalYear)
		}
		if !prefixRe.MatchString(n.Prefix) {
			v.err(p+".prefix", "prefix %q must be up to 10 upper-case letters, digits or dashes", n.Prefix)
		}
		if n.First < 1 {
			v.err(p+".first", "the first number must be positive")
		}
		if n.Last < n.First {
			v.err(p+".last", "the last number must not be below the first")
		}
	}
}

var packVersionRe = regexp.MustCompile(`^[0-9]{4}\.[1-9][0-9]*$`)

// packs checks the provenance of adopted country packs (ADR-0048): one per
// company, of the company's country.
func (v *validator) packs(c *Config) {
	seen := map[string]bool{}
	for _, a := range c.Packs {
		p := "packs[" + a.Company + "]"
		if seen[a.Company] {
			v.err(p, "duplicate pack adoption")
		}
		seen[a.Company] = true
		switch co := c.Company(a.Company); {
		case co == nil:
			v.err(p+".company", "company %q is not in the document", a.Company)
		case co.Country != a.Country:
			v.err(p+".country", "the pack of %s does not fit the company in %s", a.Country, co.Country)
		}
		if !packVersionRe.MatchString(a.Version) {
			v.err(p+".version", "version %q must be YYYY.N", a.Version)
		}
		if _, err := time.Parse(time.DateOnly, a.AppliedOn); err != nil {
			v.err(p+".applied_on", "%q is not a date (YYYY-MM-DD)", a.AppliedOn)
		}
	}
}

// calendars checks the companies' calendars (ADR-0048, ADR-0058 §3): one
// per company of the document, its weekend weekdays each once, each
// holiday a date (YYYY-MM-DD) once, named.
func (v *validator) calendars(c *Config) {
	seen := map[string]bool{}
	for _, cal := range c.Calendars {
		p := "calendars[" + cal.Company + "]"
		if seen[cal.Company] {
			v.err(p, "duplicate calendar")
		}
		seen[cal.Company] = true
		if c.Company(cal.Company) == nil {
			v.err(p+".company", "company %q is not in the document", cal.Company)
		}
		v.weekend(p, cal.Weekend)
		dates := map[string]bool{}
		for _, h := range cal.Holidays {
			hp := p + ".holidays[" + h.Date + "]"
			if _, err := time.Parse(time.DateOnly, h.Date); err != nil {
				v.err(hp+".date", "%q is not a date (YYYY-MM-DD)", h.Date)
			}
			if dates[h.Date] {
				v.err(hp, "duplicate holiday")
			}
			dates[h.Date] = true
			if strings.TrimSpace(h.Name) == "" {
				v.err(hp+".name", "name is required")
			}
		}
	}
}

type validator struct {
	exponents currency.Exponents
	out       []Finding
}

func (v *validator) err(path, format string, a ...any) {
	v.out = append(v.out, Finding{SevError, path, fmt.Sprintf(format, a...)})
}
func (v *validator) warn(path, format string, a ...any) {
	v.out = append(v.out, Finding{SevWarning, path, fmt.Sprintf(format, a...)})
}

func (v *validator) amount(path, s string, decimals int) (money.Minor, bool) {
	m, err := money.ParseDecimal(s, decimals)
	if err != nil {
		v.err(path, "%v", err)
		return 0, false
	}
	if m < 0 {
		v.err(path, "must not be negative")
		return 0, false
	}
	return m, true
}

func (v *validator) company(p string, co *Company) {
	if strings.TrimSpace(co.Name) == "" {
		v.err(p+".name", "name is required")
	}
	exp, known := v.exponents[co.Currency]
	switch {
	case !currencyRe.MatchString(co.Currency):
		v.err(p+".currency", "currency %q must be an ISO 4217 code", co.Currency)
	case !known:
		v.err(p+".currency", "currency %s is not in the currency reference data", co.Currency)
	}
	if !countryRe.MatchString(co.Country) {
		v.err(p+".country", "country %q must be an ISO 3166 two-letter code", co.Country)
	}
	if co.FiscalYearStartMonth < 1 || co.FiscalYearStartMonth > 12 {
		v.err(p+".fiscal_year_start_month", "must be 1-12")
	}
	// Required of a document submitted or bootstrapped since the product
	// owner's decision of 2026-10-31: a document activated before carries
	// none and is read as UTC (Company.Location), as is one a change request
	// submitted before then activates, since activation does not validate
	// again.
	if co.TimeZone == "" {
		v.err(p+".time_zone", "time zone is required: the IANA name of the zone the company's business dates are in, such as America/Toronto or Asia/Dubai, "+
			"as the company states it (a country can span several zones)")
	} else if _, err := co.Location(); err != nil {
		v.err(p+".time_zone", "%v", err)
	}
	v.chartOfAccounts(p, co)
	v.accountDetermination(p, co)
	v.taxCodes(p, co)
	v.withholdingTaxCodes(p, co)
	v.fxValuation(p, co)
	if co.InvoiceTolerance.PricePercent < 0 || co.InvoiceTolerance.PricePercent > 100 {
		v.err(p+".invoice_tolerance.price_percent", "must be 0-100")
	}
	if u := co.InvoiceTolerance.TaxUnits(); u < 0 || u > 100 {
		v.err(p+".invoice_tolerance.tax_units_per_code", "must be 0-100 minor units per tax code")
	}
	// Amounts are read with the currency's digits from ref.currency; with
	// no known currency there are none to read them with, and the
	// currency's finding already blocks activation.
	if known {
		v.amount(p+".invoice_tolerance.price_absolute", co.InvoiceTolerance.PriceAbsolute, exp)
		v.taxSettings(p+".tax_settings", co.TaxSettings, exp)
		v.amount(p+".auto_post_limit", co.AutoPostLimit, exp)
		v.amount(p+".manual_entry_approval_limit", co.ManualEntryApprovalLimit, exp)
		if co.ReleaseDualLimit != "" { // optional: no run then needs a second person
			v.amount(p+".release_dual_limit", co.ReleaseDualLimit, exp)
		}
		if co.ServicePostingLimit != "" { // optional: the manual entry approval limit then (ServiceLimit)
			v.amount(p+".service_posting_limit", co.ServicePostingLimit, exp)
		}
	}
	if co.OverDeliveryPercent < 0 || co.OverDeliveryPercent > 100 {
		v.err(p+".gr_over_delivery_percent", "must be 0-100")
	}
	v.houseBanks(p, co)
	v.paymentMethods(p, co)
	v.bankStatements(p, co, exp, known)
	v.companyControlling(p, co)
	v.companyCash(p, co)
	v.companyClose(p, co, exp, known)
}

// isTaxAccount reports whether the account is the input tax account or a
// tax code's.
func (c *Company) isTaxAccount(number string) bool {
	if number != "" && number == c.AccountDetermination.InputTax {
		return true
	}
	return number != "" && slices.ContainsFunc(c.TaxCodes, func(t TaxCode) bool {
		return slices.ContainsFunc(t.Parts(), func(p TaxComponent) bool { return p.Account == number })
	})
}

// houseBanks checks each house bank's IBAN and BIC and that its code is
// unique in the company, and that each has its own ledger account
// (ADR-0038) that takes no tax: a further bank names one, a plain asset
// account of the chart that no other house bank of the company uses; the
// main bank without one keeps the account determination's bank account,
// whose type is checked there.
func (v *validator) houseBanks(p string, co *Company) {
	codes := map[string]bool{}
	accounts := map[string]string{}
	for i, hb := range co.AllHouseBanks() {
		hp := houseBankPath(p, i, hb)
		if codes[hb.Name] {
			v.err(hp+".name", "duplicate house bank")
		}
		codes[hb.Name] = true
		v.houseBankAccount(hp, hb)
		v.houseBankCurrency(hp, co, hb)
		if !bicRe.MatchString(hb.BIC) {
			v.err(hp+".bic", "BIC %q is not a valid SWIFT code", hb.BIC)
		}
		if i > 0 && hb.GLAccount == "" {
			v.err(hp+".gl_account", "is required: each further house bank has its own ledger account")
			continue
		}
		if hb.GLAccount != "" {
			v.account(hp+".gl_account", co, hb.GLAccount, Asset, false)
		}
		if number := co.HouseBankAccount(hb); co.isTaxAccount(number) {
			v.err(hp+".gl_account", "account %s takes input tax, not a bank's payments", number)
		}
		number := co.HouseBankAccount(hb)
		if other, taken := accounts[number]; taken {
			v.err(hp+".gl_account", "account %s is already the ledger account of house bank %s", number, other)
		}
		accounts[number] = hb.Name
	}
	for i, hb := range co.AllHouseBanks() {
		v.statementAccounts(houseBankPath(p, i, hb), co, hb, accounts)
	}
}

// houseBankCurrency checks a house bank in another currency than the
// company's (ADR-0051 §3, T53a): a currency of the reference data, and
// each of the bank's accounts flagged revalue_foreign_currency, so the
// period-end valuation (ADR-0042) covers its balance. That no account is
// shared with a bank of another currency follows from no account being
// shared at all.
func (v *validator) houseBankCurrency(hp string, co *Company, hb HouseBank) {
	if hb.Currency == "" || hb.Currency == co.Currency {
		return
	}
	if _, known := v.exponents[hb.Currency]; !known {
		v.err(hp+".currency", "currency %q is not in the currency reference data", hb.Currency)
		return
	}
	for _, a := range []struct{ field, number string }{{"gl_account", co.HouseBankAccount(hb)}, {"main_gl_account", hb.MainGLAccount},
		{"incoming_gl_account", hb.IncomingGLAccount}} {
		if acc := co.Account(a.number); a.number != "" && acc != nil && !acc.RevalueForeignCurrency {
			v.err(hp+"."+a.field, "house bank %s is in %s: account %s must be revalue_foreign_currency, so period-end valuation covers it",
				hb.Name, hb.Currency, a.number)
		}
	}
}

// houseBankPath is the path of the company's i-th house bank: the main
// bank first, then the further ones by name.
func houseBankPath(p string, i int, hb HouseBank) string {
	if i == 0 {
		return p + ".house_bank"
	}
	return fmt.Sprintf("%s.house_banks[%s]", p, hb.Name)
}

// statementAccounts checks the accounts a house bank's statements post to
// (ADR-0047): the main bank account and the incoming payments clearing
// account, each a plain asset account of the chart that takes no tax and
// is no house bank's other account. A bank that reconciles its statements
// clears open items, so its clearing accounts must be open-item managed.
// accounts are the house banks' clearing accounts, by number; the
// statements' accounts are added to it.
func (v *validator) statementAccounts(hp string, co *Company, hb HouseBank, accounts map[string]string) {
	if hb.MainGLAccount == "" {
		if hb.IncomingGLAccount != "" {
			v.err(hp+".incoming_gl_account", "needs main_gl_account: a bank's statements post only to its main account")
		}
		return
	}
	for _, a := range []struct{ field, number string }{{"main_gl_account", hb.MainGLAccount}, {"incoming_gl_account", hb.IncomingGLAccount}} {
		if a.number == "" {
			continue
		}
		v.account(hp+"."+a.field, co, a.number, Asset, false)
		if co.isTaxAccount(a.number) {
			v.err(hp+"."+a.field, "account %s takes input tax, not a bank's payments", a.number)
		}
		if other, taken := accounts[a.number]; taken {
			v.err(hp+"."+a.field, "account %s is already an account of house bank %s", a.number, other)
		}
		accounts[a.number] = hb.Name
	}
	clearing := []struct{ field, number string }{{"gl_account", co.HouseBankAccount(hb)}, {"incoming_gl_account", hb.IncomingGLAccount}}
	for _, a := range clearing {
		if acc := co.Account(a.number); acc != nil && !acc.OpenItems {
			v.err(hp+"."+a.field, "account %s clears the statement's lines, so it must be open-item managed (open_items); "+
				"an account with postings is not switched: name a new one and transfer the old balance to it", a.number)
		}
	}
}

// isoTransactionCode is an ISO 20022 domain, family and sub-family;
// bankTransactionCode a SWIFT MT940 type (NCHG) or a bank's proprietary
// code. Codes are compared exactly, as banks send them.
var (
	isoTransactionCode  = regexp.MustCompile(`^[A-Z]{4}-[A-Z]{4}-[A-Z]{4}$`)
	bankTransactionCode = regexp.MustCompile(`^[A-Za-z0-9]{1,35}$`)
)

// ValidTransactionCode reports whether code is a bank transaction code as
// statements carry them (ADR-0047): ISO 20022 domain, family and
// sub-family joined by dashes (PMNT-CCRD-CHRG), a SWIFT MT940 transaction
// type (NCHG), or a bank's proprietary code of letters and digits, such as
// a BAI type code (398).
func ValidTransactionCode(code string) bool {
	return isoTransactionCode.MatchString(code) || bankTransactionCode.MatchString(code)
}

// bankStatements checks how the company's statements are matched and
// posted (ADR-0047): posting rules by valid, unique transaction codes to
// accounts of the chart that are no vendor reconciliation account, no tax
// account and no house bank's; matching rules of known kinds, each once,
// the amount rule's tolerance an amount of the company's currency and its
// window 0-31 days; and approval tiers, when there are any, in increasing
// order of amounts in the company's currency. A tier without steps needs
// no approval; no tiers at all means every match is approved.
func (v *validator) bankStatements(p string, co *Company, exp int, known bool) {
	bp := p + ".bank_statements"
	banks := map[string]bool{}
	for _, hb := range co.AllHouseBanks() {
		banks[co.HouseBankAccount(hb)], banks[hb.MainGLAccount], banks[hb.IncomingGLAccount] = true, true, true
	}
	codes := map[string]bool{}
	for _, r := range co.BankStatements.PostingRules {
		rp := bp + ".posting_rules[" + r.TransactionCode + "]"
		if !ValidTransactionCode(r.TransactionCode) || codes[r.TransactionCode] {
			v.err(rp+".transaction_code", "a bank transaction code (PMNT-CCRD-CHRG, NCHG or the bank's own), each once")
		}
		codes[r.TransactionCode] = true
		a := co.Account(r.Account)
		switch {
		case a == nil:
			v.err(rp+".account", "account %q is not in the chart of accounts", r.Account)
		case a.VendorReconciliation, co.isTaxAccount(r.Account), banks[r.Account]:
			v.err(rp+".account", "account %s is posted only by its own documents, not by a statement's charges or interest", r.Account)
		}
	}
	v.matchingRules(bp, co, exp, known)
	// The tiers are read with the company's digits; with an unknown
	// currency the currency's finding already blocks activation.
	v.orderedTiers(bp+".approval_tiers", co.BankStatements.ApprovalTiers, func(path, s string) (money.Minor, bool) {
		if !known {
			return 0, false
		}
		return v.amount(path, s, exp)
	})
}

// matchingRules checks the company's matching rules.
func (v *validator) matchingRules(bp string, co *Company, exp int, known bool) {
	kinds := map[string]bool{}
	for _, r := range co.BankStatements.MatchingRules {
		rp := bp + ".matching_rules[" + r.Kind + "]"
		switch r.Kind {
		case MatchEndToEndID, MatchBatchID, MatchTransactionCode, MatchAmount:
		default:
			v.err(rp+".kind", "must be end_to_end_id, batch_id, transaction_code or amount")
		}
		if kinds[r.Kind] {
			v.err(rp, "duplicate matching rule")
		}
		kinds[r.Kind] = true
		if r.Kind != MatchAmount {
			if r.AmountTolerance != "" || r.DateWindowDays != 0 || r.Counterparty {
				v.err(rp, "only the amount rule takes amount_tolerance, date_window_days and counterparty")
			}
			continue
		}
		if r.DateWindowDays < 0 || r.DateWindowDays > 31 {
			v.err(rp+".date_window_days", "must be 0-31")
		}
		if known && r.AmountTolerance != "" {
			v.amount(rp+".amount_tolerance", r.AmountTolerance, exp)
		}
	}
	if kinds[MatchTransactionCode] && len(co.BankStatements.PostingRules) == 0 {
		v.err(bp+".matching_rules[transaction_code]", "needs posting_rules to post the lines it matches")
	}
}

// houseBankAccount checks the house bank's IBAN, or its Canadian account
// by the digits and lengths of its numbers (T45d).
func (v *validator) houseBankAccount(hp string, hb HouseBank) {
	if !hb.Canadian() {
		if !ValidIBAN(hb.IBAN) {
			v.err(hp+".iban", "IBAN %q fails the ISO 13616 check", hb.IBAN)
		}
		return
	}
	if hb.IBAN != "" {
		v.err(hp+".iban", "a Canadian house bank has no IBAN; give either the IBAN or the institution, transit and account numbers")
	}
	_, err := cabank.Parse(hb.InstitutionNumber, hb.TransitNumber, hb.AccountNumber)
	switch {
	case errors.Is(err, cabank.ErrInstitution):
		v.err(hp+".institution_number", "institution number %q is not 3 digits", hb.InstitutionNumber)
	case errors.Is(err, cabank.ErrTransit):
		v.err(hp+".transit_number", "transit number %q is not 5 digits", hb.TransitNumber)
	case err != nil:
		v.err(hp+".account_number", "a Canadian account number is 7-12 digits")
	}
}

// chartOfAccounts checks the company's accounts: valid, unique numbers and
// types, and at least one vendor reconciliation account, which must be a
// liability.
func (v *validator) chartOfAccounts(p string, co *Company) {
	types := map[AccountType]bool{Asset: true, Liability: true, Equity: true, Revenue: true, Expense: true}
	nums := map[string]bool{}
	recon := 0
	for _, a := range co.ChartOfAccounts {
		ap := fmt.Sprintf("%s.chart_of_accounts[%s]", p, a.Number)
		if !accountRe.MatchString(a.Number) {
			v.err(ap, "account number %q is invalid", a.Number)
		}
		if nums[a.Number] {
			v.err(ap, "duplicate account")
		}
		nums[a.Number] = true
		if !types[a.Type] {
			v.err(ap+".type", "type %q must be asset, liability, equity, revenue or expense", a.Type)
		}
		if a.VendorReconciliation {
			recon++
			downPayment := a.Number == co.AccountDetermination.DownPayment && a.Type == Asset // advances to suppliers (ADR-0037)
			if a.Type != Liability && !downPayment {
				v.err(ap, "a vendor reconciliation account must be a liability, or the company's down payment account, an asset")
			}
		}
		v.assetReconciliation(ap, &a)
	}
	if len(co.ChartOfAccounts) == 0 {
		v.err(p+".chart_of_accounts", "chart of accounts is empty")
	}
	if recon == 0 {
		v.err(p+".chart_of_accounts", "no vendor reconciliation account")
	}
}

// paymentMethods checks the company's payment methods (ADR-0038): unique
// one-character codes of an upper-case letter or digit, each described and
// ranking house banks for at least one currency, each currency once, and
// each ranking naming house banks of the company, each once.
func (v *validator) paymentMethods(p string, co *Company) {
	banks := map[string]string{} // name → currency
	for _, hb := range co.AllHouseBanks() {
		banks[hb.Name] = co.HouseBankCurrency(hb)
	}
	codes := map[string]bool{}
	for _, m := range co.PaymentMethods {
		mp := fmt.Sprintf("%s.payment_methods[%s]", p, m.Code)
		if !methodRe.MatchString(m.Code) || codes[m.Code] {
			v.err(mp, "a payment method's code is one upper-case letter or digit, unique in the company")
		}
		codes[m.Code] = true
		if strings.TrimSpace(m.Description) == "" {
			v.err(mp+".description", "is required")
		}
		if len(m.Ranking) == 0 {
			v.err(mp+".ranking", "rank the house banks that pay with the method for at least one currency")
		}
		currencies := map[string]bool{}
		for _, r := range m.Ranking {
			rp := fmt.Sprintf("%s.ranking[%s]", mp, r.Currency)
			if _, known := v.exponents[r.Currency]; !known || currencies[r.Currency] {
				v.err(rp, "a currency of the currency reference data, each once in the method's ranking")
			}
			currencies[r.Currency] = true
			v.rankedBanks(rp, banks, r)
		}
	}
}

// rankedBanks checks that a ranking names at least one house bank, each a
// house bank of the company in the ranking's currency, named once: a bank
// pays only in its own currency (ADR-0051 §3), so ranking it for another
// would never pay.
func (v *validator) rankedBanks(rp string, banks map[string]string, r BankRanking) {
	if len(r.HouseBanks) == 0 {
		v.err(rp+".house_banks", "rank at least one house bank")
	}
	seen := map[string]bool{}
	for _, name := range r.HouseBanks {
		currency, ok := banks[name]
		switch {
		case !ok || seen[name]:
			v.err(rp+".house_banks", "house bank %s: a house bank of the company, each once", name)
		case currency != r.Currency:
			v.err(rp+".house_banks", "house bank %s is in %s and pays only in it, not in %s", name, currency, r.Currency)
		}
		seen[name] = true
	}
}

// account checks that the account is in the chart with the type its use
// needs, a vendor reconciliation account where the use is one and no other
// account.
func (v *validator) account(path string, co *Company, number string, want AccountType, recon bool) {
	a := co.Account(number)
	switch {
	case number == "":
		v.err(path, "is required")
	case a == nil:
		v.err(path, "account %s is not in the chart of accounts", number)
	case a.Type != want:
		v.err(path, "account %s is %s, expected %s", number, a.Type, want)
	case recon && !a.VendorReconciliation:
		v.err(path, "account %s is not a vendor reconciliation account", number)
	case !recon && a.VendorReconciliation:
		v.err(path, "account %s is a vendor reconciliation account, posted only through the supplier's items", number)
	}
}

// accountDetermination checks that each determined account is in the chart
// with the type its use needs, a vendor reconciliation account where the
// use is one and no other account; the FX accounts are optional, the
// GR/IR account comes with a price difference account, the cash discount
// account is a revenue (SAP SKE, cash discount received; its presence is
// required by the payment terms, paymentTerms), and the down payment
// account is an asset vendor reconciliation account (ADR-0037).
func (v *validator) accountDetermination(p string, co *Company) {
	ad := co.AccountDetermination
	check := func(field, number string, want AccountType, recon bool) {
		v.account(p+".account_determination."+field, co, number, want, recon)
	}
	check("ap_reconciliation", ad.APReconciliation, Liability, true)
	check("input_tax", ad.InputTax, Asset, false)
	check("bank", ad.Bank, Asset, false)
	if ad.FXGain != "" {
		check("fx_gain", ad.FXGain, Revenue, false)
	}
	if ad.FXLoss != "" {
		check("fx_loss", ad.FXLoss, Expense, false)
	}
	if ad.GRIR != "" {
		check("gr_ir", ad.GRIR, Liability, false)
	}
	if ad.GRIR != "" || ad.PriceDifference != "" {
		check("price_difference", ad.PriceDifference, Expense, false)
	}
	if ad.CashDiscount != "" {
		check("cash_discount", ad.CashDiscount, Revenue, false)
	}
	if ad.DownPayment != "" {
		check("down_payment", ad.DownPayment, Asset, true)
	}
}

// journalRule is a journal number range a document's range needs: a
// company that numbers documents of the type in a fiscal year, when the
// rule applies to it, posts journal entries of journal type in that year.
type journalRule struct {
	documentType, journal string
	applies               func(co *Company) bool
	why                   string // what the documents post, for the message
}

// journalRules are the journal ranges document ranges need.
var journalRules = []journalRule{
	// Receipts on orders valued through GR/IR post WE entries (T33).
	{"goods_receipt", "journal_we", func(co *Company) bool { return co.AccountDetermination.GRIR != "" }, "WE entries to GR/IR"},
	// Credit memos and subsequent credits post KG entries (T34c).
	{"supplier_invoice", "journal_kg", func(*Company) bool { return true }, "credit memos as KG entries"},
}

// journalRanges requires the journal ranges each document range needs
// (journalRules), in the same company and fiscal year.
func (v *validator) journalRanges(c *Config) {
	have := map[string]bool{}
	for _, n := range c.NumberRanges {
		have[n.Key()] = true
	}
	for _, n := range c.NumberRanges {
		co := c.Company(n.Company)
		for _, r := range journalRules {
			if n.DocumentType != r.documentType || co == nil || !r.applies(co) {
				continue
			}
			need := NumberRange{Company: n.Company, DocumentType: r.journal, FiscalYear: n.FiscalYear}
			if !have[need.Key()] {
				v.err("number_ranges["+need.Key()+"]", "%s of company %s post %s in fiscal year %d; add a %s range",
					strings.ReplaceAll(r.documentType, "_", " ")+"s", n.Company, r.why, n.FiscalYear, r.journal)
			}
		}
	}
}

// withholdingTaxCodes checks the company's withholding tax codes (T46e):
// unique codes of 1-4 upper-case letters or digits, a rate above 0 and
// below 100% with up to three decimals, a base of net (the default) or
// gross, deducted at invoice or at payment, to a liability account of the
// chart that reconciles no supplier: the withholding tax payable.
func (v *validator) withholdingTaxCodes(p string, co *Company) {
	codes := map[string]bool{}
	for _, w := range co.WithholdingTaxCodes {
		wp := fmt.Sprintf("%s.withholding_tax_codes[%s]", p, w.Code)
		if !wthCodeRe.MatchString(w.Code) || codes[w.Code] {
			v.err(wp, "a withholding tax code is 1-4 upper-case letters or digits, unique in the company")
		}
		codes[w.Code] = true
		if r, ok := v.amount(wp+".rate_percent", w.RatePercent, 3); ok && (r == 0 || r >= 100_000) {
			v.err(wp+".rate_percent", "a rate above 0 and below 100%%")
		}
		if b := w.BaseOrNet(); b != WithholdingNet && b != WithholdingGross {
			v.err(wp+".base", "net or gross")
		}
		if w.Posting != WithholdingAtInvoice && w.Posting != WithholdingAtPayment {
			v.err(wp+".posting", "invoice or payment")
		}
		if a := co.Account(w.Account); a == nil || a.Type != Liability || a.VendorReconciliation {
			v.err(wp+".account", "account %s: the withholding tax payable is a liability of the chart that reconciles no supplier", w.Account)
		}
	}
}

func (v *validator) paymentTerms(c *Config) {
	seen := map[string]bool{}
	for _, t := range c.PaymentTerms {
		p := fmt.Sprintf("payment_terms[%s]", t.Key)
		if !termRe.MatchString(t.Key) || seen[t.Key] {
			v.err(p, "payment terms key must be present, unique and up to 10 characters")
		}
		seen[t.Key] = true
		if t.NetDays < 0 || t.NetDays > 365 {
			v.err(p+".net_days", "must be 0-365")
		}
		v.discounts(p, t)
	}
	if len(c.PaymentTerms) == 0 {
		v.err("payment_terms", "at least one payment term is required")
	}
	if slices.ContainsFunc(c.PaymentTerms, func(t PaymentTerm) bool { return len(t.Discounts) > 0 }) {
		for _, co := range c.Companies {
			if co.AccountDetermination.CashDiscount == "" {
				v.err(fmt.Sprintf("companies[%s].account_determination.cash_discount", co.Code),
					"is required: payment terms grant cash discounts, which payments in any company may take")
			}
		}
	}
}

// discounts checks a term's cash discount tiers: at most two (SAP T052),
// each within the net days and later than the one before, and each giving
// a percent above 0 and below 100, of at most three decimals, smaller
// than the one before, so paying earlier never earns less.
func (v *validator) discounts(p string, t PaymentTerm) {
	if len(t.Discounts) > 2 {
		v.err(p+".discounts", "at most two cash discount tiers")
		return
	}
	var prevDays int
	var prevPct money.Minor
	for i, d := range t.Discounts {
		dp := fmt.Sprintf("%s.discounts[%d]", p, i)
		switch {
		case d.Days < 0 || d.Days >= t.NetDays:
			v.err(dp+".days", "must be 0 or more and less than the net days %d", t.NetDays)
		case i > 0 && d.Days <= prevDays:
			v.err(dp+".days", "must be later than the tier before (%d days)", prevDays)
		}
		pct, err := money.ParseDecimal(d.Percent, 3)
		switch {
		case err != nil:
			v.err(dp+".percent", "%v", err)
		case pct <= 0 || pct >= 100_000:
			v.err(dp+".percent", "must be above 0 and below 100")
		case i > 0 && pct >= prevPct:
			v.err(dp+".percent", "must be less than the tier before")
		}
		prevDays, prevPct = d.Days, pct
	}
}

func (v *validator) purchasing(c *Config) {
	v.tiers("purchasing.requisition_approval", c.Purchasing.RequisitionApproval, c)
	v.tiers("purchasing.order_release_approval", c.Purchasing.OrderReleaseApproval, c)
	if d := c.Purchasing.ContractExpiryNoticeDays; d < 1 || d > 365 {
		v.err("purchasing.contract_expiry_notice_days", "must be 1-365")
	}
}

// tiers checks approval tiers by amount: at least one, in increasing
// order of their up_to, the last without one so every amount is covered.
func (v *validator) tiers(path string, tiers []ApprovalTier, c *Config) {
	if len(tiers) == 0 {
		v.err(path, "at least one tier is required")
		return
	}
	v.orderedTiers(path, tiers, func(p, s string) (money.Minor, bool) { return v.threshold(p, s, c) })
}

// orderedTiers checks that the tiers' up_to, read by limit, increase and
// that only the last tier omits it, and each tier's steps.
func (v *validator) orderedTiers(path string, tiers []ApprovalTier, limit func(path, s string) (money.Minor, bool)) {
	var prev money.Minor = -1
	for i, t := range tiers {
		p := fmt.Sprintf("%s[%d]", path, i)
		last := i == len(tiers)-1
		switch {
		case last && t.UpTo != "":
			v.err(p+".up_to", "the last tier must have no up_to so every amount is covered")
		case !last && t.UpTo == "":
			v.err(p+".up_to", "only the last tier may omit up_to")
		case !last:
			if m, ok := limit(p+".up_to", t.UpTo); ok {
				if m <= prev {
					v.err(p+".up_to", "tiers must be in increasing order")
				}
				prev = m
			}
		}
		v.approvalSteps(p, t.Steps)
	}
}

// threshold reads an approval tier's limit (requisitions, order
// releases). A limit applies to documents in every company's currency, so
// it must read with the
// digits of each (ref.currency): the approval routing then never meets a
// limit it cannot parse. Its value, for the order of the tiers, is read
// with the most digits.
func (v *validator) threshold(path, s string, c *Config) (money.Minor, bool) {
	most, known := 0, false
	for _, co := range c.Companies {
		exp, ok := v.exponents[co.Currency]
		if !ok {
			continue // the company's currency finding blocks activation
		}
		if _, err := money.ParseDecimal(s, exp); err != nil {
			v.err(path, "%v for %s, the currency of company %s", err, co.Currency, co.Code)
			return 0, false
		}
		most, known = max(most, exp), true
	}
	if !known {
		return 0, false // no currency to read it with; the companies' findings block activation
	}
	return v.amount(path, s, most)
}

// approvalSteps checks the steps of one requisition approval tier.
func (v *validator) approvalSteps(p string, steps []ApprovalStep) {
	for j, s := range steps {
		sp := fmt.Sprintf("%s.steps[%d]", p, j)
		if strings.TrimSpace(s.Role) == "" {
			v.err(sp+".role", "the role is required")
		}
		if s.Approvals < 1 {
			v.err(sp+".approvals", "must be at least 1")
		}
	}
}

func (v *validator) payments(c *Config) {
	for field, role := range map[string]string{"release_role": c.Payments.ReleaseRole, "vendor_approver_role": c.Payments.VendorApproverRole} {
		if strings.TrimSpace(role) == "" {
			v.err("payments."+field, "the role is required")
		}
	}
	// One person releases a run; a second approves the release of a run
	// above its company's release_dual_limit first (ADR-0038). A document
	// asking for several releasers is refused, so that control is never
	// activated unenforced.
	switch n := c.Payments.ReleaseApprovals; {
	case n < 1:
		v.err("payments.release_approvals", "must be at least 1")
	case n > 1:
		v.err("payments.release_approvals", "must be 1: one person releases a run; set companies[].release_dual_limit for a second person's approval of large runs")
	}
	if c.Payments.BankChangeHoldDays < 0 {
		v.err("payments.bank_change_hold_days", "must not be negative")
	}
}

// ValidIBAN checks an IBAN with the ISO 13616 mod-97 algorithm.
func ValidIBAN(iban string) bool { return bankfile.ValidIBAN(iban) }
