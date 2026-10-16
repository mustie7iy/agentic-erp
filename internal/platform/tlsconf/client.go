package tlsconf

import (
	"crypto/tls"
	"net/http"
	"time"
)

// ClientConfig is a client configuration made from the material read
// last: the store's roots, its certificate when the server asks for one,
// at least minVersion, for serverName. Clients that keep connections (gRPC,
// HTTP) take one per connection, so renewed material reaches new
// connections.
func (s *Store) ClientConfig(minVersion uint16, serverName string) (*tls.Config, error) {
	roots, err := s.RootCAs()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: minVersion,
		ServerName: serverName,
		RootCAs:    roots,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := s.Certificate()
			if err != nil {
				// No certificate is an empty one: the server decides
				// whether it needs one.
				return &tls.Certificate{}, nil //nolint:nilerr // a missing certificate is presented as none, not as a failed handshake
			}
			return &cert, nil
		},
	}, nil
}

// HTTPClient is a client to providers' HTTPS endpoints (S3, KMS, STS)
// whose every connection is made with ClientConfig(External, host), within
// timeout per request. Providers are outside the deployment, so the
// client offers them no certificate. It connects directly: through a
// proxy net/http would handshake with its own configuration, without the
// material (ADR-0033).
func (s *Store) HTTPClient(timeout time.Duration) *http.Client {
	return s.client(timeout, func(host string) (*tls.Config, error) {
		cfg, err := s.ClientConfig(External, host)
		if err != nil {
			return nil, err
		}
		cfg.GetClientCertificate = nil
		return cfg, nil
	})
}
