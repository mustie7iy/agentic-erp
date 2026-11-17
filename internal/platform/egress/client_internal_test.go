package egress

import (
	"crypto/x509"
	"net/http"
	"net/netip"
	"testing"
	"time"
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
	if c.limit != MaxResponse {
		t.Errorf("response limit: got %d, want %d", c.limit, MaxResponse)
	}
	cfg := tr.TLSClientConfig
	if cfg.RootCAs != nil || cfg.Certificates != nil || cfg.GetClientCertificate != nil {
		t.Error("TLS: got own roots or a client certificate, want the system roots and none")
	}
}

// TestLoopbackOnlyInTests: outside a test binary the test option fails
// New rather than opening loopback to tenants' URLs.
func TestLoopbackOnlyInTests(t *testing.T) {
	if err := LoopbackDialForTests("tcp", "127.0.0.1:22", nil); err != nil {
		t.Errorf("dial loopback in a test: got %v, want allowed", err)
	}
	prev := inTest
	inTest = func() bool { return false }
	t.Cleanup(func() { inTest = prev })
	if _, err := New(LoopbackForTests(x509.NewCertPool())); err == nil {
		t.Error("got a client, want an error")
	}
	if err := LoopbackDialForTests("tcp", "93.184.216.34:22", nil); err == nil {
		t.Error("dial outside a test: got allowed, want refused")
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

// TestWalkthroughReceiverOption: New takes the walkthrough's receiver
// only as a loopback address with a port and the roots of its
// certificate, and never together with the test option.
func TestWalkthroughReceiverOption(t *testing.T) {
	roots := x509.NewCertPool()
	for name, c := range map[string]struct {
		opts []Option
		ok   bool
	}{
		"loopback":             {[]Option{WalkthroughReceiver(netip.MustParseAddrPort("127.0.0.1:18443"), roots)}, true},
		"IPv6 loopback":        {[]Option{WalkthroughReceiver(netip.MustParseAddrPort("[::1]:18443"), roots)}, true},
		"a private address":    {[]Option{WalkthroughReceiver(netip.MustParseAddrPort("10.0.0.1:18443"), roots)}, false},
		"a public address":     {[]Option{WalkthroughReceiver(netip.MustParseAddrPort("93.184.216.34:443"), roots)}, false},
		"no port":              {[]Option{WalkthroughReceiver(netip.MustParseAddrPort("127.0.0.1:0"), roots)}, false},
		"no address":           {[]Option{WalkthroughReceiver(netip.AddrPort{}, roots)}, false},
		"no roots":             {[]Option{WalkthroughReceiver(netip.MustParseAddrPort("127.0.0.1:18443"), nil)}, false},
		"with the test option": {[]Option{LoopbackForTests(roots), WalkthroughReceiver(netip.MustParseAddrPort("127.0.0.1:18443"), roots)}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(c.opts...); (err == nil) != c.ok {
				t.Errorf("New: got error %v, want accepted %v", err, c.ok)
			}
		})
	}
}

// TestAllowReceiver: the walkthrough's control adds its receiver's one
// address and port and nothing else of the machine.
func TestAllowReceiver(t *testing.T) {
	control := allowReceiver(netip.MustParseAddrPort("127.0.0.1:18443"))
	for _, c := range []struct {
		address string
		allowed bool
	}{
		{"127.0.0.1:18443", true},
		{"[::ffff:127.0.0.1]:18443", true},
		{"127.0.0.1:18444", false},
		{"127.0.0.1:443", false},
		{"127.0.0.2:18443", false},
		{"[::1]:18443", false},
		{"10.0.0.1:18443", false},
		{"169.254.169.254:80", false},
		{"93.184.216.34:443", true},
	} {
		t.Run(c.address, func(t *testing.T) {
			if got := control("tcp", c.address, nil) == nil; got != c.allowed {
				t.Errorf("allowed: got %v, want %v", got, c.allowed)
			}
		})
	}
}

// TestOwnBounds: ResponseLimit and CallTimeout replace the webhook
// client's bounds; the headers stay bounded by MaxResponse.
func TestOwnBounds(t *testing.T) {
	c, err := New(ResponseLimit(10<<20), CallTimeout(time.Minute))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.limit != 10<<20 || c.http.Timeout != time.Minute {
		t.Errorf("bounds: got %d bytes and %v, want %d and %v", c.limit, c.http.Timeout, 10<<20, time.Minute)
	}
	if tr, _ := c.http.Transport.(*http.Transport); tr == nil || tr.MaxResponseHeaderBytes != MaxResponse { // not a transport: nil, refused
		t.Errorf("header bound: got %v, want %d", tr, MaxResponse)
	}
}

// TestDialControl: without a stand-in the control is RefusePrivate; with
// one it lets that loopback address and port through and nothing else of
// the machine; a stand-in off loopback, or without a port, is refused.
func TestDialControl(t *testing.T) {
	if c, err := DialControl(netip.AddrPort{}); err != nil || c("tcp", "127.0.0.1:2222", nil) == nil || c("tcp", "93.184.216.34:22", nil) != nil {
		t.Errorf("no stand-in: got error %v, want RefusePrivate", err)
	}
	c, err := DialControl(netip.MustParseAddrPort("127.0.0.1:2222"))
	if err != nil {
		t.Fatalf("DialControl: %v", err)
	}
	for address, allowed := range map[string]bool{"127.0.0.1:2222": true, "127.0.0.1:2223": false, "10.0.0.1:2222": false, "93.184.216.34:22": true} {
		t.Run(address, func(t *testing.T) {
			if got := c("tcp", address, nil) == nil; got != allowed {
				t.Errorf("allowed: got %v, want %v", got, allowed)
			}
		})
	}
	for _, standIn := range []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:2222"), netip.MustParseAddrPort("127.0.0.1:0")} {
		if _, err := DialControl(standIn); err == nil {
			t.Errorf("stand-in %s: got a control, want an error", standIn)
		}
	}
}
