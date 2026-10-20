package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/bankfile"
	"github.com/mustie7iy/agentic-erp/internal/money"
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
	for i, co := range c.Companies {
		p := fmt.Sprintf("companies[%s]", co.Code)
		if co.Code == "" {
			p = fmt.Sprintf("companies[%d]", i)
			v.err(p+".code", "company code is required")
		}
		if seen[co.Code] {
			v.err(p, "duplicate company code")
		}
		seen[co.Code] = true
		v.company(p, &co)
	}
	v.paymentTerms(c)
	v.purchasing(c)
	v.payments(c)
	v.numberRanges(c)
	v.journalRanges(c)
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
	v.chartOfAccounts(p, co)
	v.accountDetermination(p, co)
	v.taxCodes(p, co)
	v.withholdingTaxCodes(p, co)
	if co.InvoiceTolerance.PricePercent < 0 || co.InvoiceTolerance.PricePercent > 100 {
		v.err(p+".invoice_tolerance.price_percent", "must be 0-100")
	}
	// Amounts are read with the currency's digits from ref.currency; with
	// no known currency there are none to read them with, and the
	// currency's finding already blocks activation.
	if known {
		v.amount(p+".invoice_tolerance.price_absolute", co.InvoiceTolerance.PriceAbsolute, exp)
		v.amount(p+".auto_post_limit", co.AutoPostLimit, exp)
		v.amount(p+".manual_entry_approval_limit", co.ManualEntryApprovalLimit, exp)
	}
	if co.OverDeliveryPercent < 0 || co.OverDeliveryPercent > 100 {
		v.err(p+".gr_over_delivery_percent", "must be 0-100")
	}
	v.houseBanks(p, co)
	v.paymentMethods(p, co)
}

// isTaxAccount reports whether the account is the input tax account or a
// tax code's.
func (c *Company) isTaxAccount(number string) bool {
	if number != "" && number == c.AccountDetermination.InputTax {
		return true
	}
	return slices.ContainsFunc(c.TaxCodes, func(t TaxCode) bool { return number != "" && t.Account == number })
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
		hp := p + ".house_bank"
		if i > 0 {
			hp = fmt.Sprintf("%s.house_banks[%s]", p, hb.Name)
		}
		if codes[hb.Name] {
			v.err(hp+".name", "duplicate house bank")
		}
		codes[hb.Name] = true
		if !ValidIBAN(hb.IBAN) {
			v.err(hp+".iban", "IBAN %q fails the ISO 13616 check", hb.IBAN)
		}
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
	banks := map[string]bool{}
	for _, hb := range co.AllHouseBanks() {
		banks[hb.Name] = true
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
			v.rankedBanks(rp, banks, r.HouseBanks)
		}
	}
}

// rankedBanks checks that a ranking names at least one house bank, each a
// house bank of the company and named once.
func (v *validator) rankedBanks(rp string, banks map[string]bool, ranked []string) {
	if len(ranked) == 0 {
		v.err(rp+".house_banks", "rank at least one house bank")
	}
	seen := map[string]bool{}
	for _, name := range ranked {
		if !banks[name] || seen[name] {
			v.err(rp+".house_banks", "house bank %s: a house bank of the company, each once", name)
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

// taxCodes checks the company's tax codes: unique, a rate of at most 100%
// and an account in the chart.
func (v *validator) taxCodes(p string, co *Company) {
	codes := map[string]bool{}
	for _, t := range co.TaxCodes {
		tp := fmt.Sprintf("%s.tax_codes[%s]", p, t.Code)
		if t.Code == "" || codes[t.Code] {
			v.err(tp, "tax code must be present and unique")
		}
		codes[t.Code] = true
		if r, ok := v.amount(tp+".rate_percent", t.RatePercent, 3); ok && r > 100_000 {
			v.err(tp+".rate_percent", "rate above 100%%")
		}
		switch a := co.Account(t.Account); {
		case a == nil:
			v.err(tp+".account", "account %s is not in the chart of accounts", t.Account)
		case t.Account == co.AccountDetermination.GRIR:
			v.err(tp+".account", "account %s is the GR/IR account, which only goods receipts and invoices' order items post", t.Account)
		}
	}
	if len(co.TaxCodes) == 0 {
		v.warn(p+".tax_codes", "no tax codes; captured invoices cannot be checked for tax rate")
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
			if m, ok := v.threshold(p+".up_to", t.UpTo, c); ok {
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
	if c.Payments.ReleaseApprovals < 1 {
		v.err("payments.release_approvals", "must be at least 1")
	}
	if c.Payments.BankChangeHoldDays < 0 {
		v.err("payments.bank_change_hold_days", "must not be negative")
	}
}

// ValidIBAN checks an IBAN with the ISO 13616 mod-97 algorithm.
func ValidIBAN(iban string) bool { return bankfile.ValidIBAN(iban) }
