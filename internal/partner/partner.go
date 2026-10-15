// Package partner is the business partner context (03-data-model §3.6):
// suppliers and customers on the SAP business partner model, with company
// data per legal entity, effective-dated addresses and bank accounts, and
// encrypted tax identifiers. Master data changes are governed (SAP MDG):
// creating a partner, changing its data or bank details and unblocking it
// open a partner change that is applied only when its approval request is
// approved. Blocking needs no approval and applies at once.
//
// T3 scope: approval requests arrive with the workflow context (T4), which
// calls ApplyChange, RejectChange and CancelChange; until then changes stay
// pending.
package partner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Partner statuses.
const (
	Pending  = "pending"
	Active   = "active"
	Blocked  = "blocked"
	Inactive = "inactive"
)

// Partner change kinds and statuses.
const (
	KindCreate     = "create"
	KindUpdate     = "update"
	KindBankChange = "bank_change"
	KindBlock      = "block"
	KindUnblock    = "unblock"

	ChangePending   = "pending"
	ChangeApplied   = "applied"
	ChangeRejected  = "rejected"
	ChangeCancelled = "cancelled"
)

func init() {
	fieldcrypt.Register(fieldcrypt.Column{Table: "partner.bank_account", Keys: []string{"id"}, Cipher: "account_cipher", Fingerprint: "account_fingerprint"})
	fieldcrypt.Register(fieldcrypt.Column{Table: "partner.tax_identifier", Keys: []string{"partner_id", "scheme"}, Cipher: "value_cipher", Fingerprint: "value_fingerprint", Versioned: true})
	problem.RegisterConstraint("partner_change_one_pending", func() *problem.Problem {
		return problem.Conflict("partner.change_pending", "The partner already has a pending change of this kind; it must be decided first.")
	})
	problem.RegisterConstraint("address_tenant_id_partner_id_purpose_daterange_excl", func() *problem.Problem {
		return problem.Conflict("partner.address_overlap", "Another address of this purpose is valid in the same period.")
	})
	problem.RegisterConstraint("bank_account_tenant_id_partner_id_purpose_daterange_excl", func() *problem.Problem {
		return problem.Conflict("partner.bank_account_overlap", "Another bank account of this purpose is valid in the same period.")
	})
}

// LegalEntities is what the partner context reads about legal entities.
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// Accounts is what the partner context asks the ledger about accounts.
type Accounts interface {
	// IsReconciliationAccount reports whether the account reconciles the
	// subledger of the kind (supplier or customer) and can be posted in the
	// legal entity.
	IsReconciliationAccount(ctx context.Context, legalEntity, account uuid.UUID, kind string) (bool, error)
}

// Endpoints implements the partner part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	Accounts      Accounts
	Keys          *fieldcrypt.Keyring
	Approvals     Opener
}

// Ports are the other contexts and keys requests are checked and sealed with.
type Ports struct {
	LegalEntities LegalEntities
	Accounts      Accounts
	Keys          *fieldcrypt.Keyring
}

func (e *Endpoints) ports() Ports {
	return Ports{LegalEntities: e.LegalEntities, Accounts: e.Accounts, Keys: e.Keys}
}

// Date is a calendar date as PostgreSQL renders it in JSON.
type Date struct{ time.Time }

// UnmarshalJSON reads "2006-01-02".
func (d *Date) UnmarshalJSON(b []byte) error {
	t, err := time.Parse(`"2006-01-02"`, string(b))
	d.Time = t
	if err != nil {
		return fmt.Errorf("partner: parse date: %w", err) // the parse error quotes the input
	}
	return nil
}

// MarshalJSON writes "2006-01-02".
func (d Date) MarshalJSON() ([]byte, error) { return []byte(d.Format(`"2006-01-02"`)), nil }

// SupplierCompany is a row of partner.supplier_company.
type SupplierCompany struct {
	LegalEntityID           uuid.UUID `json:"legal_entity_id"`
	ReconciliationAccountID uuid.UUID `json:"reconciliation_account_id"`
	PaymentTermCode         string    `json:"payment_term_code"`
	IsPostingBlocked        bool      `json:"is_posting_blocked"`
	IsPaymentBlocked        bool      `json:"is_payment_blocked"`
}

