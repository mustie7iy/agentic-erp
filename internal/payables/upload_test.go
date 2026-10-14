package payables_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// statOnly reports every object but cannot read one.
type statOnly struct{ blobstore.Store }

func (statOnly) Stat(context.Context, string) (int64, string, error) {
	return 10, "application/pdf", nil
}
func (statOnly) Get(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("store down")
}

// uploadScenario is TestDirectUploads' scenario: an API whose uploads go to a
// directory store, on a clock the test moves.
type uploadScenario struct {
	e       env
	store   blobstore.Dir
	clock   time.Time
	x       extractor
	pdf     string
	up, up3 map[string]any
	plain   *apitest.API // the API without object storage
}

// put sends the content to an upload URL of this API; nil means accepted.
func (u *uploadScenario) put(tb testing.TB, url, body, contentType string) *problem.Problem {
	tb.Helper()
	r := u.e.API.Do(apitest.Req{Method: "PUT", Path: url, Body: body, Header: map[string][]string{"Content-Type": {contentType}}})
	if r.Code == 204 {
		return nil
	}
	m := apitest.JSON(tb, r)
	return &problem.Problem{Status: r.Code, Code: m["code"].(string)}
}

// capture captures an invoice from the upload.
func (u *uploadScenario) capture(tb testing.TB, id string) (map[string]any, int) {
	tb.Helper()
	body := "--b\r\nContent-Disposition: form-data; name=\"legal_entity_id\"\r\n\r\n" + u.e.DE.String() + "\r\n--b\r\nContent-Disposition: form-data; name=\"upload_id\"\r\n\r\n" + id + "\r\n--b--\r\n"
	r := u.e.API.Do(apitest.Req{Method: "POST", Path: "/v1/supplier-invoices:capture", Tenant: u.e.Tenant.ID, As: u.e.Tenant.Human, Key: apitest.NewKey(),
		Header: map[string][]string{"Content-Type": {"multipart/form-data; boundary=b"}}, Body: body})
	return apitest.JSON(tb, r), r.Code
}

// TestDirectUploads: a document above the request limit goes through an
// upload URL, here the API's own (directory store), and is captured by id.
func TestDirectUploads(t *testing.T) {
	u := &uploadScenario{e: setup(t), store: blobstore.Dir{Root: t.TempDir()}, clock: time.Now(), x: extractor{e: extraction()}, pdf: "%PDF-1.7 " + strings.Repeat("x", 100)}
	u.e.order(t, u.e.Supplier, "PO-1")
	u.e.API = apitest.NewWith(t, func(cfg *httpapi.Config) {
		cfg.Objects, cfg.Extractor, cfg.Now = u.store, u.x, func() time.Time { return u.clock }
	})
	if !t.Run("create", u.create) {
		t.FailNow()
	}
	if !t.Run("upload and capture", u.uploadAndCapture) {
		t.FailNow()
	}
	if !t.Run("expired", u.expired) {
		t.FailNow()
	}
	if !t.Run("refusals", u.refusals) {
		t.FailNow()
	}
	if !t.Run("without a store", u.withoutStore) {
		t.FailNow()
	}
	if !t.Run("consumed from a failing store", u.consumeFailures) {
		t.FailNow()
	}
	if !t.Run("put failing", u.putFailures) {
		t.FailNow()
	}
	t.Run("object already there", u.objectAlreadyThere)
}

