package egress_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
)

// TestRefusePrivate: only a public unicast address is dialled, checked on
// the address itself (ADR-0031, ADR-0041).
func TestRefusePrivate(t *testing.T) {
	for _, c := range []struct {
		address string
		allowed bool
	}{
		{"93.184.216.34:443", true},
		{"[2606:2800:220:1:248:1893:25c8:1946]:443", true},
		{"127.0.0.1:443", false},
		{"[::1]:443", false},
		{"10.1.2.3:443", false},
		{"192.168.0.1:443", false},
		{"169.254.169.254:80", false},
		{"[fe80::1]:443", false},
		{"0.0.0.0:443", false},
		{"224.0.0.1:443", false},
		{"100.100.100.200:80", false},
		{"0.1.2.3:443", false},
		{"192.0.0.8:443", false},
		{"198.18.0.1:443", false},
		{"240.0.0.1:443", false},
		{"255.255.255.255:443", false},
		{"[::ffff:127.0.0.1]:443", false},
		{"[::ffff:93.184.216.34]:443", true},
		{"[64:ff9b::a9fe:a9fe]:80", false},
		{"[64:ff9b:1::1]:80", false},
		{"[2002:a9fe:a9fe::1]:80", false},
		{"[2001:0:4136:e378::1]:80", false},
		{"[fc00::1]:443", false},
		{"[::a9fe:a9fe]:80", false},
		{"[fec0::1]:443", false},
		{"[100::1]:443", false},
		{"[2001:db8::1]:443", false},
		{"192.88.99.1:443", false},
		{"192.0.2.1:443", false},
		{"198.51.100.1:443", false},
		{"203.0.113.1:443", false},
		{"no port", false},
		{"not-an-ip:443", false},
	} {
		t.Run(c.address, func(t *testing.T) {
			err := egress.RefusePrivate("tcp", c.address, nil)
			if got := err == nil; got != c.allowed {
				t.Errorf("allowed: got %v (%v), want %v", got, err, c.allowed)
			}
		})
	}
}

// endpoint is a TLS test server and a client that may reach it.
func endpoint(t *testing.T, h http.HandlerFunc) (*httptest.Server, *egress.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	c, err := egress.New(egress.LoopbackForTests(roots))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, c
}

// TestPost: the request arrives as sent and the answer comes back, a
// redirect included, unfollowed.
func TestPost(t *testing.T) {
	t.Run("delivered", func(t *testing.T) {
		srv, c := endpoint(t, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body) // a short read fails the comparison below
			w.Header().Set("X-Echo", r.Header.Get("Webhook-Id")+"|"+string(body))
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("thanks")) // the client's read is what is checked
		})
		resp, err := c.Post(context.Background(), srv.URL+"/hook", http.Header{"Webhook-Id": {"msg_1"}}, []byte(`{"a":1}`))
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if resp.Status != http.StatusAccepted {
			t.Errorf("status: got %d, want %d", resp.Status, http.StatusAccepted)
		}
		if got := resp.Header.Get("X-Echo"); got != `msg_1|{"a":1}` {
			t.Errorf("echo: got %q, want %q", got, `msg_1|{"a":1}`)
		}
		if string(resp.Body) != "thanks" || resp.Truncated {
			t.Errorf("body: got %q (truncated %v), want %q", resp.Body, resp.Truncated, "thanks")
		}
	})
	t.Run("no header", func(t *testing.T) {
		srv, c := endpoint(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		resp, err := c.Post(context.Background(), srv.URL, nil, nil)
		if err != nil || resp.Status != http.StatusNoContent {
			t.Errorf("got %d, %v; want %d", resp.Status, err, http.StatusNoContent)
		}
	})
	t.Run("redirect not followed", func(t *testing.T) {
		srv, c := endpoint(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://169.254.169.254/latest", http.StatusTemporaryRedirect)
		})
		resp, err := c.Post(context.Background(), srv.URL, nil, nil)
		if err != nil || resp.Status != http.StatusTemporaryRedirect {
			t.Errorf("got %d, %v; want %d", resp.Status, err, http.StatusTemporaryRedirect)
		}
	})
	t.Run("answer cut at 64 KB", func(t *testing.T) {
		srv, c := endpoint(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), 3*egress.MaxResponse)) // the client stops reading early
		})
		resp, err := c.Post(context.Background(), srv.URL, nil, nil)
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		if len(resp.Body) != egress.MaxResponse || !resp.Truncated {
			t.Errorf("body: got %d bytes (truncated %v), want %d truncated", len(resp.Body), resp.Truncated, egress.MaxResponse)
		}
	})
}

// TestPostRefused: what never gets an answer is an error.
func TestPostRefused(t *testing.T) {
	srv, c := endpoint(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short")) // the connection closes before the declared length
	})
	plain := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(plain.Close)
	production, err := egress.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, c := range []struct {
		name, url string
		client    *egress.Client
		want      string
	}{
		{"http", plain.URL, c, "not an https URL"},
		{"no host", "https:///hook", c, "not an https URL"},
		{"credentials", strings.Replace(srv.URL, "https://", "https://user:secret@", 1), c, "not an https URL"},
		{"unparsable", "https://exa mple.com/" + strings.Repeat("a", 40), c, "not an https URL"},
		{"loopback in production", srv.URL, production, "not a public unicast address"},
		{"name resolving to loopback", strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/t?token=secret", production, "not a public unicast address"},
		{"cut answer", srv.URL, c, "read the answer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.client.Post(context.Background(), c.url, nil, []byte("{}"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error containing %q", err, c.want)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Errorf("error %q shows the URL's credentials", err)
			}
		})
	}
	t.Run("unknown roots", func(t *testing.T) {
		other, err := egress.New(egress.LoopbackForTests(x509.NewCertPool()))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = other.Post(context.Background(), srv.URL+"/hooks/secret?token=secret", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Errorf("got %v, want a certificate error", err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("error %q shows the URL's path or query", err)
		}
	})
}

// TestWalkthroughReceiver: a client made with the walkthrough's receiver
// posts to it, trusting its certificate; the same server is refused to
// the client every other server has, and to a receiver on another port
// (ADR-0041 As built, product owner 2026-11-04).
func TestWalkthroughReceiver(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	at := netip.MustParseAddrPort(srv.Listener.Addr().String())
	other := at.Port() + 1
	if other == 0 { // the server's port was the last: the one below it differs too
		other = at.Port() - 1
	}
	walk, err := egress.New(egress.WalkthroughReceiver(at, roots))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if resp, err := walk.Post(context.Background(), srv.URL+"/hook", nil, []byte(`{}`)); err != nil || resp.Status != http.StatusNoContent {
		t.Errorf("walkthrough's client: got %d, %v; want 204", resp.Status, err)
	}
	production, err := egress.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	elsewhere, err := egress.New(egress.WalkthroughReceiver(netip.AddrPortFrom(at.Addr(), other), roots))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for name, c := range map[string]*egress.Client{"every other server's client": production, "a receiver on another port": elsewhere} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Post(context.Background(), srv.URL+"/hook", nil, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "not a public unicast address") {
				t.Errorf("got %v, want the loopback address refused", err)
			}
		})
	}
}
