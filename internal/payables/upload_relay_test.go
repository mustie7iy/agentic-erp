package payables_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// The API's own signed path on an S3 store (via api, ADR-0020 As built
// 2026-11-01, PO 2026-11-01): a client that must not reach the bucket
// (erp-web) sends the bytes to the API, which streams them into the bucket.

// relayScenario is an API on the S3 double, with the double to inspect.
type relayScenario struct {
	e   env
	s3  *blobstoretest.Server
	pdf string
}

func newRelay(t *testing.T) *relayScenario {
	t.Helper()
	r := &relayScenario{e: setup(t), s3: blobstoretest.New(t), pdf: "%PDF-1.7 relayed " + strings.Repeat("r", 200)}
	r.e.order(t, r.e.Supplier, "PO-1")
	r.e.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Objects, cfg.Extractor = r.s3.Store(), extractor{e: extraction()} })
	return r
}

// create starts an upload via api and returns it.
func (r *relayScenario) create(t *testing.T) map[string]any {
	t.Helper()
	up, _ := r.e.in(t).Must(201, "POST", "/v1/uploads", `{"purpose":"supplier_invoice","content_type":"application/pdf","via":"api"}`, "")
	return up
}

// send PUTs body to the upload's URL on the API, as a stream of unknown
// length unless size says it (-1: unknown), and returns the answer.
func (r *relayScenario) send(t *testing.T, up map[string]any, body io.Reader, size int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, up["url"].(string), body)
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/pdf")
	rec := httptest.NewRecorder()
	r.e.API.Handler.ServeHTTP(rec, req)
	return rec
}

// key is the object key of an upload.
func (r *relayScenario) key(up map[string]any) string {
	return blobstore.Key(r.e.Tenant.ID, blobstore.KindUploads, up["id"].(string))
}

// TestRelayedUploads: on S3, an upload via api gets the API's signed path,
// whose bytes the API streams into the bucket; the capture consumes them.
func TestRelayedUploads(t *testing.T) {
	r := newRelay(t)
	e := r.e.in(t)
	up := r.create(t)
	if up["via"] != "api" {
		t.Errorf("via: got %v, want api", up["via"])
	}
	if got, want := up["url"].(string), "/v1/uploads/"+up["id"].(string)+"/content?"; !strings.HasPrefix(got, want) {
		t.Fatalf("url: got %s, want prefix %s", got, want)
	}
	if rec := r.send(t, up, strings.NewReader(r.pdf), -1); rec.Code != http.StatusNoContent {
		t.Fatalf("put: got %d, want 204: %s", rec.Code, rec.Body)
	}
	obj, ok := r.s3.Object(r.key(up))
	if !ok {
		t.Fatal("the document is not in the bucket")
	}
	if string(obj.Content) != r.pdf {
		t.Errorf("bucket content: got %q, want %q", obj.Content, r.pdf)
	}
	if obj.ContentType != "application/pdf" {
		t.Errorf("bucket content type: got %q, want application/pdf", obj.ContentType)
	}
	if obj.Checksum == "" {
		t.Error("the streamed PUT carried no checksum, which an Object Lock bucket requires")
	}
	got, _ := e.Must(200, "GET", "/v1/uploads/"+up["id"].(string), "", "")
	if got["status"] != "received" {
		t.Errorf("status: got %v, want received", got["status"])
	}
	if got["via"] != "api" {
		t.Errorf("via when read: got %v, want api", got["via"])
	}
	if got["url"] != up["url"] {
		t.Errorf("url when read: got %v, want the one created %v", got["url"], up["url"])
	}
	if rec := r.send(t, up, strings.NewReader(r.pdf), -1); !strings.Contains(rec.Body.String(), "payables.upload_closed") {
		t.Errorf("second put: got %d %s, want payables.upload_closed", rec.Code, rec.Body)
	}
	u := uploadScenario{e: r.e}
	res, code := u.capture(t, up["id"].(string))
	if code != 202 {
		t.Fatalf("capture: %d %v", code, res)
	}
	inv, _ := e.Must(200, "GET", res["resource"].(string), "", "")
	doc := e.API.Do(apitest.Req{Method: "GET", Path: "/v1/supplier-invoices/" + inv["id"].(string) + "/document", Tenant: e.Tenant.ID, As: e.Tenant.Human})
	if doc.Body.String() != r.pdf {
		t.Errorf("document: got %q, want %q", doc.Body, r.pdf)
	}
}