// create: a signed path on this API, PUT with the content type promised;
// nothing uploaded yet, the capture says so.
func (u *uploadScenario) create(t *testing.T) {
	e := u.e.in(t)
	up, tag := e.Must(201, "POST", "/v1/uploads", `{"purpose":"supplier_invoice","content_type":"application/pdf"}`, "")
	u.up = up
	if up["status"] != "open" {
		t.Errorf("upload status: got %v, want open", up["status"])
	}
	if up["method"] != "PUT" {
		t.Errorf("upload method: got %v, want PUT", up["method"])
	}
	if got := up["max_bytes"].(float64); got != payables.MaxUpload {
		t.Errorf("upload max bytes: got %v, want %v", got, payables.MaxUpload)
	}
	if tag != `"1"` {
		t.Errorf("upload etag: got %s, want \"1\"", tag)
	}
	if got, want := up["url"].(string), "/v1/uploads/"+up["id"].(string)+"/content?"; !strings.HasPrefix(got, want) {
		t.Fatalf("upload url: got %s, want prefix %s", got, want)
	}
	if got := up["headers"].(map[string]any)["Content-Type"]; got != "application/pdf" {
		t.Errorf("upload content type header: got %v, want application/pdf", got)
	}
	target := up["url"].(string)
	if p := u.put(t, target, u.pdf, "image/png"); p == nil || p.Code != "payables.upload_content_type" {
		t.Fatalf("other content type: %+v", p)
	}
	if p := u.put(t, strings.Replace(target, "signature=", "signature=00", 1), u.pdf, "application/pdf"); p == nil || p.Status != 403 {
		t.Fatalf("bad signature: %+v", p)
	}
	if p := u.put(t, strings.Replace(target, "/content?", "x/content?", 1), u.pdf, "application/pdf"); p == nil || p.Status != 403 {
		t.Fatalf("bad id: %+v", p)
	}
	if res, code := u.capture(t, up["id"].(string)); code != 422 || res["code"] != "payables.upload_empty" {
		t.Fatalf("empty: %d %v", code, res)
	}
}

// uploadAndCapture: too large for the upload's limit (lowered for the
// test), then the document; captured by id, the invoice has the document,
// the upload is used and cannot be captured again.
func (u *uploadScenario) uploadAndCapture(t *testing.T) {
	e, up, pdf := u.e.in(t), u.up, u.pdf
	target := up["url"].(string)
	pgtest.Exec(t, `UPDATE payables.upload SET max_bytes = 10, version = version + 1 WHERE id = $1`, up["id"])
	if p := u.put(t, target, pdf, "application/pdf"); p == nil || p.Status != 413 {
		t.Fatalf("too large: %+v", p)
	}
	pgtest.Exec(t, `UPDATE payables.upload SET max_bytes = $2, version = version + 1 WHERE id = $1`, up["id"], payables.MaxUpload)
	if p := u.put(t, target, pdf, "application/pdf"); p != nil {
		t.Fatalf("put: %+v", p)
	}
	if p := u.put(t, target, pdf, "application/pdf"); p == nil || p.Code != "payables.upload_closed" {
		t.Fatalf("second put: %+v", p)
	}
	got, _ := e.Must(200, "GET", "/v1/uploads/"+up["id"].(string), "", "")
	if got["status"] != "received" {
		t.Errorf("received upload status: got %v, want received", got["status"])
	}
	if size := got["size"].(float64); size != float64(len(pdf)) {
		t.Errorf("received upload size: got %v, want %d", size, len(pdf))
	}
	res, code := u.capture(t, up["id"].(string))
	if code != 202 {
		t.Fatalf("capture: %d %v", code, res)
	}
	inv, _ := e.Must(200, "GET", res["resource"].(string), "", "")
	r := e.API.Do(apitest.Req{Method: "GET", Path: "/v1/supplier-invoices/" + inv["id"].(string) + "/document", Tenant: e.Tenant.ID, As: e.Tenant.Human})
	if r.Code != 200 {
		t.Fatalf("document status: got %d, want 200: %s", r.Code, r.Body)
	}
	if got := r.Body.String(); got != pdf {
		t.Errorf("document body: got %q, want %q", got, pdf)
	}
	got, _ = e.Must(200, "GET", "/v1/uploads/"+up["id"].(string), "", "")
	if got["status"] != "used" {
		t.Errorf("consumed upload status: got %v, want used", got["status"])
	}
	if got["used_by"] != inv["id"] {
		t.Errorf("consumed upload used by: got %v, want %v", got["used_by"], inv["id"])
	}
	if res, code := u.capture(t, up["id"].(string)); code != 422 || res["code"] != "payables.upload_used" {
		t.Fatalf("used: %d %v", code, res)
	}
}

