package extract_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/parquet-go/parquet-go"

	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// taxRow is what a reader sees of a gl_lines file's tax columns.
type taxRow struct {
	DocumentID      uuid.UUID `parquet:"document_id,uuid"`
	LineNo          int32     `parquet:"line_no"`
	Account         string    `parquet:"account"`
	TaxJurisdiction *string   `parquet:"tax_jurisdiction,optional"`
	TaxBaseMinor    *int64    `parquet:"tax_base_minor,optional"`
}

// olderRow is a gl_lines row as the extractor wrote it before the tax
// columns.
type olderRow struct {
	TenantID     uuid.UUID `parquet:"tenant_id,uuid"`
	DocumentID   uuid.UUID `parquet:"document_id,uuid"`
	LineNo       int32     `parquet:"line_no"`
	LedgerID     uuid.UUID `parquet:"ledger_id,uuid"`
	Company      string    `parquet:"company"`
	PostingDate  int32     `parquet:"posting_date,date"`
	Account      string    `parquet:"account"`
	DocumentType string    `parquet:"document_type"`
	Vendor       *string   `parquet:"vendor,optional"`
	AmountMinor  int64     `parquet:"amount_minor"`
	Currency     string    `parquet:"currency"`
	Exponent     int32     `parquet:"exponent"`
	PostedXID    uint64    `parquet:"posted_xid"`
}

// taxCodes gives company 2000 an output tax account 240000, S5, the UAE's
// standard rate levied in the emirate each line names, and IM5, the UAE's
// import VAT accounted for in the return, its base declared (ADR-0049 §7,
// §9).
func (e env) taxCodes(t *testing.T) {
	t.Helper()
	doc := fixture.Document(t)
	co := doc.Company("2000")
	co.ChartOfAccounts = append(co.ChartOfAccounts, config.Account{Number: "240000", Name: "Output tax", Type: config.Liability})
	five := []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}
	fixture.TaxCodes(t, e.Tenant, doc,
		fixture.UAEStandard("240000"),
		config.TaxCode{Code: "IM5", Description: "UAE import VAT", Kind: config.TaxImport, Components: []config.TaxComponent{
			{Key: "due", AccountKey: config.KeySelfOutput, Account: "240000", Rates: five},
			{Key: "claim", AccountKey: config.KeySelfInput, Account: "140000", Rates: five}}})
}

// taxEntries posts a sale in Dubai and an import's VAT from its customs
// declaration, EUR 1,000.00 declared, in company 2000 on 2 March.
func (e env) taxEntries(t *testing.T) (sale, imported string) {
	t.Helper()
	line := func(account, amount, extra string) string {
		return fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}%s}`, e.Account(t, "2000", account), amount, extra)
	}
	post := func(lines ...string) string {
		je, _ := e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-02", "posting_date": "2026-03-02",
			"currency": "EUR", "lines": [%s]}`, e.DE, strings.Join(lines, ", ")), "")
		id, _ := je["id"].(string)
		return id
	}
	dubai, declared := `, "tax_code": "S5", "tax_jurisdiction": "AE-DU"`, `, "tax_code": "IM5", "tax_base": {"amount": "1000.00", "currency": "EUR"}`
	sale = post(line("100000", "105.00", ""), line("400000", "-100.00", dubai), line("240000", "-5.00", dubai))
	imported = post(line("140000", "50.00", declared), line("240000", "-50.00", declared))
	e.settled(t)
	return sale, imported
}

// taxLines renders rows as "account jurisdiction base", "-" where null,
// those of entries alone, in file order.
func taxLines(rows []taxRow, entries ...string) []string {
	var out []string
	for _, r := range rows {
		if !slices.Contains(entries, r.DocumentID.String()) {
			continue
		}
		j, b := "-", "-"
		if r.TaxJurisdiction != nil {
			j = *r.TaxJurisdiction
		}
		if r.TaxBaseMinor != nil {
			b = fmt.Sprint(*r.TaxBaseMinor)
		}
		out = append(out, r.Account+" "+j+" "+b)
	}
	return out
}

