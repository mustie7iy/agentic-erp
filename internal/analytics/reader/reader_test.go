package reader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/parquet-go/parquet-go"

	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// glLine is a gl_lines row as the extractor writes it (the contract's
// columns), with a masked field the extractor never writes: a forged or
// careless file holding one must not leak it.
type glLine struct {
	TenantID     uuid.UUID   `parquet:"tenant_id,uuid"`
	DocumentID   uuid.UUID   `parquet:"document_id,uuid"`
	LineNo       int32       `parquet:"line_no"`
	LedgerID     uuid.UUID   `parquet:"ledger_id,uuid"`
	Company      string      `parquet:"company"`
	PostingDate  int32       `parquet:"posting_date,date"`
	Account      string      `parquet:"account"`
	DocumentType string      `parquet:"document_type"`
	Vendor       *string     `parquet:"vendor,optional"`
	AmountMinor  money.Minor `parquet:"amount_minor"`
	Currency     string      `parquet:"currency"`
	Exponent     int32       `parquet:"exponent"`
	PostedXID    uint64      `parquet:"posted_xid"`
	IBAN         string      `parquet:"iban"`
}

type accountRow struct {
	TenantID    uuid.UUID `parquet:"tenant_id,uuid"`
	Company     string    `parquet:"company"`
	Account     string    `parquet:"account"`
	AccountName string    `parquet:"account_name"`
	AccountType string    `parquet:"account_type"`
	VendorRecon bool      `parquet:"vendor_recon"`
	// Determination is "" (null) in these files: no plan here reads it.
	Determination string `parquet:"determination,optional"`
}

type partnerRow struct {
	TenantID   uuid.UUID `parquet:"tenant_id,uuid"`
	Vendor     string    `parquet:"vendor"`
	VendorName string    `parquet:"vendor_name"`
	Supplier   bool      `parquet:"supplier"`
	Customer   bool      `parquet:"customer"`
}

var asOf = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// env is a store with tenants a and b, each with its own manifest.
type env struct {
	t      *testing.T
	store  blobstore.Dir
	r      *Reader
	a, b   uuid.UUID
	keys   map[uuid.UUID]string
	manifs map[uuid.UUID]extract.Manifest
}

func day(s string) int32 {
	d, _ := time.Parse(time.DateOnly, s) // the tests' dates are valid
	return int32(d.Unix() / 86400)
}

func line(tenant uuid.UUID, company, date, account string, vendor *string, amount money.Minor) glLine {
	return glLine{TenantID: tenant, DocumentID: uuid.New(), LineNo: 1, LedgerID: uuid.Nil, Company: company, PostingDate: day(date),
		Account: account, DocumentType: "SA", Vendor: vendor, AmountMinor: amount, Currency: "EUR", Exponent: 2, PostedXID: 7, IBAN: "DE89370400440532013000"}
}