// expired: after the URL's validity the PUT is refused, and an empty upload
// is reported expired.
func (u *uploadScenario) expired(t *testing.T) {
	e := u.e.in(t)
	up2, _ := e.Must(201, "POST", "/v1/uploads", `{"purpose":"supplier_invoice","content_type":"application/pdf"}`, "")
	u.clock = u.clock.Add(payables.UploadTTL + time.Minute)
	defer func() { u.clock = time.Now() }()
	if p := u.put(t, up2["url"].(string), u.pdf, "application/pdf"); p == nil || p.Status != 403 {
		t.Fatalf("expired url: %+v", p)
	}
	if res, code := u.capture(t, up2["id"].(string)); code != 422 || res["code"] != "payables.upload_expired" {
		t.Fatalf("expired: %d %v", code, res)
	}
}

// refusals of the create and of the capture request.
func (u *uploadScenario) refusals(t *testing.T) {
	e := u.e.in(t)
	for name, c := range map[string]struct {
		body string
		code string
	}{
		"purpose":      {`{"purpose":"photo","content_type":"application/pdf"}`, "payables.unknown_upload_purpose"},
		"content type": {`{"purpose":"supplier_invoice","content_type":"text/plain"}`, "payables.unsupported_content_type"},
	} {
		t.Run(name, func(t *testing.T) {
			r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/uploads", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), Body: c.body})
			if r.Code/100 != 4 {
				t.Errorf("status: got %d, want 4xx: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), c.code) {
				t.Errorf("body: got %s, want it to contain %s", r.Body, c.code)
			}
		})
	}
	for name, body := range map[string]string{
		"neither": "--b\r\nContent-Disposition: form-data; name=\"legal_entity_id\"\r\n\r\n" + e.DE.String() + "\r\n--b--\r\n",
		"both": "--b\r\nContent-Disposition: form-data; name=\"legal_entity_id\"\r\n\r\n" + e.DE.String() + "\r\n--b\r\nContent-Disposition: form-data; name=\"document\"\r\n\r\n%PDF\r\n--b\r\nContent-Disposition: form-data; name=\"upload_id\"\r\n\r\n" +
			u.up["id"].(string) + "\r\n--b--\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/supplier-invoices:capture", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(),
				Header: map[string][]string{"Content-Type": {"multipart/form-data; boundary=b"}}, Body: body})
			if r.Code != 422 {
				t.Errorf("status: got %d, want 422: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), "payables.capture_needs_document") {
				t.Errorf("body: got %s, want it to contain payables.capture_needs_document", r.Body)
			}
		})
	}
	if res, code := u.capture(t, uuid.NewString()); code != 404 {
		t.Fatalf("unknown upload: %d %v", code, res)
	}
	e.Must(404, "GET", "/v1/uploads/"+uuid.NewString(), "", "")
	// A direct document above the request limit points at uploads.
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/supplier-invoices:capture", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(),
		Header: map[string][]string{"Content-Type": {"multipart/form-data; boundary=b"}},
		Body:   "--b\r\nContent-Disposition: form-data; name=\"legal_entity_id\"\r\n\r\n" + e.DE.String() + "\r\n--b\r\nContent-Disposition: form-data; name=\"document\"\r\n\r\n" + strings.Repeat("x", payables.MaxDocument+1) + "\r\n--b--\r\n"})
	if r.Code != 413 && (r.Code != 422 || !strings.Contains(r.Body.String(), "payables.document_too_large")) {
		t.Fatalf("oversized: %d %s", r.Code, r.Body)
	}
}

