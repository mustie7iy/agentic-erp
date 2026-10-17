package tlsconf_test

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

// files writes the authority and a leaf into a directory of the test and
// returns the store reading them by file: references.
func files(t *testing.T, ca *tlstest.CA, leaf tlstest.Leaf) (*tlsconf.Store, string) {
	t.Helper()
	dir := t.TempDir()
	src := tlsconf.Source{
		CA:   "file:" + tlstest.WriteFile(t, dir, "ca.crt", ca.PEM),
		Cert: "file:" + tlstest.WriteFile(t, dir, "tls.crt", leaf.CertPEM),
		Key:  "file:" + tlstest.WriteFile(t, dir, "tls.key", leaf.KeyPEM),
	}
	return &tlsconf.Store{Source: src, Resolver: secrets.Local{}}, dir
}

// TestStoreServesWhatItRead: the roots are the system's and the bundle's;
// the certificate is the one read, and a peer trusting the authority
// accepts it.
func TestStoreServesWhatItRead(t *testing.T) {
	ca := tlstest.NewCA(t)
	leaf := ca.Issue(t, tlstest.Options{})
	s, _ := files(t, ca, leaf)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	cert, err := s.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cert.Leaf.NotAfter.Unix(), leaf.NotAfter.Unix(); got != want {
		t.Errorf("certificate's expiry: got %d, want %d", got, want)
	}
	roots, err := s.RootCAs()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Leaf.Verify(verifyOpts(roots)); err != nil {
		t.Errorf("leaf against the store's roots: got %v, want verified", err)
	}
	if !s.HasCertificate() {
		t.Error("HasCertificate: got false, want true")
	}
}

// TestStoreWithoutMaterial: no bundle trusts the system roots alone; no
// certificate presents none and says so.
func TestStoreWithoutMaterial(t *testing.T) {
	s := &tlsconf.Store{Resolver: secrets.Local{}}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if roots, err := s.RootCAs(); err != nil || roots == nil {
		t.Errorf("roots: got %v, %v, want the system pool", roots, err)
	}
	if s.HasCertificate() {
		t.Error("HasCertificate: got true, want false")
	}
	if _, err := s.Certificate(); !errors.Is(err, tlsconf.ErrNoCertificate) {
		t.Errorf("certificate: got %v, want ErrNoCertificate", err)
	}
}

// TestStoreBeforeRefresh: a store that never read refuses rather than
// handing out an empty pool.
func TestStoreBeforeRefresh(t *testing.T) {
	s := &tlsconf.Store{}
	if _, err := s.RootCAs(); !errors.Is(err, tlsconf.ErrNotLoaded) {
		t.Errorf("roots: got %v, want ErrNotLoaded", err)
	}
	if _, err := s.Certificate(); !errors.Is(err, tlsconf.ErrNotLoaded) {
		t.Errorf("certificate: got %v, want ErrNotLoaded", err)
	}
}

// TestStoreRefusesBadMaterial: each way the material can be wrong fails
// the read with what is wrong, and keeps what was read before.
func TestStoreRefusesBadMaterial(t *testing.T) {
	ca := tlstest.NewCA(t)
	leaf := ca.Issue(t, tlstest.Options{})
	other := tlstest.NewCA(t).Issue(t, tlstest.Options{})
	expired := ca.Issue(t, tlstest.Options{NotAfter: time.Now().Add(-time.Minute)})
	env := map[string]string{"CA": string(ca.PEM), "CERT": string(leaf.CertPEM), "KEY": string(leaf.KeyPEM),
		"JUNK": "not pem", "OTHER_KEY": string(other.KeyPEM), "EXPIRED": string(expired.CertPEM), "EXPIRED_KEY": string(expired.KeyPEM)}
	resolver := secrets.Local{Getenv: func(k string) string { return env[k] }}
	good := tlsconf.Source{CA: "env:CA", Cert: "env:CERT", Key: "env:KEY"}
	for _, c := range []struct {
		name string
		src  tlsconf.Source
		want string
	}{
		{"bundle without certificates", tlsconf.Source{CA: "env:JUNK"}, "CA bundle holds no PEM certificate"},
		{"bundle unresolvable", tlsconf.Source{CA: "file:/nonexistent/ca.crt"}, "CA bundle"},
		{"certificate unresolvable", tlsconf.Source{Cert: "file:/nonexistent/tls.crt", Key: "env:KEY"}, "certificate"},
		{"key unresolvable", tlsconf.Source{Cert: "env:CERT", Key: "file:/nonexistent/tls.key"}, "key"},
		{"certificate without key", tlsconf.Source{Cert: "env:CERT"}, "certificate and key go together"},
		{"key without certificate", tlsconf.Source{Key: "env:KEY"}, "certificate and key go together"},
		{"key of another certificate", tlsconf.Source{Cert: "env:CERT", Key: "env:OTHER_KEY"}, "certificate and key"},
		{"expired certificate", tlsconf.Source{Cert: "env:EXPIRED", Key: "env:EXPIRED_KEY"}, "certificate expired at"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &tlsconf.Store{Source: good, Resolver: resolver}
			if err := s.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			s.Source = c.src
			err := s.Refresh(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refresh: got %v, want an error containing %q", err, c.want)
			}
			if cert, err := s.Certificate(); err != nil || cert.Leaf.NotAfter.Unix() != leaf.NotAfter.Unix() {
				t.Errorf("after a failed read: got %v, want the certificate read before", err)
			}
		})
	}
}

