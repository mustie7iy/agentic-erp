package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/router"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// listenerTimeout stands in for the 30-second read timeout of erp-web's
// and the API's listeners, shrunk (PO 2026-11-02).
const listenerTimeout = 2 * time.Second

// TestCaptureDeadlines: each hop of the upload path allows the hop behind
// it a margin more, so the cell is the one to cut an upload that overruns
// (PO 2026-11-02): erp-web's read past the router's and the cell's, its
// relay's call past the router's answer, its answer past its relay.
func TestCaptureDeadlines(t *testing.T) {
	for name, c := range map[string]struct{ got, want time.Duration }{
		"erp-web's read":   {captureReadTimeout, router.UploadReadTimeout + api.UploadMargin},
		"the relay's call": {relayTimeout, router.UploadWriteTimeout + api.UploadMargin},
		"erp-web's answer": {captureWriteTimeout, api.UploadMargin + relayTimeout + api.UploadMargin},
	} {
		t.Run(name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %v, want %v", c.got, c.want)
			}
		})
	}
	if got := newRelay(&url.URL{}, http.DefaultClient).hc.Timeout; got != relayTimeout {
		t.Errorf("the relay's client timeout: got %v, want %v", got, relayTimeout)
	}
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	allowTime(w, now)
	if !w.read.Equal(now.Add(captureReadTimeout)) || !w.write.Equal(now.Add(captureWriteTimeout)) {
		t.Errorf("deadlines: got read %v, write %v, want %v and %v", w.read, w.write, now.Add(captureReadTimeout), now.Add(captureWriteTimeout))
	}
}

// deadlineWriter records the deadlines asked of it.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	read, write time.Time
}

func (d *deadlineWriter) SetReadDeadline(t time.Time) error  { d.read = t; return nil }
func (d *deadlineWriter) SetWriteDeadline(t time.Time) error { d.write = t; return nil }

// TestCaptureOutlastsTheListener: a document sent to the capture more
// slowly than erp-web's and the API's listeners allow any request, pausing
// while erp-web relays it to the API's signed path, is captured; the same
// slow body on another form is cut at the listener's timeout.
func TestCaptureOutlastsTheListener(t *testing.T) {
	put, once := make(chan struct{}), sync.Once{}
	s, s3, _ := captureStack(t, paperInvoice("PO-1"), func(o *stackOptions) {
		o.listener = listenerTimeout
		o.seen = func(r *http.Request) {
			if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/uploads/") {
				once.Do(func() { close(put) })
			}
		}
	})
	b := s.browser()
	b.signIn()
	form := b.get(ui.CapturePath).page(t).form(ui.CapturePath)
	doc := scan + strings.Repeat("% padding\n", 200)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range []string{"csrf_token", "idempotency_key"} {
		_ = mw.WriteField(f, field(form, f)) // into memory
	}
	_ = mw.WriteField("legal_entity_id", s.f.DE.String()) // into memory
	part, _ := mw.CreateFormFile("file", "scan.pdf")      // into memory
	_, _ = part.Write([]byte(doc))
	_ = mw.Close()
	// The pause comes after the document's first kilobyte: past the 512
	// bytes its type is read from, so erp-web has started the upload and
	// is relaying it to the API's path when the document stops coming.
	cut := bytes.Index(body.Bytes(), []byte(doc)) + 1024
	capture := pgtest.NewPausedBody(body.Bytes()[:cut], body.Bytes()[cut:])
	sent := sendPaused(t, b, ui.CapturePath, capture, mw.FormDataContentType())
	<-capture.Paused()
	<-put
	// Another form, its body paused alike and sent once the API has the
	// relayed upload, so the listener's deadline it is cut at is past
	// erp-web's and the API's own listener deadlines for the capture.
	start := time.Now()
	statement := pgtest.NewPausedBody([]byte("--b\r\nContent-Disposition: form-data; name=\"csrf_token\"\r\n\r\n"), []byte(b.csrf()+"\r\n--b--\r\n"))
	if got := <-sendPaused(t, b, uploadPathOf, statement, "multipart/form-data; boundary=b"); got.Err == nil && got.Status < http.StatusBadRequest {
		t.Errorf("another form's slow body: got %d %s, want it cut at the listener's timeout", got.Status, got.Body)
	}
	if elapsed := time.Since(start); elapsed < listenerTimeout {
		t.Fatalf("another form's slow body was answered after %v, before the listener's %v: refused, not cut", elapsed, listenerTimeout)
	}
	capture.Resume()
	got := <-sent
	if got.Err != nil || got.Status != http.StatusSeeOther {
		t.Fatalf("the capture, slower than the listeners allow: got %d %v %s, want 303 to the invoice", got.Status, got.Err, got.Body)
	}
	// A form refused once its document has begun (no company): answered
	// at once, the rest of the document never waited for.
	refused := make([]byte, 0, len(body.Bytes()))
	refused = append(refused, body.Bytes()[:cut]...)
	refused = bytes.Replace(refused, []byte(s.f.DE.String()), []byte("not-a-company"), 1)
	cut = bytes.Index(refused, []byte(doc[:16])) + 1024
	stopped := pgtest.NewPausedBody(refused[:cut], body.Bytes()[len(body.Bytes())-16:])
	start = time.Now()
	select {
	case got := <-sendPaused(t, b, ui.CapturePath, stopped, mw.FormDataContentType()):
		if got.Status != http.StatusUnprocessableEntity {
			t.Errorf("a form refused while its document is paused: got %d %v, want 422", got.Status, got.Err)
		}
		if elapsed := time.Since(start); elapsed >= listenerTimeout {
			t.Errorf("a form refused while its document is paused was answered after %v, want at once", elapsed)
		}
	case <-time.After(10 * listenerTimeout):
		t.Fatal("a form refused while its document is paused: no answer; the lifted deadline outlived the refusal")
	}
	var upload string
	pgtest.Scan(t, `SELECT id::text FROM payables.upload WHERE tenant_id = $1 AND status = 'used'`, []any{s.ten.ID}, &upload)
	if obj, ok := s3.Object(blobstore.Key(s.ten.ID, blobstore.KindUploads, upload)); !ok || string(obj.Content) != doc {
		t.Errorf("the bucket's upload: got %d bytes (%v), want the document's %d", len(obj.Content), ok, len(doc))
	}
}

// sendPaused sends body to erp-web's path as the browser sends a form, in
// the background; the body is resumed when the test ends.
func sendPaused(t *testing.T, b *browser, path string, body *pgtest.PausedBody, ctype string) <-chan pgtest.Sent {
	t.Helper()
	t.Cleanup(body.Resume)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, b.s.public+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Origin", b.s.public)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return pgtest.Send(b.hc, req)
}
