package mergepatch_test

import (
	"reflect"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/mergepatch"
)

func TestApply(t *testing.T) {
	cases := map[string]struct {
		target, patch, want map[string]any
	}{
		"nil target":              {nil, map[string]any{"a": "x"}, map[string]any{"a": "x"}},
		"nil patch keeps target":  {map[string]any{"a": "x"}, nil, map[string]any{"a": "x"}},
		"null removes":            {map[string]any{"a": "x", "b": "y"}, map[string]any{"a": nil}, map[string]any{"b": "y"}},
		"null on missing is noop": {map[string]any{"b": "y"}, map[string]any{"a": nil}, map[string]any{"b": "y"}},
		"scalar replaces":         {map[string]any{"a": "x"}, map[string]any{"a": 2.0}, map[string]any{"a": 2.0}},
		"array replaces":          {map[string]any{"a": []any{"x"}}, map[string]any{"a": []any{"y"}}, map[string]any{"a": []any{"y"}}},
		"object merges": {
			map[string]any{"o": map[string]any{"k": "v", "drop": "d"}},
			map[string]any{"o": map[string]any{"n": "w", "drop": nil}},
			map[string]any{"o": map[string]any{"k": "v", "n": "w"}},
		},
		"object over scalar starts empty": {
			map[string]any{"o": "scalar"},
			map[string]any{"o": map[string]any{"n": "w", "gone": nil}},
			map[string]any{"o": map[string]any{"n": "w"}},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := mergepatch.Apply(c.target, c.patch); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestApplyLeavesTargetUnchanged(t *testing.T) {
	target := map[string]any{"a": "x", "o": map[string]any{"k": "v"}}
	mergepatch.Apply(target, map[string]any{"a": nil, "o": map[string]any{"k": "w"}})
	want := map[string]any{"a": "x", "o": map[string]any{"k": "v"}}
	if !reflect.DeepEqual(target, want) {
		t.Errorf("target: got %v, want %v", target, want)
	}
}