// Address is a row of partner.address.
type Address struct {
	ID          uuid.UUID `json:"id"`
	Purpose     string    `json:"purpose"`
	Line1       string    `json:"line1"`
	Line2       *string   `json:"line2"`
	City        string    `json:"city"`
	PostalCode  *string   `json:"postal_code"`
	Region      *string   `json:"region"`
	CountryCode string    `json:"country_code"`
	ValidFrom   Date      `json:"valid_from"`
	ValidTo     *Date     `json:"valid_to"`
}

// TaxIdentifier is a tax identifier, masked.
type TaxIdentifier struct {
	Scheme     string `json:"scheme"`
	ValueLast4 string `json:"value_last4"`
}

// Partner is a row of partner.business_partner with its roles, company
// data, addresses and masked tax identifiers.
type Partner struct {
	ID                uuid.UUID
	Number            string
	Kind              string
	Name              string
	LegalName         *string
	CountryCode       string
	Language          *string
	Status            string
	Extensions        map[string]any
	Roles             []string
	SupplierCompanies []SupplierCompany
	Addresses         []Address
	TaxIdentifiers    []TaxIdentifier
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
}

const partnerSelect = `SELECT p.id, p.number, p.kind, p.name, p.legal_name, p.country_code, p.language, p.status, p.extensions,
		p.version, p.created_at, p.created_by, p.updated_at, p.updated_by,
		coalesce((SELECT array_agg(r.role ORDER BY r.role) FROM partner.partner_role r WHERE r.tenant_id = p.tenant_id AND r.partner_id = p.id), '{}'),
		coalesce((SELECT json_agg(json_build_object('legal_entity_id', s.legal_entity_id, 'reconciliation_account_id', s.reconciliation_account_id,
			'payment_term_code', s.payment_term_code, 'is_posting_blocked', s.is_posting_blocked, 'is_payment_blocked', s.is_payment_blocked)
			ORDER BY s.legal_entity_id) FROM partner.supplier_company s WHERE s.tenant_id = p.tenant_id AND s.partner_id = p.id), '[]'),
		coalesce((SELECT json_agg(json_build_object('id', a.id, 'purpose', a.purpose, 'line1', a.line1, 'line2', a.line2, 'city', a.city,
			'postal_code', a.postal_code, 'region', a.region, 'country_code', a.country_code, 'valid_from', a.valid_from, 'valid_to', a.valid_to)
			ORDER BY a.purpose, a.valid_from) FROM partner.address a WHERE a.tenant_id = p.tenant_id AND a.partner_id = p.id), '[]'),
		coalesce((SELECT json_agg(json_build_object('scheme', t.scheme, 'value_last4', t.value_last4) ORDER BY t.scheme)
			FROM partner.tax_identifier t WHERE t.tenant_id = p.tenant_id AND t.partner_id = p.id), '[]')
	FROM partner.business_partner p`

func scanPartner(row pgx.Row) (Partner, error) {
	var p Partner
	err := row.Scan(&p.ID, &p.Number, &p.Kind, &p.Name, &p.LegalName, &p.CountryCode, &p.Language, &p.Status, &p.Extensions,
		&p.Version, &p.CreatedAt, &p.CreatedBy, &p.UpdatedAt, &p.UpdatedBy, &p.Roles, &p.SupplierCompanies, &p.Addresses, &p.TaxIdentifiers)
	return p, errs.Wrapf(err, "scan business partner")
}

// visible keeps the company data of legal entities the caller may act in.
func visible(p Partner, access authz.Access) Partner {
	kept := []SupplierCompany{}
	for _, s := range p.SupplierCompanies {
		if access.Allows(s.LegalEntityID) {
			kept = append(kept, s)
		}
	}
	p.SupplierCompanies = kept
	return p
}

func notFound(what string) error { return problem.NotFound("No " + what + " with this id.") }

func getPartner(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Partner, error) {
	q := partnerSelect + ` WHERE p.id = $1`
	if lock {
		q += ` FOR UPDATE OF p`
	}
	p, err := scanPartner(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, notFound("business partner")
	}
	if err != nil {
		return p, fmt.Errorf("partner: read business partner %s: %w", id, err)
	}
	return p, nil
}

// Warning is a finding recorded on a partner change.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Change is a row of partner.partner_change. Payload holds the requested
// values with sensitive ones sealed; Masked hides them.
type Change struct {
	ID                uuid.UUID
	Number            string
	Kind              string
	PartnerID         uuid.UUID
	Payload           map[string]any
	Reason            string
	Warnings          []Warning
	Status            string
	WorkflowRequestID *uuid.UUID
	RequestedBy       uuid.UUID
	AppliedAt         *time.Time
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
}

