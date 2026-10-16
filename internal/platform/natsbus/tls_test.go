package natsbus_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus/natstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

// store reads the authority as its bundle and leaf as its certificate,
// when there is one.
func store(t *testing.T, ca *tlstest.CA, leaf *tlstest.Leaf) *tlsconf.Store {
	t.Helper()
	dir := t.TempDir()
	src := tlsconf.Source{CA: "file:" + tlstest.WriteFile(t, dir, "ca.crt", ca.PEM)}
	if leaf != nil {
		src.Cert = "file:" + tlstest.WriteFile(t, dir, "tls.crt", leaf.CertPEM)
		src.Key = "file:" + tlstest.WriteFile(t, dir, "tls.key", leaf.KeyPEM)
	}
	s := &tlsconf.Store{Source: src, Resolver: secrets.Local{}}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestConnectOverMutualTLS: with the store's options the bus reaches a
// server that requires TLS 1.3 and a client certificate, presenting the
// cell's certificate and verifying the server's against the bundle
// (ADR-0033).
func TestConnectOverMutualTLS(t *testing.T) {
	ca := tlstest.NewCA(t)
	url := natstest.TLS(t, ca.Issue(t, tlstest.Options{}).Pair(t), ca.Pool())
	cell := ca.Issue(t, tlstest.Options{})
	bus, err := natsbus.Connect(url, natsbus.TLS(store(t, ca, &cell))...)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	if err := bus.EnsureStreams(context.Background(), "eu1"); err != nil {
		t.Errorf("streams over TLS: got %v, want created", err)
	}
	for _, c := range []struct {
		name string
		opts []nats.Option
		want string
	}{
		// Without a certificate the server refuses the handshake.
		{"no certificate", natsbus.TLS(store(t, ca, nil)), ""},
		// A server certificate from an authority the bundle lacks is refused.
		{"untrusted server", natsbus.TLS(store(t, tlstest.NewCA(t), &cell)), "certificate"},
		// No TLS options against a TLS server: no plaintext either.
		{"plaintext", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			opts := append(slices.Clone(c.opts), nats.Timeout(2*time.Second), nats.NoReconnect())
			if _, err := natsbus.Connect(url, opts...); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("connect: got %v, want refused (%q)", err, c.want)
			}
		})
	}
}

// TestTLSOptionsBeforeRefresh: options from a store that never read fail
// the connection rather than connecting without its material.
func TestTLSOptionsBeforeRefresh(t *testing.T) {
	if _, err := natsbus.Connect(natstest.Server(t), natsbus.TLS(&tlsconf.Store{})...); err == nil {
		t.Error("connect with an unread store: got connected, want refused")
	}
}
