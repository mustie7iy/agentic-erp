# ADR-0041: API lifecycle and webhooks

**Status:** accepted (T50, product owner 2026-10-19)

## Context
The public API and the domain events are contracts (`api/openapi.yaml`,
`api/asyncapi.yaml`). 04 §4.10 allows only additive changes within `/v1`
and announces deprecations with headers, and oasdiff checks every pull
request against main; but nothing said how long a deprecation runs, how an
event type evolves, what an accepted breaking change must record, or how
external systems receive events (04 §4.12 promised signed webhooks). A
pilot partner is about to integrate; after that, a break of `/v1` or of an
event version breaks someone else's system.

## Decision

### Versions
- **Paths carry the major version.** `/v1` changes only additively; a
  breaking change is a new `/v2` operation served alongside `/v1` for at
  least 12 months.
- **Event types carry the major version as a suffix**
  (`com.example.erp.<context>.<event>.v<N>`). Within a `.vN` only additions
  are allowed: no event type or field removed, no type changed, no field
  newly required or no longer required. A breaking change publishes
  `.vN+1` alongside `.vN` for 12 months.

### Deprecation
- An operation is deprecated in the contract: `deprecated: true`,
  `x-deprecated-at: <date>`, `x-sunset: <date>` at least 365 days after it,
  and `externalDocs.url` pointing at its migration notes. The API package
  refuses a contract where these are incomplete or the notice is shorter.
  The headers are served for the public API (`openapi.yaml`); the operator
  and model contracts have no external consumers to warn.
- Every answer to a deprecated operation, errors included, carries
  `Deprecation: @<unix seconds>` (RFC 9745), `Sunset: <HTTP date>`
  (RFC 8594) and `Link: <notes>; rel="deprecation"`. A middleware builds
  the table from the embedded contract at start; there is no hand list.
- oasdiff runs with `--deprecation-days-stable=365` in CI and in
  `scripts/ci-local.sh`, so a sunset closer than 365 days, or a removal
  before the sunset, fails the breaking-change check.

### Accepted breaking changes
- Every entry of `api/breaking-accepted.txt` sits under a comment naming
  the tracker item that accepted it and why (`# T27 (ADR-0012): reason`);
  a Go test refuses an entry without one.
- Once the pilot partner consumes `/v1`, entries are accepted only for
  operations marked `x-stability-level: beta`; stable operations change
  through `/v2`. The rule is recorded now and enforced by the test when the
  partner exists (the product owner names it).

### Event compatibility
- `cmd/eventcompat` compares `api/asyncapi.yaml` with main's, message by
  CloudEvents type, and fails on a removed event type or field, a changed
  type, or a field newly required or no longer required. It compares types,
  properties, required fields, items and alternatives; formats, enums and
  headers are left to review. It runs in the existing breaking-change
  step of CI and in `scripts/ci-local.sh`, not as a job of its own.

### Webhooks (T50b–g)
- **Signing:** Standard Webhooks (`webhook-id`, `webhook-timestamp`,
  `webhook-signature` HMAC-SHA256 over `id.timestamp.body`). Each
  subscription has its own `whsec_` secret, sealed with fieldcrypt
  (ADR-0018). Rotation keeps the old secret valid for 24 hours, signing
  with both meanwhile.
- **Subscriptions** are per tenant, at most 20, each with an HTTPS URL and
  explicit event types; `<context>.*` subscribes to a context. Only events
  marked `x-webhook: true` in the AsyncAPI contract can be subscribed; at
  first, events carrying bank or personal data are not marked.
- **Dispatcher:** a dedicated worker consumes JetStream, fans each event out
  idempotently to one delivery per matching subscription (keyed by event id
  and subscription), and delivers it. Retries back off 30 s, 2 min, 10 min,
  1 h, 6 h, then every 12 h up to 3 days, after which the delivery is dead
  lettered. A subscription failing for 5 days is disabled; a `410 Gone`
  disables it at once. Deliveries are ordered per resource (`subject`): a
  later event for a resource waits for the earlier one.
- **Operations:** replay of a time range, redelivery of one delivery, and a
  ping event to test an endpoint. Deliveries are kept 30 days.
