// Package natsbus publishes domain events to NATS JetStream (ADR-0007): one
// stream per context and cell, binary-mode CloudEvents, and de-duplication on
// the event id so a relay retry never delivers an event twice.
package natsbus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
)

// DuplicateWindow is how long JetStream remembers event ids for de-duplication.
const DuplicateWindow = 10 * time.Minute

// Bus is a JetStream connection.
type Bus struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// Connect opens a connection to the NATS cluster.
func Connect(url string, opts ...nats.Option) (*Bus, error) {
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("natsbus: connect: %w", err)
	}
	js, _ := jetstream.New(nc) // fails only for a nil connection
	return &Bus{nc: nc, js: js}, nil
}

// Close drains and closes the connection.
func (b *Bus) Close() { b.nc.Close() }

// StreamName is the stream holding one context's events in a cell.
func StreamName(cell, context string) string {
	return strings.ToUpper("ERP_" + cell + "_" + context)
}

// EnsureStreams creates or updates the stream of every context.
func (b *Bus) EnsureStreams(ctx context.Context, cell string) error {
	for _, schema := range outbox.Contexts {
		c := outbox.PublicContext(schema)
		_, err := b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
			Name:       StreamName(cell, c),
			Subjects:   []string{"erp." + cell + "." + c + ".>"},
			Storage:    jetstream.FileStorage,
			Duplicates: DuplicateWindow,
		})
		if err != nil {
			return fmt.Errorf("natsbus: stream %s: %w", StreamName(cell, c), err)
		}
	}
	return nil
}

// Publish sends one event and waits for the stream's acknowledgement.
func (b *Bus) Publish(ctx context.Context, m outbox.Message) error {
	msg := nats.NewMsg(m.Subject)
	msg.Data = m.Data
	for k, v := range m.Header {
		msg.Header.Set(k, v)
	}
	if _, err := b.js.PublishMsg(ctx, msg, jetstream.WithMsgID(m.ID.String())); err != nil {
		return fmt.Errorf("natsbus: publish %s: %w", m.Subject, err)
	}
	return nil
}
