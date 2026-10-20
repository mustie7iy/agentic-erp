package integration

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// hostKey is a server key in authorized_keys form.
const hostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl bank"

// sftpSettings are valid bank_sftp settings with the changes.
func sftpSettings(changes map[string]any) map[string]any {
	s := map[string]any{"host": "sftp.bank.example", "port": 2222.0, "user": "erp", "host_key": hostKey, "directory": "/in"}
	for k, v := range changes {
		if v == nil {
			delete(s, k)
		} else {
			s[k] = v
		}
	}
	return s
}

// TestSFTPSettings: a bank_sftp channel names the server, its pinned host
// key and the directory; settings never carry a password (ADR-0038).
func TestSFTPSettings(t *testing.T) {
	for name, c := range map[string]struct {
		changes map[string]any
		field   string
	}{
		"valid":                {nil, ""},
		"default port":         {map[string]any{"port": nil}, ""},
		"no host":              {map[string]any{"host": nil}, "settings.host"},
		"host with a port":     {map[string]any{"host": "sftp.bank.example:22"}, "settings.host"},
		"user with a password": {map[string]any{"user": "erp:secret"}, "settings.user"},
		"empty user":           {map[string]any{"user": ""}, "settings.user"},
		"port out of range":    {map[string]any{"port": 70000.0}, "settings.port"},
		"port a fraction":      {map[string]any{"port": 22.5}, "settings.port"},
		"port a string":        {map[string]any{"port": "22"}, "settings.port"},
		"port zero":            {map[string]any{"port": 0.0}, "settings.port"},
		"no host key":          {map[string]any{"host_key": nil}, "settings.host_key"},
		"host key not a key":   {map[string]any{"host_key": "SHA256:fingerprint"}, "settings.host_key"},
		"relative directory":   {map[string]any{"directory": "in"}, "settings.directory"},
		"no directory":         {map[string]any{"directory": nil}, "settings.directory"},
		"a password":           {map[string]any{"password": "secret"}, "settings.password"},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkSettings(BankSFTP, sftpSettings(c.changes))
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
