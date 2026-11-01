// Package config is configuration as code: the settings SAP keeps in
// customizing (SPRO) live here as one typed YAML document, validated and
// diffed by tier. The configuration context (03-data-model §3.5) keeps its
// versions and change requests; who may do what belongs to the identity
// context, so the document names roles (in approval steps) but does not
// define them.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	// The zone database is built in so that the standard IANA names load
	// on any host, an image without zoneinfo included; host-only names
	// are refused by Company.Location (T72n1).
	_ "time/tzdata"

	"gopkg.in/yaml.v3"

	"github.com/mustie7iy/agentic-erp/internal/platform/cabank"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

type Config struct {
	Version      int           `yaml:"version" json:"version"`
	Companies    []Company     `yaml:"companies" json:"companies"`
	PaymentTerms []PaymentTerm `yaml:"payment_terms" json:"payment_terms"`
	Purchasing   Purchasing    `yaml:"purchasing" json:"purchasing"`
	Payments     Payments      `yaml:"payments" json:"payments"`
	Output       Output        `yaml:"output" json:"output"`
	NumberRanges []NumberRange `yaml:"number_ranges,omitempty" json:"number_ranges,omitempty"`
	// Ledgers and LedgerGroups are the tenant's parallel ledgers (T52a,
	// ADR-0045); without them the tenant keeps only the leading ledger 0L.
	Ledgers      []Ledger      `yaml:"ledgers,omitempty" json:"ledgers,omitempty"`
	LedgerGroups []LedgerGroup `yaml:"ledger_groups,omitempty" json:"ledger_groups,omitempty"`
	// Extensions are the tenant's custom field schemas by entity (ADR-0046).
	Extensions map[string]extensions.Schema `yaml:"extensions,omitempty" json:"extensions,omitempty"`
	// Rules are the tenant's validation rules by entity and name (ADR-0046).
	Rules map[string]map[string]rules.Rule `yaml:"rules,omitempty" json:"rules,omitempty"`
	// Analytics is the semantic layer's governed model; nil: the seed
	// (internal/analytics/semantic.yaml) until a change request adds it.
	Analytics *Analytics `yaml:"analytics,omitempty" json:"analytics,omitempty"`
	// Packs records, per company, the country pack it adopted (ADR-0048):
	// the base the next upgrade of the pack merges from.
	Packs []PackAdoption `yaml:"packs,omitempty" json:"packs,omitempty"`
	// Calendars are the companies' holidays (ADR-0048), one calendar per
	// company: the days, besides weekends, on which it does no business.
	Calendars []Calendar `yaml:"calendars,omitempty" json:"calendars,omitempty"`
	// Controlling holds the tenant-wide rules about cost and profit
	// centers (ADR-0050 §4); the centers themselves are master data.
	Controlling Controlling `yaml:"controlling,omitempty" json:"controlling,omitzero"`
	// Cash is the tenant's liquidity items, planning groups and the rules
	// assigning flows their items (ADR-0051 §4).
	Cash Cash `yaml:"cash,omitempty" json:"cash,omitzero"`
}

// Calendar is a company's holidays, each date once.
type Calendar struct {
	Company  string    `yaml:"company" json:"company"`
	Holidays []Holiday `yaml:"holidays" json:"holidays"`
}

// Holiday is a day the company does no business, on Date (YYYY-MM-DD).
type Holiday struct {
	Date string `yaml:"date" json:"date"`
	Name string `yaml:"name" json:"name"`
}

// Calendar is the company's calendar, or nil.
func (c *Config) Calendar(company string) *Calendar {
	for i := range c.Calendars {
		if c.Calendars[i].Company == company {
			return &c.Calendars[i]
		}
	}
	return nil
}

// Holiday is the company's holiday on the date, if it has one: the read
// other contexts use to tell whether the company does business that day.
// The date is d's calendar date in d's own location, so callers pass the
// company's business date, not an instant in UTC. It returns a copy, so a
// reader cannot change the active document.
func (c *Config) Holiday(company string, d time.Time) (Holiday, bool) {
	if cal := c.Calendar(company); cal != nil {
		date := d.Format(time.DateOnly)
		for _, h := range cal.Holidays {
			if h.Date == date {
				return h, true
			}
		}
	}
	return Holiday{}, false
}

