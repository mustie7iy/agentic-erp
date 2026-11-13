package extract_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/parquet-go/parquet-go"

	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// What a reader sees of the snapshot datasets.
type (
	poRow struct {
		OrderID     uuid.UUID `parquet:"order_id,uuid"`
		Company     string    `parquet:"company"`
		Vendor      string    `parquet:"vendor"`
		Account     *string   `parquet:"account,optional"`
		AmountMinor int64     `parquet:"amount_minor"`
		Currency    string    `parquet:"currency"`
	}
	invoiceRow struct {
		InvoiceID     uuid.UUID `parquet:"invoice_id,uuid"`
		Vendor        string    `parquet:"vendor"`
		InvoiceStatus string    `parquet:"invoice_status"`
		AmountMinor   int64     `parquet:"amount_minor"`
	}
	openRow struct {
		DocumentID uuid.UUID `parquet:"document_id,uuid"`
		LineNo     int32     `parquet:"line_no"`
	}
	accountRow struct {
		Company     string `parquet:"company"`
		Account     string `parquet:"account"`
		VendorRecon bool   `parquet:"vendor_recon"`
	}
	partnerRow struct {
		Vendor   string `parquet:"vendor"`
		Supplier bool   `parquet:"supplier"`
		Customer bool   `parquet:"customer"`
	}
	salesRow struct {
		DocumentID    uuid.UUID `parquet:"document_id,uuid"`
		ItemNo        int32     `parquet:"item_no"`
		SalesDocument string    `parquet:"sales_document"`
		Company       string    `parquet:"company"`
		DocumentDate  int32     `parquet:"document_date,date"`
		Customer      string    `parquet:"customer"`
		CustomerName  string    `parquet:"customer_name"`
		ItemKey       *string   `parquet:"item_key,optional"`
		AmountMinor   int64     `parquet:"amount_minor"`
		Currency      string    `parquet:"currency"`
		Exponent      int32     `parquet:"exponent"`
	}
)

// spendRow is what a reader sees of a spend_lines file.
type spendRow struct {
	InvoiceID     uuid.UUID `parquet:"invoice_id,uuid"`
	ItemNo        int32     `parquet:"item_no"`
	Company       string    `parquet:"company"`
	PostingDate   int32     `parquet:"posting_date,date"`
	Vendor        string    `parquet:"vendor"`
	Account       *string   `parquet:"account,optional"`
	CostCenter    *string   `parquet:"cost_center,optional"`
	PurchaseOrder *string   `parquet:"purchase_order,optional"`
	Contract      *string   `parquet:"contract,optional"`
	OnContract    bool      `parquet:"on_contract"`
	AmountMinor   int64     `parquet:"amount_minor"`
	Currency      string    `parquet:"currency"`
	Exponent      int32     `parquet:"exponent"`
}

// String is the row as the tests compare it: its invoice by name, its
// order by the name of its number, "-" for what it does not name.
func (r spendRow) String(names map[uuid.UUID]string, orders map[string]string) string {
	opt := func(v *string) string {
		if v == nil {
			return "-"
		}
		return *v
	}
	return fmt.Sprintf("%s/%d %s %s %s %s cc=%s order=%s contract=%s %v %d %s e%d", names[r.InvoiceID], r.ItemNo, r.Company,
		time.Unix(int64(r.PostingDate)*86400, 0).UTC().Format(time.DateOnly), r.Vendor, opt(r.Account), opt(r.CostCenter), cmp.Or(orders[opt(r.PurchaseOrder)], "-"), opt(r.Contract),
		r.OnContract, r.AmountMinor, r.Currency, r.Exponent)
}

