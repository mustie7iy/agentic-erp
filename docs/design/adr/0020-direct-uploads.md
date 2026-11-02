# ADR-0020: Direct uploads for documents above the request limit

**Status:** proposed

## Context
Request bodies are capped at 5 MB (04-api-design §4.11), and the unit of
work reads every body to hash it for idempotency. Scanned invoices can be
larger. Since T12 documents live in write-once object storage; §4.11 left
larger ones to "upload URLs".

## Decision

**An upload is a resource** (`POST /v1/uploads`, `GET /v1/uploads/{id}`): it
says what will arrive (purpose, content type, size limit), where to send it
(`url`, `method`, `headers`), until when, and later what became of it
(`received`, `used`, size, the resource that consumed it). The operation
that consumes a document names the upload instead of carrying the bytes
(`:capture` takes `upload_id` or `document`, one of the two).

**The bytes go to the store, not through the unit of work.** With an S3
store the URL is a presigned `PUT` (query-string Signature Version 4, the
content type signed, 15 minutes); the API never sees the bytes. With the
directory store the URL is a path on this API, signed with a key derived
from the page-token key, served outside the generated server and the unit
of work: no bearer token, a body up to the upload's limit, the object
stored and the upload marked received in one transaction.

**Consuming an upload checks before it reads.** The capture `Stat`s the
object first (nothing there: 422 `payables.upload_empty`, or
`upload_expired` after the URL's validity; too large: 422), then reads it,
then stores it under its hash like a document sent in the request, so the
invoice's document path does not change. The upload is used once.

**A re-sent PUT is idempotent by content.** The store keeps the first
object under a key; a PUT whose answer was lost sends the same bytes and
is accepted, different bytes are a conflict.

## Alternatives
- **Raise the request limit.** Every replica buffers every body for the
  idempotency hash; 50 MB bodies on the request path cost memory and time
  on every request for the few that need it.
- **S3 POST policies** to bound the size at the store. Not every
  S3-compatible service supports them; the size is checked when the upload
  is consumed instead, before the bytes are read.
- **Multipart uploads to the store.** For documents far above 50 MB; none
  of the platform's documents are.

## Consequences
- One table (`payables.upload`), two operations, one route outside the
  generated server, two store calls (`Stat`, presigned `PUT`).
- Objects of uploads never consumed stay under `tenants/*/uploads/`; a
  lifecycle rule of the bucket (or the offboarding erasure to come)
  removes them.
- The API's own upload URL is a path; a deployment behind several
  hostnames resolves it against the API's base URL.

## As built: the API's own path on any store (2026-11-01, product owner 2026-11-01)
The web workspace never follows a presigned bucket URL and its egress stays
as narrow as it is (ADR-0052 §7); it streams an upload through the router
to the API instead. On S3 the API offered only the presigned URL, so:
- **Contract: the client asks for the API's path.** `UploadCreate` gains
  an optional `via` (`store`, the default, or `api`; another value is 422
  `payables.unknown_upload_via`), and `Upload` says which URL it gives
  (`via`, required in the answer). `store` keeps today's behaviour: the
  store's presigned URL when it presigns, else the API's path, so a
  directory store answers `via: api` whatever was asked. One URL per
  upload, recorded in `payables.upload.via` (migration 000188), so
  `getUpload` gives the same URL again and the API's path refuses an
  upload whose URL is the store's (403 `payables.invalid_upload_url`).
  Rows from before the column default to `store` and keep the URL they
  were given: the API's path takes an upload whose `via` is `api`, or any
  when the store cannot presign. Non-breaking: a request field that is
  optional and a field added to an answer. Chosen over the server always
  offering both URLs, which would give one upload two ways in, and over
  a store-wide setting, which would move every client off the store.
- **The API's path keeps this ADR's protections, now on S3 too.** The
  HMAC signature over tenant, upload and expiry; the upload's 15 minutes;
  one document (the upload `open`, checked before the body is read and
  again under the row's lock before it is stored; a second PUT is 409
  `payables.upload_closed`); the content type the upload was created for,
  exactly (422 `payables.upload_content_type`); the tenant the URL was
  signed for, whose row-level security scopes both transactions; and the
  upload's `max_bytes` (50 MB), enforced while the body streams in: a
  `Content-Length` above it is 413 `payables.document_too_large` before a
  byte is read, a body of unknown length is cut at the limit with the
  same answer, and nothing is stored either way.
- **Streamed, never held in memory.** The body is copied to a temporary
  file (erp-server's `/tmp`, an emptyDir of 256 MiB) while it is measured
  and hashed, then sent to the store from the file: S3 needs a PUT's
  length before its bytes, and its signed payload hash and the
  `x-amz-checksum-sha256` that an Object Lock bucket requires (T41b)
  before it accepts them (`blobstore.Streamer`, `PutStream`, both
  stores). No transaction is open while the client sends: a slow client
  holds a temporary file and one of the replica's receiving slots
  (`payables.MaxReceiving`, four, so their files fit the emptyDir; one
  more is 503 with `Retry-After`), never a database connection, which the
  directory store's path held before. A re-sent PUT of a stored object is
  compared by SHA-256.
- **Through the global router.** The router routes every request by its
  bearer token's tenant (ADR-0022), so a client reaching the API through
  it sends its bearer token with the PUT; the cell authenticates the PUT
  by its signature alone, as before. erp-web sends the person's.
- **Decided by default (for the product owner to overrule):** the
  30-second read timeout of erp-web's, the router's and erp-server's
  listeners is unchanged and bounds how long an upload may take (50 MB
  needs about 14 Mbit/s end to end; a scanned invoice of a few MB goes in
  seconds). Lifting it for the upload's route is security posture. The
  capture still reads the consumed document whole to hash and extract it,
  as before.
- **Walkthrough:** step 18 creates an upload `via: api`, streams a 6 MB
  scan to the API's path (chunked), is refused a second PUT, and captures
  the invoice from it, for item 1 of a released order of Tyrell migrated
  from SAP: an invoice names a released order, and the chairs' order is
  closed by step 15 (batch 38's integration found the capture refused
  422 `payables.invalid_purchase_order` on it); the model is a stand-in that answers the
  extraction (`internal/llm/llmtest/modelstub`), as the mock SAP extract
  stands in for SAP.
