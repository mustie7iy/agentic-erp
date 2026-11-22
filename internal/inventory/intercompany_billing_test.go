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
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// Intercompany billing (S2i2; ADR-0082 As built S2i), through the API,
// beside the S2i1 tests whose scene it bills: company 1000, which issued
// the widgets out of P3, bills the goods issue of company 2000's order to
// the business partner that is company 2000, at the order's price, and
// posts it as a customer invoice in company 1000.

const icBillings = "/v1/intercompany-billings"

// icBilled is intercompany with what company 1000 bills on: trade
// receivables 120000, intercompany revenue 410000, output tax 230000, the
// output codes O1 (10%) and OJ (10%, levying the emirate its lines name,
// AE-SH or AE-DU) and the journal_dr range its customer invoices' entries
// are numbered from.
func icBilled(c *config.Config) {
	intercompany(c)
	us := c.Company("1000")
	us.ChartOfAccounts = append(us.ChartOfAccounts, config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true},
		config.Account{Number: "230000", Name: "Output tax", Type: config.Liability}, config.Account{Number: "410000", Name: "Intercompany revenue", Type: config.Revenue})
	output := func(code string, emirates bool) config.TaxCode {
		c := config.TaxComponent{Key: "vat", AccountKey: config.KeyOutput, Account: "230000", Rates: []config.TaxRate{{RatePercent: "10", ValidFrom: "2026-01-01"}}}
		if emirates {
			c.Jurisdiction, c.JurisdictionOnLine, c.Subdivisions = "AE", true, []string{"AE-SH", "AE-DU"}
		}
		return config.TaxCode{Code: code, Description: "Output tax " + code, Direction: config.TaxOutput, Components: []config.TaxComponent{c}}
	}
	us.TaxCodes = append(us.TaxCodes, output("O1", false), output("OJ", true))
	for _, y := range []int{2026, 2027} {
		c.NumberRanges = append(c.NumberRanges, config.NumberRange{Company: "1000", DocumentType: "journal_dr", FiscalYear: y, First: 100000, Last: 999999})
	}
}

// billEnv is icEnv with company 1000's revenue account and its customer
// that is company 2000 (BP-IC-2000, set up in 1000 on 120000 with NT30).
type billEnv struct {
	icEnv
	revenue, sister2000 uuid.UUID
}

func setupBilling(t *testing.T) billEnv {
	t.Helper()
	e := billEnv{icEnv: setupIntercompanyWith(t, icBilled, "sales.billing.read", "sales.billing.post", "receivables.invoice.read", "ledger.entry.read")}
	e.revenue = e.Account(t, "1000", "410000")
	e.sister2000 = e.Customer(t, "BP-IC-2000", "Acme GmbH")
	pgtest.Exec(t, `UPDATE partner.business_partner SET group_company_id = $2, version = version + 1 WHERE id = $1`, e.sister2000, e.DE)
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, e.Tenant.ID, e.sister2000, e.US, e.Account(t, "1000", "120000"), e.Tenant.Human.ID)
	// Company 1000 books its EUR invoice in USD.
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'EUR', 'USD', '2026-01-01', 1.25, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	return e
}

// issued is a released intercompany order of one item whose goods issue
// is posted.
func (e billEnv) issued(t *testing.T) map[string]any {
	t.Helper()
	return e.issue(t, e.released(t), 200)
}

// bill is a request billing the order to the customer, its item 1 on the
// revenue account with the fields of over (nil: taken out).
func (e billEnv) bill(t *testing.T, po, customer any, over map[string]any) string {
	t.Helper()
	it := map[string]any{"po_item_no": 1, "account_id": e.revenue, "tax_code": "O1"}
	for k, v := range over {
		if v == nil {
			delete(it, k)
			continue
		}
		it[k] = v
	}
	return mustJSON(t, map[string]any{"purchase_order_id": po, "customer_id": customer, "items": []any{it}})
}

