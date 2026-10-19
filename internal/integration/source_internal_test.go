package integration

import (
	"context"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// A connector's stored settings are checked again where a run reads them:
// settings that lack the path or base URL (written before a rule, or by
// hand) are a 422 naming the setting, never a panic in the run.
func TestSourceInvalidSettings(t *testing.T) {
	e := &Endpoints{ExtractDir: t.TempDir()}
	for name, c := range map[string]struct {
		connector Connector
		field     string
	}{
		"file without a path":       {Connector{Kind: SAPFile, Settings: map[string]any{}}, "settings.path"},
		"file path not a string":    {Connector{Kind: SAPFile, Settings: map[string]any{"path": 7.0}}, "settings.path"},
		"file path outside the dir": {Connector{Kind: SAPFile, Settings: map[string]any{"path": "../etc/passwd"}}, "settings.path"},
		"odata without a base URL":  {Connector{Kind: SAPOData, Settings: map[string]any{}}, "settings.base_url"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.source(context.Background(), c.connector)
			p := problem.From(err)
			if p.Status != 422 || p.Code != "integration.invalid_settings" {
				t.Fatalf("got %d %s (%v), want 422 integration.invalid_settings", p.Status, p.Code, err)
			}
			if len(p.Errors) != 1 || p.Errors[0].Field != c.field {
				t.Errorf("field: got %v, want %s", p.Errors, c.field)
			}
		})
	}
}

// TestHost: the audit trail keeps a URL's host only, and nothing of a URL
// that does not parse (one written other than through the API).
func TestHost(t *testing.T) {
	for raw, want := range map[string]string{"https://hooks.example.com:8443/erp?token=x": "hooks.example.com:8443", "https://a%zz/": ""} {
		if got := host(raw); got != want {
			t.Errorf("host(%q): got %q, want %q", raw, got, want)
		}
	}
}
