package extensions

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"unicode/utf8"
)

// The limits of ADR-0046: what one tenant's custom fields may cost every
// write of an entity.
const (
	MaxFields       = 50       // per entity
	MaxRowBytes     = 16 << 10 // the encoded extensions of one row
	MaxStringLength = 255      // characters of a string field
	MaxTextLength   = 2000     // characters of a text field
	MaxEnumValues   = 100      // values of an enum field
	maxKeyLength    = 63
	maxTitleLength  = 255
)

// Entities accept custom fields, each once its write path validates them
// (ADR-0046: the purchase requisition and the legal entity follow).
var Entities = []string{"business_partner", "purchase_order", "supplier_invoice"}

// Schema is a tenant's custom field schema for one entity: the restricted
// subset of JSON Schema that ADR-0046 allows, a flat object of typed
// fields. It is read strictly, so a keyword outside the subset ($ref,
// patternProperties, items, ...) is refused by the document's parser.
type Schema struct {
	// Version numbers the schema; a version never changes once activated.
	Version              int                 `yaml:"version" json:"version"`
	Type                 string              `yaml:"type" json:"type"`
	AdditionalProperties *bool               `yaml:"additionalProperties" json:"additionalProperties"`
	Required             []string            `yaml:"required,omitempty" json:"required,omitempty"`
	Properties           map[string]Property `yaml:"properties,omitempty" json:"properties,omitempty"`
}

