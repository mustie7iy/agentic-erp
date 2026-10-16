package authn_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestCheckBinding: a bound token needs its certificate; requireBound needs
// a bound token of a binding provider (RFC 8705, ADR-0033).
func TestCheckBinding(t *testing.T) {
	bound := reqctx.Claims{CertThumbprint: "t1", CertificateBound: true}
	for _, c := range []struct {
		name      string
		claims    reqctx.Claims
		presented string
		require   bool
		ok        bool
	}{
		{"bound, its certificate", bound, "t1", true, true},
		{"bound, another certificate", bound, "t2", false, false},
		{"bound, no certificate", bound, "", false, false},
		{"unbound, not required", reqctx.Claims{}, "", false, true},
		{"unbound, required", reqctx.Claims{CertificateBound: true}, "t1", true, false},
		{"bound by a provider that does not bind, required", reqctx.Claims{CertThumbprint: "t1"}, "t1", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := authn.CheckBinding(c.claims, c.presented, c.require); (err == nil) != c.ok {
				t.Errorf("got %v, want accepted %v", err, c.ok)
			}
		})
	}
}
