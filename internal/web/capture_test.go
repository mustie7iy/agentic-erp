package web

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// Capture by upload (PO 2026-11-01): erp-web streams the document to the
// API's own signed path, which the API streams into the bucket (here the
// S3 double), and captures the invoice from the upload.

// reads is an extractor that answers one extraction.
type reads struct{ e *capture.Extraction }

func (x reads) Extract(context.Context, []byte) (*capture.Extraction, error) { return x.e, nil }

// paperInvoice is what the document of PO-1's paper prints.
func paperInvoice(order string) *capture.Extraction {
	return &capture.Extraction{DocumentKind: "invoice", VendorName: "Supplier", InvoiceNumber: "SCAN-1", InvoiceDate: "2026-03-10", PurchaseOrder: order,
		Currency: "EUR", Lines: []capture.Line{{Description: "Paper", Quantity: "10", Amount: "120.00", POLine: 1}}, TaxAmount: "22.80",
		GrossAmount: "142.80", Confidence: "high"}
}

// captureStack is the stack on an S3 double with an extractor of e, PO-1
// ordered; it returns the double too, and a person who may read invoices
// but not capture them.
func captureStack(t *testing.T, e *capture.Extraction) (*stack, *blobstoretest.Server, pgtest.Actor) {
	t.Helper()
	s3 := blobstoretest.New(t)
	var viewer pgtest.Actor
	s := newStack(t, stackOptions{permissions: fixture.InvoicePermissions, store: s3.Store(), extractor: reads{e: e}, seed: func(f fixture.Env) {
		f.Order(t, f.Supplier, "PO-1")
		viewer = roleHolder(t, f, "viewer", "payables.invoice.read", "tenancy.org.read")
	}})
	return s, s3, viewer
}

// captureForm posts the capture form as the page offers it: its fields,
// those given replacing them ("" leaves one out), then the file when name
// is not "".
func captureForm(b *browser, fields map[string]string, name string, file []byte, header ...string) response {
	b.s.t.Helper()
	form := b.get(ui.CapturePath).page(b.s.t).form(ui.CapturePath)
	values := []string{"csrf_token", field(form, "csrf_token"), "idempotency_key", field(form, "idempotency_key"), "legal_entity_id", b.s.f.DE.String()}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for i := 0; i < len(values); i += 2 {
		v, changed := fields[values[i]]
		if !changed {
			v = values[i+1]
		}
		if v != "" {
			_ = w.WriteField(values[i], v) // into memory
		}
	}
	if name != "" {
		part, _ := w.CreateFormFile("file", name) // into memory
		_, _ = part.Write(file)
	}
	_ = w.Close()
	return b.raw(http.MethodPost, ui.CapturePath, &body, w.FormDataContentType(), header...)
}

const scan = "%PDF-1.7\n% the supplier's scanned invoice\n"

