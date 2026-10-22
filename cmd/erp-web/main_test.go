package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
	"github.com/mustie7iy/agentic-erp/internal/web"
)

func environment() map[string]string {
	return map[string]string{"ERP_ENV": "development", "ERP_WEB_API_URL": "http://127.0.0.1:1",
		"ERP_WEB_ADDR": "127.0.0.1:0", "ERP_WEB_ADMIN_ADDR": "127.0.0.1:0", "ERP_SHUTDOWN_TIMEOUT": "5s"}
}

// start runs erp-web until the test ends and returns its listeners'
// addresses.
func start(t *testing.T, env map[string]string) (public, admin string, logs *bytes.Buffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	logs = &bytes.Buffer{}
	addrs := make(chan [2]string, 1)
	done := make(chan int, 1)
	go func() {
		done <- cli(ctx, func(k string) string { return env[k] }, logs, func(ln, adminLn net.Listener) {
			addrs <- [2]string{ln.Addr().String(), adminLn.Addr().String()}
		})
	}()
	select {
	case a := <-addrs:
		public, admin = a[0], a[1]
	case code := <-done:
		t.Fatalf("stopped before serving: %d %s", code, logs.String())
	}
	t.Cleanup(func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("stop: exit code got %d, want 0", code)
		}
	})
	return public, admin, logs
}

func fetch(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// TestServes: the workspace and its admin listener serve, and erp-web
// stops cleanly.
func TestServes(t *testing.T) {
	public, admin, logs := start(t, environment())
	for path, want := range map[string]int{"/": 503, "/login": 503, "/health": 200, "/nothing": 404} { // no directory: no sign-in
		t.Run(path, func(t *testing.T) {
			if code, _ := fetch(t, http.DefaultClient, "http://"+public+path); code != want {
				t.Errorf("status: got %d, want %d", code, want)
			}
		})
	}
	for _, path := range []string{"/livez", "/metrics", "/version"} {
		t.Run("admin "+path, func(t *testing.T) {
			if code, _ := fetch(t, http.DefaultClient, "http://"+admin+path); code != 200 {
				t.Errorf("status: got %d, want 200", code)
			}
		})
	}
	if code, _ := fetch(t, http.DefaultClient, "http://"+admin+"/"); code != 404 {
		t.Errorf("admin /: got %d, want 404: the admin listener serves no pages", code)
	}
	if !strings.Contains(logs.String(), "erp-web started") {
		t.Errorf("logs: got %s, want the start", logs.String())
	}
}

// TestServesTLS: with the public certificate the workspace is HTTPS only.
func TestServesTLS(t *testing.T) {
	ca, dir := tlstest.NewCA(t), t.TempDir()
	leaf := ca.Issue(t, tlstest.Options{DNS: []string{"app.example.com"}, IPs: []net.IP{net.IPv4(127, 0, 0, 1)}})
	env := environment()
	env["ERP_WEB_PUBLIC_CERT"] = "file:" + tlstest.WriteFile(t, dir, "tls.crt", leaf.CertPEM)
	env["ERP_WEB_PUBLIC_KEY"] = "file:" + tlstest.WriteFile(t, dir, "tls.key", leaf.KeyPEM)
	public, _, _ := start(t, env)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS12}}}
	if code, body := fetch(t, client, "https://"+public+"/health"); code != 200 || !strings.Contains(body, "erp-web") {
		t.Errorf("over TLS: got %d", code)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+public+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close() // only the status is asserted
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("plain HTTP: got %d, want 400 (TLS only)", resp.StatusCode)
		}
	}
}

// TestBehindTheLoadBalancer: connections from the balancer's subnets
// begin with its PROXY v2 header; one from there without it is not served.
func TestBehindTheLoadBalancer(t *testing.T) {
	env := environment()
	env["ERP_WEB_PROXY_FROM"] = "127.0.0.0/8"
	public, _, _ := start(t, env)
	header := append([]byte("\r\n\r\n\x00\r\nQUIT\n"), 0x21, 0x11, 0, 12, 203, 0, 113, 7, 127, 0, 0, 1)
	header = binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(header, 51000), 443)
	balancer := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, a)
		if err == nil {
			_, err = c.Write(header)
		}
		return c, err
	}}}
	if code, _ := fetch(t, balancer, "http://"+public+"/health"); code != 200 {
		t.Errorf("through the balancer: got %d, want 200", code)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+public+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	// net/http answers the failed header read with a bare 400, or the
	// connection breaks first; either way no page is served.
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close() // only the status is asserted
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("without the balancer's header: got %d, want 400", resp.StatusCode)
		}
	}
}

func TestExitCodes(t *testing.T) {
	busy, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }() // best-effort cleanup of the port the cases collide with
	for name, c := range map[string]struct {
		mutate func(env map[string]string)
		code   int
		want   string
	}{
		"configuration":   {func(e map[string]string) { delete(e, "ERP_WEB_API_URL") }, 2, "ERP_WEB_API_URL is required"},
		"address in use":  {func(e map[string]string) { e["ERP_WEB_ADDR"] = busy.Addr().String() }, 1, "address already in use"},
		"admin in use":    {func(e map[string]string) { e["ERP_WEB_ADMIN_ADDR"] = busy.Addr().String() }, 1, "address already in use"},
		"listener closed": {func(map[string]string) {}, 1, "closed"},
	} {
		t.Run(name, func(t *testing.T) {
			env := environment()
			c.mutate(env)
			var logs bytes.Buffer
			started := func(_, _ net.Listener) {}
			if name == "listener closed" {
				started = func(ln, _ net.Listener) {
					// Closing provokes the serve error under test; a failed close shows as a wrong exit code.
					_ = ln.Close()
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			code := cli(ctx, func(k string) string { return env[k] }, &logs, started)
			cancel()
			if code != c.code {
				t.Errorf("exit code: got %d, want %d (logs %s)", code, c.code, logs.String())
			}
			if !strings.Contains(logs.String(), c.want) {
				t.Errorf("logs: got %s, want them to contain %q", logs.String(), c.want)
			}
		})
	}
	t.Run("workspace", func(t *testing.T) {
		// A configuration LoadConfig would refuse: the API client cannot be made.
		cfg := web.Config{Addr: "127.0.0.1:0", AdminAddr: "127.0.0.1:0", APIURL: &url.URL{Scheme: "http", Host: "[::1"}}
		if err := run(context.Background(), cfg, nil, nil); err == nil || !strings.Contains(err.Error(), "web: API client") {
			t.Errorf("got %v, want the client's error", err)
		}
	})
	// main wires the signal context and exits with the code.
	exit = func(int) {}
	defer func() { exit = nil }()
	t.Setenv("ERP_ENV", "")
	main()
}