// TestIntercompanyBilling: company 1000 bills the goods issue of company
// 2000's order to the business partner that is company 2000, at the
// order's price, the transfer price, with its output tax, and posts it as
// a customer invoice of 1000 naming the billing as its source, Dr trade
// receivables, Cr revenue and output tax; the goods issue is billed once;
// either company reads the billing (S2i2 acceptance).
func TestIntercompanyBilling(t *testing.T) {
	e := setupBilling(t)
	o := e.issued(t)
	po := o["id"].(string)
	got, tag := e.Must(201, "POST", icBillings, e.bill(t, po, e.sister2000, nil), "")
	if tag == "" {
		t.Error("ETag: got none, want the billing's")
	}
	for f, want := range map[string]any{"legal_entity_id": e.US.String(), "company_code": "1000", "receiving_legal_entity_id": e.DE.String(),
		"receiving_company_code": "2000", "customer_id": e.sister2000.String(), "customer_number": "BP-IC-2000", "customer_name": "Acme GmbH",
		"purchase_order_id": po, "purchase_order_number": o["document_number"], "issue_document_id": o["issue_document_id"],
		"issue_document_number": o["issue_document_number"], "billing_date": o["issue_date"], "currency": "EUR"} {
		if got[f] != want {
			t.Errorf("%s: got %v, want %v", f, got[f], want)
		}
	}
	if a := fmt.Sprintf("%s %s %s", amountOf(t, got, "net_amount"), amountOf(t, got, "tax_amount"), amountOf(t, got, "gross_amount")); a != "30.00 3.00 33.00" {
		t.Errorf("net, tax and gross: got %s, want 30.00 3.00 33.00 (the order's 10 at EUR 3.00, 10%% tax)", a)
	}
	if n, _ := got["document_number"].(string); !strings.HasPrefix(n, "BD") {
		t.Errorf("number: got %v, want one of company 1000's billing_document range (BD…)", got["document_number"])
	}
	its := rows(t, got, "items")
	if len(its) != 1 {
		t.Fatalf("items: got %d, want 1", len(its))
	}
	it := its[0]
	if got, want := fmt.Sprint(it["item_no"], " ", it["po_item_no"], " ", it["material_id"] == e.widget, " ", it["description"], " ", quantityOf(t, it),
		" ", amountOf(t, it, "unit_price"), "/", it["price_unit"], " ", amountOf(t, it, "net_amount"), " ", amountOf(t, it, "tax_amount"), " ",
		it["account_number"], " ", it["tax_code"]), "1 1 true Widget 10 EA 3.00/1 30.00 3.00 410000 O1"; got != want {
		t.Errorf("the item: got %s, want %s", got, want)
	}
	inv, _ := e.Must(200, "GET", "/v1/customer-invoices/"+got["customer_invoice_id"].(string), "", "")
	if got, want := fmt.Sprint(inv["status"], " ", inv["legal_entity_id"] == e.US.String(), " ", inv["source_document_id"] == got["id"], " ",
		inv["document_number"] == got["customer_invoice_number"], " ", inv["journal_entry_id"] == got["journal_entry_id"]), "posted true true true true"; got != want {
		t.Errorf("the customer invoice: got %s, want %s", got, want)
	}
	if got, want := e.entryLines(t, got["journal_entry_id"].(string)), "120000:33.00 230000:-3.00 410000:-30.00"; got != want {
		t.Errorf("its entry in EUR: got %s, want %s", got, want)
	}
	e.Problem(409, "sales.intercompany_billed", "POST", icBillings, e.bill(t, po, e.sister2000, nil), "")
	read, readTag := e.Must(200, "GET", icBillings+"/"+got["id"].(string), "", "")
	if readTag != tag || read["journal_entry_number"] != got["journal_entry_number"] || read["journal_entry_number"] == "" {
		t.Errorf("read back: got ETag %s and entry %v, want %s and %v", readTag, read["journal_entry_number"], tag, got["journal_entry_number"])
	}
	for name, who := range map[string][]uuid.UUID{"the supplying company's": {e.US}, "the receiving company's": {e.DE}} {
		t.Run(name, func(t *testing.T) {
			e.As(e.Reader(t, "Reader of "+name, who...)).Must(200, "GET", icBillings+"/"+got["id"].(string), "", "")
		})
	}
	e.As(e.Reader(t, "No company")).Problem(404, "request.not_found", "GET", icBillings+"/"+got["id"].(string), "", "")
	for q, want := range map[string]int{"purchase_order_id=" + po: 1, "legal_entity_id=" + e.US.String(): 1, "receiving_legal_entity_id=" + e.US.String(): 0,
		"purchase_order_id=" + uuid.NewString(): 0} {
		page, _ := e.Must(200, "GET", icBillings+"?"+q, "", "")
		if n := len(rows(t, page, "items")); n != want {
			t.Errorf("billings %s: got %d, want %d", q, n, want)
		}
	}
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'sales.intercompany_billing.create' AND object_id = $2`,
		[]any{e.Tenant.ID, got["id"]}, &audits)
	if audits != 1 {
		t.Errorf("audit records: got %d, want 1", audits)
	}
}

// entryLines are the journal entry's lines as account:amount in the
// transaction currency, sorted.
func (e billEnv) entryLines(t *testing.T, id string) string {
	t.Helper()
	entry, _ := e.Must(200, "GET", "/v1/journal-entries/"+id, "", "")
	numbers := map[string]string{}
	for _, n := range []string{"120000", "230000", "410000"} {
		numbers[e.Account(t, "1000", n).String()] = n
	}
	var out []string
	for _, l := range rows(t, entry, "lines") {
		out = append(out, numbers[l["account_id"].(string)]+":"+amountOf(t, l, "amount"))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func amountOf(t *testing.T, m map[string]any, field string) string {
	t.Helper()
	a, ok := m[field].(map[string]any)
	if !ok {
		t.Fatalf("%s: got %v, want an amount", field, m[field])
	}
	return fmt.Sprint(a["amount"])
}

func quantityOf(t *testing.T, m map[string]any) string {
	t.Helper()
	q, ok := m["quantity"].(map[string]any)
	if !ok {
		t.Fatalf("quantity: got %v, want one", m["quantity"])
	}
	return fmt.Sprintf("%v %v", q["value"], q["unit"])
}

// TestIntercompanyBillingEmirate: an item whose code levies the UAE's
// emirates and that names none takes the supplying plant's region (the
// seller's side, ADR-0049 §11); one named is kept.
func TestIntercompanyBillingEmirate(t *testing.T) {
	e := setupBilling(t)
	pgtest.Exec(t, `UPDATE tenancy.org_unit SET region = 'AE-SH', version = version + 1 WHERE id = $1`, e.p3)
	for name, c := range map[string]struct {
		over map[string]any
		want string
	}{
		"defaulted": {map[string]any{"tax_code": "OJ"}, "AE-SH"},
		"named":     {map[string]any{"tax_code": "OJ", "tax_jurisdiction": "AE-DU"}, "AE-DU"},
	} {
		t.Run(name, func(t *testing.T) {
			o := e.issued(t)
			got, _ := e.Must(201, "POST", icBillings, e.bill(t, o["id"], e.sister2000, c.over), "")
			if j := rows(t, got, "items")[0]["tax_jurisdiction"]; j != c.want {
				t.Errorf("the item's jurisdiction: got %v, want %s", j, c.want)
			}
		})
	}
}

// TestIntercompanyBillingRefused: what cannot be billed is refused and
// bills nothing: an order that is not the caller's, no intercompany one,
// not issued; a customer that is not the order's company; not every item
// once; a date before the goods issue; what receivables refuses, named by
// the request's own fields.
func TestIntercompanyBillingRefused(t *testing.T) {
	e := setupBilling(t)
	o := e.issued(t)
	po := o["id"].(string)
	plain := e.stockOrder(t, "PO-PLAIN", "EUR")
	released := e.released(t)
	third := e.Customer(t, "BP-THIRD", "Third party")
	twice := mustJSON(t, map[string]any{"purchase_order_id": po, "customer_id": e.sister2000, "items": []any{
		map[string]any{"po_item_no": 1, "account_id": e.revenue}, map[string]any{"po_item_no": 1, "account_id": e.revenue}}})
	early := mustJSON(t, map[string]any{"purchase_order_id": po, "customer_id": e.sister2000, "billing_date": "2026-01-02",
		"items": []any{map[string]any{"po_item_no": 1, "account_id": e.revenue}}})
	for name, c := range map[string]struct {
		body          string
		status        int
		code, field   string
		onlyCompanies []uuid.UUID // nil: the tenant's person
	}{
		"no such order":           {e.bill(t, uuid.New(), e.sister2000, nil), 422, "request.reference_not_found", "purchase_order_id", nil},
		"no intercompany order":   {e.bill(t, plain, e.sister2000, nil), 422, "sales.not_intercompany", "purchase_order_id", nil},
		"not issued":              {e.bill(t, released["id"], e.sister2000, nil), 409, "sales.intercompany_not_issued", "", nil},
		"no such customer":        {e.bill(t, po, uuid.New(), nil), 422, "request.reference_not_found", "customer_id", nil},
		"a third party":           {e.bill(t, po, third, nil), 422, "sales.intercompany_customer", "customer_id", nil},
		"the supplier":            {e.bill(t, po, e.sister, nil), 422, "sales.intercompany_customer", "customer_id", nil},
		"another item":            {e.bill(t, po, e.sister2000, map[string]any{"po_item_no": 2}), 422, "sales.intercompany_items", "items[0].po_item_no", nil},
		"an item twice":           {twice, 422, "sales.intercompany_items", "items", nil},
		"before the goods issue":  {early, 422, "sales.billing_before_issue", "billing_date", nil},
		"an input tax code":       {e.bill(t, po, e.sister2000, map[string]any{"tax_code": "V1"}), 422, "receivables.unknown_tax_code", "items[0].tax_code", nil},
		"the receiving company's": {e.bill(t, po, e.sister2000, nil), 422, "request.reference_not_found", "purchase_order_id", []uuid.UUID{e.DE}},
		"the supplying company's": {e.bill(t, po, e.sister2000, nil), 422, "request.reference_not_found", "purchase_order_id", []uuid.UUID{e.US}},
		"an account of another":   {e.bill(t, po, e.sister2000, map[string]any{"account_id": e.Account(t, "2000", "400000")}), 422, "receivables.account_not_postable", "items[0].account_id", nil},
	} {
		t.Run(name, func(t *testing.T) {
			client := e.Client
			if c.onlyCompanies != nil {
				client = e.As(e.Reader(t, "Clerk "+name, c.onlyCompanies...))
			}
			got := client.Problem(c.status, c.code, "POST", icBillings, c.body, "")
			if c.field != "" && fieldOf(got) != c.field {
				t.Errorf("field: got %v, want %s", fieldOf(got), c.field)
			}
		})
	}
	// An order whose stored value is out of range (as no API request
	// stores one; past the released items' trigger) is not billed: its
	// value fails closed.
	huge := e.issued(t)
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE procurement.purchase_order_item SET unit_price = 9000000000000000000 WHERE purchase_order_id = '%s'; END $$`, uuid.MustParse(huge["id"].(string))))
	e.Problem(500, "platform.internal", "POST", icBillings, e.bill(t, huge["id"], e.sister2000, nil), "")
	page, _ := e.Must(200, "GET", icBillings, "", "")
	if n := len(rows(t, page, "items")); n != 0 {
		t.Errorf("billings after the refusals: got %d, want 0", n)
	}
}

