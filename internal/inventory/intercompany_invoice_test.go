package inventory_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The receiving company's supplier invoice from an intercompany billing
// (S2i3; ADR-0082 As built S2i3), through the API, on S2i2's scene:
// company 2000 enters company 1000's billing of the widgets it received
// as its supplier invoice of the business partner that is company 1000,
// at the billing's amounts and tax, and posts it against the order's
// receipt, clearing GR/IR.

// icInvoiced is icBilled with company 2000's input tax code VI (10% on
// 140000, the rate company 1000's O1 charges) and the output code O0
// (0%) of company 1000.
func icInvoiced(c *config.Config) {
	icBilled(c)
	de := c.Company("2000")
	de.TaxCodes = append(de.TaxCodes, config.TaxCode{Code: "VI", Description: "Input tax 10%", RatePercent: "10", Account: "140000"})
	us := c.Company("1000")
	us.TaxCodes = append(us.TaxCodes, config.TaxCode{Code: "O0", Description: "Output tax 0%", Direction: config.TaxOutput,
		Components: []config.TaxComponent{{Key: "vat", AccountKey: config.KeyOutput, Account: "230000", Rates: []config.TaxRate{{RatePercent: "0", ValidFrom: "2026-01-01"}}}}})
}

func setupInvoicing(t *testing.T, cfg func(*config.Config)) billEnv {
	t.Helper()
	e := billEnv{icEnv: setupIntercompanyWith(t, cfg, "sales.billing.read", "sales.billing.post", "receivables.invoice.read", "ledger.entry.read")}
	e.revenue = e.Account(t, "1000", "410000")
	e.sister2000 = e.Customer(t, "BP-IC-2000", "Acme GmbH")
	pgtest.Exec(t, `UPDATE partner.business_partner SET group_company_id = $2, version = version + 1 WHERE id = $1`, e.sister2000, e.DE)
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, e.Tenant.ID, e.sister2000, e.US, e.Account(t, "1000", "120000"), e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'EUR', 'USD', '2026-01-01', 1.25, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	return e
}

// billed is a billing, its item on the output tax code given (nil: none),
// of an intercompany order whose goods issue is posted and received.
func (e billEnv) billed(t *testing.T, code any) map[string]any {
	t.Helper()
	o := e.issued(t)
	e.receive(t, uuid.MustParse(o["id"].(string)), 1, "10")
	b, _ := e.Must(201, "POST", icBillings, e.bill(t, o["id"], e.sister2000, map[string]any{"tax_code": code}), "")
	return b
}

// entering is the path entering the billing as a supplier invoice.
func entering(billing any) string {
	return fmt.Sprintf("/v1/intercompany-billings/%v/supplier-invoices", billing)
}