// TestSpendLines: the items of posted supplier invoices, credit memos and
// subsequent credits by posting date are the spend lines, one file per
// month, a credit's amount negative; an item on a purchase order takes the
// order item's account and cost center and is under the order's contract;
// an item without an order, or on an order made on no contract, is off
// contract; an item removed from the document has no row, and one on an
// item its order no longer has keeps the order's contract and names no
// account; a parked or reversed document has no row, and a posted one
// loses its rows when it is reversed (T91b).
func TestSpendLines(t *testing.T) {
	e := setup(t)
	center := e.CostCenter(t, e.DE, "1000")
	contract := e.Contract(t, e.DE, e.Supplier, "EUR", "CT-0001")
	office, paper := e.Account(t, "2000", "520000"), e.Account(t, "2000", "610000")
	onContract := e.ContractOrder(t, e.DE, e.Supplier, &contract, "EUR", "2026-03-01", paper, &center)
	free := e.ContractOrder(t, e.DE, e.Supplier, nil, "EUR", "2026-03-01", paper, nil)
	inv := e.SupplierDocument(t, e.DE, e.Supplier, "invoice", "posted", "EUR", "2026-03-06",
		fixture.InvoiceItem{Net: 10000, Order: &onContract}, fixture.InvoiceItem{Net: 4000, Account: &office, CostCenter: &center},
		fixture.InvoiceItem{Net: 77700, Account: &office, Removed: true}, fixture.InvoiceItem{Net: 800, Order: &onContract, OrderItem: 20})
	credit := e.SupplierDocument(t, e.DE, e.Supplier, "credit_memo", "posted", "EUR", "2026-03-07", fixture.InvoiceItem{Net: 1500, Account: &office})
	later := e.SupplierDocument(t, e.DE, e.Supplier, "subsequent_credit", "posted", "EUR", "2026-04-01", fixture.InvoiceItem{Net: 500, Order: &free})
	parked := e.SupplierDocument(t, e.DE, e.Supplier, "invoice", "parked", "EUR", "2026-03-08", fixture.InvoiceItem{Net: 99900, Account: &office})
	reversed := e.SupplierDocument(t, e.DE, e.Supplier, "invoice", "reversed", "EUR", "2026-03-09", fixture.InvoiceItem{Net: 88800, Account: &office})
	names := map[uuid.UUID]string{inv: "invoice", credit: "credit", later: "later", parked: "parked", reversed: "reversed"}
	orders := map[string]string{}
	for id, name := range map[uuid.UUID]string{onContract: "on contract", free: "free"} {
		var number string
		pgtest.Scan(t, `SELECT document_number FROM procurement.purchase_order WHERE tenant_id = $1 AND id = $2`, []any{e.Tenant.ID, id}, &number)
		orders[number] = name
	}
	spend := func(m extract.Manifest) (rows, months []string) {
		for _, r := range read[spendRow](t, e, m, "spend_lines") {
			rows = append(rows, r.String(names, orders))
		}
		for _, f := range m.Datasets["spend_lines"].Files {
			_, month, _ := strings.Cut(f.Key, "/spend_lines/month=")
			months = append(months, month[:7])
		}
		return rows, months
	}
	rows, months := spend(e.manifest(t, e.run(t).ManifestKey))
	want := []string{
		"invoice/1 2000 2026-03-06 BP90000001 610000 cc=1000 order=on contract contract=CT-0001 true 10000 EUR e2",
		"invoice/2 2000 2026-03-06 BP90000001 520000 cc=1000 order=- contract=- false 4000 EUR e2",
		"invoice/4 2000 2026-03-06 BP90000001 - cc=- order=on contract contract=CT-0001 true 800 EUR e2",
		"credit/1 2000 2026-03-07 BP90000001 520000 cc=- order=- contract=- false -1500 EUR e2",
		"later/1 2000 2026-04-01 BP90000001 610000 cc=- order=free contract=- false -500 EUR e2",
	}
	if !slices.Equal(rows, want) {
		t.Errorf("spend lines:\n got %q\nwant %q", rows, want)
	}
	if !slices.Equal(months, []string{"2026-03", "2026-04"}) {
		t.Errorf("files: got months %v, want one for March and one for April", months)
	}

	t.Run("reversed", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE payables.supplier_invoice SET status = 'reversed', reversal_entry_year = 2026, reversal_entry_id = gen_random_uuid(),
			version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, inv)
		rows, months := spend(e.manifest(t, e.run(t).ManifestKey))
		if !slices.Equal(rows, want[3:]) {
			t.Errorf("spend lines:\n got %q\nwant %q", rows, want[3:])
		}
		if !slices.Equal(months, []string{"2026-03", "2026-04"}) {
			t.Errorf("files: got months %v, want March's rewritten and April's kept", months)
		}
	})
}