// fieldOf is the field the problem's first field error names.
func fieldOf(m map[string]any) any {
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 {
		return nil
	}
	f, _ := errs[0].(map[string]any)
	return f["field"]
}

// TestIntercompanyBillingRace: two requests billing one goods issue at
// once bill it once; the other is refused as billed, by the check or the
// unique key.
func TestIntercompanyBillingRace(t *testing.T) {
	e := setupBilling(t)
	o := e.issued(t)
	body := e.bill(t, o["id"], e.sister2000, nil)
	got := make([]int, 2)
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() {
			got[i] = e.API.Do(apitest.Req{Method: "POST", Path: icBillings, Body: body, Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}).Code
		})
	}
	wg.Wait()
	sort.Ints(got)
	if got[0] != 201 || got[1] != 409 {
		t.Errorf("statuses: got %v, want [201 409]", got)
	}
}

// TestIntercompanyBillingPages: the list pages by billing date and by
// number.
func TestIntercompanyBillingPages(t *testing.T) {
	e := setupBilling(t)
	for range 2 {
		e.Must(201, "POST", icBillings, e.bill(t, e.issued(t)["id"], e.sister2000, nil), "")
	}
	for _, order := range []string{"", "&order_by=document_number"} {
		first, _ := e.Must(200, "GET", icBillings+"?page_size=1"+order, "", "")
		tok, _ := first["next_page_token"].(string)
		if n := len(rows(t, first, "items")); n != 1 || tok == "" {
			t.Fatalf("page 1 %s: got %d and token %q, want 1 and a token", order, n, tok)
		}
		second, _ := e.Must(200, "GET", icBillings+"?page_size=1"+order+"&page_token="+tok, "", "")
		if n := len(rows(t, second, "items")); n != 1 {
			t.Errorf("page 2 %s: got %d, want 1", order, n)
		}
	}
	e.Problem(400, "request.invalid_page_token", "GET", icBillings+"?page_token=nonsense", "", "")
}

