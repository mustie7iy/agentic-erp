package integration_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// dpSetup is a tenant whose legal entity 2000 has 159000 as its down
// payment account (ADR-0037), whose people may also post invoices, and
// whose API reads the extract dpTiny (dp.json).
func dpSetup(t *testing.T) env {
	t.Helper()
	f := fixture.New(t, append(slices.Clone(perms), "payables.invoice.post")...)
	dir := t.TempDir()
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir })
	doc := fixture.Document(t)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "159000", Name: "Down payments made", Type: config.Asset, VendorReconciliation: true})
	de.AccountDetermination.DownPayment = "159000"
	if err := apitest.Committed(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		t.Fatal(err)
	}
	e := env{Env: f, dir: dir}
	write(t, dir, "dp.json", dpTiny(true))
	return e
}

// dpTiny is tiny with an open down payment of 1,000.00 and its input tax
// of 190.00 on V1 (19%) to the vendor in SAP's net display, special G/L
// indicator A on the down payment account 159000 when indicated, else an
// ordinary line there.
func dpTiny(indicated bool) *sap.Extract {
	x := tiny()
	x.SKB1 = append(x.SKB1, sap.SKB1{BUKRS: "2000", SAKNR: "0000140000", TXT50: "Input tax", XBILK: "X"},
		sap.SKB1{BUKRS: "2000", SAKNR: "0000159000", TXT50: "Down payments made", XBILK: "X", MITKZ: "K"})
	umskz := ""
	if indicated {
		umskz = "A"
	}
	line := func(docln, account, hsl, lifnr, umskz, mwskz string) sap.ACDOCA {
		return sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000009", DOCLN: docln, RACCT: account, HSL: hsl, BUDAT: "20260310",
			BLART: "KZ", LIFNR: lifnr, XBLNR: "DP-1", UMSKZ: umskz, MWSKZ: mwskz}
	}
	x.ACDOCA = append(x.ACDOCA, line("000001", "0000159000", "1000.00", "0000000001", umskz, "V1"), line("000002", "0000140000", "190.00", "", "", "V1"),
		line("000003", "0000100000", "-1190.00", "", "", ""))
	return x
}

// supplierOf is the partner the load made of an SAP vendor.
func supplierOf(t *testing.T, e env, lifnr string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	pgtest.Scan(t, `SELECT entity_id FROM integration.external_ref WHERE tenant_id = $1 AND object = 'LFA1' AND external_key = $2`,
		[]any{e.Tenant.ID, lifnr}, &id)
	return id
}

