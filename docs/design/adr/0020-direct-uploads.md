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
  seconds). Lifting it for the upload's route is security posture
  (superseded by the product owner's decision of 2026-11-02, below). The
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

## As built: five minutes for the upload path (2026-11-02, product owner 2026-11-02)
The product owner raised the read timeout to five minutes for the upload
path alone (security posture): 50 MB then needs about 1.4 Mbit/s rather
than 14. Every other request keeps its listener's timeouts (header 10 s,
read 30 s, write 60 s, idle 120 s), and so does every upload request until
it has passed its own checks.
- **The deadlines, hop by hop.** `api.UploadTimeout` (five minutes) is the
  cell's; each hop in front allows `api.UploadMargin` (a minute) more than
  the hop behind it, since its deadlines start first, so the cell is the
  one to cut an upload that overruns, as `router.ModelTimeout` does for a
  model call (ADR-0036). Each is set by `http.ResponseController` in the
  handler; the listeners are unchanged.

  | Hop | Set when | Body read | Answer written |
  |---|---|---|---|
  | erp-server, `PUT /v1/uploads/{id}/content` (`payables.Uploads`) | the URL's signature checked, the upload open and of its type, a receiving slot held | 5 min | 6 min |
  | erp-router, a `PUT` on `/v1/uploads/{uuid}/content` (`router.UploadReadTimeout`, `UploadWriteTimeout`) | the bearer token verified and the tenant's cell found | 6 min | 7 min |
  | erp-web, `POST /invoices/capture` (`allowTime`) | signed in, the form's CSRF token the session's, its fields read | 7 min | 10 min |
  | erp-web's relay to the router (its client's timeout) | each relayed `PUT` | 8 min, the whole call | |

  erp-web's read covers its `createUpload` (30 s at most) before the
  relay, then the cell's five minutes; its relay covers the router's
  answer; its own answer covers the relay and `captureSupplierInvoice`
  (30 s at most). The router's proxy has no timeout of its own (its
  transport sets none; the listener's deadlines bounded it), so the
  router's two deadlines are its proxy's for this path.
- **Slow-loris.** The header's timeout (10 s) is unchanged everywhere, and
  every request reads its first bytes under the listener's 30 s: a request
  is given the upload's time only once it has proved its right to it on
  that hop (above). An anonymous client holds a connection no longer than
  before; a request the cell refuses before its body is read (a wrong
  signature, an upload closed, all slots taken) keeps the cell's listener
  deadlines. The router cannot check the path's signature (the cell holds
  its key), so it lifts its deadlines for a holder of any tenant's token
  with a `PUT` on an upload's path; but only an upload the cell takes
  (signature valid, upload open, a slot held) keeps a router connection
  for six minutes, and one the cell refuses ends within the cell's
  listener timeout (30 s), as before. Once an answer is written nothing more is read: the router and
  erp-web answer an upload with `Connection: close`, so Go's server never
  reads the rest of a body nobody read before answering (otherwise up to
  256 KB, under the lifted deadline: a refused upload trickled in would
  have held the connection the full six or seven minutes; go-reviewer,
  round 1). The cost is one TLS handshake per upload.
- **Holding is bounded by size and time.** A replica receives at most four
  documents at once (`payables.MaxReceiving`), each a temporary file of at
  most 50 MB in erp-server's 256 MiB `/tmp`, now for up to five minutes
  each instead of 30 seconds; a fifth is 503 with `Retry-After: 5`. One
  upload takes one slot on a replica at most: its document sent again
  while it is still arriving there is 503 (go-reviewer, round 1: before,
  one signed URL could take all four). The slots are the replica's, shared
  by every tenant of the cell; how one tenant is kept from holding them
  all is the next section's decision.
  The router and erp-web hold no slot: a connection, a goroutine and a
  buffer each, for up to six and seven minutes. The temporary file is
  removed when the request ends, whether received, refused or cut
  (`spooled.discard`), as before.
