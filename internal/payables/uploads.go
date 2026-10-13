package payables

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Direct uploads (T16, 04-api-design §4.11): a document too large for a
// request body is uploaded to object storage through a URL the API hands
// out, then consumed by id. With an S3 store the URL is a presigned PUT on
// the store; with a directory store (development) it is a signed path on
// this API, which receives the bytes and stores them itself. Either way
// the bytes land at tenants/<tenant>/uploads/<id>, and a capture copies them
// under their hash like a document sent in the request.

// MaxUpload is the largest document a direct upload may carry.
const MaxUpload = 50 << 20

// UploadTTL is how long an upload URL accepts bytes.
const UploadTTL = 15 * time.Minute

// UploadPurposes are what uploads may be for, and the operation that consumes each.
var UploadPurposes = map[string]string{"supplier_invoice": "POST /v1/supplier-invoices:capture"}

// UploadTypes are the content types a document may be uploaded as.
var UploadTypes = map[string]bool{"application/pdf": true, "image/png": true, "image/jpeg": true, "application/xml": true, "text/xml": true}

// Upload is a row of payables.upload.
type Upload struct {
	ID          uuid.UUID
	Purpose     string
	ContentType string
	MaxBytes    int64
	ObjectRef   string
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
	// InTx runs the PUT's transaction; nil: Pool.InTx. Tests inject failures.
	InTx func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error
}

func (u *Uploads) inTx(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
	if u.InTx != nil {
		return u.InTx(ctx, scope, fn)
	}
	return u.Pool.InTx(ctx, scope, fn)
}

func (u *Uploads) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

const uploadColumns = `id, purpose, content_type, max_bytes, object_ref, status, size, sha256, used_by, expires_at, created_at, created_by, updated_at, version`

func scanUpload(row pgx.Row) (Upload, error) {
	var up Upload
	err := row.Scan(&up.ID, &up.Purpose, &up.ContentType, &up.MaxBytes, &up.ObjectRef, &up.Status, &up.Size, &up.SHA256, &up.UsedBy, &up.ExpiresAt,
		&up.CreatedAt, &up.CreatedBy, &up.UpdatedAt, &up.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Upload{}, problem.NotFound("No such upload.")
	}
	return up, err
}

// Create starts an upload in the request's transaction.
func (u *Uploads) Create(ctx context.Context, purpose, contentType string) (Upload, error) {
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
	if u == nil || u.Objects == nil {
		return Upload{}, problem.Unavailable("This server has no object storage; send documents of up to 5 MB in the request.", 0)
	}
	id := uuid.Must(uuid.NewV7())
	up := Upload{ID: id, Purpose: purpose, ContentType: contentType, MaxBytes: MaxUpload, ObjectRef: blobstore.Key(p.TenantID, blobstore.KindUploads, id.String()),
		Status: "open", ExpiresAt: u.now().Add(UploadTTL).UTC(), CreatedBy: p.ID}
	return scanUpload(tx.QueryRow(ctx, `INSERT INTO payables.upload (tenant_id, id, purpose, content_type, max_bytes, object_ref, status, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'open', $7, $8) RETURNING `+uploadColumns, p.TenantID, id, purpose, contentType, MaxUpload, up.ObjectRef, up.ExpiresAt, p.ID))
}

// Get reads an upload in the request's transaction.
func (u *Uploads) Get(ctx context.Context, id uuid.UUID) (Upload, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Upload{}, err
	}
	return scanUpload(tx.QueryRow(ctx, `SELECT `+uploadColumns+` FROM payables.upload WHERE id = $1`, id))
}

// Target is where an upload's bytes go.
type Target struct {
	URL     string
	Method  string
	Headers map[string]string
}

