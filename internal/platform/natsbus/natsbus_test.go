package natsbus_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus/natstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
)

func TestPublishDeduplicatesByEventID(t *testing.T) {
	url := natstest.Server(t)
	bus, err := natsbus.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	ctx := context.Background()
	if err := bus.EnsureStreams(ctx, "eu1"); err != nil {
		t.Fatal(err)
	}
	if err := bus.EnsureStreams(ctx, "eu1"); err != nil { // idempotent
		t.Fatal(err)
	}
	tenant := uuid.New()
	m := outbox.Message{ID: uuid.New(), Subject: "erp.eu1.platform." + tenant.String() + ".operation_completed",
		Header: map[string]string{"ce-type": "com.example.erp.platform.operation_completed.v1"}, Data: []byte(`{"a":1}`)}
	for i := 0; i < 2; i++ {
		if err := bus.Publish(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	nc, _ := nats.Connect(url)
	defer nc.Close()
	js, _ := jetstream.New(nc)
	stream, err := js.Stream(ctx, natsbus.StreamName("eu1", "platform"))
	if err != nil {
		t.Fatal(err)
	}
	info, _ := stream.Info(ctx)
	if info.State.Msgs != 1 {
		t.Fatalf("stored %d messages, want 1 (de-duplicated)", info.State.Msgs)
	}
	got, err := stream.GetLastMsgForSubject(ctx, m.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if ce, want := got.Header.Get("ce-type"), m.Header["ce-type"]; ce != want {
		t.Errorf("ce-type: got %q, want %q", ce, want)
	}
	if string(got.Data) != `{"a":1}` {
		t.Errorf("data: got %s, want %s", got.Data, `{"a":1}`)
	}
	// Subjects outside every stream are refused.
	if err := bus.Publish(ctx, outbox.Message{ID: uuid.New(), Subject: "elsewhere.x"}); err == nil {
		t.Fatal("publish without a stream accepted")
	}
}

func TestConnectAndStreamErrors(t *testing.T) {
	if _, err := natsbus.Connect("nats://127.0.0.1:1", nats.Timeout(200*time.Millisecond)); err == nil {
		t.Fatal("unreachable server accepted")
	}
	url := natstest.Server(t)
	bus, _ := natsbus.Connect(url)
	bus.Close()
	if err := bus.EnsureStreams(context.Background(), "eu1"); err == nil {
		t.Fatal("closed connection accepted")
	}
}