- **Not scoped to the path.** Nothing in the deployment cuts an upload at
  30 or 60 seconds: the balancers in front of the router and erp-web are
  network load balancers passing TCP (deploy/terraform/modules/global),
  whose listeners are TCP (`protocol = "TCP"`, no idle timeout set) and
  so time a flow out after 350 seconds, AWS's default for a TCP listener
  (settable from 60 to 6000 seconds by the `tcp.idle_timeout.seconds`
  listener attribute; a TLS listener's is fixed at 350). A flow expires
  only when no data flows either way, so an upload sending is not idle.
  A TCP idle timeout set above 350 seconds while the target's ENI has a
  lower `TcpEstablishedTimeout` drops packets, so raising it is not a free
  knob ([Network Load Balancers][nlb], [Update the idle timeout][nlb-idle],
  read 2026-11-02); erp-web reaches the router's Service and the router
  each cell's Service directly, with no ingress or proxy between. Two
  limits stay for every request alike, and so for the upload: a rollout
  drains each replica for `ERP_SHUTDOWN_TIMEOUT` (20 s) before an upload
  still arriving is cut, and the person sends the form again (the upload
  stays open for its 15 minutes, and the form's derived key finds it); and
  the upload URL's own 15 minutes, checked when the `PUT` begins.
- **Found on the way.** The telemetry middleware's response writer did not
  unwrap, so `http.ResponseController` was refused behind it: the router's
  and the cell's model-call write deadlines (ADR-0036) had never taken
  effect in a deployed server, only in tests that called the handler
  directly. It unwraps now (`statusWriter.Unwrap`), which the upload path
  needs too; a test through the middleware holds it
  (`TestHandlersReachTheServersWriter`), and the tests below go through a
  real listener.
- **Tests.** On each hop a listener whose timeouts are shrunk to a second
  or two (configuration, not sleeping): a body that pauses past the
  listener's read timeout (`pgtest.PausedBody`) is received on the upload
  path, while the same pause on another route is cut at the listener's
  timeout, observed before the upload goes on, so the upload's own
  listener deadline is known to be past; on the cell and the router the
  path's own deadline, brought near by the injected clock, still cuts a
  body that outlasts it, and on the router a refusal by the cell before
  it reads a byte is answered at once, the rest of the body not waited
  for (`TestUploadOutlastsTheListener` in payables and router); one
  upload sent twice at once takes one slot (`TestRelayedUploadFailures`,
  one upload sent twice at once). erp-web's test sends a capture whose document pauses while it
  is being relayed to the real API, both listeners shrunk
  (`TestCaptureOutlastsTheListener`, which also has a form refused while
  its document is paused answered at once); `TestCaptureDeadlines` holds
  erp-web's values, written out since erp-web links no `api` package, to
  the router's and the margins.

## As built: a tenant's share of the slots, and its quota on the path (2026-11-02, product owner 2026-11-02)
With five minutes per upload, one tenant sending four documents slowly
held every receiving slot of a replica, and the path was outside the
request quotas. The product owner chose both remedies offered (security
posture, PO 2026-11-02):
- **A tenant's share of a replica's slots.** One tenant holds at most
  `payables.MaxReceivingPerTenant` (two) of a replica's
  `payables.MaxReceiving` (four) slots at once, configurable beside it
  (`Uploads.MaxReceivingPerTenant`, 0 for the constant). Its third
  concurrent document on that replica is 503 with `Retry-After: 5` and a
  detail of its own ("as many of your documents at once as one tenant
  may"), and other tenants still take the remaining slots. The tenant is
  the one the URL's verified signature names; a slot comes back when its
  request ends, whatever the outcome. An upload sent again while it is
  arriving is still refused first, as before. Two tenants can still fill
  a replica together; with a cell's replicas behind the balancer, a
  tenant's retry may land on another replica, whose share is its own.
- **The tenant's request quota on the path.** A `PUT` on the API's path is
  charged to the request quota of the tenant its URL is signed for
  (`ERP_RATE_LIMIT_TENANT`, 04 §4.11), the same bucket that tenant's API
  requests spend on the replica (`ratelimit.Limiter.ChargeTenant`, which
  shares the API middleware's charging and refusal: `Uploads.Limits` is
  the API's limiter). It is charged only once the signature and expiry
  check out, so a forged or expired URL spends no tenant's quota (it is
  refused 403 as before, uncharged), and before the upload is read from
  the database or its body read. Over quota it is the API's 429
  `request.rate_limited` with `Retry-After`, and every answer after the
  charge carries the API's `RateLimit-Limit`, `RateLimit-Remaining` and
  `RateLimit-Reset`.
- **Decided by default: no principal quota on the path.** The path has
  no principal: the signed URL names the tenant and the upload, not who
  created it, and its holder may be erp-web relaying for a person or a
  client the URL was handed to. Charging the creator's principal bucket
  would mean looking up the creator's issuer and subject (the bucket's
  key; the upload records only `created_by`) before the quota check, and
  spending a person's quota on a holder's retries; the tenant quota
  and the per-tenant share already bound what one URL holder can take.
  So only the tenant quota applies (for the product owner to overrule).

[nlb]: https://docs.aws.amazon.com/elasticloadbalancing/latest/network/network-load-balancers.html
[nlb-idle]: https://docs.aws.amazon.com/elasticloadbalancing/latest/network/update-idle-timeout.html
