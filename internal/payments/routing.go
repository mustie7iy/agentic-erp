package payments

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/partner"
)

// via is the house bank and payment method a payment goes through: both
// nil in a run proposed for one house bank, whose bank pays every payment.
type via struct {
	bank   *uuid.UUID
	method *string
}

// router chooses how each supplier of a run is paid (ADR-0038, SAP F110's
// bank selection): in a run proposed for one house bank, through it; in one
// proposed without, through the first of the supplier's payment methods
// (its company data, in order) whose ranking for the run currency has an
// active house bank, that bank, the first the ranking names. The zero
// router routes per run.
type router struct {
	byMethod bool
	banks    map[string]uuid.UUID // method → the first active house bank its ranking for the currency names
}

// routerFor is the run's router: per run when it has a house bank, else
// from the legal entity's active methods and rankings in the run currency.
func routerFor(ctx context.Context, tx pgx.Tx, r Run) (router, error) {
	if r.HouseBankID != nil {
		return router{}, nil
	}
	rows, _ := tx.Query(ctx, `SELECT method, bank FROM (
			SELECT r.method, (SELECT h.id FROM unnest(r.house_banks) WITH ORDINALITY AS b(code, n)
				JOIN payments.house_bank h ON h.tenant_id = r.tenant_id AND h.legal_entity_id = r.legal_entity_id AND h.code = b.code AND h.status = 'active'
				ORDER BY b.n LIMIT 1) AS bank
			FROM payments.bank_ranking r
			JOIN payments.payment_method m ON m.tenant_id = r.tenant_id AND m.legal_entity_id = r.legal_entity_id AND m.code = r.method AND m.is_active
			WHERE r.legal_entity_id = $1 AND r.currency = $2 AND r.is_active) ranked
		WHERE bank IS NOT NULL`, r.LegalEntityID, r.Currency)
	rt := router{byMethod: true, banks: map[string]uuid.UUID{}}
	var method string
	var bank uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&method, &bank}, func() error { rt.banks[method] = bank; return nil }); err != nil {
		return router{}, fmt.Errorf("payments: read the bank rankings of legal entity %s in %s: %w", r.LegalEntityID, r.Currency, err)
	}
	return rt, nil
}

// route is how the payee is paid, or false when no house bank ranked for
// any of its methods can pay it (NoHouseBank).
func (rt router) route(p partner.Payee) (via, bool) {
	if !rt.byMethod {
		return via{}, true
	}
	for _, m := range p.PaymentMethods {
		if bank, ok := rt.banks[m]; ok {
			return via{bank: &bank, method: &m}, true
		}
	}
	return via{}, false
}

// methodOf is a payment method, or "" for none.
func methodOf(m *string) string {
	if m == nil {
		return ""
	}
	return *m
}

// payingBank is the house bank that pays payment p of run r: its own in a
// run proposed without one, else the run's. Both have foreign keys into
// payments.house_bank (000008, 000092), so the bank is found; a payment
// naming none in a run without one is broken data, an error.
func payingBank(ctx context.Context, tx pgx.Tx, r Run, p Payment) (houseBank, error) {
	id := r.HouseBankID
	if p.HouseBankID != nil {
		id = p.HouseBankID
	}
	if id == nil {
		return houseBank{}, fmt.Errorf("payments: payment %s of run %s names no house bank, nor does its run", p.ID, r.ID)
	}
	hb, _, err := loadHouseBank(ctx, tx, *id)
	return hb, err
}
