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
type TLS13 struct {
	encryption.TLSConfigProvider
}

// GetInternodeServerConfig implements encryption.TLSConfigProvider.
func (p TLS13) GetInternodeServerConfig() (*tls.Config, error) {
	return min13(p.TLSConfigProvider.GetInternodeServerConfig())
}

// GetInternodeClientConfig implements encryption.TLSConfigProvider.
func (p TLS13) GetInternodeClientConfig() (*tls.Config, error) {
	return min13(p.TLSConfigProvider.GetInternodeClientConfig())
}

// GetFrontendServerConfig implements encryption.TLSConfigProvider.
func (p TLS13) GetFrontendServerConfig() (*tls.Config, error) {
	return min13(p.TLSConfigProvider.GetFrontendServerConfig())
}

// GetFrontendClientConfig implements encryption.TLSConfigProvider.
func (p TLS13) GetFrontendClientConfig() (*tls.Config, error) {
	return min13(p.TLSConfigProvider.GetFrontendClientConfig())
}

// GetRemoteClusterClientConfig implements encryption.TLSConfigProvider.
func (p TLS13) GetRemoteClusterClientConfig(hostname string) (*tls.Config, error) {
	return min13(p.TLSConfigProvider.GetRemoteClusterClientConfig(hostname))
}

// GetExpiringCerts implements encryption.TLSConfigProvider.
func (p TLS13) GetExpiringCerts(window time.Duration) (encryption.CertExpirationMap, encryption.CertExpirationMap, error) {
	expiring, expired, err := p.TLSConfigProvider.GetExpiringCerts(window)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: expiring certificates: %w", err)
	}
	return expiring, expired, nil
}

// min13 holds cfg, and the configurations it hands out per connection, to
// TLS 1.3. A nil configuration (TLS off for that group) stays nil.
func min13(cfg *tls.Config, err error) (*tls.Config, error) {
	if err != nil || cfg == nil {
		return cfg, err
	}
	c := cfg.Clone()
	c.MinVersion = tls.VersionTLS13
	if per := c.GetConfigForClient; per != nil {
		c.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			return min13(per(hello))
		}
	}
	return c, nil
}
