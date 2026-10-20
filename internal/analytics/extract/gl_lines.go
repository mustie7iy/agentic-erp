package extract

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// glLine is a row of dataset gl_lines v1 (analytics/datasets/v1/gl_lines.yaml):
// a posted journal line of any ledger, its amount in local currency.
type glLine struct {
	TenantID     uuid.UUID   `parquet:"tenant_id,uuid"`
	DocumentID   uuid.UUID   `parquet:"document_id,uuid"`
	LineNo       int32       `parquet:"line_no"`
	LedgerID     uuid.UUID   `parquet:"ledger_id,uuid"`
	Company      string      `parquet:"company"`
	PostingDate  int32       `parquet:"posting_date,date"` // days since 1970-01-01
	Account      string      `parquet:"account"`
	DocumentType string      `parquet:"document_type"`
	Vendor       *string     `parquet:"vendor,optional"`
	AmountMinor  money.Minor `parquet:"amount_minor"`
	Currency     string      `parquet:"currency"`
	Exponent     int32       `parquet:"exponent"`
	PostedXID    uint64      `parquet:"posted_xid"` // UINT64, compared unsigned; 0 for entries posted before the column existed
}

const glLinesDataset = "gl_lines"

// glLinesSQL reads the lines of a run's interval of entries; row-level
// security limits it to the tenant.
const glLinesSQL = `SELECT l.tenant_id, l.journal_entry_id, l.line_no, l.ledger_id, le.code, l.posting_date, a.number, e.document_type,
	bp.number, l.amount_lc, e.currency_lc, c.exponent, coalesce(e.posted_xid, '0'::xid8)::text
	FROM ledger.journal_entry e
	JOIN ledger.journal_line l ON l.tenant_id = e.tenant_id AND l.fiscal_year = e.fiscal_year AND l.journal_entry_id = e.id
	JOIN tenancy.legal_entity le ON le.tenant_id = l.tenant_id AND le.id = l.legal_entity_id
	JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
	JOIN ref.currency c ON c.code = e.currency_lc
	LEFT JOIN partner.business_partner bp ON bp.tenant_id = l.tenant_id AND bp.id = l.partner_id
	WHERE `

// The interval of a run: a plain range for the index (tenant_id,
// posted_xid); a first run also reads the entries posted before the column
// existed (NULL), which committed before any cutoff.
const (
	nextRun  = `e.posted_xid >= $1::text::xid8 AND e.posted_xid < $2::text::xid8 ORDER BY l.posting_date, l.journal_entry_id, l.line_no`
	firstRun = `(e.posted_xid IS NULL OR e.posted_xid < $1::text::xid8) ORDER BY l.posting_date, l.journal_entry_id, l.line_no`
)

// glLines appends the journal lines above the previous watermark and below
// the cutoff to the previous manifest's, one file per posting month, and
// notes whether entries at or above the cutoff wait for a later run.
func (r *run) glLines(ctx context.Context, tx pgx.Tx) error {
	sql, args := glLinesSQL+firstRun, []any{strconv.FormatUint(r.cutoff, 10)}
	if r.prev.set {
		sql, args = glLinesSQL+nextRun, []any{strconv.FormatUint(r.prev.watermark, 10), args[0]}
	}
	ds := r.prevM.Datasets[glLinesDataset]
	ds.Files = slices.Clone(ds.Files)
	name := "part-" + seq(r.prev.watermark) + "-" + seq(r.cutoff) // the interval it holds
	months := batcher[glLine]{flush: func(month string, lines []glLine) error {
		f, err := put(ctx, r.x, r.key(glLinesDataset, month, name), lines)
		if err != nil {
			return err
		}
		ds.Files, ds.Rows, r.lines = append(ds.Files, f), ds.Rows+f.Rows, r.lines+len(lines)
		return nil
	}}
	rows, _ := tx.Query(ctx, sql, args...)
	var l glLine
	var date time.Time
	var exponent int16
	_, err := pgx.ForEachRow(rows, []any{&l.TenantID, &l.DocumentID, &l.LineNo, &l.LedgerID, &l.Company, &date, &l.Account, &l.DocumentType,
		&l.Vendor, &l.AmountMinor, &l.Currency, &exponent, (*xid)(&l.PostedXID)}, func() error {
		l.PostingDate, l.Exponent = days(date), int32(exponent)
		err := months.add(date.Format(monthLayout), l)
		l.Vendor = nil // the scan may reuse a non-nil pointer, which the batched row holds
		return err
	})
	if err == nil {
		err = months.close()
	}
	if err != nil {
		return fmt.Errorf("gl_lines: %w", err)
	}
	r.m.add(glLinesDataset, ds, r)
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.journal_entry WHERE posted_xid >= $1::text::xid8)`, args[len(args)-1]).Scan(&r.pending); err != nil {
		return fmt.Errorf("gl_lines pending: %w", err)
	}
	return nil
}
