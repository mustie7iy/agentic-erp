package assets_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// withOrderExpenses is withNextYear (2026 and 2027) with company 2000 valuing receipts
// through GR/IR (219000, price differences on 590000, which takes no
// cost object, so the toner's price difference needs no cost center) and
// the tax code B0: input tax of 5%, none of it
// recovered, borne by the item.
func withOrderExpenses(doc *config.Config) {
	withNextYear(doc)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "219000", Name: "GR/IR clearing", Type: config.Liability},
		config.Account{Number: "590000", Name: "Price differences", Type: config.Expense, CostElement: config.CostElementNone})
	de.AccountDetermination.GRIR, de.AccountDetermination.PriceDifference = "219000", "590000"
	de.TaxCodes = append(de.TaxCodes, config.TaxCode{Code: "B0", Description: "Blocked", Components: []config.TaxComponent{{Key: "tax",
		AccountKey: config.KeyInput, Account: "140000", RecoverablePercent: "0", Rates: []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}}}})
}

// expensesEnv is an assetEnv whose people post invoices and receipts on
// purchase orders of company 2000.
type expensesEnv struct{ assetEnv }

func newOrderExpenses(t *testing.T) expensesEnv {
	t.Helper()
	return expensesEnv{newAssetsWith(t, withOrderExpenses, append(postPermissions, "payables.invoice.review", "procurement.receipt.post",
		"procurement.receipt.read")...)}
}

// order is a released purchase order of company 2000 valued through
// GR/IR, fully received on 2026-03-05: item 1, 10 EA at 12.00 on the
// asset clearing account 169000, and item 2, 5 EA at 3.00 on the expense
// account 520000.
func (e expensesEnv) order(t *testing.T, number string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, 'EUR', '2026-03-01', 'NT30', 'draft', $6, $6)`,
		e.Tenant.ID, id, number, e.DE, e.Supplier, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
		price_unit, account_id, tax_code, valuation_price, valuation_price_unit)
		VALUES ($1, $2, 1, 'Milling machine', 10, 'EA', 1200, 1, $3, 'V1', 1200, 1), ($1, $2, 2, 'Toner', 5, 'EA', 300, 1, $4, 'V1', 300, 1)`,
		e.Tenant.ID, id, e.Account(t, "2000", "169000"), e.Account(t, "2000", "520000"))
	pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = 'released', valuation = 'gr_ir', version = version + 1 WHERE id = $1`, id)
	e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-05", "posting_date": "2026-03-05",
		"items": [{"purchase_order_id": %[2]q, "po_item_no": 1, "quantity": {"value": "10", "unit": "EA"}},
			{"purchase_order_id": %[2]q, "po_item_no": 2, "quantity": {"value": "5", "unit": "EA"}}]}`, e.DE, id), "")
	return id
}

// orderItem is an invoice item on an order item with a tax code.
func orderItem(po uuid.UUID, item int, qty, amount, code string) string {
	return fmt.Sprintf(`{"purchase_order_id": %q, "po_item_no": %d, "quantity": {"value": %q, "unit": "EA"}, "tax_code": %q,
		"amount": {"amount": %q, "currency": "EUR"}}`, po, item, qty, code, amount)
}

