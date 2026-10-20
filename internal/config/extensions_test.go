package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
)

const withExtensions = `
extensions:
  supplier_invoice:
    version: 1
    type: object
    additionalProperties: false
    required: [project]
    properties:
      project: {type: string, maxLength: 20, x-pii: none}
      count: {type: integer, minimum: 0, x-pii: none}
`

func withSchema(t *testing.T) *Config {
	t.Helper()
	c, err := Parse(append(fixtureYAML(t), withExtensions...))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

func TestExtensionsParse(t *testing.T) {
	c := withSchema(t)
	if got := c.Extensions["supplier_invoice"].Properties["project"].MaxLength; got != 20 {
		t.Errorf("project max length: got %d, want 20", got)
	}
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Errorf("got findings %v, want none", fs)
	}
	// A float where a string goes is read as the string it spells.
	quoted, err := Parse(append(fixtureYAML(t), (withExtensions + "      rate: {type: string, enum: [0.5, 1.5], title: 2.5, x-pii: none}\n")...))
	if err != nil {
		t.Fatalf("enum of decimals: %v", err)
	}
	if got := quoted.Extensions["supplier_invoice"].Properties["rate"].Enum; !slices.Equal(got, []string{"0.5", "1.5"}) {
		t.Errorf("enum of decimals: got %v, want [0.5 1.5]", got)
	}
	merged := strings.Replace(string(fixtureYAML(t)), "version: 1\n", "", 1)
	if _, err := Parse([]byte("<<: {version: 1, extensions: {supplier_invoice: {version: 1, type: object, additionalProperties: false, " +
		"properties: {n: {type: integer, minimum: 1.5, x-pii: none}}}}}\n" + merged)); err == nil {
		t.Error("float in a merged extensions section: got parsed, want refused")
	}
	for name, field := range map[string]string{
		"$ref":                `      other: {$ref: "https://example.com/s.json"}`,
		"patternProperties":   `    patternProperties: {"^x": {type: string}}`,
		"items":               `      other: {type: string, items: {type: string}, x-pii: none}`,
		"float bound":         `      other: {type: integer, minimum: 1.5, x-pii: none}`,
		"float version":       `  purchase_order: {version: 1.0, type: object, additionalProperties: false}`,
		"float by alias":      `      other: {type: integer, minimum: *f, x-pii: none}`,
		"float by merge":      `      other: {<<: {minimum: 1.5}, type: integer, x-pii: none}`,
		"float in merge list": `      other: {<<: [{minimum: 1.5}], type: integer, x-pii: none}`,
		"merged section":      `  <<: [{purchase_order: {version: 1, type: object, additionalProperties: false, properties: {n: {minimum: 1.5, type: integer, x-pii: none}}}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			doc := append(fixtureYAML(t), (withExtensions + field + "\n")...)
			if name == "float by alias" { // the anchor outside extensions, on a field yaml truncates silently
				doc = append([]byte("version: &f 1.0\n"), doc[len("version: 1\n"):]...)
			}
			_, err := Parse(doc)
			if err == nil {
				t.Fatal("got parsed, want refused")
			}
			if strings.HasPrefix(name, "float") || name == "merged section" {
				if !strings.Contains(err.Error(), "floating-point") {
					t.Errorf("got %v, want the float refused", err)
				}
			}
		})
	}
}

func TestExtensionsValidate(t *testing.T) {
	c := withSchema(t)
	c.Extensions["journal_entry"] = extensions.Schema{Version: 1, Type: "object", AdditionalProperties: ptr(false)}
	s := c.Extensions["supplier_invoice"]
	s.Properties["bad"] = extensions.Property{Type: "string"}
	findings := Validate(c, currency.Seeded())
	for _, want := range []string{"extensions.journal_entry", "extensions.supplier_invoice.properties.bad.x-pii"} {
		t.Run(want, func(t *testing.T) {
			if !hasPath(findings, want) {
				t.Errorf("got %v, want an error at %s", findings, want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func hasPath(fs []Finding, p string) bool {
	for _, f := range fs {
		if f.Path == p && f.Severity == SevError {
			return true
		}
	}
	return false
}

func TestExtensionTransitions(t *testing.T) {
	widen := func(c *Config) {
		s := c.Extensions["supplier_invoice"]
		p := s.Properties["project"]
		p.MaxLength = 30
		s.Properties["project"] = p
	}
	for name, c := range map[string]struct {
		base   bool
		change func(*Config)
		path   string // of the expected error; "" when none
	}{
		"first version":          {false, func(*Config) {}, ""},
		"first version not 1":    {false, bump, "extensions.supplier_invoice.version"},
		"unchanged":              {true, func(*Config) {}, ""},
		"unchanged but numbered": {true, bump, "extensions.supplier_invoice.version"},
		"changed, same version":  {true, widen, "extensions.supplier_invoice.version"},
		"changed, next version":  {true, func(c *Config) { widen(c); bump(c) }, ""},
		"removed":                {true, func(c *Config) { c.Extensions = nil }, "extensions.supplier_invoice"},
	} {
		t.Run(name, func(t *testing.T) {
			var base *Config
			if c.base {
				base = withSchema(t)
			}
			proposed := withSchema(t)
			c.change(proposed)
			fs := ExtensionTransitions(base, proposed)
			if c.path == "" && len(fs) != 0 {
				t.Errorf("got %v, want none", fs)
			}
			if c.path != "" && !hasPath(fs, c.path) {
				t.Errorf("got %v, want an error at %s", fs, c.path)
			}
		})
	}
}

func bump(c *Config) {
	s := c.Extensions["supplier_invoice"]
	s.Version++
	c.Extensions["supplier_invoice"] = s
}

func TestExtensionTiers(t *testing.T) {
	const p = "extensions.supplier_invoice.properties."
	for name, c := range map[string]struct {
		change func(s *extensions.Schema)
		path   string
		tier   Tier
	}{
		"optional field added": {func(s *extensions.Schema) {
			s.Properties["note"] = extensions.Property{Type: "string", PII: "personal"}
		}, p + "note", TierCosmetic},
		"required field added": {func(s *extensions.Schema) { addRequired(s, "note") }, p + "note", TierOperational},
		"sensitive field added": {func(s *extensions.Schema) {
			s.Properties["note"] = extensions.Property{Type: "string", PII: "sensitive"}
		}, p + "note", TierOperational},
		"field removed":  {func(s *extensions.Schema) { delete(s.Properties, "count") }, p + "count", TierOperational},
		"field widened":  {func(s *extensions.Schema) { setLength(s, 40) }, p + "project", TierCosmetic},
		"field narrowed": {func(s *extensions.Schema) { setLength(s, 10) }, p + "project", TierOperational},
		"made optional":  {func(s *extensions.Schema) { s.Required = nil }, p + "project", TierCosmetic},
		"retitled":       {retitle, p + "project", TierCosmetic},
		"new version":    {func(*extensions.Schema) {}, "extensions.supplier_invoice.version", TierCosmetic},
	} {
		t.Run(name, func(t *testing.T) {
			base, proposed := withSchema(t), withSchema(t)
			s := proposed.Extensions["supplier_invoice"]
			c.change(&s)
			s.Version = 2
			proposed.Extensions["supplier_invoice"] = s
			changes, _ := Diff(base, proposed) // never fails
			got := changeAt(changes, c.path)
			if got == nil {
				t.Fatalf("got changes %v, want one at %s", changes, c.path)
			}
			if got.Tier != c.tier {
				t.Errorf("got %v, want %v", got.Tier, c.tier)
			}
		})
	}
}

func TestExtensionDiffOnlyChangedFields(t *testing.T) {
	base, proposed := withSchema(t), withSchema(t)
	s := proposed.Extensions["supplier_invoice"]
	setLength(&s, 40)
	changes, _ := Diff(base, proposed) // never fails
	if changeAt(changes, "extensions.supplier_invoice.properties.count") != nil {
		t.Errorf("got %v, want no change of count", changes)
	}
	if ch := changeAt(changes, "extensions.supplier_invoice.properties.project"); ch == nil || !strings.Contains(ch.New, `"maxLength":40`) {
		t.Errorf("got %v, want project's new declaration", ch)
	}
	added, _ := Diff(nil, withSchema(t)) // never fails
	if ch := changeAt(added, "extensions.supplier_invoice.version"); ch == nil || ch.Old != "" || ch.New != "1" {
		t.Errorf("got %v, want the version added", ch)
	}
	removed, _ := Diff(withSchema(t), fixture(t)) // never fails
	if ch := changeAt(removed, "extensions.supplier_invoice.properties.project"); ch == nil || !strings.Contains(ch.Old, `"required":true`) {
		t.Errorf("got %v, want project removed", ch)
	}
}

func addRequired(s *extensions.Schema, key string) {
	s.Properties[key] = extensions.Property{Type: "boolean", PII: "none"}
	s.Required = append(s.Required, key)
}

func setLength(s *extensions.Schema, n int) {
	p := s.Properties["project"]
	p.MaxLength = n
	s.Properties["project"] = p
}

func retitle(s *extensions.Schema) {
	p := s.Properties["project"]
	p.Title = "Project code"
	s.Properties["project"] = p
}

func changeAt(changes []Change, path string) *Change {
	for i := range changes {
		if changes[i].Path == path {
			return &changes[i]
		}
	}
	return nil
}

func TestSameSchemaIgnoresRequiredOrder(t *testing.T) {
	a := withSchema(t).Extensions["supplier_invoice"]
	addRequired(&a, "flag")
	b := withSchema(t).Extensions["supplier_invoice"]
	addRequired(&b, "flag")
	b.Required = []string{"flag", "project"}
	if !sameSchema(a, b) {
		t.Error("got different, want the same schema")
	}
}
