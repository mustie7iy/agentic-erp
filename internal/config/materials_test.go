package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withMaterialLists is the fixture with two material types and two
// material groups (ADR-0082 As built S1a4).
func withMaterialLists(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	c.Materials = Materials{
		MaterialTypes:  []MaterialType{{Code: "trading_good", Name: "Trading goods"}, {Code: "raw_material", Name: "Raw materials"}},
		MaterialGroups: []MaterialGroup{{Code: "BRACKETS", Name: "Brackets"}, {Code: "FASTEN-01", Name: "Fasteners"}},
	}
	return c
}

// TestMaterialListsValid: types of the platform's and groups of
// materials' form, each once with a name, validate, as does a document
// without the section; each rule refuses the setting's own path.
func TestMaterialListsValid(t *testing.T) {
	if fs := Validate(withMaterialLists(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("two types and two groups: got %v, want no errors", fs)
	}
	if fs := Validate(fixture(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("no materials section: got %v, want no errors", fs)
	}
	many := make([]MaterialGroup, MaxMaterialGroups+1)
	for i := range many {
		many[i] = MaterialGroup{Code: fmt.Sprintf("G%04d", i), Name: "Group"}
	}
	cases := []struct {
		name          string
		change        func(*Materials)
		path, message string
	}{
		{"types only", func(m *Materials) { m.MaterialGroups = nil }, "", ""},
		{"groups only", func(m *Materials) { m.MaterialTypes = nil }, "", ""},
		{"every group the bound allows", func(m *Materials) { m.MaterialGroups = many[:MaxMaterialGroups] }, "", ""},
		{"a type of no platform type", func(m *Materials) { m.MaterialTypes[1].Code = "ZROH" }, "materials.material_types[1].code", "must be one of raw_material, semi_finished"},
		{"a type twice", func(m *Materials) { m.MaterialTypes[1].Code = "trading_good" }, "materials.material_types[trading_good]", "duplicate material type code"},
		{"a type without a name", func(m *Materials) { m.MaterialTypes[0].Name = " " }, "materials.material_types[trading_good].name", "is required"},
		{"a group in small letters", func(m *Materials) { m.MaterialGroups[1].Code = "fasten" }, "materials.material_groups[1].code", "1-9 capital letters"},
		{"a group of ten characters", func(m *Materials) { m.MaterialGroups[1].Code = "FASTENERS1" }, "materials.material_groups[1].code", "1-9 capital letters"},
		{"no group code", func(m *Materials) { m.MaterialGroups[0].Code = "" }, "materials.material_groups[0].code", "1-9 capital letters"},
		{"a group twice", func(m *Materials) { m.MaterialGroups[1].Code = "BRACKETS" }, "materials.material_groups[BRACKETS]", "duplicate material group code"},
		{"a group's name of 61 characters", func(m *Materials) { m.MaterialGroups[0].Name = strings.Repeat("n", 61) }, "materials.material_groups[BRACKETS].name", "at most 60"},
		{"too many groups", func(m *Materials) { m.MaterialGroups = many }, "materials.material_groups", "at most 1000 material groups"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withMaterialLists(t)
			tc.change(&c.Materials)
			fs := Validate(c, currency.Seeded())
			if tc.path == "" {
				if HasErrors(fs) {
					t.Errorf("findings: got %v, want no errors", fs)
				}
				return
			}
			if !slices.ContainsFunc(fs, func(f Finding) bool {
				return f.Severity == SevError && f.Path == tc.path && strings.Contains(f.Message, tc.message)
			}) {
				t.Errorf("findings: got %v, want an error at %s saying %q", fs, tc.path, tc.message)
			}
		})
	}
}

// TestMaterialListsAllow: an empty list allows every platform type and
// any group; a list allows only what it names; types and groups are
// found by code.
func TestMaterialListsAllow(t *testing.T) {
	none, some := Materials{}, withMaterialLists(t).Materials
	cases := []struct {
		name  string
		lists Materials
		allow func(Materials) bool
		want  bool
	}{
		{"a platform type, none configured", none, func(m Materials) bool { return m.AllowsType("service") }, true},
		{"no platform type, none configured", none, func(m Materials) bool { return m.AllowsType("gadget") }, false},
		{"a configured type", some, func(m Materials) bool { return m.AllowsType("raw_material") }, true},
		{"a platform type not configured", some, func(m Materials) bool { return m.AllowsType("service") }, false},
		{"any group, none configured", none, func(m Materials) bool { return m.AllowsGroup("WIDGETS") }, true},
		{"a configured group", some, func(m Materials) bool { return m.AllowsGroup("FASTEN-01") }, true},
		{"a group not configured", some, func(m Materials) bool { return m.AllowsGroup("WIDGETS") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.allow(tc.lists); got != tc.want {
				t.Errorf("allowed: got %v, want %v", got, tc.want)
			}
		})
	}
	if got, want := strings.Join(none.TypeCodes(), ","), "raw_material,semi_finished,finished_good,trading_good,non_stock,service"; got != want {
		t.Errorf("types without a list: got %s, want %s", got, want)
	}
	if got, want := strings.Join(some.TypeCodes(), ","), "trading_good,raw_material"; got != want {
		t.Errorf("types of the list: got %s, want %s", got, want)
	}
	codes := MaterialTypeCodes()
	codes[0] = "changed"
	if !IsMaterialType("raw_material") || IsMaterialType("changed") {
		t.Error("MaterialTypeCodes: got the platform's list, want a copy")
	}
	if ty, ok := some.MaterialType("raw_material"); !ok || ty.Name != "Raw materials" {
		t.Errorf("type raw_material: got %v %v, want Raw materials", ty, ok)
	}
	if g, ok := some.MaterialGroup("BRACKETS"); !ok || g.Name != "Brackets" {
		t.Errorf("group BRACKETS: got %v %v, want Brackets", g, ok)
	}
	if _, ok := some.MaterialGroup("WIDGETS"); ok {
		t.Error("group WIDGETS: got one, want none")
	}
}

// TestMaterialListsTransitions: a material type or group is never
// removed once activated, so a list never becomes empty again; names may
// change and entries be added.
func TestMaterialListsTransitions(t *testing.T) {
	for name, c := range map[string]struct {
		base   bool
		change func(*Materials)
		path   string // of the expected error; "" when none
	}{
		"first version": {false, func(m *Materials) { *m = Materials{} }, ""},
		"unchanged":     {true, func(*Materials) {}, ""},
		"renamed":       {true, func(m *Materials) { m.MaterialGroups[0].Name = "Angle brackets"; m.MaterialTypes[0].Name = "Goods" }, ""},
		"added": {true, func(m *Materials) {
			m.MaterialTypes = append(m.MaterialTypes, MaterialType{Code: "service", Name: "Services"})
			m.MaterialGroups = append(m.MaterialGroups, MaterialGroup{Code: "SEALS", Name: "Seals"})
		}, ""},
		"a type removed":      {true, func(m *Materials) { m.MaterialTypes = m.MaterialTypes[:1] }, "materials.material_types[raw_material]"},
		"a group removed":     {true, func(m *Materials) { m.MaterialGroups = m.MaterialGroups[1:] }, "materials.material_groups[BRACKETS]"},
		"the types emptied":   {true, func(m *Materials) { m.MaterialTypes = nil }, "materials.material_types[trading_good]"},
		"the section removed": {true, func(m *Materials) { *m = Materials{} }, "materials.material_groups[FASTEN-01]"},
	} {
		t.Run(name, func(t *testing.T) {
			var base *Config
			if c.base {
				base = withMaterialLists(t)
			}
			proposed := withMaterialLists(t)
			c.change(&proposed.Materials)
			fs := MaterialsTransitions(base, proposed)
			if c.path == "" && len(fs) != 0 {
				t.Errorf("got %v, want none", fs)
			}
			if c.path != "" && !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == c.path }) {
				t.Errorf("got %v, want an error at %s", fs, c.path)
			}
		})
	}
}

