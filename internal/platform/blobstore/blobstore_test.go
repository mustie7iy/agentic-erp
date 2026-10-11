package blobstore_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
)

// exercise puts and gets through any store: write once, typed, missing keys.
func exercise(t *testing.T, s blobstore.Store) {
	t.Helper()
	ctx := context.Background()
	ten := uuid.New()
	key := blobstore.Key(ten, blobstore.KindDocuments, "abc")
	if !blobstore.IsKey(key) || blobstore.IsKey("sha256:abc") || !strings.HasPrefix(key, "tenants/"+ten.String()+"/documents/") {
		t.Fatalf("key %s", key)
	}
	if err := s.Put(ctx, key, []byte("%PDF-1"), blobstore.PutOptions{ContentType: "application/pdf"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, key, []byte("other"), blobstore.PutOptions{}); !errors.Is(err, blobstore.ErrExists) {
		t.Fatalf("second put: %v", err)
	}
	content, ct, err := s.Get(ctx, key)
	if err != nil || string(content) != "%PDF-1" || ct != "application/pdf" {
		t.Fatalf("get: %s %s %v", content, ct, err)
	}
	if _, _, err := s.Get(ctx, blobstore.Key(ten, blobstore.KindDocuments, "nope")); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// Default content type; a retention lock is accepted.
	locked := blobstore.Key(ten, blobstore.KindAnchors, "000001")
	if err := s.Put(ctx, locked, []byte(`{"seq":1}`), blobstore.PutOptions{RetainUntil: time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if _, ct, err := s.Get(ctx, locked); err != nil || ct != "application/octet-stream" {
		t.Fatalf("locked: %s %v", ct, err)
	}
	for _, bad := range []string{"", "/abs", "tenants/../x", "tenants//x", "a b", "trailing/", strings.Repeat("k", 1025)} {
		if err := s.Put(ctx, bad, nil, blobstore.PutOptions{}); err == nil || errors.Is(err, blobstore.ErrExists) {
			t.Errorf("put %q: %v", bad, err)
		}
		if _, _, err := s.Get(ctx, bad); err == nil || errors.Is(err, blobstore.ErrNotFound) {
			t.Errorf("get %q: %v", bad, err)
		}
	}
}

func TestDir(t *testing.T) {
	root := t.TempDir()
	d := blobstore.Dir{Root: root}
	exercise(t, d)
	// Content without its sidecar is served with the default type.
	key := blobstore.Key(uuid.New(), blobstore.KindBankFiles, "f.xml")
	if err := d.Put(context.Background(), key, []byte("<x/>"), blobstore.PutOptions{ContentType: "application/xml"}); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(root, filepath.FromSlash(key)) + ".meta")
	if _, ct, err := d.Get(context.Background(), key); err != nil || ct != "application/octet-stream" {
		t.Fatalf("%s %v", ct, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// Failures of the filesystem are reported, not hidden.
	file := filepath.Join(root, "file")
	os.WriteFile(file, nil, 0o600)
	if err := (blobstore.Dir{Root: file}).Put(context.Background(), "tenants/x/y", nil, blobstore.PutOptions{}); err == nil {
		t.Error("mkdir below a file succeeded")
	}
	sealed := filepath.Join(root, "sealed")
	os.MkdirAll(filepath.Join(sealed, "tenants"), 0o700)
	os.Chmod(filepath.Join(sealed, "tenants"), 0o500)
	defer os.Chmod(filepath.Join(sealed, "tenants"), 0o700)
	if err := (blobstore.Dir{Root: sealed}).Put(context.Background(), "tenants/x", nil, blobstore.PutOptions{}); err == nil || errors.Is(err, blobstore.ErrExists) {
		t.Errorf("create in a read-only directory: %v", err)
	}
	unreadable := filepath.Join(root, "tenants", "u")
	os.MkdirAll(filepath.Dir(unreadable), 0o700)
	os.WriteFile(unreadable, []byte("x"), 0o000)
	if _, _, err := d.Get(context.Background(), "tenants/u"); err == nil || errors.Is(err, blobstore.ErrNotFound) {
		t.Errorf("unreadable: %v", err)
	}
}

func TestS3(t *testing.T) {
	srv := blobstoretest.New(t)
	s := srv.Store()
	exercise(t, s)
	ctx := context.Background()
	// The lock headers reached the service.
	var locked blobstoretest.Object
	for _, k := range srv.Keys() {
		if strings.Contains(k, "/audit/anchors/") {
			locked, _ = srv.Object(k)
		}
	}
	if locked.LockMode != "COMPLIANCE" || locked.RetainUntil != "2036-01-01T00:00:00Z" {
		t.Fatalf("lock: %+v", locked)
	}
	// Service failures are reported with the service's code, or its status.
	srv.Fail = http.StatusInternalServerError
	if err := s.Put(ctx, "tenants/t/x", nil, blobstore.PutOptions{}); err == nil || !strings.Contains(err.Error(), "InternalError") {
		t.Errorf("put failure: %v", err)
	}
	if _, _, err := s.Get(ctx, "tenants/t/x"); err == nil || !strings.Contains(err.Error(), "InternalError") {
		t.Errorf("get failure: %v", err)
	}
	srv.FailPlain = true
	if _, _, err := s.Get(ctx, "tenants/t/x"); err == nil || !strings.Contains(err.Error(), "500 Internal Server Error") {
		t.Errorf("plain failure: %v", err)
	}
	srv.Fail, srv.FailPlain = 0, false
	// Wrong credentials are refused by the service.
	wrong := s
	wrong.SecretKey = "nope"
	if err := wrong.Put(ctx, "tenants/t/y", nil, blobstore.PutOptions{}); err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("wrong secret: %v", err)
	}
	// An unreachable service and a bad endpoint are errors too.
	down := s
	down.Endpoint = "http://127.0.0.1:1"
	if err := down.Put(ctx, "tenants/t/z", nil, blobstore.PutOptions{}); err == nil {
		t.Error("unreachable service")
	}
	down.Endpoint = "://bad"
	if _, _, err := down.Get(ctx, "tenants/t/z"); err == nil {
		t.Error("bad endpoint")
	}
	// A response body that breaks off is an error.
	if _, _, err := (blobstore.S3{Endpoint: srv.URL, Bucket: blobstoretest.Bucket, Region: blobstoretest.Region, AccessKey: blobstoretest.AccessKey,
		SecretKey: blobstoretest.SecretKey, Client: &http.Client{Transport: brokenBody{}}}).Get(ctx, "tenants/t/x"); err == nil {
		t.Error("broken body")
	}
	// Keys with characters outside the unreserved set are encoded and round-trip.
	odd := "tenants/" + uuid.NewString() + "/documents/a~b.c-d_e"
	if err := s.Put(ctx, odd, []byte("1"), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.Object(odd); !ok {
		t.Fatalf("stored under %v", srv.Keys())
	}
}

// brokenBody answers 200 with a body that fails while being read.
type brokenBody struct{}

func (brokenBody) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: failingReader{}}, nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (failingReader) Close() error             { return nil }

// TestSignV4KnownAnswer checks the signer against the AWS Signature Version
// 4 test suite's get-vanilla-query case (region us-east-1, service
// "service", 2015-08-30T12:36:00Z).
func TestSignV4KnownAnswer(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	blobstore.SignV4(req, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "service", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	// A query string is canonicalized: sorted, encoded (get-vanilla-query-order-key-case).
	req, _ = http.NewRequest("GET", "https://example.amazonaws.com/?Param2=value2&Param1=value1", nil)
	blobstore.SignV4(req, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "service", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500") {
		t.Fatalf("query: %s", got)
	}
}
