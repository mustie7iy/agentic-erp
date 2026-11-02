package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The capture's rarer answers, from a stub behind the real sign-in: an
// upload whose URL is not the API's own path (never followed), the signed
// path's refusals and failures, a form sent again, a capture still
// running and its progress.

// uploadJSON is an upload whose URL is u, via via, of at most max bytes.
func uploadJSON(id uuid.UUID, via, u string, max int) string {
	return fmt.Sprintf(`{"id":%q,"version":1,"created_at":"2026-11-01T10:00:00Z","created_by":%q,"updated_at":"2026-11-01T10:00:00Z",
		"purpose":"supplier_invoice","content_type":"application/pdf","max_bytes":%d,"status":"open","via":%q,"url":%q,"method":"PUT",
		"headers":{"Content-Type":"application/pdf"},"expires_at":"2026-11-01T10:15:00Z"}`, id, uuid.New(), max, via, u)
}

// signedPath is an upload's API path, signed.
func signedPath(id uuid.UUID) string {
	return "/v1/uploads/" + id.String() + "/content?tenant=t&expires=1&signature=s"
}

// captureStub is a stub stack whose relay goes to the stub too, the
// browser and its form as the stub's capture page offers it.
func captureStub(t *testing.T, st *stubbed) (*stack, *browser) {
	t.Helper()
	s, b, _ := stubStack(t, st)
	srv := httptest.NewServer(st)
	t.Cleanup(srv.Close)
	api, err := NewAPI(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(srv.URL) // the test server's own URL parses
	s.srv.API, s.srv.relay = api, newRelay(base, srv.Client())
	return s, b
}

// stubbedCapture sends a capture form with a fresh key and the token of
// the browser's session.
func stubbedCapture(b *browser, doc string) response {
	b.s.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("csrf_token", b.csrf())              // into memory
	_ = w.WriteField("idempotency_key", uuid.NewString()) // into memory
	_ = w.WriteField("legal_entity_id", uuid.NewString()) // into memory
	part, _ := w.CreateFormFile("file", "scan.pdf")       // into memory
	_, _ = part.Write([]byte(doc))
	_ = w.Close()
	return b.raw(http.MethodPost, ui.CapturePath, &body, w.FormDataContentType())
}

// TestCaptureStubbed: the capture's answers the real API gives only in
// states hard to reach.
func TestCaptureStubbed(t *testing.T) {
	up, invoice, op := uuid.New(), uuid.New(), uuid.New()
	me := answer(200, meJSON(permReadInvoices, permCaptureInvoices, permReadCompanies, permReadOperations))
	companies := answer(200, fmt.Sprintf(`{"items":[{"id":%q,"version":1,"created_at":"2026-10-22T10:00:00Z","created_by":%q,
		"updated_at":"2026-10-22T10:00:00Z","code":"3000","name":"Acme FZE","country_code":"AE","local_currency":"AED","fiscal_calendar_id":%q,
		"status":"active","extensions":{}}]}`, uuid.New(), uuid.New(), uuid.New()))
	created := answer(201, uploadJSON(up, "api", signedPath(up), 1<<20))
	stored := answer(204, "")
	succeeded := answer(202, operationJSON(op, "payables.capture", "succeeded", fmt.Sprintf("%q", "/v1/supplier-invoices/"+invoice.String()), ""))
	var bucket atomic.Int32
	bucketSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { bucket.Add(1) }))
	t.Cleanup(bucketSrv.Close)
	for name, c := range map[string]struct {
		routes   map[string]http.HandlerFunc
		doc      string
		status   int
		location string
		want     string
	}{
		"the store's URL, never followed": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": answer(201, uploadJSON(up, "store", bucketSrv.URL+"/bucket/x?X-Amz-Signature=s", 1<<20))},
			status: http.StatusServiceUnavailable},
		"another upload's path": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": answer(201, uploadJSON(up, "api", signedPath(uuid.New()), 1<<20))},
			status: http.StatusServiceUnavailable},
		"a path without its signature": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": answer(201, uploadJSON(up, "api", "/v1/uploads/"+up.String()+"/content", 1<<20))},
			status: http.StatusServiceUnavailable},
		"no URL": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": answer(201, uploadJSON(up, "api", "%zz", 1<<20))}, status: http.StatusServiceUnavailable},
		"the upload refused": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": answer(422, problemJSON(422, "payables.unsupported_content_type"))},
			status: http.StatusUnprocessableEntity, want: "The invoice was not captured"},
		"the upload unavailable": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": answer(503, problemJSON(503, "platform.unavailable"))},
			status: http.StatusServiceUnavailable},
		"larger than the upload takes": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": answer(201, uploadJSON(up, "api", signedPath(up), 4)), "PUT /v1/uploads/": stored},
			status: http.StatusRequestEntityTooLarge, want: "The document is larger than 50 MB."},
		"the path's refusal": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": created, "PUT /v1/uploads/": answer(403, problemJSON(403, "payables.upload_url_expired"))},
			status: http.StatusForbidden, want: "The upload expired before the document arrived."},
		"the path's 413": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": created, "PUT /v1/uploads/": answer(413, problemJSON(413, "payables.document_too_large"))},
			status: http.StatusRequestEntityTooLarge, want: "The document is larger than 50 MB."},
		"the path unavailable": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": created, "PUT /v1/uploads/": answer(503, problemJSON(503, "platform.unavailable"))},
			status: http.StatusServiceUnavailable},
		"the path's 401": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": created, "PUT /v1/uploads/": answer(401, problemJSON(401, "authn.invalid_token"))},
			status: http.StatusSeeOther, location: "/login"},
		"the path answers no problem": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": created, "PUT /v1/uploads/": answer(502, "bad gateway")},
			status: http.StatusServiceUnavailable},
		"the path redirects": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": created, "PUT /v1/uploads/": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, bucketSrv.URL, http.StatusSeeOther)
		}}, status: http.StatusServiceUnavailable},
		"captured": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": created, "PUT /v1/uploads/": stored, "POST /v1/supplier-invoices:capture": succeeded},
			status: http.StatusSeeOther, location: invoicePath(invoice)},
		"the capture still running": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": created, "PUT /v1/uploads/": stored,
			"POST /v1/supplier-invoices:capture": answer(202, operationJSON(op, "payables.capture", "running", "null", ""))},
			status: http.StatusSeeOther, location: ui.CaptureWaitPath(op.String())},
		"the capture refused": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": created, "PUT /v1/uploads/": stored,
			"POST /v1/supplier-invoices:capture": answer(422, problemJSON(422, "payables.unreadable_document"))},
			status: http.StatusUnprocessableEntity, want: "The document could not be read as an invoice."},
		"sent again, captured": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": created, "PUT /v1/uploads/": answer(409, problemJSON(409, "payables.upload_closed")),
			"GET /v1/uploads/": answer(200, strings.Replace(uploadJSON(up, "api", signedPath(up), 1<<20), `"status":"open"`, fmt.Sprintf(`"status":"used","used_by":%q`, invoice), 1))},
			status: http.StatusSeeOther, location: invoicePath(invoice)},
		"sent again, not captured yet": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": created, "PUT /v1/uploads/": answer(409, problemJSON(409, "payables.upload_closed")),
			"GET /v1/uploads/": answer(200, uploadJSON(up, "api", signedPath(up), 1<<20)), "POST /v1/supplier-invoices:capture": succeeded},
			status: http.StatusSeeOther, location: invoicePath(invoice)},
		"sent again, the upload refused": {routes: map[string]http.HandlerFunc{"POST /v1/uploads": created, "PUT /v1/uploads/": answer(409, problemJSON(409, "payables.upload_closed")),
			"GET /v1/uploads/": answer(404, problemJSON(404, "request.not_found"))}, status: http.StatusNotFound, want: "The invoice was not captured"},
	} {
		t.Run(name, func(t *testing.T) {
			routes := map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/legal-entities": companies}
			for k, v := range c.routes {
				routes[k] = v
			}
			_, b := captureStub(t, &stubbed{routes: routes})
			doc := c.doc
			if doc == "" {
				doc = scan
			}
			r := stubbedCapture(b, doc)
			status(t, name, r, c.status)
			if c.location != "" && !strings.HasPrefix(r.location(), c.location) {
				t.Errorf("%s: got to %q, want %q", name, r.location(), c.location)
			}
			if c.want != "" {
				holds(t, name, r.page(t).mainText(), c.want)
			}
		})
	}
	if n := bucket.Load(); n != 0 {
		t.Errorf("the bucket was reached %d times, want never", n)
	}
}

