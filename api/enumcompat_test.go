package api

import (
	"os"
	"slices"
	"strings"
	"testing"
)

const enumBase = `paths:
  /v1/runs:
    get:
      parameters:
        - name: status
          in: query
          schema:
            type: string
            x-extensible-enum: [open, released, cancelled]
        - $ref: "#/components/parameters/PageSize"
components:
  schemas:
    Exclusion:
      type: object
      properties:
        reason:
          type: string
          x-extensible-enum:
            - blocked
            - method_not_allowed
        kinds:
          oneOf:
            - type: string
              x-extensible-enum: [a, b]
    Tag:
      name: tag
      x-extensible-enum: [x]
`

func TestEnumRemovals(t *testing.T) {
	withoutKinds, _, found := strings.Cut(enumBase, "        kinds:")
	if !found {
		t.Fatal("the base contract has no kinds property to remove")
	}
	for _, c := range []struct {
		name string
		head string
		want []string
	}{
		{"unchanged", enumBase, nil},
		{"a value added", strings.Replace(enumBase, "[open, released, cancelled]", "[open, released, cancelled, held]", 1), nil},
		{"values reordered", strings.Replace(enumBase, "[open, released, cancelled]", "[cancelled, open, released]", 1), nil},
		{
			"the value batch 65 removed", strings.Replace(enumBase, "            - method_not_allowed\n", "", 1),
			[]string{"x-extensible-enum at `#/components/schemas/Exclusion/properties/reason` removed the value `method_not_allowed`"},
		},
		{
			"a parameter's value, with one added before it",
			strings.Replace(strings.Replace(enumBase, "[open, released, cancelled]", "[open, released]", 1),
				"        - name: status\n", "        - name: company\n          in: query\n        - name: status\n", 1),
			[]string{"x-extensible-enum at `#/paths//v1/runs/get/parameters/status:query/schema` removed the value `cancelled`"},
		},
		{
			"the whole list removed, its schema kept", strings.Replace(enumBase, "              x-extensible-enum: [a, b]\n", "", 1),
			[]string{
				"x-extensible-enum at `#/components/schemas/Exclusion/properties/kinds/oneOf/0` removed the value `a`",
				"x-extensible-enum at `#/components/schemas/Exclusion/properties/kinds/oneOf/0` removed the value `b`",
			},
		},
		{
			"a value renamed", strings.Replace(enumBase, "x-extensible-enum: [x]", "x-extensible-enum: [y]", 1),
			[]string{"x-extensible-enum at `#/components/schemas/Tag` removed the value `x`"},
		},
		{"the schema itself removed: oasdiff's to report", withoutKinds, nil},
		{"the list replaced by a scalar", strings.Replace(enumBase, "x-extensible-enum: [x]", "x-extensible-enum: none", 1),
			[]string{"x-extensible-enum at `#/components/schemas/Tag` removed the value `x`"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := EnumRemovals([]byte(enumBase), []byte(c.head))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestEnumRemovalsRefusesAnUnreadableContract(t *testing.T) {
	for _, c := range []struct{ name, base, head, want string }{
		{"base", "paths: [", enumBase, "api: base contract: parse: "},
		{"head", enumBase, "paths: [", "api: head contract: parse: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := EnumRemovals([]byte(c.base), []byte(c.head))
			if err == nil || !strings.HasPrefix(err.Error(), c.want) {
				t.Errorf("got %v, want an error starting %q", err, c.want)
			}
		})
	}
}

// TestEnumRemovalsReadsTheContract: the walk finds the contract's open
// enumerations where they are, so the gate compares what the contract
// really lists (a walk that found none would pass every change).
func TestEnumRemovalsReadsTheContract(t *testing.T) {
	src, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e, err := parseEnums(src)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Count(string(src), extensibleEnum+":")
	if len(e.values) != want || want == 0 {
		t.Errorf("got %d open enumerations, want the contract's %d", len(e.values), want)
	}
	if got, err := EnumRemovals(src, src); err != nil || len(got) != 0 {
		t.Errorf("the contract against itself: got %q, %v, want no removal", got, err)
	}
}