// Target makes the URL for an upload of the tenant: presigned on the store
// when it can, else the API's own signed path.
func (u *Uploads) Target(ctx context.Context, tenant uuid.UUID, up Upload) (Target, error) {
	t := Target{Method: http.MethodPut, Headers: map[string]string{"Content-Type": up.ContentType}}
	if p, ok := u.Objects.(blobstore.Presigner); ok {
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
// size limit, stored where the upload says and marked received.
func (u *Uploads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := u.receive(w, r); err != nil {
		problem.Write(w, r, problem.From(err))
	}
}

func (u *Uploads) receive(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	id, err := uuid.Parse(r.PathValue("id"))
	tenant, terr := uuid.Parse(q.Get("tenant"))
	expires, eerr := strconv.ParseInt(q.Get("expires"), 10, 64)
	if err != nil || terr != nil || eerr != nil || !hmac.Equal([]byte(q.Get("signature")), []byte(u.sign(tenant, id, q.Get("expires")))) {
		return problem.Forbidden("payables.invalid_upload_url", "The upload URL is not valid.")
	}
	if u.now().Unix() > expires {
		return problem.Forbidden("payables.upload_url_expired", "The upload URL has expired; start a new upload.")
	}
	if u.Objects == nil {
		return problem.Unavailable("This server has no object storage.", 0)
	}
	return u.inTx(r.Context(), database.Scope{Role: u.AppRole, TenantID: tenant}, func(ctx context.Context, tx pgx.Tx) error {
		up, err := scanUpload(tx.QueryRow(ctx, `SELECT `+uploadColumns+` FROM payables.upload WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if up.Status != "open" {
			return problem.Conflict("payables.upload_closed", "This upload has already received its document.")
		}
		if ct := r.Header.Get("Content-Type"); ct != up.ContentType {
			return problem.Unprocessable("payables.upload_content_type", "Send the document as "+up.ContentType+", the type the upload was created for.")
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, up.MaxBytes))
		if err != nil {
			return problem.New(http.StatusRequestEntityTooLarge, "payables.document_too_large", "Document too large",
				fmt.Sprintf("Uploads are at most %d MB.", up.MaxBytes>>20)).WithCause(err)
		}
		err = u.Objects.Put(ctx, up.ObjectRef, body, blobstore.PutOptions{ContentType: up.ContentType})
		if errors.Is(err, blobstore.ErrExists) {
			// A PUT that stored the object but did not get to record it (a
			// failed statement, a lost answer) is sent again: the same bytes
			// are fine, others are not, since the store keeps the first.
			stored, _, gerr := u.Objects.Get(ctx, up.ObjectRef)
			if gerr != nil || !bytes.Equal(stored, body) {
				return problem.Conflict("payables.upload_conflict", "This upload already holds a different document; start a new upload.")
			}
			err = nil
		}
		if err != nil {
			return problem.Unavailable("The object store did not take the document; send it again.", 30).WithCause(err)
		}
		sum := sha256.Sum256(body)
		if _, err := tx.Exec(ctx, `UPDATE payables.upload SET status = 'received', size = $2, sha256 = $3, version = version + 1 WHERE id = $1`, id, len(body), sum[:]); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
}

// take reads the document of an upload for the operation consuming it, in
// the request's transaction, locking the upload; mark then closes it.
func (u *Uploads) take(ctx context.Context, tx pgx.Tx, id uuid.UUID, purpose string) ([]byte, Upload, error) {
	up, err := scanUpload(tx.QueryRow(ctx, `SELECT `+uploadColumns+` FROM payables.upload WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, Upload{}, err
	}
	// Only supplier_invoice uploads exist, so an upload always matches the
	// purpose of the one operation that consumes them; a second purpose
	// adds the check here.
	_ = purpose
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
	_, err := tx.Exec(ctx, `UPDATE payables.upload SET status = 'used', size = $2, sha256 = $3, used_by = $4, version = version + 1 WHERE id = $1`, up.ID, len(content), sum[:], usedBy)
	return err
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
	out.URL, out.Headers = t.URL, gen.UploadHeaders(t.Headers)
	return out, nil
}

// CreateUpload implements POST /v1/uploads.
func (e *Endpoints) CreateUpload(ctx context.Context, req *gen.UploadCreate, _ gen.CreateUploadParams) (gen.CreateUploadRes, error) {
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	up, err := e.Uploads.Create(ctx, req.Purpose, req.ContentType)
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
