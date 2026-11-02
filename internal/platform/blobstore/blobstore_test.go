package blobstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/awscreds"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// must fails the test when filesystem setup it relies on did not happen.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// seal sets a directory's mode for a failure case and restores it at the end
// of the test so the temporary directory can be removed.
func seal(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	must(t, os.Chmod(dir, mode))
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
}

// exercise puts and gets through any store: write once, typed, missing keys.
func exercise(t *testing.T, s blobstore.Store) {
	t.Helper()
	ctx := context.Background()
	ten := uuid.New()
	key := blobstore.Key(ten, blobstore.KindDocuments, "abc")
	if !blobstore.IsKey(key) {
		t.Fatalf("IsKey(%s): got false, want true", key)
	}
	if blobstore.IsKey("sha256:abc") {
		t.Fatalf("IsKey(sha256:abc): got true, want false")
	}
	if want := "tenants/" + ten.String() + "/documents/"; !strings.HasPrefix(key, want) {
		t.Fatalf("key: got %s, want prefix %s", key, want)
	}
	if err := s.Put(ctx, key, []byte("%PDF-1"), blobstore.PutOptions{ContentType: "application/pdf"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, key, []byte("other"), blobstore.PutOptions{}); !errors.Is(err, blobstore.ErrExists) {
		t.Fatalf("second put: %v", err)
	}
	content, ct, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(content) != "%PDF-1" {
		t.Fatalf("get content: got %s, want %%PDF-1", content)
	}
	if ct != "application/pdf" {
		t.Fatalf("get content type: got %s, want application/pdf", ct)
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
	for name, bad := range map[string]string{"empty": "", "absolute": "/abs", "dot-dot": "tenants/../x", "empty segment": "tenants//x",
		"space": "a b", "trailing slash": "trailing/", "too long": strings.Repeat("k", 1025)} {
		t.Run("bad key "+name, func(t *testing.T) {
			if err := s.Put(ctx, bad, nil, blobstore.PutOptions{}); err == nil || errors.Is(err, blobstore.ErrExists) {
				t.Errorf("put %q: got %v, want an invalid-key error", bad, err)
			}
			if _, _, err := s.Get(ctx, bad); err == nil || errors.Is(err, blobstore.ErrNotFound) {
				t.Errorf("get %q: got %v, want an invalid-key error", bad, err)
			}
		})
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
	must(t, os.Remove(filepath.Join(root, filepath.FromSlash(key))+".meta"))
	if _, ct, err := d.Get(context.Background(), key); err != nil || ct != "application/octet-stream" {
		t.Fatalf("%s %v", ct, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// Failures of the filesystem are reported, not hidden.
	file := filepath.Join(root, "file")
	must(t, os.WriteFile(file, nil, 0o600))
	if err := (blobstore.Dir{Root: file}).Put(context.Background(), "tenants/x/y", nil, blobstore.PutOptions{}); err == nil {
		t.Error("mkdir below a file succeeded")
	}
	sealed := filepath.Join(root, "sealed")
	must(t, os.MkdirAll(filepath.Join(sealed, "tenants"), 0o700))
	seal(t, filepath.Join(sealed, "tenants"), 0o500)
	if err := (blobstore.Dir{Root: sealed}).Put(context.Background(), "tenants/x", nil, blobstore.PutOptions{}); err == nil || errors.Is(err, blobstore.ErrExists) {
		t.Errorf("create in a read-only directory: %v", err)
	}
	unreadable := filepath.Join(root, "tenants", "u")
	must(t, os.MkdirAll(filepath.Dir(unreadable), 0o700))
	must(t, os.WriteFile(unreadable, []byte("x"), 0o000))
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
	// The locked write carried the body's SHA-256, which S3 requires with
	// Object Lock (the double refuses a locked write without one).
	if want := "tB6VxD84jVYbFGMm2E46a54xxO8yq5fAR3TDGq2jJGo="; locked.Checksum != want {
		t.Fatalf("checksum: got %q, want %q", locked.Checksum, want)
	}
	// Service failures are reported with the service's code, or its status.
	srv.Fail = http.StatusInternalServerError
	pgtest.CheckErr(t, s.Put(ctx, "tenants/t/x", nil, blobstore.PutOptions{}), "InternalError")
	_, _, err := s.Get(ctx, "tenants/t/x")
	pgtest.CheckErr(t, err, "InternalError")
	srv.FailPlain = true
	_, _, err = s.Get(ctx, "tenants/t/x")
	pgtest.CheckErr(t, err, "500 Internal Server Error")
	srv.Fail, srv.FailPlain = 0, false
	// Wrong credentials are refused by the service.
	wrong := s
	wrong.SecretKey = "nope"
	pgtest.CheckErr(t, wrong.Put(ctx, "tenants/t/y", nil, blobstore.PutOptions{}), "SignatureDoesNotMatch")
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

// TestStatAndPresignedUploads: both stores report size and type without
// reading; S3 hands out presigned PUT URLs bound to a content type and a
// validity, which the double enforces.
func TestStatAndPresignedUploads(t *testing.T) {
	ctx := context.Background()
	dir := blobstore.Dir{Root: t.TempDir()}
	srv := blobstoretest.New(t)
	s3 := srv.Store()
	for name, store := range map[string]blobstore.Store{"dir": dir, "s3": s3} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := store.Stat(ctx, "tenants/x/documents/none"); !errors.Is(err, blobstore.ErrNotFound) {
				t.Fatalf("stat of nothing: got %v, want %v", err, blobstore.ErrNotFound)
			}
			if _, _, err := store.Stat(ctx, "../etc"); err == nil {
				t.Fatalf("stat of a bad key: got nil error, want an error")
			}
			if err := store.Put(ctx, "tenants/x/documents/a", []byte("hello"), blobstore.PutOptions{ContentType: "text/plain"}); err != nil {
				t.Fatal(err)
			}
			size, ct, err := store.Stat(ctx, "tenants/x/documents/a")
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if size != 5 {
				t.Errorf("stat size: got %d, want 5", size)
			}
			if ct != "text/plain" {
				t.Errorf("stat content type: got %q, want text/plain", ct)
			}
		})
	}
	// A directory store does not presign; S3 does.
	if _, ok := blobstore.Store(dir).(blobstore.Presigner); ok {
		t.Fatal("the directory store presigns")
	}
	if _, err := s3.PresignPut(ctx, "../x", "application/pdf", time.Minute); err == nil {
		t.Fatal("presigned a bad key")
	}
	key := "tenants/x/uploads/u1"
	u, err := s3.PresignPut(ctx, key, "application/pdf", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	put := func(url, contentType string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader("%PDF-1.7"))
		req.Header.Set("Content-Type", contentType)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close() // the body is not read; the status code is the answer
		return resp.StatusCode
	}
	if code := put(u, "image/png"); code != http.StatusForbidden {
		t.Fatalf("another content type: %d", code)
	}
	if code := put(u, "application/pdf"); code != http.StatusOK {
		t.Fatalf("presigned put: %d", code)
	}
	if size, ct, err := s3.Stat(ctx, key); err != nil {
		t.Fatalf("uploaded: %v", err)
	} else if size != 8 || ct != "application/pdf" {
		t.Fatalf("uploaded: got %d bytes of %q, want 8 bytes of application/pdf", size, ct)
	}
	// Past its validity the URL is refused; a URL made with other credentials too.
	srv.Now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	if code := put(u, "application/pdf"); code != http.StatusForbidden {
		t.Fatalf("expired: %d", code)
	}
	srv.Now = nil
	other := s3
	other.SecretKey = "nope"
	u2, _ := other.PresignPut(ctx, key, "application/pdf", time.Minute)
	if code := put(u2, "application/pdf"); code != http.StatusForbidden {
		t.Fatalf("foreign credentials: %d", code)
	}
	// Failures of the service and of the transport are reported by Stat.
	srv.Fail = http.StatusInternalServerError
	_, _, err = s3.Stat(ctx, key)
	pgtest.WantErr(t, err, "HEAD")
	srv.Fail = 0
	down := s3
	down.Endpoint = "http://127.0.0.1:1"
	if _, _, err := down.Stat(ctx, key); err == nil {
		t.Fatal("unreachable service")
	}
	if _, err := (blobstore.S3{Endpoint: "::", Bucket: "b"}).PresignPut(ctx, key, "application/pdf", time.Minute); err == nil {
		t.Fatal("bad endpoint presigned")
	}
	// A directory store whose file cannot be inspected.
	if _, _, err := (blobstore.Dir{Root: filepath.Join(dir.Root, "tenants/x/documents/a")}).Stat(ctx, "tenants/y/documents/b"); err == nil || errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("stat below a file: %v", err)
	}
}

// TestListAndDelete: both stores list a prefix and delete objects, and
// refuse to delete one whose retention holds (the one deletion the
// platform makes is the erasure of an offboarded tenant).
func TestListAndDelete(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	dir := blobstore.Dir{Root: t.TempDir(), Now: func() time.Time { return clock }}
	srv := blobstoretest.New(t)
	srv.Now = func() time.Time { return clock }
	srv.PageSize = 2
	s3 := srv.Store()
	for name, store := range map[string]blobstore.Eraser{"dir": dir, "s3": s3} {
		if !t.Run(name, func(t *testing.T) {
			st := store.(blobstore.Store)
			tenant := "tenants/" + uuid.NewString()
			for _, k := range []string{"/documents/a", "/documents/b", "/bank-files/f.xml", "/uploads/u1", "/audit/anchors/000000000001.json"} {
				opts := blobstore.PutOptions{ContentType: "text/plain"}
				if strings.Contains(k, "anchors") {
					opts.RetainUntil = clock.Add(time.Hour)
				}
				if err := st.Put(ctx, tenant+k, []byte("x"), opts); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
			// Another tenant whose id starts the same.
			if err := st.Put(ctx, tenant+"-other/documents/c", []byte("y"), blobstore.PutOptions{}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			keys, err := store.List(ctx, tenant+"/")
			if err != nil || len(keys) != 5 {
				t.Fatalf("list: got %v %v, want 5 keys", keys, err)
			}
			if want := tenant + "/audit/anchors/000000000001.json"; keys[0] != want {
				t.Errorf("list first key: got %s, want %s", keys[0], want)
			}
			if want := tenant + "/uploads/u1"; keys[4] != want {
				t.Errorf("list last key: got %s, want %s", keys[4], want)
			}
			if keys, err := store.List(ctx, tenant+"/documents/"); err != nil || len(keys) != 2 {
				t.Fatalf("%s: list documents: %v %v", name, keys, err)
			}
			if keys, err := store.List(ctx, tenant); err != nil || len(keys) != 6 { // a key prefix, not a directory
				t.Fatalf("%s: list by key prefix: %v %v", name, keys, err)
			}
			if keys, err := store.List(ctx, "tenants/none/"); err != nil || len(keys) != 0 {
				t.Fatalf("%s: list nothing: %v %v", name, keys, err)
			}
			if _, err := store.List(ctx, "../"); err == nil {
				t.Fatalf("%s: listed a bad prefix", name)
			}
			err = store.Delete(ctx, tenant+"/audit/anchors/000000000001.json")
			if !errors.Is(err, blobstore.ErrLocked) {
				t.Fatalf("%s: deleted a locked object: %v", name, err)
			}
			// From S3, the store's refusal stays in the chain behind ErrLocked.
			var both interface{ Unwrap() []error }
			if name == "s3" && (!errors.As(err, &both) || len(both.Unwrap()) != 2) {
				t.Errorf("%s: got %v, want ErrLocked wrapping the store's refusal", name, err)
			}
			for _, k := range []string{"/documents/a", "/documents/b", "/bank-files/f.xml", "/uploads/u1", "/uploads/u1"} { // the last one twice: nothing to do
				if err := store.Delete(ctx, tenant+k); err != nil {
					t.Fatalf("%s: delete %s: %v", name, k, err)
				}
			}
			if err := store.Delete(ctx, "../x"); err == nil {
				t.Fatalf("%s: deleted a bad key", name)
			}
			keys, _ = store.List(ctx, tenant+"/")
			if len(keys) != 1 || !strings.Contains(keys[0], "anchors") {
				t.Fatalf("%s: after deletes: %v", name, keys)
			}
			if _, _, err := st.Get(ctx, tenant+"-other/documents/c"); err != nil {
				t.Fatalf("%s: the other tenant's object went: %v", name, err)
			}
			clock = clock.Add(2 * time.Hour) // the retention ends
			if err := store.Delete(ctx, tenant+"/audit/anchors/000000000001.json"); err != nil {
				t.Fatalf("%s: delete after retention: %v", name, err)
			}
			clock = clock.Add(-2 * time.Hour)
		}) {
			t.FailNow() // a failed store leaves the clock moved on
		}
	}
	// Failures: the service refuses, the transport fails, a listing that is not XML.
	srv.Fail = http.StatusInternalServerError
	_, err := s3.List(ctx, "tenants/")
	pgtest.WantErr(t, err, "LIST")
	if err := s3.Delete(ctx, "tenants/x/documents/a"); err == nil || errors.Is(err, blobstore.ErrLocked) {
		t.Fatalf("delete failure: %v", err)
	}
	srv.Fail = 0
	down := s3
	down.Endpoint = "http://127.0.0.1:1"
	if _, err := down.List(ctx, "tenants/"); err == nil {
		t.Fatal("list over a dead transport")
	}
	down.Endpoint = "::"
	if _, err := down.List(ctx, "tenants/"); err == nil {
		t.Fatal("list with an endpoint that is not a URL")
	}
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>")) }))
	defer garbage.Close()
	odd := s3
	odd.Endpoint = garbage.URL
	_, err = odd.List(ctx, "tenants/")
	pgtest.WantErr(t, err, "LIST")
	if err := down.Delete(ctx, "tenants/x/documents/a"); err == nil {
		t.Fatal("delete over a dead transport")
	}
	// A directory store whose object cannot be removed (a directory in its place), and a
	// directory it cannot walk; one without a clock deletes an object whose retention ended.
	bad := blobstore.Dir{Root: t.TempDir()}
	must(t, os.MkdirAll(filepath.Join(bad.Root, "tenants/x/documents/a/sub"), 0o700))
	if err := bad.Delete(ctx, "tenants/x/documents/a"); err == nil {
		t.Fatal("removed a directory as an object")
	}
	if _, err := (blobstore.Dir{Root: filepath.Join(bad.Root, "tenants/x/documents/a/sub")}).List(ctx, "tenants/"); err != nil {
		t.Fatalf("list in an empty root: %v", err)
	}
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		seal(t, filepath.Join(bad.Root, "tenants/x/documents/a/sub"), 0)
		if _, err := bad.List(ctx, "tenants/"); err == nil {
			t.Fatal("listed through an unreadable directory")
		}
	}
	if err := bad.Put(ctx, "tenants/y/audit/anchors/1", []byte("x"), blobstore.PutOptions{RetainUntil: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := bad.Delete(ctx, "tenants/y/audit/anchors/1"); err != nil {
		t.Fatalf("delete after retention without a clock: %v", err)
	}
}

// provider yields fixed credentials, or fails.
type provider struct {
	c   awscreds.Credentials
	err error
}

func (p provider) Credentials(context.Context) (awscreds.Credentials, error) { return p.c, p.err }

// TestAmbientCredentials: an S3 store signing with a credentials provider,
// session token included, and one whose provider has nothing to give.
func TestAmbientCredentials(t *testing.T) {
	ctx := context.Background()
	srv := blobstoretest.New(t)
	s3 := srv.Store()
	s3.AccessKey, s3.SecretKey = "", ""
	s3.Credentials = provider{c: awscreds.Credentials{AccessKey: blobstoretest.AccessKey, SecretKey: blobstoretest.SecretKey, SessionToken: "session"}}
	if err := s3.Put(ctx, "tenants/x/documents/a", []byte("x"), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s3.Get(ctx, "tenants/x/documents/a"); err != nil {
		t.Fatal(err)
	}
	u, err := s3.PresignPut(ctx, "tenants/x/uploads/u", "application/pdf", time.Minute)
	if err != nil || !strings.Contains(u, "X-Amz-Security-Token=session") {
		t.Fatalf("%s %v", u, err)
	}
	s3.Credentials = provider{err: awscreds.ErrNone}
	for name, call := range map[string]func() error{
		"put":    func() error { return s3.Put(ctx, "tenants/x/documents/b", []byte("x"), blobstore.PutOptions{}) },
		"get":    func() error { _, _, err := s3.Get(ctx, "tenants/x/documents/a"); return err },
		"stat":   func() error { _, _, err := s3.Stat(ctx, "tenants/x/documents/a"); return err },
		"list":   func() error { _, err := s3.List(ctx, "tenants/x/"); return err },
		"delete": func() error { return s3.Delete(ctx, "tenants/x/documents/a") },
		"presign": func() error {
			_, err := s3.PresignPut(ctx, "tenants/x/uploads/u", "application/pdf", time.Minute)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			pgtest.CheckErr(t, call(), "credentials")
		})
	}
}

// TestDirDeleteFailsClosed: a sidecar that cannot be read or decoded may
// hold a retention, so Delete refuses with its error and keeps the object
// rather than take it for unlocked. Get and Stat report it too.
func TestDirDeleteFailsClosed(t *testing.T) {
	ctx := context.Background()
	for name, spoil := range map[string]func(sidecar string) error{
		"a directory in its place": func(sidecar string) error {
			return errors.Join(os.Remove(sidecar), os.Mkdir(sidecar, 0o700))
		},
		"an undecodable retention": func(sidecar string) error {
			return os.WriteFile(sidecar, []byte("application/json\nnext year\n"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := blobstore.Dir{Root: t.TempDir()}
			const key = "tenants/x/audit/anchors/1"
			if err := d.Put(ctx, key, []byte("{}"), blobstore.PutOptions{RetainUntil: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			must(t, spoil(filepath.Join(d.Root, filepath.FromSlash(key))+".meta"))
			if err := d.Delete(ctx, key); err == nil || errors.Is(err, blobstore.ErrLocked) {
				t.Errorf("delete: got %v, want the sidecar's error", err)
			}
			if _, err := os.Stat(filepath.Join(d.Root, filepath.FromSlash(key))); err != nil {
				t.Errorf("object after a refused delete: got %v, want it kept", err)
			}
			if _, _, err := d.Get(ctx, key); err == nil {
				t.Error("get: got nil, want the sidecar's error")
			}
			if _, _, err := d.Stat(ctx, key); err == nil {
				t.Error("stat: got nil, want the sidecar's error")
			}
		})
	}
}

// TestPutStream: both stores write an object from a reader, without its
// bytes in memory (the API-hosted upload, ADR-0020 As built 2026-11-01),
// keep it write-once, and S3 refuses bytes that do not match the size and
// hash the stream declared.
func TestPutStream(t *testing.T) {
	ctx := context.Background()
	srv := blobstoretest.New(t)
	content := []byte("%PDF-1.7 streamed")
	sum := sha256.Sum256(content)
	for name, store := range map[string]blobstore.Streamer{"dir": blobstore.Dir{Root: t.TempDir()}, "s3": srv.Store()} {
		t.Run(name, func(t *testing.T) {
			const key = "tenants/x/uploads/s1"
			// A reader that is no bytes.Reader: the request cannot measure it itself.
			if err := store.PutStream(ctx, key, io.MultiReader(bytes.NewReader(content)), int64(len(content)), sum, blobstore.PutOptions{ContentType: "application/pdf"}); err != nil {
				t.Fatalf("put stream: %v", err)
			}
			got, ct, err := store.(blobstore.Store).Get(ctx, key)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if string(got) != string(content) {
				t.Errorf("content: got %q, want %q", got, content)
			}
			if ct != "application/pdf" {
				t.Errorf("content type: got %q, want application/pdf", ct)
			}
			if err := store.PutStream(ctx, key, bytes.NewReader(content), int64(len(content)), sum, blobstore.PutOptions{}); !errors.Is(err, blobstore.ErrExists) {
				t.Errorf("second put: got %v, want %v", err, blobstore.ErrExists)
			}
		})
	}
	other := sha256.Sum256([]byte("other"))
	err := srv.Store().PutStream(ctx, "tenants/x/uploads/s2", bytes.NewReader(content), int64(len(content)), other, blobstore.PutOptions{})
	pgtest.WantErr(t, err, "BadDigest")
	if _, ok := srv.Object("tenants/x/uploads/s2"); ok {
		t.Error("bytes that do not match their hash were stored")
	}
}
