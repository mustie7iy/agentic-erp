package cash_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// withSources is withForecast with a withholding tax of 5% of the net
// deducted at payment (W5) in Maple Inc.
func withSources(doc *config.Config) {
	withForecast(doc)
	co := doc.Company("3000")
	co.ChartOfAccounts = append(co.ChartOfAccounts, config.Account{Number: "236000", Name: "Withholding tax payable", Type: config.Liability})
	co.WithholdingTaxCodes = []config.WithholdingTaxCode{{Code: "W5", Description: "Withholding 5%", RatePercent: "5", Account: "236000",
		Posting: config.WithholdingAtPayment}}
}

// invoice writes a supplier document of the kind in the status (parked
// or on_hold) straight into payables: gross and tax in the currency,
// dated day, on the order if one is given.
func (e forecastEnv) invoice(t *testing.T, supplier uuid.UUID, kind, status, day, currency string, gross, tax int64, order *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.exec(t, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date,
			currency, gross_amount, tax_amount, purchase_order_id, status, hold_reason, source, kind, created_by, updated_by)
		VALUES ($1, $2, $11, $3, $4, $11, $5, $6, $7, $8, $9, $10, CASE WHEN $10 = 'on_hold' THEN 'match' END, 'manual', $12, $13, $13)`,
		e.Tenant.ID, id, e.le, supplier, day, currency, gross, tax, order, status, id.String()[24:], kind, e.Tenant.Human.ID)
	return id
}

// order writes a draft purchase order of the supplier in CAD with the
// payment term.
func (e forecastEnv) order(t *testing.T, supplier uuid.UUID, term string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.exec(t, `SET session_replication_role = replica`) // a term that is not configured, too
	defer e.exec(t, `RESET session_replication_role`)
	e.exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
			payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $6, $3, $4, 'CAD', '2026-04-01', $5, 'draft', $7, $7)`,
		e.Tenant.ID, id, e.le, supplier, term, id.String()[24:], e.Tenant.Human.ID)
	return id
}

// request writes an open down payment request of the supplier in CAD.
func (e forecastEnv) request(t *testing.T, supplier uuid.UUID, amount int64, due string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.exec(t, `INSERT INTO payables.down_payment_request (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, amount, due_date,
			created_by, updated_by) VALUES ($1, $2, $7, $3, $4, 'CAD', $5, $6, $8, $8)`,
		e.Tenant.ID, id, e.le, supplier, amount, due, id.String()[24:], e.Tenant.Human.ID)
	return id
}

