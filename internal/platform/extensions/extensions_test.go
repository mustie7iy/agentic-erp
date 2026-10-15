package extensions_test

import (
	"errors"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

func TestCheck(t *testing.T) {
	for name, c := range map[string]struct {
		ext   map[string]any
		field string // the refused field; "" when accepted
	}{
		"none":               {nil, ""},
		"snake case":         {map[string]any{"cost_center_2": 1, "a": "x"}, ""},
		"upper case":         {map[string]any{"Bad": 1}, "extensions.Bad"},
		"leading digit":      {map[string]any{"1st": 1}, "extensions.1st"},
		"leading underscore": {map[string]any{"_x": 1}, "extensions._x"},
		"hyphen":             {map[string]any{"a-b": 1}, "extensions.a-b"},
		"empty":              {map[string]any{"": 1}, "extensions."},
		"first bad in order": {map[string]any{"ok": 1, "Zed": 1, "Bad": 2}, "extensions.Bad"},
	} {
		t.Run(name, func(t *testing.T) {
			err := extensions.Check(c.ext)
			if c.field == "" {
				if err != nil {
					t.Errorf("got %v, want accepted", err)
				}
				return
			}
			var p *problem.Problem
			if !errors.As(err, &p) {
				t.Fatalf("got %v, want a problem", err)
			}
			if p.Status != 422 {
				t.Errorf("status: got %d, want 422", p.Status)
			}
			if p.Code != "request.invalid_extension" {
				t.Errorf("code: got %s, want request.invalid_extension", p.Code)
			}
			if len(p.Errors) != 1 {
				t.Fatalf("field errors: got %+v, want one", p.Errors)
			}
			if got := p.Errors[0]; got.Field != c.field || got.Code != "invalid_key" {
				t.Errorf("field error: got %s %s, want %s invalid_key", got.Field, got.Code, c.field)
			}
		})
	}
}
