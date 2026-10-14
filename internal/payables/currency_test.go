package payables_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The configured amounts of a company (the absolute price tolerance, the
// auto-post limit) are in its currency. An invoice in another currency is
// converted into it at the ledger's average rate on the invoice date before
// they are compared (F12). Company 2000 is in EUR: 1 JPY is 0.0062 EUR and
// 1 BHD is 2.4 EUR.

// foreign is a payables tenant that may record exchange rates.
func foreign(t *testing.T) env {
	t.Helper()
	f := fixture.New(t, append(perms, "ledger.rate.maintain", "config.regression.manage", "config.change.read")...)
	return env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
}

func (e env) rate(t *testing.T, from, to, rate string) {
	t.Helper()
	e.Must(201, "POST", "/v1/exchange-rates", fmt.Sprintf(`{"rate_type": "average", "from_currency": %q, "to_currency": %q,
		"valid_from": "2026-01-01", "rate": %q, "source": "ECB"}`, from, to, rate), "")
}

// orderIn inserts a released order in legal entity 2000 in the currency: 10
// EA of item 1 at the price (minor units), all of them received.
func (e env) orderIn(t *testing.T, currency string, price int64) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, '2026-03-01', 'NT30', 'released', $7, $7)`,
		e.Tenant.ID, id, "PO-"+currency+id.String()[30:], e.DE, e.Supplier, currency, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id) VALUES ($1, $2, 1, 'Paper', 10, 'EA', $3, 1, $4)`, e.Tenant.ID, id, price, e.expense)
	e.receive(t, id, 1, "10")
	return id
}

// foreignInvoice enters an invoice of 10 EA of the order's item 1 in the
// currency: the item amount, tax and gross as the API writes them.
func (e env) foreignInvoice(t *testing.T, po uuid.UUID, currency, ref, amount, tax, gross string) string {
	t.Helper()
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": "2026-03-10", "gross_amount": {"amount": %q, "currency": %q}, "tax_amount": {"amount": %q, "currency": %q},
		"items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "10", "unit": "EA"}, "amount": {"amount": %q, "currency": %q}}]}`,
		e.DE, e.Supplier, ref, gross, currency, tax, currency, po, amount, currency), "")
	return id(inv)
}

// matchCodes matches an invoice and returns its issue codes.
func (e env) matchCodes(t *testing.T, invoice string) string {
	t.Helper()
	m := e.act(t, 200, invoice, "match", "")
	var codes []string
	for _, is := range m["issues"].([]any) {
		codes = append(codes, is.(map[string]any)["code"].(string))
	}
	return strings.Join(codes, ",")
}