- **Egress** goes through a shared `internal/platform/egress` client:
  `RefusePrivate` moves there from authn, which reuses it; https only, to
  public unicast addresses checked on the address dialled (so DNS
  rebinding is refused), no proxy (ADR-0033), no redirects followed (a
  redirect is answered as the delivery's status), the system roots rather
  than `ERP_TLS_CA` and no client certificate, 5 s connect and 10 s total
  timeouts, responses and their headers read up to 64 KB. Tests reach a
  loopback server only through a constructor option that no configuration
  sets and that fails outside a test binary.
- **Signatures** (`internal/platform/webhook`): `webhook-signature` is
  `v1,<base64 HMAC-SHA256>` per secret, space-separated during a rotation;
  verification ignores other versions and allows a timestamp within 5
  minutes either way. `api/webhooks-test-vectors.json`, starting with the
  specification's reference vector, fixes the format for subscribers and
  is checked by the tests.
- **Governance:** subscription changes are `x-agent: false` (an agent never
  routes tenant data to a new endpoint); reads and the ping are
  `x-autonomy: read`. Every change writes an audit record (ADR-0011).
- **Contract and schema (T50c):** `integration.webhook_subscription` and
  `integration.webhook_delivery` (migration 000090), permissions
  `integration.webhook.read` and `.manage`. Deleting a subscription sets
  status `deleted` (nothing is deleted, 03 §3.1); the limit of 20 counts
  the subscriptions not deleted and is the application's. Events of the
  identity context, partner bank accounts and partner master data (a
  partner may be a person) are not marked `x-webhook`, nor are agents,
  platform and tenant-lifecycle events; `api/asyncapi_test.go` refuses a
  mark on an excluded kind. Free-text fields of marked events (a block
  reason, a hold reason, an approval comment) are sent as written; a user
  who types bank or personal details there sends them. The subscription
  URL is https with a host, without credentials or fragment (a query may
  carry the endpoint's token), as the egress client requires.
- **Subscription API (T50d):** `/v1/webhook-subscriptions` with create,
  list, get, update (merge patch) and the custom methods `:delete` (status
  `deleted`; 04 §4.3 has no hard delete) and `:rotateSecret`, each served
  by its handler (no 501). The URL is checked at write time by
  `egress.CheckURL`, the client's rules plus the host resolving to public
  addresses only (the client still checks every address it dials). Event
  types are those marked `x-webhook` or `<context>.*` for a context that
  offers any, duplicates dropped. The limit of 20 is counted under a
  per-tenant advisory lock. The `whsec_` secret is answered only by create
  and `:rotateSecret`; `uow.OmitOnReplay` keeps it out of the answer
  stored with the idempotency key, so a replay answers without it and it is
  never at rest in the clear. It is sealed in hex, since fieldcrypt seals
  values upper-cased. A seal without that normal form was weighed and
  left out: it would have to reach the keyring, the legacy keys and the
  re-sealing at key rotation (a mark per column), all for one value
  whose bytes hex already carries losslessly; IBANs keep the normal form. Both secret columns are re-sealed when the tenant's
  key rotates. A second rotation within 24 hours retires the oldest secret
  at once (two secrets sign at most); `:delete` drops the previous one.
  Changes are audited without the URL's path or query (only its host) and
  publish no event: nothing in the platform reacts to them. Reads are GETs
  and so already allowed at every autonomy level; no `x-autonomy` mark is
  needed. Pausing and resuming a subscription wait for the dispatcher
  (T50e).
- **Consumer review (T50h)** of the contract waits until the product owner
  names the pilot partner.

## Alternatives
- **Versions in a header or media type:** keeps URLs stable but hides the
  version from logs, caches and gateways; paths are what 04 §4.2 already
  uses.
- **Deprecation headers from a hand-kept list:** drifts from the contract;
  the contract is the only source.
- **Sunset date alone, without `x-deprecated-at`:** RFC 9745's Deprecation
  header needs the date of deprecation, and the 365-day notice can only be
  checked against a fixed start, not against the day the test runs.
- **AsyncAPI diff tools:** none checks schema compatibility per event type
  at the depth needed (refs, items, alternatives); a small Go check on the
  fields this ADR names is enough and runs without a new tool.
- **Webhooks from the outbox relay directly:** couples tenant endpoints'
  latency and failures to the relay that feeds every context.

## Consequences
- A deprecation is a contract change reviewed like any other; its headers
  appear without code.
- Breaking `/v1` after the partner integrates needs a beta operation or a
  `/v2`; breaking an event needs a `.vN+1` and both published for a year.
- The egress package came with T50b, `x-webhook` with T50c and the
  subscription API with T50d.
