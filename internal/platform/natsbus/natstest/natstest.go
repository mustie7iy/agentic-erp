// Package natstest runs an embedded NATS JetStream server for tests. It is
// test support and is excluded from the coverage figure.
package natstest

import (
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// Server starts JetStream on a random port and returns its URL; it stops
// when the test ends.
func Server(t testing.TB) string { return start(t, true) }

// Plain starts a NATS server without JetStream.
func Plain(t testing.TB) string { return start(t, false) }

func start(t testing.TB, jetstream bool) string {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: jetstream, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("natstest: server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s.ClientURL()
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
func NewRestartable(t testing.TB) *Restartable {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close() // it only found a free port; were the port still bound, Start would fail and say so
	r := &Restartable{t: t, port: port, store: t.TempDir()}
	r.Start()
	t.Cleanup(r.Stop)
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