// LeadingLedger is the code of the leading ledger every company keeps.
const LeadingLedger = "0L"

// Ledger is a ledger of the tenant (SAP FINSC_LEDGER): the leading ledger
// 0L or a parallel ledger kept under another accounting principle. Once
// activated only its name changes.
type Ledger struct {
	Code                string `yaml:"code" json:"code"`
	Name                string `yaml:"name" json:"name"`
	AccountingPrinciple string `yaml:"accounting_principle" json:"accounting_principle"`
}

// LedgerGroup names ledgers a posting goes to together (SAP FAGL ledger
// group); its representative ledger decides the posting period. Once
// activated it keeps its members and representative; only its name and
// new members change.
type LedgerGroup struct {
	Code           string   `yaml:"code" json:"code"`
	Name           string   `yaml:"name" json:"name"`
	Representative string   `yaml:"representative" json:"representative"`
	Ledgers        []string `yaml:"ledgers" json:"ledgers"`
}

// LedgerList is the tenant's ledgers: those of the document, or the
// leading ledger alone when it names none.
func (c *Config) LedgerList() []Ledger {
	if len(c.Ledgers) > 0 {
		return c.Ledgers
	}
	return []Ledger{{Code: LeadingLedger, Name: "Leading ledger", AccountingPrinciple: "IFRS"}}
}

// LedgerCodes is the company's ledgers: those it names, or the leading
// ledger alone.
func (c *Company) LedgerCodes() []string {
	if len(c.Ledgers) > 0 {
		return c.Ledgers
	}
	return []string{LeadingLedger}
}

// PackAdoption is the country pack version a company adopted and the date
// it was applied on, which picked the tax rates then in force.
type PackAdoption struct {
	Company   string `yaml:"company" json:"company"`
	Country   string `yaml:"country" json:"country"`
	Version   string `yaml:"version" json:"version"`
	AppliedOn string `yaml:"applied_on" json:"applied_on"`
}

// Pack is the adoption recorded for a company, or nil.
func (c *Config) Pack(company string) *PackAdoption {
	for i := range c.Packs {
		if c.Packs[i].Company == company {
			return &c.Packs[i]
		}
	}
	return nil
}

// NumberRange is a document number interval for one company, document type
// and fiscal year (SAP FBN1/SNRO). Once activated only Last may change, and
// only upward: issued numbers must stay valid.
type NumberRange struct {
	Company      string `yaml:"company" json:"company"`
	DocumentType string `yaml:"document_type" json:"document_type"`
	FiscalYear   int    `yaml:"fiscal_year" json:"fiscal_year"`
	Prefix       string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	First        int64  `yaml:"first" json:"first"`
	Last         int64  `yaml:"last" json:"last"`
	Gapless      bool   `yaml:"gapless,omitempty" json:"gapless,omitempty"`
}

// Key identifies a number range in the document: company/type/year.
func (n NumberRange) Key() string {
	return fmt.Sprintf("%s/%s/%d", n.Company, n.DocumentType, n.FiscalYear)
}

