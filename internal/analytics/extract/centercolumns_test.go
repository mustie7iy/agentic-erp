package extract_test

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/parquet-go/parquet-go"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// centerRow is what a reader sees of a gl_lines file's center columns.
type centerRow struct {
	DocumentID   uuid.UUID `parquet:"document_id,uuid"`
	Account      string    `parquet:"account"`
	CostCenter   *string   `parquet:"cost_center,optional"`
	ProfitCenter *string   `parquet:"profit_center,optional"`
	Segment      *string   `parquet:"segment,optional"`
}

// taxEraColumns are the columns the journal's files held with the tax
// columns and before the centers' (batch 95).
var taxEraColumns = []string{"tenant_id", "document_id", "line_no", "ledger_id", "company", "posting_date", "account", "document_type", "vendor",
	"amount_minor", "currency", "exponent", "posted_xid", "tax_jurisdiction", "tax_base_minor"}

// centerEntry gives company 2000 segment SEG1, profit center PC10 and cost
// center CC100 (fixture.Centers) and posts on 2 March an expense of 300.00
// on CC100, its profit center and segment derived from it, a sale of
// 1000.00 on PC10 and the bank's 700.00 on no center.
func (e env) centerEntry(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"ledger.cost_center.read", "ledger.cost_center.change"} { // what Centers needs, granted as CostCenters grants it
		pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
			SELECT tenant_id, gen_random_uuid(), id, $2, current_date - 1, $3 FROM identity.role WHERE tenant_id = $1 AND code = 'all'`,
			e.Tenant.ID, p, e.Tenant.Human.ID)
	}
	e.Controller(t)
	c := e.Centers(t)
	line := func(account, amount, extra string) string {
		return fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}%s}`, e.Account(t, "2000", account), amount, extra)
	}
	je, _ := e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-02", "posting_date": "2026-03-02",
		"currency": "EUR", "lines": [%s, %s, %s]}`, e.DE, line("610000", "300.00", fmt.Sprintf(`, "cost_center_id": %q`, c.Cost)),
		line("400000", "-1000.00", fmt.Sprintf(`, "profit_center_id": %q`, c.Profit)), line("100000", "700.00", "")), "")
	e.settled(t)
	id, _ := je["id"].(string)
	return id
}

// centerLines renders the entry's rows as "account cost profit segment",
// "-" where null, in file order.
func (e env) centerLines(t *testing.T, keys []string, entry string) []string {
	t.Helper()
	opt := func(s *string) string {
		if s == nil {
			return "-"
		}
		return *s
	}
	var out []string
	for _, key := range keys {
		o, ok := e.server.Object(key)
		if !ok {
			t.Fatalf("file %s not stored", key)
		}
		rows, err := parquet.Read[centerRow](bytes.NewReader(o.Content), int64(len(o.Content)))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.DocumentID.String() == entry {
				out = append(out, fmt.Sprintf("%s %s %s %s", r.Account, opt(r.CostCenter), opt(r.ProfitCenter), opt(r.Segment)))
			}
		}
	}
	return out
}

// TestCenterColumns: a journal line's cost center, profit center and
// segment are extracted by code as the journal API shows them, null where
// the line has none (ADR-0050 §5; ADR-0044 As built T56i1).
func TestCenterColumns(t *testing.T) {
	e := setup(t)
	entry := e.centerEntry(t)
	gl := e.manifest(t, e.run(t).ManifestKey).Datasets["gl_lines"]
	if !slices.Contains(gl.Columns, "cost_center") || !slices.Contains(gl.Columns, "segment") {
		t.Errorf("columns: got %v, want the centers' named", gl.Columns)
	}
	var keys []string
	for _, f := range gl.Files {
		keys = append(keys, f.Key)
	}
	want := []string{"610000 CC100 PC10 SEG1", "400000 - PC10 SEG1", "100000 - - -"}
	if got := e.centerLines(t, keys, entry); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("center columns: got %v, want %v", got, want)
	}
}

// TestJournalExtractedAgainForCenters: a journal whose manifest names the
// columns of batch 95, without the centers', is extracted again from the
// start by the next run, its lines with their centers, as a journal
// written before the tax columns is (TestJournalExtractedAgain).
func TestJournalExtractedAgainForCenters(t *testing.T) {
	e := setup(t)
	entry := e.centerEntry(t)
	res := e.run(t)
	m := e.manifest(t, res.ManifestKey)
	gl := m.Datasets["gl_lines"]
	gl.Columns = taxEraColumns
	m.Datasets["gl_lines"] = gl
	e.repoint(t, res, m)
	e.post(t, "2026-04-01", "1.00")
	again := e.run(t)
	if again.Lines != 5 {
		t.Errorf("lines: got %d, want the journal's 5 extracted again", again.Lines)
	}
	got := e.manifest(t, again.ManifestKey).Datasets["gl_lines"]
	if got.Rows != 5 || !slices.Contains(got.Columns, "profit_center") {
		t.Fatalf("gl_lines: got %+v, want 5 rows and the centers' columns named", got)
	}
	var keys []string
	for _, f := range got.Files {
		keys = append(keys, f.Key)
	}
	want := []string{"610000 CC100 PC10 SEG1", "400000 - PC10 SEG1", "100000 - - -"}
	if got := e.centerLines(t, keys, entry); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("center columns: got %v, want %v", got, want)
	}
}
