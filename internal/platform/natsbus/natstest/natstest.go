// Package natstest runs an embedded NATS JetStream server for tests. It is
// test support and is excluded from the coverage figure.
package natstest

import (
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