// withoutStore: with a public URL the API's own upload URL is absolute;
// without object storage uploads are not served, nor is a capture from one.
func (u *uploadScenario) withoutStore(t *testing.T) {
	e, x, store := u.e.in(t), u.x, u.store
	public := apitest.NewWith(t, func(cfg *httpapi.Config) { cfg.Objects, cfg.PublicURL = store, "https://api.example.com" })
	r := public.Do(apitest.Req{Method: "POST", Path: "/v1/uploads", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), Body: `{"purpose":"supplier_invoice","content_type":"application/pdf"}`})
	if r.Code != 201 || !strings.HasPrefix(apitest.JSON(t, r)["url"].(string), "https://api.example.com/v1/uploads/") {
		t.Fatalf("public url: %d %s", r.Code, r.Body)
	}
	u.plain = apitest.NewWith(t, func(cfg *httpapi.Config) { cfg.Extractor = x })
	r = u.plain.Do(apitest.Req{Method: "POST", Path: "/v1/uploads", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), Body: `{"purpose":"supplier_invoice","content_type":"application/pdf"}`})
	if r.Code != 503 {
		t.Fatalf("without a store: %d %s", r.Code, r.Body)
	}
	u.up3, _ = e.Must(201, "POST", "/v1/uploads", `{"purpose":"supplier_invoice","content_type":"application/pdf"}`, "")
	up3 := uuid.MustParse(u.up3["id"].(string))
	for name, h := range map[string]*payables.Endpoints{"no uploads": endpoints(x), "no store": func() *payables.Endpoints {
		h := endpoints(x)
		h.Uploads = &payables.Uploads{}
		return h
	}()} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.CaptureUpload(ctx, e.DE, up3)
				return err
			})
			if got := problem.From(err).Status; got != 503 {
				t.Errorf("capture status: got %d, want 503: %v", got, err)
			}
			if _, err := h.GetUpload(context.Background(), gen.GetUploadParams{ID: up3}); problem.From(err).Status != 503 && !errors.Is(err, reqctx.ErrOutsideRequest) {
				t.Errorf("get: got %v, want status 503 or reqctx.ErrOutsideRequest", err)
			}
		})
	}
	if _, err := (&payables.Uploads{Objects: store}).Create(context.Background(), "supplier_invoice", "application/pdf"); err == nil {
		t.Error("created outside a request")
	}
	// The statement that creates an upload failing is reported.
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "create upload", func(ctx context.Context) error {
		_, err := (&payables.Uploads{Objects: store}).Create(ctx, "supplier_invoice", "application/pdf")
		return err
	})
	if _, err := endpoints(x).CaptureUpload(context.Background(), e.DE, up3); err == nil {
		t.Error("captured outside a request")
	}
	// The API's upload route without a store (a valid URL from a replica that has one).
	r = u.plain.Do(apitest.Req{Method: "PUT", Path: u.up3["url"].(string), Body: u.pdf, Header: map[string][]string{"Content-Type": {"application/pdf"}}})
	if r.Code != 503 {
		t.Fatalf("put without a store: %d %s", r.Code, r.Body)
	}
	if _, err := (&payables.Uploads{Objects: store}).Get(context.Background(), uuid.New()); err == nil {
		t.Error("read outside a request")
	}
}

// consumeFailures: a store that cannot be inspected or read when the upload
// is consumed; an upload larger than its limit when consumed (the limit
// lowered after the fact); every database statement of a capture from an
// upload failing in turn.
func (u *uploadScenario) consumeFailures(t *testing.T) {
	e, x, store := u.e.in(t), u.x, u.store
	up3 := uuid.MustParse(u.up3["id"].(string))
	for name, s := range map[string]blobstore.Store{"stat fails": failingStore{err: errors.New("store down")}, "get fails": statOnly{}} {
		t.Run(name, func(t *testing.T) {
			h := endpoints(x)
			h.Uploads = &payables.Uploads{Objects: s}
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.CaptureUpload(ctx, e.DE, up3)
				return err
			})
			if got := problem.From(err).Status; got != 503 {
				t.Errorf("capture status: got %d, want 503: %v", got, err)
			}
		})
	}
	pdf3 := u.pdf + " v3"
	if p := u.put(t, u.up3["url"].(string), pdf3, "application/pdf"); p != nil {
		t.Fatalf("put: %+v", p)
	}
	pgtest.Exec(t, `UPDATE payables.upload SET max_bytes = 10, version = version + 1 WHERE id = $1`, u.up3["id"])
	if res, code := u.capture(t, u.up3["id"].(string)); code != 422 || res["code"] != "payables.document_too_large" {
		t.Fatalf("too large when consumed: %d %v", code, res)
	}
	pgtest.Exec(t, `UPDATE payables.upload SET max_bytes = $2, version = version + 1 WHERE id = $1`, u.up3["id"], payables.MaxUpload)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "capture upload", func(ctx context.Context) error {
		x := extraction()
		x.InvoiceNumber = "CAP-U"
		h := endpoints(extractor{e: x})
		h.Objects, h.Uploads = store, &payables.Uploads{Objects: store}
		_, err := h.CaptureUpload(ctx, e.DE, up3)
		return err
	})
}

