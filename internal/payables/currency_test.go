package payables_test

import (
	"context"
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
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, '2026-03-01', 'NT30', 'draft', $7, $7)`,
		e.Tenant.ID, id, "PO-"+currency+id.String()[30:], e.DE, e.Supplier, currency, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id, valuation_price, valuation_price_unit) VALUES ($1, $2, 1, 'Paper', 10, 'EA', $3, 1, $4, $3, 1)`, e.Tenant.ID, id, price, e.expense)
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', version = version + 1 WHERE id = $1`, id)
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

// decision is the decision a match answers, "outcome: reason", or
// "absent".
func decision(m map[string]any) string {
	d, ok := m["decision"].(map[string]any)
	if !ok {
		return "absent"
	}
	return fmt.Sprint(d["outcome"], ": ", d["reason"])
}

// TestMatchDecisionInTheCompanysCurrency: the match answers the invoice
// flow's decision, its gross converted into EUR and compared with the
// 50,000.00 EUR auto-post limit (F12b). 7,000,000 JPY is 43,400.00 EUR,
// posted automatically although the number is above 50,000; 30,000.000 BHD
// is 72,000.00 EUR, reviewed although the number is below it.
func TestMatchDecisionInTheCompanysCurrency(t *testing.T) {
	e := foreign(t)
	e.rate(t, "JPY", "EUR", "0.0062")
	e.rate(t, "BHD", "EUR", "2.4")
	for _, c := range []struct {
		currency    string
		price       int64
		gross, none string
		want        string
	}{
		{"JPY", 700_000, "7000000", "0", "auto_post: gross 43400.00 EUR is within the auto-post limit 50000.00 EUR, near it"},
		{"BHD", 3_000_000, "30000.000", "0.000", "review: gross 72000.00 EUR is above the auto-post limit 50000.00 EUR"},
	} {
		t.Run(c.currency, func(t *testing.T) {
			e := e.in(t)
			inv := e.foreignInvoice(t, e.orderIn(t, c.currency, c.price), c.currency, c.currency+"-1", c.gross, c.none, c.gross)
			if got := decision(e.act(t, 200, inv, "match", "")); got != c.want {
				t.Errorf("decision: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestMatchDecisionAtAnInverseRate: with only EUR into JPY recorded, at
// 3,000,000,000,000 JPY a euro, a JPY invoice is divided by that rate,
// exactly, and decided as an invoice of the same EUR value at a forward
// rate is (F19). Before, the inverse was rounded to 12 decimals, which made
// it zero: the invoice was worth 0.00 EUR and posted automatically. The
// forward case is KRW at 0.000000000001 EUR, the inverse of 3e12 having no
// 12-decimal form to record as a forward rate.
func TestMatchDecisionAtAnInverseRate(t *testing.T) {
	e := foreign(t)
	e.rate(t, "EUR", "JPY", "3000000000000")
	e.rate(t, "KRW", "EUR", "0.000000000001")
	const want = "review: gross 50001.00 EUR is above the auto-post limit 50000.00 EUR"
	for _, c := range []struct {
		name, currency string
		price          int64
		gross          string
	}{
		{"only the reverse pair", "JPY", 15_000_300_000_000_000, "150003000000000000"}, // 50,001.00 EUR at 1/3e12
		{"the forward pair", "KRW", 5_000_100_000_000_000, "50001000000000000"},        // 50,001.00 EUR at 1e-12
	} {
		t.Run(c.name, func(t *testing.T) {
			e := e.in(t)
			inv := e.foreignInvoice(t, e.orderIn(t, c.currency, c.price), c.currency, c.currency+"-1", c.gross, "0", c.gross)
			if got := decision(e.act(t, 200, inv, "match", "")); got != want {
				t.Errorf("decision: got %q, want %q", got, want)
			}
		})
	}
}

// TestNoRateRefusesTheMatch: without a rate from the invoice's currency to
// the company's, the match is refused (the user's F12 decision), naming the
// pair and the invoice date, rather than comparing the raw numbers; no
// match result is stored and the invoice is unchanged.
func TestNoRateRefusesTheMatch(t *testing.T) {
	e := foreign(t)
	po := e.orderIn(t, "USD", 1200)
	inv := e.foreignInvoice(t, po, "USD", "US-1", "120.00", "0.00", "120.00")
	tag := e.tag(t, inv)
	code, p, _ := e.Do("POST", "/v1/supplier-invoices/"+inv+":match", "", tag)
	if code != 422 || p["code"] != "ledger.no_exchange_rate" {
		t.Fatalf("match: got %d %v, want 422 ledger.no_exchange_rate", code, p["code"])
	}
	if detail, want := fmt.Sprint(p["detail"]), "No average exchange rate from USD to EUR is valid on 2026-03-10"; !strings.Contains(detail, want) {
		t.Errorf("detail: got %q, want it to contain %q", detail, want)
	}
	results, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+inv+"/match-results", "", "")
	if items := results["items"].([]any); len(items) != 0 {
		t.Errorf("match results: got %d, want 0", len(items))
	}
	if got := e.tag(t, inv); got != tag {
		t.Errorf("invoice version: got %s, want %s", got, tag)
	}
}

// TestMatchNotEvaluable: a gross amount out of range in the company's
// currency is matched, as under F12, and a clean match is not evaluable:
// it cannot be compared with the auto-post limit.
func TestMatchNotEvaluable(t *testing.T) {
	e := foreign(t)
	e.rate(t, "JPY", "EUR", "1000000")
	huge := e.foreignInvoice(t, e.orderIn(t, "JPY", 1_000_000_000_000), "JPY", "JP-1", "10000000000000", "0", "10000000000000")
	if got, want := decision(e.act(t, 200, huge, "match", "")), "not_evaluable: amount_too_large"; got != want {
		t.Errorf("decision out of range: got %q, want %q", got, want)
	}
}

// auditedDecision is the decision the audit record of an invoice's latest
// match carries, or "absent".
func (e env) auditedDecision(t *testing.T, invoice string) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT coalesce(detail->>'decision', 'absent') FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2
		AND action = 'payables.supplier_invoice.match' ORDER BY seq DESC LIMIT 1`, []any{e.Tenant.ID, invoice}, &got)
	return got
}

// TestMatchDecisionOnlyOnACleanMatch: the decision is answered by :match
// on a clean match and recorded in its audit detail; a match with issues
// has none, nor do reads of the match history, which carry the ETag the
// :match response had.
func TestMatchDecisionOnlyOnACleanMatch(t *testing.T) {
	e := foreign(t)
	e.rate(t, "JPY", "EUR", "0.0062")
	po := e.orderIn(t, "JPY", 1000)
	t.Run("clean", func(t *testing.T) {
		e := e.in(t)
		inv := e.foreignInvoice(t, po, "JPY", "JP-1", "10000", "0", "10000")
		m, tag := e.Must(200, "POST", "/v1/supplier-invoices/"+inv+":match", "", e.tag(t, inv))
		if got, want := decision(m), "auto_post: gross 62.00 EUR is within the auto-post limit 50000.00 EUR"; got != want {
			t.Errorf("decision: got %q, want %q", got, want)
		}
		if got, want := e.auditedDecision(t, inv), "auto_post"; got != want {
			t.Errorf("audited decision: got %q, want %q", got, want)
		}
		read, readTag := e.Must(200, "GET", "/v1/match-results/"+m["id"].(string), "", "")
		if got, want := decision(read), "absent"; got != want {
			t.Errorf("decision read back: got %q, want %q", got, want)
		}
		if readTag != tag {
			t.Errorf("ETag read back: got %s, want %s", readTag, tag)
		}
		list, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+inv+"/match-results", "", "")
		if got, want := decision(list["items"].([]any)[0].(map[string]any)), "absent"; got != want {
			t.Errorf("decision in the history: got %q, want %q", got, want)
		}
	})
	t.Run("with issues", func(t *testing.T) {
		e := e.in(t)
		inv := e.foreignInvoice(t, po, "JPY", "JP-2", "10900", "0", "10900") // 900 JPY = 5.58 EUR beyond 5.00
		if got, want := decision(e.act(t, 200, inv, "match", "")), "absent"; got != want {
			t.Errorf("decision: got %q, want %q", got, want)
		}
		if got, want := e.auditedDecision(t, inv), "absent"; got != want {
			t.Errorf("audited decision: got %q, want %q", got, want)
		}
	})
}

// day is the day the window of simulations and suites ends.
var day = time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)

func (e env) simulate(t *testing.T, base, proposed *config.Config) (simulated, error) {
	t.Helper()
	return e.simulated(t, payables.Simulation{LegalEntities: tenancy.Directory{}, Orders: procurement.Orders{}, Rates: rates,
		Now: func() time.Time { return day }}, base, proposed)
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
	changes := sim.Changes
	if len(changes) != 1 {
		t.Fatalf("changes: got %d, want 1: %+v", len(changes), changes)
	}
	if got := changes[0].Base + " -> " + changes[0].Proposed; got != "exception -> auto_post" {
		t.Errorf("changed decision: got %s, want exception -> auto_post", got)
	}
	postings := sim.Postings
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

// TestUnevaluableInvoices: an invoice that cannot be compared with its
// company's amounts, for want of a rate on its invoice date or because its
// gross amount is out of range in the company's currency, is reported by
// the simulation as one that cannot be evaluated and captured as
// not_evaluable. It does not fail every simulation, capture and replay of
// the tenant: entering an invoice needs no rate.
func TestUnevaluableInvoices(t *testing.T) {
	e := foreign(t)
	e.rate(t, "JPY", "EUR", "1000000")
	huge := e.orderIn(t, "JPY", 1_000_000_000_000)
	e.foreignInvoice(t, huge, "JPY", "JP-1", "10000000000000", "0", "10000000000000")
	usd := e.orderIn(t, "USD", 1200)
	e.foreignInvoice(t, usd, "USD", "US-1", "120.00", "0.00", "120.00")
	base := fixture.Document(t)
	sim, err := e.simulate(t, base, base)
	if err != nil {
		t.Fatal(err)
	}
	if sim.Passed {
		t.Error("simulation passed: got true, want false")
	}
	var reasons []string
	for _, b := range sim.Breaks {
		reasons = append(reasons, b.Reason)
	}
	if got, want := strings.Join(reasons, " | "), "no average exchange rate from USD to EUR on 2026-03-10 | gross amount 10000000000000 JPY cannot be converted into EUR: ledger: convert JPY into EUR: "+money.ErrOverflow.Error(); got != want {
		t.Errorf("breaks: got %q, want %q", got, want)
	}
	cases, err := e.capture(t, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatalf("cases: got %d, want 2: %+v", len(cases), cases)
	}
	var issues []string
	for _, c := range cases {
		if c.ExpectedDecision != payables.DecisionNotEvaluable {
			t.Errorf("case %s decision: got %s, want %s", c.DocumentNumber, c.ExpectedDecision, payables.DecisionNotEvaluable)
		}
		issues = append(issues, c.ExpectedIssues...)
	}
	if got, want := strings.Join(issues, ","), "no_exchange_rate,amount_too_large"; got != want {
		t.Errorf("case issues: got %s, want %s", got, want)
	}
	if err := e.replay(t, base, cases); err != nil {
		t.Errorf("replay: got %v, want no error", err)
	}
}
