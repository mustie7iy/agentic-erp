package router_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/router"
)

// listenerTimeout stands in for the router's 30-second read timeout: the
// router behind a real listener, the durations shrunk (PO 2026-11-02).
const listenerTimeout = time.Second

// TestUploadOutlastsTheListener: a document sent to an upload's signed
// path through the router more slowly than its listener's read timeout
// allows any request reaches the cell whole; the same slow body on another
// route is cut at the listener's timeout; and the path's own deadline
// still cuts a body that outlasts it (slow-loris).
func TestUploadOutlastsTheListener(t *testing.T) {
	is := authntest.New(t)
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	// The cell reads every body to its end: 204 is a body that came whole.
	// Its listener reads a request for listenerTimeout, as erp-server's does.
	cell := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("signature") == "bad" { // refused before a byte is read, as the cell refuses a bad signature
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"status":403,"code":"payables.invalid_upload_url"}`)) // to the router, alive
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Minute)) // as the cell lifts its own once it takes the upload
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	cell.Config.ReadTimeout = listenerTimeout
	cell.Start()
	t.Cleanup(cell.Close)
	tenant := uuid.New()
	m := &memory{entries: map[uuid.UUID]directory.Entry{tenant: {TenantID: tenant, Cell: "eu-1", APIURL: cell.URL, Status: directory.Active}}}
	listen := func(now func() time.Time) *httptest.Server {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		rt := &router.Router{Verifier: v, Directory: lookup{m}, Log: log, Now: now}
		srv := httptest.NewUnstartedServer(telemetry.Middleware(log, telemetry.NewMetrics(), tracenoop.NewTracerProvider())(rt))
		srv.Config.ReadHeaderTimeout, srv.Config.ReadTimeout, srv.Config.WriteTimeout = listenerTimeout, listenerTimeout, 2*listenerTimeout
		srv.Start()
		t.Cleanup(srv.Close)
		return srv
	}
	send := func(srv *httptest.Server, method, path string, body *pgtest.PausedBody) <-chan pgtest.Sent {
		t.Cleanup(body.Resume)
		req, _ := http.NewRequestWithContext(context.Background(), method, srv.URL+path, body) // a test server's URL parses
		req.Header.Set("Authorization", "Bearer "+is.Token(t, tenant, "ada", nil))
		return pgtest.Send(srv.Client(), req)
	}
	upload := "/v1/uploads/" + uuid.NewString() + "/content?tenant=t&expires=1&signature=s"
	srv := listen(nil)
	start := time.Now()
	doc := pgtest.NewPausedBody([]byte("%PDF-1.7 "), []byte("the rest"))
	received := send(srv, http.MethodPut, upload, doc)
	<-doc.Paused()
	// Another route, its body paused alike and sent after the upload's
	// began, so the listener's deadline it is cut at is past the upload's
	// own listener deadline too.
	if got := <-send(srv, http.MethodPut, "/v1/balances", pgtest.NewPausedBody([]byte("{"), []byte("}"))); got.Err == nil && got.Status != http.StatusBadGateway {
		t.Errorf("another route's slow body: got %d %s, want it cut at the listener's timeout (502)", got.Status, got.Body)
	}
	if elapsed := time.Since(start); elapsed < listenerTimeout {
		t.Fatalf("another route's slow body was answered after %v, before the listener's %v: refused, not cut", elapsed, listenerTimeout)
	}
	doc.Resume()
	if got := <-received; got.Err != nil || got.Status != http.StatusNoContent {
		t.Fatalf("the upload, slower than the listener allows: got %d %v %s, want 204", got.Status, got.Err, got.Body)
	}

	// The cell's refusal before reading a byte: the router passes it on and
	// closes the connection, never waiting for the rest of a body trickled
	// in under its lifted deadline (the cell gives up on it at its own
	// listener's timeout, as before).
	start = time.Now()
	refused := send(srv, http.MethodPut, strings.Replace(upload, "signature=s", "signature=bad", 1), pgtest.NewPausedBody([]byte("%PDF-1.7 "), []byte("the rest")))
	select {
	case got := <-refused:
		if got.Status != http.StatusForbidden || !strings.Contains(got.Body, "payables.invalid_upload_url") {
			t.Errorf("an upload the cell refuses: got %d %v %s, want the cell's 403 whole", got.Status, got.Err, got.Body)
		}
		if elapsed := time.Since(start); elapsed >= 3*listenerTimeout {
			t.Errorf("an upload the cell refuses was answered after %v, want within the cell's listener timeout %v, not the router's lifted deadline", elapsed, listenerTimeout)
		}
	case <-time.After(10 * listenerTimeout):
		t.Fatal("an upload the cell refuses: no answer while its body is paused; the lifted deadline outlived the refusal")
	}

	// The path's own deadline, brought to 2 s from now by the router's clock.
	window := 2 * listenerTimeout
	slow := listen(func() time.Time { return time.Now().Add(window - router.UploadReadTimeout) })
	start = time.Now()
	got := <-send(slow, http.MethodPut, upload, pgtest.NewPausedBody([]byte("%PDF-1.7 "), []byte("the rest")))
	if got.Status != http.StatusBadGateway {
		t.Errorf("a body past the path's deadline: got %d %v %s, want 502, the cell's request cut", got.Status, got.Err, got.Body)
	}
	if elapsed := time.Since(start); elapsed < window {
		t.Errorf("a body past the path's deadline was cut after %v, want the path's %v, not the listener's %v", elapsed, window, listenerTimeout)
	}
}
