package integration

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// fullReport is a report with every field the code can write set to a
// value of its own: every SAP table extracted, every count loaded, and each
// list holding a row.
func fullReport(t *testing.T) Report {
	t.Helper()
	var ext sap.Extract
	v := reflect.ValueOf(&ext).Elem()
	for i := range v.NumField() {
		if f := v.Field(i); f.Kind() == reflect.Slice {
			f.Set(reflect.MakeSlice(f.Type(), i+1, i+1))
		}
	}
	var loaded LoadedCounts
	l := reflect.ValueOf(&loaded).Elem()
	for i := range l.NumField() {
		l.Field(i).SetInt(int64(i + 1))
	}
	issue := func(table string) Issue {
		return Issue{Severity: SevWarning, Table: table, Key: table + "/1", Message: "about " + table}
	}
	return Report{Extract: ext.Counts(), Quality: []Issue{issue("LFA1")},
		LoadResult: &LoadResult{Refusals: []Issue{issue("EKKO")}, Loaded: loaded},
		Reconciliation: &Reconciliation{
			Checks:  []Check{{CompanyCode: "1000", Name: "Vendors", SAP: "2", Platform: "3", Pass: true}},
			Diffs:   []Diff{{CompanyCode: "1000", Key: "vendor 4711", SAP: "1.00 EUR", Platform: "2.00 EUR"}},
			Missing: []string{"1000/2026/0100000001"}, Issues: []Issue{issue("ACDOCA")}, Passed: true}}
}

// decoded is a JSON document decoded into plain values.
func decoded(t *testing.T, b []byte, err error) any {
	t.Helper()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

// unset lists the fields of a contract value that hold nothing: an absent
// optional, an empty list, a zero.
func unset(v reflect.Value, path string) []string {
	if v.Kind() == reflect.Struct && v.FieldByName("Set").IsValid() && v.FieldByName("Value").IsValid() {
		if !v.FieldByName("Set").Bool() {
			return []string{path}
		}
		return unset(v.FieldByName("Value"), path)
	}
	switch v.Kind() {
	case reflect.Struct:
		var out []string
		for i := range v.NumField() {
			out = append(out, unset(v.Field(i), path+"."+v.Type().Field(i).Name)...)
		}
		return out
	case reflect.Slice:
		if v.Len() == 0 {
			return []string{path}
		}
		return unset(v.Index(0), path+"[0]")
	default:
		if v.IsZero() {
			return []string{path}
		}
		return nil
	}
}

// TestReportMatchesContract holds the report a run stores and the
// contract's MigrationRunReport to the same keys and values: a key the code
// writes that the contract does not declare (or reportToAPI leaves out) is
// missing from the response, and a key the contract declares that the code
// has no field for is never set.
func TestReportMatchesContract(t *testing.T) {
	r := fullReport(t)
	stored, err := json.Marshal(r)
	want := decoded(t, stored, err)
	api := reportToAPI(r)
	answer, err := api.MarshalJSON()
	if got := decoded(t, answer, err); !reflect.DeepEqual(got, want) {
		t.Errorf("the response's report:\n got %s\nwant %s", answer, stored)
	}
	if missing := unset(reflect.ValueOf(api), "report"); len(missing) > 0 {
		t.Errorf("declared but never written: got %v, want none", missing)
	}
}

// TestReportParts: a report holds the parts that ran, and the lists of a
// part that ran are there even when empty; a stored report from before a
// key existed reads without it.
func TestReportParts(t *testing.T) {
	cases := []struct{ name, stored, want string }{
		{"refused by the quality gate", `{"extract": {"T001": 1}, "quality": [{"severity": "error", "table": "T001", "key": "1000", "message": "m"}]}`,
			`{"quality": [{"severity": "error", "table": "T001", "key": "1000", "message": "m"}]}`},
		{"loaded nothing", `{"extract": {}, "quality": [], "load": [], "loaded": {}}`, `{"quality": [], "load": [], "loaded": {}}`},
		{"reconciled, stored with null lists", `{"extract": {}, "quality": [], "load": [], "loaded": {"suppliers": 2}, "checks": null, "diffs": null, "passed": true, "gone": 1}`,
			`{"quality": [], "load": [], "loaded": {"suppliers": 2}, "checks": [], "diffs": [], "passed": true}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var r Report
			if err := json.Unmarshal([]byte(c.stored), &r); err != nil {
				t.Fatalf("read the stored report: %v", err)
			}
			api := reportToAPI(r)
			answer, err := api.MarshalJSON()
			got := decoded(t, answer, err).(map[string]any)
			delete(got, "extract") // every table, compared in TestReportMatchesContract
			if want := decoded(t, []byte(c.want), nil); !reflect.DeepEqual(got, want) {
				t.Errorf("got %s, want %s", answer, c.want)
			}
		})
	}
}

// TestEveryLoadedCountIsCounted: each count the contract declares has a
// field in LoadedCounts (TestReportMatchesContract), and the loader counts
// each field somewhere, so the contract names no count that is never
// written.
func TestEveryLoadedCountIsCounted(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	counted := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := s.X.(*ast.SelectorExpr); ok && x.Sel.Name == "counts" {
					counted[s.Sel.Name] = true
				}
			}
			return true
		})
	}
	fields := reflect.VisibleFields(reflect.TypeFor[LoadedCounts]())
	for _, f := range fields {
		if !counted[f.Name] {
			t.Errorf("LoadedCounts.%s: got no l.counts.%s in the loader, want it counted", f.Name, f.Name)
		}
	}
	if n := reflect.TypeFor[gen.MigrationLoadedCounts]().NumField(); n != len(fields) {
		t.Errorf("counts: got %d in LoadedCounts, want the contract's %d", len(fields), n)
	}
}
