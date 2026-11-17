package secrets_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets/vaulttest"
)

// TestTenantRef: a tenant names only vault secrets under its own
// namespace; the server's environment and files, another tenant's
// secrets and any path that could leave the prefix are refused.
func TestTenantRef(t *testing.T) {
	tenant := uuid.MustParse("0192a000-0000-7000-8000-000000000001")
	other := uuid.MustParse("0192a000-0000-7000-8000-000000000002")
	own := "vault:secret/tenants/" + tenant.String()
	for ref, ok := range map[string]bool{
		own + "/sap#credentials":           true,
		own + "/banks/rbc.key#value":       true,
		"env:DATABASE_URL":                 false,
		"file:/etc/passwd":                 false,
		"vault:secret/erp/field-key#value": false,
		"vault:secret/tenants/" + other.String() + "/sap#credentials": false,
		own + "#credentials":                     false,
		own + "/sap":                             false,
		own + "/sap#":                            false,
		own + "/../" + other.String() + "/sap#x": false,
		own + "/./sap#x":                         false,
		own + "//sap#x":                          false,
		own + "/sap/#x":                          false,
		"vault:/secret/tenants/" + tenant.String() + "/sap#x": false,
		own + "/%2e%2e/sap#x": false,
		own + "/sap?x=1#x":    false,
		own + "/sap#a#b":      false,
		"vault:secret/tenants/" + tenant.String()[:8] + "/sap#x": false,
		"vault:secret/TENANTS/" + tenant.String() + "/sap#x":     false,
	} {
		t.Run(ref, func(t *testing.T) {
			err := secrets.TenantRef(ref, tenant)
			if (err == nil) != ok {
				t.Fatalf("TenantRef: got %v, want accepted %v", err, ok)
			}
			if err != nil && !errors.Is(err, secrets.ErrNotTenants) {
				t.Errorf("error: got %v, want ErrNotTenants", err)
			}
		})
	}
}

// TestResolveTenant: a reference under the tenant's namespace is
// resolved; one outside it never reaches the resolver.
func TestResolveTenant(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.MustParse("0192a000-0000-7000-8000-000000000001")
	v := vaulttest.New(t)
	v.Secrets["secret/tenants/"+tenant.String()+"/sap"] = map[string]any{"credentials": "u:p"}
	v.Secrets["secret/erp/field-key"] = map[string]any{"value": "server-only"}
	r := &secrets.Vault{Address: v.URL, Token: vaulttest.Token}
	got, err := secrets.ResolveTenant(ctx, r, tenant, "vault:secret/tenants/"+tenant.String()+"/sap#credentials")
	if err != nil || got != "u:p" {
		t.Fatalf("own secret: got %q, %v, want u:p", got, err)
	}
	if got, err := secrets.ResolveTenant(ctx, r, tenant, "vault:secret/erp/field-key#value"); !errors.Is(err, secrets.ErrNotTenants) {
		t.Errorf("server's secret: got %q, %v, want ErrNotTenants", got, err)
	}
}
