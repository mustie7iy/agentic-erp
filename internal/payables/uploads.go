package payables

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Direct uploads (T16, 04-api-design §4.11, ADR-0020): a document too
// large for a request body is uploaded to object storage through a URL the
// API hands out, then consumed by id. With an S3 store the URL is a
// presigned PUT on the store, unless the client asks for the API's own
// path (via api, PO 2026-11-01: erp-web never reaches the bucket); with a
// directory store (development) it is always the API's path. The API's
// path is signed, expires with the upload, takes one document of at most
// the upload's size, of its content type, for the tenant it was signed
// for, and streams the bytes into the store through a temporary file, never
// holding them in memory. Either way the bytes land at
// tenants/<tenant>/uploads/<id>, and a capture copies them under their hash
// like a document sent in the request.

// MaxUpload is the largest document a direct upload may carry.
const MaxUpload = 50 << 20

// UploadTTL is how long an upload URL accepts bytes.
const UploadTTL = 15 * time.Minute

// UploadPurposes are what uploads may be for, and the operation that consumes each.
var UploadPurposes = map[string]string{"supplier_invoice": "POST /v1/supplier-invoices:capture"}

// Where an upload's bytes go (payables.upload.via): the store's presigned
// URL, or the API's own signed path.
const (
	ViaStore = "store"
	ViaAPI   = "api"
)

// MaxReceiving is how many documents a replica receives on the API's path
// at once unless Uploads.MaxReceiving says otherwise: each is spooled to a
// temporary file of up to MaxUpload, and erp-server's /tmp holds 256 MiB
// (deploy/helm/erp-cell server.yaml), so four fit beside whatever else is
// there. One more is answered 503 with Retry-After.
const MaxReceiving = 4

// UploadTypes are the content types a document may be uploaded as.
var UploadTypes = map[string]bool{"application/pdf": true, "image/png": true, "image/jpeg": true, "application/xml": true, "text/xml": true}

// Upload is a row of payables.upload.
type Upload struct {
	ID          uuid.UUID
	Purpose     string
	ContentType string
	MaxBytes    int64
	ObjectRef   string
	Via         string // store or api: where the URL sends the bytes as created
	Status      string // open, received, used
	Size        *int64
	SHA256      []byte
	UsedBy      *uuid.UUID
	ExpiresAt   time.Time
	CreatedAt   time.Time
	CreatedBy   uuid.UUID
	UpdatedAt   time.Time
	Version     int32
}

// Uploads issues upload URLs and receives the uploads the API hosts.
type Uploads struct {
	Objects   blobstore.Store // where uploads land; nil: uploads answer 503
	Secret    []byte          // signs the API's own upload URLs; the same on every replica
	PublicURL string          // the API's public base URL (ERP_PUBLIC_URL); set, the API's own upload URLs are absolute
	// Pool and AppRole serve the API-hosted PUT, which runs outside the unit
	// of work (no bearer token, a body above its limit).
	Pool    *database.Pool
	AppRole string
	Now     func() time.Time // nil: time.Now
	// InTx runs the PUT's transactions; nil: Pool.InTx. Tests inject failures.
	InTx func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error
	// SpoolDir holds the documents being received on the API's path until
	// they are measured and hashed ("": os.TempDir()); MaxReceiving bounds
	// how many at once (0: the package's MaxReceiving).
	SpoolDir     string
	MaxReceiving int

	slotsOnce sync.Once
	slots     chan struct{}
}

func (u *Uploads) inTx(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
	if u.InTx != nil {
		return u.InTx(ctx, scope, fn)
	}
	return u.Pool.InTx(ctx, scope, fn)
}

func (u *Uploads) now() time.Time {
	return clock.Now(u.Now)
}

const uploadColumns = `id, purpose, content_type, max_bytes, object_ref, via, status, size, sha256, used_by, expires_at, created_at, created_by, updated_at, version`

func scanUpload(row pgx.Row) (Upload, error) {
	var up Upload
	err := row.Scan(&up.ID, &up.Purpose, &up.ContentType, &up.MaxBytes, &up.ObjectRef, &up.Via, &up.Status, &up.Size, &up.SHA256, &up.UsedBy, &up.ExpiresAt,
		&up.CreatedAt, &up.CreatedBy, &up.UpdatedAt, &up.Version)
	return up, errs.Wrapf(err, "scan upload")
}