// TestMaterialListsTier: the lists post nothing, so setting them is tier
// 2, operational, and a name is cosmetic (ADR-0082 As built S1a4); an
// entry is compared by its code, not its place.
func TestMaterialListsTier(t *testing.T) {
	renamed := withMaterialLists(t)
	renamed.Materials.MaterialGroups[1].Name = "Bolts and nuts"
	swapped := withMaterialLists(t)
	g := swapped.Materials.MaterialGroups
	g[0], g[1] = g[1], g[0]
	cases := []struct {
		name      string
		old, new  *Config
		tier      Tier
		wantPaths string
	}{
		{"set", fixture(t), withMaterialLists(t), TierOperational, "materials.material_groups[BRACKETS] materials.material_groups[FASTEN-01] " +
			"materials.material_types[raw_material] materials.material_types[trading_good]"},
		{"a name", withMaterialLists(t), renamed, TierCosmetic, "materials.material_groups[FASTEN-01].name"},
		{"the order", withMaterialLists(t), swapped, TierOperational, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diffs, _ := Diff(tc.old, tc.new) // Diff never fails
			var paths []string
			for _, d := range diffs {
				paths = append(paths, d.Path)
				if d.Tier != tc.tier {
					t.Errorf("tier of %s: got %v, want %v", d.Path, d.Tier, tc.tier)
				}
			}
			slices.Sort(paths)
			if got := strings.Join(paths, " "); got != tc.wantPaths {
				t.Errorf("changes: got %s, want %s", got, tc.wantPaths)
			}
		})
	}
}
