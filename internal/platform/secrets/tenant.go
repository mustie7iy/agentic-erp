package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ErrNotTenants means a reference a tenant gave is not under its own
// namespace: the resolver is the server's, so it would read whatever the
// server may read, another tenant's secrets and the server's own included
// (product owner 2026-11-16, ADR-0017 As built).
var ErrNotTenants = errors.New("secrets: not a reference under the tenant's namespace")

// TenantNamespace is the path segment every tenant-given reference
// carries after its mount: vault:<mount>/tenants/<tenant_id>/<path>#<field>.
const TenantNamespace = "tenants"

// TenantRef checks that ref, given by a tenant, names a secret under that
// tenant's namespace, vault:<mount>/tenants/<tenant_id>/<path>#<field>. An
// env: or file: reference is refused, as is any other vault path: the
// replica's environment and files are the server's, and the vault
// policy grants per tenant only by that prefix. The segments are held to
// letters, digits, '.', '_' and '-', never "." or "..", so no reading of
// the path (Vault's, a proxy's, a URL decoder's) can leave the prefix.
func TenantRef(ref string, tenant uuid.UUID) error {
	rest, ok := strings.CutPrefix(ref, SchemeVault+":")
	if !ok {
		return fmt.Errorf("%w: only vault references, never env or file ones", ErrNotTenants)
	}
	location, field, ok := strings.Cut(rest, "#")
	if !ok || !segment(field) {
		return fmt.Errorf("%w: a field after #", ErrNotTenants)
	}
	parts := strings.Split(location, "/")
	if len(parts) < 4 || parts[1] != TenantNamespace || parts[2] != tenant.String() {
		return fmt.Errorf("%w: vault:<mount>/%s/%s/<path>#<field>", ErrNotTenants, TenantNamespace, tenant)
	}
	for _, p := range parts {
		if !segment(p) {
			return fmt.Errorf("%w: path segments of letters, digits, '.', '_' and '-'", ErrNotTenants)
		}
	}
	return nil
}

// segment tells whether s is a path segment a tenant reference may hold.
func segment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-'
	})
}

// ResolveTenant resolves ref, given by the tenant, through the server's
// resolver only when it is under the tenant's namespace (TenantRef): a
// reference stored before the rule, or written around the API, is refused
// on use too.
func ResolveTenant(ctx context.Context, r Resolver, tenant uuid.UUID, ref string) (string, error) {
	if err := TenantRef(ref, tenant); err != nil {
		return "", err
	}
	return r.Resolve(ctx, ref)
}