// readUpload reads an upload; lock takes the row lock for a change.
func readUpload(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Upload, error) {
	q := `SELECT ` + uploadColumns + ` FROM payables.upload WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	up, err := scanUpload(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Upload{}, problem.NotFoundOf("upload")
	}
	if err != nil {
		return Upload{}, fmt.Errorf("payables: read upload %s: %w", id, err)
	}
	return up, nil
}

// Create starts an upload in the request's transaction, its bytes to go
// via the store (ViaStore or "": presigned when the store can, else the
// API's path) or the API (ViaAPI); the row records which URL it was given.
func (u *Uploads) Create(ctx context.Context, purpose, contentType, via string) (Upload, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Upload{}, err
	}
	if _, ok := UploadPurposes[purpose]; !ok {
		return Upload{}, problem.Unprocessable("payables.unknown_upload_purpose", "Uploads are for supplier_invoice documents.")
	}
	if !UploadTypes[contentType] {
		return Upload{}, problem.Unprocessable("payables.unsupported_content_type", "Documents are PDF, PNG, JPEG or XML.")
	}
	if via != "" && via != ViaStore && via != ViaAPI {
		return Upload{}, problem.Unprocessable("payables.unknown_upload_via", "Uploads go via store or api.")
	}
	if u == nil || u.Objects == nil {
		return Upload{}, problem.Unavailable("This server has no object storage; send documents of up to 5 MB in the request.", 0)
	}
	if _, presigns := u.Objects.(blobstore.Presigner); via != ViaAPI && presigns {
		via = ViaStore
	} else {
		via = ViaAPI
	}
	id := uuid.Must(uuid.NewV7())
	ref, expires := blobstore.Key(p.TenantID, blobstore.KindUploads, id.String()), u.now().Add(UploadTTL).UTC()
	created, err := scanUpload(tx.QueryRow(ctx, `INSERT INTO payables.upload (tenant_id, id, purpose, content_type, max_bytes, object_ref, via, status, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'open', $8, $9) RETURNING `+uploadColumns, p.TenantID, id, purpose, contentType, MaxUpload, ref, via, expires, p.ID))
	if err != nil {
		return Upload{}, fmt.Errorf("payables: create upload %s: %w", id, err)
	}
	return created, nil
}

// Get reads an upload in the request's transaction.
func (u *Uploads) Get(ctx context.Context, id uuid.UUID) (Upload, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Upload{}, err
	}
	return readUpload(ctx, tx, id, false)
}

// Target is where an upload's bytes go.
type Target struct {
	URL     string
	Method  string
	Headers map[string]string
	Via     string // ViaStore or ViaAPI
}

// hosted reports whether the upload's bytes come to the API's own path:
// one created via the API, or any when the store cannot presign (a row
// from before the via column defaults to the store, ADR-0020 As built).
func (u *Uploads) hosted(up Upload) bool {
	_, presigns := u.Objects.(blobstore.Presigner)
	return up.Via == ViaAPI || !presigns
}

// Target makes the URL for an upload of the tenant: presigned on the store
// unless the upload is hosted, else the API's own signed path. Via is
// where it sends the bytes.
func (u *Uploads) Target(ctx context.Context, tenant uuid.UUID, up Upload) (Target, error) {
	t := Target{Method: http.MethodPut, Headers: map[string]string{"Content-Type": up.ContentType}, Via: ViaAPI}
	if p, ok := u.Objects.(blobstore.Presigner); ok && !u.hosted(up) {
		t.Via = ViaStore
		var err error
		if t.URL, err = p.PresignPut(ctx, up.ObjectRef, up.ContentType, up.ExpiresAt.Sub(u.now())); err != nil {
			return Target{}, problem.Unavailable("The object store did not issue an upload URL; try again.", 30).WithCause(err)
		}
		return t, nil
	}
	expires := strconv.FormatInt(up.ExpiresAt.Unix(), 10)
	q := url.Values{"tenant": {tenant.String()}, "expires": {expires}, "signature": {u.sign(tenant, up.ID, expires)}}
	t.URL = u.PublicURL + "/v1/uploads/" + up.ID.String() + "/content?" + q.Encode()
	return t, nil
}

func (u *Uploads) sign(tenant, id uuid.UUID, expires string) string {
	m := hmac.New(sha256.New, u.Secret)
	m.Write([]byte("upload\n" + tenant.String() + "\n" + id.String() + "\n" + expires))
	return hex.EncodeToString(m.Sum(nil))
}

// ServeHTTP receives PUT /v1/uploads/{id}/content: the API-hosted upload,
// authenticated by the signature in the URL and bounded by the upload's
// size limit, streamed into the store where the upload says and marked
// received.
func (u *Uploads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := u.receive(w, r); err != nil {
		problem.Write(w, r, problem.From(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// receive checks the URL and the upload before reading a byte, spools the
// body to a temporary file while measuring and hashing it, then stores it
// and marks the upload received under the upload's lock. No transaction is
// open while the client sends: a slow client holds a temporary file and a
// receiving slot, never a database connection, for api.UploadTimeout at
// most (allowTime).
func (u *Uploads) receive(w http.ResponseWriter, r *http.Request) error {
	id, tenant, err := u.checkURL(r)
	if err != nil {
		return err
	}
	if u.Objects == nil {
		return problem.Unavailable("This server has no object storage.", 0)
	}
	streamer, ok := u.Objects.(blobstore.Streamer)
	if !ok {
		return problem.Unavailable("This server's object storage cannot receive uploads.", 0)
	}
	scope := database.Scope{Role: u.AppRole, TenantID: tenant}
	up, err := u.expect(r, scope, id)
	if err != nil {
		return err
	}
	release, err := u.slot()
	if err != nil {
		return err
	}
	defer release()
	allowTime(w, u.now())
	doc, err := u.spool(w, r, up.MaxBytes)
	if err != nil {
		return err
	}
	defer doc.discard()
	return u.inTx(r.Context(), scope, func(ctx context.Context, tx pgx.Tx) error {
		up, err := readUpload(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if up.Status != "open" {
			return uploadClosed()
		}
		if err := u.store(ctx, streamer, up, doc); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payables.upload SET status = 'received', size = $2, sha256 = $3, version = version + 1 WHERE id = $1 AND status = 'open'`,
			id, doc.size, doc.sum[:]); err != nil {
			return fmt.Errorf("payables: mark upload %s received: %w", id, err)
		}
		return nil
	})
}