func encode[T any](t *testing.T, rows []T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := parquet.Write(&buf, rows); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// put stores b under key and describes it for a manifest.
func (e *env) put(key string, b []byte) extract.File {
	e.t.Helper()
	if err := e.store.Put(context.Background(), key, b, blobstore.PutOptions{}); err != nil {
		e.t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return extract.File{Key: key, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}
}

func part(tenant uuid.UUID, dataset, month string, b []byte) string {
	sum := sha256.Sum256(b)
	dir := extract.Prefix + "/tenant=" + tenant.String() + "/" + dataset
	if month != "" {
		dir += "/month=" + month
	}
	return dir + "/part-" + hex.EncodeToString(sum[:]) + ".parquet"
}

// publish writes a manifest of tenant listing files and returns its key.
func (e *env) publish(tenant uuid.UUID, m extract.Manifest) string {
	e.t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		e.t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	key := extract.ManifestKey(tenant, 7, hex.EncodeToString(sum[:8]))
	if err := e.store.Put(context.Background(), key, b, blobstore.PutOptions{}); err != nil {
		e.t.Fatal(err)
	}
	return key
}

func dataset(files ...extract.File) extract.Dataset {
	return extract.Dataset{Version: 1, Watermark: 7, AsOf: asOf, Files: files}
}

// seed gives tenant its lines (company 1000 and 2000), accounts and
// partners, and publishes its manifest.
func (e *env) seed(tenant uuid.UUID, scale money.Minor) {
	e.t.Helper()
	bp1 := "BP1"
	lines := encode(e.t, []glLine{
		line(tenant, "1000", "2026-03-02", "610000", &bp1, 100*scale),
		line(tenant, "1000", "2026-03-20", "610000", nil, 50*scale),
		line(tenant, "2000", "2026-03-05", "620000", &bp1, 7*scale),
	})
	accounts := encode(e.t, []accountRow{
		{TenantID: tenant, Company: "1000", Account: "610000", AccountName: "Office", AccountType: "expense"},
		{TenantID: tenant, Company: "2000", Account: "620000", AccountName: "Travel", AccountType: "expense"},
	})
	partners := encode(e.t, []partnerRow{{TenantID: tenant, Vendor: "BP1", VendorName: "Paper Co", Supplier: true}})
	m := extract.Manifest{TenantID: tenant, Datasets: map[string]extract.Dataset{
		"gl_lines": dataset(e.put(part(tenant, "gl_lines", "2026-03", lines), lines)),
		"accounts": dataset(e.put(part(tenant, "accounts", "", accounts), accounts)),
		"partners": dataset(e.put(part(tenant, "partners", "", partners), partners)),
		"po_lines": dataset(),
	}}
	e.manifs[tenant] = m
	e.keys[tenant] = e.publish(tenant, m)
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, store: blobstore.Dir{Root: t.TempDir()}, a: uuid.New(), b: uuid.New(), keys: map[uuid.UUID]string{}, manifs: map[uuid.UUID]extract.Manifest{}}
	r, err := New(e.store, t.TempDir(), currency.Seeded(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	e.r = r
	e.seed(e.a, 1)
	e.seed(e.b, 1000)
	return e
}

var nilLedger = uuid.Nil.String()

var everything = Scope{AllCompanies: true, Datasets: []string{"gl_lines", "po_lines", "ap_invoices"}}

func (e *env) request(tenant uuid.UUID, p Plan) Request {
	return Request{TenantID: tenant, ManifestKey: e.keys[tenant], Scope: everything, Plan: p}
}

func (e *env) query(req Request) []Row {
	e.t.Helper()
	res, err := e.r.Query(context.Background(), req)
	if err != nil {
		e.t.Fatalf("query: %v", err)
	}
	if !res.AsOf.Equal(asOf) || res.ManifestKey != req.ManifestKey {
		e.t.Errorf("result: got %s as of %s, want %s as of %s", res.ManifestKey, res.AsOf, req.ManifestKey, asOf)
	}
	return res.Rows
}

func sums(rows []Row) map[string]money.Minor {
	out := map[string]money.Minor{}
	for _, r := range rows {
		out[strings.Join(r.By, "|")+"|"+r.Currency] = r.Amount
	}
	return out
}

func wantSums(t *testing.T, rows []Row, want map[string]money.Minor) {
	t.Helper()
	got := sums(rows)
	if len(got) != len(want) {
		t.Errorf("groups: got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("group %s: got %d, want %d", k, got[k], v)
		}
	}
}

func refusedErr(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrRefused) {
		t.Errorf("error: got %v, want ErrRefused", err)
	}
}

// TestTwoTenants: each tenant's answer holds its own rows alone.
func TestTwoTenants(t *testing.T) {
	e := setup(t)
	p := Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", By: []string{"company"}}
	wantSums(t, e.query(e.request(e.a, p)), map[string]money.Minor{"1000|EUR": 150, "2000|EUR": 7})
	wantSums(t, e.query(e.request(e.b, p)), map[string]money.Minor{"1000|EUR": 150000, "2000|EUR": 7000})
}

// TestPlans: joins, filters and both aggregates.
func TestPlans(t *testing.T) {
	e := setup(t)
	for name, tc := range map[string]struct {
		plan Plan
		want map[string]money.Minor
	}{
		"by partner name":    {Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", By: []string{"vendor_name"}}, map[string]money.Minor{"Paper Co|EUR": 107, "|EUR": 50}},
		"account type":       {Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", Where: []Filter{{Column: "account_type", Op: "in", Values: []string{"expense"}}}}, map[string]money.Minor{"|EUR": 157}},
		"not in":             {Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", Where: []Filter{{Column: "company", Op: "not_in", Values: []string{"1000"}}}}, map[string]money.Minor{"|EUR": 7}},
		"between":            {Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", Where: []Filter{{Column: "posting_date", Op: "between", Values: []string{"2026-03-01", "2026-03-10"}}}}, map[string]money.Minor{"|EUR": 107}},
		"from":               {Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", Where: []Filter{{Column: "posting_date", Op: ">=", Values: []string{"2026-03-10"}}}}, map[string]money.Minor{"|EUR": 50}},
		"supplier attribute": {Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", By: []string{"supplier"}}, map[string]money.Minor{"true|EUR": 107, "|EUR": 50}},
	} {
		t.Run(name, func(t *testing.T) { wantSums(t, e.query(e.request(e.a, tc.plan)), tc.want) })
	}
	t.Run("count distinct", func(t *testing.T) {
		rows := e.query(e.request(e.a, Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "count_distinct", By: []string{"company"}}))
		if len(rows) != 2 || rows[0].Count != 2 || rows[1].Count != 1 {
			t.Errorf("counts: got %+v, want 2 documents of 1000 and 1 of 2000", rows)
		}
	})
	t.Run("empty dataset", func(t *testing.T) {
		rows := e.query(e.request(e.a, Plan{Dataset: "po_lines", Aggregate: "count_distinct"}))
		if len(rows) != 1 || rows[0].Count != 0 {
			t.Errorf("po_lines: got %+v, want one row counting 0", rows)
		}
	})
}

type openItemRow struct {
	TenantID   uuid.UUID `parquet:"tenant_id,uuid"`
	DocumentID uuid.UUID `parquet:"document_id,uuid"`
	LineNo     int32     `parquet:"line_no"`
}

// TestDerived: one ledger per plan, fiscal periods from the date and the
// calendar, and cleared from the open items.
func TestDerived(t *testing.T) {
	e := setup(t)
	tenant, leading, parallel := uuid.New(), uuid.New(), uuid.New()
	bp := "BP1"
	open, paid, other := line(tenant, "1000", "2026-03-02", "211000", &bp, -100), line(tenant, "1000", "2026-07-15", "211000", &bp, -40),
		line(tenant, "1000", "2026-03-02", "211000", &bp, -999)
	open.LedgerID, paid.LedgerID, other.LedgerID = leading, leading, parallel
	lines := encode(t, []glLine{open, paid, other})
	accounts := encode(t, []accountRow{{TenantID: tenant, Company: "1000", Account: "211000", AccountName: "Payables", AccountType: "liability", VendorRecon: true}})
	items := encode(t, []openItemRow{{TenantID: tenant, DocumentID: open.DocumentID, LineNo: 1}})
	e.keys[tenant] = e.publish(tenant, extract.Manifest{TenantID: tenant, Datasets: map[string]extract.Dataset{
		"gl_lines":   dataset(e.put(part(tenant, "gl_lines", "2026-03", lines), lines)),
		"accounts":   dataset(e.put(part(tenant, "accounts", "", accounts), accounts)),
		"open_items": dataset(e.put(part(tenant, "open_items", "", items), items)),
		"partners":   dataset(),
	}})
	in := func(ledger uuid.UUID, by []string, where ...Filter) Plan {
		return Plan{Dataset: "gl_lines", Aggregate: "sum", Ledger: ledger.String(), By: by, Where: where, FiscalYearStartMonth: 7}
	}
	for name, tc := range map[string]struct {
		plan Plan
		want map[string]money.Minor
	}{
		"leading ledger": {in(leading, nil), map[string]money.Minor{"|EUR": -140}},
		"another ledger": {in(parallel, nil), map[string]money.Minor{"|EUR": -999}},
		"fiscal periods": {in(leading, []string{"fiscal_year", "fiscal_quarter", "fiscal_month"}), map[string]money.Minor{"2026|3|9|EUR": -100, "2027|1|1|EUR": -40}},
		"fiscal range":   {in(leading, nil, Filter{Column: "fiscal_month", Op: "between", Values: []string{"1", "3"}}), map[string]money.Minor{"|EUR": -40}},
		"cleared":        {in(leading, []string{"cleared"}), map[string]money.Minor{"false|EUR": -100, "true|EUR": -40}},
		"cleared filter": {in(leading, nil, Filter{Column: "cleared", Op: "in", Values: []string{"false"}}), map[string]money.Minor{"|EUR": -100}},
		"calendar year":  {Plan{Dataset: "gl_lines", Aggregate: "sum", Ledger: leading.String(), By: []string{"fiscal_year"}}, map[string]money.Minor{"2026|EUR": -140}},
		"vendor_recon":   {in(leading, []string{"vendor_recon"}), map[string]money.Minor{"true|EUR": -140}},
	} {
		t.Run(name, func(t *testing.T) { wantSums(t, e.query(e.request(tenant, tc.plan)), tc.want) })
	}
	t.Run("data through", func(t *testing.T) {
		res, err := e.r.Query(context.Background(), e.request(tenant, in(leading, nil)))
		if err != nil || res.DataThrough != "2026-07-15" {
			t.Errorf("data through: got %v, %v, want 2026-07-15", res, err)
		}
	})
}

// TestScope: the principal's companies are applied in the views, joined
// dimensions included, and its fact types in the plan.
func TestScope(t *testing.T) {
	e := setup(t)
	req := e.request(e.a, Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", By: []string{"company", "account_name"}})
	req.Scope = Scope{Companies: []string{"2000"}, Datasets: []string{"gl_lines"}}
	wantSums(t, e.query(req), map[string]money.Minor{"2000|Travel|EUR": 7})
	t.Run("no company", func(t *testing.T) {
		req.Scope.Companies = nil
		_, err := e.r.Query(context.Background(), req)
		refusedErr(t, err)
	})
	t.Run("fact type", func(t *testing.T) {
		req.Scope = Scope{AllCompanies: true, Datasets: []string{"po_lines"}}
		_, err := e.r.Query(context.Background(), req)
		refusedErr(t, err)
	})
}

// TestForgedManifests: a manifest naming another tenant's files, or of
// another tenant, is refused; a file that holds another tenant's rows
// under this tenant's prefix returns none of them.
func TestForgedManifests(t *testing.T) {
	e := setup(t)
	p := Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum"}
	query := func(t *testing.T, tenant uuid.UUID, key string) error {
		t.Helper()
		req := e.request(tenant, p)
		req.ManifestKey = key
		_, err := e.r.Query(context.Background(), req)
		return err
	}
	t.Run("other tenant's file", func(t *testing.T) {
		m := e.manifs[e.a]
		m.Datasets = map[string]extract.Dataset{"gl_lines": e.manifs[e.b].Datasets["gl_lines"]}
		refusedErr(t, query(t, e.a, e.publish(e.a, m)))
	})
	t.Run("other tenant's manifest", func(t *testing.T) { refusedErr(t, query(t, e.a, e.keys[e.b])) })
	t.Run("manifest of another tenant under this prefix", func(t *testing.T) {
		m := e.manifs[e.b]
		m.Datasets = map[string]extract.Dataset{}
		refusedErr(t, query(t, e.a, e.publish(e.a, m)))
	})
	t.Run("traversal", func(t *testing.T) {
		m := e.manifs[e.a]
		f := e.manifs[e.b].Datasets["gl_lines"].Files[0]
		f.Key = strings.Replace(f.Key, "tenant="+e.b.String(), "tenant="+e.a.String()+"/gl_lines/../../tenant="+e.b.String(), 1)
		m.Datasets = map[string]extract.Dataset{"gl_lines": dataset(f)}
		refusedErr(t, query(t, e.a, e.publish(e.a, m)))
	})
	t.Run("other tenant's rows under this prefix", func(t *testing.T) {
		b, _, err := e.store.Get(context.Background(), e.manifs[e.b].Datasets["gl_lines"].Files[0].Key)
		if err != nil {
			t.Fatal(err)
		}
		m := e.manifs[e.a]
		m.Datasets = map[string]extract.Dataset{"gl_lines": dataset(e.put(part(e.a, "gl_lines", "2026-04", b), b))}
		req := e.request(e.a, p)
		req.ManifestKey = e.publish(e.a, m)
		if rows := e.query(req); len(rows) != 0 {
			t.Errorf("rows: got %+v, want none of the other tenant's", rows)
		}
	})
	t.Run("tampered file", func(t *testing.T) {
		m := e.manifs[e.a]
		f := m.Datasets["gl_lines"].Files[0]
		f.SHA256 = strings.Repeat("0", 64)
		m.Datasets = map[string]extract.Dataset{"gl_lines": dataset(f)}
		refusedErr(t, query(t, e.a, e.publish(e.a, m)))
	})
}

// TestMaskedFieldsAbsent: a column the contract does not list never
// reaches a view, so no plan can read it.
func TestMaskedFieldsAbsent(t *testing.T) {
	e := setup(t)
	_, err := e.r.Query(context.Background(), e.request(e.a, Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum", By: []string{"iban"}}))
	refusedErr(t, err)
	v := view(e.r.contract["gl_lines"], []string{"/x.parquet"}, true)
	if strings.Contains(v, "iban") || strings.Contains(v, "*") {
		t.Errorf("view: got %s, want the contract's columns alone", v)
	}
}

// TestConfined: a query's database reads the files it registered alone and
// its configuration cannot be changed back.
func TestConfined(t *testing.T) {
	e := setup(t)
	c, err := compile(e.r.contract, everything, Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum"})
	if err != nil {
		t.Fatal(err)
	}
	mine, err := e.r.files(context.Background(), e.a, "gl_lines", e.manifs[e.a])
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(e.store.Root, filepath.FromSlash(e.manifs[e.b].Datasets["gl_lines"].Files[0].Key))
	db, cx, err := e.r.open(context.Background(), e.request(e.a, Plan{}), c, map[string][]string{"gl_lines": mine})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cx.Close() // an in-memory database: nothing to lose
		_ = db.Close()
	})
	for name, q := range map[string]string{
		"another file":       "SELECT count(*) FROM read_parquet('" + other + "')",
		"external access on": "SET enable_external_access = true",
		"more paths":         "SET allowed_paths = ['" + other + "']",
		"an extension":       "INSTALL httpfs",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cx.ExecContext(context.Background(), q); err == nil {
				t.Errorf("%s: got nil error, want a refusal", q)
			}
		})
	}
}

// TestRefusedAmounts: an amount whose exponent is not its currency's, of
// an unknown currency or whose sum leaves int64 is refused.
func TestRefusedAmounts(t *testing.T) {
	for name, edit := range map[string]func(*glLine){
		"exponent":         func(l *glLine) { l.Exponent = 3 },
		"unknown currency": func(l *glLine) { l.Currency = "XXQ" },
		"overflow":         func(l *glLine) { l.AmountMinor = 1<<63 - 1 },
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			l1, l2 := line(e.a, "1000", "2026-03-02", "610000", nil, 1), line(e.a, "1000", "2026-03-02", "610000", nil, 1)
			edit(&l1)
			edit(&l2)
			b := encode(t, []glLine{l1, l2})
			m := e.manifs[e.a]
			m.Datasets = map[string]extract.Dataset{"gl_lines": dataset(e.put(part(e.a, "gl_lines", "2026-03", b), b))}
			req := e.request(e.a, Plan{Dataset: "gl_lines", Ledger: nilLedger, Aggregate: "sum"})
			req.ManifestKey = e.publish(e.a, m)
			_, err := e.r.Query(context.Background(), req)
			refusedErr(t, err)
		})
	}
}

// TestCache: a file is fetched once, and a cached copy that no longer
// matches is fetched again.
func TestCache(t *testing.T) {
	e := setup(t)
	f := e.manifs[e.a].Datasets["gl_lines"].Files[0]
	p, err := e.r.fetch(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.fetch(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !matches(b, f) {
		t.Error("cache: got the changed copy, want the file fetched again")
	}
	entries, _ := os.ReadDir(e.r.Cache)
	if len(entries) != 1 {
		t.Errorf("cache entries: got %d, want 1 (no leftover)", len(entries))
	}
}
