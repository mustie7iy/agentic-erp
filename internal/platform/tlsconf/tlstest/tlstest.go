// Package tlstest makes certificate authorities and certificates in memory
// for tests of TLS connections (ADR-0033). It is test support and is
// excluded from the coverage figure.
package tlstest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CA is a certificate authority that signs the certificates of one test.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	// PEM is the authority's certificate, what a peer trusts.
	PEM []byte
}

// NewCA makes an authority valid from an hour ago for a day.
func NewCA(tb testing.TB) *CA {
	tb.Helper()
	key := newKey(tb)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(tb),
		Subject:               pkix.Name{CommonName: "tlstest CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		tb.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		tb.Fatal(err)
	}
	return &CA{cert: cert, key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// Leaf is an issued certificate and its key, in PEM.
type Leaf struct {
	CertPEM, KeyPEM []byte
	NotAfter        time.Time
}

// Pair is the leaf as a tls.Certificate.
func (l Leaf) Pair(tb testing.TB) tls.Certificate {
	tb.Helper()
	c, err := tls.X509KeyPair(l.CertPEM, l.KeyPEM)
	if err != nil {
		tb.Fatal(err)
	}
	return c
}

// Options shape a leaf: its names and its validity.
type Options struct {
	DNS      []string // default localhost
	IPs      []net.IP // default 127.0.0.1
	URIs     []string // SPIFFE IDs and the like
	NotAfter time.Time
}

// Issue signs a certificate for servers and clients alike, valid for
// localhost and 127.0.0.1 unless o names others, until o.NotAfter
// (default a day).
func (ca *CA) Issue(tb testing.TB, o Options) Leaf {
	tb.Helper()
	if o.DNS == nil && o.IPs == nil {
		o.DNS, o.IPs = []string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)}
	}
	if o.NotAfter.IsZero() {
		o.NotAfter = time.Now().Add(24 * time.Hour)
	}
	var uris []*url.URL
	for _, s := range o.URIs {
		u, err := url.Parse(s)
		if err != nil {
			tb.Fatal(err)
		}
		uris = append(uris, u)
	}
	key := newKey(tb)
	tmpl := &x509.Certificate{
		SerialNumber: serial(tb),
		Subject:      pkix.Name{CommonName: "tlstest leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     o.NotAfter,
		DNSNames:     o.DNS,
		IPAddresses:  o.IPs,
		URIs:         uris,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		tb.Fatal(err)
	}
	kd, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		tb.Fatal(err)
	}
	return Leaf{
		CertPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}),
		NotAfter: o.NotAfter,
	}
}

// Pool is a certificate pool holding only the authority.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// WriteFile writes data to name in a directory of the test and returns
// its path, for file: references and sslrootcert.
func WriteFile(tb testing.TB, dir, name string, data []byte) string {
	tb.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		tb.Fatal(err)
	}
	return path
}

func newKey(tb testing.TB) *ecdsa.PrivateKey {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	return key
}

func serial(tb testing.TB) *big.Int {
	tb.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		tb.Fatal(err)
	}
	return n
}

// Proxy terminates TLS with cert in front of the plaintext server at
// backend and returns its address: what a test's client dials to reach a
// development server (Temporal, an HTTP stub) over TLS. With clientCAs it
// requires a client certificate they signed; nextProtos are offered by
// ALPN (h2 for gRPC).
func Proxy(tb testing.TB, backend string, cert tls.Certificate, clientCAs *x509.CertPool, nextProtos ...string) string {
	tb.Helper()
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: nextProtos}
	if clientCAs != nil {
		cfg.ClientCAs, cfg.ClientAuth = clientCAs, tls.RequireAndVerifyClientCert
	}
	ctx, cancel := context.WithCancel(context.Background())
	raw, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		tb.Fatal(err)
	}
	ln := tls.NewListener(raw, cfg)
	tb.Cleanup(func() { cancel(); _ = ln.Close() }) // the test is over
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // closed when the test ends
			}
			go relay(ctx, c, backend)
		}
	}()
	return ln.Addr().String()
}

// relay copies between the client and a new connection to backend until
// either side closes.
func relay(ctx context.Context, client net.Conn, backend string) {
	defer func() { _ = client.Close() }() // the relay is over
	server, err := (&net.Dialer{}).DialContext(ctx, "tcp", backend)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }() // the relay is over
	go func() { _, _ = io.Copy(server, client); _ = server.Close() }() // ends when either side closes
	_, _ = io.Copy(client, server)
}
