package reqctx

import (
	"context"
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
	if TraceID(ctx) != "" || InfoFrom(ctx) == nil {
		t.Fatal("defaults")
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
	if TraceID(ctx) != "t" || InfoFrom(ctx).Operation != "getOperation" {
		t.Fatal("trace or info")
	}
}
