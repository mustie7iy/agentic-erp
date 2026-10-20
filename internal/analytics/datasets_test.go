package analytics

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/analytics/datasets"
)

// The analytical store's dataset contract (ADR-0044) lives in
// analytics/datasets/v1; these tests keep it and semantic.yaml in step, so a
// metric cannot name a dimension the store does not carry and the store
// cannot carry a column no metric explains.

type (
	dataset  = datasets.Dataset
	dsColumn = datasets.Column
)

var (
	dsTypes = []string{"uuid", "string", "int32", "int64", "bool", "date", "timestamp", "xid8"}
	// attribute: a column the reader may filter or group by that no semantic
	// dimension names (a partner's roles).
	dsRoles = []string{"tenant", "id", "key", "dimension", "date", "amount", "currency", "exponent", "watermark", "attribute"}
	// A guard, not the masking policy itself: names of fields that are
	// masked or redacted (bank details, tax ids, contact data, ADR-0036) or
	// free text that may hold them. The store never holds them, so it cannot
	// show what the API would mask.
	maskedFields = []string{"iban", "card", "tax_id", "bank", "email", "phone", "text", "note"}
	calendarDims = []string{"fiscal_year", "fiscal_quarter", "fiscal_month"}
)

func loadDatasets(t *testing.T) map[string]*dataset {
	t.Helper()
	sets, err := datasets.V1()
	if err != nil {
		t.Fatalf("dataset files: %v", err)
	}
	return sets
}

func TestDatasetContractShape(t *testing.T) {
	sets := loadDatasets(t)
	for name, d := range sets {
		t.Run(name, func(t *testing.T) {
			checkHeader(t, d)
			checkColumns(t, d)
			checkJoins(t, d, sets)
			if d.Kind == "fact" {
				checkFact(t, d)
			}
		})
	}
}

func checkHeader(t *testing.T, d *dataset) {
	t.Helper()
	if d.Version != 1 {
		t.Errorf("version: got %d, want 1 (the directory)", d.Version)
	}
	if !slices.Contains([]string{"fact", "dimension"}, d.Kind) {
		t.Errorf("kind: got %q, want fact or dimension", d.Kind)
	}
	if !slices.Contains([]string{"append", "snapshot"}, d.Load) {
		t.Errorf("load: got %q, want append or snapshot", d.Load)
	}
	if d.Grain == "" {
		t.Error("grain: got empty, want the row's meaning")
	}
	wm := d.WithRole("watermark")
	if d.Load == "append" && (len(wm) != 1 || wm[0].Name != d.Watermark || wm[0].Type != "xid8") {
		t.Errorf("watermark: got %q %v, want one xid8 watermark column (commit order, ADR-0044) when appending", d.Watermark, wm)
	}
	if d.Load != "append" && (d.Watermark != "" || len(wm) != 0) {
		t.Errorf("watermark: got %q %v with load %s, want none on a snapshot", d.Watermark, wm, d.Load)
	}
}

func checkColumns(t *testing.T, d *dataset) {
	t.Helper()
	if len(d.Columns) == 0 || d.Columns[0] != (dsColumn{Name: "tenant_id", Type: "uuid", Role: "tenant"}) {
		t.Errorf("first column: got %v, want tenant_id uuid tenant (tenant_id leads, ADR-0003)", d.Columns)
	}
	seen := map[string]bool{}
	for _, c := range d.Columns {
		if seen[c.Name] {
			t.Errorf("column %s: got a duplicate, want unique names", c.Name)
		}
		seen[c.Name] = true
		if !slices.Contains(dsTypes, c.Type) {
			t.Errorf("column %s: type got %q, want one of %v", c.Name, c.Type, dsTypes)
		}
		if !slices.Contains(dsRoles, c.Role) {
			t.Errorf("column %s: role got %q, want one of %v", c.Name, c.Role, dsRoles)
		}
		for _, m := range maskedFields {
			if strings.Contains(c.Name, m) {
				t.Errorf("column %s: got a masked field (%s), want it never extracted", c.Name, m)
			}
		}
	}
}

