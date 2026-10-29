package apitest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
)

// eventSchemas are the event contract's schemas by name, and each event
// type's payload schema name.
type eventSchemas struct {
	schemas  map[string]any
	payloads map[string]string // CloudEvents type -> schema name
}

var (
	schemasOnce sync.Once
	schemasOf   eventSchemas
	schemasErr  error
)

// eventSchemaSet reads api/asyncapi.yaml's schemas as plain values, which
// Conforms walks.
func eventSchemaSet() (eventSchemas, error) {
	schemasOnce.Do(func() {
		b, err := contractFile()
		if err != nil {
			schemasErr = err
			return
		}
		schemasOf, schemasErr = parseEventSchemas(b)
	})
	return schemasOf, schemasErr
}

func parseEventSchemas(b []byte) (eventSchemas, error) {
	var doc struct {
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return eventSchemas{}, fmt.Errorf("apitest: parse event contract: %w", err)
	}
	_, types, err := contractOf(b)
	if err != nil {
		return eventSchemas{}, err
	}
	return eventSchemas{schemas: doc.Components.Schemas, payloads: types}, nil
}

// Conforms fails tb where payload, an event of e's type as Published
// returns it, departs from the payload schema api/asyncapi.yaml declares:
// at any depth, a value of another type, a required field missing, a field
// not declared, a value out of range or not of the pattern, or a value
// matching not exactly one of a oneOf. Published checks the top level of
// every payload; Conforms is for an event whose nested values carry the
// contract.
func Conforms(tb testing.TB, e outbox.Event, payload map[string]any) {
	tb.Helper()
	s, err := eventSchemaSet()
	if err != nil {
		tb.Fatal(err)
	}
	problems, err := s.conformance(e.Type(), payload)
	if err != nil {
		tb.Fatal(err)
	}
	for _, p := range problems {
		tb.Errorf("%s: %s", e.Type(), p)
	}
}

// conformance lists where payload departs from the event type's payload
// schema.
func (s eventSchemas) conformance(typ string, payload map[string]any) ([]string, error) {
	name, ok := s.payloads[typ]
	if !ok {
		return nil, fmt.Errorf("apitest: event type %s is not in api/asyncapi.yaml", typ)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("apitest: encode the %s payload: %w", typ, err)
	}
	// Numbers are read as written (json.Number), so an integer is told
	// from a fraction exactly.
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	_ = d.Decode(&v) // b is the JSON json.Marshal just wrote, which decodes
	return s.check("payload", v, map[string]any{"$ref": "#/components/schemas/" + name}, 0), nil
}

// check lists where v departs from the schema at path.
func (s eventSchemas) check(path string, v any, schema map[string]any, depth int) []string {
	schema, err := s.resolve(schema)
	if err != nil {
		return []string{path + ": " + err.Error()}
	}
	if depth > 32 {
		return []string{path + ": the schema nests deeper than 32 levels"}
	}
	if alts, ok := schema["oneOf"].([]any); ok {
		matched := 0
		for _, alt := range alts {
			if len(s.check(path, v, asSchema(alt), depth+1)) == 0 {
				matched++
			}
		}
		if matched != 1 {
			return []string{fmt.Sprintf("%s: got %s matching %d of oneOf's %d schemas, want exactly 1", path, encoded(v), matched, len(alts))}
		}
		return nil
	}
	if want := typesOf(schema["type"]); want != nil && !hasType(want, v) {
		return []string{fmt.Sprintf("%s: got %s, want %s", path, encoded(v), strings.Join(want, " or "))}
	}
	switch x := v.(type) {
	case map[string]any:
		return s.object(path, x, schema, depth)
	case []any:
		var out []string
		for i, item := range x {
			out = append(out, s.check(fmt.Sprintf("%s[%d]", path, i), item, asSchema(schema["items"]), depth+1)...)
		}
		return out
	case json.Number:
		return bounds(path, x, schema)
	case string:
		if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(x) {
			return []string{fmt.Sprintf("%s: got %q, want it to match %s", path, x, p)}
		}
	}
	return nil
}

// object lists the required fields missing from v, the fields it holds
// that the schema does not declare (unless additionalProperties allows
// them), and where each declared field departs from its schema.
func (s eventSchemas) object(path string, v, schema map[string]any, depth int) []string {
	var out []string
	for _, r := range asList(schema["required"]) {
		if _, ok := v[fmt.Sprint(r)]; !ok {
			out = append(out, fmt.Sprintf("%s: required field %q missing", path, r))
		}
	}
	props := asSchema(schema["properties"])
	for _, k := range slices.Sorted(maps.Keys(v)) {
		prop, declared := props[k]
		if !declared {
			if extra, ok := schema["additionalProperties"]; ok && extra != false {
				prop = extra
			} else {
				out = append(out, fmt.Sprintf("%s: field %q is not declared", path, k))
				continue
			}
		}
		if prop == true {
			continue
		}
		out = append(out, s.check(path+"."+k, v[k], asSchema(prop), depth+1)...)
	}
	return out
}

// resolve follows a schema's $ref within components.schemas.
func (s eventSchemas) resolve(schema map[string]any) (map[string]any, error) {
	for range 32 {
		ref, ok := schema["$ref"].(string)
		if !ok {
			return schema, nil
		}
		name, ok := strings.CutPrefix(ref, "#/components/schemas/")
		target, found := s.schemas[name]
		if !ok || !found {
			return nil, fmt.Errorf("unresolved $ref %q", ref)
		}
		schema = asSchema(target)
	}
	return nil, fmt.Errorf("$ref chain longer than 32")
}

// bounds lists a number outside the schema's minimum or maximum.
func bounds(path string, n json.Number, schema map[string]any) []string {
	r, _ := new(big.Rat).SetString(n.String()) // json.Number holds a valid JSON number, which SetString reads
	if lo, ok := schema["minimum"].(int); ok && r.Cmp(big.NewRat(int64(lo), 1)) < 0 {
		return []string{fmt.Sprintf("%s: got %s, want at least %d", path, n, lo)}
	}
	if hi, ok := schema["maximum"].(int); ok && r.Cmp(big.NewRat(int64(hi), 1)) > 0 {
		return []string{fmt.Sprintf("%s: got %s, want at most %d", path, n, hi)}
	}
	return nil
}

// typesOf is a schema's type as a list, nil when the schema sets none.
func typesOf(t any) []string {
	switch x := t.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, v := range x {
			out = append(out, fmt.Sprint(v))
		}
		return out
	}
	return nil
}

// hasType reports whether a decoded JSON value is one of the types.
func hasType(types []string, v any) bool {
	var is []string
	switch x := v.(type) {
	case nil:
		is = []string{"null"}
	case bool:
		is = []string{"boolean"}
	case string:
		is = []string{"string"}
	case json.Number:
		is = []string{"number"}
		if r, ok := new(big.Rat).SetString(x.String()); ok && r.IsInt() {
			is = append(is, "integer")
		}
	case []any:
		is = []string{"array"}
	case map[string]any:
		is = []string{"object"}
	}
	for _, t := range types {
		for _, got := range is {
			if t == got {
				return true
			}
		}
	}
	return false
}

func asSchema(v any) map[string]any {
	m, _ := v.(map[string]any) // not a mapping: an empty schema, which allows anything
	return m
}

func asList(v any) []any {
	l, _ := v.([]any) // no list: nothing required
	return l
}

func encoded(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
