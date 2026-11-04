package cash

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// update rewrites the golden export files:
//
//	go test ./internal/cash -run TestExportGolden -update
var update = flag.Bool("update", false, "rewrite testdata/cash-export.v1.* from the golden rows")

func ptr(s string) *string { return &s }

// goldenRows are a snapshot's export rows as exportRows orders them: a
// day at a bank and one with neither bank nor item, a bucket, and another
// currency's day.
func goldenRows() []exportRow {
	base := exportRow{SchemaVersion: 1, CompanyCode: "3000", SnapshotID: uuid.MustParse("0192a000-0000-7000-8000-000000000e01"), SnapshotDate: "2026-04-17",
		SnapshotKind: "on_demand", TakenAt: "2026-04-17T13:05:00.123456Z", Currency: "CAD", Granularity: "day"}
	row := func(change func(*exportRow)) exportRow {
		r := base
		change(&r)
		return r
	}
	return []exportRow{
		row(func(r *exportRow) {
			r.PeriodStart, r.PeriodEnd, r.Source, r.Certainty, r.Amount, r.FlowCount = "2026-04-17", "2026-04-17", "payment_run", "confirmed", "-800.00", 1
			r.HouseBankCode, r.AccountLast4, r.LiquidityItem = ptr("MAIN"), ptr("3000"), ptr("OP_SUPPLIERS")
		}),
		row(func(r *exportRow) {
			r.PeriodStart, r.PeriodEnd, r.Source, r.Certainty, r.Amount, r.FlowCount = "2026-04-24", "2026-04-24", "memo", "expected", "100.00", 1
		}),
		row(func(r *exportRow) {
			r.Granularity, r.PeriodStart, r.PeriodEnd, r.Source, r.Certainty, r.Amount, r.FlowCount = "bucket", "2026-05-18", "2026-05-21", "payable", "expected", "-200.00", 2
			r.HouseBankCode, r.AccountLast4, r.LiquidityItem = ptr("MAIN"), ptr("3000"), ptr("OP_SUPPLIERS")
		}),
		row(func(r *exportRow) {
			r.Currency, r.PeriodStart, r.PeriodEnd, r.Source, r.Certainty, r.Amount, r.FlowCount = "USD", "2026-04-17", "2026-04-17", "in_transit", "actual", "-300.00", 1
			r.HouseBankCode, r.AccountLast4, r.LiquidityItem = ptr("USD1"), ptr("0011"), ptr("OP_SUPPLIERS")
		}),
	}
}

// TestExportGolden: the export of the golden rows in each format is the
// published example of schema v1 byte for byte, named by the company, the
// snapshot's date and its content's hash; rendered twice it is the same
// file (a resend).
func TestExportGolden(t *testing.T) {
	day := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)
	for _, format := range []string{FormatJSONL, FormatCSV} {
		t.Run(format, func(t *testing.T) {
			f, err := renderExport("3000", day, format, goldenRows())
			if err != nil {
				t.Fatal(err)
			}
			path := "testdata/cash-export.v1." + format
			if *update {
				if err := os.WriteFile(path, f.content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			golden, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(f.content, golden) {
				t.Errorf("content:\n got %q\nwant %q", f.content, golden)
			}
			sum := sha256.Sum256(golden)
			if want := "cash-3000-2026-04-17-" + hex.EncodeToString(sum[:]) + "." + format; f.name != want {
				t.Errorf("name: got %s, want %s", f.name, want)
			}
			if !bytes.Equal(f.sha256, sum[:]) {
				t.Errorf("hash: got %x, want %x", f.sha256, sum)
			}
			again, err := renderExport("3000", day, format, goldenRows())
			if err != nil || again.name != f.name {
				t.Errorf("rendered again: got %s (%v), want the same file %s", again.name, err, f.name)
			}
		})
	}
	t.Run("another format", func(t *testing.T) {
		if _, err := renderExport("3000", day, "xlsx", goldenRows()); err == nil || !strings.Contains(err.Error(), "not jsonl or csv") {
			t.Errorf("got %v, want the format refused", err)
		}
	})
}

// exportSchema is the published schema's columns, required keys and
// properties.
type exportSchema struct {
	Columns    []string                   `json:"x-columns"`
	Required   []string                   `json:"required"`
	Properties map[string]json.RawMessage `json:"properties"`
}

func readSchema(t *testing.T) exportSchema {
	t.Helper()
	b, err := os.ReadFile("../../api/schemas/cash-export.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var s exportSchema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestExportSchema: the published schema (api/schemas/cash-export.v1.json)
// and the code agree: its columns, in order, are every key of a JSON line
// and the CSV's header, all required, each a property; the golden files
// hold rows of those columns.
func TestExportSchema(t *testing.T) {
	s := readSchema(t)
	if !slices.Equal(s.Columns, exportColumns) {
		t.Errorf("x-columns: got %v, want %v", s.Columns, exportColumns)
	}
	if !slices.Equal(s.Required, exportColumns) {
		t.Errorf("required: got %v, want every column %v", s.Required, exportColumns)
	}
	var props []string
	for k := range s.Properties {
		props = append(props, k)
	}
	slices.Sort(props)
	cols := slices.Sorted(slices.Values(exportColumns))
	if !slices.Equal(props, cols) {
		t.Errorf("properties: got %v, want %v", props, cols)
	}
	typ := reflect.TypeFor[exportRow]()
	var tags []string
	for i := range typ.NumField() {
		tags = append(tags, typ.Field(i).Tag.Get("json"))
	}
	if !slices.Equal(tags, exportColumns) {
		t.Errorf("a row's JSON keys: got %v, want the columns %v", tags, exportColumns)
	}
	lines, err := os.ReadFile("testdata/cash-export.v1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(strings.TrimSuffix(string(lines), "\n"), "\n") {
		var keys []string
		dec := json.NewDecoder(strings.NewReader(line))
		_, _ = dec.Token() // the object's opening brace
		for dec.More() {
			k, _ := dec.Token() // a key, then its value
			keys = append(keys, k.(string))
			var v any
			if err := dec.Decode(&v); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(keys, exportColumns) {
			t.Errorf("JSON line %d: keys %v, want the columns in order", i+1, keys)
		}
	}
	records, err := csv.NewReader(bytes.NewReader(mustRead(t, "testdata/cash-export.v1.csv"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(records[0], exportColumns) {
		t.Errorf("CSV header: got %v, want the columns", records[0])
	}
	if len(records) != len(goldenRows())+1 {
		t.Errorf("CSV records: got %d, want a header and %d rows", len(records), len(goldenRows()))
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCompareRows: rows sort by currency, period, house bank (none
// first), source, certainty and item (none first).
func TestCompareRows(t *testing.T) {
	want := goldenRows()
	got := slices.Clone(want)
	slices.Reverse(got)
	slices.SortFunc(got, compareRows)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sorted: got %v, want %v", got, want)
	}
	a, b := want[0], want[0]
	b.HouseBankCode = nil
	if compareRows(b, a) >= 0 {
		t.Error("a row with no bank: got sorted after one with a bank, want before")
	}
}
