package workflow

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"google.golang.org/grpc/credentials"
)

// reloading is gRPC transport credentials that make the TLS configuration
// of each connection when it is opened, so a certificate renewed while the
// client runs is presented on the next connection (ADR-0033).
type reloading struct {
	config func() (*tls.Config, error)
}

func (r reloading) ClientHandshake(ctx context.Context, authority string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	cfg, err := r.config()
	if err != nil {
		return nil, nil, fmt.Errorf("workflow: tls: %w", err)
	}
	c, info, err := credentials.NewTLS(cfg).ClientHandshake(ctx, authority, conn)
	if err != nil {
		return nil, nil, fmt.Errorf("workflow: tls: %w", err)
	}
	return c, info, nil
}

// ServerHandshake is never called: the engine's client only dials.
func (reloading) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("workflow: tls: client credentials only")
}

func (reloading) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls"}
}

func (r reloading) Clone() credentials.TransportCredentials { return r }

// OverrideServerName is deprecated in gRPC and unused; the configuration
// names the server.
func (reloading) OverrideServerName(string) error { return nil }
