package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	capturepkg "github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

func TestCaptureCommand(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	var uploads, puts int
	absolute := false
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/uploads" && r.Method == "POST":
			if r.Header.Get("Authorization") != "Bearer t" {
				w.WriteHeader(401)
				w.Write([]byte(`{"code": "authn.invalid_token", "detail": "A bearer token is required."}`))
				return
			}
			uploads++
			if uploads == 4 { // the fourth upload cannot be stored
				w.WriteHeader(503)
				w.Write([]byte(`{"code": "platform.unavailable", "detail": "This server has no object storage."}`))
				return
			}
			url := "/v1/uploads/u1/content?tenant=x&expires=1&signature=s"
			switch {
			case uploads == 5: // an upload URL that is not a URL
				url = "::"
			case uploads == 6: // an unreachable store
				url = "http://127.0.0.1:1/bucket/x"
			case absolute:
				url = srv.URL + url
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"id": "u1", "url": "` + url + `", "method": "PUT", "headers": {"Content-Type": "application/pdf"}}`))
		case r.URL.Path == "/v1/uploads/u1/content" && r.Method == "PUT":
			body, _ := io.ReadAll(r.Body)
			puts++
			seen = append(seen, "PUT "+r.Header.Get("Content-Type")+" "+r.Header.Get("Authorization")+" bytes="+itoa(len(body)))
			if puts == 3 {
				w.WriteHeader(403)
				w.Write([]byte(`{"code": "payables.invalid_upload_url", "detail": "The upload URL is not valid."}`))
				return
			}
			w.WriteHeader(204)
		case r.URL.Path == "/v1/supplier-invoices:capture":
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				w.WriteHeader(400)
				return
			}
			line := "capture le=" + r.FormValue("legal_entity_id") + " upload=" + r.FormValue("upload_id")
			// The contract's document is a form value (a string of bytes), as
			// the generated server reads it, not a file part.
			if doc, ok := r.MultipartForm.Value["document"]; ok {
				line += " document=" + itoa(len(doc[0]))
			}
			seen = append(seen, line)
			switch r.FormValue("legal_entity_id") {
			case "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d22":
				w.WriteHeader(422)
				w.Write([]byte(`{"code": "payables.unreadable_document", "detail": "The document cannot be captured: no number."}`))
			case "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d33":
				w.WriteHeader(500)
				w.Write([]byte(`<html>`))
			case "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d44": // an answer cut off: shorter than its declared length
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(202)
				w.Write([]byte(`{"id"`))
			default:
				w.WriteHeader(202)
				w.Write([]byte(`{"id": "op1", "kind": "payables.capture", "status": "succeeded"}`))
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	env := map[string]string{"ERP_API_URL": srv.URL, "ERP_TOKEN": "t"}
	getenv = func(k string) string { return env[k] }
	defer func() { getenv = os.Getenv }()
	dir := t.TempDir()
	small := filepath.Join(dir, "small.pdf")
	writeFile(t, small, []byte("%PDF small"))
	large := filepath.Join(dir, "scan.PNG")
	writeFile(t, large, make([]byte, requestLimit+1))
	le := "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d11"

	// Small: in the request. Large: through an upload (a path on the API). -upload: forced.
	if code, out := cli(t, "capture", "-legal-entity", le, "-file", small); code != 0 || !strings.Contains(out, `"op1"`) {
		t.Fatalf("small: %d %s", code, out)
	}
	if code, out := cli(t, "capture", "-legal-entity", le, "-file", large); code != 0 || !strings.Contains(out, "uploaded 5242881 bytes as u1") {
		t.Fatalf("large: %d %s", code, out)
	}
	absolute = true
	if code, out := cli(t, "capture", "-legal-entity", le, "-file", small, "-upload", "-content-type", "application/pdf"); code != 0 || !strings.Contains(out, "uploaded 10 bytes") {
		t.Fatalf("forced: %d %s", code, out)
	}
	mu.Lock()
	want := []string{"capture le=" + le + " upload= document=10", "PUT application/pdf  bytes=5242881", "capture le=" + le + " upload=u1",
		"PUT application/pdf  bytes=10", "capture le=" + le + " upload=u1"}
	got := strings.Join(seen, "\n")
	mu.Unlock()
	if got != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	printFails(t, "capture", "-legal-entity", le, "-file", small)
	// Failures: the PUT refused, no object storage, the capture refused, a refusal that is not JSON (its status reported), an answer cut off, and usage.
	fails(t, "uploading the document: 403", "capture", "-legal-entity", le, "-file", small, "-upload")
	fails(t, "503 platform.unavailable", "capture", "-legal-entity", le, "-file", small, "-upload")
	fails(t, "upload URL", "capture", "-legal-entity", le, "-file", small, "-upload")
	fails(t, "connect", "capture", "-legal-entity", le, "-file", small, "-upload")
	fails(t, "422 payables.unreadable_document", "capture", "-legal-entity", "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d22", "-file", small)
	fails(t, "capture: API 500", "capture", "-legal-entity", "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d33", "-file", small)
	fails(t, "capture: send: apitools: POST /v1/supplier-invoices:capture", "capture", "-legal-entity", "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d44", "-file", small)
	fails(t, "-legal-entity and -file are required", "capture", "-file", small)
	fails(t, "is not an id", "capture", "-legal-entity", "acme", "-file", small)
	fails(t, "no such file", "capture", "-legal-entity", le, "-file", filepath.Join(dir, "none.pdf"))
	fails(t, "flag provided but not defined", "capture", "-x")
	env["ERP_TOKEN"] = ""
	fails(t, "ERP_TOKEN is not set", "capture", "-legal-entity", le, "-file", small)
	env["ERP_TOKEN"], env["ERP_API_URL"] = "t", "http://127.0.0.1:1"
	fails(t, "connect", "capture", "-legal-entity", le, "-file", small)
	fails(t, "connect", "capture", "-legal-entity", le, "-file", small, "-upload")
	env["ERP_API_URL"] = "::"
	fails(t, "", "capture", "-legal-entity", le, "-file", small, "-upload")
	// Content types by extension.
	for name, want := range map[string]string{"a.png": "image/png", "a.JPG": "image/jpeg", "a.jpeg": "image/jpeg", "a.xml": "application/xml", "a.pdf": "application/pdf", "noext": "application/pdf"} {
		t.Run(name, func(t *testing.T) {
			if got := contentTypeOf(name); got != want {
				t.Errorf("contentTypeOf(%s): got %s, want %s", name, got, want)
			}
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// recordingExtractor records the document capture asked it to read.
type recordingExtractor struct{ got []byte }

func (x *recordingExtractor) Extract(_ context.Context, pdf []byte) (*capturepkg.Extraction, error) {
	x.got = pdf
	return nil, errors.New("not read")
}

// TestCaptureReachesTheServer: a document within the request limit reaches
// the API's extraction as the contract's document value, which the
// generated server reads from the form's values.
func TestCaptureReachesTheServer(t *testing.T) {
	x := &recordingExtractor{}
	f := fixture.New(t, "payables.invoice.capture")
	f.API = apitest.NewWith(t, func(c *app.Config) { c.Extractor = x })
	srv := httptest.NewServer(f.API.Handler)
	t.Cleanup(srv.Close)
	c := &apitools.Client{BaseURL: srv.URL, Token: f.API.Issuer.Token(t, f.Tenant.ID, f.Tenant.Human.Subject, nil)}
	doc := []byte("%PDF-1.4 an invoice")
	_, err := capture(context.Background(), c, map[string]string{"legal_entity_id": f.DE.String()}, doc)
	if err == nil {
		t.Fatal("got no error, want the extraction's refusal")
	}
	if string(x.got) != string(doc) {
		t.Errorf("extracted: got %q (%v), want the document", x.got, err)
	}
}

// TestCaptureOrderCommand (T90a): erp capture -order uploads a customer's
// purchase order for order capture and captures it with the revenue
// account and tax code its items take, printing the Operation; an account
// that is not an id, a missing token and the API's refusals are reported.
func TestCaptureOrderCommand(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(body))
		switch r.URL.Path {
		case "/v1/sales-order-uploads":
			w.WriteHeader(201)
			w.Write([]byte(`{"id": "u9", "url": "/v1/uploads/u9/content?s=1", "method": "PUT", "headers": {"Content-Type": "text/plain"}}`))
		case "/v1/uploads/u9/content":
			w.WriteHeader(204)
		case "/v1/sales-orders:capture":
			switch {
			case strings.Contains(string(body), "REFUSE"):
				w.WriteHeader(422)
				w.Write([]byte(`{"code": "sales.capture_customer_unknown", "detail": "No business partner."}`))
			case strings.Contains(string(body), "GARBLE"):
				w.WriteHeader(202)
				w.Write([]byte(`not json`))
			default:
				w.WriteHeader(202)
				w.Write([]byte(`{"id": "op9", "status": "succeeded", "resource": "/v1/sales-orders/o1"}`))
			}
		}
	}))
	defer srv.Close()
	env := map[string]string{"ERP_API_URL": srv.URL, "ERP_TOKEN": "t"}
	getenv = func(k string) string { return env[k] }
	defer func() { getenv = os.Getenv }()
	file := filepath.Join(t.TempDir(), "order.txt")
	writeFile(t, file, []byte("Purchase order NW-4711"))
	le, account := "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d11", "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d31"
	args := []string{"capture", "-order", "-legal-entity", le, "-file", file, "-account", account, "-tax-code", "GO5"}
	with := func(code string) []string { return append(append([]string{}, args[:len(args)-1]...), code) }
	if code, out := cli(t, args...); code != 0 || !strings.Contains(out, `"op9"`) {
		t.Fatalf("captured: got %d %s, want the operation", code, out)
	}
	mu.Lock()
	got := strings.Join(seen, "\n")
	mu.Unlock()
	for _, want := range []string{"POST /v1/sales-order-uploads", `"content_type":"text/plain"`, "PUT /v1/uploads/u9/content Purchase order NW-4711",
		`"account_id":"` + account + `"`, `"tax_code":"GO5"`, `"upload_id":"u9"`} {
		if !strings.Contains(got, want) {
			t.Errorf("requests: got\n%s\nwant one with %s", got, want)
		}
	}
	for name, c := range map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"a refusal":             {with("REFUSE"), env, "sales.capture_customer_unknown"},
		"an answer not JSON":    {with("GARBLE"), env, "decode"},
		"an account not an id":  {[]string{"capture", "-order", "-legal-entity", le, "-file", file, "-account", "x"}, env, "is not an id"},
		"without a token":       {args, map[string]string{"ERP_API_URL": srv.URL}, "ERP_TOKEN"},
		"an upload not started": {args, map[string]string{"ERP_API_URL": "http://127.0.0.1:1", "ERP_TOKEN": "t"}, "upload"},
	} {
		t.Run(name, func(t *testing.T) {
			getenv = func(k string) string { return c.env[k] }
			if code, out := cli(t, c.args...); code == 0 || !strings.Contains(out, c.want) {
				t.Errorf("got %d %s, want a failure naming %s", code, out, c.want)
			}
		})
	}
}