type Company struct {
	Code                 string `yaml:"code" json:"code"`
	Name                 string `yaml:"name" json:"name"`
	Currency             string `yaml:"currency" json:"currency"`
	Country              string `yaml:"country" json:"country"`
	FiscalYearStartMonth int    `yaml:"fiscal_year_start_month" json:"fiscal_year_start_month"`
	// TimeZone is the IANA name of the zone the company's business dates
	// are in (America/Toronto, Asia/Dubai): which day is today for it.
	// Validation requires it (product owner 2026-10-31); a document
	// activated before then may lack it and is read as UTC, as every date
	// was before it was set (T72n1).
	TimeZone             string               `yaml:"time_zone,omitempty" json:"time_zone,omitempty"`
	ChartOfAccounts      []Account            `yaml:"chart_of_accounts" json:"chart_of_accounts"`
	AccountDetermination AccountDetermination `yaml:"account_determination" json:"account_determination"`
	TaxCodes             []TaxCode            `yaml:"tax_codes" json:"tax_codes"`
	// TaxRegistrations are the company's registrations with tax
	// authorities, from which its tax filing periods follow (ADR-0049 §10,
	// T61g).
	TaxRegistrations []TaxRegistration `yaml:"tax_registrations,omitempty" json:"tax_registrations,omitempty"`
	// WithholdingTaxCodes are the withholding taxes deducted from suppliers
	// (SAP extended withholding tax, T059P/T059Z; T46e).
	WithholdingTaxCodes []WithholdingTaxCode `yaml:"withholding_tax_codes,omitempty" json:"withholding_tax_codes,omitempty"`
	// TaxSettings are the authority's rate type and rounding and the
	// documentary requirements of input tax, from the country pack
	// (ADR-0049 §6, §8).
	TaxSettings      TaxSettings `yaml:"tax_settings,omitempty" json:"tax_settings,omitempty"`
	InvoiceTolerance Tolerance   `yaml:"invoice_tolerance" json:"invoice_tolerance"`
	AutoPostLimit    string      `yaml:"auto_post_limit" json:"auto_post_limit"`
	// ManualEntryApprovalLimit is the largest manual journal entry, by its
	// debits in the company's currency, that people post without approval
	// (T35); above it the controller approves the entry first.
	ManualEntryApprovalLimit string `yaml:"manual_entry_approval_limit" json:"manual_entry_approval_limit"`
	// ServicePostingLimit is the largest journal entry, by its debits in
	// the company's currency, that a service principal (an integration such
	// as a billing system) posts through the API; above it the entry is
	// parked for a person instead of posted (ADR-0049 §7, product owner
	// 2026-10-22, T61n). Empty: ManualEntryApprovalLimit, which held
	// services before the limit existed (ServiceLimit).
	ServicePostingLimit string `yaml:"service_posting_limit,omitempty" json:"service_posting_limit,omitempty"`
	// ReleaseDualLimit is the largest payment run, by its total in the
	// company's currency at the run date's rate, that one releaser
	// releases (ADR-0038, product owner 2026-10-22); above it a second
	// holder of payments.release_role approves the release first
	// (approvals.PaymentRunRelease). Empty: no run needs the second person.
	ReleaseDualLimit    string    `yaml:"release_dual_limit,omitempty" json:"release_dual_limit,omitempty"`
	OverDeliveryPercent int       `yaml:"gr_over_delivery_percent" json:"gr_over_delivery_percent"`
	HouseBank           HouseBank `yaml:"house_bank" json:"house_bank"`
	// HouseBanks are the company's further house banks (SAP T012K), each
	// with its own ledger account (ADR-0038).
	HouseBanks []HouseBank `yaml:"house_banks,omitempty" json:"house_banks,omitempty"`
	// PaymentMethods are the ways the company pays suppliers (SAP T042Z),
	// each ranking per currency the house banks that pay with it (SAP
	// T042E/T042I; ADR-0038).
	PaymentMethods []PaymentMethod `yaml:"payment_methods,omitempty" json:"payment_methods,omitempty"`
	// FXValuation is how a revaluation run values the company's open items
	// and balances in foreign currency (SAP FAGL_FCV; ADR-0042).
	FXValuation FXValuation `yaml:"fx_valuation,omitempty" json:"fx_valuation,omitempty"`
	// BankStatements says how the company's bank statements are matched
	// and posted (SAP FF_5/FEBAN, T028G; ADR-0047).
	BankStatements BankStatements `yaml:"bank_statements,omitempty" json:"bank_statements,omitzero"`
	// Ledgers are the codes of the ledgers the company keeps (SAP ledger
	// assignment), the leading ledger 0L among them; none means 0L alone.
	Ledgers []string `yaml:"ledgers,omitempty" json:"ledgers,omitempty"`
	// CostCenterAccounting requires a cost center on every line of an
	// account whose cost_element is primary_cost (ADR-0050 §4-5, decision
	// P6), from the fiscal year it is turned on in; ProfitCenterAccounting
	// requires a profit center on every P&L line (decision P2), falling
	// back to DefaultProfitCenter. Each is turned on only while the
	// company has no postings in the current fiscal year or later, and
	// never off (ledger.Setup.Check).
	CostCenterAccounting   bool   `yaml:"cost_center_accounting,omitempty" json:"cost_center_accounting,omitempty"`
	ProfitCenterAccounting bool   `yaml:"profit_center_accounting,omitempty" json:"profit_center_accounting,omitempty"`
	DefaultProfitCenter    string `yaml:"default_profit_center,omitempty" json:"default_profit_center,omitempty"`
	// DefaultAssignments give the cost center a line on an account takes
	// when none is entered or derived (SAP OKB9).
	DefaultAssignments []DefaultAssignment `yaml:"default_assignments,omitempty" json:"default_assignments,omitempty"`
	// Cash is how the company's cash position and liquidity forecast are
	// computed (ADR-0051 §5, §8-§10).
	Cash CompanyCash `yaml:"cash,omitempty" json:"cash,omitzero"`
}

