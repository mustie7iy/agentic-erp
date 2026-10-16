package partner

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Postings answers the ledger's question whether a partner may be posted
// to, in the request's transaction.
type Postings struct{}

// PostingBlock says why the partner may not be posted on a reconciliation
// account of the kind (supplier or customer) in the legal entity, or ""
// when it may: the partner must be active, play the role and, as a
// supplier, be set up and not blocked for posting in the legal entity.
func (Postings) PostingBlock(ctx context.Context, partnerID, legalEntity uuid.UUID, kind string) (string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return "", err
	}
	var status string
	var hasRole bool
	var blocked *bool
	err = tx.QueryRow(ctx, `SELECT p.status,
			EXISTS (SELECT FROM partner.partner_role r WHERE r.tenant_id = p.tenant_id AND r.partner_id = p.id AND r.role = $3),
			(SELECT sc.is_posting_blocked FROM partner.supplier_company sc WHERE sc.tenant_id = p.tenant_id AND sc.partner_id = p.id AND sc.legal_entity_id = $2)
		FROM partner.business_partner p WHERE p.id = $1`, partnerID, legalEntity, kind).Scan(&status, &hasRole, &blocked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "there is no business partner with this id", nil
	case err != nil:
		return "", fmt.Errorf("partner: read the posting status of business partner %s in legal entity %s: %w", partnerID, legalEntity, err)
	case status != Active:
		return "the business partner is " + status, nil
	case !hasRole:
		return "the business partner does not have the " + kind + " role", nil
	case kind == "supplier" && blocked == nil:
		return "the supplier is not set up in this legal entity", nil
	case kind == "supplier" && *blocked:
		return "the supplier is blocked for posting in this legal entity", nil
	}
	return "", nil
}

// PaymentTerms is the configuration document's payment_terms section,
// kept in partner.payment_term.
type PaymentTerms struct{}

// Check refuses removing a payment term: suppliers may use it, and terms
// have no lifecycle to retire them.
func (PaymentTerms) Check(_ context.Context, base, proposed *config.Config) ([]config.Finding, error) {
	if base == nil {
		return nil, nil
	}
	kept := map[string]bool{}
	for _, t := range proposed.PaymentTerms {
		kept[t.Key] = true
	}
	var out []config.Finding
	for _, t := range base.PaymentTerms {
		if !kept[t.Key] {
			out = append(out, config.Finding{Severity: config.SevError, Path: "payment_terms[" + t.Key + "]",
				Message: "a payment term cannot be removed once activated"})
		}
	}
	return out, nil
}

// Apply creates new payment terms and updates changed ones, with their
// cash discount tiers (validated in the document: at most two, later
// tiers smaller, all within the net days).
func (PaymentTerms) Apply(ctx context.Context, doc *config.Config) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	for _, t := range doc.PaymentTerms {
		if len(t.Discounts) > 2 {
			return fmt.Errorf("partner: apply payment term %s: %d discount tiers, the table holds two", t.Key, len(t.Discounts))
		}
		var days [2]*int
		var pct [2]*string
		for i, d := range t.Discounts {
			days[i], pct[i] = &d.Days, &d.Percent
		}
		if _, err := tx.Exec(ctx, `INSERT INTO partner.payment_term AS t (tenant_id, code, description, net_days, discount_days, discount_pct,
				discount2_days, discount2_pct, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8::numeric, $9, $9)
			ON CONFLICT (tenant_id, code) DO UPDATE SET description = EXCLUDED.description, net_days = EXCLUDED.net_days,
				discount_days = EXCLUDED.discount_days, discount_pct = EXCLUDED.discount_pct, discount2_days = EXCLUDED.discount2_days,
				discount2_pct = EXCLUDED.discount2_pct, version = t.version + 1, updated_by = EXCLUDED.updated_by
			WHERE (t.description, t.net_days, t.discount_days, t.discount_pct, t.discount2_days, t.discount2_pct)
				IS DISTINCT FROM (EXCLUDED.description, EXCLUDED.net_days, EXCLUDED.discount_days, EXCLUDED.discount_pct,
					EXCLUDED.discount2_days, EXCLUDED.discount2_pct)`,
			p.TenantID, t.Key, t.Description, t.NetDays, days[0], pct[0], days[1], pct[1], p.ID); err != nil {
			return fmt.Errorf("partner: apply payment term %s: %w", t.Key, err)
		}
	}
	return nil
}

// Suppliers answers procurement's and payables' questions about suppliers
// in the request's transaction.
type Suppliers struct{}

// Supplier is a business partner as a supplier of one legal entity.
type Supplier struct {
	ID               uuid.UUID
	Number           string
	Name             string
	CountryCode      string
	Status           string // business partner status
	IsSupplier       bool   // plays the supplier role
	SetUp            bool   // has supplier data in the legal entity
	ReconciliationID uuid.UUID
	PaymentTermCode  string
	IsPostingBlocked bool
	IsPaymentBlocked bool
}

// Usable reports why the supplier cannot be ordered from or invoiced in
// the legal entity, or "" when it can.
func (s Supplier) Usable() string {
	switch {
	case s.Status != Active:
		return "the business partner is " + s.Status
	case !s.IsSupplier:
		return "the business partner is not a supplier"
	case !s.SetUp:
		return "the supplier is not set up in this legal entity"
	case s.IsPostingBlocked:
		return "the supplier is blocked for posting in this legal entity"
	}
	return ""
}

// Supplier returns the partner as a supplier of the legal entity, or
// ok = false when there is no partner with the id.
func (Suppliers) Supplier(ctx context.Context, partnerID, legalEntity uuid.UUID) (Supplier, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Supplier{}, false, err
	}
	s := Supplier{ID: partnerID}
	var term *string
	var recon *uuid.UUID
	var posting, payment *bool
	err = tx.QueryRow(ctx, `SELECT p.number, p.name, p.country_code, p.status,
			EXISTS (SELECT FROM partner.partner_role r WHERE r.tenant_id = p.tenant_id AND r.partner_id = p.id AND r.role = 'supplier'),
			sc.reconciliation_account_id, sc.payment_term_code, sc.is_posting_blocked, sc.is_payment_blocked
		FROM partner.business_partner p
		LEFT JOIN partner.supplier_company sc ON sc.tenant_id = p.tenant_id AND sc.partner_id = p.id AND sc.legal_entity_id = $2
		WHERE p.id = $1`, partnerID, legalEntity).Scan(&s.Number, &s.Name, &s.CountryCode, &s.Status, &s.IsSupplier, &recon, &term, &posting, &payment)
	if errors.Is(err, pgx.ErrNoRows) {
		return Supplier{}, false, nil
	}
	if err != nil {
		return Supplier{}, false, fmt.Errorf("partner: read supplier %s of legal entity %s: %w", partnerID, legalEntity, err)
	}
	if term != nil {
		s.SetUp, s.ReconciliationID, s.PaymentTermCode, s.IsPostingBlocked, s.IsPaymentBlocked = true, *recon, *term, *posting, *payment
	}
	return s, true, nil
}

// PaymentTermExists reports whether the payment terms are configured.
func (Suppliers) PaymentTermExists(ctx context.Context, code string) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM partner.payment_term WHERE code = $1)`, code).Scan(&ok)
	if err != nil {
		return ok, fmt.Errorf("partner: find payment term %s: %w", code, err)
	}
	return ok, nil
}
