package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/bankfile"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/money"
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
// activated when no finding has error severity.
func Validate(c *Config) []Finding {
	v := &validator{}
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
	v.roles(c)
	v.users(c)
	v.agents(c)
	v.purchasing(c)
	v.payments(c)
	v.sod(c)
	v.numberRanges(c)
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

type validator struct{ out []Finding }

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
	if !currencyRe.MatchString(co.Currency) {
		v.err(p+".currency", "currency %q must be an ISO 4217 code", co.Currency)
	}
	if !countryRe.MatchString(co.Country) {
		v.err(p+".country", "country %q must be an ISO 3166 two-letter code", co.Country)
	}
	if co.FiscalYearStartMonth < 1 || co.FiscalYearStartMonth > 12 {
		v.err(p+".fiscal_year_start_month", "must be 1-12")
	}
	types := map[core.AccountType]bool{core.Asset: true, core.Liability: true, core.Equity: true, core.Revenue: true, core.Expense: true}
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
			if a.Type != core.Liability {
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
	ad := co.AccountDetermination
	check := func(field, number string, want core.AccountType, recon bool) {
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
		}
	}
	check("ap_reconciliation", ad.APReconciliation, core.Liability, true)
	check("input_tax", ad.InputTax, core.Asset, false)
	check("bank", ad.Bank, core.Asset, false)
	if ad.FXGain != "" {
		check("fx_gain", ad.FXGain, core.Revenue, false)
	}
	if ad.FXLoss != "" {
		check("fx_loss", ad.FXLoss, core.Expense, false)
	}

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
	if co.InvoiceTolerance.PricePercent < 0 || co.InvoiceTolerance.PricePercent > 100 {
		v.err(p+".invoice_tolerance.price_percent", "must be 0-100")
	}
	v.amount(p+".invoice_tolerance.price_absolute", co.InvoiceTolerance.PriceAbsolute, decimalsFor(co.Currency))
	v.amount(p+".auto_post_limit", co.AutoPostLimit, decimalsFor(co.Currency))
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

func (v *validator) roles(c *Config) {
	seen := map[string]bool{}
	for _, r := range c.Roles {
		p := fmt.Sprintf("roles[%s]", r.Name)
		if r.Name == "" || seen[r.Name] {
			v.err(p, "role name must be present and unique")
		}
		seen[r.Name] = true
		for _, perm := range r.Permissions {
			if !slices.Contains(governance.Permissions, perm) {
				v.err(p+".permissions", "unknown permission %q", perm)
			}
		}
	}
}

func (v *validator) users(c *Config) {
	seen := map[string]bool{}
	for _, u := range c.Users {
		p := fmt.Sprintf("users[%s]", u.ID)
		if u.ID == "" || seen[u.ID] {
			v.err(p, "user id must be present and unique")
		}
		seen[u.ID] = true
		for _, r := range u.Roles {
			if c.Role(r) == nil {
				v.err(p+".roles", "unknown role %q", r)
			}
		}
		for _, cc := range u.Companies {
			if c.Company(cc) == nil {
				v.err(p+".companies", "unknown company %q", cc)
			}
		}
	}
}

func (v *validator) agents(c *Config) {
	seen := map[string]bool{}
	for _, a := range c.Agents {
		p := fmt.Sprintf("agents[%s]", a.ID)
		if !strings.HasPrefix(a.ID, "agent:") || seen[a.ID] {
			v.err(p, "agent id must start with \"agent:\" and be unique")
		}
		seen[a.ID] = true
		if a.Autonomy < 0 || a.Autonomy > 3 {
			v.err(p+".autonomy", "must be 0-3")
		}
		for _, perm := range a.Permissions {
			if !slices.Contains(governance.Permissions, perm) {
				v.err(p+".permissions", "unknown permission %q", perm)
			}
			if slices.Contains(governance.ApprovalPermissions, perm) {
				v.err(p+".permissions", "agents may not hold approval permission %q", perm)
			}
		}
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
			if m, ok := v.amount(p+".up_to", t.UpTo, 2); ok {
				if m <= prev {
					v.err(p+".up_to", "tiers must be in increasing order")
				}
				prev = m
			}
		}
		for j, s := range t.Steps {
			sp := fmt.Sprintf("%s.steps[%d]", p, j)
			r := c.Role(s.Role)
			if r == nil {
				v.err(sp+".role", "unknown role %q", s.Role)
			} else if !slices.Contains(r.Permissions, governance.ActRequisitionApp) {
				v.err(sp+".role", "role %q lacks %s", s.Role, governance.ActRequisitionApp)
			}
			if s.Approvals < 1 {
				v.err(sp+".approvals", "must be at least 1")
			}
		}
	}
}

func (v *validator) payments(c *Config) {
	check := func(field, role, perm string) {
		r := c.Role(role)
		switch {
		case r == nil:
			v.err("payments."+field, "unknown role %q", role)
		case !slices.Contains(r.Permissions, perm):
			v.err("payments."+field, "role %q lacks %s", role, perm)
		}
	}
	check("release_role", c.Payments.ReleaseRole, governance.ActPaymentRun)
	check("vendor_approver_role", c.Payments.VendorApproverRole, governance.ActVendorApprove)
	if c.Payments.ReleaseApprovals < 1 {
		v.err("payments.release_approvals", "must be at least 1")
	}
	if c.Payments.BankChangeHoldDays < 0 {
		v.err("payments.bank_change_hold_days", "must not be negative")
	}
}

// sod checks every role, user and agent against the conflict pairs.
func (v *validator) sod(c *Config) {
	if len(c.SoDConflicts) == 0 {
		v.warn("sod_conflicts", "no segregation-of-duties rules configured")
	}
	for i, pair := range c.SoDConflicts {
		for _, perm := range pair {
			if !slices.Contains(governance.Permissions, perm) {
				v.err(fmt.Sprintf("sod_conflicts[%d]", i), "unknown permission %q", perm)
			}
		}
	}
	conflict := func(perms []string) (string, bool) {
		for _, pair := range c.SoDConflicts {
			if slices.Contains(perms, pair[0]) && slices.Contains(perms, pair[1]) {
				return pair[0] + " and " + pair[1], true
			}
		}
		return "", false
	}
	for _, r := range c.Roles {
		if what, bad := conflict(r.Permissions); bad {
			v.err(fmt.Sprintf("roles[%s]", r.Name), "segregation of duties: role combines %s", what)
		}
	}
	for _, u := range c.Users {
		if what, bad := conflict(c.UserPermissions(u.ID)); bad {
			v.err(fmt.Sprintf("users[%s]", u.ID), "segregation of duties: user's roles combine %s", what)
		}
	}
	for _, a := range c.Agents {
		if what, bad := conflict(a.Permissions); bad {
			v.err(fmt.Sprintf("agents[%s]", a.ID), "segregation of duties: agent combines %s", what)
		}
	}
}

// UserPermissions is the union of permissions across a user's roles.
func (c *Config) UserPermissions(id string) []string {
	u := c.User(id)
	if u == nil {
		return nil
	}
	var out []string
	for _, rn := range u.Roles {
		if r := c.Role(rn); r != nil {
			for _, p := range r.Permissions {
				if !slices.Contains(out, p) {
					out = append(out, p)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// ValidIBAN checks an IBAN with the ISO 13616 mod-97 algorithm.
func ValidIBAN(iban string) bool { return bankfile.ValidIBAN(iban) }

func decimalsFor(currency string) int {
	switch currency {
	case "JPY", "KRW", "CLP", "ISK", "VND":
		return 0
	case "BHD", "KWD", "OMR", "JOD", "TND":
		return 3
	}
	return 2
}
