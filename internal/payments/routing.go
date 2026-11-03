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
// active house bank in that currency, that bank, the first the ranking
// names; a ranked bank whose account is in another currency is skipped
// (product owner 2026-10-19). The zero router routes per run.
type router struct {
	byMethod bool
	banks    map[string]uuid.UUID // method → its ranking's first active house bank in the run currency
}

// routerFor is the run's router: per run when it has a house bank, else
// from the legal entity's active methods and rankings in the run currency.
func routerFor(ctx context.Context, tx pgx.Tx, r Run) (router, error) {
	if r.HouseBankID != nil {
		return router{}, nil
	}
	return rankedRouter(ctx, tx, r.LegalEntityID, r.Currency)
}

// rankedRouter routes by the legal entity's active methods and their
// rankings in the currency: a run proposed without a house bank, and the
// liquidity forecast's choice of the bank that will pay an item
// (CashPosition.Payables).
func rankedRouter(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, currency string) (router, error) {
	rows, _ := tx.Query(ctx, `SELECT method, bank FROM (
			SELECT r.method, (SELECT h.id FROM unnest(r.house_banks) WITH ORDINALITY AS b(code, n)
				JOIN payments.house_bank h ON h.tenant_id = r.tenant_id AND h.legal_entity_id = r.legal_entity_id AND h.code = b.code AND h.status = 'active' AND h.currency = r.currency
				ORDER BY b.n LIMIT 1) AS bank
			FROM payments.bank_ranking r
			JOIN payments.payment_method m ON m.tenant_id = r.tenant_id AND m.legal_entity_id = r.legal_entity_id AND m.code = r.method AND m.is_active
			WHERE r.legal_entity_id = $1 AND r.currency = $2 AND r.is_active) ranked
		WHERE bank IS NOT NULL`, legalEntity, currency)
	rt := router{byMethod: true, banks: map[string]uuid.UUID{}}
	var method string
	var bank uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&method, &bank}, func() error { rt.banks[method] = bank; return nil }); err != nil {
		return router{}, fmt.Errorf("payments: read the bank rankings of legal entity %s in %s: %w", legalEntity, currency, err)
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

// bankOf is the house bank that pays payment p of run r: its own in a run
// proposed without one, else the run's; nil when neither names one.
func bankOf(r Run, p Payment) *uuid.UUID {
	if p.HouseBankID != nil {
		return p.HouseBankID
	}
	return r.HouseBankID
}

// payingBank is the house bank that pays payment p of run r: its own in a
// run proposed without one, else the run's. Both have foreign keys into
// payments.house_bank (000008, 000093), so the bank is found; a payment
// naming none in a run without one is broken data, an error.
func payingBank(ctx context.Context, tx pgx.Tx, r Run, p Payment) (houseBank, error) {
	id := bankOf(r, p)
	if id == nil {
		return houseBank{}, fmt.Errorf("payments: payment %s of run %s names no house bank, nor does its run", p.ID, r.ID)
	}
	hb, _, err := loadHouseBank(ctx, tx, *id)
	return hb, err
}