// uploadClosed refuses a second document: a new problem each time, since
// problem.Write fills in the request's instance and trace id.
func uploadClosed() *problem.Problem {
	return problem.Conflict("payables.upload_closed", "This upload has already received its document.")
}

// expect reads the upload the URL names and refuses, before the body is
// read, what the upload will not take: one already received or used, one
// whose URL is the store's, another content type, or a body that says it
// is too large.
func (u *Uploads) expect(r *http.Request, scope database.Scope, id uuid.UUID) (Upload, error) {
	var up Upload
	err := u.inTx(r.Context(), scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		up, err = readUpload(ctx, tx, id, false)
		return err
	})
	switch {
	case err != nil:
		return Upload{}, err
	case !u.hosted(up):
		// Its bytes go to the store's presigned URL; this path was never given out for it.
		return Upload{}, problem.Forbidden("payables.invalid_upload_url", "The upload URL is not valid.")
	case up.Status != "open":
		return Upload{}, uploadClosed()
	case r.Header.Get("Content-Type") != up.ContentType:
		return Upload{}, problem.Unprocessable("payables.upload_content_type", "Send the document as "+up.ContentType+", the type the upload was created for.")
	case r.ContentLength > up.MaxBytes:
		return Upload{}, tooLarge(up.MaxBytes, nil)
	}
	return up, nil
}

// tooLarge refuses a body above the upload's limit; cause is the reader's
// error when the body passed it, nil when it said so before.
func tooLarge(limit int64, cause error) *problem.Problem {
	return problem.New(http.StatusRequestEntityTooLarge, "payables.document_too_large", "Document too large",
		fmt.Sprintf("Uploads are at most %d MB.", limit>>20)).WithCause(cause)
}

// slot takes one of the replica's receiving slots, or answers 503 while
// all are taken; release gives it back.
func (u *Uploads) slot() (release func(), err error) {
	u.slotsOnce.Do(func() { u.slots = make(chan struct{}, cmp.Or(u.MaxReceiving, MaxReceiving)) })
	select {
	case u.slots <- struct{}{}:
		return func() { <-u.slots }, nil
	default:
		return nil, problem.Unavailable("This server is receiving as many documents as it can; send it again shortly.", 5)
	}
}

