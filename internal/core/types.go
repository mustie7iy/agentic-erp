// Package core is the native system of record that replaces SAP functions
// one scope at a time. Everything here is deterministic: agents call into it,
// they never bypass it.
package core

import "github.com/mustie7iy/agentic-erp/internal/money"

type AccountType string

const (
	Asset     AccountType = "asset"
	Liability AccountType = "liability"
	Equity    AccountType = "equity"
	Revenue   AccountType = "revenue"
	Expense   AccountType = "expense"
)

type Company struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
}

type GLAccount struct {
	CompanyCode string      `json:"companyCode"`
	Number      string      `json:"number"`
	Name        string      `json:"name"`
	Type        AccountType `json:"type"`
	// VendorRecon marks an AP reconciliation account: every posting needs a vendor.
	VendorRecon bool `json:"vendorRecon,omitempty"`
}

type Vendor struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Country          string `json:"country,omitempty"`
	TaxID            string `json:"taxId,omitempty"`
	BankAccount      string `json:"bankAccount,omitempty"`
	PaymentTermsDays int    `json:"paymentTermsDays"`
	// APAccount is the reconciliation account per company code.
	APAccount map[string]string `json:"apAccount"`
	Blocked   bool              `json:"blocked,omitempty"`
}

type POLine struct {
	LineNo      int         `json:"lineNo"`
	Material    string      `json:"material,omitempty"`
	Description string      `json:"description"`
	Quantity    money.Qty   `json:"quantity"`
	UnitPrice   money.Minor `json:"unitPrice"`
	// PriceUnit: UnitPrice applies per PriceUnit units (SAP PEINH).
	PriceUnit int64  `json:"priceUnit"`
	GLAccount string `json:"glAccount"`
}

type PurchaseOrder struct {
	ID          string   `json:"id"`
	CompanyCode string   `json:"companyCode"`
	VendorID    string   `json:"vendorId"`
	Currency    string   `json:"currency"`
	OrderDate   string   `json:"orderDate"`
	Lines       []POLine `json:"lines"`
}

type GoodsReceipt struct {
	ID          string    `json:"id"`
	POID        string    `json:"poId"`
	POLineNo    int       `json:"poLineNo"`
	Quantity    money.Qty `json:"quantity"`
	PostingDate string    `json:"postingDate"`
}

type InvoiceLine struct {
	// POLineNo is 0 when the line does not reference a PO item.
	POLineNo    int         `json:"poLineNo,omitempty"`
	Description string      `json:"description,omitempty"`
	Quantity    money.Qty   `json:"quantity"`
	Amount      money.Minor `json:"amount"`
	GLAccount   string      `json:"glAccount,omitempty"`
}

type InvoiceStatus string

const (
	InvoiceParked   InvoiceStatus = "parked"
	InvoiceOnHold   InvoiceStatus = "on_hold"
	InvoicePosted   InvoiceStatus = "posted"
	InvoiceRejected InvoiceStatus = "rejected"
)

type SupplierInvoice struct {
	ID              string        `json:"id"`
	CompanyCode     string        `json:"companyCode"`
	VendorID        string        `json:"vendorId"`
	VendorReference string        `json:"vendorReference"`
	POID            string        `json:"poId,omitempty"`
	InvoiceDate     string        `json:"invoiceDate"`
	Currency        string        `json:"currency"`
	Lines           []InvoiceLine `json:"lines"`
	TaxAmount       money.Minor   `json:"taxAmount"`
	GrossAmount     money.Minor   `json:"grossAmount"`
	Status          InvoiceStatus `json:"status"`
	HoldReason      string        `json:"holdReason,omitempty"`
	JournalEntryID  string        `json:"journalEntryId,omitempty"`
	Source          string        `json:"source"`
}

type JournalLine struct {
	Account string `json:"account"`
	// Amount: positive = debit, negative = credit (SAP ACDOCA convention).
	Amount    money.Minor `json:"amount"`
	VendorID  string      `json:"vendorId,omitempty"`
	Text      string      `json:"text,omitempty"`
	ClearedBy string      `json:"clearedBy,omitempty"`
}

type JournalEntry struct {
	ID           string `json:"id"`
	CompanyCode  string `json:"companyCode"`
	PostingDate  string `json:"postingDate"`
	DocumentType string `json:"documentType"`
	Reference    string `json:"reference,omitempty"`
	// Source records provenance, e.g. "sap:1000/2026/5100000001" or "native:invoice:X".
	Source string        `json:"source"`
	Lines  []JournalLine `json:"lines"`
}

type TaskStatus string

const (
	TaskOpen     TaskStatus = "open"
	TaskApproved TaskStatus = "approved"
	TaskRejected TaskStatus = "rejected"
)

// ReviewTask is a decision handed to a human: agents propose, people approve.
type ReviewTask struct {
	ID             string     `json:"id"`
	InvoiceID      string     `json:"invoiceId"`
	Reason         string     `json:"reason"`
	Recommendation string     `json:"recommendation,omitempty"`
	CreatedBy      string     `json:"createdBy"`
	Status         TaskStatus `json:"status"`
}