// TestToleranceInTheCompanysCurrency: the 5.00 EUR price tolerance applies
// to a JPY or BHD invoice at its EUR value. Before, it was read at the
// invoice's digits: 5 JPY, which flagged 500 JPY (3.10 EUR), and 5.000 BHD,
// which let 3.000 BHD (7.20 EUR) through. The 2% tolerance stays relative.
func TestToleranceInTheCompanysCurrency(t *testing.T) {
	e := foreign(t)
	e.rate(t, "JPY", "EUR", "0.0062")
	e.rate(t, "BHD", "EUR", "2.4")
	jpy := e.orderIn(t, "JPY", 1000)   // 10 EA at 1,000 JPY: 10,000 JPY, 2% is 200 JPY
	bhd := e.orderIn(t, "BHD", 10_000) // 10 EA at 10.000 BHD: 100.000 BHD, 2% is 2.000 BHD
	for i, c := range []struct {
		name           string
		po             uuid.UUID
		currency, none string // none is zero in the currency's digits
		amount, want   string
	}{
		{"JPY within 5.00 EUR", jpy, "JPY", "0", "10500", ""},                     // 500 JPY = 3.10 EUR
		{"JPY beyond 5.00 EUR", jpy, "JPY", "0", "10900", "price_variance"},       // 900 JPY = 5.58 EUR
		{"BHD within 5.00 EUR", bhd, "BHD", "0.000", "102.050", ""},               // 2.050 BHD = 4.92 EUR
		{"BHD beyond 5.00 EUR", bhd, "BHD", "0.000", "103.000", "price_variance"}, // 3.000 BHD = 7.20 EUR
	} {
		t.Run(c.name, func(t *testing.T) {
			e := e.in(t)
			inv := e.foreignInvoice(t, c.po, c.currency, fmt.Sprint("T-", i), c.amount, c.none, c.amount)
			if got := e.matchCodes(t, inv); got != c.want {
				t.Errorf("issues: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestNoRateRefusesTheMatch: without a rate from the invoice's currency to
// the company's, the match is refused rather than comparing the raw
// numbers.
func TestNoRateRefusesTheMatch(t *testing.T) {
	e := foreign(t)
	po := e.orderIn(t, "USD", 1200)
	inv := e.foreignInvoice(t, po, "USD", "US-1", "120.00", "0.00", "120.00")
	e.Problem(422, "ledger.no_exchange_rate", "POST", "/v1/supplier-invoices/"+inv+":match", "", e.tag(t, inv))
}

// day is the day the window of simulations and suites ends.
var day = time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)

func (e env) simulate(t *testing.T, base, proposed *config.Config) (configuration.Simulation, error) {
	t.Helper()
	var sim configuration.Simulation
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		sim, err = payables.Simulation{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Rates: rates,
			Now: func() time.Time { return day }}.Simulate(ctx, base, proposed)
		return err
	})
	return sim, err
}

func (e env) capture(t *testing.T, doc *config.Config) ([]configuration.RegressionCase, error) {
	t.Helper()
	var cases []configuration.RegressionCase
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		cases, err = payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Rates: rates,
			Now: func() time.Time { return day }}.Capture(ctx, doc, 90*24*time.Hour, 0)
		return err
	})
	return cases, err
}

func (e env) replay(t *testing.T, doc *config.Config, cases []configuration.RegressionCase) error {
	t.Helper()
	return apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := payables.Regression{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Rates: rates}.Replay(ctx, doc, cases)
		return err
	})
}

// TestSimulationInTheCompanysCurrency: replayed decisions compare a JPY
// invoice with the tolerance and the auto-post limit in EUR, and the
// amounts a posting rule moves are added up in EUR. Before, a proposed
// tolerance of 5.50 read at JPY's digits failed and counted as none, and
// 1,000 JPY of tax was added to 22.80 EUR as 10.00.
func TestSimulationInTheCompanysCurrency(t *testing.T) {
	e := foreign(t)
	e.rate(t, "JPY", "EUR", "0.0062")
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	e.receive(t, po, 2, "5")
	e.clean(t, po, "INV-1") // 22.80 EUR of tax, clean
	jpy := e.orderIn(t, "JPY", 1000)
	// 850 JPY over the order price is 5.27 EUR: beyond 5.00, within 5.50.
	e.foreignInvoice(t, jpy, "JPY", "JP-1", "10850", "1000", "11850")
	base := fixture.Document(t)
	proposed := fixture.Document(t)
	proposed.Companies[1].InvoiceTolerance.PriceAbsolute = "5.50"
	proposed.Companies[1].AccountDetermination.InputTax = "140100"
	sim, err := e.simulate(t, base, proposed)
	if err != nil {
		t.Fatal(err)
	}
	changes := sim.Detail["changes"].([]change)
	if len(changes) != 1 {
		t.Fatalf("changes: got %d, want 1: %+v", len(changes), changes)
	}
	if got := changes[0].Base + " -> " + changes[0].Proposed; got != "exception -> auto_post" {
		t.Errorf("changed decision: got %s, want exception -> auto_post", got)
	}
	postings := sim.Detail["postings"].([]posting)
	if len(postings) != 1 {
		t.Fatalf("postings: got %d, want 1: %+v", len(postings), postings)
	}
	if postings[0].Amount != 2280+620 { // 22.80 EUR and 1,000 JPY = 6.20 EUR
		t.Errorf("input tax moved: got %d, want %d", postings[0].Amount, 2280+620)
	}
}

