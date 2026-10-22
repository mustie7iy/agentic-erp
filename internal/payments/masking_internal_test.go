package payments

import (
	"context"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestStatementsMasked: payments' own rule for bank statements (T49g,
// ADR-0047), not reqctx.Masked's people-only one: agents and a caller the
// request does not name see accounts masked; people and integrations, which
// import and reconcile statements, see them whole.
func TestStatementsMasked(t *testing.T) {
	as := func(kind string) context.Context {
		return reqctx.WithPrincipal(context.Background(), reqctx.Principal{Kind: kind})
	}
	for name, tc := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"no principal":      {context.Background(), true},
		"agent":             {as(reqctx.KindAgent), true},
		"person":            {as(reqctx.KindHuman), false},
		"service principal": {as(reqctx.KindService), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := statementsMasked(tc.ctx); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