// allowTime gives the body api.UploadTimeout to arrive from now, where
// every other request has the listener's 30 seconds (PO 2026-11-02), and
// the answer, written once the document is stored, api.UploadMargin more.
// Only a request about to be received gets it: its URL's signature
// checked, its upload open and of its type, a receiving slot held. A
// refusal before keeps the listener's deadlines, so nobody without a
// signed URL holds a connection longer than any other request does. The
// header's own timeout is the listener's, unchanged.
func allowTime(w http.ResponseWriter, now time.Time) {
	rc := http.NewResponseController(w)
	// A writer without deadlines (a test's recorder) has none to lift; the
	// listener's are lifted through the telemetry middleware (Unwrap).
	_ = rc.SetReadDeadline(now.Add(api.UploadTimeout))
	_ = rc.SetWriteDeadline(now.Add(api.UploadTimeout + api.UploadMargin))
}

// spooled is a document received on the API's path, in a temporary file.
type spooled struct {
	file *os.File
	size int64
	sum  [32]byte
}

// reader reads the document from its start.
func (d spooled) reader() io.Reader { return io.NewSectionReader(d.file, 0, d.size) }

// discard removes the temporary file.
func (d spooled) discard() {
	// Only read from; the file is removed below whatever closing says.
	_ = d.file.Close()
	// A file left behind holds one tenant's document in the replica's
	// private /tmp, an emptyDir that goes with the pod.
	_ = os.Remove(d.file.Name())
}

// spool copies the body to a temporary file, at most limit bytes, hashing
// it on the way: a larger body is refused when it passes the limit, and
// nothing is stored.
func (u *Uploads) spool(w http.ResponseWriter, r *http.Request, limit int64) (spooled, error) {
	f, err := os.CreateTemp(u.SpoolDir, "upload-*")
	if err != nil {
		return spooled{}, problem.Unavailable("This server cannot take documents now; send it again.", 30).WithCause(err)
	}
	doc := spooled{file: f}
	h := sha256.New()
	doc.size, err = io.Copy(io.MultiWriter(f, h), http.MaxBytesReader(w, r.Body, limit))
	var over *http.MaxBytesError
	switch {
	case errors.As(err, &over):
		doc.discard()
		return spooled{}, tooLarge(limit, err)
	case err != nil:
		doc.discard()
		return spooled{}, problem.Unavailable("The document did not arrive whole; send it again.", 0).WithCause(err)
	}
	h.Sum(doc.sum[:0])
	return doc, nil
}

// checkURL verifies the signature and expiry of an upload URL and returns
// the upload and tenant it names.
func (u *Uploads) checkURL(r *http.Request) (id, tenant uuid.UUID, err error) {
	q := r.URL.Query()
	id, err = uuid.Parse(r.PathValue("id"))
	tenant, terr := uuid.Parse(q.Get("tenant"))
	expires, eerr := strconv.ParseInt(q.Get("expires"), 10, 64)
	if err != nil || terr != nil || eerr != nil || !hmac.Equal([]byte(q.Get("signature")), []byte(u.sign(tenant, id, q.Get("expires")))) {
		return uuid.Nil, uuid.Nil, problem.Forbidden("payables.invalid_upload_url", "The upload URL is not valid.")
	}
	if u.now().Unix() > expires {
		return uuid.Nil, uuid.Nil, problem.Forbidden("payables.upload_url_expired", "The upload URL has expired; start a new upload.")
	}
	return id, tenant, nil
}

// store streams an upload's document where the upload says.
func (u *Uploads) store(ctx context.Context, s blobstore.Streamer, up Upload, doc spooled) error {
	err := s.PutStream(ctx, up.ObjectRef, doc.reader(), doc.size, doc.sum, blobstore.PutOptions{ContentType: up.ContentType})
	if errors.Is(err, blobstore.ErrExists) {
		// A PUT that stored the object but did not get to record it (a
		// failed statement, a lost answer) is sent again: the same bytes
		// are fine, others are not, since the store keeps the first.
		stored, _, gerr := u.Objects.Get(ctx, up.ObjectRef)
		if gerr != nil || sha256.Sum256(stored) != doc.sum {
			return problem.Conflict("payables.upload_conflict", "This upload already holds a different document; start a new upload.")
		}
		err = nil
	}
	if err != nil {
		return problem.Unavailable("The object store did not take the document; send it again.", 30).WithCause(err)
	}
	return nil
}

