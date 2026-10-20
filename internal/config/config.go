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

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version      int           `yaml:"version" json:"version"`
	Companies    []Company     `yaml:"companies" json:"companies"`
	PaymentTerms []PaymentTerm `yaml:"payment_terms" json:"payment_terms"`
	Purchasing   Purchasing    `yaml:"purchasing" json:"purchasing"`
	Payments     Payments      `yaml:"payments" json:"payments"`
	Output       Output        `yaml:"output" json:"output"`
	NumberRanges []NumberRange `yaml:"number_ranges,omitempty" json:"number_ranges,omitempty"`
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
	Code                 string               `yaml:"code" json:"code"`
	Name                 string               `yaml:"name" json:"name"`
	Currency             string               `yaml:"currency" json:"currency"`
	Country              string               `yaml:"country" json:"country"`
	FiscalYearStartMonth int                  `yaml:"fiscal_year_start_month" json:"fiscal_year_start_month"`
	ChartOfAccounts      []Account            `yaml:"chart_of_accounts" json:"chart_of_accounts"`
	AccountDetermination AccountDetermination `yaml:"account_determination" json:"account_determination"`
	TaxCodes             []TaxCode            `yaml:"tax_codes" json:"tax_codes"`
	// WithholdingTaxCodes are the withholding taxes deducted from suppliers
	// (SAP extended withholding tax, T059P/T059Z; T46e).
	WithholdingTaxCodes []WithholdingTaxCode `yaml:"withholding_tax_codes,omitempty" json:"withholding_tax_codes,omitempty"`
	InvoiceTolerance    Tolerance            `yaml:"invoice_tolerance" json:"invoice_tolerance"`
	AutoPostLimit       string               `yaml:"auto_post_limit" json:"auto_post_limit"`
	// ManualEntryApprovalLimit is the largest manual journal entry, by its
	// debits in the company's currency, that people post without approval
	// (T35); above it the controller approves the entry first.
	ManualEntryApprovalLimit string    `yaml:"manual_entry_approval_limit" json:"manual_entry_approval_limit"`
	OverDeliveryPercent      int       `yaml:"gr_over_delivery_percent" json:"gr_over_delivery_percent"`
	HouseBank                HouseBank `yaml:"house_bank" json:"house_bank"`
	// HouseBanks are the company's further house banks (SAP T012K), each
	// with its own ledger account (ADR-0038).
	HouseBanks []HouseBank `yaml:"house_banks,omitempty" json:"house_banks,omitempty"`
	// PaymentMethods are the ways the company pays suppliers (SAP T042Z),
	// each ranking per currency the house banks that pay with it (SAP
	// T042E/T042I; ADR-0038).
	PaymentMethods []PaymentMethod `yaml:"payment_methods,omitempty" json:"payment_methods,omitempty"`
	// BankStatements says how the company's bank statements are matched
	// and posted (SAP FF_5/FEBAN, T028G; ADR-0047).
	BankStatements BankStatements `yaml:"bank_statements,omitempty" json:"bank_statements,omitzero"`
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
	// OpenItems manages the account's lines as open items that a later
	// posting clears (SAP SKB1-XOPVW), as bank clearing accounts must be
	// (ADR-0047). It is turned on only for an account without postings and
	// never turned off.
	OpenItems bool `yaml:"open_items,omitempty" json:"open_items,omitempty"`
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

type TaxCode struct {
	Code        string `yaml:"code" json:"code"`
	Description string `yaml:"description" json:"description"`
	RatePercent string `yaml:"rate_percent" json:"rate_percent"`
	Account     string `yaml:"account" json:"account"`
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

// Tolerance bounds the price variance on an invoice line (SAP T169G key PP).
type Tolerance struct {
	PricePercent  int    `yaml:"price_percent" json:"price_percent"`
	PriceAbsolute string `yaml:"price_absolute" json:"price_absolute"`
}

type HouseBank struct {
	Name string `yaml:"name" json:"name"`
	IBAN string `yaml:"iban" json:"iban"`
	BIC  string `yaml:"bic" json:"bic"`
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
}

// AllHouseBanks is the main house bank followed by the further ones.
func (c *Company) AllHouseBanks() []HouseBank {
	return append([]HouseBank{c.HouseBank}, c.HouseBanks...)
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
	// Config holds only strings, numbers, bools, slices and structs, and the
	// buffer never fails, so encoding cannot fail; the error result is kept
	// for API stability.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(c)
	_ = enc.Close()
	return buf.Bytes(), nil
}

func (c *Config) Company(code string) *Company {
	for i := range c.Companies {
		if c.Companies[i].Code == code {
			return &c.Companies[i]
		}
	}
	return nil
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
