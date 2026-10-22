package reqctx

import (
	"context"
	"net/netip"
	"testing"

	"github.com/google/uuid"
)

func TestContextValues(t *testing.T) {
	ctx := context.Background()
	if _, ok := ClaimsFrom(ctx); ok {
		t.Fatal("claims in empty context")
	}
	if _, ok := PrincipalFrom(ctx); ok {
		t.Fatal("principal in empty context")
	}
	if _, ok := TxFrom(ctx); ok {
		t.Fatal("tx in empty context")
	}
	if got := TraceID(ctx); got != "" {
		t.Fatalf("TraceID of an empty context = %q, want empty", got)
	}
	if InfoFrom(ctx) == nil {
		t.Fatal("InfoFrom of an empty context = nil, want a default Info")
	}
	id := uuid.New()
	ctx = WithClaims(ctx, Claims{Subject: "s", TenantID: id})
	ctx = WithPrincipal(ctx, Principal{ID: id, Kind: KindHuman})
	ctx = WithTraceID(ctx, "t")
	ctx = WithTx(ctx, nil)
	ctx, info := WithInfo(ctx)
	info.Operation = "getOperation"
	if c, _ := ClaimsFrom(ctx); c.Subject != "s" {
		t.Fatal(c)
	}
	if p, _ := PrincipalFrom(ctx); p.ID != id {
		t.Fatal(p)
	}
	if got := TraceID(ctx); got != "t" {
		t.Errorf("TraceID = %q, want %q", got, "t")
	}
	if got := InfoFrom(ctx).Operation; got != "getOperation" {
		t.Errorf("InfoFrom(ctx).Operation = %q, want %q", got, "getOperation")
	}
}

// TestClientAddress: an address recorded is read back; none, or an invalid
// one, is no address.
func TestMasked(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"no principal": {context.Background(), true},
		"agent":        {WithPrincipal(context.Background(), Principal{Kind: KindAgent}), true},
		"person":       {WithPrincipal(context.Background(), Principal{Kind: KindHuman}), false},
		"service":      {WithPrincipal(context.Background(), Principal{Kind: KindService}), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Masked(tc.ctx); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClientAddress(t *testing.T) {
	if _, ok := ClientAddressFrom(context.Background()); ok {
		t.Error("empty context: got an address, want none")
	}
	if _, ok := ClientAddressFrom(WithClientAddress(context.Background(), netip.Addr{})); ok {
		t.Error("invalid address: got an address, want none")
	}
	want := netip.MustParseAddr("2001:db8::7")
	if got, ok := ClientAddressFrom(WithClientAddress(context.Background(), want)); !ok || got != want {
		t.Errorf("got %v %v, want %v", got, ok, want)
	}
}

func TestReadOnly(t *testing.T) {
	ctx := context.Background()
	if ReadOnly(ctx) {
		t.Error("ReadOnly outside a request: got true, want false")
	}
	if !ReadOnly(WithReadOnly(ctx)) {
		t.Error("ReadOnly after WithReadOnly: got false, want true")
	}
}

// TestCommit: what is added runs once, when the unit of work runs it, and
// nothing is kept outside one.
func TestCommit(t *testing.T) {
	runs := 0
	OnCommit(context.Background(), func() { runs++ }) // no Commit: dropped
	ctx, c := WithCommit(context.Background())
	OnCommit(ctx, func() { runs++ })
	if runs != 0 {
		t.Fatalf("runs before commit: got %d, want 0", runs)
	}
	c.Run()
	c.Run()
	if runs != 1 {
		t.Errorf("runs: got %d, want 1", runs)
	}
}
