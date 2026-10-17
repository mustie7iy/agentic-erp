package pgtest

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

// sslRequest is PostgreSQL's SSLRequest: length 8, code 80877103.
var sslRequest = []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}

// TLS returns raw (a database URL) behind a proxy that speaks PostgreSQL's
// TLS negotiation to clients and plaintext to the server: the test
// database runs without TLS, and production's sslmode=verify-full is
// tested against the proxy (ADR-0033). The URL names localhost, which the
// proxy's certificate is for, and its authority as sslrootcert. Clients
// that ask for no TLS are forwarded as they are.
func TLS(tb testing.TB, raw string) string {
	tb.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		tb.Fatal(err)
	}
	backend := u.Host
	ca := tlstest.NewCA(tb)
	cfg := &tls.Config{Certificates: []tls.Certificate{ca.Issue(tb, tlstest.Options{}).Pair(tb)}, MinVersion: tls.VersionTLS12}
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		tb.Fatal(err)
	}
	p := &proxy{ctx: ctx, backend: backend, cfg: cfg}
	go p.serve(ln)
	tb.Cleanup(func() { cancel(); _ = ln.Close(); p.closeAll() }) // the test is over; nothing reads these anymore
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		tb.Fatal(err)
	}
	u.Host = net.JoinHostPort("localhost", port)
	q := u.Query()
	q.Set("sslmode", "verify-full")
	q.Set("sslrootcert", tlstest.WriteFile(tb, tb.TempDir(), "ca.crt", ca.PEM))
	u.RawQuery = q.Encode()
	return u.String()
}

type proxy struct {
	ctx     context.Context // ends with the test
	backend string
	cfg     *tls.Config
	mu      sync.Mutex
	conns   []net.Conn
}

func (p *proxy) track(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns = append(p.conns, c)
}

func (p *proxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close() // the test is over
	}
}

func (p *proxy) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // closed when the test ends
		}
		p.track(c)
		go p.handle(c)
	}
}

// handle answers an SSLRequest with S and a TLS handshake, then relays the
// rest to the server in plaintext; any other first message is relayed as
// it is.
func (p *proxy) handle(c net.Conn) {
	first := make([]byte, 8)
	if _, err := io.ReadFull(c, first); err != nil {
		_ = c.Close() // the client went away before its first message
		return
	}
	client, pending := c, first
	if bytes.Equal(first, sslRequest) {
		if _, err := c.Write([]byte{'S'}); err != nil {
			_ = c.Close()
			return
		}
		tc := tls.Server(c, p.cfg)
		if err := tc.HandshakeContext(p.ctx); err != nil {
			_ = c.Close() // the client refused the certificate: what a test may check
			return
		}
		client, pending = tc, nil
	}
	server, err := (&net.Dialer{}).DialContext(p.ctx, "tcp", p.backend)
	if err != nil {
		_ = client.Close()
		return
	}
	p.track(server)
	if _, err := server.Write(pending); err != nil {
		_ = client.Close()
		_ = server.Close()
		return
	}
	go func() { _, _ = io.Copy(server, client); _ = server.Close() }() // relays until either side closes
	_, _ = io.Copy(client, server)
	_ = client.Close()
}
