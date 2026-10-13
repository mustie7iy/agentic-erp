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

// TestDirWriteFailures fails the writes a Put makes after the exclusive
// create succeeded: the content (a read-only descriptor) and the sidecar.
func TestDirWriteFailures(t *testing.T) {
	root := t.TempDir()
	d := Dir{Root: root}
	readOnly := filepath.Join(root, "ro")
	if err := os.WriteFile(readOnly, nil, 0o600); err != nil {
		t.Fatal(err)
	}
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
}

// TestNow checks the injectable clock.
func TestNow(t *testing.T) {
	s := S3{Now: func() time.Time { return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC) }}
	if !s.now().Equal(time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)) {
		t.Fatal("Now not used")
	}
}
