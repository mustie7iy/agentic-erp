package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
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
			if f, _, err := r.FormFile("document"); err == nil {
				b, _ := io.ReadAll(f)
				line += " document=" + itoa(len(b))
			}
			seen = append(seen, line)
			switch r.FormValue("legal_entity_id") {
			case "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d22":
				w.WriteHeader(422)
				w.Write([]byte(`{"code": "payables.unreadable_document", "detail": "The document cannot be captured: no number."}`))
			case "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d33":
				w.WriteHeader(500)
				w.Write([]byte(`<html>`))
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
	// Failures: the PUT refused, no object storage, the capture refused, an answer that is not JSON, and usage.
	fails(t, "uploading the document: 403", "capture", "-legal-entity", le, "-file", small, "-upload")
	fails(t, "503 platform.unavailable", "capture", "-legal-entity", le, "-file", small, "-upload")
	fails(t, "upload URL", "capture", "-legal-entity", le, "-file", small, "-upload")
	fails(t, "connect", "capture", "-legal-entity", le, "-file", small, "-upload")
	fails(t, "422 payables.unreadable_document", "capture", "-legal-entity", "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d22", "-file", small)
	fails(t, "not JSON", "capture", "-legal-entity", "01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d33", "-file", small)
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
