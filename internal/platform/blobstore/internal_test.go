package blobstore

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDirWriteFailures fails the writes a Put makes after the exclusive
// create succeeded: the content (a read-only descriptor) and the sidecar.
func TestDirWriteFailures(t *testing.T) {
	root := t.TempDir()
	d := Dir{Root: root}
	readOnly := filepath.Join(root, "ro")
	os.WriteFile(readOnly, nil, 0o600)
	openFile = func(string, int, os.FileMode) (*os.File, error) { return os.Open(readOnly) }
	defer func() { openFile = os.OpenFile }()
	if err := d.Put(context.Background(), "tenants/x/a", []byte("1"), PutOptions{}); err == nil || errors.Is(err, ErrExists) {
		t.Fatalf("write on a read-only descriptor: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tenants", "x", "a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed write left the object behind")
	}
	openFile = os.OpenFile
	writeFile = func(string, []byte, os.FileMode) error { return errors.New("disk full") }
	defer func() { writeFile = os.WriteFile }()
	if err := d.Put(context.Background(), "tenants/x/b", []byte("1"), PutOptions{}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("sidecar: %v", err)
	}
}

func TestEncoding(t *testing.T) {
	if got := encodePath("tenants/a b/é~x"); got != "tenants/a%20b/%C3%A9~x" {
		t.Fatalf("encodePath: %s", got)
	}
	if got := canonicalQuery(url.Values{"b": {"2", "1"}, "a": {"x y"}}); got != "a=x%20y&b=1&b=2" {
		t.Fatalf("canonicalQuery: %s", got)
	}
}

// TestSignV4Defaults signs a request built by hand, without Host or path.
func TestSignV4Defaults(t *testing.T) {
	req := &http.Request{Method: "GET", URL: &url.URL{Scheme: "https", Host: "example.amazonaws.com"}, Header: http.Header{}}
	SignV4(req, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "service", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	// The same signature as the test suite's get-vanilla: host from the URL, path "/".
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31") {
		t.Fatalf("%s", got)
	}
	s := S3{Now: func() time.Time { return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC) }}
	if !s.now().Equal(time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)) {
		t.Fatal("Now not used")
	}
}
