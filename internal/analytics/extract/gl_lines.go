package extract

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/parquet-go/parquet-go"

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

// readLines reads the journal lines above the previous watermark and below
// the cutoff, in posting order.
func readLines(ctx context.Context, tx pgx.Tx, prev pointer, cutoff uint64) ([]glLine, error) {
	sql, args := glLinesSQL+firstRun, []any{strconv.FormatUint(cutoff, 10)}
	if prev.set {
		sql, args = glLinesSQL+nextRun, []any{strconv.FormatUint(prev.watermark, 10), args[0]}
	}
	rows, _ := tx.Query(ctx, sql, args...)
	var out []glLine
	var l glLine
	var date time.Time
	var exponent int16
	_, err := pgx.ForEachRow(rows, []any{&l.TenantID, &l.DocumentID, &l.LineNo, &l.LedgerID, &l.Company, &date, &l.Account, &l.DocumentType,
		&l.Vendor, &l.AmountMinor, &l.Currency, &exponent, (*xid)(&l.PostedXID)}, func() error {
		l.PostingDate, l.Exponent = int32(date.Unix()/86400), int32(exponent) //nolint:gosec // a date's day number and a currency's exponent (0-4) fit int32
		out = append(out, l)
		l.Vendor = nil // the scan may reuse a non-nil pointer, which the appended row holds
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gl_lines: %w", err)
	}
	return out, nil
}

// byMonth splits lines into their posting months (YYYY-MM), the dataset's
// partitions, keeping their order.
func byMonth(lines []glLine) ([]string, map[string][]glLine) {
	var months []string
	parts := map[string][]glLine{}
	for _, l := range lines {
		m := time.Unix(int64(l.PostingDate)*86400, 0).UTC().Format("2006-01")
		if _, ok := parts[m]; !ok {
			months = append(months, m)
		}
		parts[m] = append(parts[m], l)
	}
	return months, parts
}

// encode writes lines as one Parquet file. The same lines always give the
// same bytes, so a run repeated with the same cutoff finds its files equal.
func encode(lines []glLine) []byte {
	var buf bytes.Buffer
	// The schema is fixed (TestColumnsAreTheContract) and a bytes.Buffer
	// never returns a write error, so the error, the writer's, cannot occur.
	_ = parquet.Write(&buf, lines)
	return buf.Bytes()
}
