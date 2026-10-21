package extract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/parquet-go/parquet-go"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The snapshot datasets (analytics/datasets/v1): documents that change after
// they are written and the dimensions joined at read time. Each row type's
// columns are its contract file's (TestColumnsAreTheContract).

type poLine struct {
	TenantID    uuid.UUID   `parquet:"tenant_id,uuid"`
	OrderID     uuid.UUID   `parquet:"order_id,uuid"`
	ItemNo      int32       `parquet:"item_no"`
	Company     string      `parquet:"company"`
	OrderDate   int32       `parquet:"order_date,date"`
	Vendor      string      `parquet:"vendor"`
	Account     *string     `parquet:"account,optional"`
	AmountMinor money.Minor `parquet:"amount_minor"`
	Currency    string      `parquet:"currency"`
	Exponent    int32       `parquet:"exponent"`
}

type apInvoice struct {
	TenantID      uuid.UUID   `parquet:"tenant_id,uuid"`
	InvoiceID     uuid.UUID   `parquet:"invoice_id,uuid"`
	Company       string      `parquet:"company"`
	InvoiceDate   int32       `parquet:"invoice_date,date"`
	Vendor        string      `parquet:"vendor"`
	InvoiceStatus string      `parquet:"invoice_status"`
	AmountMinor   money.Minor `parquet:"amount_minor"`
	Currency      string      `parquet:"currency"`
	Exponent      int32       `parquet:"exponent"`
}

type openItem struct {
	TenantID   uuid.UUID `parquet:"tenant_id,uuid"`
	DocumentID uuid.UUID `parquet:"document_id,uuid"`
	LineNo     int32     `parquet:"line_no"`
}

type accountRow struct {
	TenantID    uuid.UUID `parquet:"tenant_id,uuid"`
	Company     string    `parquet:"company"`
	Account     string    `parquet:"account"`
	AccountName string    `parquet:"account_name"`
	AccountType string    `parquet:"account_type"`
	VendorRecon bool      `parquet:"vendor_recon"`
}

// partnerRow is a business partner in any role: a journal line names a
// customer as its partner as well as a supplier, and BuildFacts gives it the
// partner's name either way.
type partnerRow struct {
	TenantID   uuid.UUID `parquet:"tenant_id,uuid"`
	Vendor     string    `parquet:"vendor"`
	VendorName string    `parquet:"vendor_name"`
	Supplier   bool      `parquet:"supplier"`
	Customer   bool      `parquet:"customer"`
}

// The snapshots' queries, ordered by month first for the fact datasets;
// row-level security limits each to the tenant. An order item is valued as
// the semantic layer values it (money.Scale of the decimal quantity); a
// removed item is not one the API returns.
const (
	poLinesSQL = `SELECT o.tenant_id, o.id, i.item_no, le.code, o.order_date, bp.number, a.number, i.unit_price, i.quantity::text, i.price_unit,
		o.currency, c.exponent
		FROM procurement.purchase_order o
		JOIN procurement.purchase_order_item i ON i.tenant_id = o.tenant_id AND i.purchase_order_id = o.id AND NOT i.is_removed
		JOIN tenancy.legal_entity le ON le.tenant_id = o.tenant_id AND le.id = o.legal_entity_id
		JOIN partner.business_partner bp ON bp.tenant_id = o.tenant_id AND bp.id = o.supplier_id
		LEFT JOIN ledger.gl_account a ON a.tenant_id = i.tenant_id AND a.id = i.account_id
		JOIN ref.currency c ON c.code = o.currency
		ORDER BY o.order_date, o.id, i.item_no`
	apInvoicesSQL = `SELECT i.tenant_id, i.id, le.code, i.invoice_date, bp.number, i.status, i.gross_amount, i.currency, c.exponent
		FROM payables.supplier_invoice i
		JOIN tenancy.legal_entity le ON le.tenant_id = i.tenant_id AND le.id = i.legal_entity_id
		JOIN partner.business_partner bp ON bp.tenant_id = i.tenant_id AND bp.id = i.supplier_id
		JOIN ref.currency c ON c.code = i.currency
		ORDER BY i.invoice_date, i.id`
	// An open supplier item is a line of the leading ledger on a supplier
	// reconciliation account that no clearing has taken, as /v1/open-items
	// returns it: open items are the leading ledger's, the parallel ledgers'
	// copies of them are not items of their own (ADR-0045).
	openItemsSQL = `SELECT l.tenant_id, l.journal_entry_id, l.line_no FROM ledger.journal_line l
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'supplier'
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		WHERE NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year
			AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)
		ORDER BY l.journal_entry_id, l.line_no`
	accountsSQL = `SELECT a.tenant_id, le.code, a.number, a.name, a.account_type, a.reconciliation_kind = 'supplier'
		FROM ledger.company_account ca
		JOIN ledger.gl_account a ON a.tenant_id = ca.tenant_id AND a.id = ca.account_id
		JOIN tenancy.legal_entity le ON le.tenant_id = ca.tenant_id AND le.id = ca.legal_entity_id
		ORDER BY le.code, a.number`
	// Every partner, as /v1/business-partners lists them, with its roles.
	partnersSQL = `SELECT bp.tenant_id, bp.number, bp.name,
		EXISTS (SELECT FROM partner.partner_role r WHERE r.tenant_id = bp.tenant_id AND r.partner_id = bp.id AND r.role = 'supplier'),
		EXISTS (SELECT FROM partner.partner_role r WHERE r.tenant_id = bp.tenant_id AND r.partner_id = bp.id AND r.role = 'customer')
		FROM partner.business_partner bp ORDER BY bp.number`
)

