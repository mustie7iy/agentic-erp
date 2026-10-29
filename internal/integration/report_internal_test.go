package integration

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// fullReport is a report with every field set, by reflection, to a value
// of its own: every SAP table extracted, every string, count and flag set,
// every list holding a row, every part present. A field added to the
// report or to any type in it is set too, so the contract test sees it.
func fullReport(t *testing.T) Report {
	t.Helper()
	var ext sap.Extract
	v := reflect.ValueOf(&ext).Elem()
	for i := range v.NumField() {
		if f := v.Field(i); f.Kind() == reflect.Slice {
			f.Set(reflect.MakeSlice(f.Type(), i+1, i+1))
		}
	}
	r := Report{Extract: ext.Counts()}
	n := 0
	fill(t, reflect.ValueOf(&r).Elem(), &n)
	return r
}

// fill sets v and everything in it to values that differ from each other
// and from the zero value; a map must already be set.
func fill(t *testing.T, v reflect.Value, n *int) {
	t.Helper()
	*n++
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(t, v.Elem(), n)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(t, v.Field(i), n)
			}
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(t, s.Index(0), n)
		v.Set(s)
	case reflect.String:
		v.SetString(fmt.Sprintf("value %d", *n))
	case reflect.Int:
		v.SetInt(int64(*n))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Map:
		if v.IsNil() {
			t.Fatalf("fill %s: got a nil map, want it set before", v.Type())
		}
	default:
		t.Fatalf("fill: got a field of kind %s, want fill to know how to set it", v.Kind())
	}
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
			got, ok := decoded(t, answer, err).(map[string]any)
			if !ok {
				t.Fatalf("the response's report: got %s, want an object", answer)
			}
			delete(got, "extract") // every table, compared in TestReportMatchesContract
			if want := decoded(t, []byte(c.want), nil); !reflect.DeepEqual(got, want) {
				t.Errorf("got %s, want %s", answer, c.want)
			}
		})
	}
}

// written collects, from the package's code outside tests and report.go
// (which only reads the report), the fields of the report's types the code
// writes: the keys of a keyed literal of the type, and the fields
// assigned, incremented or taken the address of through report, rc.rec or
// l.counts.
func written(t *testing.T) map[string]map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	out := map[string]map[string]bool{}
	add := func(typ, field string) {
		if out[typ] == nil {
			out[typ] = map[string]bool{}
		}
		out[typ][field] = true
	}
	through := func(e ast.Expr) {
		s, ok := e.(*ast.SelectorExpr)
		if !ok {
			return
		}
		switch x := s.X.(type) {
		case *ast.Ident:
			if x.Name == "report" {
				add("Report", s.Sel.Name)
			}
		case *ast.SelectorExpr:
			if typ := map[string]string{"rec": "Reconciliation", "counts": "LoadedCounts"}[x.Sel.Name]; typ != "" {
				add(typ, s.Sel.Name)
			}
		}
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if name := e.Name(); !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "report.go" {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		ast.Inspect(f, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CompositeLit:
				if typ, ok := n.Type.(*ast.Ident); ok {
					for _, el := range n.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok {
								add(typ.Name, k.Name)
							}
						}
					}
				}
			case *ast.AssignStmt:
				for _, l := range n.Lhs {
					through(l)
				}
			case *ast.IncDecStmt:
				through(n.X)
			case *ast.UnaryExpr:
				if n.Op == token.AND {
					through(n.X)
				}
			}
			return true
		})
	}
	return out
}

// TestEveryReportFieldIsWritten: every field of the report's types, each of
// which the contract declares (TestReportMatchesContract), is written by
// the code that runs a migration, so the contract names no key a run never
// writes.
func TestEveryReportFieldIsWritten(t *testing.T) {
	got := written(t)
	for _, typ := range []reflect.Type{reflect.TypeFor[Report](), reflect.TypeFor[LoadResult](), reflect.TypeFor[LoadedCounts](),
		reflect.TypeFor[Reconciliation](), reflect.TypeFor[Issue](), reflect.TypeFor[Check](), reflect.TypeFor[Diff]()} {
		for i := range typ.NumField() {
			if f := typ.Field(i).Name; !got[typ.Name()][f] {
				t.Errorf("%s.%s: got it never written outside tests, want the run to write it", typ.Name(), f)
			}
		}
	}
}

// TestSummaryMatchesEventContract: MigrationRunCompleted's summary, and
// the loaded counts in it, hold the keys api/asyncapi.yaml declares, every
// one of them, so an event consumer reads the same figures as the report's
// reader (the published events are checked against the whole schema in
// the package's tests through apitest.Conforms).
func TestSummaryMatchesEventContract(t *testing.T) {
	b, err := os.ReadFile("../../api/asyncapi.yaml")
	if err != nil {
		t.Fatalf("read the event contract: %v", err)
	}
	type props struct {
		Properties map[string]props `yaml:"properties"`
	}
	var doc struct {
		Components struct {
			Schemas map[string]props `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse the event contract: %v", err)
	}
	stored, err := json.Marshal(summaryOf(fullReport(t)))
	got, ok := decoded(t, stored, err).(map[string]any)
	if !ok {
		t.Fatalf("summary: got %s, want an object", stored)
	}
	loaded, ok := got["loaded"].(map[string]any)
	if !ok {
		t.Fatalf("summary loaded: got %s, want an object", stored)
	}
	schemas := doc.Components.Schemas
	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"summary", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(schemas["MigrationRunCompletedV1"].Properties["summary"].Properties))},
		{"loaded", slices.Sorted(maps.Keys(loaded)), slices.Sorted(maps.Keys(schemas["MigrationLoadedCounts"].Properties))},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !slices.Equal(c.got, c.want) {
				t.Errorf("keys: got %v, want %v", c.got, c.want)
			}
		})
	}
}
