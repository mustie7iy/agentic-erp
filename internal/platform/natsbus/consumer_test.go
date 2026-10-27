package natsbus_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus/natstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// bus connects to a fresh JetStream with the cell's streams.
func bus(t *testing.T) *natsbus.Bus {
	t.Helper()
	return connect(t, natstest.Server(t))
}

// connect connects to url and makes sure the cell's streams exist.
func connect(t *testing.T, url string) *natsbus.Bus {
	t.Helper()
	b, err := natsbus.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	if err := b.EnsureStreams(t.Context(), "eu1"); err != nil {
		t.Fatal(err)
	}
	return b
}

func event(t *testing.T, b *natsbus.Bus, context string) outbox.Message {
	t.Helper()
	id := uuid.New()
	m := outbox.Message{ID: id, Subject: "erp.eu1." + context + "." + uuid.NewString() + ".x",
		Header: map[string]string{"ce-id": id.String(), "ce-type": "com.example.erp." + context + ".x.v1"}, Data: []byte(`{}`)}
	if err := b.Publish(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

// seen records the events a handler was given.
type seen struct {
	mu   sync.Mutex
	ids  []uuid.UUID
	fail map[uuid.UUID]int // failures left per event
}

func (s *seen) handle(_ context.Context, m outbox.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail[m.ID] > 0 {
		s.fail[m.ID]--
		return errors.New("not now")
	}
	s.ids = append(s.ids, m.ID)
	return nil
}

func (s *seen) got() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ids)
}

// TestConsumerReadsAtLeastOnce: a durable consumer on the named contexts'
// streams hands each event to the handler with its headers; a failed event
// comes back, and an acknowledged one does not.
func TestConsumerReadsAtLeastOnce(t *testing.T) {
	b := bus(t)
	ctx := t.Context()
	s := &seen{fail: map[uuid.UUID]int{}}
	c, err := b.Consume(ctx, "eu1", "test", []string{"ledger", "tenancy"}, s.handle)
	if err != nil {
		t.Fatal(err)
	}
	c.Redeliver = time.Millisecond
	ledger, tenancy, platform := event(t, b, "ledger"), event(t, b, "tenancy"), event(t, b, "platform")
	s.fail[ledger.ID] = 1
	// A round fetches what has reached the consumer by then, so under load
	// the first round may hold one event or both: rounds run until both are
	// handled, and the ledger event must have failed once on the way.
	failed := false
	pgtest.Eventually(t, 10*time.Second, func() bool {
		if _, err := c.RunOnce(ctx); err != nil {
			failed = true
		}
		return len(s.got()) == 2
	}, "the failed event was not read again")
	if !failed {
		t.Error("no round reported the ledger event's failure")
	}
	got := s.got()
	slices.SortFunc(got, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	want := []uuid.UUID{tenancy.ID, ledger.ID}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(got, want) {
		t.Errorf("handled %v, want %v (never %s of an unread stream)", got, want, platform.ID)
	}
	// A second consumer of the same name continues where the first stopped.
	again, err := b.Consume(ctx, "eu1", "test", []string{"ledger", "tenancy"}, s.handle)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := again.RunOnce(ctx); n != 0 || err != nil {
		t.Errorf("acknowledged events read again: %d, %v", n, err)
	}
}

// TestConsumerRun: Run reads until its context ends.
func TestConsumerRun(t *testing.T) {
	b := bus(t)
	s := &seen{}
	c, err := b.Consume(t.Context(), "eu1", "run", []string{"ledger"}, s.handle)
	if err != nil {
		t.Fatal(err)
	}
	c.Batch, c.Interval, c.Log = 1, time.Millisecond, slog.New(slog.DiscardHandler)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	first, second := event(t, b, "ledger"), event(t, b, "ledger")
	pgtest.Eventually(t, 10*time.Second, func() bool { return len(s.got()) == 2 }, "events %s and %s not read", first.ID, second.ID)
	cancel()
	<-done
}

// TestConsumerFailures: a stream that does not exist, a closed connection
// and a lost acknowledgement are errors.
func TestConsumerFailures(t *testing.T) {
	url := natstest.Server(t)
	b := connect(t, url)
	ctx := t.Context()
	if _, err := b.Consume(ctx, "eu1", "test", []string{"nowhere"}, (&seen{}).handle); !errors.Is(err, jetstream.ErrStreamNotFound) {
		t.Errorf("consumer on a missing stream: got %v, want %v", err, jetstream.ErrStreamNotFound)
	}
	// A failed handler hands the event back after the default wait.
	errNotNow := errors.New("not now")
	failing, err := b.Consume(ctx, "eu1", "nak", []string{"ledger"}, func(context.Context, outbox.Message) error { return errNotNow })
	if err != nil {
		t.Fatal(err)
	}
	event(t, b, "ledger")
	// A fetch can end before the event is visible to the consumer (seen on
	// a loaded CI runner: 0 handled, no error), so read until it arrives.
	var n int
	pgtest.Eventually(t, 10*time.Second, func() bool {
		n, err = failing.RunOnce(ctx)
		return err != nil || n != 0
	}, "failed handler: the event never arrived")
	if n != 0 {
		t.Errorf("failed handler: handled %d, want 0", n)
	}
	if !errors.Is(err, errNotNow) {
		t.Errorf("failed handler: got %v, want %v", err, errNotNow)
	}
	// A consumer deleted under the reader answers the fetch with an error.
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.DeleteConsumer(ctx, natsbus.StreamName("eu1", "ledger"), "nak"); err != nil {
		t.Fatal(err)
	}
	// Which error reports the deletion is the client library's choice (it
	// answers "no responders" today), so only the failure is asserted.
	if _, err := failing.RunOnce(ctx); err == nil {
		t.Error("fetch from a deleted consumer: got nil error, want one")
	}
	c, err := b.Consume(ctx, "eu1", "ack", []string{"ledger"}, func(context.Context, outbox.Message) error {
		b.Close() // the acknowledgement then cannot be sent
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	event(t, b, "ledger")
	// As with the failed handler, a fetch can end before the event arrives
	// (seen on CI, 2026-10-26: 0 handled, no error); the handler closes the
	// bus, so the round that holds the event is the last one.
	pgtest.Eventually(t, 10*time.Second, func() bool {
		n, err = c.RunOnce(ctx)
		return err != nil || n != 0
	}, "lost acknowledgement: the event never arrived")
	if n != 0 {
		t.Errorf("lost acknowledgement: handled %d, want 0", n)
	}
	if !errors.Is(err, nats.ErrConnectionClosed) {
		t.Errorf("lost acknowledgement: got %v, want %v", err, nats.ErrConnectionClosed)
	}
	if _, err := c.RunOnce(ctx); !errors.Is(err, nats.ErrConnectionClosed) {
		t.Errorf("fetch on a closed connection: got %v, want %v", err, nats.ErrConnectionClosed)
	}
}
