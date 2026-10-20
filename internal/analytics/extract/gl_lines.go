package extract

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
	months := batcher[glLine]{flush: func(month string, lines []glLine) error {
		var err error
		ds.Files, err = r.appendMonth(ctx, ds.Files, month, lines)
		ds.Rows, r.lines = ds.Rows+int64(len(lines)), r.lines+len(lines)
		return err
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

// CompactAt is how many journal parts a month gathers before a run merges
// them: each run that posts to a month adds a small part, and a reader
// opens every one.
const CompactAt = 8

func (x Extractor) compactAt() int {
	if x.CompactAt <= 0 {
		return CompactAt
	}
	return x.CompactAt
}

// appendMonth adds a month's new lines to the journal's files: as a part
// named by the run's interval, or, once the month would hold CompactAt
// parts, merged with the month's parts into one file named by the interval
// they cover together, from the first part's start to the run's cutoff.
// Every line of the month in that interval is in those parts (a run that
// wrote no part for the month had no line of it), so the name stays exact
// and a repeated run writes the same bytes under it. The parts it replaces
// leave the manifest and are collected with it (Collect).
func (r *run) appendMonth(ctx context.Context, files []File, month string, lines []glLine) ([]File, error) {
	dir := r.dir(glLinesDataset, month)
	var parts, rest []File
	for _, f := range files {
		if strings.HasPrefix(f.Key, dir) {
			parts = append(parts, f)
		} else {
			rest = append(rest, f)
		}
	}
	from := seq(r.prev.watermark)
	if len(parts)+1 < r.x.compactAt() {
		f, err := put(ctx, r.x, r.key(glLinesDataset, month, "part-"+from+"-"+seq(r.cutoff)), lines)
		return append(files, f), err
	}
	merged := slices.Clone(lines)
	for _, p := range parts {
		got, err := r.x.readPart(ctx, p)
		if err != nil {
			return files, err
		}
		merged = append(merged, got...)
	}
	slices.SortFunc(merged, func(a, b glLine) int {
		return cmp.Or(cmp.Compare(a.PostingDate, b.PostingDate), bytes.Compare(a.DocumentID[:], b.DocumentID[:]), cmp.Compare(a.LineNo, b.LineNo))
	})
	from, _, _ = strings.Cut(strings.TrimPrefix(parts[0].Key, dir+"part-"), "-") // parts[0] is the month's oldest: files are listed in run order
	f, err := put(ctx, r.x, r.key(glLinesDataset, month, "part-"+from+"-"+seq(r.cutoff)), merged)
	return append(rest, f), err
}

// readPart reads a journal part back, refusing one whose content is not
// the manifest's.
func (x Extractor) readPart(ctx context.Context, f File) ([]glLine, error) {
	b, _, err := x.Store.Get(ctx, f.Key)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Key, err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != f.SHA256 {
		return nil, fmt.Errorf("%s is not the file the manifest lists", f.Key)
	}
	lines, err := parquet.Read[glLine](bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Key, err)
	}
	return lines, nil
}
