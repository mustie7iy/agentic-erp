package apitools

import (
	"encoding/json"
	"testing"
)

// TestDecodeInputKeepsIntegers: a model's input decodes with its numbers
// as their text, so an integer past 2^53 reaches the URL unchanged, as it
// would not through float64 (review:batch77).
func TestDecodeInputKeepsIntegers(t *testing.T) {
	in, err := DecodeInput([]byte(`{"page_size": 9007199254740993, "amount": 0.000001}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"page_size": "9007199254740993", "amount": "0.000001"} {
		t.Run(name, func(t *testing.T) {
			if got := paramText(in[name]); got != want {
				t.Errorf("parameter: got %s, want %s", got, want)
			}
		})
	}
	if _, isNumber := in["page_size"].(json.Number); !isNumber {
		t.Errorf("number: got %T, want json.Number", in["page_size"])
	}
	if got := paramText(1e6); got != "1000000" {
		t.Errorf("a float64 from a Go caller: got %s, want 1000000", got)
	}
	if in, err := DecodeInput([]byte(`null`)); err != nil || in == nil {
		t.Errorf("null: got %v, %v, want an empty input", in, err)
	}
}

// TestBody: the body object of an input, or nil without one.
func TestBody(t *testing.T) {
	for name, c := range map[string]struct {
		in   map[string]any
		want bool
	}{
		"object":    {map[string]any{"body": map[string]any{"a": 1}}, true},
		"none":      {map[string]any{}, false},
		"no object": {map[string]any{"body": "text"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Body(c.in) != nil; got != c.want {
				t.Errorf("body found: got %v, want %v", got, c.want)
			}
		})
	}
}
