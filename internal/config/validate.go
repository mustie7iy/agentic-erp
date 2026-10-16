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
	v.grIRJournalRanges(c)
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
	if !ValidIBAN(co.HouseBank.IBAN) {
		v.err(p+".house_bank.iban", "IBAN %q fails the ISO 13616 check", co.HouseBank.IBAN)
	}
	if !bicRe.MatchString(co.HouseBank.BIC) {
		v.err(p+".house_bank.bic", "BIC %q is not a valid SWIFT code", co.HouseBank.BIC)
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
			if a.Type != Liability {
				v.err(ap, "a vendor reconciliation account must be a liability")
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

// accountDetermination checks that each determined account is in the chart
// with the type its use needs, a vendor reconciliation account where the
// use is one and no other account; the FX accounts are optional, and the
// GR/IR account comes with a price difference account.
func (v *validator) accountDetermination(p string, co *Company) {
	ad := co.AccountDetermination
	check := func(field, number string, want AccountType, recon bool) {
		a := co.Account(number)
		switch {
		case number == "":
			v.err(p+".account_determination."+field, "is required")
		case a == nil:
			v.err(p+".account_determination."+field, "account %s is not in the chart of accounts", number)
		case a.Type != want:
			v.err(p+".account_determination."+field, "account %s is %s, expected %s", number, a.Type, want)
		case recon && !a.VendorReconciliation:
			v.err(p+".account_determination."+field, "account %s is not a vendor reconciliation account", number)
		case !recon && a.VendorReconciliation:
			v.err(p+".account_determination."+field, "account %s is a vendor reconciliation account, posted only through the supplier's items", number)
		}
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
}

// grIRJournalRanges requires a WE journal range (journal_we) wherever a
// company that values receipts through GR/IR numbers goods receipts: each
// receipt on such an order posts a WE entry in its fiscal year (T33).
func (v *validator) grIRJournalRanges(c *Config) {
	have := map[string]bool{}
	for _, n := range c.NumberRanges {
		have[n.Key()] = true
	}
	for _, n := range c.NumberRanges {
		co := c.Company(n.Company)
		if n.DocumentType != "goods_receipt" || co == nil || co.AccountDetermination.GRIR == "" {
			continue
		}
		we := NumberRange{Company: n.Company, DocumentType: "journal_we", FiscalYear: n.FiscalYear}
		if !have[we.Key()] {
			v.err("number_ranges["+we.Key()+"]", "goods receipts of company %s post WE entries to GR/IR in fiscal year %d; add a journal_we range",
				n.Company, n.FiscalYear)
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
		if a := co.Account(t.Account); a == nil {
			v.err(tp+".account", "account %s is not in the chart of accounts", t.Account)
		}
	}
	if len(co.TaxCodes) == 0 {
		v.warn(p+".tax_codes", "no tax codes; captured invoices cannot be checked for tax rate")
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
	}
	if len(c.PaymentTerms) == 0 {
		v.err("payment_terms", "at least one payment term is required")
	}
}

func (v *validator) purchasing(c *Config) {
	tiers := c.Purchasing.RequisitionApproval
	if len(tiers) == 0 {
		v.err("purchasing.requisition_approval", "at least one tier is required")
		return
	}
	var prev money.Minor = -1
	for i, t := range tiers {
		p := fmt.Sprintf("purchasing.requisition_approval[%d]", i)
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

// threshold reads a requisition approval limit. A limit applies to
// requisitions in every company's currency, so it must read with the
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
