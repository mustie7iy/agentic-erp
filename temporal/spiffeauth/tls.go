package spiffeauth

import (
	"crypto/tls"
	"fmt"
	"time"

	"go.temporal.io/server/common/rpc/encryption"
)

// TLS13 is Temporal's TLS configuration provider with every configuration
// it gives held to TLS 1.3, which the platform's own services speak to each
// other (ADR-0033); Temporal's configuration has no setting for it.
//
// The engine's own connections, internode (its services and their
// membership) and its clients of the frontend, are also held to Nodes'
// identity on the other side. No authorizer runs on the internode
// services, and every service of the environment has a certificate of
// the same authority: without the pin, the cell's server could call the
// history service directly, past the namespace's claims.
type TLS13 struct {
	encryption.TLSConfigProvider
	// Nodes is the engine's own SPIFFE ID; empty pins no identity.
	Nodes string
}

// GetInternodeServerConfig implements encryption.TLSConfigProvider.
func (p TLS13) GetInternodeServerConfig() (*tls.Config, error) {
	return harden(p.Nodes)(p.TLSConfigProvider.GetInternodeServerConfig())
}

// GetInternodeClientConfig implements encryption.TLSConfigProvider.
func (p TLS13) GetInternodeClientConfig() (*tls.Config, error) {
	return harden(p.Nodes)(p.TLSConfigProvider.GetInternodeClientConfig())
}

// GetFrontendServerConfig implements encryption.TLSConfigProvider. The
// frontend serves the cell's server too, whom the claims authorize.
func (p TLS13) GetFrontendServerConfig() (*tls.Config, error) {
	return harden("")(p.TLSConfigProvider.GetFrontendServerConfig())
}

// GetFrontendClientConfig implements encryption.TLSConfigProvider. The
// frontend presents the engine's own certificate (the erp-cell chart), so
// the engine's clients of it hold it to Nodes too.
func (p TLS13) GetFrontendClientConfig() (*tls.Config, error) {
	return harden(p.Nodes)(p.TLSConfigProvider.GetFrontendClientConfig())
}

// GetRemoteClusterClientConfig implements encryption.TLSConfigProvider.
func (p TLS13) GetRemoteClusterClientConfig(hostname string) (*tls.Config, error) {
	return harden("")(p.TLSConfigProvider.GetRemoteClusterClientConfig(hostname))
}

// GetExpiringCerts implements encryption.TLSConfigProvider.
func (p TLS13) GetExpiringCerts(window time.Duration) (encryption.CertExpirationMap, encryption.CertExpirationMap, error) {
	expiring, expired, err := p.TLSConfigProvider.GetExpiringCerts(window)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: expiring certificates: %w", err)
	}
	return expiring, expired, nil
}

// harden holds a configuration, and those it hands out per connection, to
// TLS 1.3 and, unless peer is empty, to a verified peer of that SPIFFE ID.
// A nil configuration (TLS off for that group) stays nil.
func harden(peer string) func(*tls.Config, error) (*tls.Config, error) {
	var h func(*tls.Config, error) (*tls.Config, error)
	h = func(cfg *tls.Config, err error) (*tls.Config, error) {
		if err != nil || cfg == nil {
			return cfg, err
		}
		c := cfg.Clone()
		c.MinVersion = tls.VersionTLS13
		if peer != "" {
			c.VerifyConnection = pin(peer, c.VerifyConnection)
		}
		if per := c.GetConfigForClient; per != nil {
			c.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) { return h(per(hello)) }
		}
		return c, nil
	}
	return h
}

// pin refuses a connection whose verified peer is not peer, then runs
// what the configuration verified before.
func pin(peer string, next func(tls.ConnectionState) error) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if got := PeerID(&cs); got != peer {
			return fmt.Errorf("spiffeauth: peer %q is not the engine's %q", got, peer)
		}
		if next != nil {
			return next(cs)
		}
		return nil
	}
}
