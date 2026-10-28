# 4. API design

The API is the product. The web UI, agents, CLI, SAP connectors and partner
integrations are all clients of the same public API; there are no private
endpoints and no direct database access (ADR-0013). This document lists the
rules every endpoint follows. The contract itself is
[`api/openapi.yaml`](../../api/openapi.yaml); events are in
[`api/asyncapi.yaml`](../../api/asyncapi.yaml).

## 4.1 Contract first

1. A capability starts as a change to `api/openapi.yaml` (and
   `api/asyncapi.yaml` if it emits events), reviewed like code.
2. CI lints the spec and runs a breaking-change check against `main`.
3. Server interfaces, request/response types and the Go client are
   generated from the spec. Handlers implement the generated interfaces, so
   the code cannot drift from the contract.
4. Contract tests replay the examples in the spec against the running service.

Implementation starts only after the contract is approved
([06-engineering-process.md](06-engineering-process.md)).

## 4.2 Resources and URLs

| Rule | Example |
|---|---|
| Base path carries the major version | `https://{cell}.api.example.com/v1` |
| Collections are plural kebab-case nouns | `/v1/supplier-invoices` |
| Resources are addressed by UUID | `/v1/supplier-invoices/0192f3a4-...` |
| Child collections are nested only when the child cannot exist alone | `/v1/purchase-orders/{id}/items` |
| Human document numbers are a filter, not a path | `?document_number=5100000123` |
| Company scope is a filter or a field, never a path prefix | `?legal_entity_id=...` |
| JSON field names are `snake_case` | `posting_date`, `gross_amount` |

The tenant is never in the URL or body. It comes from the access token and is
set on the database session (ADR-0003); a client cannot address another
tenant's data by any URL.

## 4.3 Methods

Standard methods map to HTTP verbs. State transitions are **custom methods**
(AIP-136): `POST` to `{resource}:{verb}`. A document's state is never changed
by `PATCH`, so every transition has its own permission, validation, audit
record and event.

| Method | HTTP | Returns | Notes |
|---|---|---|---|
| List | `GET /v1/things` | 200, page | Filters, sort, cursor |
| Get | `GET /v1/things/{id}` | 200, resource, `ETag` | |
| Create | `POST /v1/things` | 201, resource, `Location`, `ETag` | `Idempotency-Key` required |
| Update | `PATCH /v1/things/{id}` | 200, resource, `ETag` | JSON Merge Patch; `If-Match` required; only drafts and master data |
| Custom | `POST /v1/things/{id}:release` | 200 resource, or 202 `Operation` | `If-Match` and `Idempotency-Key` required |
| Collection custom | `POST /v1/things:propose` | 202 `Operation` (or 200) | `Idempotency-Key` required; no `If-Match` (no single resource) |
| Delete | — | — | No hard deletes. Drafts are `:cancel`led, master data is `:block`ed |

Posted financial documents have no update method at all. Creating a journal
entry *is* posting it (`POST /v1/journal-entries`; there are no draft journal
entries). Corrections are custom methods that create new documents:
`journal-entries/{id}:reverse`, `supplier-invoices/{id}:reverse` (posts a
reversal entry). `supplier-invoices/{id}:cancel` applies only to invoices not
yet posted.

**Effective-dated assignments** (role assignments, role permissions, company
access, SoD rules) are created and then ended with `POST {id}:end`
(`{"valid_to": "..."}`); they are never updated or deleted. Bank accounts are
ended only through an approved partner change.

**Approvals** are decided in one place: `approval-requests/{id}:approve` and
`:reject`, by humans only. Objects that need approval (requisitions, change
requests, partner changes, high-value invoices) are `:submit`ted, and move on
when their approval request completes; they have no `:approve` method of their
own. Payment-run `:release` / `:reject` are the single SoD-checked human
release step, not a multi-step approval. A run above its company's
`release_dual_limit` is the one exception to a direct release (product
owner 2026-10-22, ADR-0038): a releaser first `:submit`s it, which opens a
`payment_run_release` approval request that a second person, neither the
proposer nor the releaser, approves; the approval moves nothing on, and
`:release` stays the release step, refused until the approval is there.
A rejected request rejects the run and frees its items (product owner
2026-10-25), so a new run is proposed; a withdrawn one leaves it proposed.
The requester, human or agent, may
`:cancel` its own pending request with a reason.

## 4.4 Representations

- Money is an object: `{"amount": "1080.00", "currency": "EUR"}`. The amount
  is a decimal **string** with exactly the currency's minor-unit digits, so no
  client parses money as floating point. The server stores minor units
  (ADR-0005) and rejects amounts with the wrong number of decimals.
- Quantities are decimal strings with a unit: `{"value": "12.5", "unit": "KG"}`.
- Instants are RFC 3339 UTC (`2026-10-09T14:03:11Z`); business dates are
  `YYYY-MM-DD`.
- Enumerations are lower-case strings and are **open**: clients must accept
  values they do not know, so adding a status is not a breaking change.