// TestIntercompanyInvoice: company 2000 enters company 1000's billing as
// its supplier invoice of the business partner that is company 1000,
// parked, at the billing's number, date, amounts and tax, its item the
// order's at the billing's net with the input code charging the billed
// rate; posted, it clears GR/IR at the transfer price and recovers the
// tax, and each company's entry balances (S2i3 acceptance). A billing is
// entered once.
func TestIntercompanyInvoice(t *testing.T) {
	e := setupInvoicing(t, icInvoiced)
	b := e.billed(t, "O1")
	inv, tag := e.Must(201, "POST", entering(b["id"]), "", "")
	for f, want := range map[string]any{"status": "parked", "source": "edi", "legal_entity_id": e.DE.String(), "supplier_id": e.sister.String(),
		"supplier_reference": b["document_number"], "invoice_date": b["billing_date"], "purchase_order_id": b["purchase_order_id"],
		"intercompany_billing_id": b["id"], "kind": "invoice"} {
		if inv[f] != want {
			t.Errorf("%s: got %v, want %v", f, inv[f], want)
		}
	}
	if got, want := amountOf(t, inv, "gross_amount")+" "+amountOf(t, inv, "tax_amount"), amountOf(t, b, "gross_amount")+" "+amountOf(t, b, "tax_amount"); got != want {
		t.Errorf("gross and tax: got %s, want the billing's %s", got, want)
	}
	its := rows(t, inv, "items")
	if len(its) != 1 {
		t.Fatalf("items: got %d, want 1", len(its))
	}
	if got, want := fmt.Sprint(its[0]["po_item_no"], " ", quantityOf(t, its[0]), " ", amountOf(t, its[0], "amount"), " ", its[0]["tax_code"]), "1 10 EA 30.00 VI"; got != want {
		t.Errorf("the item: got %s, want %s", got, want)
	}
	e.Problem(409, "payables.intercompany_billing_entered", "POST", entering(b["id"]), "", "")
	id := inv["id"].(string)
	if m, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id+":match", "", tag); m["is_clean"] != true {
		t.Fatalf("match: got %v, want clean", m["issues"])
	}
	_, tag = e.Must(200, "GET", "/v1/supplier-invoices/"+id, "", "")
	posted, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id+":post", mustJSON(t, map[string]any{"posting_date": e.day}), tag)
	if posted["status"] != "posted" {
		t.Fatalf("posted: got %v", posted["status"])
	}
	// Company 2000: GR/IR credited 30.00 by the receipt, debited by the
	// invoice; the input tax recovered; the sister owed the billing's 33.00.
	for account, want := range map[string]int64{"191000": 0, "140000": 300, "150000": 3000} {
		if got := e.balance(t, account); got != want {
			t.Errorf("company 2000's balance of %s: got %d, want %d", account, got, want)
		}
	}
	entry, _ := e.Must(200, "GET", "/v1/journal-entries/"+posted["journal_entry_id"].(string), "", "")
	var sum money.Minor
	var amounts []string
	for _, l := range rows(t, entry, "lines") {
		a := amountOf(t, l, "amount")
		amounts = append(amounts, a)
		m, err := money.ParseDecimal(a, 2)
		if err != nil {
			t.Fatal(err)
		}
		sum += m // three lines of a few euros: in range
	}
	sort.Strings(amounts)
	if got, want := strings.Join(amounts, " "), "-33.00 3.00 30.00"; got != want || sum != 0 {
		t.Errorf("the invoice's entry: got %s summing to %d, want %s summing to 0", got, sum, want)
	}
	if got, want := e.entryLines(t, b["journal_entry_id"].(string)), "120000:33.00 230000:-3.00 410000:-30.00"; got != want {
		t.Errorf("company 1000's billing entry: got %s, want %s", got, want)
	}
}

// TestIntercompanyInvoiceTax: an item billed without a code, or on one
// charging 0%, whose order item has none stays without one; one billed
// without a code whose order item has one states 0%, which proposes the
// company's 0% code (V0) for it; a rate no
// input code of the company charges, or a code the supplying company no
// longer has on the billing date, holds the invoice naming the line.
func TestIntercompanyInvoiceTax(t *testing.T) {
	e := setupInvoicing(t, icInvoiced)
	e.post(t, "561", e.item("30", "EA", map[string]any{"material_id": e.widget, "plant_id": e.p3, "storage_location_id": e.s4}))
	// In turn: the last two change the tax codes the others bill on.
	for _, c := range []struct {
		name         string
		code         any
		orderCode    string // "": the order item has none
		before       string // run as the owner before entering
		status, held string
		itemCode     any // the invoice item's, nil: none
	}{
		{"no code", nil, "", "", "parked", "", nil},
		{"zero", "O0", "", "", "parked", "", nil},
		{"no code on a coded order item", nil, "V1", "", "parked", "", "V0"},
		{"no input code at the rate", "O1", "", `DO $$ BEGIN SET LOCAL session_replication_role = replica;
			DELETE FROM ledger.tax_rate WHERE code = 'VI'; DELETE FROM ledger.tax_component WHERE code = 'VI'; DELETE FROM ledger.tax_code_definition WHERE code = 'VI'; END $$`,
			"on_hold", "Line 1 prints a tax rate of 10%, which no input tax code charges", nil},
		{"a code gone", "O0", "", `DO $$ BEGIN SET LOCAL session_replication_role = replica; UPDATE ledger.tax_rate SET valid_from = '2099-01-01' WHERE code = 'O0'; END $$`,
			"on_hold", "Line 1 is billed on tax code O0, which the supplying company no longer has on", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := e.billed(t, c.code)
			if c.orderCode != "" {
				pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica; UPDATE procurement.purchase_order_item SET tax_code = '`+c.orderCode+
					`' WHERE purchase_order_id = '`+b["purchase_order_id"].(string)+`'; END $$`)
			}
			if c.before != "" {
				pgtest.Exec(t, c.before)
			}
			inv, _ := e.Must(201, "POST", entering(b["id"]), "", "")
			if inv["status"] != c.status {
				t.Errorf("status: got %v, want %s", inv["status"], c.status)
			}
			if reason, _ := inv["hold_reason"].(string); c.held == "" && reason != "" || !strings.Contains(reason, c.held) {
				t.Errorf("hold reason: got %q, want %q", reason, c.held)
			}
			if code := rows(t, inv, "items")[0]["tax_code"]; code != c.itemCode {
				t.Errorf("the item's code: got %v, want %v", code, c.itemCode)
			}
		})
	}
}

