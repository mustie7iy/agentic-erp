package extensions_test

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
)

func ptr[T any](v T) *T { return &v }

// schema is a valid supplier invoice schema with one field of every kind.
func schema() extensions.Schema {
	return extensions.Schema{Version: 1, Type: "object", AdditionalProperties: ptr(false), Required: []string{"project"},
		Properties: map[string]extensions.Property{
			"project": {Type: "string", MaxLength: 20, Title: "Project", PII: extensions.PIINone},
			"note":    {Type: "string", Format: "text", PII: extensions.PIIPersonal},
			"count":   {Type: "integer", Minimum: ptr[int64](0), Maximum: ptr[int64](10), PII: extensions.PIINone},
			"rate":    {Type: "string", Format: "decimal", PII: extensions.PIINone},
			"cap":     {Type: "object", Format: "money", PII: extensions.PIINone},
			"due":     {Type: "string", Format: "date", PII: extensions.PIINone},
			"urgent":  {Type: "boolean", PII: extensions.PIINone},
			"channel": {Type: "string", Enum: []string{"web", "mail"}, PII: extensions.PIINone},
			"broker":  {Type: "string", Format: "partner", PII: extensions.PIISensitive},
		}}
}

func TestCompile(t *testing.T) {
	c, issues := extensions.Compile("supplier_invoice", schema())
	if len(issues) != 0 {
		t.Fatalf("got issues %v, want none", issues)
	}
	kinds := map[string]extensions.Kind{}
	for _, f := range c.Fields {
		kinds[f.Key] = f.Kind
	}
	want := map[string]extensions.Kind{"project": extensions.String, "note": extensions.Text, "count": extensions.Integer,
		"rate": extensions.Decimal, "cap": extensions.Money, "due": extensions.Date, "urgent": extensions.Boolean,
		"channel": extensions.Enum, "broker": extensions.Partner}
	for k, kind := range want {
		if kinds[k] != kind {
			t.Errorf("field %s: got kind %q, want %q", k, kinds[k], kind)
		}
	}
	if c.Fields[0].Key != "broker" {
		t.Errorf("first field: got %s, want broker (key order)", c.Fields[0].Key)
	}
	for key, want := range map[string]int{"project": 20, "note": extensions.MaxTextLength, "channel": 0, "count": 0} {
		if f, _ := c.Field(key); f.MaxLength != want {
			t.Errorf("field %s: got max length %d, want %d", key, f.MaxLength, want)
		}
	}
	if f, _ := c.Field("project"); !f.Required {
		t.Error("project: got optional, want required")
	}
	if _, ok := c.Field("missing"); ok {
		t.Error("missing field: got found, want not found")
	}
}

func TestCompileRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		entity string
		change func(*extensions.Schema)
		path   string // of the expected issue
	}{
		"unsupported entity":       {"journal_entry", func(*extensions.Schema) {}, ""},
		"version":                  {"", func(s *extensions.Schema) { s.Version = 0 }, "version"},
		"version above int32":      {"", func(s *extensions.Schema) { s.Version = math.MaxInt32 + 1 }, "version"},
		"type":                     {"", func(s *extensions.Schema) { s.Type = "array" }, "type"},
		"additional allowed":       {"", func(s *extensions.Schema) { s.AdditionalProperties = ptr(true) }, "additionalProperties"},
		"additional unset":         {"", func(s *extensions.Schema) { s.AdditionalProperties = nil }, "additionalProperties"},
		"too many fields":          {"", tooMany, "properties"},
		"required unknown":         {"", func(s *extensions.Schema) { s.Required = append(s.Required, "nope") }, "required[1]"},
		"required twice":           {"", func(s *extensions.Schema) { s.Required = append(s.Required, "project") }, "required[1]"},
		"key case":                 {"", put("Bad", extensions.Property{Type: "boolean", PII: "none"}), "properties.Bad"},
		"key length":               {"", put("k"+strings.Repeat("x", 63), extensions.Property{Type: "boolean", PII: "none"}), "properties.k" + strings.Repeat("x", 63)},
		"no pii":                   {"", put("x", extensions.Property{Type: "boolean"}), "properties.x.x-pii"},
		"title":                    {"", put("x", extensions.Property{Type: "boolean", PII: "none", Title: strings.Repeat("é", 256)}), "properties.x.title"},
		"description":              {"", put("x", extensions.Property{Type: "boolean", PII: "none", Description: strings.Repeat("d", 2001)}), "properties.x.description"},
		"float":                    {"", put("x", extensions.Property{Type: "number", PII: "none"}), "properties.x"},
		"array":                    {"", put("x", extensions.Property{Type: "array", PII: "none"}), "properties.x"},
		"nested object":            {"", put("x", extensions.Property{Type: "object", PII: "none"}), "properties.x"},
		"cost object":              {"", put("x", extensions.Property{Type: "string", Format: "cost_object", PII: "none"}), "properties.x.format"},
		"unknown format":           {"", put("x", extensions.Property{Type: "string", Format: "email", PII: "none"}), "properties.x"},
		"enum with format":         {"", put("x", extensions.Property{Type: "string", Format: "date", Enum: []string{"a"}, PII: "none"}), "properties.x.enum"},
		"enum on an integer":       {"", put("x", extensions.Property{Type: "integer", Enum: []string{"1"}, PII: "none"}), "properties.x.enum"},
		"enum empty":               {"", put("x", extensions.Property{Type: "string", Enum: []string{}, PII: "none"}), "properties.x.enum"},
		"enum too long":            {"", put("x", extensions.Property{Type: "string", Enum: values(101), PII: "none"}), "properties.x.enum"},
		"enum blank value":         {"", put("x", extensions.Property{Type: "string", Enum: []string{""}, PII: "none"}), "properties.x.enum[0]"},
		"enum value too long":      {"", put("x", extensions.Property{Type: "string", Enum: []string{strings.Repeat("v", 256)}, PII: "none"}), "properties.x.enum[0]"},
		"enum value twice":         {"", put("x", extensions.Property{Type: "string", Enum: []string{"a", "a"}, PII: "none"}), "properties.x.enum[1]"},
		"enum with a length":       {"", put("x", extensions.Property{Type: "string", Enum: []string{"a"}, MaxLength: 3, PII: "none"}), "properties.x.maxLength"},
		"string too long":          {"", put("x", extensions.Property{Type: "string", MaxLength: 256, PII: "none"}), "properties.x.maxLength"},
		"text too long":            {"", put("x", extensions.Property{Type: "string", Format: "text", MaxLength: 2001, PII: "none"}), "properties.x.maxLength"},
		"negative length":          {"", put("x", extensions.Property{Type: "string", MaxLength: -1, PII: "none"}), "properties.x.maxLength"},
		"length on a date":         {"", put("x", extensions.Property{Type: "string", Format: "date", MaxLength: 10, PII: "none"}), "properties.x.maxLength"},
		"bounds on a string":       {"", put("x", extensions.Property{Type: "string", Minimum: ptr[int64](1), PII: "none"}), "properties.x"},
		"minimum above maximum":    {"", put("x", extensions.Property{Type: "integer", Minimum: ptr[int64](2), Maximum: ptr[int64](1), PII: "none"}), "properties.x.minimum"},
		"bounds on a money amount": {"", put("x", extensions.Property{Type: "object", Format: "money", Maximum: ptr[int64](1), PII: "none"}), "properties.x"},
	} {
		t.Run(name, func(t *testing.T) {
			s := schema()
			c.change(&s)
			entity := c.entity
			if entity == "" {
				entity = "purchase_order"
			}
			_, issues := extensions.Compile(entity, s)
			var paths []string
			for _, is := range issues {
				paths = append(paths, is.Path)
			}
			if len(issues) == 0 || !contains(paths, c.path) {
				t.Errorf("got issues at %v, want one at %q", paths, c.path)
			}
		})
	}
}

func put(key string, p extensions.Property) func(*extensions.Schema) {
	return func(s *extensions.Schema) { s.Properties[key] = p }
}

func tooMany(s *extensions.Schema) {
	for i := range extensions.MaxFields {
		s.Properties[fmt.Sprintf("f%d", i)] = extensions.Property{Type: "boolean", PII: "none"}
	}
}