// FXValuation is the company's foreign-currency valuation (ADR-0042): the
// one rate type it values at, average when none is given, and where the
// valuation of each revalued account posts. Ledgers are the parallel
// ledgers that value at a rate type of their own, their valuation area's
// (T52f, ADR-0045); the leading ledger and any other values at RateType,
// and every ledger posts to the same valuation accounts.
type FXValuation struct {
	RateType string               `yaml:"rate_type,omitempty" json:"rate_type,omitempty"`
	Accounts []FXValuationAccount `yaml:"accounts,omitempty" json:"accounts,omitempty"`
	Ledgers  []FXValuationLedger  `yaml:"ledgers,omitempty" json:"ledgers,omitempty"`
}

// FXValuationLedger is the rate type a parallel ledger the company keeps
// values at (SAP valuation area per accounting principle).
type FXValuationLedger struct {
	Code     string `yaml:"code" json:"code"`
	RateType string `yaml:"rate_type" json:"rate_type"`
}

// RateTypeOrAverage is the rate type the company values at, average when
// none is given.
func (f FXValuation) RateTypeOrAverage() string {
	if f.RateType == "" {
		return "average"
	}
	return f.RateType
}

// FXValuationAccount says where the valuation of one revalued account
// posts (SAP T030S): its unrealised gain and loss, which may be the
// realised fx_gain and fx_loss accounts, and, for a vendor reconciliation
// account, the balance-sheet adjustment account its difference goes to; a
// flagged account (revalue_foreign_currency) takes the difference itself.
type FXValuationAccount struct {
	Account        string `yaml:"account" json:"account"`
	Adjustment     string `yaml:"adjustment,omitempty" json:"adjustment,omitempty"`
	UnrealisedGain string `yaml:"unrealised_gain" json:"unrealised_gain"`
	UnrealisedLoss string `yaml:"unrealised_loss" json:"unrealised_loss"`
}

// BankStatements is how bank statement lines are matched to what they
// clear and posted (ADR-0047). A line posts only when matched.
type BankStatements struct {
	// PostingRules post the lines the bank books itself, such as charges
	// and interest, by their bank transaction code (SAP T028G).
	PostingRules []StatementPostingRule `yaml:"posting_rules,omitempty" json:"posting_rules,omitempty"`
	// MatchingRules are tried in order; a line is posted automatically only
	// when the first rule that finds candidates finds exactly one.
	MatchingRules []MatchingRule `yaml:"matching_rules,omitempty" json:"matching_rules,omitempty"`
	// ApprovalTiers route a match by the line's amount in the company's
	// currency (approvals.BankClearing); a tier without steps posts without
	// approval. There is no default: without tiers every match is approved
	// first (product owner 2026-10-20).
	ApprovalTiers []ApprovalTier `yaml:"approval_tiers,omitempty" json:"approval_tiers,omitempty"`
}

// StatementPostingRule posts statement lines with a bank transaction code
// to an account: the ISO 20022 domain, family and sub-family (PMNT-CCRD-CHRG),
// a SWIFT MT940 type (NCHG) or a bank's proprietary code, such as a BAI
// code (ADR-0047).
type StatementPostingRule struct {
	TransactionCode string `yaml:"transaction_code" json:"transaction_code"`
	Description     string `yaml:"description" json:"description"`
	Account         string `yaml:"account" json:"account"`
}

// MatchingRule is one way of finding what a statement line clears.
type MatchingRule struct {
	Kind string `yaml:"kind" json:"kind"` // one of the Match* kinds
	// AmountTolerance, DateWindowDays and Counterparty refine MatchAmount:
	// the payment's amount within the tolerance (in the company's
	// currency), its date within the window of the booking date and, when
	// Counterparty is set, the counterparty's IBAN or name the payee's.
	AmountTolerance string `yaml:"amount_tolerance,omitempty" json:"amount_tolerance,omitempty"`
	DateWindowDays  int    `yaml:"date_window_days,omitempty" json:"date_window_days,omitempty"`
	Counterparty    bool   `yaml:"counterparty,omitempty" json:"counterparty,omitempty"`
}

