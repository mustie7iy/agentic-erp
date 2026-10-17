package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// authority is a certificate authority of the test, its certificate and
// key written to dir (the module has its own; the platform's tlstest is in
// another module, ADR-0035).
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	file string // the PEM certificate's path
}

func newAuthority(t *testing.T, dir, name string) authority {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: serial(t), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return authority{cert: cert, key: key, file: write(t, dir, name+".crt", "CERTIFICATE", der)}
}

// issue writes a leaf for localhost with the SPIFFE ID id ("" for none) and
// returns its certificate's and key's paths.
func (a authority) issue(t *testing.T, dir, name, id string) (certFile, keyFile string) {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: serial(t), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	if id != "" {
		u, err := url.Parse(id)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return write(t, dir, name+".crt", "CERTIFICATE", der), write(t, dir, name+".key", "PRIVATE KEY", keyDER)
}

// clientTLS is a client configuration presenting the leaf in certFile and
// keyFile ("" for none), trusting a, at the given minimum version.
func (a authority) clientTLS(t *testing.T, certFile, keyFile string, minVersion uint16) *tls.Config {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(a.cert)
	cfg := &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: minVersion}
	if minVersion == tls.VersionTLS12 {
		cfg.MaxVersion = tls.VersionTLS12
	}
	if certFile != "" {
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func serial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func write(t *testing.T, dir, name, kind string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
