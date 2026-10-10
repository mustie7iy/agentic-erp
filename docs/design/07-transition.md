# 7. Transition from the Phase 1 prototype

## 7.1 What Phase 1 is

The code in `internal/` (about 11,000 lines, 100% statement coverage) is a
working prototype of procure-to-pay, configuration as code, the delivery
agents and the SAP migration path. It proved the business rules and the agent
workflows. Its foundation does not meet this design:

| Aspect | Phase 1 prototype | This design |
|---|---|---|
| Storage | `core.Store`: in-memory maps saved as one JSON snapshot | PostgreSQL, schema per context, migrations in `db/` |
| Concurrency | One process-wide mutex (`Store.Mu`) | Transactions, optimistic locking, row-level constraints |
| Tenancy | Single tenant | `tenant_id` on every row, RLS, cells |
| Identifiers | Prefixed strings from a counter (`PO-000012`) | UUIDv7 ids plus document numbers from number ranges |
| Interface | CLI and agent tools calling Go functions | Public API from `openapi.yaml`; CLI and agents are API clients |
| Ledger | Mutable journal entries in a map; balances computed by scanning | Insert-only, partitioned journal; clearing table; derived balances |
| Events | None | Outbox per context, CloudEvents |
| Audit | In-process log | Hash-chained `audit.audit_event` in the same transaction |
| Packages | Organized by feature (`core`, `p2p`, `workflow`) | Organized by bounded context with domain/app/postgres/api layers |

## 7.2 Principle: keep the rules, replace the foundation

The prototype's valuable part is its **business logic and its tests**, not its
storage. The transition moves rules into context `domain` packages
unchanged where possible, puts repositories and the API around them, and uses
the prototype's tests as the first acceptance tests of the new code.

| Prototype package | Moves to | How |
|---|---|---|
| `money` | shared kernel `internal/money` | Kept; add decimal-string API encoding (04 §4.4) |
| `core` (matching, tolerances, index) | `payables/domain` | Rules kept; `Store` lookups become repository ports |
| `core` (ledger, posting) | `ledger/domain` | Becomes the posting service that builds balanced lines; persistence is insert-only |
| `core` (procurement) | `procurement/domain` | Kept; goods-receipt limits are also enforced by the database |
| `p2p` | `procurement/app`, `payables/app`, `payments/app` | Split by context; each use case becomes one transaction with outbox and audit |
| `workflow` | `workflow/domain` + `workflow/app` | Rules kept (no self-approval, human-only decisions); state in `workflow.*` tables |
| `governance` | `identity/app` (SoD, permissions) and `audit/app` | Runtime SoD checks move to the authorization layer |
| `config` | `configuration/domain` | Configuration documents become `config.config_version` rows; validation kept |
| `bankfile` | `payments/adapters/bankfile` | Kept as an outbound adapter |
| `capture` | `payables/adapters/capture` | Kept; captured documents go to object storage, hash in `capture_sha256` |
| `sap`, `migration` | `integration` | Extract readers kept; loaders write through the application services, never into tables directly |
| `delivery/*` agents | `agents` | Tools regenerated from OpenAPI operations instead of calling Go functions |
| `analytics` | `analytics` | Semantic layer kept; reads read models instead of the snapshot |
| `llm`, `llmtest` | `internal/llm` | Kept |
| `nfr`, `testkit` | Kept | Fixtures rebuilt on the database; NFR budgets re-baselined |

## 7.3 Sequence

Each step ends with CI green and nothing in production depending on the
prototype store for that context.

| Step | Work | Exit criterion |
|---|---|---|
| T0 | **Design approval**: this package reviewed, assumptions confirmed, ADRs accepted | Review checklist in README signed off |
| T1 (done) | **Platform skeleton** (ADR-0014): config, pgx pool with `SET LOCAL` tenant/actor per transaction, error translation, idempotency middleware, ETag handling, outbox writer and relay, audit writer, OpenAPI code generation, migration runner, health checks, telemetry | A generated "operations" endpoint works end to end with authn, idempotency and audit |
| T2 (done) | **Tenancy, identity, configuration** contexts behind the API | Tenants, legal entities, principals, roles, number ranges managed only through the API; authz and SoD enforced |
| T3 (done) | **Partner and ledger** | Journal entries posted and reversed through the API; projector maintains balances; trial balance meets its budget |
| T4 (done) | **Procurement, payables, workflow** | Requisition → PO → receipt → invoice → match → post through the API, with approvals |
| T5 | **Payments** | Proposal as an operation, release with SoD, bank file, clearing |
| T6 | **Agents and integration** onto the API | Delivery agents and SAP migration use generated API tools only; prototype store deleted |
| T7 | **Hardening** | Load, soak and resilience suites pass at the targets in 01 §1.3 |