func (e env) taxRows(t *testing.T, key string) []taxRow {
	t.Helper()
	o, ok := e.server.Object(key)
	if !ok {
		t.Fatalf("file %s not stored", key)
	}
	rows, err := parquet.Read[taxRow](bytes.NewReader(o.Content), int64(len(o.Content)))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestTaxColumns: a journal line's tax jurisdiction and its declared tax
// base in local currency are extracted as the journal API shows them, null
// where the line has none (ADR-0049 §7, §9; ADR-0044 As built).
func TestTaxColumns(t *testing.T) {
	e := setup(t)
	e.taxCodes(t)
	sale, imported := e.taxEntries(t)
	res := e.run(t)
	gl := e.manifest(t, res.ManifestKey).Datasets["gl_lines"]
	if len(gl.Files) != 1 {
		t.Fatalf("gl_lines: got %d files, want 1", len(gl.Files))
	}
	got := taxLines(e.taxRows(t, gl.Files[0].Key), sale, imported)
	want := []string{"100000 - -", "400000 AE-DU -", "240000 AE-DU -", "140000 - 100000", "240000 - 100000"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("tax columns: got %v, want %v", got, want)
	}
}

// TestJournalExtractedAgain: a journal whose files were written before
// the tax columns existed (its manifest naming no columns) is extracted
// again from the start by the next run: every line, those of the older
// files with their tax columns filled from the database, in files of the
// columns the extractor writes; the older files leave the manifest. The
// run after it appends again.
func TestJournalExtractedAgain(t *testing.T) {
	e := setup(t)
	e.taxCodes(t)
	sale, imported := e.taxEntries(t)
	res := e.run(t)
	// The pointer moves to a manifest listing the same lines as a part of
	// the older schema, naming no columns, as a manifest before them did.
	m := e.manifest(t, res.ManifestKey)
	gl := m.Datasets["gl_lines"]
	o, ok := e.server.Object(gl.Files[0].Key)
	if !ok {
		t.Fatalf("file %s not stored", gl.Files[0].Key)
	}
	older, err := parquet.Read[olderRow](bytes.NewReader(o.Content), int64(len(o.Content)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := parquet.Write(&buf, older); err != nil {
		t.Fatal(err)
	}
	key := strings.Replace(gl.Files[0].Key, "/part-", "/part-older-", 1)
	sum := sha256.Sum256(buf.Bytes())
	gl.Files, gl.Columns = []extract.File{{Key: key, Size: int64(buf.Len()), SHA256: hex.EncodeToString(sum[:]), Rows: int64(len(older))}}, nil
	m.Datasets["gl_lines"] = gl
	if err := e.server.Store().Put(context.Background(), key, buf.Bytes(), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	e.repoint(t, res, m)
	april := e.post(t, "2026-04-01", "1.00")
	again := e.run(t)
	if again.Lines != 7 {
		t.Errorf("lines: got %d, want the journal's 7 extracted again", again.Lines)
	}
	got := e.manifest(t, again.ManifestKey).Datasets["gl_lines"]
	if len(got.Files) != 2 || got.Rows != 7 || len(got.Columns) == 0 {
		t.Fatalf("gl_lines: got %+v, want a file per month, 7 rows and the columns named", got)
	}
	var rows []taxRow
	for _, f := range got.Files {
		if f.Key == key {
			t.Errorf("files: got the older part %s, want it gone from the manifest", key)
		}
		rows = append(rows, e.taxRows(t, f.Key)...)
	}
	want := []string{"100000 - -", "400000 AE-DU -", "240000 AE-DU -", "140000 - 100000", "240000 - 100000", "610000 - -", "211000 - -"}
	if got := taxLines(rows, sale, imported, april); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("tax columns: got %v, want %v", got, want)
	}
	e.post(t, "2026-04-02", "2.00")
	if next := e.run(t); next.Lines != 2 {
		t.Errorf("the next run: got %d lines, want the new entry's 2 appended", next.Lines)
	}
}

// repoint moves the tenant's pointer to m, published under a manifest key
// of res's watermark, as a run before it would have left it.
func (e env) repoint(t *testing.T, res extract.Result, m extract.Manifest) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	manifest := extract.ManifestKey(e.Tenant.ID, res.Watermark, "0123456789abcdef")
	if err := e.server.Store().Put(context.Background(), manifest, b, blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `UPDATE analytics.snapshot SET manifest_key = $2 WHERE tenant_id = $1`, e.Tenant.ID, manifest)
}
