package apitest

import (
	"strings"
	"testing"
)

// conformsContract is an event contract with one event, which uses each
// part of a schema Conforms reads.
const conformsContract = `
components:
  messages:
    Thing:
      headers:
        allOf:
          - properties:
              type: {const: com.example.erp.test.thing.v1}
      payload: {$ref: '#/components/schemas/ThingV1'}
  schemas:
    Count: {type: integer, minimum: 1, maximum: 9}
    Code: {type: string, pattern: '^[A-Z]+$'}
    Ahead: {type: string, pattern: '^(?=a)'}
    Loop: {$ref: '#/components/schemas/Loop'}
    ThingV1:
      type: object
      required: [code]
      properties:
        code: {$ref: '#/components/schemas/Code'}
        counts:
          oneOf: [{type: object, properties: {n: {$ref: '#/components/schemas/Count'}}}, {type: 'null'}]
        tags: {type: array, items: {type: [string, 'null']}}
        extensions: {type: object, additionalProperties: true}
        labels: {type: object, additionalProperties: {type: string}}
        anything: {}
        flag: {type: boolean}
        ratio: {type: number}
        loop: {$ref: '#/components/schemas/Loop'}
        elsewhere: {$ref: 'other.yaml#/X'}
        ahead: {$ref: '#/components/schemas/Ahead'}
`

func TestConformance(t *testing.T) {
	s, err := parseEventSchemas([]byte(conformsContract))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cases := []struct {
		name    string
		payload map[string]any
		want    string // a problem reported, "" for none
	}{
		{"every part", map[string]any{"code": "AB", "counts": map[string]any{"n": 3}, "tags": []any{"a", nil},
			"extensions": map[string]any{"x": 1}, "labels": map[string]any{"k": "v"}, "anything": []any{1}, "flag": true, "ratio": 0.5}, ""},
		{"null of a oneOf", map[string]any{"code": "AB", "counts": nil}, ""},
		{"required missing", map[string]any{}, `payload: required field "code" missing`},
		{"not declared", map[string]any{"code": "AB", "other": 1}, `payload: field "other" is not declared`},
		{"nested not declared", map[string]any{"code": "AB", "counts": map[string]any{"m": 1}}, "payload.counts: got {\"m\":1} matching 0 of oneOf's 2 schemas"},
		{"another type", map[string]any{"code": "AB", "flag": "yes"}, `payload.flag: got "yes", want boolean`},
		{"an integer's fraction", map[string]any{"code": "AB", "counts": map[string]any{"n": 1.5}}, "matching 0 of oneOf's 2 schemas"},
		{"below the minimum", map[string]any{"code": "AB", "counts": map[string]any{"n": 0}}, "matching 0"},
		{"above the maximum", map[string]any{"code": "AB", "counts": map[string]any{"n": 10}}, "matching 0"},
		{"not of the pattern", map[string]any{"code": "ab"}, `payload.code: got "ab", want it to match ^[A-Z]+$`},
		{"an item of another type", map[string]any{"code": "AB", "tags": []any{1}}, "payload.tags[0]: got 1, want string or null"},
		{"an additional value of another type", map[string]any{"code": "AB", "labels": map[string]any{"k": 1}}, "payload.labels.k: got 1, want string"},
		{"a ref loop", map[string]any{"code": "AB", "loop": 1}, "payload.loop: $ref chain longer than 32"},
		{"a pattern RE2 cannot read", map[string]any{"code": "AB", "ahead": "a"}, "payload.ahead: pattern ^(?=a): error parsing regexp"},
		{"a ref elsewhere", map[string]any{"code": "AB", "elsewhere": 1}, `payload.elsewhere: unresolved $ref "other.yaml#/X"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.conformance("com.example.erp.test.thing.v1", c.payload)
			if err != nil {
				t.Fatalf("conformance: %v", err)
			}
			joined := strings.Join(got, "\n")
			if c.want == "" && len(got) > 0 {
				t.Errorf("got %s, want no problem", joined)
			}
			if c.want != "" && !strings.Contains(joined, c.want) {
				t.Errorf("got %q, want %q among the problems", joined, c.want)
			}
		})
	}
}

func TestConformanceRefuses(t *testing.T) {
	s, err := parseEventSchemas([]byte(conformsContract))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := s.conformance("com.example.erp.test.thing.v1", map[string]any{"code": func() {}}); err == nil {
		t.Error("a payload JSON cannot encode: got no error, want one")
	}
	if _, err := s.conformance("com.example.erp.test.unknown.v1", nil); err == nil {
		t.Error("an unknown event type: got no error, want one")
	}
	if got := s.check("p", 1, map[string]any{}, 33); len(got) != 1 {
		t.Errorf("too deep: got %v, want one problem", got)
	}
	if _, err := parseEventSchemas([]byte("components: [")); err == nil {
		t.Error("an unreadable contract: got no error, want one")
	}
	if encoded(func() {}) == "" {
		t.Error("a value JSON cannot encode: got nothing, want it printed")
	}
}

// TestMigrationRunSummaryIsDeclared: the real contract declares the
// summary's keys, so the figures the example once showed are refused.
func TestMigrationRunSummaryIsDeclared(t *testing.T) {
	s, err := eventSchemaSet()
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.conformance("com.example.erp.integration.migration_run_completed.v1", map[string]any{
		"summary": map[string]any{"records_read": 1, "loaded": map[string]any{"suppliers": 0}}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{`payload.summary: field "records_read" is not declared`, "payload.summary.loaded", `required field "migration_run_id" missing`} {
		if !strings.Contains(joined, want) {
			t.Errorf("got %s, want %q among the problems", joined, want)
		}
	}
}