// TestNearTheLimitInTheCompanysCurrency: a clean invoice of 7,000,000 JPY
// (43,400 EUR) is posted automatically under the 50,000.00 EUR limit, and
// it is near the limit. Before, the limit was read as 50,000 JPY.
func TestNearTheLimitInTheCompanysCurrency(t *testing.T) {
	e := foreign(t)
	e.rate(t, "JPY", "EUR", "0.0062")
	po := e.orderIn(t, "JPY", 700_000)
	e.foreignInvoice(t, po, "JPY", "JP-1", "7000000", "0", "7000000")
	cases, err := e.capture(t, fixture.Document(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("cases: got %d, want 1: %+v", len(cases), cases)
	}
	if cases[0].ExpectedDecision != payables.DecisionAutoPost {
		t.Errorf("decision: got %s, want %s", cases[0].ExpectedDecision, payables.DecisionAutoPost)
	}
	if !strings.Contains(cases[0].Why, "near the auto-post limit") {
		t.Errorf("why: got %q, want it to contain %q", cases[0].Why, "near the auto-post limit")
	}
	t.Run("configured amounts that are not decimals", func(t *testing.T) {
		checkNotDecimals(t, e.in(t), cases)
	})
}

// checkNotDecimals: a configured amount that does not read as a decimal
// fails the match, the simulation, the capture and the replay; it is never
// taken as zero.
func checkNotDecimals(t *testing.T, e env, cases []configuration.RegressionCase) {
	t.Helper()
	bad := fixture.Document(t)
	bad.Companies[1].InvoiceTolerance.PriceAbsolute = "5,00"
	const want = "payables: company 2000: invoice_tolerance.price_absolute"
	_, err := e.simulate(t, bad, fixture.Document(t))
	pgtest.CheckErr(t, err, want)
	_, err = e.simulate(t, fixture.Document(t), bad)
	pgtest.CheckErr(t, err, want)
	_, err = e.capture(t, bad)
	pgtest.CheckErr(t, err, want)
	pgtest.CheckErr(t, e.replay(t, bad, cases), want)
	// An active document is validated, so only a document written past the
	// configuration context has such an amount.
	pgtest.Exec(t, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, restored_from, activated_by)
		SELECT tenant_id, gen_random_uuid(), 2, jsonb_set(document, '{companies,1,invoice_tolerance,price_absolute}', '"5,00"'), hash, 'rollback', 1,
			activated_by FROM config.config_version WHERE tenant_id = $1 AND number = 1`, e.Tenant.ID)
	pgtest.Exec(t, `UPDATE config.active_version SET number = 2, version = version + 1 WHERE tenant_id = $1`, e.Tenant.ID)
	inv := cases[0].InvoiceID.String()
	if code, m, _ := e.Do("POST", "/v1/supplier-invoices/"+inv+":match", "", e.tag(t, inv)); code != 500 {
		t.Errorf("match under the document: got %d %v, want 500", code, m)
	}
}

// TestGrossOutOfRangeInTheCompanysCurrency: an invoice whose gross amount
// is out of range in the company's currency can be neither compared with
// its limits nor posted; replaying it fails rather than wrapping.
func TestGrossOutOfRangeInTheCompanysCurrency(t *testing.T) {
	e := foreign(t)
	e.rate(t, "JPY", "EUR", "1000000")
	po := e.orderIn(t, "JPY", 1_000_000_000_000)
	e.foreignInvoice(t, po, "JPY", "JP-1", "10000000000000", "0", "10000000000000")
	_, err := e.capture(t, fixture.Document(t))
	pgtest.WantErr(t, err, "payables: replay invoice SI")
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("capture: got %v, want %v", err, money.ErrOverflow)
	}
}