// TestCaptureByUpload: the queue offers the capture; its form names the
// company and takes the document, its token first; sent, the document is
// streamed to the API's signed path into the bucket, the invoice captured
// from it, and the person lands on it; the same form sent again, with the
// same document or another, is not taken for its first document: shown
// again, fresh, with a link to the invoice captured then.
func TestCaptureByUpload(t *testing.T) {
	s, s3, _ := captureStack(t, paperInvoice("PO-1"))
	b := s.browser()
	b.signIn()
	queue := b.get(ui.InvoicesPath).page(t)
	if len(queue.all("a", "")) == 0 || !strings.Contains(queue.raw, `href="/invoices/capture"`) {
		t.Errorf("the queue does not offer the capture")
	}
	r := b.get(ui.CapturePath)
	status(t, "the form", r, http.StatusOK)
	p := r.page(t)
	holds(t, "the form", p.mainText(), "Capture an invoice from its document", "Company the invoice is addressed to", "Acme GmbH",
		"A PDF, PNG, JPEG or e-invoice XML file of at most 50 MB.")
	var order []string
	walk(p.form(ui.CapturePath), func(n *html.Node) {
		if name := attrOf(n, "name"); name != "" && (n.Data == "input" || n.Data == "select") {
			order = append(order, name)
		}
	})
	if got, want := strings.Join(order, " "), "csrf_token idempotency_key legal_entity_id file"; got != want {
		t.Errorf("the form's fields: got %s, want %s (the token first, the document last)", got, want)
	}
	page := b.get(ui.CapturePath).page(t).form(ui.CapturePath)
	sent := captureWith(b, page, []byte(scan))
	if sent.status != http.StatusSeeOther || !strings.HasPrefix(sent.location(), ui.InvoicesPath+"/") {
		t.Fatalf("sent: got %d to %q %s, want the new invoice", sent.status, sent.location(), sent.body)
	}
	invoice := uuid.MustParse(strings.TrimPrefix(sent.location(), ui.InvoicesPath+"/"))
	holds(t, "the invoice", b.get(sent.location()).page(t).mainText(), "Supplier's reference SCAN-1", "Status Parked")
	var upload uuid.UUID
	var via, upStatus string
	pgtest.Scan(t, `SELECT id, via, status FROM payables.upload WHERE used_by = $1`, []any{invoice}, &upload, &via, &upStatus)
	if via != "api" || upStatus != "used" {
		t.Errorf("the upload: got via %s, %s, want api, used", via, upStatus)
	}
	obj, ok := s3.Object(blobstore.Key(s.ten.ID, blobstore.KindUploads, upload.String()))
	if !ok || string(obj.Content) != scan || obj.ContentType != "application/pdf" {
		t.Errorf("the bucket's upload: got %q of %q (%v), want the scan as application/pdf", obj.Content, obj.ContentType, ok)
	}
	for name, doc := range map[string]string{"the same document": scan, "another document": scan + "% another invoice\n"} {
		again := captureWith(b, page, []byte(doc))
		status(t, "sent again with "+name, again, http.StatusConflict)
		holds(t, "sent again with "+name, again.page(t).mainText(), "This form was sent before")
		if !strings.Contains(again.body, `href="`+sent.location()+`"`) {
			t.Errorf("sent again with %s: no link to the invoice captured before, %s", name, sent.location())
		}
		if key := field(again.page(t).form(ui.CapturePath), "idempotency_key"); key == field(page, "idempotency_key") {
			t.Errorf("sent again with %s: got the form's key %s again, want a fresh one", name, key)
		}
	}
	var uploads int
	pgtest.Scan(t, `SELECT count(*) FROM payables.upload WHERE tenant_id = $1`, []any{s.ten.ID}, &uploads)
	if uploads != 1 {
		t.Errorf("uploads after sending twice: got %d, want 1", uploads)
	}
}

// captureWith sends the form's own fields with the document.
func captureWith(b *browser, form *html.Node, doc []byte) response {
	b.s.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, name := range []string{"csrf_token", "idempotency_key"} {
		_ = w.WriteField(name, field(form, name)) // into memory
	}
	_ = w.WriteField("legal_entity_id", b.s.f.DE.String()) // into memory
	part, _ := w.CreateFormFile("file", "scan.pdf")        // into memory
	_, _ = part.Write(doc)
	_ = w.Close()
	return b.raw(http.MethodPost, ui.CapturePath, &body, w.FormDataContentType())
}

