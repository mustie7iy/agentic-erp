package awscreds

import (
	"net/http"
	"testing"
)

// TestIMDSClientBypassesProxies: the instance metadata service is reached
// directly, never through HTTP_PROXY: its plaintext is exempt from TLS only
// because it never leaves the host (ADR-0033), and Go's proxy rules exempt
// loopback, not link-local addresses.
func TestIMDSClientBypassesProxies(t *testing.T) {
	tr, ok := imdsClient().Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Errorf("transport: got %#v, want one without a proxy", imdsClient().Transport)
	}
}