const changeColumns = `id, number, kind, partner_id, payload, reason, warnings, status, workflow_request_id, requested_by, applied_at,
	version, created_at, created_by, updated_at, updated_by`

func scanChange(row pgx.Row) (Change, error) {
	var c Change
	err := row.Scan(&c.ID, &c.Number, &c.Kind, &c.PartnerID, &c.Payload, &c.Reason, &c.Warnings, &c.Status, &c.WorkflowRequestID,
		&c.RequestedBy, &c.AppliedAt, &c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy)
	return c, errs.Wrapf(err, "scan partner change")
}

func getChange(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Change, error) {
	q := `SELECT ` + changeColumns + ` FROM partner.partner_change WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	c, err := scanChange(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, notFound("partner change")
	}
	if err != nil {
		return c, fmt.Errorf("partner: read partner change %s: %w", id, err)
	}
	return c, nil
}

// Masked returns the payload without sealed values and fingerprints.
func (c Change) Masked() map[string]any {
	m, _ := mask(c.Payload).(map[string]any) // payloads are JSON objects
	return m
}

func mask(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if strings.HasSuffix(k, "_cipher") || strings.HasSuffix(k, "_fingerprint") {
				continue
			}
			out[k] = mask(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = mask(val)
		}
		return out
	}
	return v
}

// BankAccount is a row of partner.bank_account, masked.
type BankAccount struct {
	ID              uuid.UUID
	PartnerID       uuid.UUID
	Purpose         string
	AccountLast4    string
	AccountScheme   string
	BIC             *string
	BankCountryCode string
	HolderName      string
	ValidFrom       time.Time
	ValidTo         *time.Time
	PartnerChangeID uuid.UUID
	CreatedAt       time.Time
	CreatedBy       uuid.UUID
	EndedAt         *time.Time
	EndedBy         *uuid.UUID
}

const bankColumns = `id, partner_id, purpose, account_last4, account_scheme, bic, bank_country_code, holder_name, valid_from, valid_to,
	partner_change_id, created_at, created_by, ended_at, ended_by`

func scanBank(row pgx.Row) (BankAccount, error) {
	var b BankAccount
	err := row.Scan(&b.ID, &b.PartnerID, &b.Purpose, &b.AccountLast4, &b.AccountScheme, &b.BIC, &b.BankCountryCode, &b.HolderName,
		&b.ValidFrom, &b.ValidTo, &b.PartnerChangeID, &b.CreatedAt, &b.CreatedBy, &b.EndedAt, &b.EndedBy)
	return b, errs.Wrapf(err, "scan bank account")
}

func getBank(ctx context.Context, tx pgx.Tx, partnerID, id uuid.UUID) (BankAccount, error) {
	b, err := scanBank(tx.QueryRow(ctx, `SELECT `+bankColumns+` FROM partner.bank_account WHERE id = $1 AND partner_id = $2`, id, partnerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return b, notFound("bank account of this partner")
	}
	if err != nil {
		return b, fmt.Errorf("partner: read bank account %s of business partner %s: %w", id, partnerID, err)
	}
	return b, nil
}

// nextNumber issues the tenant's next number with the prefix (BP or PC):
// the prefix and eight digits. Numbering is serialized per tenant and table.
func nextNumber(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, table, prefix string) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "partner."+table+"."+tenant.String()); err != nil {
		return "", fmt.Errorf("partner: lock the %s numbering: %w", table, err)
	}
	var n string
	err := tx.QueryRow(ctx, `SELECT $1 || lpad((coalesce(substr(max(number), 3)::bigint, 0) + 1)::text, 8, '0')
		FROM `+pgx.Identifier{"partner", table}.Sanitize()+` WHERE number LIKE $1 || '%'`, prefix).Scan(&n)
	if err != nil {
		return n, fmt.Errorf("partner: issue the next %s number: %w", table, err)
	}
	return n, nil
}

func event(name string, aggregate uuid.UUID, aggregateType string, payload map[string]any) outbox.Event {
	return outbox.Event{Context: "partner", Name: name, AggregateType: aggregateType, AggregateID: aggregate, Payload: payload}
}

// toJSON turns a payload struct into the map stored on a change.
func toJSON(v any) map[string]any {
	b, _ := json.Marshal(v) // payload structs always encode
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}
