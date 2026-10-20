package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Loopback serves reads of the public API in-process as the principal of
// the request whose context they carry: the request's verified claims and
// client certificate go to the unit of work and the generated server, as a
// call from outside would (permissions, company access, an agent's rules),
// without a second token check or a network hop. A handler that answers
// through the API's own reads (the analytics endpoint's database path,
// ADR-0044) uses it as its HTTP client's transport. New sets its handler.
//
// Each call runs in a transaction of its own, beside the caller's: a
// caller limits how many it makes at once, so they never wait on the pool
// for each other.
type Loopback struct {
	handler http.Handler
}

// ErrLoopback is returned for a call the loopback does not serve: outside
// a request, before the API is built, or not a read.
var ErrLoopback = errors.New("httpapi: loopback")

// RoundTrip implements http.RoundTripper.
func (l *Loopback) RoundTrip(req *http.Request) (*http.Response, error) {
	claims, ok := reqctx.ClaimsFrom(req.Context())
	switch {
	case l.handler == nil:
		return nil, fmt.Errorf("%w: the API is not built", ErrLoopback)
	case !ok:
		return nil, fmt.Errorf("%w: no request's principal", ErrLoopback)
	case req.Method != http.MethodGet:
		return nil, fmt.Errorf("%w: %s is not a read", ErrLoopback, req.Method)
	}
	ctx, cancel := detached(req.Context())
	defer cancel()
	ctx = reqctx.WithClaims(ctx, claims)
	if cert := reqctx.ClientCertFrom(req.Context()); cert != "" {
		ctx = reqctx.WithClientCert(ctx, cert)
	}
	ctx = reqctx.WithTraceID(ctx, reqctx.TraceID(req.Context()))
	ctx, _ = reqctx.WithInfo(ctx)
	w := &recorder{header: http.Header{}, status: http.StatusOK}
	l.handler.ServeHTTP(w, req.Clone(ctx))
	return &http.Response{StatusCode: w.status, Status: http.StatusText(w.status), Header: w.header,
		Body: io.NopCloser(bytes.NewReader(w.body.Bytes())), ContentLength: int64(w.body.Len()), Request: req,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}, nil
}

// detached is a context with parent's deadline and cancellation but none
// of its values: the call must not see the caller's transaction or
// principal, which the unit of work sets afresh.
func detached(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	if d, ok := parent.Deadline(); ok {
		var cancelDeadline context.CancelFunc
		ctx, cancelDeadline = context.WithDeadline(ctx, d)
		prev := cancel
		cancel = func() { cancelDeadline(); prev() }
	}
	stop := context.AfterFunc(parent, cancel)
	return ctx, func() { stop(); cancel() }
}

// recorder holds a response served in-process.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if !r.wrote {
		r.status, r.wrote = status, true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.body.Write(b) //nolint:wrapcheck // a bytes.Buffer never fails a write
}
