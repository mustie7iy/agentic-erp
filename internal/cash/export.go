package cash

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
)

// The treasury export (ADR-0051 §14, decision P8, T53h): a snapshot as a
// treasury system takes it, in the cash export schema v1
// (api/schemas/cash-export.v1.json), a published contract. A row is what
// the snapshot kept of one currency, day (within its 31 days at document
// level) or bucket (beyond), house bank, source, certainty and liquidity
// item, summed: the forecast per item, bucket, source, certainty and
// currency, and per bank and day, §14's content. It names no document,
// supplier or payment, so what a treasury system receives needs none of
// the read permissions the snapshot's lines show those by; a bank is named
// by its code and the last four characters of its account (§15). The rows
// are in a fixed order and the snapshot never changes, so the same
// snapshot gives the same file, byte for byte, named by its company, date
// and content hash: a resend is the same file under the same name.

// ExportSchemaVersion is the version of the cash export schema the files
// are written in.
const ExportSchemaVersion = 1

// Export formats: JSON lines, one object a row, and CSV with a header of
// the same columns (RFC 4180).
const (
	FormatJSONL = "jsonl"
	FormatCSV   = "csv"
)

// Granularities of an export row.
const (
	granularityDay    = "day"
	granularityBucket = "bucket"
)

// exportColumns are the columns of the schema, in order: a JSON line's
// keys and the CSV's header.
var exportColumns = []string{"schema_version", "company_code", "snapshot_id", "snapshot_date", "snapshot_kind", "taken_at", "currency", "granularity",
	"period_start", "period_end", "house_bank_code", "account_last4", "source", "certainty", "liquidity_item", "amount", "flow_count"}

// BankAccount is a house bank as the export names it.
type BankAccount struct {
	Code, Last4 string
}

// exportRow is a row of the export, its fields in exportColumns' order;
// nil is JSON's null and CSV's empty field.
type exportRow struct {
	SchemaVersion int       `json:"schema_version"`
	CompanyCode   string    `json:"company_code"`
	SnapshotID    uuid.UUID `json:"snapshot_id"`
	SnapshotDate  string    `json:"snapshot_date"`
	SnapshotKind  string    `json:"snapshot_kind"`
	TakenAt       string    `json:"taken_at"`
	Currency      string    `json:"currency"`
	Granularity   string    `json:"granularity"`
	PeriodStart   string    `json:"period_start"`
	PeriodEnd     string    `json:"period_end"`
	HouseBankCode *string   `json:"house_bank_code"`
	AccountLast4  *string   `json:"account_last4"`
	Source        string    `json:"source"`
	Certainty     string    `json:"certainty"`
	LiquidityItem *string   `json:"liquidity_item"`
	Amount        string    `json:"amount"`
	FlowCount     int       `json:"flow_count"`
}

func (r exportRow) fields() []string {
	text := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	return []string{strconv.Itoa(r.SchemaVersion), r.CompanyCode, r.SnapshotID.String(), r.SnapshotDate, r.SnapshotKind, r.TakenAt, r.Currency, r.Granularity,
		r.PeriodStart, r.PeriodEnd, text(r.HouseBankCode), text(r.AccountLast4), r.Source, r.Certainty, text(r.LiquidityItem), r.Amount, strconv.Itoa(r.FlowCount)}
}

// exportFile is a file of the export: its name, content and hash.
type exportFile struct {
	company string
	name    string
	content []byte
	sha256  []byte
}

// renderExport writes the rows in the format and names the file
// cash-<company>-<snapshot date>-<sha256 of the content>.<format>.
func renderExport(company string, date time.Time, format string, rows []exportRow) (exportFile, error) {
	var b bytes.Buffer
	switch format {
	case FormatJSONL:
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		for _, r := range rows {
			_ = enc.Encode(r) // a row of strings, numbers and nulls always encodes, and a bytes.Buffer takes every write
		}
	case FormatCSV:
		w := csv.NewWriter(&b)
		w.UseCRLF = true
		_ = w.Write(exportColumns) // a bytes.Buffer takes every write, so the writer never fails
		for _, r := range rows {
			_ = w.Write(r.fields()) // as above
		}
		w.Flush()
	default:
		return exportFile{}, fmt.Errorf("cash: export format %q is not jsonl or csv", format)
	}
	sum := sha256.Sum256(b.Bytes())
	return exportFile{company: company, name: "cash-" + company + "-" + date.Format(time.DateOnly) + "-" + hex.EncodeToString(sum[:]) + "." + format,
		content: b.Bytes(), sha256: sum[:]}, nil
}

