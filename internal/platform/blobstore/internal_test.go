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
	link = os.Link
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

// TestListSkipsTemporaryFiles: a temporary file a failed remove left behind
// is not an object.
func TestListSkipsTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	d := Dir{Root: root}
	ctx := context.Background()
	if err := d.Put(ctx, "tenants/x/a", []byte("1"), PutOptions{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "tenants", "x", "b"+tempMark+"123"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("leftover: %v", err)
	}
	keys, err := d.List(ctx, "tenants/x/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Join(keys, ",") != "tenants/x/a" {
		t.Errorf("got %v, want [tenants/x/a]", keys)
	}
}
