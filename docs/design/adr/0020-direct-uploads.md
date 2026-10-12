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