- Every resource has a uuid `id`, `created_at` and `created_by`. Mutable
  resources add `version`, `updated_at` and `updated_by`, and their ETag is
  `version`. Insert-only resources (journal entries, goods movements, match
  results, decisions) have no `version`; their ETag is a hash of the
  representation. Effective-dated assignments add `valid_from`, `valid_to`,
  `ended_at` and `ended_by`. Human keys (document numbers, configuration
  version numbers) are fields and filters, never the path.
- References to other resources are ids (`supplier_id`). `?expand=supplier`
  may embed a read-only copy for a fixed list of fields; expansion never
  crosses tenants or permissions.
- Custom fields (ADR-0009) appear under `extensions`, validated against the
  tenant's published schema for that resource.
- Sensitive values are never returned in full: bank accounts expose
  `account_last4` and `account_scheme` only, and for a Canadian account
  (scheme `ca`, T45d) the bank's `institution_number` and `transit_number`,
  which name the bank as a BIC does.

## 4.5 Lists, filters and pagination

- **Cursor pagination only:** `?page_size=50&page_token=...`; responses carry
  `next_page_token` (absent on the last page). `page_size` defaults to 50,
  maximum 500. Tokens are opaque, signed and encode the sort keys, so pages
  stay stable while rows are inserted, and every page is an index range scan
  whatever the table size (05-scalability).
- No total counts by default. `?include=total_estimate` returns a planner
  estimate where it is cheap.
- Filters are named query parameters documented per resource
  (`status`, `legal_entity_id`, `supplier_id`, `posting_date_from`,
  `posting_date_to`). Each one is backed by an index that leads with
  `tenant_id`; filters without an index are not offered.
- Sorting is restricted to documented fields: `?order_by=posting_date desc`.
  List queries are planned for their arguments on every call (the keyset
  helper runs them as unnamed statements): a cached generic plan cannot
  drop the filters a request leaves out, and so reads the whole table on
  every page. The target is that each sort is backed by an index ending in
  the sort column and `id`, after the filters it serves; the journal entry
  list has one (migration 000055), and indexing the other lists' sorts is
  tracked.
- Bulk reads and exports (more than about 10,000 rows) use an export
  operation that writes a file to object storage, not deep pagination.

## 4.6 Errors

Errors use RFC 9457 Problem Details (`application/problem+json`):

```json
{
  "type": "https://errors.example.com/payables/duplicate-invoice",
  "title": "Duplicate supplier invoice",
  "status": 409,
  "code": "payables.duplicate_invoice",
  "detail": "Supplier S0001 already has invoice INV-2026/001 (5100000123).",
  "instance": "/v1/supplier-invoices",
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "errors": [{"field": "supplier_reference", "code": "duplicate", "message": "..."}]
}
```

| Status | When |
|---|---|
| 400 | Malformed request, unknown field, wrong money precision |
| 401 | Missing or invalid token |
| 403 | No permission, no company access, or a segregation-of-duties conflict (`code` says which) |
| 404 | Not found, **or** exists in another company the caller cannot see |
| 409 | Business rule conflict: wrong state for the transition, duplicate, period closed |
| 412 | `If-Match` does not match the current version |
| 422 | Validation failed (`errors[]` lists fields), or `Idempotency-Key` reused with a different body |
| 428 | `If-Match` or `Idempotency-Key` missing where required |
| 429 | Rate limit; `Retry-After` set |
| 503 | Dependency unavailable; safe to retry with the same `Idempotency-Key` |