// putFailures: the API-hosted PUT with a store that cannot write, and with
// each statement failing in turn.
func (u *uploadScenario) putFailures(t *testing.T) {
	e, store := u.e.in(t), u.store
	up4, _ := e.Must(201, "POST", "/v1/uploads", `{"purpose":"supplier_invoice","content_type":"application/pdf"}`, "")
	broken := apitest.NewWith(t, func(cfg *httpapi.Config) {
		cfg.Objects, cfg.Now = failingStore{err: errors.New("store down")}, func() time.Time { return u.clock }
	})
	r := broken.Do(apitest.Req{Method: "PUT", Path: up4["url"].(string), Body: u.pdf, Header: map[string][]string{"Content-Type": {"application/pdf"}}})
	if r.Code != 503 {
		t.Fatalf("store down: %d %s", r.Code, r.Body)
	}
	uploads := &payables.Uploads{Objects: store, Secret: []byte(strings.Repeat("u", 32)), Pool: pgtest.Pool(t), AppRole: "erp_app"}
	for n := 1; n < 10; n++ {
		uploads.InTx = func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
			return pgtest.Pool(t).InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}) })
		}
		req := httptest.NewRequest("PUT", up4["url"].(string), strings.NewReader(u.pdf))
		req.Header.Set("Content-Type", "application/pdf")
		rec := httptest.NewRecorder()
		mux := http.NewServeMux()
		mux.Handle("PUT /v1/uploads/{id}/content", uploads)
		mux.ServeHTTP(rec, req)
		if rec.Code == 204 {
			if n == 1 {
				t.Fatal("received without a statement to fail")
			}
			break
		}
		if rec.Code != 500 {
			t.Fatalf("statement %d: %d %s", n, rec.Code, rec.Body)
		}
	}
	if _, _, err := store.Stat(context.Background(), blobstore.Key(e.Tenant.ID, blobstore.KindUploads, up4["id"].(string))); err != nil {
		t.Fatalf("the failed statement left the object: %v", err)
	}
}

// objectAlreadyThere: an object already at the upload's key (a PUT whose
// answer was lost): the same bytes sent again are fine, other bytes are a
// conflict.
func (u *uploadScenario) objectAlreadyThere(t *testing.T) {
	e := u.e.in(t)
	up5, _ := e.Must(201, "POST", "/v1/uploads", `{"purpose":"supplier_invoice","content_type":"application/pdf"}`, "")
	if err := u.store.Put(context.Background(), blobstore.Key(e.Tenant.ID, blobstore.KindUploads, up5["id"].(string)), []byte("other"), blobstore.PutOptions{ContentType: "application/pdf"}); err != nil {
		t.Fatal(err)
	}
	if p := u.put(t, up5["url"].(string), u.pdf, "application/pdf"); p == nil || p.Code != "payables.upload_conflict" {
		t.Fatalf("conflict: %+v", p)
	}
	if p := u.put(t, up5["url"].(string), "other", "application/pdf"); p != nil {
		t.Fatalf("same bytes again: %+v", p)
	}
}