// TestRelayedUploadVia: the store's URL by default on S3, the API's when
// asked or when the store cannot presign; any other via is refused; the
// API's path refuses an upload whose URL is the store's.
func TestRelayedUploadVia(t *testing.T) {
	r := newRelay(t)
	e := r.e.in(t)
	def, _ := e.Must(201, "POST", "/v1/uploads", `{"purpose":"supplier_invoice","content_type":"application/pdf"}`, "")
	if def["via"] != "store" || !strings.HasPrefix(def["url"].(string), r.s3.URL) {
		t.Errorf("default on S3: got via %v, url %v, want the store's", def["via"], def["url"])
	}
	e.Problem(422, "payables.unknown_upload_via", "POST", "/v1/uploads", `{"purpose":"supplier_invoice","content_type":"application/pdf","via":"bucket"}`, "")
	dir := apitest.NewWith(t, func(cfg *app.Config) { cfg.Objects = blobstore.Dir{Root: t.TempDir()} })
	rec := dir.Do(apitest.Req{Method: "POST", Path: "/v1/uploads", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(),
		Body: `{"purpose":"supplier_invoice","content_type":"application/pdf","via":"store"}`})
	if got := apitest.JSON(t, rec)["via"]; got != "api" {
		t.Errorf("via store on a directory store: got %v, want api", got)
	}
	// An upload whose URL is the store's: the API's path, signed for it, is refused.
	up := r.create(t)
	pgtest.Exec(t, `UPDATE payables.upload SET via = 'store', version = version + 1 WHERE id = $1`, up["id"])
	if rec := r.send(t, up, strings.NewReader(r.pdf), -1); !strings.Contains(rec.Body.String(), "payables.invalid_upload_url") {
		t.Errorf("an upload to the store on the API's path: got %d %s, want payables.invalid_upload_url", rec.Code, rec.Body)
	}
}

// zeros is an endless stream of zero bytes that counts what was read.
type zeros struct{ read int64 }

func (z *zeros) Read(p []byte) (int, error) {
	clear(p)
	z.read += int64(len(p))
	return len(p), nil
}

// TestRelayedUploadCap: the 50 MB cap holds while the body streams in. A
// body that says it is larger is refused before it is read; one of
// unknown length is refused once it passes the cap; neither is written to
// the bucket, and the upload stays open for the right document.
func TestRelayedUploadCap(t *testing.T) {
	r := newRelay(t)
	up := r.create(t)
	declared := &zeros{}
	rec := r.send(t, up, io.LimitReader(declared, payables.MaxUpload+1), payables.MaxUpload+1)
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "payables.document_too_large") {
		t.Fatalf("declared too large: got %d %s, want 413 payables.document_too_large", rec.Code, rec.Body)
	}
	if declared.read != 0 {
		t.Errorf("declared too large: read %d bytes, want none", declared.read)
	}
	streamed := &zeros{}
	rec = r.send(t, up, streamed, -1)
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "payables.document_too_large") {
		t.Fatalf("streamed too large: got %d %s, want 413 payables.document_too_large", rec.Code, rec.Body)
	}
	if streamed.read > payables.MaxUpload+64<<10 {
		t.Errorf("streamed too large: read %d bytes, want the cap and at most one buffer more", streamed.read)
	}
	if _, ok := r.s3.Object(r.key(up)); ok {
		t.Fatal("a body over the cap was written to the bucket")
	}
	if rec := r.send(t, up, io.LimitReader(&zeros{}, payables.MaxUpload), -1); rec.Code != http.StatusNoContent {
		t.Fatalf("exactly the cap: got %d %s, want 204", rec.Code, rec.Body)
	}
	if obj, _ := r.s3.Object(r.key(up)); len(obj.Content) != payables.MaxUpload {
		t.Errorf("exactly the cap: stored %d bytes, want %d", len(obj.Content), payables.MaxUpload)
	}
}

// brokenBody fails after its first bytes, as a client that went away.
type brokenBody struct{ sent bool }

func (b *brokenBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errors.New("connection reset")
	}
	b.sent = true
	return copy(p, "%PDF"), nil
}

// TestRelayedUploadFailures: a store that cannot stream, a spool that
// cannot be made, a body cut short, all receiving slots taken, each
// transaction's statements failing in turn, and the upload received by
// another PUT meanwhile.
func TestRelayedUploadFailures(t *testing.T) {
	r := newRelay(t)
	serve := func(t *testing.T, u *payables.Uploads, up map[string]any, body io.Reader) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, up["url"].(string), body)
		req.Header.Set("Content-Type", "application/pdf")
		rec := httptest.NewRecorder()
		mux := http.NewServeMux()
		mux.Handle("PUT /v1/uploads/{id}/content", u)
		mux.ServeHTTP(rec, req)
		return rec
	}
	uploads := func() *payables.Uploads {
		return &payables.Uploads{Objects: r.s3.Store(), Secret: []byte(strings.Repeat("u", 32)), Pool: pgtest.Pool(t), AppRole: "erp_app"}
	}
	t.Run("a store that cannot stream", func(t *testing.T) {
		u := uploads()
		u.Objects = statOnly{}
		if rec := serve(t, u, r.create(t), strings.NewReader(r.pdf)); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("got %d %s, want 503", rec.Code, rec.Body)
		}
	})
	t.Run("no spool", func(t *testing.T) {
		u := uploads()
		u.SpoolDir = filepath.Join(t.TempDir(), "missing")
		if rec := serve(t, u, r.create(t), strings.NewReader(r.pdf)); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("got %d %s, want 503", rec.Code, rec.Body)
		}
	})
	t.Run("a body cut short", func(t *testing.T) {
		u, up := uploads(), r.create(t)
		u.SpoolDir = t.TempDir()
		if rec := serve(t, u, up, &brokenBody{}); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("got %d %s, want 503", rec.Code, rec.Body)
		}
		if _, ok := r.s3.Object(r.key(up)); ok {
			t.Error("part of a body was stored")
		}
		if left, _ := os.ReadDir(u.SpoolDir); len(left) != 0 {
			t.Errorf("got %d spooled files left, want none", len(left))
		}
	})
	t.Run("every slot taken", func(t *testing.T) { busy(t, serve, uploads(), r) })
	t.Run("one upload sent twice at once", func(t *testing.T) { twice(t, serve, uploads(), r) })
	t.Run("statements failing", func(t *testing.T) { failingStatements(t, serve, uploads, r) })
}

