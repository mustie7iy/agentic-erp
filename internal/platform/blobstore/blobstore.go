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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
)

// Errors a store reports.
var (
	// ErrExists is returned by Put when the key already holds an object.
	ErrExists = errors.New("blobstore: object exists")
	// ErrNotFound is returned by Get when the key holds nothing.
	ErrNotFound = errors.New("blobstore: object not found")
	// ErrLocked is returned by Delete while the object's retention holds.
	ErrLocked = errors.New("blobstore: object is locked")
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

// Eraser is a store that can list and delete a tenant's objects, for the
// one deletion the platform makes: the erasure of an offboarded tenant
// (ADR-0021). Locked objects (anchors, exports) stay: Delete returns
// ErrLocked for them until their retention ends.
type Eraser interface {
	// List returns the keys below prefix (a key prefix ending in "/" lists a
	// directory; without it, keys starting with the prefix).
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes the object; nothing to do when the key holds nothing.
	Delete(ctx context.Context, key string) error
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
	KindExports   = "exports" // the export of an offboarded tenant, one directory per export, locked
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
// sidecar file. Writes are exclusive links of a whole temporary file, so a
// key is written once and never holds part of an object;
// RetainUntil is recorded but not enforced (the filesystem cannot).
type Dir struct {
	Root string
	Now  func() time.Time // for retention checks on Delete; nil: time.Now
}

func (d Dir) now() time.Time {
	return clock.Now(d.Now)
}

func (d Dir) path(key string) string { return filepath.Join(d.Root, filepath.FromSlash(key)) }

// createTemp, writeContent, link and rename are the filesystem calls Put
// makes; tests replace them to fail a write, so this package's tests do
// not run in parallel.
var (
	createTemp   = os.CreateTemp
	writeContent = (*os.File).Write
	link         = os.Link
	rename       = os.Rename
)

// tempMark separates a key from the random suffix of the temporary file its
// content or sidecar is written to. Keys cannot contain it (keyPattern), so
// a temporary file never collides with a key; List reports it under its key
// and Delete removes it with the key.
const tempMark = "#"

// Put implements Store. The content is written to a temporary file and
// linked under the key only once it is whole and synced: a failed write, a
// failed cleanup or a crash leaves the key free, never holding part of an
// object that a later Put would answer ErrExists for. A link, unlike a
// rename, fails when the key is taken, which keeps objects write-once.
//
// The sidecar (content type and retention) follows the content, so a Put
// that fails after the link leaves content without one, and its retry is
// answered ErrExists, which callers take for "already stored" (an audit
// anchor does). The retry therefore writes a missing sidecar from its own
// options before answering (repairSidecar): once a Put of the key has
// returned nil or ErrExists, the sidecar is there and an anchor's retention
// holds (ADR-0011). A sidecar is written to a temporary file and moved into
// place whole, so it is either missing or complete, never a content type
// without the retention that follows it.
func (d Dir) Put(_ context.Context, key string, content []byte, opts PutOptions) error {
	if err := checkKey(key); err != nil {
		return err
	}
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("blobstore: %w", err)
	}
	tmp, err := writeTemp(p, content)
	// Once linked, or after a failed write, the temporary name is not needed.
	// A failed remove leaves a file the key's Delete removes; the key stays free.
	defer func() { _ = os.Remove(tmp) }()
	if err != nil {
		return fmt.Errorf("blobstore: %w", err)
	}
	if err := link(tmp, p); errors.Is(err, os.ErrExist) {
		return repairSidecar(p, opts)
	} else if err != nil {
		return fmt.Errorf("blobstore: %w", err)
	}
	// This Put created the content, so a sidecar already there is stale (one
	// a Delete could not remove) and is replaced.
	if err := writeSidecar(p, opts, rename); err != nil {
		return fmt.Errorf("blobstore: %w", err)
	}
	return nil
}

// repairSidecar answers a Put whose key is taken: ErrExists once the
// object's sidecar is there, writing it from opts when an earlier Put failed
// before writing it. A link never replaces a sidecar that exists, so the
// options of the Put that stored the object win over a later one's.
func repairSidecar(p string, opts PutOptions) error {
	if err := writeSidecar(p, opts, link); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("blobstore: %w", err)
	}
	return ErrExists
}

