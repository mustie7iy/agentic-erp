package extract_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

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
)

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

// customer inserts a business partner in the customer role alone.
func (e env) customer(t *testing.T, number string) {
	t.Helper()
	id := uuid.New()
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'organization', 'Customer', 'DE', 'active', $4, $4)`, e.Tenant.ID, id, number, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'customer', $3)`, e.Tenant.ID, id, e.Tenant.Human.ID)
}

// TestSnapshotDatasets: documents and master data are read whole in the
// journal's transaction, and a file whose content did not change is kept.
func TestSnapshotDatasets(t *testing.T) {
	e := setup(t)
	entry := e.post(t, "2026-03-02", "100.00")
	po := e.order(t, "2026-03-05", "2.5", 199)
	inv := e.invoice(t, "2026-04-01", 11900)
	e.customer(t, "BP90000009")
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