// String is the row as the tests compare it: its document by name.
func (r salesRow) String(names map[uuid.UUID]string) string {
	key := "-"
	if r.ItemKey != nil {
		key = *r.ItemKey
	}
	return fmt.Sprintf("%s/%d %s %s %s %s %s %s %d %s e%d", names[r.DocumentID], r.ItemNo, r.SalesDocument, r.Company,
		time.Unix(int64(r.DocumentDate)*86400, 0).UTC().Format(time.DateOnly), r.Customer, r.CustomerName, key, r.AmountMinor, r.Currency, r.Exponent)
}

// salesRows are the manifest's sales lines in file order, and the months
// of its files.
func salesRows(t *testing.T, e env, m extract.Manifest, names map[uuid.UUID]string) (rows, months []string) {
	t.Helper()
	for _, r := range read[salesRow](t, e, m, "sales_lines") {
		rows = append(rows, r.String(names))
	}
	for _, f := range m.Datasets["sales_lines"].Files {
		_, month, _ := strings.Cut(f.Key, "/sales_lines/month=")
		months = append(months, month[:7])
	}
	return rows, months
}

// TestSalesLines: the items of confirmed sales orders by order date and of
// posted billing documents by billing date are the sales lines, one file
// per month, each at its net amount naming its customer and item key; a
// draft order and a billing document not posted have no row until they
// are confirmed and posted; a renamed customer rewrites the rows (T91a).
func TestSalesLines(t *testing.T) {
	e := setup(t)
	customer := e.Customer(t, "BP90000007", "Northwind")
	order := e.SalesOrder(t, e.DE, customer, "EUR", "2026-03-10", "confirmed", fixture.SalesItem{Key: "BRACKET", Net: 150000}, fixture.SalesItem{Net: 23753})
	draft := e.SalesOrder(t, e.DE, customer, "EUR", "2026-03-11", "draft", fixture.SalesItem{Key: "DRAFT", Net: 999})
	yen := e.SalesOrder(t, e.DE, customer, "JPY", "2026-04-02", "confirmed", fixture.SalesItem{Key: "BRACKET", Net: 5000})
	posted := e.BillingDocument(t, order, "2026-04-01", true)
	created := e.BillingDocument(t, yen, "2026-04-03", false)
	names := map[uuid.UUID]string{order: "order", draft: "draft", yen: "yen", posted: "posted", created: "created"}
	first := e.manifest(t, e.run(t).ManifestKey)
	rows, months := salesRows(t, e, first, names)
	want := []string{
		"order/1 order 2000 2026-03-10 BP90000007 Northwind BRACKET 150000 EUR e2",
		"order/2 order 2000 2026-03-10 BP90000007 Northwind - 23753 EUR e2",
		"posted/1 billing 2000 2026-04-01 BP90000007 Northwind BRACKET 150000 EUR e2",
		"posted/2 billing 2000 2026-04-01 BP90000007 Northwind - 23753 EUR e2",
		"yen/1 order 2000 2026-04-02 BP90000007 Northwind BRACKET 5000 JPY e0",
	}
	if !slices.Equal(rows, want) {
		t.Errorf("sales lines:\n got %q\nwant %q", rows, want)
	}
	if !slices.Equal(months, []string{"2026-03", "2026-04"}) {
		t.Errorf("files: got months %v, want one for March and one for April", months)
	}

	t.Run("unchanged", func(t *testing.T) {
		if next := e.run(t); next.Published {
			t.Errorf("second run: got %+v, want nothing new", next)
		}
	})
	t.Run("confirmed and renamed", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE sales.sales_order SET status = 'confirmed', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, draft)
		pgtest.Exec(t, `UPDATE partner.business_partner SET name = 'Northwind Traders', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, customer)
		next := e.manifest(t, e.run(t).ManifestKey)
		rows, _ := salesRows(t, e, next, names)
		want := []string{
			"order/1 order 2000 2026-03-10 BP90000007 Northwind Traders BRACKET 150000 EUR e2",
			"order/2 order 2000 2026-03-10 BP90000007 Northwind Traders - 23753 EUR e2",
			"draft/1 order 2000 2026-03-11 BP90000007 Northwind Traders DRAFT 999 EUR e2",
			"posted/1 billing 2000 2026-04-01 BP90000007 Northwind Traders BRACKET 150000 EUR e2",
			"posted/2 billing 2000 2026-04-01 BP90000007 Northwind Traders - 23753 EUR e2",
			"yen/1 order 2000 2026-04-02 BP90000007 Northwind Traders BRACKET 5000 JPY e0",
		}
		if !slices.Equal(rows, want) {
			t.Errorf("sales lines:\n got %q\nwant %q", rows, want)
		}
	})
}

// read reads every file of a dataset of the manifest.
func read[T any](t *testing.T, e env, m extract.Manifest, dataset string) []T {
	t.Helper()
	var out []T
	for _, f := range m.Datasets[dataset].Files {
		o, ok := e.server.Object(f.Key)
		if !ok {
			t.Fatalf("file %s not stored", f.Key)
		}
		rows, err := parquet.Read[T](bytes.NewReader(o.Content), int64(len(o.Content)))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rows...)
	}
	return out
}

// order inserts a purchase order of company 2000 with one item.
func (e env) order(t *testing.T, date, quantity string, price int64) uuid.UUID {
	t.Helper()
	po := uuid.New()
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, 'EUR', $6, 'NT30', 'draft', $7, $7)`,
		e.Tenant.ID, po, po.String()[:10], e.DE, e.Supplier, date, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price, account_id)
		VALUES ($1, $2, 10, 'Paper', $3::numeric, 'EA', $4, $5)`, e.Tenant.ID, po, quantity, price, e.Account(t, "2000", "610000"))
	// A removed item, which the API does not return.
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price, account_id, is_removed)
		VALUES ($1, $2, 20, 'Toner', 1, 'EA', 5000, $3, true)`, e.Tenant.ID, po, e.Account(t, "2000", "610000"))
	return po
}

