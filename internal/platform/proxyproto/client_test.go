package proxyproto

import (
	"net/http/httptest"
	"testing"
)

func TestRemoteIP(t *testing.T) {
	for _, c := range []struct{ remote, want string }{
		{"203.0.113.7:4431", "203.0.113.7"},
		{"[2001:db8::7]:4431", "2001:db8::7"},
		{"[::ffff:203.0.113.7]:4431", "203.0.113.7"}, // a v4 client of a dual-stack listener
		{"[fe80::1%eth0]:4431", "fe80::1"},           // the host's interface is not the client's
		{"pipe", ""},
	} {
		t.Run(c.remote, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remote
			got, ok := RemoteIP(r)
			if want := c.want != ""; ok != want {
				t.Fatalf("ok: got %v, want %v", ok, want)
			}
			if ok && got.String() != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestParseClientAddress(t *testing.T) {
	for _, c := range []struct{ value, want string }{
		{"198.51.100.9", "198.51.100.9"},
		{"2001:db8::9", "2001:db8::9"},
		{"::ffff:198.51.100.9", "198.51.100.9"},
		{"fe80::1%eth0", ""},
		{"198.51.100.9:443", ""},
		{"198.51.100.9, 10.0.0.1", ""},
		{"", ""},
	} {
		t.Run(c.value, func(t *testing.T) {
			got, ok := ParseClientAddress(c.value)
			if want := c.want != ""; ok != want {
				t.Fatalf("ok: got %v, want %v", ok, want)
			}
			if ok && got.String() != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
