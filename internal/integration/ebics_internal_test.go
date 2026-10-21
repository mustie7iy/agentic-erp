package integration

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestEBICSSettings: a bank_ebics channel names the bank's HTTPS URL and
// the subscriber's EBICS ids; settings never carry keys (ADR-0038, T44d).
func TestEBICSSettings(t *testing.T) {
	for name, c := range map[string]struct {
		changes map[string]any
		field   string
	}{
		"valid":                {nil, ""},
		"no url":               {map[string]any{"url": nil}, "settings.url"},
		"plain http":           {map[string]any{"url": "http://ebics.bank.example/ebicsweb"}, "settings.url"},
		"url with credentials": {map[string]any{"url": "https://erp:secret@ebics.bank.example/"}, "settings.url"},
		"url with a query":     {map[string]any{"url": "https://ebics.bank.example/?x=1"}, "settings.url"},
		"url with a fragment":  {map[string]any{"url": "https://ebics.bank.example/#x"}, "settings.url"},
		"url without a host":   {map[string]any{"url": "https:///ebicsweb"}, "settings.url"},
		"url not parsed":       {map[string]any{"url": "https://ebics.bank.example/%zz"}, "settings.url"},
		"no host id":           {map[string]any{"host_id": nil}, "settings.host_id"},
		"partner id too long":  {map[string]any{"partner_id": "P123456789012345678901234567890123456"}, "settings.partner_id"},
		"user id with a space": {map[string]any{"user_id": "USER 1"}, "settings.user_id"},
		"user id a number":     {map[string]any{"user_id": 1.0}, "settings.user_id"},
		"a key":                {map[string]any{"signature_key": "-----BEGIN"}, "settings.signature_key"},
	} {
		t.Run(name, func(t *testing.T) {
			s := map[string]any{"url": "https://ebics.bank.example/ebicsweb", "host_id": "BANKHOST", "partner_id": "PARTNER1", "user_id": "USER1"}
			for k, v := range c.changes {
				if v == nil {
					delete(s, k)
				} else {
					s[k] = v
				}
			}
			err := checkSettings(BankEBICS, s)
			if c.field == "" {
				if err != nil {
					t.Fatalf("got %v, want the settings accepted", err)
				}
				return
			}
			p := problem.From(err)
			if p.Code != "integration.invalid_settings" {
				t.Fatalf("code: got %d %s, want 422 integration.invalid_settings", p.Status, p.Code)
			}
			if len(p.Errors) != 1 || p.Errors[0].Field != c.field {
				t.Errorf("field: got %v, want %s", p.Errors, c.field)
			}
		})
	}
}
