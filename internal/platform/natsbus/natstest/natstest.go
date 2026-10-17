// Package natstest runs an embedded NATS JetStream server for tests of the
// outbox relay and its consumers (ADR-0007, 06-engineering-process §6.6). It
// is test support and is excluded from the coverage figure.
package natstest

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// Server starts JetStream on a random port and returns its URL; it stops
// when the test ends.
func Server(tb testing.TB) string {
	tb.Helper()
	return start(tb, true)
}

// Plain starts a NATS server without JetStream.
func Plain(tb testing.TB) string {
	tb.Helper()
	return start(tb, false)
}

// TLS starts JetStream that speaks only TLS 1.3, presents cert and
// requires a client certificate that clientCAs signed (ADR-0033); it
// returns its tls:// URL.
func TLS(tb testing.TB, cert tls.Certificate, clientCAs *x509.CertPool) string {
	tb.Helper()
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: clientCAs, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13}
	s := run(tb, &server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: tb.TempDir(), NoLog: true, NoSigs: true,
		TLS: true, TLSVerify: true, TLSConfig: cfg})
	return "tls://" + s.Addr().String()
}

func start(tb testing.TB, jetstream bool) string {
	tb.Helper()
	return run(tb, &server.Options{Host: "127.0.0.1", Port: -1, JetStream: jetstream, StoreDir: tb.TempDir(), NoLog: true, NoSigs: true}).ClientURL()
}

func run(tb testing.TB, o *server.Options) *server.Server {
	tb.Helper()
	s, err := server.NewServer(o)
	if err != nil {
		tb.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		tb.Fatal("natstest: server not ready")
	}
	tb.Cleanup(s.Shutdown)
	return s
}

// Restartable is a JetStream server that can be stopped and started again
// on the same port with the same store, for resilience tests of consumers
// and relays that must survive a node loss.
type Restartable struct {
	URL   string
	t     testing.TB
	port  int
	store string
	srv   *server.Server
}

// NewRestartable starts a restartable JetStream server.
func NewRestartable(tb testing.TB) *Restartable {
	tb.Helper()
	l, err := new(net.ListenConfig).Listen(tb.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close() // it only found a free port; were the port still bound, Start would fail and say so
	r := &Restartable{t: tb, port: port, store: tb.TempDir()}
	r.Start()
	tb.Cleanup(r.Stop)
	return r
}

// Start starts the server (again).
func (r *Restartable) Start() {
	r.t.Helper()
	if r.srv != nil {
		return
	}
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: r.port, JetStream: true, StoreDir: r.store, NoLog: true, NoSigs: true})
	if err != nil {
		r.t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		r.t.Fatal("natstest: server not ready")
	}
	r.srv, r.URL = s, s.ClientURL()
}

// Stop shuts the server down; clients see the node disappear.
func (r *Restartable) Stop() {
	if r.srv == nil {
		return
	}
	r.srv.Shutdown()
	r.srv.WaitForShutdown()
	r.srv = nil
}
