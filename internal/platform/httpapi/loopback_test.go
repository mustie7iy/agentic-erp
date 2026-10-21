package httpapi

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestLoopback: a read is served in-process with the caller's claims and
// certificate and deadline but none of its transaction; anything else is
// refused.
func TestLoopback(t *testing.T) {
	claims := reqctx.Claims{Subject: "s", TenantID: uuid.New()}
	var seen context.Context
	l := &Loopback{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Context()
		w.WriteHeader(http.StatusTeapot)
		w.WriteHeader(http.StatusOK)   // ignored: the status is written once
		_, _ = w.Write([]byte("body")) // a recorder's write cannot fail
	})}
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	ctx = reqctx.WithTx(reqctx.WithClientCert(reqctx.WithClaims(ctx, claims), "thumb"), nil)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://loopback/v1/x", nil)
	resp, err := l.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // an in-memory body
	b, _ := io.ReadAll(resp.Body)            // an in-memory body
	if resp.StatusCode != http.StatusTeapot || string(b) != "body" || resp.Header == nil {
		t.Errorf("response: got %d %q, want 418 body", resp.StatusCode, b)
	}
	if got, _ := reqctx.ClaimsFrom(seen); got.Subject != "s" {
		t.Errorf("claims: got %+v, want the caller's", got)
	}
	if got := reqctx.ClientCertFrom(seen); got != "thumb" {
		t.Errorf("certificate: got %q, want the caller's", got)
	}
	if _, ok := reqctx.TxFrom(seen); ok {
		t.Error("transaction: got the caller's, want none")
	}
	if got, ok := seen.Deadline(); !ok || !got.Equal(deadline) {
		t.Errorf("deadline: got %v, want the caller's %v", got, deadline)
	}
	for name, c := range map[string]struct {
		l    *Loopback
		req  *http.Request
		want string
	}{
		"not built": {&Loopback{}, req, "not built"},
		"no caller": {l, mustRequest(t, context.Background(), http.MethodGet), "no request's principal"},
		"a write":   {l, mustRequest(t, ctx, http.MethodPost), "POST is not a read"},
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := c.l.RoundTrip(c.req)
			if resp != nil {
				_ = resp.Body.Close() // an in-memory body; the error is what is checked
			}
			pgtest.CheckErr(t, err, c.want)
		})
	}
	// Without a deadline, the call ends with its caller.
	parent, stop := context.WithCancel(reqctx.WithClaims(context.Background(), claims))
	l.handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	resp, err = l.RoundTrip(mustRequest(t, parent, http.MethodGet))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close() // an in-memory body
	stop()
}

func mustRequest(t *testing.T, ctx context.Context, method string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, "http://loopback/v1/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
