// Package tlsconf is the process's TLS material (ADR-0033): the CA bundle
// it verifies its peers against, in addition to the system roots, and the
// certificate it presents. Both are read from secret references (ADR-0017)
// and read again while the process runs, so a certificate that
// cert-manager renews is used without a restart.
package tlsconf

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
)

// The lowest TLS versions the deployment speaks: 1.3 between its own
// services, which it controls on both ends; 1.2 to providers' endpoints
// (01-requirements: TLS 1.2+).
const (
	Internal = tls.VersionTLS13
	External = tls.VersionTLS12
)

// RefreshInterval is how often a running process reads its material
// again: well inside the lifetime of a short-lived certificate, which
// cert-manager renews at two thirds of it.
const RefreshInterval = time.Minute

// ErrNotLoaded means the store has not read its material yet.
var ErrNotLoaded = errors.New("tlsconf: material not read yet")

// ErrNoCertificate means the process presents no certificate.
var ErrNoCertificate = errors.New("tlsconf: no certificate configured (ERP_TLS_CERT, ERP_TLS_KEY)")

// Source names the material, each a secret reference or empty.
type Source struct {
	CA   string // ERP_TLS_CA: PEM certificates trusted in addition to the system roots
	Cert string // ERP_TLS_CERT: the PEM certificate chain this process presents
	Key  string // ERP_TLS_KEY: its PEM private key
}

// Enabled reports whether any material is configured.
func (s Source) Enabled() bool { return s != Source{} }

// Store holds the material last read. Its getters never read: handshakes
// take what Refresh, at start and then from Run, read last.
type Store struct {
	Source   Source
	Resolver secrets.Resolver
	// SystemRoots is the pool the bundle adds to; nil is x509.SystemCertPool.
	SystemRoots func() (*x509.CertPool, error)
	// Now is the clock expiry is checked against; nil is time.Now.
	Now func() time.Time

	mu     sync.RWMutex
	loaded bool
	roots  *x509.CertPool
	bundle *x509.CertPool   // the CA bundle alone: the deployment's own authority
	cert   *tls.Certificate // nil: none configured
}

// Refresh reads the material. On an error the store keeps what it read
// before, so a renewal caught half-written is retried, not served.
func (s *Store) Refresh(ctx context.Context) error {
	roots, bundle, err := s.readRoots(ctx)
	if err != nil {
		return err
	}
	cert, err := s.readCertificate(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded, s.roots, s.bundle, s.cert = true, roots, bundle, cert
	return nil
}

// readRoots reads the system roots with the bundle added, and the bundle
// alone (empty without one).
func (s *Store) readRoots(ctx context.Context) (roots, bundle *x509.CertPool, err error) {
	system := s.SystemRoots
	if system == nil {
		system = x509.SystemCertPool
	}
	if roots, err = system(); err != nil {
		return nil, nil, fmt.Errorf("tlsconf: system roots: %w", err)
	}
	bundle = x509.NewCertPool()
	if s.Source.CA == "" {
		return roots, bundle, nil
	}
	pem, err := secrets.Value(ctx, s.Resolver, s.Source.CA)
	if err != nil {
		return nil, nil, fmt.Errorf("tlsconf: CA bundle: %w", err)
	}
	if !roots.AppendCertsFromPEM([]byte(pem)) || !bundle.AppendCertsFromPEM([]byte(pem)) {
		return nil, nil, errors.New("tlsconf: the CA bundle holds no PEM certificate")
	}
	return roots, bundle, nil
}

func (s *Store) readCertificate(ctx context.Context) (*tls.Certificate, error) {
	if (s.Source.Cert == "") != (s.Source.Key == "") {
		return nil, errors.New("tlsconf: the certificate and key go together (ERP_TLS_CERT, ERP_TLS_KEY)")
	}
	if s.Source.Cert == "" {
		return nil, nil
	}
	certPEM, err := secrets.Value(ctx, s.Resolver, s.Source.Cert)
	if err != nil {
		return nil, fmt.Errorf("tlsconf: certificate: %w", err)
	}
	keyPEM, err := secrets.Value(ctx, s.Resolver, s.Source.Key)
	if err != nil {
		return nil, fmt.Errorf("tlsconf: key: %w", err)
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("tlsconf: certificate and key: %w", err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	if expiry := cert.Leaf.NotAfter; now().After(expiry) {
		return nil, fmt.Errorf("tlsconf: the certificate expired at %s", expiry.UTC().Format(time.RFC3339))
	}
	return &cert, nil
}

// RootCAs is the pool peers are verified against: the system roots and
// the bundle.
func (s *Store) RootCAs() (*x509.CertPool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.loaded {
		return nil, ErrNotLoaded
	}
	return s.roots, nil
}

// Bundle is the CA bundle alone, the deployment's own authority: what
// clients' and internal servers' certificates are verified against.
func (s *Store) Bundle() (*x509.CertPool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.loaded {
		return nil, ErrNotLoaded
	}
	return s.bundle, nil
}

// Certificate is the certificate the process presents.
func (s *Store) Certificate() (tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch {
	case !s.loaded:
		return tls.Certificate{}, ErrNotLoaded
	case s.cert == nil:
		return tls.Certificate{}, ErrNoCertificate
	}
	return *s.cert, nil
}

// HasCertificate reports whether the process presents a certificate.
func (s *Store) HasCertificate() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cert != nil
}

// Run reads the material every interval until ctx ends. A failed read is
// logged at error level and the store keeps what it had: connections go
// on with the old certificate until it expires, which the log warns of.
func (s *Store) Run(ctx context.Context, interval time.Duration, log *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
				log.Error("tls: reading the certificates failed; keeping the material read before", "error", err)
			}
		}
	}
}
