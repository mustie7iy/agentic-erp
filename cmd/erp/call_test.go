package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestCallMultipartAndFiles: erp call sends a multipart operation's body
// as a form, -file adding a file's bytes as a field, prints an answer that
// is not JSON as it came and writes one to -out (walkthrough, 2026-10-22:
// every body went as JSON and a pain.001 file printed as null).
func TestCallMultipartAndFiles(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFile(t, "statement.xml", []byte("<Document>camt</Document>\n"))
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/bank-statements":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("the upload is not a multipart form: %v", err)
				w.WriteHeader(400)
				return
			}
			for k, v := range r.MultipartForm.Value {
				got[k] = v[0]
			}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"file_sha256": "ab", "statements": []}`))
		case "GET /v1/bank-files/f1/content":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte("<Document>pain.001</Document>\n"))
		default:
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(srv.Close)
	getenv = func(k string) string { return map[string]string{"ERP_API_URL": srv.URL, "ERP_TOKEN": "t"}[k] }
	t.Cleanup(func() { getenv = os.Getenv })

	out := must(t, "call", "importBankStatements", "-body", `{"house_bank_id": "h1", "format": "camt.053", "n": 2}`, "-file", "file=@statement.xml")
	if !strings.HasPrefix(out, "201") {
		t.Errorf("upload answer: got %q, want 201 first", out)
	}
	for field, want := range map[string]string{"house_bank_id": "h1", "format": "camt.053", "n": "2", "file": "<Document>camt</Document>\n"} {
		if got[field] != want {
			t.Errorf("form field %s: got %q, want %q", field, got[field], want)
		}
	}
	if out := must(t, "call", "getBankFileContent", "-p", "id=f1"); !strings.Contains(out, "<Document>pain.001</Document>") {
		t.Errorf("an XML answer: got %q, want it printed as it came", out)
	}
	if out := must(t, "call", "getBankFileContent", "-p", "id=f1", "-out", "pain.xml"); !strings.Contains(out, "wrote 30 bytes to pain.xml") {
		t.Errorf("-out: got %q, want the bytes written named", out)
	}
	if b, err := os.ReadFile("pain.xml"); err != nil || string(b) != "<Document>pain.001</Document>\n" {
		t.Errorf("-out file: got %q, %v, want the answer's body", b, err)
	}
	if out := must(t, "call", "getBankFileContent", "-p", "id=empty"); !strings.Contains(out, "null") {
		t.Errorf("an empty answer: got %q, want null", out)
	}
	t.Run("refused", func(t *testing.T) {
		fails(t, "want field=@path", "call", "importBankStatements", "-file", "statement.xml")
		fails(t, "want field=@path", "call", "importBankStatements", "-file", "=@statement.xml")
		fails(t, "read file of file", "call", "importBankStatements", "-file", "file=@missing.xml")
		fails(t, "needs a body that is a JSON object", "call", "importBankStatements", "-body", `[1]`, "-file", "file=@statement.xml")
		fails(t, "object of its fields", "call", "importBankStatements", "-body", `[1]`)
		fails(t, "write no/such/dir/pain.xml", "call", "getBankFileContent", "-p", "id=f1", "-out", "no/such/dir/pain.xml")
	})
}