// invoice inserts a parked supplier invoice of company 2000.
func (e env) invoice(t *testing.T, date string, gross int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	pgtest.Exec(t, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference,
		invoice_date, currency, gross_amount, tax_amount, status, source, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $3, $6, 'EUR', $7, 0, 'parked', 'manual', $8, $8)`,
		e.Tenant.ID, id, id.String()[:10], e.DE, e.Supplier, date, gross, e.Tenant.Human.ID)
	return id
}

// TestSnapshotDatasets: documents and master data are read whole in the
// journal's transaction, and a file whose content did not change is kept.
func TestSnapshotDatasets(t *testing.T) {
	e := setup(t)
	entry := e.post(t, "2026-03-02", "100.00")
	po := e.order(t, "2026-03-05", "2.5", 199)
	inv := e.invoice(t, "2026-04-01", 11900)
	e.Customer(t, "BP90000009", "Customer")
	first := e.run(t)
	m := e.manifest(t, first.ManifestKey)

	want, _ := money.Scale(199, "2.5", 1) // 497.5 rounded half away from zero
	pos := read[poRow](t, e, m, "po_lines")
	if len(pos) != 1 {
		t.Fatalf("po_lines: got %d rows, want the order's one item that is not removed", len(pos))
	}
	if p := pos[0]; p.OrderID != po || p.Company != "2000" || p.Vendor != "BP90000001" {
		t.Errorf("po_lines keys: got order %s of %s for %s, want %s of 2000 for BP90000001", p.OrderID, p.Company, p.Vendor, po)
	}
	if p := pos[0]; p.Account == nil || *p.Account != "610000" {
		t.Errorf("po_lines account: got %v, want 610000", p.Account)
	}
	if p := pos[0]; p.AmountMinor != int64(want) || p.Currency != "EUR" {
		t.Errorf("po_lines value: got %d %s, want %d EUR", p.AmountMinor, p.Currency, want)
	}
	if got := read[invoiceRow](t, e, m, "ap_invoices"); len(got) != 1 || got[0].InvoiceID != inv || got[0].InvoiceStatus != "parked" || got[0].AmountMinor != 11900 {
		t.Errorf("ap_invoices: got %+v, want invoice %s parked at 11900", got, inv)
	}
	if got := read[openRow](t, e, m, "open_items"); len(got) != 1 || got[0].DocumentID.String() != entry || got[0].LineNo != 2 {
		t.Errorf("open_items: got %+v, want line 2 of entry %s", got, entry)
	}
	partners := map[string]partnerRow{}
	for _, p := range read[partnerRow](t, e, m, "partners") {
		partners[p.Vendor] = p
	}
	for number, want := range map[string][2]bool{"BP90000001": {true, false}, "BP90000002": {true, false}, "BP90000009": {false, true}} {
		if p, ok := partners[number]; !ok || p.Supplier != want[0] || p.Customer != want[1] {
			t.Errorf("partner %s: got %+v (present %v), want supplier %v customer %v", number, p, ok, want[0], want[1])
		}
	}
	recon := map[string]bool{}
	for _, a := range read[accountRow](t, e, m, "accounts") {
		recon[a.Company+"/"+a.Account] = a.VendorRecon
	}
	if r, ok := recon["2000/211000"]; !ok || !r || recon["2000/610000"] {
		t.Errorf("accounts: got %v, want 2000/211000 a supplier reconciliation account and 2000/610000 not", recon)
	}
	if n := len(m.Datasets["po_lines"].Files) + len(m.Datasets["ap_invoices"].Files); n != 2 {
		t.Errorf("fact files: got %d, want one per dataset and month", n)
	}

	t.Run("changed invoice", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE payables.supplier_invoice SET status = 'rejected', version = version + 1 WHERE tenant_id = $1 AND id = $2`, e.Tenant.ID, inv)
		next := e.run(t)
		if !next.Published || next.Lines != 0 {
			t.Fatalf("run: got %+v, want a publication without journal lines", next)
		}
		n := e.manifest(t, next.ManifestKey)
		if got := read[invoiceRow](t, e, n, "ap_invoices"); len(got) != 1 || got[0].InvoiceStatus != "rejected" {
			t.Errorf("ap_invoices: got %+v, want the invoice rejected", got)
		}
		if a, b := m.Datasets["ap_invoices"].Files[0].Key, n.Datasets["ap_invoices"].Files[0].Key; a == b {
			t.Errorf("ap_invoices file: got %s again, want a new file", a)
		}
		if a, b := m.Datasets["po_lines"].Files, n.Datasets["po_lines"].Files; len(b) != 1 || a[0] != b[0] {
			t.Errorf("po_lines files: got %v, want %v kept", b, a)
		}
	})
}