// writeSidecar writes the sidecar of the object at p to a temporary file and
// moves it into place with place: rename to replace a sidecar, link to keep
// one that exists.
func writeSidecar(p string, opts PutOptions, place func(string, string) error) error {
	meta := contentType(opts) + "\n"
	if !opts.RetainUntil.IsZero() {
		meta += opts.RetainUntil.UTC().Format(time.RFC3339) + "\n"
	}
	tmp, err := writeTemp(p, []byte(meta))
	// Placed or not, the temporary name is not needed; see Put.
	defer func() { _ = os.Remove(tmp) }()
	if err != nil {
		return err
	}
	return place(tmp, p+".meta")
}

// writeTemp writes content to a new temporary file next to p, synced so the
// object a link publishes survives a crash whole, and returns its name ("" when
// none was created).
func writeTemp(p string, content []byte) (string, error) {
	f, err := createTemp(filepath.Dir(p), filepath.Base(p)+tempMark+"*")
	if err != nil {
		return "", err
	}
	_, werr := writeContent(f, content)
	return f.Name(), errors.Join(werr, f.Sync(), f.Close())
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
	ct, _ := d.meta(key)
	return ct
}

// meta reads the sidecar: content type and retention (zero when unlocked).
func (d Dir) meta(key string) (string, time.Time) {
	ct, until := "application/octet-stream", time.Time{}
	if meta, err := os.ReadFile(d.path(key) + ".meta"); err == nil {
		lines := strings.Split(string(meta), "\n")
		if lines[0] != "" {
			ct = lines[0]
		}
		if len(lines) > 1 {
			until, _ = time.Parse(time.RFC3339, lines[1]) // written by Put; anything else is unlocked
		}
	}
	return ct, until
}

// List implements Eraser.
func (d Dir) List(_ context.Context, prefix string) ([]string, error) {
	if err := checkKey(strings.TrimSuffix(prefix, "/")); err != nil {
		return nil, err
	}
	base := d.path(strings.TrimSuffix(prefix, "/"))
	if !strings.HasSuffix(prefix, "/") {
		base = filepath.Dir(base) // keys starting with the prefix live in its directory and below
	}
	var keys []string
	err := filepath.WalkDir(base, func(p string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || strings.HasSuffix(p, ".meta") {
			return err
		}
		rel, _ := filepath.Rel(d.Root, p) // p is below Root
		// A temporary file a failed Put left behind is listed under the key it
		// was for, so an erasure deletes it with the key (ADR-0021). Only the
		// part below Root is cut: Root itself may contain the mark.
		rel, _, _ = strings.Cut(rel, tempMark)
		if key := filepath.ToSlash(rel); strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("blobstore: %w", err)
	}
	sort.Strings(keys)
	return slices.Compact(keys), nil
}

// Delete implements Eraser: the retention a Put recorded is honoured.
func (d Dir) Delete(_ context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	// Leftovers of failed Puts are never locked, even under a locked key.
	if err := d.removeTemporary(key); err != nil {
		return err
	}
	if _, until := d.meta(key); !until.IsZero() && d.now().Before(until) {
		return ErrLocked
	}
	if err := os.Remove(d.path(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blobstore: %w", err)
	}
	// The content is gone, which is what Delete promises. A sidecar left
	// behind holds no content, is skipped by List and is rewritten by the
	// next Put of the key.
	_ = os.Remove(d.path(key) + ".meta")
	return nil
}

// removeTemporary removes what failed Puts of key left behind: part of its
// content, which an erasure must not leave.
func (d Dir) removeTemporary(key string) error {
	p := d.path(key)
	entries, err := os.ReadDir(filepath.Dir(p))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("blobstore: %w", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(p)+tempMark) {
			if err := os.Remove(filepath.Join(filepath.Dir(p), e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("blobstore: %w", err)
			}
		}
	}
	return nil
}

func contentType(opts PutOptions) string {
	if opts.ContentType == "" {
		return "application/octet-stream"
	}
	return opts.ContentType
}
