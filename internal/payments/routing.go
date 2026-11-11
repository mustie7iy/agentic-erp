package payments

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/partner"
)

// via is the house bank and payment method a payment goes through: both
// nil in a run proposed for one house bank, whose bank pays every payment.
// fixed says its items fix the method (router.fixed), rather than the
// company's run order choosing it.
type via struct {
	bank   *uuid.UUID
	method *string
	fixed  bool
}

// router chooses how each supplier of a run is paid (ADR-0038, SAP F110's
// bank selection): in a run proposed for one house bank, through it; in one
// proposed without, through the first of the company's payment methods, in
// the company's run order, that the supplier may be paid by (its company
// data, which only restricts the order: SAP specialist 2026-11-09, A4(c))
// and whose ranking for the run currency has an active house bank in that
// currency, that bank, the first the ranking names; a ranked bank whose
// account is in another currency is skipped (product owner 2026-10-19).
// A supplier's items that fix a payment method go by that method alone
// (fixed; product owner 2026-11-10). The zero router routes per run.
type router struct {
	byMethod bool
	methods  []string             // the methods a bank can pay with in the run currency, in the company's run order
	banks    map[string]uuid.UUID // method → its ranking's first active house bank in the run currency
	active   map[string]bool      // the company's active methods, ranked or not: what an item may fix
}

// routerFor is the run's router: per run when it has a house bank, else
// from the legal entity's active methods and rankings in the run currency.
func routerFor(ctx context.Context, tx pgx.Tx, r Run) (router, error) {
	if r.HouseBankID != nil {
		return router{}, nil
	}
	return rankedRouter(ctx, tx, r.LegalEntityID, r.Currency)
}

// rankedRouter routes by the legal entity's active methods, in their run
// order, and their rankings in the currency: a run proposed without a
// house bank, and the liquidity forecast's choice of the bank that will
// pay an item (CashPosition.Payables). A method without a place in the
// order (written by a replica of the release before 000251) comes after
// those with one, by code, so the order is the same at every read. Every
// active method is read, with the bank that pays by it or none: an item
// that fixes a method the company no longer has is told apart from one
// whose method no bank is ranked for (fixed).
func rankedRouter(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, currency string) (router, error) {
	rows, _ := tx.Query(ctx, `SELECT m.code, (SELECT h.id FROM payments.bank_ranking r CROSS JOIN LATERAL unnest(r.house_banks) WITH ORDINALITY AS b(code, n)
				JOIN payments.house_bank h ON h.tenant_id = r.tenant_id AND h.legal_entity_id = r.legal_entity_id AND h.code = b.code AND h.status = 'active' AND h.currency = r.currency
				WHERE r.tenant_id = m.tenant_id AND r.legal_entity_id = m.legal_entity_id AND r.method = m.code AND r.currency = $2 AND r.is_active
				ORDER BY b.n LIMIT 1) AS bank
		FROM payments.payment_method m
		WHERE m.legal_entity_id = $1 AND m.is_active
		ORDER BY m.run_order NULLS LAST, m.code`, legalEntity, currency)
	rt := router{byMethod: true, banks: map[string]uuid.UUID{}, active: map[string]bool{}}
	var method string
	var bank *uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&method, &bank}, func() error {
		rt.active[method] = true
		if bank != nil {
			rt.methods, rt.banks[method] = append(rt.methods, method), *bank
		}
		return nil
	}); err != nil {
		return router{}, fmt.Errorf("payments: read the bank rankings of legal entity %s in %s: %w", legalEntity, currency, err)
	}
	return rt, nil
}

// route is how the payee is paid: by the first method of the company's
// run order that the payee may be paid by, or false when no house bank
// ranked for any of its methods can pay it (NoHouseBank).
func (rt router) route(p partner.Payee) (via, bool) {
	if !rt.byMethod {
		return via{}, true
	}
	for _, m := range rt.methods {
		if slices.Contains(p.PaymentMethods, m) {
			bank := rt.banks[m]
			return via{bank: &bank, method: &m}, true
		}
	}
	return via{}, false
}

// fixed is how the payee's items that fix a payment method are paid: by
// that method alone, through the bank ranked for it, or the reason they
// are left out, never another method (ADR-0038; product owner
// 2026-11-10). The method must be an active one of the company
// (MethodNotActive: it was when the document was entered), one the payee
// may be paid by (MethodNotAllowed) and one a ranked house bank pays with
// in the run currency (NoHouseBank). A run proposed for one house bank
// pays by no method, so it cannot honour one (FixedMethodInBankRun).
func (rt router) fixed(p partner.Payee, method string) (via, string) {
	bank, ranked := rt.banks[method]
	switch {
	case !rt.byMethod:
		return via{}, FixedMethodInBankRun
	case !rt.active[method]:
		return via{}, MethodNotActive
	case !slices.Contains(p.PaymentMethods, method):
		return via{}, MethodNotAllowed
	case !ranked:
		return via{}, NoHouseBank
	}
	return via{bank: &bank, method: &method, fixed: true}, ""
}

// items is how the payee's items that fix method, or none (""), are
// paid, or the reason they are left out: by the fixed method alone
// (fixed), else by the company's run order (route).
func (rt router) items(p partner.Payee, method string) (via, string) {
	if method != "" {
		return rt.fixed(p, method)
	}
	v, routed := rt.route(p)
	if !routed {
		return via{}, NoHouseBank
	}
	return v, ""
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