// exportRows are the snapshot's lines summed per currency, day or bucket,
// house bank, source, certainty and liquidity item, in the schema's order:
// by currency, period, house bank code (none first), source, certainty and
// item (none first). A day's flow_count counts the flows with a line in
// the row once, a bucket's the flows its lines summed.
func (e *Endpoints) exportRows(ctx context.Context, tx pgx.Tx, s Snapshot) ([]exportRow, error) {
	rows, _ := tx.Query(ctx, `SELECT f.currency, c.exponent, f.value_date, f.bucket_end, f.house_bank_id, f.source, f.certainty, f.liquidity_item,
			sum(f.amount)::bigint, CASE WHEN f.bucket_end IS NULL THEN count(DISTINCT f.flow_no) ELSE sum(f.flow_count) END
		FROM cash.snapshot_flow f JOIN ref.currency c ON c.code = f.currency
		WHERE f.snapshot_id = $1
		GROUP BY f.currency, c.exponent, f.value_date, f.bucket_end, f.house_bank_id, f.source, f.certainty, f.liquidity_item`, s.ID)
	type summed struct {
		currency          string
		exp               int
		start             time.Time
		end               *time.Time
		bank              *uuid.UUID
		source, certainty string
		item              *string
		amount            money.Minor
		count             int
	}
	sums, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (summed, error) {
		var x summed
		return x, r.Scan(&x.currency, &x.exp, &x.start, &x.end, &x.bank, &x.source, &x.certainty, &x.item, &x.amount, &x.count)
	})
	if err != nil {
		return nil, fmt.Errorf("cash: export rows of snapshot %s: %w", s.ID, err)
	}
	accounts, err := idref.Resolve(ctx, e.Payments.HouseBankAccounts, idref.Of(sums, func(x summed) *uuid.UUID { return x.bank }), houseBankRef)
	if err != nil {
		return nil, fmt.Errorf("cash: house banks of snapshot %s: %w", s.ID, err)
	}
	out := make([]exportRow, len(sums))
	for i, x := range sums {
		r := exportRow{SchemaVersion: ExportSchemaVersion, CompanyCode: s.CompanyCode, SnapshotID: s.ID, SnapshotDate: s.Date.Format(time.DateOnly),
			SnapshotKind: s.Kind, TakenAt: s.TakenAt.UTC().Format(time.RFC3339Nano), Currency: x.currency, Granularity: granularityDay,
			PeriodStart: x.start.Format(time.DateOnly), PeriodEnd: x.start.Format(time.DateOnly), Source: x.source, Certainty: x.certainty,
			LiquidityItem: x.item, Amount: x.amount.Plain(x.exp), FlowCount: x.count}
		if x.end != nil {
			r.Granularity, r.PeriodEnd = granularityBucket, x.end.Format(time.DateOnly)
		}
		if x.bank != nil {
			a := accounts[*x.bank]
			r.HouseBankCode, r.AccountLast4 = &a.Code, &a.Last4
		}
		out[i] = r
	}
	slices.SortFunc(out, compareRows)
	return out, nil
}

// compareRows orders the rows as the schema does.
func compareRows(a, b exportRow) int {
	text := func(s *string) string { // none sorts first
		if s == nil {
			return ""
		}
		return "\x00" + *s
	}
	return cmp.Or(cmp.Compare(a.Currency, b.Currency), cmp.Compare(a.PeriodStart, b.PeriodStart), cmp.Compare(a.PeriodEnd, b.PeriodEnd),
		cmp.Compare(text(a.HouseBankCode), text(b.HouseBankCode)), cmp.Compare(a.Source, b.Source), cmp.Compare(a.Certainty, b.Certainty),
		cmp.Compare(text(a.LiquidityItem), text(b.LiquidityItem)))
}
