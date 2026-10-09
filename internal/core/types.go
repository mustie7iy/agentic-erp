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
	PaymentTerms     string `json:"paymentTerms,omitempty"`
	// BankChangedAt is the date of the last approved bank change; payments
	// are held for a configured number of days after it.
	BankChangedAt string `json:"bankChangedAt,omitempty"`
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
	// Set for purchase orders created in the platform (not migrated).
	RequisitionID string `json:"requisitionId,omitempty"`
	ContractID    string `json:"contractId,omitempty"`
	CreatedBy     string `json:"createdBy,omitempty"`
}

type GoodsReceipt struct {
	ID          string    `json:"id"`
	POID        string    `json:"poId"`
	POLineNo    int       `json:"poLineNo"`
	Quantity    money.Qty `json:"quantity"`
	PostingDate string    `json:"postingDate"`
	// MovementType follows SAP: 101 receipt, 102 reversal (negative quantity).
	MovementType string `json:"movementType,omitempty"`
	PostedBy     string `json:"postedBy,omitempty"`
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

type RequisitionStatus string

const (
	ReqPending   RequisitionStatus = "pending_approval"
	ReqApproved  RequisitionStatus = "approved"
	ReqRejected  RequisitionStatus = "rejected"
	ReqCancelled RequisitionStatus = "cancelled"
	ReqOrdered   RequisitionStatus = "ordered"
)

type RequisitionLine struct {
	LineNo      int         `json:"lineNo"`
	Material    string      `json:"material,omitempty"`
	Description string      `json:"description"`
	Quantity    money.Qty   `json:"quantity"`
	UnitPrice   money.Minor `json:"unitPrice"` // estimate; contract prices win
	GLAccount   string      `json:"glAccount"`
}

// Requisition is a request to buy, approved before it becomes a PO (SAP EBAN).
type Requisition struct {
	ID          string            `json:"id"`
	CompanyCode string            `json:"companyCode"`
	Requester   string            `json:"requester"`
	VendorID    string            `json:"vendorId,omitempty"`
	ContractID  string            `json:"contractId,omitempty"`
	Currency    string            `json:"currency"`
	NeededBy    string            `json:"neededBy,omitempty"`
	Lines       []RequisitionLine `json:"lines"`
	Status      RequisitionStatus `json:"status"`
	WorkflowID  string            `json:"workflowId"`
	POID        string            `json:"poId,omitempty"`
	CreatedAt   string            `json:"createdAt"`
}

// Total is the estimated value of the requisition.
func (r *Requisition) Total() money.Minor {
	var t money.Minor
	for _, l := range r.Lines {
		t += money.Extend(l.Quantity, l.UnitPrice, 1)
	}
	return t
}

type ContractItem struct {
	Material    string      `json:"material"`
	Description string      `json:"description"`
	UnitPrice   money.Minor `json:"unitPrice"`
	PriceUnit   int64       `json:"priceUnit"`
	GLAccount   string      `json:"glAccount"`
}

// Contract is an outline agreement with agreed prices and a target value (SAP ME31K).
type Contract struct {
	ID            string         `json:"id"`
	CompanyCode   string         `json:"companyCode"`
	VendorID      string         `json:"vendorId"`
	Currency      string         `json:"currency"`
	ValidFrom     string         `json:"validFrom"`
	ValidTo       string         `json:"validTo"`
	TargetValue   money.Minor    `json:"targetValue"`
	ReleasedValue money.Minor    `json:"releasedValue"`
	Items         []ContractItem `json:"items"`
	CreatedBy     string         `json:"createdBy"`
}

type ChangeKind string

const (
	VendorCreate     ChangeKind = "vendor_create"
	VendorBankChange ChangeKind = "vendor_bank_change"
	VendorUnblock    ChangeKind = "vendor_unblock"
)

type ChangeStatus string

const (
	ChangePending  ChangeStatus = "pending"
	ChangeApplied  ChangeStatus = "applied"
	ChangeRejected ChangeStatus = "rejected"
)

// MasterDataChange is a governed change to supplier master data (SAP MDG):
// it takes effect only after a different person approves it.
type MasterDataChange struct {
	ID             string       `json:"id"`
	Kind           ChangeKind   `json:"kind"`
	VendorID       string       `json:"vendorId"`
	Proposed       *Vendor      `json:"proposed,omitempty"`
	NewBankAccount string       `json:"newBankAccount,omitempty"`
	Reason         string       `json:"reason,omitempty"`
	Warnings       []string     `json:"warnings,omitempty"`
	Requester      string       `json:"requester"`
	WorkflowID     string       `json:"workflowId"`
	Status         ChangeStatus `json:"status"`
	CreatedAt      string       `json:"createdAt"`
	AppliedAt      string       `json:"appliedAt,omitempty"`
}

type RunStatus string

const (
	RunProposed RunStatus = "proposed"
	RunReleased RunStatus = "released"
	RunRejected RunStatus = "rejected"
)

type PaidItem struct {
	JournalEntryID string      `json:"journalEntryId"`
	Line           int         `json:"line"`
	Reference      string      `json:"reference,omitempty"`
	DueDate        string      `json:"dueDate"`
	Amount         money.Minor `json:"amount"` // positive: what is paid
}

type VendorPayment struct {
	VendorID    string      `json:"vendorId"`
	VendorName  string      `json:"vendorName"`
	BankAccount string      `json:"bankAccount"`
	Items       []PaidItem  `json:"items"`
	Total       money.Minor `json:"total"`
}

type Exclusion struct {
	VendorID string      `json:"vendorId"`
	Reason   string      `json:"reason"`
	Amount   money.Minor `json:"amount"`
}

// PaymentRun is a proposal of what to pay, released by a different person
// before anything is posted (SAP F110 with bank approval).
type PaymentRun struct {
	ID              string          `json:"id"`
	CompanyCode     string          `json:"companyCode"`
	RunDate         string          `json:"runDate"`
	ProposedBy      string          `json:"proposedBy"`
	Status          RunStatus       `json:"status"`
	Payments        []VendorPayment `json:"payments"`
	Excluded        []Exclusion     `json:"excluded"`
	Total           money.Minor     `json:"total"`
	WorkflowID      string          `json:"workflowId"`
	JournalEntryIDs []string        `json:"journalEntryIds,omitempty"`
	BankFile        string          `json:"bankFile,omitempty"`
	ReleasedAt      string          `json:"releasedAt,omitempty"`
}

// OutputDocument is a message sent to a business partner (SAP output management).
type OutputDocument struct {
	ID        string `json:"id"`
	Type      string `json:"type"` // purchase_order
	Reference string `json:"reference"`
	Channel   string `json:"channel"`
	Recipient string `json:"recipient"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}