// sourcesScenario writes Maple Inc's documents not yet posted, its down
// payment requests and a blocked vendor's, read on Friday 17 April 2026
// (payment days Tuesday and Thursday, Monday the 20th a holiday):
//   - parked: an invoice of 1,000.00 of 10 April on an order under SK20,
//     2% until Thursday 30 April: 980.00 that day; and on hold, one of
//     500.00 of 1 April under its supplier's NT30, due Friday 1 May: paid
//     the Thursday before, netted with it into one payment of 1,480.00;
//   - a parked credit memo of 200.00 of another supplier: a debit balance,
//     no flow (on the order of the invoice of 300.00 below, which is read
//     once);
//   - a parked invoice of 1,050.00 (tax 50.00) of 8 April whose supplier
//     has W5: due Friday 8 May, paid on Thursday the 7th, less 5% of the
//     net 1,000.00: 1,000.00;
//   - a parked invoice of 300.00 of 15 April whose supplier has a code the
//     company does not know, on an order whose term is not configured:
//     neither refuses the forecast; its supplier's NT30 (Friday 15 May,
//     paid on Thursday the 14th) and no withholding;
//   - down payment requests of 400.00 due Wednesday 22 April, of 250.00
//     due 10 April (late, on the working day after the read date, Tuesday
//     the 21st, past the holiday), each alone; one of 700.00 a proposed
//     run pays, which counts as the run's payment, and one cancelled;
//   - the vendor, blocked for payment: its parked invoice of 600.00 of 1
//     April (NT30, Friday 1 May, planned) and request of 100.00 due 1
//     April (overdue: on the read date), and its posted item of 900.00 due
//     on 1 June 2027, after the last bucket: apart, in blocked;
//   - a parked invoice of 50.00 of 1 April of a supplier not set up in the
//     company: blocked, due on its date, so on the read date.
func (e forecastEnv) sourcesScenario(t *testing.T) {
	t.Helper()
	a, b, w, x, stranger := e.NewSupplier(t, "BP90000021"), e.NewSupplier(t, "BP90000022"), e.NewSupplier(t, "BP90000023"), e.NewSupplier(t, "BP90000024"),
		e.NewSupplier(t, "BP90000025")
	// A term of 2% within 20 days, net 30, as partner keeps it: in the
	// document it would need every company's cash discount account.
	e.exec(t, `INSERT INTO partner.payment_term (tenant_id, code, description, net_days, discount_days, discount_pct, created_by, updated_by)
		VALUES ($1, 'SK20', '2% 20, net 30', 30, 20, 2, $2, $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	sk20 := e.order(t, a, "SK20")
	e.invoice(t, a, "invoice", "parked", "2026-04-10", "CAD", 100000, 0, &sk20)
	e.invoice(t, a, "invoice", "on_hold", "2026-04-01", "CAD", 50000, 0, nil)
	nope := e.order(t, x, "NOPE")
	e.invoice(t, b, "credit_memo", "parked", "2026-04-10", "CAD", 20000, 0, &nope)
	e.exec(t, `INSERT INTO partner.supplier_withholding (tenant_id, partner_id, legal_entity_id, code, created_by, updated_by)
		VALUES ($1, $2, $3, 'W5', $5, $5), ($1, $4, $3, 'NOPE', $5, $5)`, e.Tenant.ID, w, e.le, x, e.Tenant.Human.ID)
	e.invoice(t, w, "invoice", "parked", "2026-04-08", "CAD", 105000, 5000, nil)
	e.invoice(t, x, "invoice", "parked", "2026-04-15", "CAD", 30000, 0, &nope)
	e.request(t, a, 40000, "2026-04-22")
	e.request(t, a, 25000, "2026-04-10")
	held := e.request(t, a, 70000, "2026-04-24")
	e.exec(t, `UPDATE payments.payment SET kind = 'down_payment', down_payment_request_id = $2 WHERE id = $1`,
		e.payment(t, e.run(t, e.main, "2026-04-23", "CAD", "proposed"), 70000, "CAD", nil), held)
	cancelled := e.request(t, a, 99900, "2026-04-22")
	e.exec(t, `UPDATE payables.down_payment_request SET status = 'cancelled', cancel_reason = 'no longer needed', version = version + 1 WHERE id = $1`, cancelled)
	e.invoice(t, e.Vendor, "invoice", "parked", "2026-04-01", "CAD", 60000, 0, nil)
	e.request(t, e.Vendor, 10000, "2026-04-01")
	e.item(t, e.Vendor, "2026-04-17", "CAD", -90000, &terms{baseline: "2026-06-01", net: 365})
	e.exec(t, `SET session_replication_role = replica`)
	defer e.exec(t, `RESET session_replication_role`)
	e.exec(t, `DELETE FROM partner.supplier_company WHERE partner_id = $1 AND legal_entity_id = $2`, stranger, e.le)
	e.invoice(t, stranger, "invoice", "parked", "2026-04-01", "CAD", 5000, 0, nil)
}

// TestLiquidityForecastSources: parked and held invoices are forecast as
// the items they would post (planned, netted among themselves), down
// payment requests on their due dates (expected, never netted), a run's
// request as the run's payment, and what blocked suppliers are owed apart
// on its due date, in the totals only when asked (ADR-0051 §4, §5, T53e2).
func TestLiquidityForecastSources(t *testing.T) {
	e := setupForecast(t, withSources)
	e.sourcesScenario(t)
	cad := []string{
		"CAD day 2026-04-17..2026-04-17: payable_blocked/expected/late=false -100.00 CAD x1, payable_blocked/planned/late=false -50.00 CAD x1 = 0.00 CAD",
		"CAD day 2026-04-21..2026-04-21: down_payment/expected/late=true -250.00 CAD x1 = -250.00 CAD",
		"CAD day 2026-04-22..2026-04-22: down_payment/expected/late=false -400.00 CAD x1 = -400.00 CAD",
		"CAD day 2026-04-23..2026-04-23: payment_run/confirmed/late=false -700.00 CAD x1 = -700.00 CAD",
		"CAD day 2026-04-30..2026-04-30: invoice_unposted/planned/late=false -1480.00 CAD x1 = -1480.00 CAD",
		"CAD week 2026-05-01..2026-05-07: invoice_unposted/planned/late=false -1000.00 CAD x1, payable_blocked/planned/late=false -600.00 CAD x1 = -1000.00 CAD",
		"CAD week 2026-05-08..2026-05-14: invoice_unposted/planned/late=false -300.00 CAD x1 = -300.00 CAD",
	}
	f := e.forecastOf(t, "")
	want(t, "flows", flowsOf(t, f), append(cad, "CAD total -4130.00 CAD, beyond 0.00 CAD x0, blocked -1650.00 CAD x4", noUSD))

	t.Run("with the blocked suppliers", func(t *testing.T) {
		f := e.forecastOf(t, "&include_blocked=true")
		want(t, "counted", f["include_blocked"], true)
		with := []string{
			"CAD day 2026-04-17..2026-04-17: payable_blocked/expected/late=false -100.00 CAD x1, payable_blocked/planned/late=false -50.00 CAD x1 = -150.00 CAD",
			cad[1], cad[2], cad[3], cad[4],
			"CAD week 2026-05-01..2026-05-07: invoice_unposted/planned/late=false -1000.00 CAD x1, payable_blocked/planned/late=false -600.00 CAD x1 = -1600.00 CAD",
			cad[6],
			"CAD total -4880.00 CAD, beyond -900.00 CAD x1, blocked -1650.00 CAD x4", noUSD,
		}
		want(t, "flows", flowsOf(t, f), with)
	})
	t.Run("the certainty cut", func(t *testing.T) {
		want(t, "expected and above", flowsOf(t, e.forecastOf(t, "&min_certainty=expected")), []string{
			"CAD day 2026-04-17..2026-04-17: payable_blocked/expected/late=false -100.00 CAD x1 = 0.00 CAD",
			cad[1], cad[2], cad[3],
			"CAD total -1350.00 CAD, beyond 0.00 CAD x0, blocked -1000.00 CAD x2", noUSD,
		})
	})
}

// TestLiquidityForecastUnpostedFailure: an invoice on an order the
// procurement context does not know fails the read rather than forecasting
// it by another term.
func TestLiquidityForecastUnpostedFailure(t *testing.T) {
	e := setupForecast(t, withForecast)
	missing := uuid.Must(uuid.NewV7())
	e.invoice(t, e.Supplier, "invoice", "parked", "2026-04-01", "CAD", 10000, 0, &missing)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := app.Cash(app.Config{Now: func() time.Time { return readAt }}).Forecast(ctx, cash.ForecastQuery{LegalEntity: e.le})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), missing.String()) {
		t.Errorf("an invoice on an unknown order: got %v, want the read failed naming the order", err)
	}
}

// TestLiquidityForecastSourcesFailures: the forecast fails with the ports
// that read the unposted invoices, their suppliers, orders, terms and
// withholding codes, and the down payment requests.
func TestLiquidityForecastSourcesFailures(t *testing.T) {
	e := setupForecast(t, withSources)
	e.sourcesScenario(t)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "forecast", func(ctx context.Context) error {
		_, err := app.Cash(app.Config{Now: func() time.Time { return readAt }}).Forecast(ctx, cash.ForecastQuery{LegalEntity: e.le})
		return err
	})
	ps := payables.Endpoints{}
	if _, err := ps.Unposted(context.Background(), e.le); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("unposted invoices outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
}