// TestIntercompanyBillingDatabaseFailures: every statement of billing,
// reading and listing intercompany billings fails cleanly.
func TestIntercompanyBillingDatabaseFailures(t *testing.T) {
	e := setupBilling(t)
	pages, err := pagination.NewCodec([]byte("pppppppppppppppppppppppppppppppp"))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Sales(app.Config{Pages: pages})
	// OJ's emirate defaults from P3's region: its code is read too.
	pgtest.Exec(t, `UPDATE tenancy.org_unit SET region = 'AE-SH', version = version + 1 WHERE id = $1`, e.p3)
	o := e.issued(t)
	po := uuid.MustParse(o["id"].(string))
	billed := e.issued(t)
	b, _ := e.Must(201, "POST", icBillings, e.bill(t, billed["id"], e.sister2000, map[string]any{"tax_code": "OJ", "tax_jurisdiction": nil}), "")
	id := uuid.MustParse(b["id"].(string))
	calls := map[string]func(ctx context.Context) error{
		"bill": func(ctx context.Context) error {
			_, err := h.CreateIntercompanyBilling(ctx, &gen.IntercompanyBillingCreate{PurchaseOrderID: po, CustomerID: e.sister2000,
				Items: []gen.IntercompanyBillingItemInput{{PoItemNo: 1, AccountID: e.revenue, TaxCode: gen.NewOptString("OJ")}}},
				gen.CreateIntercompanyBillingParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetIntercompanyBilling(ctx, gen.GetIntercompanyBillingParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListIntercompanyBillings(ctx, gen.ListIntercompanyBillingsParams{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	if _, err := (procurement.Orders{}).Issued(context.Background(), po); err == nil {
		t.Error("procurement's read outside a unit of work: got nil error, want an error")
	}
}
