package egress

import (
	"crypto/x509"
	"net/http"
	"testing"
)

// TestNewBounds: the client has the bounds ADR-0041 sets, and no proxy,
// client certificate or roots of its own in production.
func TestNewBounds(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.http.Timeout != Timeout {
		t.Errorf("timeout: got %v, want %v", c.http.Timeout, Timeout)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport: got %T, want *http.Transport", c.http.Transport)
	}
	if tr.Proxy != nil {
		t.Error("proxy: got a proxy function, want none")
	}
	if tr.TLSHandshakeTimeout != ConnectTimeout {
		t.Errorf("handshake timeout: got %v, want %v", tr.TLSHandshakeTimeout, ConnectTimeout)
	}
	cfg := tr.TLSClientConfig
	if cfg.RootCAs != nil || cfg.Certificates != nil || cfg.GetClientCertificate != nil {
		t.Error("TLS: got own roots or a client certificate, want the system roots and none")
	}
}

// TestLoopbackOnlyInTests: outside a test binary the test option fails
// New rather than opening loopback to tenants' URLs.
func TestLoopbackOnlyInTests(t *testing.T) {
	prev := inTest
	inTest = func() bool { return false }
	t.Cleanup(func() { inTest = prev })
	if _, err := New(LoopbackForTests(x509.NewCertPool())); err == nil {
		t.Error("got a client, want an error")
	}
}

// TestAllowLoopback: the test control adds loopback and nothing else.
func TestAllowLoopback(t *testing.T) {
	for _, c := range []struct {
		address string
		allowed bool
	}{
		{"127.0.0.1:443", true},
		{"[::1]:443", true},
		{"10.0.0.1:443", false},
		{"169.254.169.254:80", false},
		{"93.184.216.34:443", true},
	} {
		t.Run(c.address, func(t *testing.T) {
			if got := allowLoopback("tcp", c.address, nil) == nil; got != c.allowed {
				t.Errorf("allowed: got %v, want %v", got, c.allowed)
			}
		})
	}
}