// snapshots reads every snapshot dataset.
func (r *run) snapshots(ctx context.Context, tx pgx.Tx) error {
	for _, read := range []func(context.Context, pgx.Tx) error{r.poLines, r.apInvoices, r.openItems, r.accounts, r.partners} {
		if err := read(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) poLines(ctx context.Context, tx pgx.Tx) error {
	var l poLine
	var date time.Time
	var price money.Minor
	var qty string
	var unit int64
	var exponent int16
	return snapshot(ctx, tx, r, "po_lines", poLinesSQL, []any{&l.TenantID, &l.OrderID, &l.ItemNo, &l.Company, &date, &l.Vendor, &l.Account,
		&price, &qty, &unit, &l.Currency, &exponent}, func() (poLine, string, error) {
		row := l
		l.Account = nil // the scan may reuse a non-nil pointer, which row holds
		amount, err := money.Scale(price, qty, unit)
		if err != nil {
			return row, "", fmt.Errorf("order %s item %d quantity %s: %w", row.OrderID, row.ItemNo, qty, err)
		}
		row.OrderDate, row.AmountMinor, row.Exponent = days(date), amount, int32(exponent)
		return row, date.Format(monthLayout), nil
	})
}

func (r *run) apInvoices(ctx context.Context, tx pgx.Tx) error {
	var v apInvoice
	var date time.Time
	var exponent int16
	return snapshot(ctx, tx, r, "ap_invoices", apInvoicesSQL, []any{&v.TenantID, &v.InvoiceID, &v.Company, &date, &v.Vendor, &v.InvoiceStatus,
		&v.AmountMinor, &v.Currency, &exponent}, func() (apInvoice, string, error) {
		v.InvoiceDate, v.Exponent = days(date), int32(exponent)
		return v, date.Format(monthLayout), nil
	})
}

func (r *run) openItems(ctx context.Context, tx pgx.Tx) error {
	var o openItem
	return snapshot(ctx, tx, r, "open_items", openItemsSQL, []any{&o.TenantID, &o.DocumentID, &o.LineNo},
		func() (openItem, string, error) { return o, "", nil })
}

func (r *run) accounts(ctx context.Context, tx pgx.Tx) error {
	var a accountRow
	return snapshot(ctx, tx, r, "accounts", accountsSQL, []any{&a.TenantID, &a.Company, &a.Account, &a.AccountName, &a.AccountType, &a.VendorRecon},
		func() (accountRow, string, error) { return a, "", nil })
}

func (r *run) partners(ctx context.Context, tx pgx.Tx) error {
	var v partnerRow
	return snapshot(ctx, tx, r, "partners", partnersSQL, []any{&v.TenantID, &v.Vendor, &v.VendorName, &v.Supplier, &v.Customer},
		func() (partnerRow, string, error) { return v, "", nil })
}

// snapshot reads a snapshot dataset into files named by their content, one
// per month of a fact dataset (row's month) and one for a dimension (""),
// keeping the previous manifest's file where the content is unchanged.
func snapshot[T any](ctx context.Context, tx pgx.Tx, r *run, name, sql string, targets []any, row func() (T, string, error)) error {
	kept := map[string]File{}
	for _, f := range r.prevM.Datasets[name].Files {
		kept[f.Key] = f
	}
	var ds Dataset
	months := batcher[T]{flush: func(month string, rows []T) error {
		b := encode(rows)
		sum := sha256.Sum256(b)
		key := r.key(name, month, "part-"+hex.EncodeToString(sum[:]))
		f, ok := kept[key]
		if !ok {
			var err error
			if f, err = store(ctx, r.x, key, b, len(rows)); err != nil {
				return err
			}
		}
		ds.Files, ds.Rows = append(ds.Files, f), ds.Rows+f.Rows
		return nil
	}}
	rows, _ := tx.Query(ctx, sql)
	_, err := pgx.ForEachRow(rows, targets, func() error {
		v, month, err := row()
		if err != nil {
			return err
		}
		return months.add(month, v)
	})
	if err == nil {
		err = months.close()
	}
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	r.m.add(name, ds, r)
	return nil
}

// batcher collects rows ordered by month and hands each month's rows to
// flush when the month ends, so one month is held in memory at a time.
type batcher[T any] struct {
	month string
	rows  []T
	flush func(month string, rows []T) error
}

func (b *batcher[T]) add(month string, row T) error {
	if len(b.rows) > 0 && month != b.month {
		if err := b.close(); err != nil {
			return err
		}
	}
	b.month, b.rows = month, append(b.rows, row)
	return nil
}

// close flushes the last month.
func (b *batcher[T]) close() error {
	if len(b.rows) == 0 {
		return nil
	}
	err := b.flush(b.month, b.rows)
	b.rows = b.rows[:0] // flush encoded them; the next month reuses the array
	return err
}

// monthLayout names a month partition (month=YYYY-MM).
const monthLayout = "2006-01"

// days is a date's Parquet DATE: days since 1970-01-01.
func days(date time.Time) int32 {
	return int32(date.Unix() / 86400) //nolint:gosec // a date's day number fits int32
}

// key is where a run stores a dataset's file: under its month for a fact
// dataset, directly under the dataset for a dimension.
func (r *run) key(dataset, month, name string) string {
	return r.dir(dataset, month) + name + ".parquet"
}

// dir is the directory of a dataset's files, of a month for a fact dataset.
func (r *run) dir(dataset, month string) string {
	dir := tenantPrefix(r.tenant) + "/" + dataset
	if month != "" {
		dir += "/month=" + month
	}
	return dir + "/"
}

// put stores rows as one Parquet file.
func put[T any](ctx context.Context, x Extractor, key string, rows []T) (File, error) {
	return store(ctx, x, key, encode(rows), len(rows))
}

// store writes a file once and describes it for the manifest.
func store(ctx context.Context, x Extractor, key string, b []byte, rows int) (File, error) {
	if err := x.putOnce(ctx, key, b, "application/vnd.apache.parquet"); err != nil {
		return File{}, err
	}
	sum := sha256.Sum256(b)
	return File{Key: key, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:]), Rows: int64(rows)}, nil
}

// encode writes rows as one Parquet file. The same rows always give the
// same bytes, so a file named by its content or its interval is written once.
func encode[T any](rows []T) []byte {
	var buf bytes.Buffer
	// The schemas are fixed (TestColumnsAreTheContract) and a bytes.Buffer
	// never returns a write error, so the error, the writer's, cannot occur.
	_ = parquet.Write(&buf, rows)
	return buf.Bytes()
}

// sameSnapshots reports whether two manifests list the same snapshot files:
// with no journal line appended, a run that read them found nothing new.
func sameSnapshots(a, b Manifest) bool {
	for name, d := range b.Datasets {
		prev, ok := a.Datasets[name]
		if name != glLinesDataset && (!ok || !slices.Equal(prev.Files, d.Files)) {
			return false
		}
	}
	return true
}