// twice sends one upload's document while it is already arriving, two
// slots free: refused with 503, so one signed URL holds one slot, and
// another upload still takes the other; the first is received.
func twice(t *testing.T, serve func(*testing.T, *payables.Uploads, map[string]any, io.Reader) *httptest.ResponseRecorder, u *payables.Uploads, r *relayScenario) {
	t.Helper()
	u.MaxReceiving = 2
	first, other := r.create(t), r.create(t)
	pr, pw := io.Pipe()
	done := make(chan int)
	go func() {
		defer func() { _ = pr.Close() }() // a pipe's Close never fails; a refusal before reading then fails the write below rather than hanging it
		done <- serve(t, u, first, pr).Code
	}()
	if _, err := pw.Write([]byte(r.pdf)); err != nil { // returns once the first PUT reads it: it holds a slot
		t.Fatal(err)
	}
	rec := serve(t, u, first, strings.NewReader(r.pdf))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "already arriving") {
		t.Errorf("the same upload again meanwhile: got %d %s, want 503, already arriving", rec.Code, rec.Body)
	}
	if rec := serve(t, u, other, strings.NewReader(r.pdf)); rec.Code != http.StatusNoContent {
		t.Errorf("another upload meanwhile: got %d %s, want 204 in the other slot", rec.Code, rec.Body)
	}
	_ = pw.Close() // a pipe's Close never fails
	if code := <-done; code != http.StatusNoContent {
		t.Errorf("the first: got %d, want 204", code)
	}
}

// busy holds the one receiving slot with a body that has started to
// arrive, and sends another document meanwhile: it is refused with 503,
// and the first is received once its body ends.
func busy(t *testing.T, serve func(*testing.T, *payables.Uploads, map[string]any, io.Reader) *httptest.ResponseRecorder, u *payables.Uploads, r *relayScenario) {
	t.Helper()
	u.MaxReceiving = 1
	first, second := r.create(t), r.create(t)
	pr, pw := io.Pipe()
	done := make(chan int)
	go func() { done <- serve(t, u, first, pr).Code }()
	// The write returns once the first PUT reads it: it holds the slot.
	if _, err := pw.Write([]byte(r.pdf)); err != nil {
		t.Fatal(err)
	}
	if rec := serve(t, u, second, strings.NewReader(r.pdf)); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("while the slot is taken: got %d (Retry-After %q), want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	_ = pw.Close() // a pipe's Close never fails
	if code := <-done; code != http.StatusNoContent {
		t.Errorf("the first: got %d, want 204", code)
	}
	if rec := serve(t, u, second, strings.NewReader(r.pdf)); rec.Code != http.StatusNoContent {
		t.Errorf("once the slot is free: got %d %s, want 204", rec.Code, rec.Body)
	}
}

// failingStatements fails each statement of the receiving transaction
// named (1: the check before the body, 2: the one that stores it) in
// turn, then has another PUT receive the upload between the two.
func failingStatements(t *testing.T, serve func(*testing.T, *payables.Uploads, map[string]any, io.Reader) *httptest.ResponseRecorder, uploads func() *payables.Uploads, r *relayScenario) {
	t.Helper()
	for _, call := range []int{1, 2} {
		for n := 1; ; n++ {
			u, up := uploads(), r.create(t)
			calls := 0
			u.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
				calls++
				return pgtest.Pool(t).InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error {
					if calls == call {
						tx = &pgtest.FailingTx{Tx: tx, FailAt: n}
					}
					return fn(ctx, tx)
				})
			}
			rec := serve(t, u, up, strings.NewReader(r.pdf))
			if rec.Code == http.StatusNoContent {
				break
			}
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("transaction %d, statement %d: got %d %s, want 500", call, n, rec.Code, rec.Body)
			}
		}
	}
	u, up := uploads(), r.create(t)
	calls := 0
	u.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
		if calls++; calls == 2 {
			pgtest.Exec(t, `UPDATE payables.upload SET status = 'received', version = version + 1 WHERE id = $1`, up["id"])
		}
		return pgtest.Pool(t).InTx(ctx, scope, fn)
	}
	if rec := serve(t, u, up, strings.NewReader(r.pdf)); !strings.Contains(rec.Body.String(), "payables.upload_closed") {
		t.Errorf("received meanwhile: got %d %s, want payables.upload_closed", rec.Code, rec.Body)
	}
	if _, ok := r.s3.Object(r.key(up)); ok {
		t.Error("an upload received meanwhile was written again")
	}
}
