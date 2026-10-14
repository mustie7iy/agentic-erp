package blobstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDirWriteFailures fails the writes a Put makes after the temporary file
// was created: the content (a read-only descriptor), the link and the
// sidecar.
func TestDirWriteFailures(t *testing.T) {
	root := t.TempDir()
	d := Dir{Root: root}
	readOnly := filepath.Join(root, "ro")
	if err := os.WriteFile(readOnly, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	createTemp = func(string, string) (*os.File, error) { return os.Open(readOnly) }
	defer func() { createTemp = os.CreateTemp }()
	if err := d.Put(context.Background(), "tenants/x/a", []byte("1"), PutOptions{}); err == nil || errors.Is(err, ErrExists) {
		t.Fatalf("write on a read-only descriptor: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tenants", "x", "a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed write left the object behind")
	}
	createTemp = os.CreateTemp
	link = func(string, string) error { return errors.New("no links here") }
	if err := d.Put(context.Background(), "tenants/x/c", []byte("1"), PutOptions{}); err == nil || !strings.Contains(err.Error(), "no links here") {
		t.Fatalf("got link error %v, want the link's", err)
	}
	if left := temporaries(t, filepath.Join(root, "tenants", "x")); len(left) != 0 {
		t.Errorf("got %v left after a failed link, want nothing", left)
	}
	link = os.Link
	rename = func(string, string) error { return errors.New("disk full") }
	defer func() { rename = os.Rename }()
	if err := d.Put(context.Background(), "tenants/x/b", []byte("1"), PutOptions{}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("sidecar: %v", err)
	}
	rename = os.Rename
	if left := temporaries(t, filepath.Join(root, "tenants", "x")); len(left) != 0 {
		t.Errorf("got %v left after a failed sidecar, want nothing", left)
	}
}

// TestDirSidecarWriteFailures fails the sidecar's own temporary file, after
// a Put and on the retry that repairs it: both report the failure rather
// than leave or accept an object without its sidecar.
func TestDirSidecarWriteFailures(t *testing.T) {
	ctx := context.Background()
	d := Dir{Root: t.TempDir()}
	const key = "tenants/x/a"
	calls := 0
	createTemp = func(dir, pattern string) (*os.File, error) {
		if calls++; calls > 1 {
			return nil, errors.New("no temporary sidecar")
		}
		return os.CreateTemp(dir, pattern)
	}
	t.Cleanup(func() { createTemp = os.CreateTemp })
	if err := d.Put(ctx, key, []byte("1"), PutOptions{}); err == nil || !strings.Contains(err.Error(), "no temporary sidecar") {
		t.Fatalf("got %v from a Put whose sidecar could not be created, want its error", err)
	}
	calls = 0
	if err := d.Put(ctx, key, []byte("1"), PutOptions{}); err == nil || errors.Is(err, ErrExists) {
		t.Fatalf("got %v from a retry that could not repair the sidecar, want its error", err)
	}
	createTemp = os.CreateTemp
	if err := d.Put(ctx, key, []byte("1"), PutOptions{ContentType: "text/plain"}); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v from the repairing retry, want ErrExists", err)
	}
	if _, ct, err := d.Stat(ctx, key); err != nil || ct != "text/plain" {
		t.Errorf("got content type %q, %v after the repair, want text/plain", ct, err)
	}
	if err := d.Put(ctx, key, []byte("1"), PutOptions{ContentType: "image/png"}); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v from a Put of a whole object, want ErrExists", err)
	}
	if _, ct, _ := d.Stat(ctx, key); ct != "text/plain" {
		t.Errorf("got content type %q after a later Put, want the stored text/plain", ct)
	}
	if left := temporaries(t, filepath.Join(d.Root, "tenants", "x")); len(left) != 0 {
		t.Errorf("got %v left after the sidecar writes, want nothing", left)
	}
}

// TestDirPutReplacesStaleSidecar: a sidecar whose content a Delete removed
// is replaced by the next Put of the key, not taken for the new object's.
func TestDirPutReplacesStaleSidecar(t *testing.T) {
	ctx := context.Background()
	d := Dir{Root: t.TempDir()}
	const key = "tenants/x/a"
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+".meta", []byte("text/stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(ctx, key, []byte("1"), PutOptions{ContentType: "text/plain"}); err != nil {
		t.Fatalf("put over a stale sidecar: %v", err)
	}
	if _, ct, err := d.Stat(ctx, key); err != nil || ct != "text/plain" {
		t.Errorf("got content type %q, %v, want text/plain", ct, err)
	}
}

func TestEncoding(t *testing.T) {
	if got := encodePath("tenants/a b/é~x"); got != "tenants/a%20b/%C3%A9~x" {
		t.Fatalf("encodePath: %s", got)
	}
}

// TestNow checks the injectable clock.
func TestNow(t *testing.T) {
	s := S3{Now: func() time.Time { return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC) }}
	if !s.now().Equal(time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)) {
		t.Fatal("Now not used")
	}
}