func values(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprint("v", i)
	}
	return out
}

func contains(paths []string, p string) bool {
	for _, x := range paths {
		if x == p {
			return true
		}
	}
	return false
}

func TestNarrows(t *testing.T) {
	base := extensions.Field{Key: "x", Kind: extensions.String, MaxLength: 20, PII: extensions.PIINone}
	enum := extensions.Field{Key: "x", Kind: extensions.Enum, Enum: []string{"a", "b"}, PII: extensions.PIINone}
	num := extensions.Field{Key: "x", Kind: extensions.Integer, Minimum: ptr[int64](0), Maximum: ptr[int64](10), PII: extensions.PIINone}
	open := extensions.Field{Key: "x", Kind: extensions.Integer, PII: extensions.PIINone}
	for name, c := range map[string]struct {
		old, new extensions.Field
		want     bool
	}{
		"unchanged":          {base, base, false},
		"longer":             {base, with(base, func(f *extensions.Field) { f.MaxLength = 30 }), false},
		"shorter":            {base, with(base, func(f *extensions.Field) { f.MaxLength = 10 }), true},
		"made required":      {base, with(base, func(f *extensions.Field) { f.Required = true }), true},
		"made optional":      {with(base, func(f *extensions.Field) { f.Required = true }), base, false},
		"retyped":            {base, with(base, func(f *extensions.Field) { f.Kind = extensions.Text }), true},
		"reclassified":       {base, with(base, func(f *extensions.Field) { f.PII = extensions.PIIPersonal }), true},
		"value added":        {enum, with(enum, func(f *extensions.Field) { f.Enum = []string{"a", "b", "c"} }), false},
		"value removed":      {enum, with(enum, func(f *extensions.Field) { f.Enum = []string{"a"} }), true},
		"minimum raised":     {num, with(num, func(f *extensions.Field) { f.Minimum = ptr[int64](1) }), true},
		"minimum lowered":    {num, with(num, func(f *extensions.Field) { f.Minimum = ptr[int64](-1) }), false},
		"maximum lowered":    {num, with(num, func(f *extensions.Field) { f.Maximum = ptr[int64](9) }), true},
		"maximum raised":     {num, with(num, func(f *extensions.Field) { f.Maximum = ptr[int64](11) }), false},
		"bounds dropped":     {num, open, false},
		"bounds added":       {open, num, true},
		"maximum only added": {open, with(open, func(f *extensions.Field) { f.Maximum = ptr[int64](5) }), true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := extensions.Narrows(c.old, c.new); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func with(f extensions.Field, change func(*extensions.Field)) extensions.Field {
	change(&f)
	return f
}

// The compiler never panics on an arbitrary schema, a schema without
// issues stays within the limits, and the same schema never narrows itself.
func FuzzCompile(f *testing.F) {
	seed, _ := json.Marshal(schema())
	f.Add("supplier_invoice", seed)
	f.Add("purchase_order", []byte(`{"version":1,"type":"object","additionalProperties":false,"properties":{"a":{"type":"string","enum":["x","x"],"x-pii":"none"}}}`))
	f.Add("business_partner", []byte(`{"version":1,"type":"object","properties":{"$ref":{"type":"integer","minimum":5,"maximum":1}}}`))
	f.Fuzz(func(t *testing.T, entity string, doc []byte) {
		var s extensions.Schema
		if json.Unmarshal(doc, &s) != nil {
			return
		}
		c, issues := extensions.Compile(entity, s)
		for _, fld := range c.Fields {
			if extensions.Narrows(fld, fld) {
				t.Fatalf("field %s narrows itself", fld.Key)
			}
		}
		if len(issues) > 0 {
			return
		}
		if len(c.Fields) > extensions.MaxFields {
			t.Fatalf("got %d fields, want at most %d", len(c.Fields), extensions.MaxFields)
		}
		for _, fld := range c.Fields {
			if fld.Kind == "" || len(fld.Enum) > extensions.MaxEnumValues || fld.MaxLength > extensions.MaxTextLength {
				t.Fatalf("field %+v is outside the limits", fld)
			}
		}
	})
}