// Matching rule kinds (ADR-0047).
const (
	MatchEndToEndID      = "end_to_end_id"    // the payment whose end-to-end id the line carries
	MatchBatchID         = "batch_id"         // the bank file whose PmtInfId or MsgId the line carries
	MatchTransactionCode = "transaction_code" // a posting rule for the line's transaction code
	MatchAmount          = "amount"           // a payment by amount, date and counterparty
)

// AccountType classifies an account for the balance sheet and the income
// statement.
type AccountType string

const (
	Asset     AccountType = "asset"
	Liability AccountType = "liability"
	Equity    AccountType = "equity"
	Revenue   AccountType = "revenue"
	Expense   AccountType = "expense"
)

type Account struct {
	Number               string      `yaml:"number" json:"number"`
	Name                 string      `yaml:"name" json:"name"`
	Type                 AccountType `yaml:"type" json:"type"`
	VendorReconciliation bool        `yaml:"vendor_reconciliation,omitempty" json:"vendor_reconciliation,omitempty"`
	// RevalueForeignCurrency marks a balance-sheet account that reconciles
	// no supplier, such as a foreign-currency bank account, whose balance
	// in each foreign currency a revaluation run values (ADR-0042);
	// reconciliation accounts are valued through their open items.
	RevalueForeignCurrency bool `yaml:"revalue_foreign_currency,omitempty" json:"revalue_foreign_currency,omitempty"`
	// OpenItems manages the account's lines as open items that a later
	// posting clears (SAP SKB1-XOPVW), as bank clearing accounts must be
	// (ADR-0047). It is turned on only for an account without postings and
	// never turned off.
	OpenItems bool `yaml:"open_items,omitempty" json:"open_items,omitempty"`
	// CostElement says which cost object a line on the account needs
	// (ADR-0050 §4); empty takes the default of the account's type
	// (CostElementOrDefault).
	CostElement CostElement `yaml:"cost_element,omitempty" json:"cost_element,omitempty"`
}

// AccountDetermination says which accounts automatic postings use (SAP T030).
type AccountDetermination struct {
	APReconciliation string `yaml:"ap_reconciliation" json:"ap_reconciliation"`
	InputTax         string `yaml:"input_tax" json:"input_tax"`
	Bank             string `yaml:"bank" json:"bank"`
	// FXGain and FXLoss take the exchange differences payments realize in
	// local or group currency; a payment that has one needs them.
	FXGain string `yaml:"fx_gain,omitempty" json:"fx_gain,omitempty"`
	FXLoss string `yaml:"fx_loss,omitempty" json:"fx_loss,omitempty"`
	// GRIR is the goods receipt/invoice receipt clearing account (SAP
	// WRX). Orders released while it is set are valued through it: their
	// receipts credit it and their invoices clear it (T33). Once activated
	// it stays (ledger.Setup.Check).
	GRIR string `yaml:"gr_ir,omitempty" json:"gr_ir,omitempty"`
	// PriceDifference takes what an invoice on an order valued through
	// GR/IR differs from the order price; required with GRIR.
	PriceDifference string `yaml:"price_difference,omitempty" json:"price_difference,omitempty"`
	// CashDiscount takes the cash discounts payments take from suppliers
	// (SAP SKE, cash discount received); required in every company once a
	// payment term has discounts (T36a). Discounts granted to customers
	// (SAP SKT, an expense) will need a key of their own.
	CashDiscount string `yaml:"cash_discount,omitempty" json:"cash_discount,omitempty"`
	// DownPayment is the alternative reconciliation account of down
	// payments (SAP T074, special G/L A; ADR-0037): an asset that is a
	// vendor reconciliation account. Once activated it stays
	// (ledger.Setup.Check).
	DownPayment string `yaml:"down_payment,omitempty" json:"down_payment,omitempty"`
}

