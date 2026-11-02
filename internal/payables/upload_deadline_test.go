package payables_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// listenerTimeout stands in for the listener's 30-second read timeout:
// the API behind a real listener, the durations shrunk (PO 2026-11-02).
const listenerTimeout = time.Second

// listen serves h on a listener whose read timeout is listenerTimeout,
// as erp-server's public listener serves the API.
func listen(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ReadHeaderTimeout, srv.Config.ReadTimeout, srv.Config.WriteTimeout = listenerTimeout, listenerTimeout, 2*listenerTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// paused sends body to the URL with the method and headers, in the
// background; the body is resumed when the test ends, whatever happened.
func paused(t *testing.T, srv *httptest.Server, method, url string, body *pgtest.PausedBody, header http.Header) <-chan pgtest.Sent {
	t.Helper()
	t.Cleanup(body.Resume)
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	return pgtest.Send(srv.Client(), req)
}

// TestUploadOutlastsTheListener: a document sent on the API's signed path
// more slowly than the listener's read timeout allows any request is
// received, since the path gives its body api.UploadTimeout; the same
// slow body on another route is cut at the listener's timeout; and the
// path's own deadline still cuts a body that outlasts it (slow-loris).
func TestUploadOutlastsTheListener(t *testing.T) {
	r := newRelay(t)
	srv := listen(t, r.e.API.Handler)
	pdf := map[string][]string{"Content-Type": {"application/pdf"}}
	head, tail := r.pdf[:10], r.pdf[10:]
	up := r.create(t)
	start := time.Now()
	upload := pgtest.NewPausedBody([]byte(head), []byte(tail))
	received := paused(t, srv, http.MethodPut, up["url"].(string), upload, pdf)
	<-upload.Paused()
	// Another route, its body paused alike and sent after the upload's
	// began, so the listener's deadline it is cut at is past the upload's
	// own listener deadline too.
	capture := pgtest.NewPausedBody([]byte("--b\r\nContent-Disposition: form-data; name=\"legal_entity_id\"\r\n\r\n"), []byte(r.e.DE.String()+"\r\n--b--\r\n"))
	cut := paused(t, srv, http.MethodPost, "/v1/supplier-invoices:capture", capture, http.Header{
		"Content-Type":    {"multipart/form-data; boundary=b"},
		"Authorization":   {"Bearer " + r.e.API.Issuer.Token(t, r.e.Tenant.ID, r.e.Tenant.Human.Subject, nil)},
		"Idempotency-Key": {apitest.NewKey()},
	})
	if got := <-cut; got.Err == nil && got.Status < http.StatusBadRequest {
		t.Errorf("another route's slow body: got %d %s, want it cut at the listener's timeout", got.Status, got.Body)
	}
	if elapsed := time.Since(start); elapsed < listenerTimeout {
		t.Fatalf("another route's slow body was answered after %v, before the listener's %v: refused, not cut", elapsed, listenerTimeout)
	}
	upload.Resume()
	if got := <-received; got.Err != nil || got.Status != http.StatusNoContent {
		t.Fatalf("the upload, slower than the listener allows: got %d %v %s, want 204", got.Status, got.Err, got.Body)
	}
	if obj, ok := r.s3.Object(r.key(up)); !ok || string(obj.Content) != r.pdf {
		t.Errorf("the bucket's upload: got %q (%v), want the document", obj.Content, ok)
	}

	// The path's own deadline, brought to 2 s from now by the cell's clock.
	window := 2 * listenerTimeout
	late := apitest.NewWith(t, func(cfg *app.Config) {
		cfg.Objects = r.s3.Store()
		cfg.Now = func() time.Time { return time.Now().Add(window - api.UploadTimeout) }
	})
	slow := listen(t, late.Handler)
	up = r.create(t)
	start = time.Now()
	never := pgtest.NewPausedBody([]byte(head), []byte(tail))
	got := <-paused(t, slow, http.MethodPut, up["url"].(string), never, pdf)
	if got.Status != http.StatusServiceUnavailable || !strings.Contains(got.Body, "did not arrive whole") {
		t.Errorf("a body past the path's deadline: got %d %v %s, want 503, not whole", got.Status, got.Err, got.Body)
	}
	if elapsed := time.Since(start); elapsed < window {
		t.Errorf("a body past the path's deadline was cut after %v, want the path's %v, not the listener's %v", elapsed, window, listenerTimeout)
	}
	if _, ok := r.s3.Object(r.key(up)); ok {
		t.Error("a document cut short is in the bucket")
	}
}
