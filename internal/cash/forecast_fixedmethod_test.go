package cash_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
)

// fix has the open item of the entry fix the payment method, as its
// posting would (ledger.open_item_payment_method).
func (e forecastEnv) fix(t *testing.T, entry uuid.UUID, method string) {
	t.Helper()
	e.exec(t, `INSERT INTO ledger.open_item_payment_method (tenant_id, fiscal_year, journal_entry_id, line_no, payment_method) VALUES ($1, 2026, $2, 1, $3)`,
		e.Tenant.ID, entry, method)
}

// TestLiquidityForecastFixedMethod: the forecast routes and nets a
// supplier's items that fix a payment method as a run does (ADR-0038;
// product owner 2026-11-10): by that method alone, on the bank ranked for
// it, and apart from the supplier's items that fix none, so a credit memo
// fixing a method does not lower what the run order's payment is expected
// to pay, and a group left in debit pays nothing. A method its supplier's
// own methods lack routes the item all the same, as a run pays it (the
// item's method wins; product owner 2026-11-10). An item whose fixed
// method cannot pay it (no bank is ranked for it) has no bank and no
// method, as an item no ranked bank pays.
func TestLiquidityForecastFixedMethod(t *testing.T) {
	e := setupForecast(t, withForecast)
	apart, alone := e.NewSupplier(t, "BP90000021"), e.NewSupplier(t, "BP90000022")
	e.exec(t, `UPDATE partner.supplier_company SET payment_methods = '{T}', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND partner_id = ANY($3)`, e.Tenant.ID, e.le, []uuid.UUID{apart, alone})
	e.exec(t, `INSERT INTO payments.payment_method (tenant_id, legal_entity_id, code, description, is_active, created_by, updated_by)
		VALUES ($1, $2, 'W', 'Wire', true, $3, $3), ($1, $2, 'U', 'Unranked', true, $3, $3)`, e.Tenant.ID, e.le, e.Tenant.Human.ID)
	e.exec(t, `INSERT INTO payments.bank_ranking (tenant_id, legal_entity_id, method, currency, house_banks, created_by, updated_by)
		VALUES ($1, $2, 'W', 'CAD', '{MAIN}', $3, $3)`, e.Tenant.ID, e.le, e.Tenant.Human.ID)
	// The invoice goes by the run order; the credit memo fixes T and is
	// alone in its group, a debit balance no run pays.
	e.item(t, apart, "2026-03-01", "CAD", -100000, nil)
	e.fix(t, e.item(t, apart, "2026-04-15", "CAD", 30000, nil), "T")
	// An invoice fixing a method its supplier's own methods lack, routed
	// by it all the same, and one fixing a method no bank is ranked for.
	e.fix(t, e.item(t, alone, "2026-03-01", "CAD", -50000, nil), "W")
	e.fix(t, e.item(t, alone, "2026-03-01", "CAD", -20000, nil), "U")

	var payables []cash.Payable
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		payables, err = app.Cash(app.Config{Now: func() time.Time { return readAt }}).Payments.Payables(ctx, e.le, readAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var routed []string
	for _, p := range payables {
		if p.Supplier == apart || p.Supplier == alone {
			routed = append(routed, fmt.Sprintf("fixes %q: bank %q method %q", p.FixedMethod, p.BankCode, p.Method))
		}
	}
	slices.Sort(routed)
	want(t, "routing", routed, []string{`fixes "": bank "MAIN" method "T"`, `fixes "T": bank "MAIN" method "T"`, `fixes "U": bank "" method ""`,
		`fixes "W": bank "MAIN" method "W"`})
	want(t, "flows", flowsOf(t, e.forecastOf(t, "")), []string{
		// 1,000.00 by the run order, the credit memo fixing T not netted
		// with it, 500.00 by the fixed method W and 200.00 of the item
		// whose method cannot pay it.
		"CAD day 2026-04-21..2026-04-21: payable/expected/late=false -1700.00 CAD x3 = -1700.00 CAD",
		"CAD total -1700.00 CAD, beyond 0.00 CAD x0, blocked 0.00 CAD x0", noUSD,
	})
}
