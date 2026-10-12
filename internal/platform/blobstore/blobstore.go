// Package blobstore stores the platform's files (02-architecture: captured
// documents, bank files, audit anchors) in object storage. Objects are
// written once: a key is never overwritten, so a stored document, file or
// anchor cannot be replaced unnoticed, and anchors can carry a retention
// lock the store enforces (ADR-0011). Keys are prefixed by tenant, so one
// tenant's objects can be listed, exported or erased together.
//
// Two stores exist: Dir, a directory on the local filesystem for
// development and tests, and S3, any S3-compatible service (AWS S3, MinIO,
// Ceph) over HTTP with Signature Version 4.
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors a store reports.
var (
	// ErrExists is returned by Put when the key already holds an object.
	ErrExists = errors.New("blobstore: object exists")
	// ErrNotFound is returned by Get when the key holds nothing.
	ErrNotFound = errors.New("blobstore: object not found")
)

// PutOptions describe an object being written.
type PutOptions struct {
	ContentType string    // default application/octet-stream
	RetainUntil time.Time // when set, the store must refuse to delete or change the object before then (S3 Object Lock, compliance mode)
}

// Store is write-once object storage.
type Store interface {
	// Put writes the object under key; ErrExists when the key is taken.
	Put(ctx context.Context, key string, content []byte, opts PutOptions) error
	// Get reads the object and its content type; ErrNotFound when the key
	// holds nothing.
	Get(ctx context.Context, key string) ([]byte, string, error)
	// Stat reports the object's size and content type without reading it;
	// ErrNotFound when the key holds nothing.
	Stat(ctx context.Context, key string) (int64, string, error)
}

// Presigner is a store that can hand out URLs clients write to directly
// (04-api-design §4.11: documents above the request limit). S3 is one; the
// directory store is not, and the API receives those uploads itself.
type Presigner interface {
	// PresignPut returns a URL a client may PUT the object to within
	// expires, sending exactly the content type given.
	PresignPut(ctx context.Context, key, contentType string, expires time.Duration) (string, error)
}

// Kinds of objects, the second key segment after the tenant.
const (
	KindDocuments = "documents"  // captured supplier invoice documents, named by sha256
	KindBankFiles = "bank-files" // pain.001 files, named by bank file id
	KindUploads   = "uploads"    // documents clients upload directly, named by upload id; copied under their hash when used
	KindAnchors   = "audit/anchors"
)

// Prefix is the first segment of every key.
const Prefix = "tenants"

// Key names an object of a tenant: tenants/<tenant id>/<kind>/<name>.
func Key(tenant uuid.UUID, kind, name string) string {
	return Prefix + "/" + tenant.String() + "/" + kind + "/" + name
}

// IsKey reports whether a reference is an object key of this package
// (rather than a bare content hash recorded before object storage existed).
func IsKey(ref string) bool { return strings.HasPrefix(ref, Prefix+"/") }

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_.~/-]+$`)

// checkKey refuses keys that could escape a directory or confuse a URL.
func checkKey(key string) error {
	if key == "" || len(key) > 1024 || !keyPattern.MatchString(key) || strings.Contains(key, "..") || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") ||
		strings.Contains(key, "//") {
		return fmt.Errorf("blobstore: invalid key %q", key)
	}
	return nil
}

// Dir stores objects as files below Root, with the content type in a
// sidecar file. Writes are exclusive creates, so a key is written once;
// RetainUntil is recorded but not enforced (the filesystem cannot).
type Dir struct {
	Root string
}

func (d Dir) path(key string) string { return filepath.Join(d.Root, filepath.FromSlash(key)) }

// openFile and writeFile are the filesystem calls Put makes; tests replace
// them to fail a write.
var (
	openFile  = os.OpenFile
	writeFile = os.WriteFile
)

// Put implements Store.
func (d Dir) Put(_ context.Context, key string, content []byte, opts PutOptions) error {
	if err := checkKey(key); err != nil {
		return err
	}
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("blobstore: %w", err)
	}
	f, err := openFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return ErrExists
	}
	if err != nil {
		return fmt.Errorf("blobstore: %w", err)
	}
	_, werr := f.Write(content)
	if err := errors.Join(werr, f.Close()); err != nil {
		os.Remove(p)
		return fmt.Errorf("blobstore: %w", err)
	}
	meta := contentType(opts) + "\n"
	if !opts.RetainUntil.IsZero() {
		meta += opts.RetainUntil.UTC().Format(time.RFC3339) + "\n"
	}
	// The sidecar follows the content: a reader that finds content without
	// it falls back to the default type.
	if err := writeFile(p+".meta", []byte(meta), 0o600); err != nil {
		return fmt.Errorf("blobstore: %w", err)
	}
	return nil
}

// Get implements Store.
func (d Dir) Get(_ context.Context, key string) ([]byte, string, error) {
	if err := checkKey(key); err != nil {
		return nil, "", err
	}
	content, err := os.ReadFile(d.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("blobstore: %w", err)
	}
	return content, d.contentTypeOf(key), nil
}

// Stat implements Store.
func (d Dir) Stat(_ context.Context, key string) (int64, string, error) {
	if err := checkKey(key); err != nil {
		return 0, "", err
	}
	info, err := os.Stat(d.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return 0, "", ErrNotFound
	}
	if err != nil {
		return 0, "", fmt.Errorf("blobstore: %w", err)
	}
	return info.Size(), d.contentTypeOf(key), nil
}

// contentTypeOf reads the sidecar; content without one is binary.
func (d Dir) contentTypeOf(key string) string {
	if meta, err := os.ReadFile(d.path(key) + ".meta"); err == nil {
		if line, _, _ := strings.Cut(string(meta), "\n"); line != "" {
			return line
		}
	}
	return "application/octet-stream"
}

func contentType(opts PutOptions) string {
	if opts.ContentType == "" {
		return "application/octet-stream"
	}
	return opts.ContentType
}
