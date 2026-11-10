package payments

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// PaymentMethods is the configuration document's payment methods (SAP
// T042Z) and, per method and currency, the house banks ranked to pay with
// it (SAP T042E/T042I; ADR-0038), kept per legal entity in
// payments.payment_method and payments.bank_ranking, each method with its
// place in the order a payment run tries them in (run_order, from
// config.Company.MethodsInRunOrder). A method or ranking
// the document no longer has becomes inactive, never deleted; the partner
// context refuses taking away a method a supplier has
// (partner.PaymentMethodCodes). The configuration's own validation keeps
// each ranking to house banks of the company, so HouseBanks, applied
// before, has them.
type PaymentMethods struct {
	LegalEntities interface {
		EntityByCode(ctx context.Context, code string) (tenancy.Entity, bool, error)
	}
}

// Check has nothing to check against the tenant's data: the document's
// validation covers the methods and rankings.
func (PaymentMethods) Check(context.Context, *config.Config, *config.Config) ([]config.Finding, error) {
	return nil, nil
}

// Apply makes each legal entity's payment methods and rankings the
// company's.
func (m PaymentMethods) Apply(ctx context.Context, doc *config.Config) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	for _, co := range doc.Companies {
		le, ok, err := m.LegalEntities.EntityByCode(ctx, co.Code)
		if err != nil {
			return fmt.Errorf("payments: legal entity %s: %w", co.Code, err)
		}
		if !ok { // checked on submission; legal entities are never deleted
			return fmt.Errorf("payments: no legal entity %s", co.Code)
		}
		if err := applyMethods(ctx, tx, p, le.ID, co.MethodsInRunOrder()); err != nil {
			return fmt.Errorf("payments: payment methods of legal entity %s: %w", co.Code, err)
		}
	}
	return nil
}

// applyMethods deactivates the methods and rankings not given, then
// inserts, updates or reactivates the given ones, which come in the
// company's run order: each takes its place in it, from 1.
func applyMethods(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le uuid.UUID, methods []config.PaymentMethod) error {
	var codes, descriptions, rankMethods, currencies, banks []string
	for _, pm := range methods {
		codes, descriptions = append(codes, pm.Code), append(descriptions, pm.Description)
		for _, r := range pm.Ranking {
			// The codes are 1-5 upper-case letters or digits, so a comma
			// separates them unambiguously.
			rankMethods, currencies, banks = append(rankMethods, pm.Code), append(currencies, r.Currency), append(banks, strings.Join(r.HouseBanks, ","))
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.bank_ranking r SET is_active = false, version = version + 1, updated_by = $4
		WHERE r.legal_entity_id = $1 AND r.is_active
			AND NOT EXISTS (SELECT FROM unnest($2::text[], $3::text[]) AS x(method, currency) WHERE x.method = r.method AND x.currency = r.currency)`,
		le, rankMethods, currencies, p.ID); err != nil {
		return fmt.Errorf("deactivate rankings: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payments.payment_method SET is_active = false, version = version + 1, updated_by = $3
		WHERE legal_entity_id = $1 AND is_active AND code <> ALL($2)`, le, codes, p.ID); err != nil {
		return fmt.Errorf("deactivate methods: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payments.payment_method AS m (tenant_id, legal_entity_id, code, description, run_order, created_by, updated_by)
		SELECT $1, $2, code, description, n, $3, $3 FROM unnest($4::text[], $5::text[]) WITH ORDINALITY AS x(code, description, n)
		ON CONFLICT (tenant_id, legal_entity_id, code) DO UPDATE SET description = EXCLUDED.description, run_order = EXCLUDED.run_order, is_active = true,
			version = m.version + 1, updated_by = $3
		WHERE (m.description, m.run_order, m.is_active) IS DISTINCT FROM (EXCLUDED.description, EXCLUDED.run_order, true)`,
		p.TenantID, le, p.ID, codes, descriptions); err != nil {
		return fmt.Errorf("write methods: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payments.bank_ranking AS r (tenant_id, legal_entity_id, method, currency, house_banks, created_by, updated_by)
		SELECT $1, $2, method, currency, string_to_array(banks, ','), $3, $3 FROM unnest($4::text[], $5::text[], $6::text[]) AS x(method, currency, banks)
		ON CONFLICT (tenant_id, legal_entity_id, method, currency) DO UPDATE SET house_banks = EXCLUDED.house_banks, is_active = true,
			version = r.version + 1, updated_by = $3
		WHERE (r.house_banks, r.is_active) IS DISTINCT FROM (EXCLUDED.house_banks, true)`,
		p.TenantID, le, p.ID, rankMethods, currencies, banks); err != nil {
		return fmt.Errorf("write rankings: %w", err)
	}
	return nil
}

// IsPaymentMethod reports whether the code is an active payment method of
// the legal entity (the partner context's check of a supplier's methods).
func (PaymentMethods) IsPaymentMethod(ctx context.Context, legalEntity uuid.UUID, code string) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM payments.payment_method WHERE legal_entity_id = $1 AND code = $2 AND is_active)`,
		legalEntity, code).Scan(&ok); err != nil {
		return false, fmt.Errorf("payments: payment method %s in %s: %w", code, legalEntity, err)
	}
	return ok, nil
}