The prototype stays runnable as a demo until T6, then `core.Store` is
removed. The CLI becomes a thin API client.

### T2 decisions and open points

What T2 built: `internal/tenancy` (14 operations), `internal/identity` (24)
and `internal/configuration` (13) on the T1 pipeline; company access
(`authz.Companies`, 404 outside) and run-time segregation of duties in
`authz.Authorize`; keyset pagination for every sortable list.

Decisions taken while building it:

- **Grants are day-granular.** `valid_from` is never in the past (the
  access history must stay reconstructable) and `valid_to` must be after
  it, so a grant made today runs at least until tomorrow. Immediate removal
  of access is `principals/{id}:block`.
- **SoD is enforced twice.** Role assignments, permission grants and new
  rules are refused (409 `identity.sod_conflict`) if any principal would hold
  both permissions of a rule on any day from today on; conflicts that exist
  anyway block both permissions at run time (403).
- **Agent approvals**: the database's deferred constraint trigger is made
  immediate after each grant and now names itself (migration 000013), so the
  answer is 409 `identity.agent_approval_permission` from the request, not a
  failed commit.
- **Nobody blocks themselves** (409 `identity.cannot_block_self`).
- **Change requests are validated synchronously.** A draft that fails
  validation is answered 422 `config.validation_failed` with one field error
  per finding and stays a draft, because the contract's `ChangeRequest` has
  no field for findings. `validating` and `invalid` stay reserved for
  asynchronous validation.
- **Number ranges live in the configuration document** (`number_ranges`,
  keyed by company code, document type and fiscal year) and are applied to
  `config.number_range` on activation. Only new ranges and raised upper
  limits pass submission, so they are tier 1 (no approval), as SAP treats
  number range intervals as current settings.
- **Approvals wait for T4.** Tier 1 changes are approved on submission;
  tier 2 changes stop in `pending_approval` and tier 3-4 changes in
  `awaiting_evidence` (`:simulate` starts a `config.simulate` operation that
  no worker runs yet) until the workflow context and the simulation worker
  exist.
- Contexts call each other through small ports answered in the request's
  transaction (`tenancy.Directory` for identity and configuration), never
  through each other's tables.

Open points for the contract and design review:

- There is no API to create a tenant or its first administrator, so the
  T2 exit criterion "tenants managed only through the API" is met for
  legal entities, principals, roles and number ranges only.
  `configuration.Bootstrap` installs version 1 inside a provisioning
  transaction; an operator provisioning command is still to be designed.
- The per-tenant identity provider registry that ADR-0014 expected with
  the identity context is not built: there is no table or operation for it
  in the contract, so trusted issuers still come from deployment
  configuration.
- No event announces a legal entity or org unit leaving `inactive`
  (`:activate`); `LegalEntityUnblocked` is documented as emitted by
  `:activate` but is emitted by `:unblock`.
- `Extensions` are checked for snake_case keys only; validation against
  published custom field schemas arrives with them (ADR-0009).
- The configuration document still carries the prototype's roles, users
  and SoD pairs, which the identity context now owns; they move out when the
  delivery agents move onto the API (T6).

### T3 decisions and open points

What T3 built: `internal/partner` (12 operations) and `internal/ledger` (18
operations) on the T1/T2 pipeline; the configuration document's ledger and
payment-term sections applied on activation (`configuration.Section`);
field encryption for bank accounts and tax identifiers
(`internal/platform/fieldcrypt`, `ERP_FIELD_KEY`); the balance projector,
run by `erp-server` as role `erp_projector`; migration 000014.

Decisions taken while building it:

