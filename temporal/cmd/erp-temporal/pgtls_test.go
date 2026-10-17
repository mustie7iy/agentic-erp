package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
)

// sslRequest is PostgreSQL's SSLRequest: length 8, code 80877103.
var sslRequest = []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}

// tlsURL is raw behind a proxy that speaks PostgreSQL's TLS negotiation to
// clients, as the authority a's certificate for localhost, and plaintext
// to the test database, which has no TLS: what production's verify-full
// does is tested against it (as the platform's pgtest.TLS, in another
// module, ADR-0035). The URL names localhost and a as sslrootcert.
func tlsURL(t *testing.T, raw string, a authority, certFile, keyFile string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	p := &pgProxy{ctx: ctx, backend: u.Host, cfg: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}
	go p.serve(ln)
	t.Cleanup(func() { cancel(); _ = ln.Close(); p.closeAll() }) // the test is over; nothing reads these anymore
	u.Host = net.JoinHostPort("localhost", portOf(&url.URL{Host: ln.Addr().String()}))
	q := u.Query()
	q.Set("sslmode", "verify-full")
	q.Set("sslrootcert", a.file)
	u.RawQuery = q.Encode()
	return u.String()
}

type pgProxy struct {
	ctx     context.Context
	backend string
	cfg     *tls.Config
	mu      sync.Mutex
	conns   []net.Conn
}

func (p *pgProxy) track(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns = append(p.conns, c)
}

func (p *pgProxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close() // the test is over
	}
}

func (p *pgProxy) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // closed when the test ends
		}
		p.track(c)
		go p.handle(c)
	}
}

// handle answers an SSLRequest with S and a handshake, then relays the
// rest in plaintext; any other first message is relayed as it is.
func (p *pgProxy) handle(c net.Conn) {
	first := make([]byte, 8)
	if _, err := io.ReadFull(c, first); err != nil {
		_ = c.Close() // the client went away
		return
	}
	client, pending := c, first
	if bytes.Equal(first, sslRequest) {
		if _, err := c.Write([]byte{'S'}); err != nil {
			_ = c.Close() // the client went away
			return
		}
		tc := tls.Server(c, p.cfg)
		if err := tc.HandshakeContext(p.ctx); err != nil {
			_ = c.Close() // the client refused the certificate
			return
		}
		client, pending = tc, nil
	}
	server, err := (&net.Dialer{}).DialContext(p.ctx, "tcp", p.backend)
	if err != nil {
		_ = client.Close() // no database: the client sees the connection end
		return
	}
	p.track(server)
	if _, err := server.Write(pending); err != nil {
		_ = client.Close() // the database went away
		_ = server.Close()
		return
	}
	go func() { _, _ = io.Copy(server, client); _ = server.Close() }() // relays until either side closes
	_, _ = io.Copy(client, server)
	_ = client.Close()
}
