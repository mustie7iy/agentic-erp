package config

import (
	"regexp"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// AssetClass is a class of assets (SAP ANKA, ADR-0057 §3): its code and
// name, and the depreciation terms an asset of the class starts with in
// each ledger. Its accounts are each company's (Company.AssetAccounts).
type AssetClass struct {
	Code  string       `yaml:"code" json:"code"`
	Name  string       `yaml:"name" json:"name"`
	Terms []AssetTerms `yaml:"terms" json:"terms"`
}

// AssetTerms are a class's default depreciation terms in one ledger (SAP's
// depreciation area, ADR-0057 §7): the method, the useful life in months
// and, for the declining balance method, its factor, a plain decimal.
type AssetTerms struct {
	Ledger           string `yaml:"ledger" json:"ledger"`
	Method           string `yaml:"method" json:"method"`
	UsefulLifeMonths int    `yaml:"useful_life_months" json:"useful_life_months"`
	DecliningFactor  string `yaml:"declining_factor,omitempty" json:"declining_factor,omitempty"`
}

// Depreciation methods (ADR-0057 §7).
const (
	StraightLine     = "straight_line"
	DecliningBalance = "declining_balance"
)

// AssetAccounts are the accounts a company posts one class's assets to
// (SAP's account determination per class, ADR-0057 §3): the acquisition
// cost and accumulated depreciation, both asset reconciliation accounts;
// the depreciation expense; the asset clearing account acquisitions
// settle; the gain and loss on retirements; and the clearing account a
// sale's proceeds wait on until receivables bills them.
type AssetAccounts struct {
	Class                   string `yaml:"class" json:"class"`
	AcquisitionCost         string `yaml:"acquisition_cost" json:"acquisition_cost"`
	AccumulatedDepreciation string `yaml:"accumulated_depreciation" json:"accumulated_depreciation"`
	Depreciation            string `yaml:"depreciation" json:"depreciation"`
	Clearing                string `yaml:"clearing" json:"clearing"`
	Gain                    string `yaml:"gain" json:"gain"`
	Loss                    string `yaml:"loss" json:"loss"`
	SaleClearing            string `yaml:"sale_clearing" json:"sale_clearing"`
}

// AssetClass is the class with the code, or nil.
func (c *Config) AssetClass(code string) *AssetClass {
	for i := range c.AssetClasses {
		if c.AssetClasses[i].Code == code {
			return &c.AssetClasses[i]
		}
	}
	return nil
}

// TermsIn is the class's terms in the ledger, or nil.
func (a *AssetClass) TermsIn(ledger string) *AssetTerms {
	for i := range a.Terms {
		if a.Terms[i].Ledger == ledger {
			return &a.Terms[i]
		}
	}
	return nil
}

// AssetRanges are the number ranges a company with asset accounts needs in
// the current fiscal year (ADR-0057 §3): asset numbers, and the journal
// entries of acquisitions, retirements and transfers (AA) and of
// depreciation (AF). ledger.Setup.Check requires them, since only it knows
// the company's date.
var AssetRanges = []string{"asset", "journal_aa", "journal_af"}

var assetClassRe = regexp.MustCompile(`^[A-Z0-9_]{1,20}$`)

// declining factor bounds, in ten-thousandths: above 1, at most 10.
const (
	factorDecimals = 4
	factorOne      = 10_000
	factorMax      = 100_000
)

// assetClasses checks the asset classes (ADR-0057 §3): unique codes of up
// to 20 upper-case letters, digits or underscores, a name, and terms for at
// least one ledger, each a ledger of the document once; then each
// company's asset accounts.
func (v *validator) assetClasses(c *Config) {
	known := map[string]bool{}
	for _, l := range c.LedgerList() {
		known[l.Code] = true
	}
	seen := map[string]bool{}
	for _, ac := range c.AssetClasses {
		p := "asset_classes[" + ac.Code + "]"
		switch {
		case !assetClassRe.MatchString(ac.Code):
			v.err(p+".code", "an asset class's code is 1-20 upper-case letters, digits or underscores")
		case seen[ac.Code]:
			v.err(p, "duplicate asset class")
		}
		seen[ac.Code] = true
		if strings.TrimSpace(ac.Name) == "" {
			v.err(p+".name", "name is required")
		}
		if len(ac.Terms) == 0 {
			v.err(p+".terms", "give the class's depreciation terms in at least one ledger")
		}
		ledgers := map[string]bool{}
		for _, t := range ac.Terms {
			v.assetTerms(p+".terms["+t.Ledger+"]", t, known, ledgers)
		}
	}
	for _, co := range c.Companies {
		v.assetAccounts(c, "companies["+co.Code+"]", &co)
		v.notAutomatic("companies["+co.Code+"]", &co)
	}
}

// notAutomatic refuses an asset reconciliation account wherever the
// company names an account the system posts to automatically (account
// determination, tax codes and their components, withholding taxes,
// foreign-currency valuation, house banks, statement posting rules): only
// assets post to it (ADR-0057 §3), so every such posting would be refused.
func (v *validator) notAutomatic(p string, co *Company) {
	for _, a := range co.AutomaticAccounts() {
		if acc := co.Account(a.Account); acc != nil && acc.AssetReconciliation {
			v.err(p+"."+a.Path, "account %s is an asset reconciliation account, posted only through assets", a.Account)
		}
	}
}

// assetTerms checks one ledger's terms: a known ledger named once, a
// method, a useful life of 1-1200 months, and a declining factor above 1
// and at most 10 with up to four decimals, given exactly with the
// declining balance method.
func (v *validator) assetTerms(tp string, t AssetTerms, known, seen map[string]bool) {
	switch {
	case !known[t.Ledger]:
		v.err(tp+".ledger", "no ledger %s", t.Ledger)
	case seen[t.Ledger]:
		v.err(tp, "the class has terms in ledger %s twice", t.Ledger)
	}
	seen[t.Ledger] = true
	if t.Method != StraightLine && t.Method != DecliningBalance {
		v.err(tp+".method", "must be straight_line or declining_balance")
	}
	if t.UsefulLifeMonths < 1 || t.UsefulLifeMonths > 1200 {
		v.err(tp+".useful_life_months", "must be 1-1200 months")
	}
	switch {
	case t.Method == DecliningBalance && t.DecliningFactor == "":
		v.err(tp+".declining_factor", "is required with the declining balance method")
	case t.Method != DecliningBalance && t.DecliningFactor != "":
		v.err(tp+".declining_factor", "is given only with the declining balance method")
	case t.DecliningFactor != "":
		if f, err := money.ParseDecimal(t.DecliningFactor, factorDecimals); err != nil || f <= factorOne || f > factorMax {
			v.err(tp+".declining_factor", "a plain decimal above 1 and at most 10, with up to four decimals")
		}
	}
}

// assetRole is one account of a class's asset accounts.
type assetRole struct {
	field, number string
	want          AccountType
	reconciles    bool // an asset reconciliation account
}

// assetAccounts checks a company's asset accounts (ADR-0057 §3): each of a
// class of the document, once, with terms in every ledger the company
// keeps; each account in the chart with its type, the acquisition cost and
// accumulated depreciation asset reconciliation accounts and the others
// not; the two reconciliation accounts and the two clearing accounts
// apart; and the clearing accounts without open items or revaluation,
// since a cleared or revalued line would keep an acquisition or a sale
// from being reversed.
func (v *validator) assetAccounts(c *Config, p string, co *Company) {
	seen := map[string]bool{}
	for _, aa := range co.AssetAccounts {
		ap := p + ".asset_accounts[" + aa.Class + "]"
		cls := c.AssetClass(aa.Class)
		switch {
		case cls == nil:
			v.err(ap+".class", "no asset class %s", aa.Class)
		case seen[aa.Class]:
			v.err(ap, "the company names the accounts of class %s twice", aa.Class)
		}
		seen[aa.Class] = true
		for _, l := range co.LedgerCodes() {
			if cls != nil && cls.TermsIn(l) == nil {
				v.err(ap+".class", "class %s has no terms in ledger %s, which the company keeps", aa.Class, l)
			}
		}
		for _, r := range []assetRole{{"acquisition_cost", aa.AcquisitionCost, Asset, true}, {"accumulated_depreciation", aa.AccumulatedDepreciation, Asset, true},
			{"depreciation", aa.Depreciation, Expense, false}, {"clearing", aa.Clearing, Asset, false}, {"gain", aa.Gain, Revenue, false},
			{"loss", aa.Loss, Expense, false}, {"sale_clearing", aa.SaleClearing, Asset, false}} {
			v.assetAccount(ap+"."+r.field, co, r)
		}
		if aa.AccumulatedDepreciation != "" && aa.AccumulatedDepreciation == aa.AcquisitionCost {
			v.err(ap+".accumulated_depreciation", "the accumulated depreciation has an account of its own, not the acquisition cost's")
		}
		if aa.SaleClearing != "" && aa.SaleClearing == aa.Clearing {
			v.err(ap+".sale_clearing", "sales clear an account of their own, not the acquisitions' clearing account")
		}
		for _, r := range []assetRole{{field: "clearing", number: aa.Clearing}, {field: "sale_clearing", number: aa.SaleClearing}} {
			v.notOpenOrRevalued(ap+"."+r.field, ap+"."+r.field, co.Account(r.number))
		}
	}
}

// assetAccount checks one asset account: in the chart, of the type its use
// needs, no vendor reconciliation account, and an asset reconciliation
// account exactly where the use is one.
func (v *validator) assetAccount(path string, co *Company, r assetRole) {
	a := co.Account(r.number)
	switch {
	case r.number == "":
		v.err(path, "is required")
	case a == nil:
		v.err(path, "account %s is not in the chart of accounts", r.number)
	case a.Type != r.want:
		v.err(path, "account %s is %s, expected %s", r.number, a.Type, r.want)
	case a.PartnerReconciliation():
		v.err(path, "account %s is a supplier or customer reconciliation account, posted only through the partner's items", r.number)
	case r.reconciles && !a.AssetReconciliation:
		v.err(path, "account %s is not an asset reconciliation account (asset_reconciliation)", r.number)
	case !r.reconciles && a.AssetReconciliation:
		v.err(path, "account %s is an asset reconciliation account, which takes only assets' acquisition cost or accumulated depreciation", r.number)
	}
}

// assetReconciliation checks an account flagged asset_reconciliation: an
// asset account that reconciles no supplier, without open items or
// revaluation (notOpenOrRevalued).
func (v *validator) assetReconciliation(ap string, a *Account) {
	if !a.AssetReconciliation {
		return
	}
	switch {
	case a.Type != Asset:
		v.err(ap+".asset_reconciliation", "an asset reconciliation account is an asset account")
	case a.VendorReconciliation:
		v.err(ap+".asset_reconciliation", "an account reconciles either suppliers or assets, not both")
	}
	v.notOpenOrRevalued(ap+".open_items", ap+".revalue_foreign_currency", a)
}

// notOpenOrRevalued refuses open-item management and foreign-currency
// revaluation on an account assets post to (ADR-0057 §3), reported at
// openPath and revaluePath: a cleared line cannot be reversed
// (ledger.entry_cleared), which would block reversing an acquisition and
// the supplier invoice behind it, and a revaluation would change an
// asset's booked values outside its movements. a is nil for an account
// the chart lacks, which the account's own check reports.
func (v *validator) notOpenOrRevalued(openPath, revaluePath string, a *Account) {
	if a == nil {
		return
	}
	if a.OpenItems {
		v.err(openPath, "account %s takes assets' postings, so it is not open-item managed (open_items)", a.Number)
	}
	if a.RevalueForeignCurrency {
		v.err(revaluePath, "account %s takes assets' postings, so it is not revalued (revalue_foreign_currency)", a.Number)
	}
}

// AssetClearingAccounts are the numbers of the company's asset clearing
// accounts, each once, in the order its classes name them (ADR-0057 §6:
// an order item on one of them is an asset's).
func (c *Company) AssetClearingAccounts() []string {
	var numbers []string
	for _, a := range c.AssetAccounts {
		if !slices.Contains(numbers, a.Clearing) {
			numbers = append(numbers, a.Clearing)
		}
	}
	return numbers
}

// AssetAccountsOf is the company's accounts of the class, or nil.
func (c *Company) AssetAccountsOf(class string) *AssetAccounts {
	i := slices.IndexFunc(c.AssetAccounts, func(a AssetAccounts) bool { return a.Class == class })
	if i < 0 {
		return nil
	}
	return &c.AssetAccounts[i]
}