// WithholdingTaxCode is a withholding tax deducted from what a supplier is
// paid and owed to the tax authority instead: a rate of the invoice's net
// (the default) or gross, deducted when the invoice posts or when it is
// paid, to a liability account (the withholding tax payable).
type WithholdingTaxCode struct {
	Code        string `yaml:"code" json:"code"`
	Description string `yaml:"description" json:"description"`
	RatePercent string `yaml:"rate_percent" json:"rate_percent"`
	Base        string `yaml:"base,omitempty" json:"base,omitempty"` // WithholdingNet (also when empty) or WithholdingGross
	Account     string `yaml:"account" json:"account"`
	Posting     string `yaml:"posting" json:"posting"` // WithholdingAtInvoice or WithholdingAtPayment
}

// Withholding tax bases and posting moments.
const (
	WithholdingNet       = "net"
	WithholdingGross     = "gross"
	WithholdingAtInvoice = "invoice"
	WithholdingAtPayment = "payment"
)

// BaseOrNet is the code's base, net when none is given.
func (w WithholdingTaxCode) BaseOrNet() string {
	if w.Base == "" {
		return WithholdingNet
	}
	return w.Base
}

// Tolerance bounds the price variance on an invoice line (SAP T169G key PP)
// and how far a supplier's stated tax may differ from the calculated one:
// TaxUnitsPerCode minor units of the invoice's currency per tax code that
// charges tax (ADR-0049 §6, decision P5; nil: one).
type Tolerance struct {
	PricePercent    int    `yaml:"price_percent" json:"price_percent"`
	PriceAbsolute   string `yaml:"price_absolute" json:"price_absolute"`
	TaxUnitsPerCode *int   `yaml:"tax_units_per_code,omitempty" json:"tax_units_per_code,omitempty"`
}

// TaxUnits is the tax tolerance in minor units per tax code: one unless
// configured.
func (t Tolerance) TaxUnits() int {
	if t.TaxUnitsPerCode == nil {
		return 1
	}
	return *t.TaxUnitsPerCode
}

type HouseBank struct {
	Name string `yaml:"name" json:"name"`
	IBAN string `yaml:"iban" json:"iban"`
	BIC  string `yaml:"bic" json:"bic"`
	// InstitutionNumber, TransitNumber and AccountNumber are a Canadian
	// bank account, which has no IBAN (ADR-0038, T45d); a house bank has
	// either an IBAN or these.
	InstitutionNumber string `yaml:"institution_number,omitempty" json:"institution_number,omitempty"`
	TransitNumber     string `yaml:"transit_number,omitempty" json:"transit_number,omitempty"`
	AccountNumber     string `yaml:"account_number,omitempty" json:"account_number,omitempty"`
	// GLAccount is the bank's own ledger account, a bank clearing account
	// (ADR-0038). The main house bank may leave it empty and keeps the
	// account determination's bank account, as documents before it did;
	// a further house bank must name one.
	GLAccount string `yaml:"gl_account,omitempty" json:"gl_account,omitempty"`
	// Connector is the name of the bank connector the bank's files are
	// transmitted through (ADR-0038, T44b); without one they go to the
	// server's directory, which a transfer agent collects from.
	Connector string `yaml:"connector,omitempty" json:"connector,omitempty"`
	// MainGLAccount is the bank account itself, which the statement's
	// lines post to while they clear GLAccount (ADR-0047); a bank without
	// one has its statements imported but never posted.
	MainGLAccount string `yaml:"main_gl_account,omitempty" json:"main_gl_account,omitempty"`
	// IncomingGLAccount takes the bank's incoming payments until accounts
	// receivable clears them (ADR-0047); a bank without one leaves them
	// unposted.
	IncomingGLAccount string `yaml:"incoming_gl_account,omitempty" json:"incoming_gl_account,omitempty"`
	// Currency is the account's currency (ISO 4217); empty is the
	// company's. A bank in another currency (ADR-0051 §3, T53a) pays runs
	// and takes statements in it, and its accounts are revalued at period
	// end (ADR-0042). It never changes under the bank's name.
	Currency string `yaml:"currency,omitempty" json:"currency,omitempty"`
}

// Canadian reports whether the house bank names a Canadian account
// instead of an IBAN.
func (hb HouseBank) Canadian() bool {
	return hb.InstitutionNumber != "" || hb.TransitNumber != "" || hb.AccountNumber != ""
}

// Account is the house bank's account as it is sealed: its IBAN, or a
// Canadian account's number, without spaces and dashes.
func (hb HouseBank) Account() string {
	if hb.Canadian() {
		return cabank.Digits(hb.AccountNumber)
	}
	return hb.IBAN
}

