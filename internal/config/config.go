// Package config is configuration as code: the settings SAP keeps in
// customizing (SPRO) live here as one typed YAML document. Changes are
// proposed, validated, simulated, approved by tier and activated as new
// versions with one-step rollback. Agents propose; people approve.
package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/mustie7iy/agentic-erp/internal/core"
)

type Config struct {
	Version      int           `yaml:"version" json:"version"`
	Companies    []Company     `yaml:"companies" json:"companies"`
	PaymentTerms []PaymentTerm `yaml:"payment_terms" json:"payment_terms"`
	Purchasing   Purchasing    `yaml:"purchasing" json:"purchasing"`
	Payments     Payments      `yaml:"payments" json:"payments"`
	Roles        []Role        `yaml:"roles" json:"roles"`
	Users        []User        `yaml:"users" json:"users"`
	Agents       []Agent       `yaml:"agents" json:"agents"`
	SoDConflicts [][2]string   `yaml:"sod_conflicts" json:"sod_conflicts"`
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
	InvoiceTolerance     Tolerance            `yaml:"invoice_tolerance" json:"invoice_tolerance"`
	AutoPostLimit        string               `yaml:"auto_post_limit" json:"auto_post_limit"`
	OverDeliveryPercent  int                  `yaml:"gr_over_delivery_percent" json:"gr_over_delivery_percent"`
	HouseBank            HouseBank            `yaml:"house_bank" json:"house_bank"`
}

type Account struct {
	Number               string           `yaml:"number" json:"number"`
	Name                 string           `yaml:"name" json:"name"`
	Type                 core.AccountType `yaml:"type" json:"type"`
	VendorReconciliation bool             `yaml:"vendor_reconciliation,omitempty" json:"vendor_reconciliation,omitempty"`
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
}

type TaxCode struct {
	Code        string `yaml:"code" json:"code"`
	Description string `yaml:"description" json:"description"`
	RatePercent string `yaml:"rate_percent" json:"rate_percent"`
	Account     string `yaml:"account" json:"account"`
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
}

type PaymentTerm struct {
	Key         string `yaml:"key" json:"key"`
	Description string `yaml:"description" json:"description"`
	NetDays     int    `yaml:"net_days" json:"net_days"`
}

type Purchasing struct {
	// RequisitionApproval routes requisitions by total value. Tiers are
	// ordered; the first tier whose up_to covers the amount applies, and the
	// last tier has no up_to.
	RequisitionApproval []ApprovalTier `yaml:"requisition_approval" json:"requisition_approval"`
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

type Role struct {
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Permissions []string `yaml:"permissions" json:"permissions"`
}

type User struct {
	ID        string   `yaml:"id" json:"id"`
	Name      string   `yaml:"name" json:"name"`
	Roles     []string `yaml:"roles" json:"roles"`
	Companies []string `yaml:"companies,omitempty" json:"companies,omitempty"`
}

type Agent struct {
	ID          string   `yaml:"id" json:"id"`
	Description string   `yaml:"description" json:"description"`
	Permissions []string `yaml:"permissions" json:"permissions"`
	Autonomy    int      `yaml:"autonomy" json:"autonomy"`
}

type Output struct {
	PurchaseOrderChannel string `yaml:"purchase_order_channel" json:"purchase_order_channel"` // email | edi | print
}

// Parse reads a configuration strictly: unknown fields are errors, so a
// typo never silently falls back to a default.
func Parse(b []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	return &c, nil
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
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

func (c *Config) Role(name string) *Role {
	for i := range c.Roles {
		if c.Roles[i].Name == name {
			return &c.Roles[i]
		}
	}
	return nil
}

func (c *Config) User(id string) *User {
	for i := range c.Users {
		if c.Users[i].ID == id {
			return &c.Users[i]
		}
	}
	return nil
}

func (c *Config) Agent(id string) *Agent {
	for i := range c.Agents {
		if c.Agents[i].ID == id {
			return &c.Agents[i]
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