`code` values are stable and documented; clients and agents branch on `code`,
never on `detail`. Database constraint violations are translated to these
codes by the repository layer; a raw database message never reaches a client.
Each operation lists the codes its handler can answer in `x-problem-codes`,
and the document lists those the request pipeline can answer on any
operation (authentication, authorization, rate limits, idempotency,
timeouts, the database's generic codes). Both are derived from the code
and checked to match it (`api`'s `TestProblemCodes`, ADR-0052 G7); `go
test ./api -run TestProblemCodes -update` rewrites them.

Every operation that posts can answer the account assignment refusals
(ADR-0050 §5, T56d): 422 `ledger.cost_center_required`,
`ledger.cost_center_invalid` and `ledger.profit_center_invalid` (the
reason is the field error's `code`, on `lines[i].cost_center_id` or
`lines[i].profit_center_id`), `ledger.profit_center_mismatch`,
and `ledger.segment_mismatch`. Manual and parked entries take the centers
on their lines (`cost_center_id`, `profit_center_id`, `segment_id`, T56d2);
journal lines show each with its code (`cost_center_code`...), parked lines
the ones entered; `GET /v1/journal-entries` filters by `cost_center_id`
and `profit_center_id`. A center change that contradicts posted lines is
409: `ledger.cost_center_posted_after` (its validity would end on or before
a line's posting date), `ledger.cost_center_period_in_use` (the attributes
in force on a posted date would differ; a lock is compared with the lines
of its kind only), `ledger.profit_center_posted_after` and
`ledger.profit_center_period_in_use` (the same for a profit center and the
lines carrying it, T56d4) and `ledger.segment_in_use` (a posted profit
center's segment), at the request and again at the approval.

Requisition and purchase order items take `account_assignment` (`none` or
`cost_center`; left out: `cost_center` when a `cost_center_id` is given)
and `cost_center_id` (T56e): 422 `procurement.invalid_account_assignment`
when they disagree. Submitting a requisition, releasing an order (and
approving its release), and submitting or approving an order change that
adds items check them as their postings will, naming `items[i]`: on a
primary cost account the item names a cost center while cost center
accounting applies (422 `ledger.cost_center_required`, no default taken),
and the center is valid and unlocked (`ledger.cost_center_invalid`,
`ledger.profit_center_invalid`). A released item keeps its assignment (422
`procurement.invalid_change`). Supplier invoice items without an order take
`cost_center_id` and `profit_center_id`, checked at entry with the
account's default; an item on an order takes its order item's (422
`payables.assignment_from_order` for one of its own).

The actual reports by account assignment (ADR-0050 §7, T56f), `GET
/v1/cost-center-balances` and `GET /v1/profit-center-balances`
(`ledger.cost_center.read` and, as for every ledger amount,
`ledger.entry.read`; `x-slo-class: report`), give the period totals
of one legal entity, ledger (the leading one by default) and fiscal year,
per center, account, period and transaction currency, a profit center's
rows with its segment; profit and loss accounts only. They filter by
period range (`fiscal_period_from`, `fiscal_period_to`; a range ending
before it starts is 400 `request.invalid_parameter`), center, hierarchy
node (its centers and those below it under today's tree), segment and
account, and page like the trial balance. The figures are exact at the
read, as the trial balance's: projected balances plus the lines not yet
projected. A row's lines are the entries `GET /v1/journal-entries` lists
for its center (`cost_center_id`, `profit_center_id`) with the company,
ledger and year; the reports add no journal filter.

Plan values (ADR-0050 §8, T56g) are the controlling context's.
`/v1/plan-categories` (`controlling.plan.read`, `.write`) are open or
locked by a person (`:lock`, `:unlock`: `controlling.plan.lock`, an
approval permission, `x-agent: false`). `POST /v1/plan-batches` (`x-agent:
false`; agents' drafts are T56j's) replaces the plan of a category,
company and fiscal year on the centers its lines name; every line is
checked first (422 `controlling.plan_batch_invalid`, naming
`lines[N].field`), and it is idempotent by `batch_key` beyond the
`Idempotency-Key`'s window: the key applied with the same lines answers the
batch as it was (`replayed`) and publishes nothing, with other lines 409
`controlling.plan_batch_reused`; a locked category is 409
`controlling.plan_category_locked`; a batch that would change a cost
center's plan in a period it is locked for planning in (`lock_plan`,
T56g2), by a line there or by replacing its current line there, is 422
`controlling.cost_center_plan_locked`, naming the lines. `GET /v1/plan-lines` lists the current
plan. `GET /v1/cost-center-plan-actual` (`controlling.plan.read` and
`ledger.entry.read`, `x-slo-class: report`) gives per cost center and
account, over a range of periods, the plan, the leading ledger's actual
exact at the read, the variance (actual less plan) and the variance in
percent of the plan as a decimal string with two decimals, computed
exactly and rounded half away from zero, absent on a zero plan; it pages
by a cursor over cost center and account. `GET
/v1/profit-center-plan-actual` (T56g3) is the same by profit center (a
profit center or a node of a profit center hierarchy), its actual as `GET
/v1/profit-center-balances` reads it: profit and loss accounts only, the
segments summed, paged by profit center and account.

## 4.7 Concurrency and idempotency

Per ADR-0008:

- `GET` returns `ETag: "<version>"`. `PATCH` and custom methods require
  `If-Match`; a mismatch is 412 and nothing changes.
- Every `POST` and `PATCH` requires `Idempotency-Key` (8–255 characters,
  a UUID is recommended). A repeat with the same body within 7 days returns
  the original status and body with `Idempotent-Replayed: true`; a different
  body is 422; a repeat while the first is still running is 409 with
  `Retry-After`.
- Keys are scoped to tenant, so two tenants cannot collide.
- A `GET` marked `x-snapshot: true` reads as of one database state: its
  unit of work runs at `REPEATABLE READ` (read-only), so every statement
  sees what the first saw. A report computed from several contexts' reads
  needs it, such as the daily cash position (`getCashPosition`, ADR-0051
  §6); the contract allows the mark on `GET` only (`api/spec.go`).

## 4.8 Long-running operations

Requests that may exceed 5 seconds (payment proposals, migrations,
simulations, exports, period close) return **202** with an `Operation`
(ADR-0012):

```json
{"id": "...", "kind": "payments.propose", "status": "running",
 "progress": {"done": 41000, "total": 100000},
 "resource": "/v1/payment-runs/...", "error": null}
```

`progress.total` is null while unknown; counts are integers. The resource the
operation creates is visible immediately in an in-progress status (for
example a payment run in `proposing`).

`GET /v1/operations/{id}` polls; `POST /v1/operations/{id}:cancel` cancels
where the operation supports it; completion is also published as an event.

A succeeded, failed or cancelled operation is final. Cancelling one is
`409 platform.operation_not_cancellable`; a worker that tries to finish it
again gets the internal conflict `platform.operation_finished` (never
returned to a client) and changes nothing, so the
outcome is never overwritten and completion is published once; progress
reported late is ignored and never brings it back to `running`.

Cancellation stops the work where the operation can stop, not what is
already done. A payment-run release (`:release`, 202) posts one payment per
step: cancelling its operation keeps the payments already posted, writes no
bank file, and leaves the operation `cancelled` and the run `releasing`
(with nothing posted, a refused release returns the run to `proposed`). The
in-process releaser of a cell without a workflow engine keeps posting the
release it is on. The run is resumed with `:release` again, which starts a
new operation and posts only what is left; steps of the cancelled
operation still running then do nothing. While the run is releasing, an
invoice whose payment is not posted yet cannot be reversed
(`409 payables.invoice_in_payment_run`), so the resumed release never pays a
reversed invoice. A payment that can never post (an item reversed, or no
longer open, since the proposal) is dropped by the release rather than
failing it again on every resume: its supplier is excluded with
`item_reversed` or `item_not_open`, its items are freed and
`PaymentExcluded` is published; a release that drops every payment fails
with `payments.nothing_to_pay` and returns the run to `proposed`. A run has
at most one bank file.

## 4.9 Security

- OAuth 2.0 bearer tokens (OIDC) from the tenant's identity provider; the
  gateway validates signature, issuer and audience. Agents and services use
  client-credentials tokens tied to their `principal`.
- The identity provider a tenant registered decides a token's tenant
  (ADR-0031): the cell resolves the token's issuer and audience to the one
  active registration (as `erp_control`, cached 30 s) and the router to the
  directory's claim of them, whatever tenant a claim names; a registered
  tenant claim must name that tenant. An issuer no tenant registered is
  401 and never fetched; a registry or provider that cannot answer is 503
  `platform.identity_provider_unavailable` (keys) or `platform.unavailable`
  (registry). Issuers configured for every tenant (`ERP_OIDC_ISSUERS`) keep
  the tenant claim and are for development: production refuses them, and
  refuses to start without operator issuers or while an active tenant has
  no active identity provider; `erp-server register-provider` registers
  one without a running server, and a production import refuses a tenant
  without one.
- Where the tenant requires multi-factor authentication (its policy on
  the control plane), a person's token must prove it by the provider's
  `acr` or `amr` values; otherwise the answer is 401
  `identity.mfa_required` with the step-up challenge of RFC 9470
  (`WWW-Authenticate: Bearer error="insufficient_user_authentication",
  acr_values="…"`). Services and agents are not asked: the unit of work
  applies the rule once it knows the principal is a person.
- Every operation in the spec declares its permission with
  `x-permission: payables.invoice.post`. The generated router refuses to start
  if a handler has no permission, and a test checks that every permission in
  the spec exists in `ref.permission`. An operation whose resource is read by
  whoever may read an entity (`GET /v1/schemas/{entity}`, ADR-0046) names a
  read permission per entity in `x-entity-permissions` instead. An operation
  that needs further permissions beside its own lists them in
  `x-also-permissions`, and the caller must hold each: the cost and profit
  center balance reports show ledger amounts, so they need the trial
  balance's `ledger.entry.read` beside `ledger.cost_center.read` (ADR-0050).
- Company access is checked on every read and write; resources outside the
  caller's companies are 404, not 403, so ids cannot be probed. A resource
  of no company that holds values of several (a change request's simulation
  and regression evidence, a regression suite's cases) shows each reader the
  values of their own companies alone, with counts over those, failing
  closed; a verdict over every company (the evidence's `passed`) is shown
  whole, as the request's status shows it anyway (03 §3.5).
- Segregation of duties is checked on transitions (for example, the user who
  created a payment run cannot release it).
- A document's journal entry is reversed through the document, never
  around it: `journal-entries/{id}:reverse` refuses a supplier invoice's
  entry (`409 ledger.entry_owned_elsewhere`, naming
  `supplier-invoices/{id}:reverse`), so `ledger.entry.post` cannot stand in
  for `payables.invoice.post`; a goods movement's WE entry is refused the
  same way, naming `goods-receipts/{id}:reverse` (a 102 posts its own
  entry, T33). Entries of contexts with no reversal path yet (receivables,
  assets) are refused the same way, saying so.
- Approval decisions (`:approve`, `:reject` on approval requests) require a
  human principal; agent tokens are refused with `403 identity.agent_not_allowed`
  and the database enforces the same rule (ADR-0010).

## 4.10 Versioning and compatibility

Within `/v1`, only additive changes are allowed: new resources, new optional
fields, new optional parameters, new enum values, new error codes. Removing or
renaming anything, making a field required or changing a type needs `/v2`,
which runs side by side with `/v1` for at least 12 months (ADR-0041).

A deprecated operation says so in the contract: `deprecated: true`,
`x-deprecated-at`, `x-sunset` at least 365 days later and `externalDocs.url`
with its migration notes. Every answer to it carries `Deprecation`
(RFC 9745, `@<unix seconds>`), `Sunset` (RFC 8594) and
`Link: <notes>; rel="deprecation"`, built from the contract.

The breaking-change check in CI enforces this: oasdiff against main with
`--deprecation-days-stable=365`. An accepted break is listed in
`api/breaking-accepted.txt` under a comment naming its T-number and reason;
once the pilot partner consumes `/v1`, only operations marked
`x-stability-level: beta` may be listed.

## 4.11 Limits and quotas

| Limit | Default |
|---|---|
| Requests per tenant | 2,000/s sustained, burst 10,000 |
| Requests per principal | 50/s (agents configurable per profile) |
| Request body | 5 MB (documents are stored in object storage and read back through `GET .../document` and `.../content`; larger ones, up to 50 MB, go through `POST /v1/uploads`: a presigned `PUT` to the store, or a signed path on the API, then `upload_id` in the consuming operation, ADR-0020) |
| Lines per document | 10,000 (larger loads use import operations) |
| `page_size` | 500 |

While quotas are on (the default), every response to an authenticated request
carries `RateLimit-Limit`,
`RateLimit-Remaining` and `RateLimit-Reset` (set by `internal/platform/ratelimit`).
The contract states them once, in `info.description`, and declares them on no
response: declared on each response of every operation, they made the generated
client parse them everywhere, about 430,000 of `api/gen`'s lines, and doubled
the memory of a race build (product owner, 2026-10-26). `api/spec_test.go`
holds the single statement and `internal/app` holds that every response,
success, problem or 429, carries them. `Retry-After` stays declared on the
responses that set it.

## 4.12 Events

Every state change of a business document, and every change to master data
or access that other contexts or external systems consume (partners, bank
accounts, principals, roles, assignments, exchange rates), publishes a domain
event through the outbox (ADR-0007). Settings maintained only through
configuration (accounts, payment terms, number ranges, house banks, approval
policies, custom field schemas) change only by activating a configuration
version, so `ConfigurationActivated` covers them. Internal mechanics
(integration message delivery states, agent session bookkeeping) publish no
events. Drafts are private to their context, so a document's first
event is its submission or release, not its creation as a draft; individual
approval steps are recorded in the audit trail, and the request publishes
`ApprovalRequested` and `ApprovalCompleted`.
Events are CloudEvents 1.0 with `type` `com.example.erp.<context>.<event>.v1`
(for example `com.example.erp.payables.supplier_invoice_posted.v1`),
`subject` = resource id, and the extension attributes `tenantid`, `actorid`
and `traceid`. Payloads carry ids and the changed facts, not whole
aggregates. Delivery is at least once and ordered per resource; consumers
deduplicate on `id` through their inbox table.

Within a `.vN` an event changes only additively: no event type or field
removed, no type changed, no field newly required or no longer required.
A breaking change
publishes `.vN+1` beside `.vN` for 12 months. `cmd/eventcompat` checks this
against main in the breaking-change step (ADR-0041).

External subscribers receive events marked `x-webhook: true` through
webhooks signed per Standard Webhooks (HMAC-SHA256, a `whsec_` secret per
subscription, 24 h overlap on rotation), at most 20 subscriptions per tenant,
retried with backoff for 3 days, ordered per resource (ADR-0041, T50b–g).

## 4.13 Agents

Agents use the public API like any client. Agent tools are generated from the
OpenAPI operations (one tool per operation, with the operation's schema as the
tool input), so agents cannot do anything the API does not allow, and every
agent call carries the same permission, company and SoD checks and the same
audit record. Operations marked `x-agent: false` (approval decisions, user
and role administration) are never exposed as agent tools.

Agents reach models only through their cell's **model gateway**, a third
small contract, [`api/model.yaml`](../../api/model.yaml) (ADR-0036): the
provider's Messages API at `POST /v1/model/v1/messages`, so an agent's SDK
only changes its base URL. The request names a model alias, which the
gateway routes to a model with a recorded price; each call is one row of
the tenant's model call ledger (tokens, cost, never content) and adds to
the agent session it names (`Erp-Agent-Session`). It needs
`agents.model.invoke`, authenticates in a short transaction of its own and
writes the ledger in another after the call: no unit of work is held
while the model works. Each call is held against the agent's monthly
budget and the tenant's cap before it is forwarded, at all of
`max_tokens` and an estimate of its input, and settled at its cost
after: one that does not fit is 429 `agents.budget_exhausted`, with
`Retry-After` at the next month when the budget is spent, or of seconds
when calls in flight hold it. Each line's 80% and 100% are announced
once a month (`BudgetThresholdReached`); since the input is estimated, a
line may end its month past its cap by the estimate's error.
Before forwarding, the gateway redacts the call's text (system prompt,
messages, tool results): IBANs, card numbers and the tax identifiers of
the tenant's countries become `[IBAN]`, `[CARD]` and `[TAX_ID]`, and the
call records how many it replaced. Documents and images go only while the
tenant's AI policy lets them (422 `agents.documents_withheld`).

The tenant's **AI policy** (`getAiPolicy`, `updateAiPolicy`, human-only,
`agents.ai_policy.read` and `.manage`) decides where its aliases route,
which models they may route to, its monthly cap in USD and whether
documents may go to models. Its kill switch has two hands: `enabled`, the
tenant's administrators', and `suspended`, the platform's operators', which
wins: they suspend and resume it on the control plane (`suspendAi`,
`resumeAi` in `api/control.yaml`) with a reason the tenant's administrators
see and events and the audit trail leave out. While either is off, the gateway refuses every call with 403
`agents.ai_disabled`, and the unit of work refuses agents' API requests
with the same code, so an agent stopped mid-run cannot act on the answers
it already has. Each change is audited and published as `AiPolicyChanged`.
An agent acts within its autonomy level: 0 reads, 1 also performs the
operations marked `x-autonomy: propose`, 2 any write its permissions allow
(403 `identity.autonomy_insufficient` above it); every level performs the
operations marked `x-autonomy: read`, which change no business data.
`startAgentSession` answers with the agent's `autonomy_level`, and an agent
offers its model only the tools of that level (ADR-0036).
A proposal that a person takes up as it stands is a domain operation of
its own, as a change request or a draft is: the reconciliation agent
(ADR-0047, T49g) proposes for a bank statement line in its session
(`proposeBankStatementLine`, `x-autonomy: propose`), and a person accepts
or rejects it (`acceptBankStatementLineProposal`,
`rejectBankStatementLineProposal`, `x-agent: false`), and an agent proposes
a memo record of an expected receipt or payment (ADR-0051 §13, T53d:
`proposeMemoRecord`, `x-autonomy: propose`, counted nowhere until a person
takes it up with `acceptMemoRecordProposal` or turns it down with
`rejectMemoRecordProposal`, `x-agent: false`, as every other memo record
write is); free-text advice is a recommendation. Bank statements reach agents with the accounts masked to
their last four characters, as partner bank accounts reach everyone. A
report that names no account, such as a house bank's reconciliation
(`getBankReconciliation`, ADR-0047, T49h: its statements' balances against
the ledger's main bank account, what explains the difference and each
line's match and proposal outcome, paged by statement), reaches agents as
it reaches people, as does the daily cash position (`getCashPosition`,
ADR-0051 §8, T53c), which names house banks by code and the last four
characters of their accounts.
`getAiUsage` reports a month's model calls per principal from the call
ledger: calls, tokens, redactions and cost, summed exact and rounded once,
with the month's total against the tenant's cap.

## 4.14 Phase 1 resources

| Context | Resources | Custom methods |
|---|---|---|
| tenancy | `legal-entities`, `org-units` | `:activate`, `:block`, `:unblock` |
| identity | `principals`, `roles`, `role-permissions`, `role-assignments`, `company-access`, `sod-rules` | `:block`, `:end` |
| configuration | `change-requests`, `config-versions`, `number-ranges` | `:submit`, `:withdraw`, `:activate`, `:simulate` |
| partner | `business-partners`, `bank-accounts`, `partner-changes` | `:block`, `:unblock`, `:submit` |
| ledger | `accounts`, `fiscal-periods`, `journal-entries`, `parked-journal-entries`, `clearings`, `open-items`, `balances`, `exchange-rates` | `:reverse`, `:open`, `:close`, `:lock`, `:post`, `:cancel`, `:unlock` |
| procurement | `requisitions`, `purchase-orders`, `contracts`, `goods-receipts` | `:submit`, `:release`, `:close`, `:cancel`, `:terminate`, `:reverse` |
| payables | `supplier-invoices`, `match-results`, `uploads` | `:capture`, `:match`, `:post`, `:hold`, `:release`, `:cancel`, `:reverse` |
| payments | `payment-runs`, `bank-files` | `:propose` (202), `:submit`, `:release` (202, ADR-0012), `:reject`, `:transmit` (202), `:report` |
| cash | `cash-position` (computed on read, ADR-0051), `memo-records`, `memo-record-proposals`, `memo-record-uploads` (CSV by batch key) | `:cancel`, `:realise` (201 for a series' occurrence's override), `:acceptProposal`, `:rejectProposal` |
| controlling | `plan-categories`, `plan-batches` (by batch key), `plan-lines`, `cost-center-plan-actual`, `profit-center-plan-actual` (computed on read, ADR-0050 §8) | `:lock`, `:unlock` |
| workflow | `approval-requests` | `:approve`, `:reject`, `:cancel` |
| audit | `audit-events` | `:verify` |
| integration | `migration-runs`, `connectors` | `:start`, `:reconcile`, `:enable`, `:disable` |
| platform | `operations` | `:cancel` |

`:match` on a supplier invoice answers the match result with the invoice
flow's `decision` (F12b): `outcome` `auto_post` (clean, gross within the
company's auto-post limit), `review` (above it) or `not_evaluable` (the
gross is out of range in the company's currency; `reason` is the stable
code `amount_too_large`), with a `reason`. Without an average rate on the
invoice date the match is refused (422 `ledger.no_exchange_rate`) and
nothing is recorded. It
is the rule simulations and regression suites apply (03 §3.9): the gross is
converted at the average rate of the invoice date and its exact value, before
rounding to the company's minor units, is compared with the limit, as an item's
price variance is with the absolute tolerance (F13): 50,000.004 EUR is above
a limit of 50,000.00, though `reason` shows amounts rounded. A
match with issues has no `decision` (the invoice is held and decided on its
exception request), and neither do reads of the match history, as the
decision is not stored; the ETag of the `:match` response is that of the
stored result. Clients act on the server's decision rather than
compare amounts themselves; a server without the field leaves the decision
unavailable, not guessed. `:post` of an invoice on a purchase order needs
its latest match to be clean and of the invoice's current version (or an
exception approved at that version, F16): an invoice changed after its
match or its approval is refused (409 `payables.match_required`) until it
is matched, or its exception approved, again (F13). It is also matched
again under its order items' locks, and refused (409
`payables.match_outdated`, left parked) when documents posted on the order
since, a change of the order or of the tolerances leave it with an issue
its clean match, or the match its exception was approved on, did not have
(03 §3.9). `erp invoices`
posts or holds with the ETag it sent to `:match`, which a clean match of a
parked invoice leaves valid, so an edit in between fails with 412 and the
invoice is left parked for the next run rather than acted on unmatched.
A match of a parked invoice whose exception was approved at its current
version records its issues but leaves it parked on that approval: it is not
held again nor its approvers asked again (F18), unless it has an issue the
approvers did not see (a new one, or one whose figures moved since), which
holds it for a new request, as `:post` refuses it. `erp invoices` does not
match such an invoice (`workflow_request_id` set) at all, nor post it: a
match with issues answers no decision and `:post` does not check the
auto-post limit, so the rule cannot tell an approved exception it may post
from one a person reviews; it lists the invoice as left parked for a
person to decide. Once every invoice is settled (and `-agent` has run), a
run in which the server refused a match (422: no exchange rate, no order)
exits 1, naming the invoices, as nothing settles them until a person fixes
the cause; invoices only left parked (404, 412, an exception linked, no
decision known) exit 0 (T30).

## 4.15 Control plane

Tenants are not resources of the public API: the tenant comes from the
token, so no principal can see or create another tenant. The operators'
work on tenants (list the cell's tenants, provision one, suspend, resume,
offboard) is a second, small contract, [`api/control.yaml`](../../api/control.yaml),
served by `erp-server` on the **admin listener** under `/control/v1`,
which is reachable only inside the platform (ADR-0015).

| Operation | Method | Notes |
|---|---|---|
| `listTenants` | `GET /control/v1/tenants` | `status` filter; `order_by` `created_at` (default, newest first) or `name`; cursor pages |
| `createTenant` | `POST /control/v1/tenants` | `Idempotency-Key` required; the tenant, its administrator, roles, legal entities, configuration v1 and, with `identity_provider` (required in production), its identity provider in one transaction; 201 with the tenant and what was provisioned |
| `getTenant` | `GET /control/v1/tenants/{id}` | `ETag` = version |
| `suspendTenant`, `resumeTenant`, `offboardTenant` | `POST /control/v1/tenants/{id}:verb` | `If-Match` required; 409 `tenancy.invalid_transition` outside active → suspended → active, active or suspended → offboarded (final) |
| `rotateTenantKey` | `POST /control/v1/tenants/{id}:rotateKey` | `If-Match` required; a new field-encryption key, every sensitive field re-sealed (ADR-0018) |
| `eraseTenant` | `POST /control/v1/tenants/{id}:erase` | `If-Match` required; an offboarded, exported tenant's data erased before its grace period ends (ADR-0021); `offboarding` on the tenant shows the stages |
| `getTenantFingerprint` | `GET /control/v1/tenants/{id}/fingerprint` | The tenant with its fingerprint: rows per table (outboxes left out) and the audit chain head in one snapshot (ADR-0030); read role; `ETag` = version |
| `listIdentityProviders`, `registerIdentityProvider` | `GET`, `POST /control/v1/tenants/{id}/identity-providers` | The tenant's identity providers (ADR-0031); registering needs `erp:admin` and an `Idempotency-Key`, claims the issuer and audience in the global directory first (409 `identity.provider_taken`, 503 while it cannot be reached), and publishes `IdentityProviderRegistered` |
| `getIdentityProvider`, `updateIdentityProvider` | `GET`, `PATCH /control/v1/tenants/{id}/identity-providers/{provider_id}` | Merge patch of the tenant claim and the `acr`/`amr` values; issuer and audience never change; `If-Match` required; `erp:admin` |
| `retireIdentityProvider` | `POST /control/v1/tenants/{id}/identity-providers/{provider_id}:retire` | `If-Match` required; 409 `identity.last_provider` for the tenant's last active one; publishes `IdentityProviderRetired`; `erp:admin` |
| `listTenantPrincipals` | `GET /control/v1/tenants/{id}/principals` | `binding=unbound` (default: those whose issuer is no active provider of the tenant) or `all`; cursor pages; `erp:read` |
| `bindPrincipal` | `POST /control/v1/tenants/{id}/principals/{principal_id}:bind` | Binds the principal to an active provider with its subject there; `If-Match` required; publishes `PrincipalBound`; `erp:admin` |
| `getAuthPolicy`, `updateAuthPolicy` | `GET`, `PATCH /control/v1/tenants/{id}/auth-policy` | Multi-factor requirement, session lifetime and idle timeout; merge patch with `If-Match`; publishes `AuthPolicyChanged`; `erp:admin` |

The control plane follows this document's rules (problems, pagination,
idempotency, concurrency) with these differences:

- **A production cell needs operators.** It refuses to start without
  `ERP_OPERATOR_ISSUERS`: tenants' identity providers are registered and
  principals bound here (ADR-0031).
- **Callers are operators, not principals.** Tokens come from the operator
  identity providers (`ERP_OPERATOR_ISSUERS`) and must name no tenant; a
  tenant's token is refused, and an operator token is refused by the public
  API. There is no permission model: any operator the verifier accepts may
  call any operation, so the operator identity provider decides who operates.
- **Every change is recorded in the tenant it changes.** A suspension is an
  audit event in that tenant's hash chain with actor kind `system` and an
  id derived from the operator's issuer and subject, and a
  `TenantStatusChanged` event through the tenant's outbox. Provisioning's
  audit entry names the operator in its detail.
- **Reads cross tenants on a read-only role.** Listing runs as `erp_control`
  (`BYPASSRLS`, `SELECT` on `tenancy.tenant` and the idempotency keys only);
  a status change runs in the tenant's scope as `erp_app`, like any
  request of that tenant.
- **The provisioning key lives with the tenant it created.** There is no
  tenant to store an `Idempotency-Key` under before the request runs, so the
  provisioner stores it with the new tenant in the same transaction, under
  a `control|` prefix that is unique across tenants: a retry answers the
  tenant the first request created (`Idempotent-Replayed: true`), and two
  concurrent requests with one key create one tenant (409 for the loser).
- **Offboarding is a final status with a lifecycle.** The worker exports
  the tenant's data to locked object storage and erases it after the grace
  period; `:erase` does it earlier. The tenant row and its audit trail
  stay (ADR-0021).

## 4.16 Global control plane

Across cells, operators use a third small contract,
[`api/directory.yaml`](../../api/directory.yaml), served by `erp-router` on
its admin listener under `/directory/v1` (ADR-0023) with the same operator
tokens as the cells' control planes:

| Operation | Method | Notes |
|---|---|---|
| `listDirectoryTenants` | `GET /directory/v1/tenants` | `region`, `cell`, `status` filters; `order_by` `updated_at` (default, latest first) or `cell`; cursor pages |
| `getDirectoryTenant` | `GET /directory/v1/tenants/{id}` | The entry; `ETag` = version |
| `createDirectoryTenant` | `POST /directory/v1/tenants` | The cell contract's `TenantCreate` plus an optional `cell`; placed in the emptiest accepting cell of the region and forwarded to that cell's control plane with the operator's token and `Idempotency-Key`; the answer names the cell (`X-Erp-Cell`) |
| `freezeTenant`, `moveTenant`, `unfreezeTenant` | `POST /directory/v1/tenants/{id}:verb` | `If-Match` required; the directory steps of a move (05 §5.4): read-only at the router, the entry switched to a registered cell of the same region, active again |
| `verifyTenant` | `POST /directory/v1/tenants/{id}:verify` | The tenant's fingerprint in its cell and in the `cell` named, fetched with the operator's token, with every difference (ADR-0030); changes nothing, so no `If-Match`; 422 for an unknown or the tenant's own cell, 502 for a cell not answering or refusing |
| `listCells` | `GET /directory/v1/cells` | The cells that registered themselves, with tenant counts, fewest first |

The global plane keeps no audit trail: what it forwards is recorded by the
cell that serves it, and what it changes is the directory's routing state.