// TestOpenDownPayments: an open SAP down payment (special G/L A) loads as
// the platform posts its own down payments (T72c, ADR-0037): the AP clerk
// sees it as a paid down payment request of the supplier and as its open
// special G/L item, a payment run's payable items leave it out, the
// reconciliation checks it apart from the open payables, and the
// supplier's final invoice clears it and reverses its input tax (T46d).
func TestOpenDownPayments(t *testing.T) {
	e := dpSetup(t)
	c := e.connector(t, "dp.json")
	run := e.run(t, c, "documents", false, e.DE.String())
	_, got := e.act(t, run, "start")
	if got["status"] != "loaded" {
		t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
	}
	loaded := got["report"].(map[string]any)["loaded"].(map[string]any)
	for k, want := range map[string]float64{"journal_entries": 3, "down_payments": 1} {
		if loaded[k] != want {
			t.Errorf("loaded %s: got %v, want %v", k, loaded[k], want)
		}
	}
	if _, got = e.act(t, run, "reconcile"); got["status"] != "reconciled" {
		t.Fatalf("reconcile: %v", got["report"])
	}
	for _, check := range []string{"2000 Open payables total true", "2000 Open down payments total true", "2000 Open down payments: vendors with differences true"} {
		if c := checks(got["report"].(map[string]any)); !strings.Contains(c, check) {
			t.Errorf("checks: got %s, want %s among them", c, check)
		}
	}
	supplier := supplierOf(t, e, "0000000001")
	page, _ := e.Must(200, "GET", "/v1/down-payment-requests?status=paid&supplier_id="+supplier.String(), "", "")
	if n := len(page["items"].([]any)); n != 1 {
		t.Fatalf("paid down payment requests of the supplier: got %d, want 1", n)
	}
	request := page["items"].([]any)[0].(map[string]any)
	for field, want := range map[string]string{"amount": "map[amount:1190.00 currency:EUR]", "tax_amount": "map[amount:190.00 currency:EUR]",
		"tax_code": "V1", "reference": "SAP 2000/2026/0100000009", "due_date": "2026-03-10"} {
		if got := fmt.Sprint(request[field]); got != want {
			t.Errorf("request %s: got %s, want %s", field, got, want)
		}
	}
	entry := request["journal_entry_id"].(string)
	items, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+supplier.String(), "", "")
	special := 0
	for _, it := range items["items"].([]any) {
		if m := it.(map[string]any); m["special_gl"] == "A" && m["journal_entry_id"] == entry {
			special++
		}
	}
	if special != 1 {
		t.Errorf("open special G/L items A of the supplier on the down payment's entry: got %d, want 1: %v", special, items["items"])
	}
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		payable, err := ledger.OpenItems{}.Payable(ctx, e.DE, "EUR", time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
		for _, p := range payable {
			if p.EntryID.String() == entry {
				t.Errorf("payable items: got the down payment's entry %s, want it left out of payment runs", entry)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.clearMigrated(t, supplier, request)
}

// clearMigrated posts the supplier's final invoice, 2,000.00 plus 380.00
// tax on V1, and clears the migrated down payment against it: the request
// is cleared, and the transfer reverses its 190.00 input tax with its tax
// item.
func (e env) clearMigrated(t *testing.T, supplier uuid.UUID, request map[string]any) {
	t.Helper()
	invoice, _ := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": "FINAL-1",
		"invoice_date": "2026-03-20", "gross_amount": {"amount": "2380.00", "currency": "EUR"}, "tax_amount": {"amount": "380.00", "currency": "EUR"},
		"items": [%s]}`, e.DE, supplier, e.AccountItem(t, "Machine", "2000.00")), "")
	path := "/v1/supplier-invoices/" + invoice["id"].(string)
	_, tag := e.Must(200, "GET", path, "", "")
	e.Must(200, "POST", path+":post", `{"posting_date": "2026-03-20"}`, tag)
	_, tag = e.Must(200, "GET", path, "", "")
	e.Must(200, "POST", path+":clearDownPayments", fmt.Sprintf(`{"posting_date": "2026-03-21", "items": [{"fiscal_year": 2026, "journal_entry_id": %q, "line_no": 1}]}`,
		request["journal_entry_id"]), tag)
	cleared, _ := e.Must(200, "GET", "/v1/down-payment-requests/"+request["id"].(string), "", "")
	if cleared["status"] != "cleared" || cleared["cleared_invoice_id"] != invoice["id"] {
		t.Fatalf("request after clearing: got %v against %v, want cleared against %v", cleared["status"], cleared["cleared_invoice_id"], invoice["id"])
	}
	var tax, item int64
	pgtest.Scan(t, `SELECT (SELECT sum(amount_tc) FROM ledger.journal_line WHERE tenant_id = $1 AND journal_entry_id = $2 AND tax_code = 'V1' AND special_gl IS NULL),
		(SELECT sum(tax_tc) FROM ledger.tax_item WHERE tenant_id = $1 AND journal_entry_id = $2)`, []any{e.Tenant.ID, cleared["clearing_entry_id"]}, &tax, &item)
	if tax != -19000 || item != -19000 {
		t.Errorf("the transfer's input tax line and tax item: got %d and %d, want -19000 and -19000", tax, item)
	}
}

// TestOpenDownPaymentsInBalances: in balances mode the down payment's
// document loads as an entry of its own beside the opening entry, which
// leaves it out; a reconciliation that finds SAP's line no longer a down
// payment fails both open item checks and names the vendor's down
// payments.
func TestOpenDownPaymentsInBalances(t *testing.T) {
	e := dpSetup(t)
	c := e.connector(t, "dp.json")
	run := e.run(t, c, "balances", false, e.DE.String())
	_, got := e.act(t, run, "start")
	if got["status"] != "loaded" {
		t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
	}
	loaded := got["report"].(map[string]any)["loaded"].(map[string]any)
	for k, want := range map[string]float64{"journal_entries": 2, "down_payments": 1} {
		if loaded[k] != want {
			t.Errorf("loaded %s: got %v, want %v", k, loaded[k], want)
		}
	}
	if n := count(t, `SELECT count(*) FROM payables.down_payment_request WHERE tenant_id = $1 AND status = 'paid'`, e.Tenant.ID); n != 1 {
		t.Errorf("paid down payment requests: got %d, want 1", n)
	}
	write(t, e.dir, "dp.json", dpTiny(false))
	_, got = e.act(t, run, "reconcile")
	report := got["report"].(map[string]any)
	if got["status"] != "failed" {
		t.Fatalf("reconcile status: got %v, want failed: %v", got["status"], report)
	}
	for _, check := range []string{"2000 Open payables total false", "2000 Open down payments total false"} {
		if c := checks(report); !strings.Contains(c, check) {
			t.Errorf("checks: got %s, want %s among them", c, check)
		}
	}
	if d := fmt.Sprint(report["diffs"]); !strings.Contains(d, "down payments of vendor 1") {
		t.Errorf("diffs: got %s, want the vendor's down payments among them", d)
	}
}
