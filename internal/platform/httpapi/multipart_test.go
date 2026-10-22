package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFileParts: a multipart body's file parts reach the generated server
// as form values, like plain fields; a body that is not a readable
// multipart form is 400 request.invalid_body; other bodies pass as they
// are.
func TestFileParts(t *testing.T) {
	var got map[string][]string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		if r.MultipartForm != nil {
			got = r.MultipartForm.Value
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h := fileParts(next)
	const plain = "--b\r\nContent-Disposition: form-data; name=\"format\"\r\n\r\ncamt.053\r\n"
	const file = "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"s.xml\"\r\nContent-Type: application/xml\r\n\r\n<Document/>\r\n"
	cases := map[string]struct {
		method            string // POST when empty
		contentType, body string
		code              int
		want              map[string]string
	}{
		"a file part and a plain field": {"", "multipart/form-data; boundary=b", plain + file + "--b--\r\n", 204, map[string]string{"format": "camt.053", "file": "<Document/>"}},
		"not multipart":                 {"", "application/json", `{}`, 204, nil},
		"no boundary":                   {"", "multipart/form-data", plain, 400, nil},
		"a malformed part header":       {"", "multipart/form-data; boundary=b", "--b\r\nno colon here\r\n\r\nx\r\n--b--\r\n", 400, nil},
		"a part cut short":              {"", "multipart/form-data; boundary=b", "--b\r\nContent-Disposition: form-data; name=\"file\"\r\n\r\n<Docu", 400, nil},
		"a GET's body left unread":      {"GET", "multipart/form-data; boundary=b", plain + "--b--\r\n", 204, nil},
		"too many parts":                {"", "multipart/form-data; boundary=b", strings.Repeat(plain, maxParts+1) + "--b--\r\n", 400, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			method := c.method
			if method == "" {
				method = "POST"
			}
			r := httptest.NewRequest(method, "/v1/bank-statements", strings.NewReader(c.body))
			r.Header.Set("Content-Type", c.contentType)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if c.method == "GET" && got != nil {
				t.Errorf("a GET's form: got %v, want it left unread", got)
			}
			if rec.Code != c.code {
				t.Fatalf("status: got %d, want %d: %s", rec.Code, c.code, rec.Body)
			}
			if c.code == 400 && !strings.Contains(rec.Body.String(), "request.invalid_body") {
				t.Errorf("problem: got %s, want request.invalid_body", rec.Body)
			}
			for field, want := range c.want {
				if v := got[field]; len(v) != 1 || v[0] != want {
					t.Errorf("value %s: got %q, want %q", field, v, want)
				}
			}
		})
	}
}