// TestStoreSystemRootsFail: a system pool that cannot be read fails the
// read rather than trusting the bundle alone by surprise.
func TestStoreSystemRootsFail(t *testing.T) {
	s := &tlsconf.Store{Resolver: secrets.Local{}, SystemRoots: func() (*x509.CertPool, error) { return nil, errors.New("no system roots") }}
	if err := s.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "no system roots") {
		t.Fatalf("refresh: got %v, want the system pool's error", err)
	}
}

// TestStoreRunPicksUpRenewal: Run re-reads on its interval, so a renewed
// certificate is presented without a restart; a failed read is logged and
// the store keeps what it had.
func TestStoreRunPicksUpRenewal(t *testing.T) {
	ca := tlstest.NewCA(t)
	first := ca.Issue(t, tlstest.Options{NotAfter: time.Now().Add(time.Hour)})
	s, dir := files(t, ca, first)
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	var logged pgtest.SyncBuffer // Run logs on its goroutine while the test reads
	done := make(chan struct{})
	go func() {
		s.Run(ctx, time.Millisecond, slog.New(slog.NewTextHandler(&logged, nil)))
		close(done)
	}()
	// A half-written renewal: the certificate is new, the key still old.
	renewed := ca.Issue(t, tlstest.Options{NotAfter: time.Now().Add(2 * time.Hour)})
	tlstest.WriteFile(t, dir, "tls.crt", renewed.CertPEM)
	pgtest.Eventually(t, 5*time.Second, func() bool { return strings.Contains(logged.String(), "keeping the material read before") }, "the failed read logged")
	if cert, _ := s.Certificate(); cert.Leaf.NotAfter.Unix() != first.NotAfter.Unix() {
		t.Errorf("during a half-written renewal: got expiry %v, want the first certificate's %v", cert.Leaf.NotAfter, first.NotAfter)
	}
	tlstest.WriteFile(t, dir, "tls.key", renewed.KeyPEM)
	pgtest.Eventually(t, 5*time.Second, func() bool {
		cert, err := s.Certificate()
		return err == nil && cert.Leaf.NotAfter.Unix() == renewed.NotAfter.Unix()
	}, "the renewed certificate served")
	cancel()
	<-done
}

// verifyOpts verifies a leaf for both of its uses against roots.
func verifyOpts(roots *x509.CertPool) x509.VerifyOptions {
	return x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}
}

// TestStoreClock: expiry is checked against the store's clock, so a
// certificate read after its end fails even though the authority issued it
// for now.
func TestStoreClock(t *testing.T) {
	ca := tlstest.NewCA(t)
	leaf := ca.Issue(t, tlstest.Options{})
	s, _ := files(t, ca, leaf)
	s.Now = func() time.Time { return leaf.NotAfter.Add(time.Second) }
	if err := s.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "certificate expired at") {
		t.Fatalf("refresh after expiry: got %v, want expired", err)
	}
}

// TestSourceEnabled: any one setting enables the material.
func TestSourceEnabled(t *testing.T) {
	for _, c := range []struct {
		name string
		src  tlsconf.Source
		want bool
	}{
		{"none", tlsconf.Source{}, false},
		{"bundle", tlsconf.Source{CA: "file:/ca"}, true},
		{"certificate", tlsconf.Source{Cert: "file:/c", Key: "file:/k"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.src.Enabled(); got != c.want {
				t.Errorf("Enabled: got %v, want %v", got, c.want)
			}
		})
	}
}