- **Ledger master data comes from the configuration document.** The
  contract makes accounts read-only, so activation applies each company's
  chart (one chart per company, coded with the company code), its accounts
  (removed ones become inactive; type and reconciliation kind never change
  once activated), account determination, and the leading ledger 0L (IFRS)
  for a tenant's first version. Companies without a legal entity, a
  currency other than the legal entity's, or a start month contradicting
  the legal entity's calendar are refused on submission.
- **Fiscal years.** The legal entity's `fiscal_calendar_id` names its
  calendar (12 normal periods and 4 special periods sharing the last
  period's dates). Fiscal years are named after the calendar year they end
  in (start month 4: fiscal year 2026 runs from 1 April 2025). A year is set
  up for a company when the document has a number range for it; its periods
  are created closed in every ledger and opened with `:open`.
- **Document numbers** come from the number range `journal_<type>`
  (`journal_sa`, clearing entries `journal_ab`); without one a posting is
  422 `config.no_number_range`, an exhausted one 409
  `config.number_range_exhausted`.
- **Conversion.** Local and group amounts use the latest average rate valid
  on the posting date in either direction (an inverse rate is rounded to 12
  decimals and stored); amounts round half away from zero and the rounding
  difference goes to the largest line, per currency. Reversals copy the
  original rates and negate every amount; they post on the original date
  unless another is given, and refuse reversals of reversals and entries
  with cleared lines (resetting a clearing is not in the contract).
- **Partners only on reconciliation lines**, checked through the partner
  context (`partner.Postings`): active, playing the role, and for suppliers
  set up and not posting-blocked in the legal entity. Tax codes on lines
  must exist in `ledger.tax_code`, which nothing maintains yet (see below).
- **Clearing** requires open items of one legal entity, ledger and
  transaction currency that net to zero per partner in all three
  currencies. The database needs a balanced clearing entry, so the entry
  (type AB) offsets every item on its account and its lines are cleared
  with the items; each reconciliation account keeps the balance of its open
  items. Differences (cash discount, FX) arrive with payments in T5.
- **Balances are exact without waiting for the projector.** Posting queues
  the entry in `ledger.balance_projection` next to its `JournalEntryPosted`
  event; the projector adds a batch to `period_balance` and marks it
  projected in one transaction (exactly once, `SKIP LOCKED` across
  replicas). `GET /v1/balances` adds the entries still queued, so it is the
  trial balance at the moment of the read. A trial balance of 10,000 rows
  (5,000 accounts, two currencies, three years of history, 2,000 pending
  lines) reads in about 0.45 s p95 against the 1 s budget; CI runs this
  budget without the race detector.
- **Partner changes wait for T4.** Create, update, bank and unblock changes
  stay `pending`; `partner.ApplyChange`, `RejectChange` and `CancelChange`
  are what the workflow context will call. Blocking applies at once. Applying
  late moves a requested `valid_from`/`valid_to` that has passed to the
  application day. Partners are numbered BP + 8 digits, changes PC + 8 digits.
- **Sensitive values** are sealed with AES-256-GCM bound to the tenant and
  fingerprinted with a keyed HMAC to warn about duplicate tax identifiers
  and bank accounts across partners; responses show only the last four
  characters, never ciphertext or fingerprints.
- Migration 000014 also adds what the contract needed from the schema: an
  index to read entries by id, the end-validity trigger on addresses (a new
  address ends the current one), and change columns on tax identifiers
  (replaced in place).

Open points for the contract and design review:

- Tax codes in the configuration document are not applied: `ledger.tax_code`
  is insert-only without an end-validity rule, so a rate change cannot end
  the current rate. They arrive with payables (T4) together with that rule.
- An end-only bank change publishes `BankAccountChanged` with the ended
  account as `bank_account_id`; the contract describes only added accounts.
  `PartnerChangeCancelled` has no operation yet (`CancelChange` exists).
- `PeriodClosed` is described as following an operation, but
  `:close` answers 200 synchronously.
- Projector lag is not yet exported as a metric (05 §5.6 expects an alert).
- Field encryption uses one deployment key; per-tenant keys in a KMS and
  key rotation (the sealed format is versioned) are still to come.

### T4 decisions and open points

What T4 built: `internal/approvals` (the workflow context, 5 operations),
`internal/procurement` (23) and `internal/payables` (12) on the pipeline;
approval requests for partner changes and tier 2 configuration changes;
tax codes applied from the configuration; ledger posting and reversal for
other contexts (`ledger.Postings`); migration 000015. The exit criterion is
`TestProcureToPay` (internal/platform/httpapi): requisition, two-step
approval, order, receipt, invoice, clean match and posting, then an
over-invoiced item held, decided as an exception and posted.

Decisions taken while building it:

- **The workflow context is `internal/approvals`**: the prototype's engine
  keeps `internal/workflow` until T6, as configuration sits next to the
  prototype's `config`.
- **Approvals run in the owning transaction.** The design routes them
  through events (RequisitionSubmitted opens a request, ApprovalCompleted
  moves the object on); in the modular monolith the owning context opens
  the request through a port and is called back (`approvals.Handler`) in
  the deciding transaction, so the decision and its effect commit together.
  The events are still published.
- **Approval policy is the active configuration document**: requisitions by
  amount tier (`purchasing.requisition_approval`, compared in the
  requisition's currency), partner changes by
  `payments.vendor_approver_role`, configuration changes by tier
  (`config.ApprovalSteps`), invoice exceptions by `process_owner`. Roles are
  identity roles whose code is the document's role name; a missing role
  refuses the request (422 `workflow.unknown_role`). The
  `workflow.approval_policy` tables are not used yet and `policy_id` stays
  empty.
- Deciding needs `workflow.request.decide`, the object type's approval
  permission and the current step's role; requesters never decide, nobody
  approves twice, only the requester withdraws. When an owning context
  cancels or decides the object itself (a cancelled requisition, an invoice
  released by hand), its pending request is withdrawn without a decision.
- **Draft items are edited in place**: PATCH replaces the item list by
  updating items 1..n, adding new ones and marking the rest removed
  (`is_removed`, migration 000015, guarded by a trigger that allows it only
  while the document is a draft or parked). Nothing is deleted.
- **Document numbers** come from number ranges `requisition`, `contract`,
  `purchase_order`, `goods_receipt` and `supplier_invoice` (invoices post as
  journal type KR, range `journal_kr`), in the fiscal year of the document
  date (today for requisitions and contracts).
- **Goods receipts do not post to the ledger**; invoices post expense on the
  order item's account directly (Dr expense, Dr input tax, Cr the supplier's
  reconciliation account). GR/IR clearing arrives with inventory valuation.
- **Tax codes** from the document (input tax) are valid from the company's
  first fiscal year; a changed rate or account ends the current code and
  starts the new one on the activation day; a second change on the same day
  is refused on submission. Invoices post one input tax line on the first
  item tax code's account.
- **Three-way match** compares quantities with the received quantity minus
  quantities on other posted invoices, and prices with the larger of the
  company's price percentage and absolute tolerance; results record the
  configuration version. A non-clean result holds the invoice and opens an
  `invoice_exception` request; an approved exception (the request id kept on
  the parked invoice) allows posting without a clean match. An invoice
  refers to one purchase order.
- **Capture** extracts with Claude (`capture.ClaudeExtractor`) when
  `ANTHROPIC_API_KEY` is set and answers 503 otherwise; the operation
  completes within the request (`operations.Succeed`); the document must
  print a purchase order number, and low-confidence extractions are entered
  on hold.

Open points for the contract and design review:

- Purchase orders are not closed (`PurchaseOrderClosed`) when fully received
  and invoiced, and contracts are not expired by a job (orders refuse
  contracts past `valid_to`).
- Captured documents are not stored: `capture_object_ref` is
  `sha256:<hex>` until object storage exists.
- Reversing a posted invoice does not yet check live payment runs (T5).
- `SupplierInvoiceCaptured` carries `on_hold` for low-confidence captures;
  the contract says `parked` in v1.
- Tier 3 and 4 configuration changes still wait for simulation evidence
  before their approval request is opened.

## 7.4 Data from the prototype

Prototype snapshots are demo data, not production data, so there is no data
migration from the JSON store. SAP is the production source: real client data
arrives through the migration path in `integration` (reconciled loads through
the API), exactly as it will for every client.

## 7.5 Tracker

`docs/tracker.json` tracks the transition: module 14 (non-functional and
infrastructure) holds the platform items for T1, and each functional module
gets "on foundation" items for its context's step. A Phase 1 feature counts as
done again only when it runs on the foundation through the API.