// TestCaptureWait: a capture's progress, a page and htmx's poll of its
// status: while it runs (a status already shown answered 204), once it has
// stopped (why, on the page), once it names the invoice (the person goes
// there); refused, of another kind, or no operation at all.
func TestCaptureWait(t *testing.T) {
	op, invoice := uuid.New(), uuid.New()
	me := answer(200, meJSON(permReadInvoices, permCaptureInvoices, permReadOperations))
	running := answer(200, operationJSON(op, "payables.capture", "running", "null", ""))
	failed := answer(200, operationJSON(op, "payables.capture", "failed", "null", problemJSON(422, "payables.unreadable_document")))
	done := answer(200, operationJSON(op, "payables.capture", "succeeded", fmt.Sprintf("%q", "/v1/supplier-invoices/"+invoice.String()), ""))
	wait := ui.CaptureWaitPath(op.String())
	htmx := []string{"HX-Request", "true"}
	for name, c := range map[string]struct {
		operation http.HandlerFunc
		path      string
		header    []string
		status    int
		redirect  string // Location or HX-Redirect
		want      string
	}{
		"running":                {running, wait, nil, http.StatusOK, "", "The document was uploaded and is being read."},
		"running, polled":        {running, wait + "&part=status", htmx, http.StatusOK, "", "Running"},
		"stopped":                {failed, wait, nil, http.StatusOK, "", "The document could not be read as an invoice."},
		"stopped, polled":        {failed, wait + "&part=status", htmx, http.StatusOK, wait, ""},
		"done":                   {done, wait, nil, http.StatusSeeOther, invoicePath(invoice), ""},
		"done, polled":           {done, wait + "&part=status", htmx, http.StatusOK, invoicePath(invoice), ""},
		"refused":                {answer(403, problemJSON(403, "identity.permission_missing")), wait, nil, http.StatusForbidden, "", "The capture's progress could not be read"},
		"unavailable":            {answer(503, problemJSON(503, "platform.unavailable")), wait, nil, http.StatusServiceUnavailable, "", ""},
		"another kind":           {answer(200, operationJSON(op, "payments.propose", "running", "null", "")), wait, nil, http.StatusNotFound, "", ""},
		"no operation":           {running, ui.CapturePath + "?operation=x", nil, http.StatusNotFound, "", ""},
		"who am I, unavailable":  {running, wait, nil, http.StatusServiceUnavailable, "", ""},
		"the form, unavailable":  {running, ui.CapturePath, nil, http.StatusServiceUnavailable, "", ""},
		"the form, no companies": {running, ui.CapturePath, nil, http.StatusOK, "", "Your companies could not be read"},
	} {
		t.Run(name, func(t *testing.T) {
			routes := map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/operations/": c.operation,
				"GET /v1/legal-entities": answer(503, problemJSON(503, "platform.unavailable"))}
			if strings.HasSuffix(name, "unavailable") && strings.HasPrefix(name, "who am I") || name == "the form, unavailable" {
				routes["GET /v1/me"] = answer(500, problemJSON(500, "platform.internal"))
			}
			_, b := captureStub(t, &stubbed{routes: routes})
			r := b.get(c.path, c.header...)
			status(t, name, r, c.status)
			if got := r.location() + r.header.Get("HX-Redirect"); c.redirect != "" && got != c.redirect {
				t.Errorf("%s: went to %q, want %q", name, got, c.redirect)
			}
			if c.want != "" {
				holds(t, name, text(r.page(t).doc), c.want)
			}
		})
	}
	t.Run("polled, unchanged", func(t *testing.T) {
		_, b := captureStub(t, &stubbed{routes: map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/operations/": running}})
		page := b.get(wait).page(t)
		var poll string
		walk(page.doc, func(n *html.Node) {
			if v := attrOf(n, "hx-get"); v != "" {
				poll = v
			}
		})
		if poll == "" {
			t.Fatal("the page does not poll")
		}
		status(t, "polled, unchanged", b.get(poll, htmx...), http.StatusNoContent)
	})
	t.Run("the form sent, who am I unavailable", func(t *testing.T) {
		s, b := captureStub(t, &stubbed{routes: map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/legal-entities": answer(200, `{"items":[]}`)}})
		token := b.csrf()
		s.srv.API, _ = NewAPI("http://127.0.0.1:1", http.DefaultClient) // nothing listens there
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		_ = w.WriteField("csrf_token", token)                 // into memory
		_ = w.WriteField("idempotency_key", uuid.NewString()) // into memory
		_ = w.Close()
		status(t, "who am I", b.raw(http.MethodPost, ui.CapturePath, &body, w.FormDataContentType()), http.StatusServiceUnavailable)
	})
	t.Run("a part without its headers' end", func(t *testing.T) {
		_, b := captureStub(t, &stubbed{routes: map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/legal-entities": answer(200, `{"items":[]}`)}})
		body := "--b\r\nContent-Disposition: form-data; name=\"csrf_token\"\r\n\r\n" + b.csrf() + "\r\n--b\r\nContent-Disposition"
		status(t, "no headers' end", b.raw(http.MethodPost, ui.CapturePath, strings.NewReader(body), "multipart/form-data; boundary=b"), http.StatusForbidden)
	})
	t.Run("a form cut short", func(t *testing.T) {
		_, b := captureStub(t, &stubbed{routes: map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/legal-entities": answer(200, `{"items":[]}`)}})
		body := "--b\r\nContent-Disposition: form-data; name=\"csrf_token\"\r\n\r\n" + b.csrf()
		status(t, "cut short", b.raw(http.MethodPost, ui.CapturePath, strings.NewReader(body), "multipart/form-data; boundary=b"), http.StatusForbidden)
	})
}

// TestRelay: the relay's own refusals, before any byte is sent: no
// session's token, an address that is no URL; and the limit it holds a
// document to.
func TestRelay(t *testing.T) {
	up := gen.Upload{ID: uuid.New(), Via: "api", Method: http.MethodPut}
	up.URL = signedPath(up.ID)
	rl := newRelay(&url.URL{Scheme: "http", Host: "127.0.0.1:1", Path: "/api/"}, http.DefaultClient)
	got, err := rl.target(up)
	if err != nil || got != "http://127.0.0.1:1/api"+signedPath(up.ID) {
		t.Errorf("target below a base path: got %q, %v", got, err)
	}
	if _, err := rl.put(context.Background(), up, strings.NewReader(scan)); !errors.Is(err, ErrNoSession) {
		t.Errorf("without a session: got %v, want %v", err, ErrNoSession)
	}
	bad := newRelay(&url.URL{Scheme: "1x", Host: "h"}, http.DefaultClient) // a scheme no URL parses with
	if _, err := bad.put(WithToken(context.Background(), "t"), up, strings.NewReader(scan)); err == nil {
		t.Error("an address that is no URL: got no error")
	}
	c := &capped{r: strings.NewReader("12345"), left: 5}
	if b, err := io.ReadAll(c); err != nil || string(b) != "12345" {
		t.Errorf("exactly the limit: got %q, %v", b, err)
	}
	c = &capped{r: strings.NewReader("123456"), left: 5}
	if _, err := io.ReadAll(c); !errors.Is(err, errTooLarge) {
		t.Errorf("past the limit: got %v, want %v", err, errTooLarge)
	}
	if same := uploadKey(up.ID); same != uploadKey(uuid.MustParse(up.ID.String())) || same == up.ID.String() || same == uploadKey(uuid.New()) {
		t.Error("the upload's key is not derived from the form's alone")
	}
}
