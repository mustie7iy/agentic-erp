package tlsconf

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ID is a service's SPIFFE ID, spiffe://<trustDomain>/<scope>/<service>:
// scope is the cell, or global for the router and the global plane
// (ADR-0033). An ID authorizes a hop; it is never a principal.
func ID(trustDomain, scope, service string) string {
	return "spiffe://" + trustDomain + "/" + scope + "/" + service
}

// PeerID is the SPIFFE ID of the peer a connection verified, or "" when
// it verified none.
func PeerID(cs *tls.ConnectionState) string {
	if cs == nil || len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
		return ""
	}
	return spiffeID(cs.VerifiedChains[0][0].URIs)
}

// ServerConfig serves with the material read last, taken on each
// handshake: the store's certificate, and clients verified against the
// bundle alone as clientAuth says.
func (s *Store) ServerConfig(minVersion uint16, clientAuth tls.ClientAuthType) *tls.Config {
	return &tls.Config{MinVersion: minVersion, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		bundle, err := s.Bundle()
		if err != nil {
			return nil, err
		}
		cert, err := s.Certificate()
		if err != nil {
			return nil, err
		}
		return &tls.Config{MinVersion: minVersion, Certificates: []tls.Certificate{cert}, ClientAuth: clientAuth, ClientCAs: bundle}, nil
	}}
}

// InternalClient is a client to the deployment's own services: TLS 1.3,
// the store's certificate presented, the server verified against the
// bundle alone and accepted only when allow accepts its SPIFFE ID. It
// connects directly, like HTTPClient.
func (s *Store) InternalClient(timeout time.Duration, allow func(id string) bool) *http.Client {
	return s.client(timeout, func(host string) (*tls.Config, error) {
		bundle, err := s.Bundle()
		if err != nil {
			return nil, err
		}
		// ClientConfig fails only before the store read, which Bundle has
		// just ruled out: a store never unreads.
		cfg, _ := s.ClientConfig(Internal, host)
		cfg.RootCAs = bundle
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			id := spiffeID(cs.PeerCertificates[0].URIs)
			if !allow(id) {
				return fmt.Errorf("tlsconf: the server's identity %q is not one this client may reach", id)
			}
			return nil
		}
		return cfg, nil
	})
}

// client is a direct client whose every connection is made with the
// configuration config returns for the host.
func (s *Store) client(timeout time.Duration, config func(host string) (*tls.Config, error)) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's default is an *http.Transport
	t.Proxy = nil
	t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// The transport dials host:port, always with a port.
		host, _, _ := net.SplitHostPort(addr)
		cfg, err := config(host)
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

// spiffeID is the first spiffe URI among uris, or "".
func spiffeID(uris []*url.URL) string {
	for _, u := range uris {
		if u.Scheme == "spiffe" {
			return u.String()
		}
	}
	return ""
}
