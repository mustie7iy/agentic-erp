package tlsconf

import (
	"context"
	"crypto/tls"
	"errors"
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
// handshake in one read: the store's certificate, and clients verified
// against the bundle alone as clientAuth says. It issues no session
// tickets, so a client whose authority leaves the bundle is verified again
// on its next connection rather than resumed.
func (s *Store) ServerConfig(minVersion uint16, clientAuth tls.ClientAuthType) *tls.Config {
	return &tls.Config{MinVersion: minVersion, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		m, err := s.snapshot()
		if err != nil {
			return nil, err
		}
		if m.cert == nil {
			return nil, ErrNoCertificate
		}
		return &tls.Config{MinVersion: minVersion, Certificates: []tls.Certificate{*m.cert}, ClientAuth: clientAuth, ClientCAs: m.bundle,
			SessionTicketsDisabled: true}, nil
	}}
}

// InternalClient is a client to the deployment's own services: TLS 1.3,
// the store's certificate presented, the server verified against the
// bundle alone and accepted only when it presents the SPIFFE ID its
// request's context names (ExpectPeer); a request naming none is refused.
// Bundle and certificate are taken in one read per connection. It
// connects directly, like HTTPClient.
func (s *Store) InternalClient(timeout time.Duration) *http.Client {
	return s.client(timeout, func(ctx context.Context, host string) (*tls.Config, error) {
		want, ok := ExpectedPeer(ctx)
		if !ok {
			return nil, errors.New("tlsconf: the request names no identity the server must present (ExpectPeer)")
		}
		m, err := s.snapshot()
		if err != nil {
			return nil, err
		}
		return &tls.Config{
			MinVersion: Internal,
			ServerName: host,
			RootCAs:    m.bundle,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				if m.cert == nil {
					return &tls.Certificate{}, nil // none: the server decides whether it needs one
				}
				return m.cert, nil
			},
			VerifyConnection: func(cs tls.ConnectionState) error {
				if id := spiffeID(cs.PeerCertificates[0].URIs); id != want {
					return fmt.Errorf("tlsconf: the server presented %q, want %q", id, want)
				}
				return nil
			},
		}, nil
	})
}

// client is a direct client whose every connection is made with the
// configuration config returns for the host.
func (s *Store) client(timeout time.Duration, config func(ctx context.Context, host string) (*tls.Config, error)) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's default is an *http.Transport
	t.Proxy = nil
	t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// The transport dials host:port, always with a port.
		host, _, _ := net.SplitHostPort(addr)
		cfg, err := config(ctx, host)
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

// peerKey keys the SPIFFE ID a request's connection must present.
type peerKey struct{}

// ExpectPeer makes the connection an InternalClient dials for a request
// with ctx accept exactly id: the router names the cell the directory
// names, so a cell's certificate impersonates no other cell. A connection
// is kept for its host and port, which one cell serves (ADR-0033).
func ExpectPeer(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, peerKey{}, id)
}

// ExpectedPeer is the SPIFFE ID ctx names (ExpectPeer).
func ExpectedPeer(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(peerKey{}).(string)
	return id, ok
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