// document enters, matches and posts a supplier document of the kind on
// 2026-03-21 and returns its id. An asset's order item's difference goes
// to its asset clearing account (product owner 2026-11-10): nothing of it
// is expensed.
func (e expensesEnv) document(t *testing.T, kind, ref, gross, tax string, items ...string) string {
	t.Helper()
	inv, tag := e.Must(201, "POST", "/v1/supplier-invoices", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "supplier_reference": %q,
		"invoice_date": "2026-03-20", "kind": %q, "gross_amount": {"amount": %q, "currency": "EUR"}, "tax_amount": {"amount": %q, "currency": "EUR"},
		"items": [%s]}`, e.DE, e.Supplier, ref, kind, gross, tax, strings.Join(items, ", ")), "")
	id := inv["id"].(string)
	m, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id+":match", "", tag)
	if m["is_clean"] != true {
		t.Fatalf("match of %s: got issues %v, want clean", ref, m["issues"])
	}
	_, tag = e.Must(200, "GET", "/v1/supplier-invoices/"+id, "", "")
	e.Must(200, "POST", "/v1/supplier-invoices/"+id+":post", `{"posting_date": "2026-03-21"}`, tag)
	return id
}

// earlierItem is an order item of an earlier posting: one piece of it
// cleared on the account (default the GR/IR account 219000) for 10.00,
// with diff on the price difference account; recorded: the line is
// recorded with the order item and tax of it as the tax the item bore.
type earlierItem struct {
	item     int32
	diff     int64
	account  string
	recorded bool
	tax      int64
}

// expensed is an order item of a posting made while the ledger recorded
// order items on price difference lines and still expensed an asset's
// item's difference there (before the product owner's decision of
// 2026-11-10): diff on the price difference account, tax of it the tax
// the item bore.
func expensed(item int32, diff, tax int64) earlierItem {
	return earlierItem{item: item, diff: diff, recorded: true, tax: tax}
}

// earlier posts, as an invoice, credit memo or reversal did before an
// asset's item's difference went to its asset clearing account, an entry
// of company 2000 dated 10 February of the year in the leading ledger:
// per item Dr its account 10.00 naming the order item and Dr price
// differences its diff, then Cr the supplier; the price difference lines
// recorded with their order item (expensed) or, as before the ledger
// recorded them, not.
func (e expensesEnv) earlier(t *testing.T, year int, po uuid.UUID, items ...earlierItem) {
	t.Helper()
	je := uuid.Must(uuid.NewV7())
	var nos []int32
	var diffs []int64
	var accounts []uuid.UUID
	total := int64(0)
	for _, it := range items {
		account := it.account
		if account == "" {
			account = "219000"
		}
		nos, diffs, accounts = append(nos, it.item), append(diffs, it.diff), append(accounts, e.Account(t, "2000", account))
		total += 1000 + it.diff
	}
	day := fmt.Sprintf("%d-02-10", year)
	// One statement: an entry and its lines are checked together.
	pgtest.Exec(t, `WITH entry AS (INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period,
			document_type, document_number, document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc,
			source_context, source_document_id, posted_by)
		VALUES ($1, $14, $2, $3, $4, 2, 'KR', $12, $15::date, $15::date, 'EUR', 'EUR', 'EUR', 1, 1, 'payables', $2, $13))
		INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period,
			posting_date, account_id, amount_tc, amount_lc, amount_gc, partner_id, purchase_order_id, po_item_no, quantity, uom_code)
		SELECT $1, $14, $2, 2 * x.n - 1, $3, $4, 2, $15::date, x.account, 1000::bigint, 1000::bigint, 1000::bigint, NULL::uuid, $5::uuid, x.item, 1::numeric, 'EA'
			FROM unnest($6::int[], $7::bigint[], $8::uuid[]) WITH ORDINALITY AS x(item, diff, account, n)
		UNION ALL SELECT $1, $14, $2, 2 * x.n, $3, $4, 2, $15::date, $9, x.diff, x.diff, x.diff, NULL, NULL, NULL, NULL, NULL
			FROM unnest($6::int[], $7::bigint[], $8::uuid[]) WITH ORDINALITY AS x(item, diff, account, n)
		UNION ALL SELECT $1, $14, $2, 2 * cardinality($6::int[]) + 1, $3, $4, 2, $15::date, $10, -$16::bigint, -$16::bigint, -$16::bigint, $11::uuid, NULL, NULL,
			NULL, NULL`,
		e.Tenant.ID, je, e.DE, e.l0, po, nos, diffs, accounts, e.Account(t, "2000", "590000"), e.Account(t, "2000", "211000"), e.Supplier,
		je.String()[24:], e.Tenant.Human.ID, year, day, total)
	for n, it := range items {
		if it.recorded {
			pgtest.Exec(t, `INSERT INTO ledger.price_difference_item (tenant_id, fiscal_year, journal_entry_id, line_no, purchase_order_id, po_item_no,
				tax_borne_tc, tax_borne_lc) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`, e.Tenant.ID, year, je, 2*(n+1), po, it.item, it.tax)
		}
	}
}

// expenses reads the report of company 2000 and shows it as one line:
// each row, the totals and what is unattributed.
func (e expensesEnv) expenses(t *testing.T, query string) (string, map[string]any) {
	t.Helper()
	r, _ := e.Must(200, "GET", "/v1/asset-order-expenses?legal_entity_id="+e.DE.String()+query, "", "")
	amounts := func(m map[string]any) string {
		return fmt.Sprintf("tax %s + difference %s = %s", moneyOf(m["tax_not_recovered"]), moneyOf(m["price_difference"]), moneyOf(m["expensed"]))
	}
	var parts []string
	for _, it := range r["items"].([]any) {
		m := it.(map[string]any)
		parts = append(parts, fmt.Sprintf("%v/%v %v on %v %v: %s", m["purchase_order_number"], m["po_item_no"], m["description"], m["account_number"],
			m["account_name"], amounts(m)))
	}
	u := r["unattributed"].(map[string]any)
	parts = append(parts, "totals: "+amounts(r["totals"].(map[string]any)), fmt.Sprintf("unattributed: %s (debits %s, credits %s) in %v entries",
		moneyOf(u["expensed"]), moneyOf(u["debits"]), moneyOf(u["credits"]), u["entries"]))
	return strings.Join(parts, "; "), r
}

// TestAssetOrderExpenses: the report shows, per asset order item, the tax
// not recovered and the price difference its invoices and credit memos
// expensed, exactly as recorded at posting, with the totals on every
// page; an item on another account is left out; earlier postings without
// a recorded order item are stated as unattributed, their debits and
// credits apart, when their entry clears GR/IR for an asset order item; a
// reversal nets out; a posting counts in its own fiscal year alone (B10).
// It is history: a document posted now puts an asset's item's difference
// on its asset clearing account, recorded as before, and shows nothing
// (product owner 2026-11-10).
func TestAssetOrderExpenses(t *testing.T) {
	e := newOrderExpenses(t)
	const none = "unattributed: 0.00 EUR (debits 0.00 EUR, credits 0.00 EUR) in 0 entries"
	const nothing = "totals: tax 0.00 EUR + difference 0.00 EUR = 0.00 EUR; " + none
	got, head := e.expenses(t, "&fiscal_year=2026")
	want(t, "nothing posted", got, nothing)
	want(t, "the head", fmt.Sprint(head["company_code"], " ", head["company_name"] != "", " ", head["fiscal_year"], " ", head["start_date"], " ",
		head["end_date"], " ", head["currency"], " ", head["next_page_token"]), "2000 true 2026 2026-01-01 2026-12-31 EUR <nil>")
	a, b := e.order(t, "PO-A"), e.order(t, "PO-B")
	// PO-A invoiced now: the machine at 12.50 for 12.00 with 6.25 of
	// blocked tax, capitalised on 169000 and recorded there; the toner at
	// 3.20 for 3.00, its tax recovered, expensed on an expense account's
	// item. Neither is an expense on an asset order item.
	e.document(t, "invoice", "INV-A", "150.29", "9.29", orderItem(a, 1, "10", "125.00", "B0"), orderItem(a, 2, "5", "16.00", "V1"))
	var capitalised string
	pgtest.Scan(t, `SELECT string_agg(a.number || ' ' || l.amount_lc || ' tax ' || i.tax_borne_lc, '; ' ORDER BY a.number)
		FROM ledger.price_difference_item i
		JOIN ledger.journal_line l ON l.fiscal_year = i.fiscal_year AND l.journal_entry_id = i.journal_entry_id AND l.line_no = i.line_no
		JOIN ledger.gl_account a ON a.id = l.account_id WHERE i.purchase_order_id = $1`, []any{a}, &capitalised)
	want(t, "recorded for the invoice posted now", capitalised, "169000 1125 tax 625; 590000 100 tax 0")
	got, _ = e.expenses(t, "&fiscal_year=2026")
	want(t, "an invoice posted now", got, nothing)
	// The same invoice as it posted before: 11.25 on price differences.
	e.earlier(t, 2026, a, expensed(1, 1125, 625))
	const invoiced = "PO-A/1 Milling machine on 169000 Asset clearing: tax 6.25 EUR + difference 5.00 EUR = 11.25 EUR"
	got, _ = e.expenses(t, "&fiscal_year=2026")
	want(t, "an invoice", got, invoiced+"; totals: tax 6.25 EUR + difference 5.00 EUR = 11.25 EUR; "+none)
	// A credit memo of two machines at 12.50 gave 1.00 and 1.25 of tax back.
	e.earlier(t, 2026, a, expensed(1, -225, -125))
	const credited = "PO-A/1 Milling machine on 169000 Asset clearing: tax 5.00 EUR + difference 4.00 EUR = 9.00 EUR"
	got, _ = e.expenses(t, "&fiscal_year=2026")
	want(t, "a credit memo", got, credited+"; totals: tax 5.00 EUR + difference 4.00 EUR = 9.00 EUR; "+none)
	e.earlier(t, 2026, a, expensed(1, 225, 125)) // its reversal
	got, _ = e.expenses(t, "&fiscal_year=2026")
	want(t, "the credit memo reversed", got, invoiced+"; totals: tax 6.25 EUR + difference 5.00 EUR = 11.25 EUR; "+none)
	// PO-B: the machine below the order price, its tax recovered.
	e.earlier(t, 2026, b, expensed(1, -500, 0))
	// Earlier postings: one on PO-B's machine, 7.00 over, and its toner,
	// 3.00 under (the net understates the machine's share; the debits
	// bound it); one on PO-A's toner alone (no asset order item: not
	// counted); one on an order unknown now; one naming PO-A's machine on
	// a line that is not on the GR/IR account (not an invoice clearing
	// GR/IR for it: not counted).
	e.earlier(t, 2026, b, earlierItem{item: 1, diff: 700}, earlierItem{item: 2, diff: -300})
	e.earlier(t, 2026, a, earlierItem{item: 2, diff: 300})
	e.earlier(t, 2026, uuid.New(), earlierItem{item: 1, diff: 900})
	e.earlier(t, 2026, a, earlierItem{item: 1, diff: 1100, account: "520000"})
	// The next fiscal year's postings are its own: a third order's machine
	// invoiced 3.00 over in 2027, and an earlier posting dated 2027.
	c := e.order(t, "PO-C")
	e.earlier(t, 2027, c, expensed(1, 300, 0))
	e.earlier(t, 2027, c, earlierItem{item: 1, diff: 400})
	got, _ = e.expenses(t, "&fiscal_year=2027")
	want(t, "the next year", got, "PO-C/1 Milling machine on 169000 Asset clearing: tax 0.00 EUR + difference 3.00 EUR = 3.00 EUR; "+
		"totals: tax 0.00 EUR + difference 3.00 EUR = 3.00 EUR; unattributed: 4.00 EUR (debits 4.00 EUR, credits 0.00 EUR) in 1 entries")
	const second = "PO-B/1 Milling machine on 169000 Asset clearing: tax 0.00 EUR + difference -5.00 EUR = -5.00 EUR"
	const totals = "totals: tax 6.25 EUR + difference 0.00 EUR = 6.25 EUR; unattributed: 4.00 EUR (debits 7.00 EUR, credits -3.00 EUR) in 1 entries"
	got, _ = e.expenses(t, "&fiscal_year=2026")
	want(t, "two orders and earlier postings", got, invoiced+"; "+second+"; "+totals)
	got, page := e.expenses(t, "&fiscal_year=2026&page_size=1")
	want(t, "the first page", got, invoiced+"; "+totals)
	got, page = e.expenses(t, "&fiscal_year=2026&page_size=1&page_token="+page["next_page_token"].(string))
	want(t, "the second page", got, second+"; "+totals)
	want(t, "the last page's token", page["next_page_token"], nil)
	us, _ := e.Must(200, "GET", "/v1/asset-order-expenses?fiscal_year=2026&legal_entity_id="+e.US.String(), "", "")
	want(t, "a company without a price difference account", fmt.Sprint(len(us["items"].([]any)), " ", moneyOf(us["totals"].(map[string]any)["expensed"]), " ",
		moneyOf(us["unattributed"].(map[string]any)["expensed"])), "0 0.00 USD 0.00 USD")
}

// TestAssetOrderExpensesRefused: a fiscal year not set up, a company the
// caller cannot act in, a page size out of range and a page token not
// issued for the request; a token naming the last row gives an empty
// page with the totals.
func TestAssetOrderExpensesRefused(t *testing.T) {
	e := newOrderExpenses(t)
	po := e.order(t, "PO-A")
	e.earlier(t, 2026, po, expensed(1, 1125, 625))
	path := "/v1/asset-order-expenses?legal_entity_id=" + e.DE.String()
	e.Problem(422, "ledger.no_fiscal_period", "GET", path+"&fiscal_year=2030", "", "")
	e.Problem(400, "request.invalid_page_token", "GET", path+"&fiscal_year=2026&page_token=nonsense", "", "")
	reader := e.Client
	reader.As = e.Reader(t, "reader", e.US)
	reader.Problem(404, "request.not_found", "GET", path+"&fiscal_year=2026", "", "")
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Assets(app.Config{Pages: pages})
	// A token of the same request naming a place after every row.
	tok, err := pages.Encode(fmt.Sprintf("getAssetOrderExpenses|%s|%d", e.DE, 2026), map[string]any{"n": "PO-Z", "o": uuid.New(), "i": 1})
	if err != nil {
		t.Fatal(err)
	}
	var res gen.GetAssetOrderExpensesRes
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		res, err = h.GetAssetOrderExpenses(ctx, gen.GetAssetOrderExpensesParams{LegalEntityID: e.DE, FiscalYear: 2026, PageToken: gen.NewOptString(tok)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The contract refuses a page size of 0 before the handler; called
	// directly, the handler refuses it itself.
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.GetAssetOrderExpenses(ctx, gen.GetAssetOrderExpensesParams{LegalEntityID: e.DE, FiscalYear: 2026, PageSize: gen.NewOptInt32(0)})
		return err
	})
	pgtest.WantErr(t, err, "request.invalid_page_size")
	r := res.(*gen.AssetOrderExpenses)
	want(t, "a page after the last row", fmt.Sprint(len(r.Items), " ", r.Totals.Expensed.Amount, " ", r.NextPageToken.Set), "0 11.25 false")
}

// lostPurchases fails the read named.
type lostPurchases struct {
	assets.Purchases
	fails string
}

var errLost = errors.New("purchases lost")

func (l lostPurchases) Recorded(ctx context.Context, le uuid.UUID, year int16) ([]assets.OrderItemExpense, error) {
	if l.fails == "recorded" {
		return nil, errLost
	}
	return l.Purchases.Recorded(ctx, le, year)
}

func (l lostPurchases) Unrecorded(ctx context.Context, le uuid.UUID, year int16) ([]assets.UnrecordedExpense, error) {
	if l.fails == "unrecorded" {
		return nil, errLost
	}
	return l.Purchases.Unrecorded(ctx, le, year)
}

func (l lostPurchases) Items(ctx context.Context, orders []uuid.UUID) ([]assets.OrderItem, error) {
	if l.fails == "items" {
		return nil, errLost
	}
	return l.Purchases.Items(ctx, orders)
}

// TestAssetOrderExpensesFailures: every statement of the report reports
// its failure, as do its reads of the other contexts and of the
// configuration, and neither it nor those reads run outside a unit of
// work.
func TestAssetOrderExpensesFailures(t *testing.T) {
	e := newOrderExpenses(t)
	po := e.order(t, "PO-A")
	e.earlier(t, 2026, po, expensed(1, 1125, 625))
	e.earlier(t, 2026, po, earlierItem{item: 1, diff: 700})
	h := app.Assets(app.Config{})
	params := gen.GetAssetOrderExpensesParams{LegalEntityID: e.DE, FiscalYear: 2026}
	report := func(ctx context.Context) error {
		_, err := h.GetAssetOrderExpenses(ctx, params)
		return err
	}
	for name, fn := range map[string]func(ctx context.Context) error{
		"the report": report,
		"the ledger's recorded price differences": func(ctx context.Context) error {
			_, err := ledger.AssetReads{}.OrderItemExpenses(ctx, e.DE, 2026)
			return err
		},
		"the ledger's unrecorded price differences": func(ctx context.Context) error {
			_, err := ledger.AssetReads{}.UnrecordedExpenses(ctx, e.DE, 2026)
			return err
		},
		"the orders' items": func(ctx context.Context) error {
			_, err := procurement.Orders{}.ItemAccounts(ctx, []uuid.UUID{po})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); err == nil || !strings.Contains(err.Error(), "outside a unit of work") {
				t.Errorf("outside a unit of work: got %v, want it refused", err)
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
	for _, fails := range []string{"recorded", "unrecorded", "items"} {
		t.Run("a lost read: "+fails, func(t *testing.T) {
			lost := *h
			lost.Purchases = lostPurchases{Purchases: h.Purchases, fails: fails}
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := lost.GetAssetOrderExpenses(ctx, params)
				return err
			})
			if !errors.Is(err, errLost) {
				t.Errorf("got %v, want the read's failure", err)
			}
		})
	}
	t.Run("a configuration that does not load", func(t *testing.T) {
		lost := *h
		lost.Settings = settingsLost{}
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := lost.GetAssetOrderExpenses(ctx, params)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "configuration lost") {
			t.Errorf("got %v, want it to fail the report", err)
		}
	})
}
