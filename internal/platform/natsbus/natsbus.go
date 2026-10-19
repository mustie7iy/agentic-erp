// Package natsbus publishes domain events to NATS JetStream (ADR-0007): one
// stream per context and cell, binary-mode CloudEvents, and de-duplication on
// the event id so a relay retry never delivers an event twice. Durable
// consumers read them back at least once (the webhook fan-out, ADR-0041). TLS
// connects with the process's certificate over TLS 1.3 (ADR-0033).
package natsbus

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// DuplicateWindow is how long JetStream remembers event ids for de-duplication.
const DuplicateWindow = 10 * time.Minute

// Bus is a JetStream connection.
type Bus struct {
	nc *nats.Conn
	js jetstream.JetStream
}

// TLS is the options that connect over TLS 1.3 with the store's material
// (ADR-0033): the server is verified against the store's roots, and the
// store's certificate, when it has one, is the cell's identity. Both are
// taken from the store on every connection, so a renewed certificate is
// used on the next reconnect.
func TLS(s *tlsconf.Store) []nats.Option {
	var cert nats.TLSCertHandler
	if s.HasCertificate() {
		cert = s.Certificate
	}
	return []nats.Option{nats.Secure(&tls.Config{MinVersion: tlsconf.Internal}), nats.ClientTLSConfig(cert, s.RootCAs)}
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

// Handler handles one event read from the bus. nil acknowledges it; an
// error leaves it to be read again.
type Handler func(ctx context.Context, m outbox.Message) error

// RedeliverAfter is how long an event whose handler failed waits before it
// is read again, unless the consumer says otherwise.
const RedeliverAfter = 5 * time.Second

// Consumer reads a durable consumer on some contexts' streams of a cell
// (ADR-0007). Replicas sharing the durable name share the work: each event
// reaches one of them, at least once.
type Consumer struct {
	consumers []jetstream.Consumer
	handle    Handler
	Batch     int           // events per stream per round; default 100
	Interval  time.Duration // idle wait between rounds; default 1s
	Redeliver time.Duration // wait before a failed event is read again; default RedeliverAfter
	Log       *slog.Logger
}

// Consume creates or keeps the durable consumer named durable on the
// streams of contexts (public names) in cell. A new consumer starts at the
// events published after it: the history before it was never promised to
// it.
func (b *Bus) Consume(ctx context.Context, cell, durable string, contexts []string, h Handler) (*Consumer, error) {
	c := &Consumer{handle: h}
	for _, name := range contexts {
		cons, err := b.js.CreateOrUpdateConsumer(ctx, StreamName(cell, name), jetstream.ConsumerConfig{
			Durable:       durable,
			AckPolicy:     jetstream.AckExplicitPolicy,
			DeliverPolicy: jetstream.DeliverNewPolicy,
			AckWait:       30 * time.Second,
		})
		if err != nil {
			return nil, fmt.Errorf("natsbus: consumer %s on %s: %w", durable, StreamName(cell, name), err)
		}
		c.consumers = append(c.consumers, cons)
	}
	return c, nil
}

func (c *Consumer) batch() int {
	if c.Batch <= 0 {
		return 100
	}
	return c.Batch
}

func (c *Consumer) redeliver() time.Duration {
	if c.Redeliver <= 0 {
		return RedeliverAfter
	}
	return c.Redeliver
}

// RunOnce reads what is waiting on every stream, up to a batch each, and
// returns how many events were handled and acknowledged. A failed event is
// handed back for redelivery and the rest of the batch still handled.
func (c *Consumer) RunOnce(ctx context.Context) (int, error) {
	handled := 0
	var errs []error
	for _, cons := range c.consumers {
		batch, err := cons.FetchNoWait(c.batch())
		if err != nil { // the other streams are still read this round
			errs = append(errs, fmt.Errorf("natsbus: fetch from %s: %w", cons.CachedInfo().Stream, err))
			continue
		}
		for msg := range batch.Messages() {
			if err := c.handle(ctx, message(msg)); err != nil {
				// A lost negative acknowledgement is the ack wait's redelivery instead.
				_ = msg.NakWithDelay(c.redeliver())
				errs = append(errs, fmt.Errorf("natsbus: handle %s: %w", msg.Subject(), err))
				continue
			}
			if err := msg.Ack(); err != nil {
				errs = append(errs, fmt.Errorf("natsbus: acknowledge %s: %w", msg.Subject(), err))
				continue
			}
			handled++
		}
		if err := batch.Error(); err != nil {
			errs = append(errs, fmt.Errorf("natsbus: fetch from %s: %w", cons.CachedInfo().Stream, err))
		}
	}
	return handled, errors.Join(errs...)
}

// message is the bus message as the relay sent it: the subject, the first
// value of each header and the data; ID is the ce-id when it is a UUID.
func message(msg jetstream.Msg) outbox.Message {
	h := make(map[string]string, len(msg.Headers()))
	for k := range msg.Headers() {
		h[k] = msg.Headers().Get(k)
	}
	id, _ := uuid.Parse(h["ce-id"]) // the handler decides what an event without a UUID id is
	return outbox.Message{ID: id, Subject: msg.Subject(), Header: h, Data: msg.Data()}
}

// Run reads until ctx ends. A round that filled a batch is followed at once
// by the next; errors are logged and the round retried after the interval.
func (c *Consumer) Run(ctx context.Context) {
	worker.Loop(ctx, c.Interval, time.Second, func(ctx context.Context) (bool, error) {
		n, err := c.RunOnce(ctx)
		return n >= c.batch(), err
	}, c.Log, "bus consumer failed")
}