// TestDirPutIsWholeOrNothing: while a Put writes the content, and after it
// fails, the key holds nothing, so a crash or a failed cleanup mid-write
// never leaves a partial object that a later Put would take for the stored
// one (payables treats ErrExists as "already stored").
func TestDirPutIsWholeOrNothing(t *testing.T) {
	ctx := context.Background()
	d := Dir{Root: t.TempDir()}
	const key = "tenants/x/documents/a"
	var during error
	writeContent = func(f *os.File, b []byte) (int, error) {
		n, err := f.Write(b[:len(b)/2])
		if err != nil {
			t.Fatalf("write half: %v", err)
		}
		_, _, during = d.Stat(ctx, key)
		return n, errors.New("crash")
	}
	t.Cleanup(func() { writeContent = (*os.File).Write })
	if err := d.Put(ctx, key, []byte("%PDF-1.7 whole"), PutOptions{}); err == nil || !strings.Contains(err.Error(), "crash") {
		t.Fatalf("got Put error %v, want the write's", err)
	}
	if left := temporaries(t, filepath.Join(d.Root, "tenants", "x", "documents")); len(left) != 0 {
		t.Errorf("got %v left after a failed write, want nothing", left)
	}
	if !errors.Is(during, ErrNotFound) {
		t.Errorf("got Stat %v while the content was written, want ErrNotFound", during)
	}
	writeContent = (*os.File).Write
	if err := d.Put(ctx, key, []byte("%PDF-1.7 whole"), PutOptions{}); err != nil {
		t.Fatalf("got Put %v after a failed one, want the key free", err)
	}
	if got, _, err := d.Get(ctx, key); err != nil || string(got) != "%PDF-1.7 whole" {
		t.Errorf("got %q, %v, want the whole content", got, err)
	}
}

// temporaries lists the temporary files left in dir.
func temporaries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var left []string
	for _, e := range entries {
		if strings.Contains(e.Name(), tempMark) {
			left = append(left, e.Name())
		}
	}
	return left
}

// TestErasureRemovesTemporaryFiles: what failed Puts left behind is part of
// a tenant's content, so List reports it under its key and Delete removes
// it, whether or not the key holds an object (ADR-0021).
func TestErasureRemovesTemporaryFiles(t *testing.T) {
	for name, root := range map[string]string{
		"plain root":         t.TempDir(),
		"root with the mark": filepath.Join(t.TempDir(), "erp"+tempMark+"1"),
	} {
		t.Run(name, func(t *testing.T) { eraseLeftovers(t, root) })
	}
}

func eraseLeftovers(t *testing.T, root string) {
	t.Helper()
	d := Dir{Root: root}
	ctx := context.Background()
	if err := d.Put(ctx, "tenants/x/a", []byte("1"), PutOptions{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	dir := filepath.Join(root, "tenants", "x")
	for _, name := range []string{"a" + tempMark + "1", "b" + tempMark + "1", "b" + tempMark + "2"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("partial"), 0o600); err != nil {
			t.Fatalf("leftover %s: %v", name, err)
		}
	}
	keys, err := d.List(ctx, "tenants/x/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := strings.Join(keys, ","); got != "tenants/x/a,tenants/x/b" {
		t.Fatalf("got keys %s, want tenants/x/a,tenants/x/b", got)
	}
	for _, k := range keys {
		if err := d.Delete(ctx, k); err != nil {
			t.Fatalf("delete %s: %v", k, err)
		}
	}
	if left := temporaries(t, dir); len(left) != 0 {
		t.Errorf("got %v left after the erasure, want nothing", left)
	}
	locked := "tenants/x/locked"
	if err := d.Put(ctx, locked, []byte("1"), PutOptions{RetainUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("put locked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "locked"+tempMark+"1"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("leftover of locked: %v", err)
	}
	if err := d.Delete(ctx, locked); !errors.Is(err, ErrLocked) {
		t.Fatalf("got %v deleting a locked key, want ErrLocked", err)
	}
	if left := temporaries(t, dir); len(left) != 0 {
		t.Errorf("got %v left beside a locked key, want nothing", left)
	}
	if err := d.Delete(ctx, "tenants/gone/a"); err != nil {
		t.Errorf("got %v deleting below a missing directory, want nil", err)
	}
}

// TestDeleteReportsTemporaryFilesItCannotRemove: an erasure that cannot
// remove what a failed Put left behind says so rather than leave it.
func TestDeleteReportsTemporaryFilesItCannotRemove(t *testing.T) {
	root := t.TempDir()
	d := Dir{Root: root}
	ctx := context.Background()
	dir := filepath.Join(root, "tenants", "x")
	stuck := filepath.Join(dir, "a"+tempMark+"1")
	if err := os.MkdirAll(stuck, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "f"), nil, 0o600); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if err := d.Delete(ctx, "tenants/x/a"); err == nil {
		t.Error("got nil from removing a non-empty leftover, want its error")
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatalf("seal: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	if err := d.Delete(ctx, "tenants/x/b"); err == nil {
		t.Error("got nil from an unreadable directory, want its error")
	}
}

// TestDirRetriedPutRecordsRetention: a Put whose sidecar failed is retried,
// and callers take the retry's ErrExists for "already stored" (an audit
// anchor does). The retention the retry carries must then hold, or an
// erasure deletes an anchor that must stay locked (ADR-0011).
func TestDirRetriedPutRecordsRetention(t *testing.T) {
	ctx := context.Background()
	d := Dir{Root: t.TempDir()}
	const key = "tenants/x/audit/anchors/1"
	opts := PutOptions{ContentType: "application/json", RetainUntil: time.Now().Add(time.Hour)}
	rename = func(string, string) error { return errors.New("disk full") }
	t.Cleanup(func() { rename = os.Rename })
	if err := d.Put(ctx, key, []byte("{}"), opts); err == nil {
		t.Fatal("got nil from a Put whose sidecar failed, want its error")
	}
	rename = os.Rename
	if err := d.Put(ctx, key, []byte("{}"), opts); err != nil && !errors.Is(err, ErrExists) {
		t.Fatalf("got %v retrying the Put, want nil or ErrExists", err)
	}
	if _, ct, err := d.Stat(ctx, key); err != nil || ct != "application/json" {
		t.Errorf("got content type %q, %v after the retry, want application/json", ct, err)
	}
	if err := d.Delete(ctx, key); !errors.Is(err, ErrLocked) {
		t.Errorf("got %v deleting the retried object, want ErrLocked", err)
	}
}