// take reads the document of an upload for the operation consuming it, in
// the request's transaction, locking the upload; mark then closes it. Only
// supplier_invoice uploads exist (UploadPurposes), so an upload always suits
// the one operation that consumes them; a second purpose adds a purpose
// parameter and its check here.
func (u *Uploads) take(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]byte, Upload, error) {
	up, err := readUpload(ctx, tx, id, true)
	if err != nil {
		return nil, Upload{}, err
	}
	switch {
	case up.Status == "used":
		return nil, Upload{}, problem.Unprocessable("payables.upload_used", "This upload has already been captured.")
	case u == nil || u.Objects == nil:
		return nil, Upload{}, problem.Unavailable("This server has no object storage.", 0)
	}
	size, _, err := u.Objects.Stat(ctx, up.ObjectRef)
	if errors.Is(err, blobstore.ErrNotFound) {
		if up.ExpiresAt.Before(u.now()) {
			return nil, Upload{}, problem.Unprocessable("payables.upload_expired", "Nothing was uploaded before the upload expired; start a new one.")
		}
		return nil, Upload{}, problem.Unprocessable("payables.upload_empty", "Nothing has been uploaded to this upload yet.")
	}
	if err != nil {
		return nil, Upload{}, problem.Unavailable("The object store did not answer; capture again.", 30).WithCause(err)
	}
	if size > up.MaxBytes {
		return nil, Upload{}, problem.Unprocessable("payables.document_too_large", fmt.Sprintf("Uploads are at most %d MB.", up.MaxBytes>>20))
	}
	content, _, err := u.Objects.Get(ctx, up.ObjectRef)
	if err != nil {
		return nil, Upload{}, problem.Unavailable("The object store did not answer; capture again.", 30).WithCause(err)
	}
	return content, up, nil
}

// mark closes an upload as used by a resource.
func (u *Uploads) mark(ctx context.Context, tx pgx.Tx, up Upload, content []byte, usedBy uuid.UUID) error {
	sum := sha256.Sum256(content)
	if _, err := tx.Exec(ctx, `UPDATE payables.upload SET status = 'used', size = $2, sha256 = $3, used_by = $4, version = version + 1 WHERE id = $1`,
		up.ID, len(content), sum[:], usedBy); err != nil {
		return fmt.Errorf("payables: mark upload %s used: %w", up.ID, err)
	}
	return nil
}

func (e *Endpoints) uploadToAPI(ctx context.Context, tenant uuid.UUID, up Upload) (*gen.Upload, error) {
	out := &gen.Upload{ID: up.ID, Version: up.Version, CreatedAt: up.CreatedAt, CreatedBy: up.CreatedBy, UpdatedAt: up.UpdatedAt,
		Purpose: up.Purpose, ContentType: up.ContentType, MaxBytes: up.MaxBytes, Status: up.Status, Method: http.MethodPut, ExpiresAt: up.ExpiresAt}
	if up.Size != nil {
		out.Size = gen.NewOptInt64(*up.Size)
	}
	if up.UsedBy != nil {
		out.UsedBy = gen.NewOptUUID(*up.UsedBy)
	}
	t, err := e.Uploads.Target(ctx, tenant, up)
	if err != nil {
		return nil, err
	}
	out.URL, out.Headers, out.Via = t.URL, gen.UploadHeaders(t.Headers), t.Via
	return out, nil
}

// CreateUpload implements POST /v1/uploads.
func (e *Endpoints) CreateUpload(ctx context.Context, req *gen.UploadCreate, _ gen.CreateUploadParams) (gen.CreateUploadRes, error) {
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	up, err := e.Uploads.Create(ctx, req.Purpose, req.ContentType, req.Via.Or(ViaStore))
	if err != nil {
		return nil, err
	}
	out, err := e.uploadToAPI(ctx, p.TenantID, up)
	if err != nil {
		return nil, err
	}
	return &gen.CreateUploadCreatedHeaders{ETag: gen.NewOptString(etag.Format(up.Version)), Location: gen.NewOptString("/v1/uploads/" + up.ID.String()), Response: *out}, nil
}

// GetUpload implements GET /v1/uploads/{id}.
func (e *Endpoints) GetUpload(ctx context.Context, params gen.GetUploadParams) (gen.GetUploadRes, error) {
	if e.Uploads == nil {
		return nil, problem.Unavailable("This server has no object storage.", 0)
	}
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	up, err := e.Uploads.Get(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out, err := e.uploadToAPI(ctx, p.TenantID, up)
	if err != nil {
		return nil, err
	}
	return &gen.GetUploadOKHeaders{ETag: gen.NewOptString(etag.Format(up.Version)), Response: *out}, nil
}