// TestPresignedUploads: with an S3 store the upload URL is the store's,
// and the capture consumes what was uploaded there.
func TestPresignedUploads(t *testing.T) {
	e := setup(t)
	e.order(t, e.Supplier, "PO-1")
	s3 := blobstoretest.New(t)
	x := extractor{e: extraction()}
	e.API = apitest.NewWith(t, func(cfg *httpapi.Config) { cfg.Objects, cfg.Extractor = s3.Store(), x })
	up, _ := e.Must(201, "POST", "/v1/uploads", `{"purpose":"supplier_invoice","content_type":"application/pdf"}`, "")
	url := up["url"].(string)
	if !strings.HasPrefix(url, s3.URL+"/"+blobstoretest.Bucket+"/tenants/"+e.Tenant.ID.String()+"/uploads/"+up["id"].(string)+"?X-Amz-Algorithm=") {
		t.Fatalf("url: %s", url)
	}
	pdf := "%PDF-1.7 presigned"
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, url, strings.NewReader(pdf))
	req.Header.Set("Content-Type", "application/pdf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("put to the store: %v %v", resp, err)
	}
	_ = resp.Body.Close() // the status is all the test needs from the store's answer
	got, _ := e.Must(200, "GET", "/v1/uploads/"+up["id"].(string), "", "")
	if got["status"] != "open" { // the store does not tell the API
		t.Fatalf("%v", got)
	}
	body := "--b\r\nContent-Disposition: form-data; name=\"legal_entity_id\"\r\n\r\n" + e.DE.String() + "\r\n--b\r\nContent-Disposition: form-data; name=\"upload_id\"\r\n\r\n" + up["id"].(string) + "\r\n--b--\r\n"
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/supplier-invoices:capture", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(),
		Header: map[string][]string{"Content-Type": {"multipart/form-data; boundary=b"}}, Body: body})
	if r.Code != 202 {
		t.Fatalf("capture: %d %s", r.Code, r.Body)
	}
	inv, _ := e.Must(200, "GET", apitest.JSON(t, r)["resource"].(string), "", "")
	r = e.API.Do(apitest.Req{Method: "GET", Path: "/v1/supplier-invoices/" + inv["id"].(string) + "/document", Tenant: e.Tenant.ID, As: e.Tenant.Human})
	if r.Code != 200 {
		t.Fatalf("document status: got %d, want 200: %s", r.Code, r.Body)
	}
	if got := r.Body.String(); got != pdf {
		t.Errorf("document body: got %q, want %q", got, pdf)
	}
	got, _ = e.Must(200, "GET", "/v1/uploads/"+up["id"].(string), "", "")
	if got["status"] != "used" {
		t.Errorf("consumed upload status: got %v, want used", got["status"])
	}
	if size := got["size"].(float64); size != float64(len(pdf)) {
		t.Errorf("consumed upload size: got %v, want %d", size, len(pdf))
	}
	// A store that cannot presign fails the create and the read.
	odd := apitest.NewWith(t, func(cfg *httpapi.Config) { cfg.Objects = blobstore.S3{Endpoint: "::", Bucket: "b"} })
	r = odd.Do(apitest.Req{Method: "POST", Path: "/v1/uploads", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), Body: `{"purpose":"supplier_invoice","content_type":"application/pdf"}`})
	if r.Code != 503 {
		t.Fatalf("presign failure: %d %s", r.Code, r.Body)
	}
	r = odd.Do(apitest.Req{Method: "GET", Path: "/v1/uploads/" + up["id"].(string), Tenant: e.Tenant.ID, As: e.Tenant.Human})
	if r.Code != 503 {
		t.Fatalf("presign failure on read: %d %s", r.Code, r.Body)
	}
}

// TestUploadsOutsideARequest: the upload handlers called without a unit of
// work report reqctx.ErrOutsideRequest instead of using a nil transaction.
func TestUploadsOutsideARequest(t *testing.T) {
	h := endpoints(nil)
	h.Uploads = &payables.Uploads{}
	t.Run("create", func(t *testing.T) {
		if _, err := h.CreateUpload(context.Background(), &gen.UploadCreate{}, gen.CreateUploadParams{}); !errors.Is(err, reqctx.ErrOutsideRequest) {
			t.Errorf("got %v, want reqctx.ErrOutsideRequest", err)
		}
	})
	t.Run("get", func(t *testing.T) {
		if _, err := h.GetUpload(context.Background(), gen.GetUploadParams{ID: uuid.New()}); !errors.Is(err, reqctx.ErrOutsideRequest) {
			t.Errorf("got %v, want reqctx.ErrOutsideRequest", err)
		}
	})
}
