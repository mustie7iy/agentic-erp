package regulatoryagent

import (
	"testing"
)

// TestHolds: a path is held when each field it names is there and each
// key in brackets names an item of a list by one of its own values, or
// the list's value itself, or an object's entry.
func TestHolds(t *testing.T) {
	doc := version{Document: []byte(`{"companies": [{"code": "1000", "tax_codes": [{"code": "V0", "components": [{"key": "gst",
		"rates": [{"rate_percent": "5", "valid_from": "2025-01-01"}]}]}], "subdivisions": ["AZ", "DU"]}],
		"levels": [{"level": 2, "final": true}], "extensions": {"supplier": {"fields": 1}}, "name": "x"}`)}.document()
	for path, want := range map[string]bool{
		"companies":                     true,
		"companies[1000]":               true,
		"companies[1000].tax_codes[V0]": true,
		"companies[1000].tax_codes[V0].components[gst].rates[2025-01-01]": true,
		"companies[1000].subdivisions[DU]":                                true,
		"levels[2]":                                                       true,
		"levels[true]":                                                    true,
		"extensions[supplier].fields":                                     true,
		"companies[2000]":                                                 false,
		"companies[1000].tax_codes[V9]":                                   false,
		"companies[1000].nothing":                                         false,
		"companies.code":                                                  false,
		"name[x]":                                                         false,
		"Companies":                                                       false,
		"companies[1000]x":                                                false,
		"companies[1000].tax_codes[V0].":                                  false,
		"":                                                                false,
		"extensions[customer]":                                            false,
	} {
		t.Run(path, func(t *testing.T) {
			if got := holds(doc, path); got != want {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
	if holds(version{Document: []byte(`[]`)}.document(), "companies") {
		t.Error("a document that is no object: got a path held, want none")
	}
}

// TestTakesEffect: the second line says when, quoted straight or
// typographically, or that the notice does not state it.
func TestTakesEffect(t *testing.T) {
	for content, want := range map[string]bool{
		"Paths: a\nTakes effect: \"1 July\"\nmore": true,
		"Paths: a\nTakes effect: “1 July”":         true,
		"Paths: a\nTakes effect: not stated":       true,
		"Paths: a\nTakes effect: 1 July":           false,
		"Paths: a\nIt takes effect: \"1 July\"":    false,
		"Paths: a":                                 false,
	} {
		if got := takesEffect(content); got != want {
			t.Errorf("%q: got %v, want %v", content, got, want)
		}
	}
}