// TestOpenItemsOfTheLeadingLedger: with a parallel ledger, a posting's
// supplier line is copied into it, and the open items stay the leading
// ledger's, as /v1/open-items returns them (ADR-0045).
func TestOpenItemsOfTheLeadingLedger(t *testing.T) {
	e := setup(t)
	doc := fixture.Document(t)
	doc.Ledgers = []config.Ledger{{Code: "0L", Name: "Leading ledger", AccountingPrinciple: "IFRS"}, {Code: "2L", Name: "HGB", AccountingPrinciple: "HGB"}}
	doc.Company("2000").Ledgers = []string{"0L", "2L"}
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND status = 'closed' AND period <= 12`, e.Tenant.ID)
	entry := e.post(t, "2026-03-02", "100.00")
	var copies int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1 AND posting_group_id = $2`, []any{e.Tenant.ID, entry}, &copies)
	if copies != 2 {
		t.Fatalf("entries of the posting: got %d, want one per ledger", copies)
	}
	m := e.manifest(t, e.run(t).ManifestKey)
	if got := read[openRow](t, e, m, "open_items"); len(got) != 1 || got[0].DocumentID.String() != entry {
		t.Errorf("open_items: got %+v, want only the leading ledger's line of entry %s", got, entry)
	}
}

// TestBacklogInMonths: a first run over several months writes one file per
// month, each holding only its month's lines.
func TestBacklogInMonths(t *testing.T) {
	e := setup(t)
	months := []string{"2026-01", "2026-02", "2026-03", "2026-04"}
	for _, m := range months {
		e.postOnly(t, m+"-10", "3.00")
	}
	e.settled(t)
	res := e.run(t)
	files := e.manifest(t, res.ManifestKey).Datasets["gl_lines"].Files
	if len(files) != len(months) || res.Lines != 2*len(months) {
		t.Fatalf("gl_lines: got %d files of %d lines, want %d files of %d", len(files), res.Lines, len(months), 2*len(months))
	}
	for i, f := range files {
		if want := "/gl_lines/month=" + months[i] + "/"; !strings.Contains(f.Key, want) || f.Rows != 2 {
			t.Errorf("file %d: got %s with %d rows, want under %s with 2", i, f.Key, f.Rows, want)
		}
	}
}