// Routing is a Canadian house bank's institution and transit numbers as
// they are stored, without spaces and dashes; empty for an IBAN.
func (hb HouseBank) Routing() (institution, transit string) {
	return cabank.Digits(hb.InstitutionNumber), cabank.Digits(hb.TransitNumber)
}

// AllHouseBanks is the main house bank followed by the further ones.
func (c *Company) AllHouseBanks() []HouseBank {
	return append([]HouseBank{c.HouseBank}, c.HouseBanks...)
}

// HouseBankCurrency is the house bank's currency: its own, or the
// company's.
func (c *Company) HouseBankCurrency(hb HouseBank) string {
	if hb.Currency == "" {
		return c.Currency
	}
	return hb.Currency
}

// HouseBankAccount is the number of the house bank's ledger account: its
// own, or for the main bank without one the account determination's bank.
func (c *Company) HouseBankAccount(hb HouseBank) string {
	if hb.GLAccount == "" {
		return c.AccountDetermination.Bank
	}
	return hb.GLAccount
}

// PaymentMethod is a way of paying suppliers, such as a SEPA credit
// transfer, identified by a one-character code as in SAP (ZLSCH). A
// supplier's company data lists the methods it may be paid by, in order.
type PaymentMethod struct {
	Code        string        `yaml:"code" json:"code"`
	Description string        `yaml:"description" json:"description"`
	Ranking     []BankRanking `yaml:"ranking" json:"ranking"`
}

// BankRanking is, for payments in a currency, the house banks that pay
// with a method, the first one preferred (SAP T042I).
type BankRanking struct {
	Currency   string   `yaml:"currency" json:"currency"`
	HouseBanks []string `yaml:"house_banks" json:"house_banks"`
}

type PaymentTerm struct {
	Key         string `yaml:"key" json:"key"`
	Description string `yaml:"description" json:"description"`
	NetDays     int    `yaml:"net_days" json:"net_days"`
	// Discounts are the cash discount tiers, earliest first: at most two,
	// as SAP's T052 has (T36a).
	Discounts []Discount `yaml:"discounts,omitempty" json:"discounts,omitempty"`
}

// Discount is a cash discount tier: Percent off for payment within Days
// of the baseline date. Percent is a plain decimal of at most three
// decimals, as SAP keeps it.
type Discount struct {
	Days    int    `yaml:"days" json:"days"`
	Percent string `yaml:"percent" json:"percent"`
}

type Purchasing struct {
	// RequisitionApproval routes requisitions by total value. Tiers are
	// ordered; the first tier whose up_to covers the amount applies, and the
	// last tier has no up_to.
	RequisitionApproval []ApprovalTier `yaml:"requisition_approval" json:"requisition_approval"`
	// OrderReleaseApproval routes the release of purchase orders without
	// an approved requisition, and changes that raise a released order's
	// total, by the order's total, as RequisitionApproval does (T47c).
	OrderReleaseApproval []ApprovalTier `yaml:"order_release_approval" json:"order_release_approval"`
	// ContractExpiryNoticeDays is how many days before an active contract
	// ends, without a renewal, ContractExpiring is published (T47g).
	ContractExpiryNoticeDays int `yaml:"contract_expiry_notice_days" json:"contract_expiry_notice_days"`
}

type ApprovalTier struct {
	UpTo  string         `yaml:"up_to,omitempty" json:"up_to,omitempty"`
	Steps []ApprovalStep `yaml:"steps" json:"steps"`
}

type ApprovalStep struct {
	Role      string `yaml:"role" json:"role"`
	Approvals int    `yaml:"approvals" json:"approvals"`
}

type Payments struct {
	ReleaseRole        string `yaml:"release_role" json:"release_role"`
	ReleaseApprovals   int    `yaml:"release_approvals" json:"release_approvals"`
	BankChangeHoldDays int    `yaml:"bank_change_hold_days" json:"bank_change_hold_days"`
	VendorApproverRole string `yaml:"vendor_approver_role" json:"vendor_approver_role"`
}

type Output struct {
	PurchaseOrderChannel string `yaml:"purchase_order_channel" json:"purchase_order_channel"` // email | edi | print
}