// TestCaptureRefusals: what the form refuses before any call (no
// document, one that is no document, no company, no key), what erp-web
// refuses as a possible forgery (no token, the token after the document,
// no multipart form), and what the API refuses (a document naming no
// order; a person who may not capture), each said on the form in the
// person's language.
func TestCaptureRefusals(t *testing.T) {
	s, _, viewer := captureStack(t, paperInvoice(""))
	b := s.browser()
	b.signIn()
	for _, c := range []struct {
		name   string
		fields map[string]string
		file   string
		doc    []byte
		status int
		want   string
	}{
		{"no document", nil, "", nil, http.StatusUnprocessableEntity, "Choose the invoice's document."},
		{"an empty document", nil, "scan.pdf", nil, http.StatusUnprocessableEntity, "Choose the invoice's document."},
		{"no document of a kind taken", nil, "notes.txt", []byte("plain notes"), http.StatusUnprocessableEntity, "This file is not a PDF, PNG, JPEG or XML document."},
		{"no company", map[string]string{"legal_entity_id": "x"}, "scan.pdf", []byte(scan), http.StatusUnprocessableEntity, "Choose the company the invoice is addressed to."},
		{"no key", map[string]string{"idempotency_key": "x"}, "scan.pdf", []byte(scan), http.StatusBadRequest, ""},
		{"no token", map[string]string{"csrf_token": ""}, "scan.pdf", []byte(scan), http.StatusForbidden, ""},
		{"no order on the document", nil, "scan.pdf", []byte(scan), http.StatusUnprocessableEntity, "The document names no purchase order."},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := captureForm(b, c.fields, c.file, c.doc)
			status(t, c.name, r, c.status)
			if c.want != "" {
				holds(t, c.name, r.page(t).mainText(), c.want)
			}
		})
	}
	t.Run("in French", func(t *testing.T) {
		r := captureForm(b, nil, "", nil, "Accept-Language", "fr-CA")
		holds(t, "in French", r.page(t).mainText(), "Choisissez le document de la facture.")
	})
	t.Run("the token after the document", func(t *testing.T) {
		form := b.get(ui.CapturePath).page(t).form(ui.CapturePath)
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		part, _ := w.CreateFormFile("file", "scan.pdf") // into memory
		_, _ = part.Write([]byte(scan))
		_ = w.WriteField("csrf_token", field(form, "csrf_token")) // into memory
		_ = w.Close()
		status(t, "the token after the document", b.raw(http.MethodPost, ui.CapturePath, &body, w.FormDataContentType()), http.StatusForbidden)
	})
	t.Run("no multipart form", func(t *testing.T) {
		status(t, "no multipart form", b.raw(http.MethodPost, ui.CapturePath, strings.NewReader("x"), "multipart/form-data; boundary=b"), http.StatusForbidden)
		status(t, "a urlencoded form", b.do(http.MethodPost, ui.CapturePath, map[string][]string{"csrf_token": {b.csrf()}}), http.StatusForbidden)
	})
	t.Run("too many fields", func(t *testing.T) {
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		for range maxStreamedFields + 1 {
			_ = w.WriteField("csrf_token", b.csrf()) // into memory
		}
		_ = w.Close()
		status(t, "too many fields", b.raw(http.MethodPost, ui.CapturePath, &body, w.FormDataContentType()), http.StatusForbidden)
	})
	t.Run("a field too long", func(t *testing.T) {
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		_ = w.WriteField("csrf_token", strings.Repeat("x", maxForm+1)) // into memory
		_ = w.Close()
		status(t, "a field too long", b.raw(http.MethodPost, ui.CapturePath, &body, w.FormDataContentType()), http.StatusForbidden)
	})
	t.Run("a person who may not capture", func(t *testing.T) {
		v := s.as(viewer)
		if strings.Contains(v.get(ui.InvoicesPath).body, `href="/invoices/capture"`) {
			t.Error("the queue offers the capture to a person who may not")
		}
		r := captureForm(v, nil, "scan.pdf", []byte(scan))
		status(t, "a person who may not capture", r, http.StatusForbidden)
		holds(t, "a person who may not capture", r.page(t).mainText(), "The invoice was not captured")
	})
}

// TestCaptureTooLarge: a document above the API's 50 MB is cut off while
// it streams and said too large on the form; nothing is stored.
func TestCaptureTooLarge(t *testing.T) {
	s, s3, _ := captureStack(t, paperInvoice("PO-1"))
	b := s.browser()
	b.signIn()
	form := b.get(ui.CapturePath).page(t).form(ui.CapturePath)
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)
	go func() {
		for _, name := range []string{"csrf_token", "idempotency_key"} {
			_ = w.WriteField(name, field(form, name)) // a pipe the request reads
		}
		_ = w.WriteField("legal_entity_id", s.f.DE.String()) // a pipe the request reads
		part, _ := w.CreateFormFile("file", "scan.pdf")      // a pipe the request reads
		_, _ = part.Write([]byte(scan))
		_, _ = io.Copy(part, io.LimitReader(zeros{}, maxCaptureDocument))
		_ = w.Close()
		_ = pw.Close()
	}()
	r := b.raw(http.MethodPost, ui.CapturePath, pr, w.FormDataContentType())
	status(t, "too large", r, http.StatusRequestEntityTooLarge)
	holds(t, "too large", r.page(t).mainText(), "The document is larger than 50 MB.")
	for _, k := range s3.Keys() {
		if strings.Contains(k, "/uploads/") {
			t.Errorf("a document over the limit was stored at %s", k)
		}
	}
}

// zeros is an endless stream of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
