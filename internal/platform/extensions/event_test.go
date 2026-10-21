package extensions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
)

// An event shows only the values of fields the schema declares
// `x-pii: none`: personal, sensitive and undeclared keys are masked.
func TestWritesEvent(t *testing.T) {
	w := extensions.Writes{Entity: "supplier_invoice", Schemas: schemas{s: every()}}
	version := 2
	payload := map[string]any{}
	ext := map[string]any{"s": "abc", "t": "Ada Lovelace", "p": "a partner", "gone": "kept before"}
	if err := w.Event(context.Background(), payload, ext, &version); err != nil {
		t.Fatal(err)
	}
	got := payload["extensions"].(map[string]any)
	for k, want := range map[string]any{"s": "abc", "t": extensions.Masked, "p": extensions.Masked, "gone": extensions.Masked} {
		t.Run(k, func(t *testing.T) {
			if got[k] != want {
				t.Errorf("got %v, want %v", got[k], want)
			}
		})
	}
	if ext["t"] != "Ada Lovelace" {
		t.Errorf("the row's extensions: got %v, want them unchanged", ext["t"])
	}
	if v := payload["extensions_schema"].(*int); *v != 2 {
		t.Errorf("extensions_schema: got %d, want 2", *v)
	}
}

func TestWritesEventWithoutSchema(t *testing.T) {
	w := extensions.Writes{Entity: "purchase_order", Schemas: schemas{}}
	payload := map[string]any{}
	if err := w.Event(context.Background(), payload, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := payload["extensions"].(map[string]any); len(got) != 0 {
		t.Errorf("extensions: got %v, want an empty object", got)
	}
	if v := payload["extensions_schema"].(*int); v != nil {
		t.Errorf("extensions_schema: got %d, want none", *v)
	}
	bad := extensions.Writes{Schemas: schemas{err: errors.New("boom")}}
	if err := bad.Event(context.Background(), payload, nil, nil); err == nil {
		t.Error("schema read: got no error, want one")
	}
}