// Property declares one custom field. Its kind follows from type and format.
type Property struct {
	Type        string   `yaml:"type" json:"type"`
	Format      string   `yaml:"format,omitempty" json:"format,omitempty"`
	MaxLength   int      `yaml:"maxLength,omitempty" json:"maxLength,omitempty"`
	Enum        []string `yaml:"enum,omitempty" json:"enum,omitempty"`
	Minimum     *int64   `yaml:"minimum,omitempty" json:"minimum,omitempty"`
	Maximum     *int64   `yaml:"maximum,omitempty" json:"maximum,omitempty"`
	Title       string   `yaml:"title,omitempty" json:"title,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	PII         PII      `yaml:"x-pii" json:"x-pii"`
}

// Kind is the type of a compiled field.
type Kind string

// The kinds of ADR-0046. There is no floating-point kind.
const (
	String  Kind = "string"  // type string, at most 255 characters
	Text    Kind = "text"    // type string, format text, at most 2000
	Integer Kind = "integer" // type integer
	Decimal Kind = "decimal" // type string, format decimal
	Money   Kind = "money"   // type object, format money: amount and currency
	Date    Kind = "date"    // type string, format date
	Boolean Kind = "boolean" // type boolean
	Enum    Kind = "enum"    // type string with enum
	Partner Kind = "partner" // type string, format partner: a business partner's id
)

// PII is a field's privacy class: personal and sensitive values are masked
// wherever the AI policy does not allow them and never reach analytics.
type PII string

// The privacy classes.
const (
	PIINone      PII = "none"
	PIIPersonal  PII = "personal"
	PIISensitive PII = "sensitive"
)

// Field is a compiled custom field.
type Field struct {
	Key       string
	Kind      Kind
	Required  bool
	MaxLength int      // String and Text: the limit, never 0
	Enum      []string // Enum: the allowed values
	Minimum   *int64   // Integer: optional bounds
	Maximum   *int64
	PII       PII
}

// Compiled is an entity's schema ready to validate rows: its fields in key
// order.
type Compiled struct {
	Entity  string
	Version int
	Fields  []Field
}

// Issue is what is wrong with a schema, at a path relative to it.
type Issue struct {
	Path    string
	Message string
}

// Compile checks a schema against ADR-0046's restrictions and compiles it.
// The compiled schema is returned even with issues, as far as it can be
// read, so a document with issues can still be compared; it is valid only
// when there are none.
func Compile(entity string, s Schema) (*Compiled, []Issue) {
	c := &compiler{}
	if !slices.Contains(Entities, entity) {
		c.issue("", "custom fields are not supported on %q; supported: %v", entity, Entities)
	}
	if s.Version < 1 || s.Version > math.MaxInt32 { // the API serves it as an int32
		c.issue("version", "must be 1 to %d", math.MaxInt32)
	}
	if s.Type != "object" {
		c.issue("type", "must be object")
	}
	if s.AdditionalProperties == nil || *s.AdditionalProperties {
		c.issue("additionalProperties", "must be false: unknown keys are refused")
	}
	if len(s.Properties) > MaxFields {
		c.issue("properties", "at most %d fields", MaxFields)
	}
	required := c.required(s)
	out := &Compiled{Entity: entity, Version: s.Version}
	for _, k := range slices.Sorted(maps.Keys(s.Properties)) {
		f := c.field("properties."+k, s.Properties[k])
		f.Key, f.Required = k, required[k]
		out.Fields = append(out.Fields, f)
	}
	return out, c.issues
}

type compiler struct{ issues []Issue }

func (c *compiler) issue(path, format string, a ...any) {
	c.issues = append(c.issues, Issue{Path: path, Message: fmt.Sprintf(format, a...)})
}

// required checks that every required key is a declared field, once.
func (c *compiler) required(s Schema) map[string]bool {
	out := map[string]bool{}
	for i, k := range s.Required {
		p := fmt.Sprintf("required[%d]", i)
		if _, ok := s.Properties[k]; !ok {
			c.issue(p, "%q is not a declared field", k)
		}
		if out[k] {
			c.issue(p, "%q is listed twice", k)
		}
		out[k] = true
	}
	return out
}

func (c *compiler) field(p string, d Property) Field {
	key := p[len("properties."):]
	if !keyPattern.MatchString(key) || len(key) > maxKeyLength {
		c.issue(p, "field keys are snake_case, starting with a letter, at most %d characters", maxKeyLength)
	}
	f := Field{Kind: c.kind(p, d), PII: d.PII, Minimum: d.Minimum, Maximum: d.Maximum}
	switch d.PII {
	case PIINone, PIIPersonal, PIISensitive:
	default:
		c.issue(p+".x-pii", "must be none, personal or sensitive")
	}
	if utf8.RuneCountInString(d.Title) > maxTitleLength {
		c.issue(p+".title", "at most %d characters", maxTitleLength)
	}
	if utf8.RuneCountInString(d.Description) > MaxTextLength {
		c.issue(p+".description", "at most %d characters", MaxTextLength)
	}
	if f.Kind == "" { // not a custom field type: its other keywords mean nothing
		return f
	}
	f.MaxLength = c.maxLength(p, f.Kind, d.MaxLength)
	if f.Kind == Enum {
		f.Enum = c.enum(p, d.Enum)
	}
	c.bounds(p, f)
	return f
}

// formats are the kinds a type and format declare; an enum is a string
// without a format that lists values.
var formats = map[[2]string]Kind{
	{"string", ""}: String, {"string", "text"}: Text, {"string", "decimal"}: Decimal, {"string", "date"}: Date,
	{"string", "partner"}: Partner,
	{"integer", ""}:       Integer, {"boolean", ""}: Boolean, {"object", "money"}: Money,
}

func (c *compiler) kind(p string, d Property) Kind {
	if d.Type == "string" && d.Format == "cost_object" { // ADR-0046: allowed once cost objects exist (T56)
		c.issue(p+".format", "cost object references are refused until cost objects exist (T56)")
		return ""
	}
	k, ok := formats[[2]string{d.Type, d.Format}]
	if !ok {
		c.issue(p, "type %q with format %q is not a custom field type", d.Type, d.Format)
		return ""
	}
	if d.Enum == nil {
		return k
	}
	if k != String {
		c.issue(p+".enum", "only a string without a format lists values")
		return k
	}
	return Enum
}

func (c *compiler) maxLength(p string, k Kind, n int) int {
	limit := map[Kind]int{String: MaxStringLength, Text: MaxTextLength}[k]
	switch {
	case limit == 0 && n != 0:
		c.issue(p+".maxLength", "only string and text fields have a maximum length")
	case n < 0 || n > limit:
		c.issue(p+".maxLength", "between 1 and %d", limit)
	case n > 0:
		return n
	}
	return limit
}

func (c *compiler) enum(p string, values []string) []string {
	if len(values) == 0 || len(values) > MaxEnumValues {
		c.issue(p+".enum", "between 1 and %d values", MaxEnumValues)
	}
	seen := map[string]bool{}
	for i, v := range values {
		if v == "" || utf8.RuneCountInString(v) > MaxStringLength {
			c.issue(fmt.Sprintf("%s.enum[%d]", p, i), "between 1 and %d characters", MaxStringLength)
		}
		if seen[v] {
			c.issue(fmt.Sprintf("%s.enum[%d]", p, i), "%q is listed twice", v)
		}
		seen[v] = true
	}
	return values
}

func (c *compiler) bounds(p string, f Field) {
	if f.Kind != Integer && (f.Minimum != nil || f.Maximum != nil) {
		c.issue(p, "only integer fields have a minimum or maximum")
	}
	if f.Minimum != nil && f.Maximum != nil && *f.Minimum > *f.Maximum {
		c.issue(p+".minimum", "must not exceed the maximum")
	}
}