// Parse reads a configuration strictly: unknown fields are errors, so a
// typo never silently falls back to a default, and a fraction in an
// integer field is refused rather than truncated.
func Parse(b []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	if err := notWhole(b); err != nil {
		return nil, err
	}
	return &c, nil
}

// ParseActive reads an activated document, tolerating keys this binary
// does not know: during a rolling upgrade a newer replica may have
// activated a document with keys added after this binary was built (06
// §6.4). They are ignored and returned, one finding per key, for the
// caller to log. Submission and validation use Parse, which refuses them.
func ParseActive(b []byte) (*Config, []string, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, nil, fmt.Errorf("configuration: %w", err)
	}
	var unknown []string
	var te *yaml.TypeError
	if _, err := Parse(b); errors.As(err, &te) { // only unknown keys remain: the document read without them
		unknown = te.Errors
	}
	return &c, unknown, nil
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("configuration: read %s: %w", path, err)
	}
	return Parse(b)
}

// Marshal renders a configuration as YAML.
func (c *Config) Marshal() ([]byte, error) {
	// Config holds only strings, numbers, bools, slices, string-keyed maps,
	// structs and the decoded YAML values of rule cases, and the buffer
	// never fails, so encoding cannot fail; the error result is kept for API
	// stability.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(c)
	_ = enc.Close()
	return buf.Bytes(), nil
}

// ServiceLimit is the limit a service principal's journal entries are
// measured against (T61n): ServicePostingLimit, or, for a company without
// one, ManualEntryApprovalLimit, so integrations post what they posted
// before the limit existed. "" when the company has neither, which the
// ledger reads as 0, as approvals reads a missing manual limit: every
// entry is then parked, never posted unchecked.
func (co *Company) ServiceLimit() string {
	if co.ServicePostingLimit != "" { // validated as given: no space around it
		return co.ServicePostingLimit
	}
	return strings.TrimSpace(co.ManualEntryApprovalLimit)
}

func (c *Config) Company(code string) *Company {
	for i := range c.Companies {
		if c.Companies[i].Code == code {
			return &c.Companies[i]
		}
	}
	return nil
}

// zoneName is the IANA form of a zone name, every part starting with a
// capital (America/Toronto, Etc/GMT+5, UTC).
var zoneName = regexp.MustCompile(`^[A-Z][A-Za-z0-9_+-]*(/[A-Z][A-Za-z0-9_+-]*)*$`)

// Location is the company's time zone; UTC when none is set, as in a
// document activated before validation required one (tolerant read,
// strict write: 06 §6.4). Validation refuses a name that does not load, and the names only some hosts accept
// (LoadLocation tries the host's zoneinfo before the embedded copy): Local,
// the reading host's zone, and any name not in IANA form, so localtime,
// posixrules, the posix/ and right/ trees and a lower-case name a
// case-insensitive filesystem would open are refused too.
func (c *Company) Location() (*time.Location, error) {
	switch {
	case c.TimeZone == "":
		return time.UTC, nil
	case c.TimeZone == "Local", !zoneName.MatchString(c.TimeZone):
		return nil, fmt.Errorf("time zone %q is not a standard IANA zone name every host has", c.TimeZone)
	}
	loc, err := time.LoadLocation(c.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("time zone %q is not an IANA zone name: %w", c.TimeZone, err)
	}
	return loc, nil
}

// CompanyLocation is the time zone of the company with the code, which its
// business dates are in: UTC when the document has no such company, or the
// company sets none (a document activated before the zone was required,
// T72n7). Every business date judged for a company is its date in this zone:
// the cash position's read date (T72n1), a payment run's discount lapse
// (T72n3) and a requisition's date (T72n5).
func (c *Config) CompanyLocation(code string) (*time.Location, error) {
	co := c.Company(code)
	if co == nil {
		return time.UTC, nil
	}
	return co.Location()
}

func (c *Company) Account(number string) *Account {
	for i := range c.ChartOfAccounts {
		if c.ChartOfAccounts[i].Number == number {
			return &c.ChartOfAccounts[i]
		}
	}
	return nil
}

func (c *Config) PaymentTerm(key string) *PaymentTerm {
	for i := range c.PaymentTerms {
		if c.PaymentTerms[i].Key == key {
			return &c.PaymentTerms[i]
		}
	}
	return nil
}