// TestValueOutOfRange: an order item whose value overflows stops the run
// rather than writing a wrong amount.
func TestValueOutOfRange(t *testing.T) {
	e := setup(t)
	e.order(t, "2026-03-05", "10", 9_000_000_000_000_000_000)
	_, err := e.x.Run(context.Background(), e.Tenant.ID)
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("run: got %v, want ErrOverflow", err)
	}
}

// TestPendingEntries: entries posted above a run's cutoff (an open
// transaction holds the snapshot horizon) wait for a later run, and the
// pointer does not record the store current until they are in it.
func TestPendingEntries(t *testing.T) {
	e := setup(t)
	e.post(t, "2026-03-02", "1.00")
	e.run(t)
	checked := e.checkedAt(t)

	ctx := context.Background()
	hold, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `SELECT pg_current_xact_id()`); err != nil {
		t.Fatal(err)
	}
	entry := e.postOnly(t, "2026-03-03", "2.00")
	res := e.run(t)
	if !res.Pending || res.Lines != 0 {
		t.Errorf("run behind an open transaction: got %+v, want entry %s pending", res, entry)
	}
	if got := e.checkedAt(t); !got.Equal(checked) {
		t.Errorf("checked_at: got %v, want %v kept", got, checked)
	}
	if again := e.run(t); again.Published || !again.Pending {
		t.Errorf("second run behind the transaction: got %+v, want nothing new and the entry pending", again)
	}
	if got := e.checkedAt(t); !got.Equal(checked) {
		t.Errorf("checked_at after the second run: got %v, want %v kept", got, checked)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	e.settled(t)
	res = e.run(t)
	if res.Pending || res.Lines != 2 {
		t.Errorf("run after the transaction ended: got %+v, want the 2 lines and nothing pending", res)
	}
	if got := e.checkedAt(t); !got.After(checked) {
		t.Errorf("checked_at: got %v, want after %v", got, checked)
	}
}