func checkJoins(t *testing.T, d *dataset, sets map[string]*dataset) {
	t.Helper()
	for _, j := range d.Joins {
		dim := sets[j.Dataset]
		if dim == nil || dim.Kind != "dimension" {
			t.Errorf("join %s: got no dimension dataset, want one", j.Dataset)
			continue
		}
		for _, k := range j.On {
			if d.Column(k) == nil || dim.Column(k) == nil || dim.Column(k).Role != "key" {
				t.Errorf("join %s on %s: got a key missing on either side, want it in both and a key of %s", j.Dataset, k, j.Dataset)
			}
		}
	}
	for _, dv := range d.Derived {
		if sets[dv.From] == nil || dv.Rule == "" {
			t.Errorf("derived %s: got from %q rule %q, want an existing dataset and a rule", dv.Name, dv.From, dv.Rule)
		}
	}
}

// checkFact holds a fact dataset to the semantic layer's Fact: one document
// id (what count_distinct counts), a date the calendar derives fiscal
// periods from, and money as minor units, currency and exponent (ADR-0005).
func checkFact(t *testing.T, d *dataset) {
	t.Helper()
	want := map[string]string{"id": "uuid", "date": "date", "amount": "int64", "currency": "string", "exponent": "int32"}
	for role, typ := range want {
		cols := d.WithRole(role)
		if len(cols) != 1 || cols[0].Type != typ || cols[0].Nullable {
			t.Errorf("role %s: got %v, want one non-null %s column", role, cols, typ)
		}
	}
	if c := d.Column(d.Date); c == nil || c.Role != "date" {
		t.Errorf("date: got %q, want the name of the date column", d.Date)
	}
}

// provides lists the names a fact dataset answers: its own dimensions and
// date, the attributes of the dimensions it joins, its derived names and the
// fiscal periods of its date.
func provides(d *dataset, sets map[string]*dataset) map[string]bool {
	out := map[string]bool{}
	for _, c := range d.Columns {
		if c.Role == "dimension" || c.Role == "date" {
			out[c.Name] = true
		}
	}
	for _, j := range d.Joins {
		for _, c := range sets[j.Dataset].Columns {
			if c.Role == "dimension" || c.Role == "key" {
				out[c.Name] = true
			}
		}
	}
	for _, dv := range d.Derived {
		out[dv.Name] = true
	}
	for _, c := range calendarDims {
		out[c] = true
	}
	return out
}

func TestDatasetsCoverSemanticModel(t *testing.T) {
	m, err := DefaultModel()
	if err != nil {
		t.Fatalf("semantic model: %v", err)
	}
	sets := loadDatasets(t)
	for _, met := range m.Metrics {
		if met.Source == "" {
			continue
		}
		t.Run(met.Name, func(t *testing.T) {
			d := sets[met.Source]
			if d == nil || d.Kind != "fact" {
				t.Fatalf("source %s: got no fact dataset, want one", met.Source)
			}
			have := provides(d, sets)
			for _, dim := range met.Dimensions {
				if !have[dim] {
					t.Errorf("dimension %s: got no column in %s or its joins, want one", dim, d.Dataset)
				}
			}
			for key := range met.Filter {
				if !have[key] {
					t.Errorf("filter %s: got no column in %s or its joins, want one", key, d.Dataset)
				}
			}
		})
	}
}

func TestDatasetDimensionsMapToSemanticModel(t *testing.T) {
	m, err := DefaultModel()
	if err != nil {
		t.Fatalf("semantic model: %v", err)
	}
	known := map[string]bool{}
	for _, d := range m.Dimensions {
		known[d.Name] = true
	}
	for _, met := range m.Metrics {
		for key := range met.Filter {
			known[key] = true
		}
	}
	for name, d := range loadDatasets(t) {
		t.Run(name, func(t *testing.T) {
			for _, c := range d.WithRole("dimension") {
				if !known[c.Name] {
					t.Errorf("column %s: got no dimension or filter of that name in semantic.yaml, want one", c.Name)
				}
			}
			for _, dv := range d.Derived {
				if !known[dv.Name] {
					t.Errorf("derived %s: got no dimension or filter of that name in semantic.yaml, want one", dv.Name)
				}
			}
		})
	}
}
