package egress_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
)

// names is a resolver answering from a table; a name not in it fails.
type names map[string][]netip.Addr

func (n names) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := n[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

// TestCheckURL: an endpoint is accepted when written as the client posts
// to it and its host is, or resolves only to, public addresses (ADR-0041).
func TestCheckURL(t *testing.T) {
	r := names{
		"hooks.example.com": {netip.MustParseAddr("93.184.216.34")},
		"mixed.example.com": {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.1")},
		"empty.example.com": {},
	}
	for _, c := range []struct {
		name, url, want string // want "" accepts
	}{
		{"name", "https://hooks.example.com/erp?token=x", ""},
		{"port", "https://hooks.example.com:8443/erp", ""},
		{"public address", "https://93.184.216.34/erp", ""},
		{"http", "http://hooks.example.com/erp", "not an https URL"},
		{"no host", "https:///erp", "not an https URL"},
		{"port only", "https://:443/erp", "not an https URL"},
		{"opaque", "https:hooks.example.com", "not an https URL"},
		{"credentials", "https://u:secret@hooks.example.com/", "not an https URL"},
		{"fragment", "https://hooks.example.com/erp#x", "not an https URL"},
		{"space", "https://hooks.example.com/e rp", "not an https URL"},
		{"unparsable", "https://hooks.example.com/%zz", "not an https URL"},
		{"too long", "https://hooks.example.com/" + strings.Repeat("a", egress.MaxURL), "not an https URL"},
		{"loopback address", "https://127.0.0.1/erp", "not a public unicast address"},
		{"private address", "https://[fd00::1]/erp", "not a public unicast address"},
		{"metadata address", "https://169.254.169.254/latest", "not a public unicast address"},
		{"unknown name", "https://nowhere.example.com/erp", "does not resolve"},
		{"no addresses", "https://empty.example.com/erp", "does not resolve"},
		{"a private address among public ones", "https://mixed.example.com/erp", "resolves to 10.0.0.1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := egress.CheckURL(context.Background(), r, c.url, netip.AddrPort{})
			if c.want == "" {
				if err != nil {
					t.Errorf("got %v, want accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error containing %q", err, c.want)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Errorf("error %q shows the URL's credentials", err)
			}
		})
	}
}

// TestCheckURLReceiver: with the walkthrough's receiver a URL naming its
// address and port is accepted, and every other loopback URL still
// refused; without it (every other server) the same URL is refused.
func TestCheckURLReceiver(t *testing.T) {
	r := names{"localhost": {netip.MustParseAddr("127.0.0.1")}}
	for _, c := range []struct {
		name, url, receiver string
		ok                  bool
	}{
		{"the receiver", "https://127.0.0.1:18443/hook?token=x", "127.0.0.1:18443", true},
		{"the receiver, IPv6", "https://[::1]:18443/hook", "[::1]:18443", true},
		{"the receiver on 443", "https://127.0.0.1/hook", "127.0.0.1:443", true},
		{"without a receiver", "https://127.0.0.1:18443/hook", "", false},
		{"another port", "https://127.0.0.1:18444/hook", "127.0.0.1:18443", false},
		{"the default port", "https://127.0.0.1/hook", "127.0.0.1:18443", false},
		{"another loopback address", "https://127.0.0.2:18443/hook", "127.0.0.1:18443", false},
		{"a name for it", "https://localhost:18443/hook", "127.0.0.1:18443", false},
		{"a port out of range", "https://127.0.0.1:99999/hook", "127.0.0.1:18443", false},
		{"http", "http://127.0.0.1:18443/hook", "127.0.0.1:18443", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var receiver netip.AddrPort
			if c.receiver != "" {
				receiver = netip.MustParseAddrPort(c.receiver)
			}
			if err := egress.CheckURL(context.Background(), r, c.url, receiver); (err == nil) != c.ok {
				t.Errorf("got %v, want accepted %v", err, c.ok)
			}
		})
	}
}
