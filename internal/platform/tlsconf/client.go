package tlsconf

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
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
// timeout per request.
func (s *Store) HTTPClient(timeout time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's default is an *http.Transport
	t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// The transport dials host:port, always with a port.
		host, _, _ := net.SplitHostPort(addr)
		cfg, err := s.ClientConfig(External, host)
		if err != nil {
			return nil, err
		}
		raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("tlsconf: %w", err)
		}
		conn := tls.Client(raw, cfg)
		if err := conn.HandshakeContext(ctx); err != nil {
			_ = raw.Close() // the handshake failed; its error is the one reported
			return nil, fmt.Errorf("tlsconf: handshake with %s: %w", addr, err)
		}
		return conn, nil
	}
	return &http.Client{Timeout: timeout, Transport: t}
}