// TestIntercompanyInvoiceRefused: a billing that does not exist, or is
// billed to a company the caller may not act in, is not found; one
// entered stays entered when the invoice's reference changes, and is
// freed when it is cancelled.
func TestIntercompanyInvoiceRefused(t *testing.T) {
	e := setupInvoicing(t, icInvoiced)
	b := e.billed(t, "O1")
	for name, c := range map[string]struct {
		path      string
		companies []uuid.UUID // nil: the tenant's person
	}{
		"no such billing":         {entering(uuid.New()), nil},
		"the supplying company's": {entering(b["id"]), []uuid.UUID{e.US}},
	} {
		t.Run(name, func(t *testing.T) {
			client := e.Client
			if c.companies != nil {
				client = e.As(e.Reader(t, "Clerk "+name, c.companies...))
			}
			client.Problem(404, "request.not_found", "POST", c.path, "", "")
		})
	}
	inv, tag := e.Must(201, "POST", entering(b["id"]), "", "")
	id := inv["id"].(string)
	_, tag = e.Must(200, "PATCH", "/v1/supplier-invoices/"+id, `{"supplier_reference": "ANOTHER-1"}`, tag)
	e.Problem(409, "payables.intercompany_billing_entered", "POST", entering(b["id"]), "", "")
	e.Must(200, "POST", "/v1/supplier-invoices/"+id+":cancel", `{"reason": "Entered by mistake"}`, tag)
	again, _ := e.Must(201, "POST", entering(b["id"]), "", "")
	if again["id"] == id || again["status"] != "parked" {
		t.Errorf("entered again after the cancellation: got %v %v, want a new parked invoice", again["id"], again["status"])
	}
}

// TestIntercompanyInvoiceRace: two requests entering one billing at once
// enter it once; the other is refused, by the check or the key.
func TestIntercompanyInvoiceRace(t *testing.T) {
	e := setupInvoicing(t, icInvoiced)
	path := entering(e.billed(t, "O1")["id"])
	got := make([]int, 2)
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() {
			got[i] = e.API.Do(apitest.Req{Method: "POST", Path: path, Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}).Code
		})
	}
	wg.Wait()
	sort.Ints(got)
	if got[0] != 201 || got[1] != 409 {
		t.Errorf("statuses: got %v, want [201 409]", got)
	}
}

// TestIntercompanyInvoiceDatabaseFailures: every statement of entering a
// billing fails cleanly.
func TestIntercompanyInvoiceDatabaseFailures(t *testing.T) {
	e := setupInvoicing(t, icInvoiced)
	h := app.Payables(app.Config{})
	id := uuid.MustParse(e.billed(t, "O1")["id"].(string))
	call := func(ctx context.Context) error {
		_, err := h.EnterIntercompanySupplierInvoice(ctx, gen.EnterIntercompanySupplierInvoiceParams{ID: id})
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "enter", call)
}

// TestIntercompanyInvoiceKey: a request entering a billing while another,
// not yet committed, has entered it and changed its reference meets the
// key, not the reference's: 409 payables.intercompany_billing_entered.
func TestIntercompanyInvoiceKey(t *testing.T) {
	e := setupInvoicing(t, icInvoiced)
	h := app.Payables(app.Config{})
	params := gen.EnterIntercompanySupplierInvoiceParams{ID: uuid.MustParse(e.billed(t, "O1")["id"].(string))}
	first := func(ctx context.Context) error {
		res, err := h.EnterIntercompanySupplierInvoice(ctx, params)
		if err != nil {
			return err
		}
		tx, _, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE payables.supplier_invoice SET supplier_reference = 'ANOTHER-1', version = version + 1 WHERE id = $1`,
			res.(*gen.SupplierInvoiceHeaders).Response.ID)
		return err
	}
	second := func(ctx context.Context) error {
		_, err := h.EnterIntercompanySupplierInvoice(ctx, params)
		return err
	}
	firstErr, secondErr := apitest.Race(t, e.Tenant, e.Tenant.Human, e.Tenant.Human, first, second)
	if firstErr != nil {
		t.Fatalf("the first: got %v, want nil", firstErr)
	}
	if p := problem.From(secondErr); p == nil || p.Code != "payables.intercompany_billing_entered" {
		t.Errorf("the second: got %v, want payables.intercompany_billing_entered", secondErr)
	}
}
