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
| T5 (done) | **Payments** | Proposal as an operation, release with SoD, bank file, clearing |
| T6 (done) | **Agents and integration** onto the API | Delivery agents and SAP migration use generated API tools only; prototype store deleted |
| T7 (done) | **Hardening** | Load, soak and resilience suites pass at the targets in 01 §1.3 |
| T8 (done) | **Operations**: provisioning and local login | A tenant is provisioned by an operator and run through the API from the command line, with a development issuer locally |
| T9 (done) | **Simulation** | Tier 3 and 4 configuration changes get simulation evidence from a worker that replays recent invoices under the proposal, and move on to approval |
| T10 (done) | **Regression suites** | Suites captured from real invoices through the API; the simulation replays the base version's latest suite under the proposal and records regression evidence |
| T11 (done) | **Control plane** | Operators list, provision, suspend, resume and offboard tenants through a contract on the admin listener, with operator tokens; every change recorded in the tenant's audit trail and published |
| T12 (done) | **Object storage** | Captured documents and bank files kept in write-once object storage and served through the API; the audit chain anchored to locked objects and verified against them |
| T13 (done) | **Secrets** | Settings and connectors name their secrets by reference (environment, mounted file, Vault) and the server resolves them at start and on use; the repository is scanned for leaked secrets |
| T14 (done) | **Key management** | Every tenant's sensitive fields are sealed with the tenant's own key, wrapped by a ring of master keys; operators rotate a tenant's key and the fields are re-sealed in one transaction |
| T15 (done) | **KMS master** | The master key that wraps tenant keys lives in a key management service; existing tenant keys move onto it without rewriting a sealed row, and the local masters it replaces leave the ring |
| T16 (done) | **Direct uploads** | Documents above the request limit reach object storage through an upload URL (presigned on the store, or signed on the API) and are captured by id; the invoice's document is kept and served as before |
| T17 (done) | **Offboarding lifecycle** | An offboarded tenant's data is exported to locked object storage, kept for a grace period and erased from the cell by a worker or on an operator's request; the tenant row and its audit trail remain |
| T18 (done) | **Tenant directory and router** | A global directory maps tenants to cells and is kept current by the cells; `erp-router` verifies a request's token, looks its tenant up and forwards it to the tenant's cell, refusing unknown, suspended, offboarded and (for writes) moving tenants at the door |
| T19 (done) | **Global control plane** | Operators see tenants and cells across the platform, provision a tenant into the emptiest accepting cell of its region (or a named one) through the cell's control plane, and move a tenant between cells in three directory steps |
| T20 (done) | **Ambient cloud credentials** | The S3 store and the KMS master sign with the deployment's role credentials (environment, web identity, instance metadata) when no access keys are set; session tokens are signed; temporary credentials are renewed before they expire |
| T21 (done) | **Scheduled audit verification** | A worker recomputes every tenant's audit chain against its anchors every hour, records each round, and raises an alarm (log, event, gauge) once when a chain stops verifying |
| T22 (done) | **Chunked exports** | Offboarding exports stream each table into objects of bounded size, listed part by part in the manifest; audit details stay as written, with minimization a write-time rule rather than a rewrite |
| T23 (done) | **Operator roles, rotation policy, security lint** | Operator tokens carry roles the control planes check per operation; a worker rotates tenant keys older than a configured age; the API's own upload URLs can be absolute; `gosec` runs in CI with its exclusions decided and recorded |
| T24 (done) | **Re-onboarding from export** | `erp-server import` brings an exported tenant back into a cell, row for row and chain head and all, every part checked against the manifest; the row copy a move between cells needs |
| T25 (done) | **Cross-cell verification** | Each cell answers a tenant's fingerprint (rows per table, audit chain head); the global control plane compares the tenant's cell with the destination and names every difference, the evidence a move waits for before switching the entry |

The prototype stayed runnable as a demo until T6, which removed
`core.Store` and made the CLI a thin API client.

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
- `SupplierInvoiceCaptured` carries `on_hold` for low-confidence captures;
  the contract says `parked` in v1.
- Tier 3 and 4 configuration changes still wait for simulation evidence
  before their approval request is opened.

### T5 decisions and open points

What T5 built: `internal/payments` (10 operations) on the pipeline; house
banks applied from the configuration document (`payments.HouseBanks`); the
proposal worker `payments.Proposer`, run by `erp-server`; payment posting and
clearing in the ledger (`ledger.Postings.Settle`, `ledger.OpenItems`); the
partner context's payee port (`partner.Payees`); migration 000016. The exit
criterion is `TestProposeReleaseTransmit` (internal/payments): invoices and a
credit memo posted through payables and the ledger, a proposal built by the
worker, release by a person other than the proposer, the payment entry
clearing the items, and the bank file transmitted.

Decisions taken while building it:

- **Proposals run in a worker until Temporal arrives** (ADR-0012).
  `:propose` creates the run in `proposing` with its `payments.propose`
  operation (whose `resource` is the run from the start) and answers 202.
  `payments.Proposer` finds queued proposals across tenants as role
  `erp_worker` (it may read only the operation and run columns it needs)
  and builds each one in one transaction of its tenant as `erp_app`, acting
  as the proposer. Proposals of one legal entity are serialized with an
  advisory lock. A problem while building (for example no active
  configuration) fails the run and its operation; other errors leave the
  job queued for the next round. A cancelled operation fails its run in the
  next round. 100,000 items are proposed in about 1.3 s against the 60 s
  budget (`TestProposalBudget`, `make perf`).
- **House banks come from the configuration document.** Each company's
  `house_bank` becomes `payments.house_bank`: its name is its code, its
  currency the company's, its ledger account the account determination's
  `bank` account, its IBAN sealed like partner bank accounts. IBAN and BIC
  never change under the same name, so released files stay reproducible; a
  new bank account gets a new name and the house bank it replaces becomes
  inactive. The run currency is the house bank's, so runs pay in local
  currency.
- **Selection.** A run takes the legal entity's open items on supplier
  reconciliation accounts of the leading ledger in the run currency, due on
  or before `due_by` and not held by an active item of another run. The due
  date is the document date plus the net days of the supplier's payment
  terms in the legal entity (SAP's default baseline date); terms have no
  cash discount yet.
- **Suppliers are paid net.** Credit memos and other debit items due are
  settled with the invoices at a negative amount (migration 000016 allows
  non-zero item amounts; the payment stays positive). A supplier whose due
  items net to zero or a debit is excluded with `debit_balance`. The other
  exclusions, in order: `supplier_blocked` (partner not active, or blocked
  for posting), `payment_blocked`, `no_bank_account` (none valid on the run
  date), `bank_change_pending`, and `bank_change_hold`: an account that came
  from a change after the partner's creation pays only
  `payments.bank_change_hold_days` after its `valid_from`.
- **Release** is the human-only F110 step of the contract, not an approval
  request (`workflow_request_id` stays null on the events). The proposer is
  refused with 403 `identity.sod_conflict` before the database's
  `payment_run_sod` constraint would. Every supplier is checked again on the
  run date, including that its bank account is still the proposed one; a
  supplier that can no longer be paid refuses the whole release with 409
  `payments.proposal_outdated` (reject and propose again), and a run without
  payments is 409 `payments.nothing_to_pay`. A run is posted completely or
  not at all.
- **The payment entry is the clearing document.** Each payment posts one
  entry (document type ZP, range `journal_zp`, on the run date, reference
  the end-to-end id) that offsets each item at its original amounts in all
  three currencies, credits the house bank's account at the run date's
  rates, and books what that leaves in local or group currency on the new
  optional account determination keys `fx_gain` / `fx_loss` (the schema had
  them; a difference without the account is 422 `ledger.no_fx_account`).
  The items and the entry's partner lines are cleared with it.
- **Numbers and ids.** Runs are numbered from the `payment_run` range of the
  run date's fiscal year; end-to-end ids are `<company code>-<run
  number>-<sequence>`.
- **Bank files** are pain.001.001.03 (the writer now takes the currency's
  minor-unit digits), one per run, message id `<company code>-<run
  number>`, each payment telling the supplier the references (else document
  numbers) of the items it settles. Until object storage exists the file is
  not stored: `object_ref` is `sha256:<hex>`, the file is stamped with the
  release transaction's time (its creation time) and is rendered again on
  `:transmit`, which refuses it with 409 `payments.bank_file_changed` when
  the hash no longer matches.
- **Transmission** is an operation that completes within the request,
  through a `payments.Connector`. `erp-server` drops files into
  `ERP_BANK_OUTBOX_DIR` for a transfer agent (written under a hidden name,
  then renamed); without it `:transmit` answers 503. A connector failure is
  503 with `Retry-After` and leaves the file `created`.
- **Invoices in a live run cannot be reversed** (409
  `payables.invoice_in_payment_run`), which closes T4's open point.
- `erp-server` runs the worker with `ERP_DB_WORKER_ROLE` (default
  `erp_worker`), which the login role must be granted like `erp_relay` and
  `erp_projector`.

Open points for the contract and design review:

- `:release` answers synchronously (200 in the contract), and releasing
  2,000 payments over 100,000 items takes about 26 s locally (one entry and
  clearing per payment). Large releases should become an operation, or
  postings be batched, before tenants reach that size.
- There is no operation for bank status reports (pain.002, camt.054), so
  `acknowledged` and `rejected` bank files, `BankFileAcknowledged`,
  `BankFileRejected` and `PaymentRunFailed` for released runs are not
  produced yet.
- A proposal reports its progress only when it completes (one
  transaction); chunked, resumable proposals wait for Temporal.
- The configuration document has one house bank per company and no payment
  methods; cash discount and partial payments are not modelled.
- Payments cannot be reversed (resetting a clearing is not in the
  contract); a paid item keeps its active payment item.
- If the transaction fails after the connector took a file, retrying sends
  it again; banks refuse a message id they already processed, but a
  connector should confirm before commit once integration messages exist
  (`integration_message_id` is null).
- Bank files and captured documents still wait for object storage.

### T6 decisions and open points

What T6 built: `internal/integration` (11 operations: connectors and
migration runs), `internal/agents` (10 operations: agent sessions, their
tool calls, recommendations and their decision) and `internal/audit` (2:
audit events and chain verification) on the pipeline; `internal/apitools`,
the runtime that gives agents the API as tools; migration 000017. The
invoice exceptions agent, the configuration, migration and test agents and
analytics were rewritten as API clients, and the prototype was deleted:
`internal/core`, `migration`, `governance`, `p2p`, `workflow`, the
simulation, regression and change-evidence packages, and the roles, users
and SoD pairs of the configuration document. The exit criterion is
`TestEndToEnd` (cmd/erp): through the CLI against a real server, a mock SAP
scope is migrated and reconciled through the integration API, the invoice
rule runs, all four agents work through API tools only (scripted model), and
analytics answers from the data the API returns.

Decisions taken while building it:

- **Agents use the public API, nothing else.** `apitools.Tools` builds one
  tool per operationId from the embedded contract: parameters and the
  request body become the input schema (references inlined, read-only
  properties dropped, open enumerations described); operations marked
  `x-agent: false` are refused. Each agent names the operations it may use.
  `apitools.Operation` gives people (the CLI) any operation, approvals
  included; the API still refuses agents those. Results go back to the
  model as status, ETag and body, truncated at 40,000 bytes.
- **Sessions are recorded through the API.** An agent opens a session
  (`startAgentSession`, agents only), every tool call is recorded in order
  before its result reaches the model, and the session ends completed,
  refused or failed with the tokens it used. Agents act by
  `createRecommendation` or by proposing change requests; deciding a
  recommendation is a person's approval (`agents.recommendation.decide`).
  The invoice agent skips invoices that already have an open
  recommendation; the migration agent's guard lets it start rehearsals only.
- **Migration loads through the owning contexts' services**, inside the
  request transaction: partners (`partner.Migration`), orders and receipts
  (procurement), parked invoices (payables), journal entries and clearings
  (ledger). Each record runs in a savepoint; a refusal (4xx) is reported
  and the record rolled back, an error (5xx) fails the request. Any
  refusal or quality error fails the run as a whole and nothing stays. An
  external reference per record (`integration.external_ref`, keyed by the
  connector's `settings.system`, default `SAP`) makes runs idempotent: a
  run of a scope already loaded loads nothing again.
- **Runs complete within the request.** `:start` (operation
  `integration.migrate`) extracts, runs the quality gate and loads;
  `:reconcile` (`integration.reconcile`) recomputes the controls from the
  SAP extract against the platform. A rehearsal loads, reconciles and rolls
  back to a savepoint, keeping only its report. An extraction failure is
  503 and leaves the run queued.
- **What is migrated.** Posted SAP invoices are carried by their FI
  documents (ACDOCA), not loaded as supplier invoices again; parked ones
  become parked invoices. Movement type 102 (receipt reversal) is not
  migrated: the quality gate reports it. Migrated clearing
  (`ledger.Postings.Clear`) requires the items to net to zero in
  transaction and local currency; group-currency rounding is accepted. A
  payment cleared by a document outside the scope is cleared by the latest
  of its items' documents. Balances mode posts one opening entry per
  company (document type OB, range `journal_ob`) with open vendor items
  kept individually.
- **Connector secrets are references.** `secret_ref` is `env:NAME`,
  resolved by `integration.EnvSecrets` in `erp-server`, standing in for a
  vault. `sap_file` connectors read only below `ERP_EXTRACT_DIR` (local
  paths); without it they answer 503.
- **The configuration document no longer has roles, users, agents or SoD
  pairs** (closing T2's open point); identity owns them. The document keeps
  role names in approval steps, and `sapconfig` turns SAP's role design into
  notes for the people who set up identity.
- **The CLI is a thin client** (`ERP_API_URL`, `ERP_TOKEN`,
  `ERP_AGENT_TOKEN`): `call` for any operation, `migrate`, the invoice rule,
  the agents and analytics. Only preparing input stays local: the mock SAP
  extract and a configuration draft from SAP customizing.
- **Analytics runs client-side over the API.** `analytics.Source` reads
  legal entities, partners, accounts, open items, journal entries, orders
  and invoices with the person's token, so row-level security is the API's
  company access; the semantic layer and query engine are unchanged.
  Queries are no longer written to the audit log, which they were in the
  prototype; that returns when analytics gets an API.
- **The non-functional suite was re-baselined.** The prototype's
  concurrency, reliability and security tests went with the store; those
  properties are now tested per context on PostgreSQL (`EveryFailure`,
  company access, SoD). `internal/nfr` keeps the budgets of the client-side
  engines (analytics queries, configuration validation).

Open points for the contract and design review:

- Migration runs complete within the request, like the other operations
  that do not yet have a worker; a large scope needs the run to move to a
  worker (Temporal, ADR-0012) before tenants reach that size.
- The prototype's sandbox simulation and regression suites have no API, and
  no worker runs `config.simulate`, so tier 3 and 4 configuration changes
  still wait for evidence; the test agent writes test scripts instead of
  replaying suites.
- There is no tenant bootstrap or development identity provider, so the
  CLI against a live server needs an OIDC issuer and a provisioned tenant;
  `make demo` runs the walkthrough as a test.
- `EnvSecrets` is a stand-in: connector credentials need a vault.
- Agents are tested against a scripted model only; evals against the live
  API come before any agent gets autonomy above L0.

### T7 decisions and open points

What T7 built: the non-functional suites of 05 §5.8 in `internal/nfr`
(`make load`, `make soak`, `make resilience`; the nightly workflow runs
them long) over a harness that runs the platform the way `erp-server` does:
the public API on a real listener over a configured tenant, the balance
projector and the outbox relay to an embedded JetStream, and virtual users
with a principal each. Hardening the suites asked for: request quotas per
tenant and principal (04 §4.11), a request deadline and pool sizing that
shed load as 503 instead of queueing, lost database connections answered
as retryable 503s, backlog gauges for the workers (05 §5.7), and partition
maintenance with a readiness check (05 §5.3, §5.7). The exit criterion is
`TestLoad`, `TestSoak` and the four `TestResilience*` tests passing against
the budgets of 01 §1.3; locally (16 users, PostgreSQL in Docker) the mix
runs at about 500 requests/s with read p95 27 ms (budget 50), list 30 ms
(150), posting 175-180 ms (200), approval 34 ms (150) and trial balance
28 ms (1 s), and recovery from a connection loss takes about 50 ms.

Decisions taken while building it:

- **Rate limits are token buckets per replica** (`internal/platform/ratelimit`):
  a bucket per tenant (default 2,000/s, burst 10,000) and per principal
  (default 50/s, burst 100; `ERP_RATE_LIMIT_TENANT`,
  `ERP_RATE_LIMIT_PRINCIPAL`, `off` to disable). The principal's bucket is
  checked first, so a hot principal's refusals do not count against its
  tenant. Every response carries `RateLimit-Limit`, `RateLimit-Remaining`
  and `RateLimit-Reset` for the tighter quota; a refusal is 429
  `request.rate_limited` with `Retry-After`, answered before the unit of
  work opens a transaction. With n replicas a caller gets up to n times the
  quota until limits move to the gateway.
- **Load is shed, not queued.** Each request has a deadline
  (`ERP_REQUEST_TIMEOUT`, default 30 s) and the pool a size
  (`ERP_DB_MAX_CONNS`, default the driver's); a request that cannot get a
  connection or finish in time is a 503 with `Retry-After`, and the unit
  of work buffers responses until commit, so nothing is half-written.
- **A lost connection is a 503 the client retries.** `problem.From` maps
  class 08 and 57P0x errors, connection errors and network faults to
  `platform.unavailable` with "retry with the same Idempotency-Key"; the
  resilience suite found them answered as 500. Nothing of the aborted
  transaction survives, and the idempotency key makes the retry safe.
- **Backlogs are read on each scrape** (`telemetry.Metrics.Gauge`):
  `erp_outbox_backlog`, `erp_projection_backlog` and
  `erp_payment_proposals_queued`, counted by the role that drains each
  queue; a failed read counts in `erp_gauge_read_errors_total`.
- **Partitions are maintained by the server.** `db.EnsurePartitions`
  creates audit months six months ahead and journal years three years
  ahead at start and every six hours (`db.Horizon` measures the horizon);
  readiness fails below three audit months or two journal years, so a
  horizon that stopped growing shows before a posting fails.
- **The suites are tests, not tools.** They are plain `go test` runs sized
  by environment variables (`ERP_LOAD_USERS`, `ERP_LOAD_DURATION`,
  `ERP_LOAD_RPS`, `ERP_SOAK_DURATION`, `ERP_SOAK_WINDOW`), so they use the
  same fixtures and assertions as the functional suites. Load and soak
  skip under `-short` and the race detector; the resilience tests run in
  `make test`. The load test asserts the p95 and p99 budgets of every
  operation and that the workers drained; the soak asserts no failed
  request, p95 drift within 1.5× of the first window or within budget,
  stable goroutines and heap, and workers keeping up; every suite ends with
  the consistency check (each reference posted once, every 201 in the
  journal, projected balances equal to the journal, audit chain intact).
- **Resilience cases.** Killing every database connection under load
  (`pg_terminate_backend`, as a failover does): requests fail as 503 for
  tens of milliseconds, failed posts retried with their key post once, and
  recovery is measured against the RTO. Stopping the JetStream node:
  events wait in the outbox and are in the stream exactly once after the
  node returns. Killing workers: the projector restarted every 100 ms
  mid-batch projects each entry once; a relay that crashes after
  publishing republishes and JetStream de-duplicates; a proposal worker
  that crashes mid-build (a failing transaction injected) leaves the run
  for the next round, which builds it once. Shutting the server down under
  load: every request gets a complete answer or a connection error.

Open points for the contract and design review:

- The budgets are met at the rate a laptop reaches (about 500 requests/s
  for the mix with 16 users), not at the largest tenant's 2,000/s sustained
  and 10,000/s peak. The suite paces with `ERP_LOAD_RPS`; a run at the
  peak needs a cell-sized environment, which is the first scalability gate
  of 05 §5.1 before Phase 1 goes live.
- Primary failover and NATS node loss are simulated inside one process
  (connections terminated, an embedded node stopped); the quarterly
  resilience tests of 05 §5.8 against real clusters are still to be run.
- Rate limits are per replica and the per-agent-profile quotas of 04 §4.11
  are not implemented; limits per tenant per cell belong at the gateway.
- `erp-server`'s outbox gauge counts every context's outbox on each
  scrape; at very large backlogs the count itself gets slow, and a
  sampled gauge would be better.
- Read replicas, PgBouncer and the global router of 05 §5.2-5.4 are
  deployment concerns not exercised here.

### T8 decisions and open points

What T8 built: `internal/provisioning`, the one step the API cannot do for
itself (every request runs as a principal of an existing tenant), as the
operator command `erp-server provision`; and a development issuer
(`authn.DevIssuer`, `ERP_DEV_ISSUER=true`) that mints tokens on the admin
listener, with `erp token` to fetch them. The exit criterion is
`TestProvisionAndDevelopmentLogin` (cmd/erp-server): a tenant provisioned
from a configuration file, its administrator's token minted, and the API
answering the administrator. This closes T2's first open point and the
README's "no local tenant bootstrap" gap.

Decisions taken while building it:

- **One transaction provisions everything.** The tenant row is written as
  the connection's own role (the migrations' owner), then the transaction
  switches to `erp_app` acting as the new administrator, so the tenant's
  audit trail starts with the administrator: the administrator principal
  (all companies), the `tenant_admin` role holding every permission of
  `ref.permission`, the roles the configuration's approval policies name
  (empty, for the administrator to fill), an active legal entity per
  company of the configuration, and the configuration as version 1 through
  `configuration.Bootstrap`. `TenantStatusChanged` (provisioning → active)
  is published. A failure anywhere leaves nothing (`TestProvisioningIsAllOrNothing`).
- **The first configuration is required.** A tenant without one cannot
  post, and there is no API to bootstrap later (changes go through change
  requests on version 1). `erp config generate` writes one from SAP
  customizing before the tenant exists.
- **The development issuer is static, not discovered.** `authn.New` takes
  static issuers with known keys next to the discovered ones; the server
  trusts its own fresh RSA key under issuer `erp-dev`, audience `erp-api`,
  and serves `POST /dev/token` on the admin listener only. With
  `ERP_DEV_ISSUER=true`, `ERP_OIDC_ISSUERS` becomes optional; the server
  logs a warning at start. Tokens live an hour.

Open points for the contract and design review:

- Provisioning is an operator command on the server, not an API: a
  control-plane API (and the global tenant directory of 05 §5.4) is still
  to be designed, and with it suspending and offboarding tenants.
- Per-tenant identity providers (ADR-0014's registry) are still deployment
  configuration; the development issuer does not change that.
- The administrator holds every permission, including approvals, so
  segregation of duties starts when the administrator provisions the real
  roles; a tenant's first SoD rules could be part of the configuration.

### T9 decisions and open points

What T9 built: the `config.simulate` worker (`configuration.SimulationWorker`,
run by `erp-server` like the payment proposer) and the replay behind it
(`payables.Simulation`). `:simulate` on a change request in
`awaiting_evidence` queues the operation; the worker finds queued
simulations across tenants as `erp_worker`, runs each in one transaction of
its tenant as `erp_app` acting as the person who asked, records the result
as `simulation` evidence and, when it passed, moves the request to
`pending_approval` with its approval request (the same routing submission
uses for tier 2). The exit criterion is `TestSimulationEvidenceMovesTier3On`
(internal/configuration): a tax-rate change is submitted, simulated, approved
by two people and activated as version 2, and
`TestSimulationReplaysInvoices` (internal/payables) for what the replay
finds. This closes the open point carried since T2 ("tier 3 and 4 changes
wait for simulation evidence").

Decisions taken while building it:

- **The replay reads committed data and writes nothing.** The three-way
  match is now a pure function (`payables.evaluate`) of the invoice, its
  order, what was invoiced before and a tolerance, used by `:match` and by
  the simulation alike. The simulation replays the supplier invoices on
  purchase orders of the last 90 days (at most 2,000, newest first) under
  the base and the proposed configuration and reports: decisions that
  change (auto-post, review by a person, exception), posting rules that
  move with the invoices and amounts of the window they apply to (input tax
  and payables reconciliation accounts, tax code rates and accounts), and
  invoices the proposal cannot evaluate (a tax code or company it drops).
  It passes when every invoice could be evaluated; the differences are
  information for the approvers, in the evidence detail.
- **Required evidence is the simulation alone** for now
  (`config.RequiredEvidence`). The contract's evidence kinds still include
  `regression`; it returns when regression suites have an API again (T6
  removed the prototype's).
- **Operations complete or fail honestly.** A request no longer awaiting
  evidence (withdrawn meanwhile), a proposal the simulator refuses, or an
  operation naming no request fails the operation with the problem; a
  transient error leaves it queued for the next round; a cancelled
  operation is left alone. A simulation that does not pass still completes
  the operation and records the evidence; the request waits for a better
  proposal or another run.
- **Backlog gauge** `erp_simulations_queued` joins the worker gauges.

Open points for the contract and design review:

- The replay covers supplier invoices; changes to payment terms, approval
  routes, house banks or number ranges are classified by tier but have no
  replay yet, so their simulation reports nothing to decide on and passes.
- There is no event for recorded evidence; `ChangeRequestValidated` is
  emitted again with the new status when the request moves on.
- Simulation runs within one worker transaction, like proposals and
  migration runs; chunked runs wait for Temporal (ADR-0012).
- Regression evidence and the regression suites it needs are still to be
  rebuilt on the API.

### T10 decisions and open points

What T10 built: regression suites on the API (`regression-suites`: capture,
get, list; permission `config.regression.manage`; migration 000018), their
capture and replay in payables (`payables.Regression`) and the regression
evidence the simulation worker now records next to the simulation. A suite
is a stratified sample of the recent supplier invoices on purchase orders
under the active version (every decision, every match issue kind, invoices
near the auto-post limit, then more up to the maximum) with the decision
each case got. Tier 3 and 4 changes need both kinds of evidence again
(`config.RequiredEvidence`); the test agent can capture suites. The exit
criterion is `TestRegressionSuites` (internal/payables) for capture and
replay on real invoices, and `TestRegressionEvidence`
(internal/configuration) for the evidence: no suite, a stale suite, a
regression outside the change's scope, and changes inside it.

Decisions taken while building it:

- **A suite is pinned expectations, not a snapshot.** The prototype stored
  a copy of the data; here a case is the invoice id and its decision at
  capture, replayed on today's data (the ledger is append-only, so the
  inputs do not change). A replay under the base version that no longer
  matches means the suite is stale ("capture a new suite"), and the
  evidence fails until one is.
- **What passes.** The simulation's replay is informational; the regression
  suite judges. A case decided differently under the proposal is the
  change at work when its company is one the change's items touch
  (`companies[<code>]...` paths); outside those companies it is a
  regression and the evidence fails. Changes to payment terms, purchasing
  or payments touch no company, so any difference fails them. Both kinds
  are recorded in the same worker run; the request moves on when both pass.
- **No suite is a failing evidence, not an error.** The operation
  succeeds, the evidence says "capture one (createRegressionSuite) and
  simulate again", and the test agent's prompt tells it to do so.
- **Capture is synchronous** (at most 2,000 invoices looked at, 50 cases by
  default) and reads committed data in the request; evidence of the
  regression kind is immutable like all evidence.

Open points for the contract and design review:

- The replay covers supplier invoices like the simulation; suites of other
  transaction kinds (payments, receipts) come with their replays.
- A suite belongs to the version it was captured on; after an activation,
  the next tier 3 change needs a new suite on the new version (the evidence
  says so). Carrying a suite forward when its cases still match would save
  a step.
- The scope rule (touched companies) is a heuristic; a change could
  legitimately alter decisions in another company (shared payment terms)
  and would need a new suite to approve. An explicit "accepted changes"
  step on the change request would be more precise.

### T11 decisions and open points

What T11 built: the control plane (`api/control.yaml`, 04 §4.15, ADR-0015),
the operators' API for tenants, served by `erp-server` on the admin listener
under `/control/v1` by hand-written handlers (`internal/controlplane`):
list (status filter, cursor pages), provision (`createTenant`, through the
T8 provisioner with an `Idempotency-Key`), get, `:suspend`, `:resume` and
`:offboard`. Operators authenticate with tokens from the operator identity
providers (`ERP_OPERATOR_ISSUERS`; `authn.VerifyOperator`) that name no
tenant; the development issuer mints them without a `tenant_id`
(`erp token -subject NAME`). Migration 000019 adds the read-only
`erp_control` role and the unique index for control-plane idempotency keys.
`erp tenants list|get|create|suspend|resume|offboard` drives it from the
command line. The exit criterion is
`TestTenantsLifecycleThroughTheControlPlane` (internal/controlplane): a
tenant provisioned and replayed through the control plane, suspended and
refused at the public API's door, resumed, offboarded, with the events and
the tenant's audit chain checked; and `TestProvisionAndDevelopmentLogin`
(cmd/erp-server) doing it on the running server with development tokens.

Decisions taken while building it:

- **A second contract on the admin listener, not operations on `/v1`.**
  Tenants cannot be resources of the public API: the tenant comes from the
  token (04 §4.2), so no principal can address another tenant. The admin
  listener is already the internal surface (readiness, metrics, the
  development issuer), so the control plane lives there, reachable only
  inside the platform. The contract is hand-written OpenAPI linted with
  the same ruleset (the kebab-case rule now allows `/control/v1`), and
  `TestContractIsServed` checks every operation of it is routed; the
  handlers are plain `net/http` because `ServeMux` cannot pattern
  `{id}:verb` (ADR-0014), so the item handler splits the last segment.
- **Operators are not principals.** They have no row in any tenant, so
  authorization is the operator identity provider's: any accepted operator
  token may do anything the contract offers. Their actions are still
  attributable: the audit entry in the tenant's chain carries actor kind
  `system` and an id derived (UUIDv5) from issuer and subject, with both in
  the detail. A tenant's token is refused by the control plane, and an
  operator token by the public API (`VerifyOperator` requires the tenant
  claim to be absent; `Verify` requires it present).
- **A read-only cross-tenant role.** `erp_control` (`BYPASSRLS`) may
  `SELECT` `tenancy.tenant` and `platform.idempotency_key` and nothing
  else (`TestControlRoleIsReadOnly`); status changes run in the tenant's
  scope as `erp_app`, so RLS confines the write to one tenant and the
  tenant's outbox and chain record it. This is the "separate, audited
  maintenance role" ADR-0003 asked for.
- **Idempotent provisioning without a tenant to key on.** The provisioner
  stores the key with the tenant it creates, in the same transaction, under
  the `control|` prefix; a unique partial index on prefixed keys makes two
  concurrent requests with one key create one tenant (the loser gets 409
  `request.idempotency_key_in_use`), and the control plane looks the key up
  as `erp_control` before provisioning to replay (the same operator and
  body; anything else is 422 `request.idempotency_key_reused`).
- **Suspension is enforced where requests enter.** The unit of work already
  refused inactive tenants (403 `tenancy.tenant_inactive`); the control
  plane only changes the status. Workers leave a suspended tenant's queued
  work where it is.

Open points for the contract and design review:

- The global tenant directory of 05 §5.4 (tenant → region → cell) is still
  to be built; the control plane manages the tenants of one cell. A global
  control plane would front the cells' control planes and the directory.
- Offboarding is a status, not a data lifecycle: retention, export and
  erasure of an offboarded tenant's data need their own design (and the
  object storage that is still open).
- Operator authorization is all-or-nothing by issuer; roles for operators
  (read-only operators, provisioning operators) would be a claim the
  verifier checks.
- `erp-server provision` and `createTenant` share the provisioner; the
  command stays for bootstrapping a cell with no operator identity
  provider yet, and could go once every environment has one.

### T12 decisions and open points

What T12 built: object storage behind one small port
(`internal/platform/blobstore`: `Put` once, `Get`), with a directory store
for development and an S3-compatible store (path-style, Signature Version
4 written in the package and checked against the AWS test vectors,
`If-None-Match: *` and Object Lock headers), configured by `ERP_OBJECT_DIR`
or `ERP_OBJECT_S3_*` (ADR-0016). Three consumers: captured supplier invoice
documents are stored under their hash before extraction and downloaded
through `GET /v1/supplier-invoices/{id}/document`; bank files are stored at
release and read back for `:transmit` and
`GET /v1/bank-files/{id}/content`, always against the hash recorded with the
file; and an anchoring worker (`audit.Anchorer`, run by `erp-server`)
writes every tenant's moved chain head to a locked object and to
`audit.anchor`, which `verifyAuditChain` now starts from and checks
(migration 000020 grants the worker role its reads). The exit criteria are
`TestCapturedDocumentsAreKept` (payables), `TestBankFilesAreKept`
(payments) and `TestAnchoring` (audit), and the server test shows the
anchor appearing on a running server.

Decisions taken while building it:

- **Write-once by the store, not by convention.** `Put` refuses an
  existing key (`O_EXCL` on disk, `If-None-Match: *` on S3), so nothing in
  the platform can replace a stored document, file or anchor; a document
  captured twice is the same object under the same hash, which is how a
  retried capture finds it already there. Anchors add a compliance-mode
  retention lock (`ERP_OBJECT_RETENTION`, default ten years, 01 §1.3).
- **Keys lead with the tenant** (`tenants/<id>/<kind>/<name>`), so one
  tenant's objects can be listed, exported or erased together when
  offboarding gets its data lifecycle.
- **The hash in the database is the truth about the content.** Downloads
  and transmissions compare what the store returns with the hash recorded
  in the row (409 `payments.bank_file_changed`, `payables.document_changed`),
  so a store that lost or altered an object cannot hand out a different
  file. Anchors go the other way: the locked object is the truth about the
  chain head, and verification compares the chain and the `audit.anchor`
  row with it.
- **Without object storage the platform still runs**: capture records the
  hash only (as before), bank files are rendered again from the run, the
  chain is not anchored, and the downloads answer 503; the server warns at
  start. Files stored before object storage keep their `sha256:` references
  and are rendered again.
- **No SDK.** The S3 client is a few hundred lines with the signing
  algorithm; a test double of the service (`blobstoretest`) checks
  signatures, conditional writes and lock headers. The secret key comes
  from the environment until the vault exists.

Open points for the contract and design review:

- Documents above 5 MB still have no path (04 §4.11's `:upload` URLs would
  be presigned writes to the store).
- Exports to object storage (04 §4.5) and SAP extracts (`ERP_EXTRACT_DIR`)
  are still files on disk.
- Offboarding's data lifecycle (export, retention, erasure of a tenant's
  prefix) is now possible and still to be designed.
- Anchors are checked on request (`verifyAuditChain`); a scheduled
  verification per tenant would turn tampering into an alert.

### T13 decisions and open points

What T13 built: secret references (`internal/platform/secrets`, ADR-0017).
A reference names where a secret lives: `env:NAME`, `file:/path` (a
mounted secret) or `vault:<mount>/<path>#<field>` (HashiCorp Vault KV v2,
reached with a token or by the Kubernetes login method, the login renewed
once when it expires). The settings that carry secrets (`DATABASE_URL`,
`ERP_PAGE_TOKEN_KEY`, `ERP_FIELD_KEY`, `ERP_OBJECT_S3_ACCESS_KEY`,
`ERP_OBJECT_S3_SECRET_KEY`, `ERP_NATS_URL`) accept a reference in place of
the value and are resolved when the configuration loads
(`ERP_VAULT_ADDR`, `ERP_VAULT_TOKEN`, `ERP_VAULT_NAMESPACE`,
`ERP_VAULT_K8S_ROLE`, `ERP_VAULT_K8S_JWT_FILE`). Connectors' `secret_ref`
goes through the same resolver, replacing the `env:`-only stand-in of T6.
CI scans the repository for leaked secrets (gitleaks). The exit criteria
are `TestSecretReferences` (config: settings from the environment, files
and a Vault double, with the Kubernetes login) and the vault-backed
connector in `TestConnectorsAndRuns` (integration).

Decisions taken while building it:

- **References, not a secrets store of our own.** The platform stores no
  secret: configuration carries references, connectors carry references,
  and the replica resolves them at start (settings) or on use
  (connectors). Which store holds them is the deployment's choice; the
  three schemes cover the environment, Kubernetes secrets mounted as files,
  and Vault.
- **One resolver, built from the environment.** `config.Load` builds the
  resolver from `ERP_VAULT_*` (the Vault token may itself be an `env:` or
  `file:` reference) and keeps it in the configuration for the API; a
  `vault:` reference without a vault configured is a start-up error that
  names the setting.
- **No Vault SDK.** KV v2 reads and the Kubernetes login are two HTTP
  calls; a test double (`vaulttest`) serves them with token checks,
  revocation and namespaces.
- **Keys are resolved, not yet rotated.** The field-encryption and
  page-token keys still come as one value each; rotation (the sealed
  format is versioned) remains open.

Open points for the contract and design review:

- Key rotation for field encryption and page tokens, and per-tenant keys in
  a KMS (T3's open point).
- Other secret stores (AWS Secrets Manager, GCP Secret Manager, Azure Key
  Vault) would be further schemes behind the same port.
- The CLI's own credentials (`ERP_TOKEN`, `SAP_USER`/`SAP_PASSWORD` for
  `config generate`) stay in the user's environment.
- `gosec` is not in CI yet; secret scanning is.

### T14 decisions and open points

What T14 built: per-tenant field-encryption keys and rotation (ADR-0018).
`fieldcrypt.Keyring` seals each tenant's bank accounts, tax identifiers and
house bank IBANs with the tenant's own data key and fingerprints them with
the tenant's own fingerprint key; both are generated at provisioning,
stored wrapped by a master key in `tenancy.tenant_key` (migration 000021),
and unwrapped on first use per replica. Masters form a ring
(`ERP_FIELD_KEYS`, `id=base64,...`, the first wraps new keys, all unwrap);
without it the legacy `ERP_FIELD_KEY` is the one master. The sealed format
gained a version that names its key, and the legacy format still opens.
`POST /control/v1/tenants/{id}:rotateKey` (`erp tenants rotate-key`)
retires the tenant's key, stores a new one and re-seals every registered
sealed column (contexts declare theirs with `fieldcrypt.Register`) in one
transaction, recorded in the tenant's chain; a tenant still on the legacy
key gets its first key this way. The exit criteria are
`TestRotationResealsEveryColumn` (fieldcrypt, on a configured tenant),
`TestKeyRotationThroughTheControlPlane` and the provisioning test showing
a new tenant sealed with its own key from the start.

Decisions taken while building it:

- **Envelope encryption, no KMS yet.** The tenant data key is the unit of
  rotation and the thing a KMS would hold or wrap; the master ring is the
  KMS stand-in (`Master` is an interface). Rotating a master is re-wrapping
  tenant keys at the next rotation; a master may leave the ring once no
  tenant key is wrapped by it (`ErrNoMaster` says so otherwise).
- **Rotation re-seals in the request.** A tenant's sealed rows are few
  (bank accounts, tax identifiers, house banks), so the rotation is one
  transaction and the previous key is retired but kept, so nothing sealed
  with it is unreadable. Contexts register their sealed columns; the
  validity trigger of effective-dated bank accounts lets a re-seal through
  when nothing but sealed columns changed.
- **Fingerprints move with the tenant key.** The fingerprint key is the
  tenant's too, so a tenant moving off the legacy key gets new fingerprints
  for every row in the same rotation; equality lookups keep working because
  new values are fingerprinted the same way.
- **Legacy stays readable.** Values sealed before tenant keys (format 1)
  open through the legacy key until the tenant is rotated; tenants made
  before T14 keep working unrotated.

Open points for the contract and design review:

- A KMS implementation of `Master` (AWS KMS, GCP KMS, Azure Key Vault) with
  the same wrap and unwrap calls.
- Rotation is on request by an operator; a policy (yearly, on staff change)
  would schedule it, and a list of tenants by key age would show who is due.
- The page-token key (`ERP_PAGE_TOKEN_KEY`) is not rotated; tokens are
  short-lived cursors, so a key change only invalidates open pages.

### T15 decisions and open points

What T15 built: a KMS master for the ring (ADR-0019). `fieldcrypt.KMS`
implements `Master` with AWS KMS `Encrypt` and `Decrypt`, the tenant id as
encryption context, the JSON protocol signed by the platform's own
Signature Version 4 signer, now a package of its own
(`internal/platform/sigv4`, shared with the S3 store and checked against
the AWS test vectors). `ERP_FIELD_KMS_KEY` (a key id, ARN or alias,
optionally `name=key`), `ERP_FIELD_KMS_REGION`, `ERP_FIELD_KMS_ENDPOINT`
and the credentials `ERP_FIELD_KMS_ACCESS_KEY`/`_SECRET_KEY` (secret
references allowed) put it first in the ring; `ERP_FIELD_KEY` and
`ERP_FIELD_KEYS` become optional. `Keyring.Rewrap` moves a tenant's keys
onto the first master without changing them, and `erp-server rewrap` does
it for every tenant of the cell, so the masters a KMS replaces can leave
`ERP_FIELD_KEYS`. A test double of KMS (`fieldcrypt/kmstest`) checks
signatures and binds ciphertexts to their context. The exit criteria are
`TestKMSMasterWrapsTenantKeys` (a tenant provisioned under the KMS master,
unwrapped once per replica), `TestRewrap` (keys moved, retired ones too,
every failure reported) and `TestRewrapCommand` (a cell moved from the
legacy master to KMS from the command line, nothing left to move the
second time).

Decisions taken while building it:

- **Encrypt, not GenerateDataKey.** The platform generates tenant keys and
  asks the master to wrap them, so one `Master` interface serves local keys
  and KMS alike and the fingerprint key is made the same way.
- **Re-wrap is the migration.** Moving a cell to KMS rewrites `tenant_key`
  only; rotation (which re-seals rows) stays an operator's per-tenant
  action. The run stops at the first tenant that fails and resumes where it
  stopped.
- **A name for the master, separate from the key.** `tenant_key` records
  `kms` (or the configured name), so the KMS key may be renamed, aliased or
  rotated in place without touching stored rows; a second KMS key would be
  a second name.
- **No KMS call at start.** A misconfigured KMS shows at the first wrap or
  unwrap (provisioning, rotation, a tenant's first request on a replica),
  reported with the service's exception type; a probe at start would add a
  billable call per replica start for a check the first request makes
  anyway.

Open points for the contract and design review:

- Ambient AWS credentials (instance roles, IRSA) instead of access keys;
  GCP KMS and Azure Key Vault as further masters.
- Re-wrap is a command on the cell; the control plane could expose it per
  tenant, and the tenant list could show which master wraps each tenant's
  key and when it was rotated.
- A KMS key policy that restricts `Decrypt` to the replicas' role is the
  deployment's; the platform cannot check it.

### T16 decisions and open points

What T16 built: direct uploads (ADR-0020). `POST /v1/uploads` starts an
upload of a document above the 5 MB request limit (up to 50 MB) and answers
with where to `PUT` it: a presigned URL of the S3 store (query-string
Signature Version 4, now in `internal/platform/sigv4` with the AWS example
as its known answer), or, with the directory store, a signed path on the
API (`PUT /v1/uploads/{id}/content`, the one route outside the generated
server and the unit of work, bounded by the upload's limit). `GET
/v1/uploads/{id}` shows the upload; `:capture` takes `upload_id` in place
of `document`, checks the object (`Stat`, new on the store port) before
reading it, stores it under its hash like any captured document and closes
the upload. `erp capture` sends small documents in the request and large
ones through an upload. The exit criteria are `TestDirectUploads`
(payables, through the API's own URL: refusals, limits, expiry, re-sent
bytes, every failure) and `TestPresignedUploads` (through the S3 double's
presigned URL), and `TestStatAndPresignedUploads` (blobstore).

Decisions taken while building it:

- **The upload is a resource, the bytes are not a request.** The unit of
  work hashes every body for idempotency; documents bypass it and land in
  the store, which is where they were going anyway. The API's own upload
  route exists for the directory store and for deployments whose store is
  not reachable by clients.
- **Checked when consumed, not when uploaded.** A presigned `PUT` cannot
  bound the size on every S3-compatible service; the capture asks the
  store for the size first and refuses before reading. Nothing uploaded
  yet, expired, used before and too large are distinct 422 codes.
- **Idempotent by content.** A PUT re-sent after a lost answer finds the
  object in the write-once store; the same bytes are accepted, different
  bytes are a conflict, so a failed statement never leaves an upload that
  cannot complete.
- **The signing key is derived**, from the page-token key, rather than one
  more setting: both guard short-lived URLs of one deployment.

Open points for the contract and design review:

- Objects of uploads never consumed stay under `tenants/*/uploads/`; a
  bucket lifecycle rule or the offboarding erasure removes them.
- The API's upload URL is a path; `ERP_PUBLIC_URL` would make it absolute
  for clients that do not know the API's base.
- Uploads serve capture only; attachments on other documents would reuse
  the resource with further purposes.

### T17 decisions and open points

What T17 built: the offboarding lifecycle (ADR-0021). `internal/offboarding`
is a worker that exports offboarded tenants (every table with a `tenant_id`
as JSON Lines plus a manifest with row counts, hashes and the audit chain
head, under `tenants/<id>/exports/<offboarding time>/`, locked for the
retention period) and, after `ERP_OFFBOARD_GRACE` (default 30 days), erases
them: `platform.erase_tenant` (migration 000023, owner-run, in the tenant's
scope, passes over foreign keys, insert-only tables let it through) deletes
every row but the audit schema, and the store's new `List` and `Delete`
remove every object under the tenant's prefix but the locked ones. The
tenant row records `exported_at`, `export_ref`, `erase_after`, `erased_at`,
shown as `offboarding` by the control plane; `POST
/control/v1/tenants/{id}:erase` (`erp tenants erase`) erases earlier, as
the operator. The exit criteria are `TestOffboardingLifecycle` and
`TestOffboardingFailures` (offboarding), the erase flow in
`TestTenantsLifecycleThroughTheControlPlane` (controlplane) and
`TestListAndDelete` (blobstore).

Decisions taken while building it:

- **Export before erasure, once, by offboarding time.** A retried round
  finds the objects of an earlier attempt and describes what is stored;
  nothing is overwritten.
- **One owner-run function erases.** The application role keeps its lack
  of `DELETE` grants; the mutation guard of insert-only tables admits the
  erasure through a transaction-local setting only that function sets.
- **The audit trail and the tenant row stay.** The erasure's own entry is
  the last of the trail; locked anchors and exports stay in the store.
- **The worker and the operator share one path.** `:erase` runs the same
  erasure with the operator as actor, after the tenant's state is checked
  under `If-Match`.

Open points for the contract and design review:

- Audit entries keep details about erased objects; a minimization pass
  (hashing or dropping detail after erasure) is a separate design.
- Exports are built table by table in memory; a tenant whose largest table
  exceeds a replica's memory needs streaming exports.
- Restoring an exported tenant into a cell (re-onboarding, migration
  between cells) is not built; the export's format is the input it would
  take.

### T18 decisions and open points

What T18 built: the global tenant directory and router of 05 §5.4
(ADR-0022). `internal/directory` is the directory (one table in its own
database, `ERP_DIRECTORY_URL`, created on open), a cache, and a sync worker
that every cell runs to keep its tenants' entries current (status, region,
`ERP_CELL_API_URL`); the control plane registers a tenant right after
provisioning and after each status change. `internal/router` and
`cmd/erp-router` verify the token with the cells' issuers, look the tenant
up (cached for `ERP_DIRECTORY_CACHE`) and reverse-proxy the request to the
cell's API URL unchanged, naming the cell in `X-Erp-Cell`; unknown tenants
404, suspended and offboarded 403, a moving tenant's writes 503 with a
retry, a cell down 502. The exit criteria are `TestRouting` (router: two
cells, every refusal), `TestRoutesToTheCell` (the binary end to end),
`TestPostgresDirectory`, `TestCache`, `TestSync` (directory), the
directory registrations in `TestTenantsLifecycleThroughTheControlPlane`
and `TestDirectorySync` (the server recording its tenants).

Decisions taken while building it:

- **The directory lives outside the cells.** Its own database URL, its own
  one-table schema created by whoever opens it; locally it may be a schema
  in the same PostgreSQL.
- **Cells write, the router reads.** A registration after each change plus
  a reconciling round a minute: no change is lost, and a cell that comes up
  with a directory for the first time registers every tenant it has.
- **`read_only` belongs to the directory.** It marks a move in progress
  and survives the cells' writes; the cell knows nothing of it.
- **The router trusts no cell with routing.** It verifies the token itself
  before reading the tenant claim, and the cell verifies again: a cell never
  trusts the router's word for who is calling.

Open points for the contract and design review:

- Operator operations on the directory (set `read_only`, move an entry to
  another cell) are in code (`Postgres.SetStatus`) but not on a contract;
  a global control plane would carry them, together with the tenant move
  of 05 §5.4.
- Placement of new tenants (which cell takes the next tenant of a region)
  is the operator's choice of which cell's control plane to call.
- The router caches lookups; a cell change reaches it within the cache
  period, which is the shortest "read-only" window a move can have.

### T19 decisions and open points

What T19 built: the global control plane (ADR-0023). `api/directory.yaml`
is a third contract, served by `erp-router` on its admin listener
(`ERP_ROUTER_ADMIN_ADDR`, with `ERP_OPERATOR_ISSUERS` and
`ERP_PAGE_TOKEN_KEY`) by `internal/globalplane`: tenants across cells
(filters, cursor pages), cells with their tenant counts, provisioning with
placement (`directory.placement` remembers the cell per `Idempotency-Key`;
the request is forwarded to the cell's control plane with the operator's
token; the answer names the cell; the entry is in the directory at once),
and `:freeze`, `:move`, `:unfreeze` under `If-Match` on the entry's new
`version`. Cells register themselves each round (`ERP_CELL_REGION`,
`ERP_CELL_CONTROL_URL`, `ERP_CELL_ACCEPTING`). `erp directory` drives it
from the command line. The exit criteria are `TestGlobalControlPlane`
(globalplane: placement across two fake cells, retries, refusals, a full
move, every operation served), `TestPostgresDirectory` (cells, placement,
versions, listing) and the admin listener in `TestRoutesToTheCell`.

Decisions taken while building it:

- **The cell's control plane stays the one that provisions.** The global
  plane places and forwards; idempotency, audit and the transaction are
  the cell's. One Idempotency-Key, one cell, recorded before the forward.
- **Fewest tenants wins, names break ties.** Capacity is a count for now;
  a cell stops taking tenants by setting `ERP_CELL_ACCEPTING=false`, and a
  named cell may still be chosen while it accepts.
- **Versions on directory entries.** An upsert that changes nothing leaves
  the version alone, so the cells' rounds do not race operators' `If-Match`.
- **The admin listener is the router's.** No new deployment; the router
  already has the directory and the issuers.

Open points for the contract and design review:

- The copy and verification of a tenant's rows between cells (05 §5.4
  steps 1 and 3) are outside the API; a `:verify` that compares row counts
  and the audit chain head across two cells would close the loop.
- Placement weighs tenants, not size or load; cell capacity (05 §5.4
  caps) would be a registered attribute.
- Operator roles (read-only, provisioning, moving) apply here as on the
  cells' control planes.

### T20 decisions and open points

What T20 built: ambient AWS credentials (ADR-0024).
`internal/platform/awscreds` is a provider chain without the SDK: static
keys, the `AWS_*` environment, a web identity exchanged at STS
(`AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN`), the instance metadata
service (v2), with a cache that renews temporary credentials five minutes
before expiry. The S3 store and the KMS master take a provider; the signer
signs session tokens (`SignWith`, `PresignWith`). `ERP_OBJECT_S3_*` and
`ERP_FIELD_KMS_*` access keys are optional (both or neither);
`ERP_AWS_STS_ENDPOINT` and `ERP_AWS_IMDS_ENDPOINT` point tests and
emulators elsewhere. The exit criteria are the `awscreds` suite (every
provider against a double, the chain, the cache), `TestAmbientCredentials`
(blobstore) and `TestKMSAmbientCredentials` (fieldcrypt), and the ambient
case of `TestObjectStores` (a server anchoring through the S3 double with
the `AWS_*` environment and no keys in settings).

Decisions taken while building it:

- **Static first, then ambient.** Settings keep working unchanged; a
  deployment on roles leaves them unset.
- **The metadata service is absent, not broken, when unreachable.** The
  chain ends with ErrNone and the first signed call reports it; the other
  providers' failures (a denied STS exchange) are errors.
- **One cache per client.** The store and the master hold their own
  credentials; both renew early.

Open points for the contract and design review:

- An ECS task role (the container credentials endpoint) and a process
  credential provider are further providers behind the same interface.
- A credentials check at start would fail fast; today the first store or
  master call does.

### T21 decisions and open points

What T21 built: scheduled audit verification (ADR-0025). `audit.Verifier`
recomputes every tenant's chain from its first event and checks every
anchor each round (`ERP_AUDIT_VERIFY_INTERVAL`, default 1h, 0 disables),
records the result in `audit.verification` (migration 000024, insert-only),
and, when a chain that verified stops doing so, logs an error, publishes
`AuditChainTampered` (a `platform` event) and is counted by the gauge
`erp_audit_chains_tampered` until a round verifies again. The exit
criterion is `TestScheduledVerification` (audit): a verified chain
recorded, an event rewritten behind the chain's back found at its sequence,
the alarm logged and published once, every statement and transaction
failure reported, a failing anchor store reported, no store tolerated.

Decisions taken while building it:

- **Full recomputation each round.** The anchors fix points; everything
  between them is suspect, so the round reads the whole chain.
- **Alarm on the transition, not every round.** The previous row of
  `audit.verification` says whether the chain verified before; the gauge
  carries the state in between.
- **The platform channel carries the event.** The audit schema has no
  outbox of its own; a tampered chain is a platform-level alarm.

Open points for the contract and design review:

- Incremental verification with checkpoints for chains too long to read
  hourly.
- The control plane could show a tenant's latest verification; today it is
  the table, the gauge and the event.

### T22 decisions and open points

What T22 built: chunked exports (ADR-0026). The offboarding worker streams
a table's rows and stores a part every `ChunkRows` rows (10,000 by
default), named `<table>.00001.jsonl` onwards; the manifest lists each part
with its rows, bytes and hash, and a retried export finds the parts already
stored. The audit-detail question of T17 is closed by decision: details are
not rewritten after an erasure (the hash chain covers them), and the review
checklist asks that details carry identifiers, not personal data. The exit
criterion is `TestOffboardingLifecycle` with one row per part: every part
locked, listed, and summing to the table's rows.

Decisions taken while building it:

- **Parts, not multipart uploads.** The store port stays write-once and
  whole-object; a part of 10,000 rows is far below any object limit.
- **Chain integrity over detail minimization.** A rewritten detail is a
  broken chain; the trail of an erased tenant says what happened to which
  object, and the export holds the content under retention.

Open points for the contract and design review:

- Restoring a tenant from its export into a cell (re-onboarding) would read
  the parts in manifest order; it is not built.
- A lint of audit details for personal data (names, IBANs, emails) would
  turn the write-time rule into a check.

### T23 decisions and open points

What T23 built: four smaller items from the open points. Operator roles
(ADR-0027): the verifier reads a roles claim (`roles`, or `roles_claim` per
issuer) and both control planes check one role per operation (`erp:read`,
`erp:provision`, `erp:operate`, `erp:admin` implying all; a token without
the claim does everything). A key rotation policy (ADR-0028):
`controlplane.KeyRotator` rotates the keys of active tenants older than
`ERP_KEY_ROTATION_AGE` (migration 000025 lets the worker role see key
ages). `ERP_PUBLIC_URL` makes the API's own upload URLs absolute. `gosec`
runs in CI, with G104, G115, G304 and G107 excluded by decision and the
remaining findings fixed (file permissions) or annotated with their reason
(test doubles' example keys, the deterministic mock generator, SVG built
from numbers, a slice walked in pairs). The exit criteria are
`TestOperatorRoles` (authn, controlplane; the globalplane cases), `TestKeyRotationPolicy`
(controlplane and the server), the public URL case of `TestDirectUploads`,
and a clean `gosec` run.

Decisions taken while building it:

- **No claim, every role.** Existing issuers keep working; a deployment
  opts into roles by emitting the claim.
- **The rotation policy reuses the keyring.** The worker does what
  `:rotateKey` does, with a system actor and the policy in the audit detail.
- **Exclusions over annotations where a rule is noise by design.** Four
  rules are excluded with the reason recorded; everything else is fixed or
  annotated at the line.

Open points for the contract and design review:

- Re-onboarding a tenant from its export, and a cross-cell `:verify` of a
  moved tenant, are the two unbuilt items left from earlier steps.
- Operator roles are coarse; per-cell or per-region scoping would be a
  claim the verifier also reads.

### T24 decisions and open points

What T24 built: re-onboarding from an export (ADR-0029).
`offboarding.Importer` reads a manifest and its parts from the store, checks
each part's hash and row count, writes the tenant row (new, or back from its
tombstone) and every table's rows in passes over foreign keys through
`json_populate_record` (generated columns left to the database, outboxes
skipped), then checks the audit chain head against the manifest, all in one
transaction. `erp-server import -manifest KEY` runs it. The exit criteria
are `TestReonboarding` (an erased fixture tenant back with every table's
count and the chain head equal; refused while live or when rows are
present; created in a cell that never had it) and `TestImportRefusals`
(every mismatch and failure named).

Decisions taken while building it:

- **Verified before written, verified after.** Hashes and counts gate the
  insert; the chain head gates the commit.
- **Passes over foreign keys**, as the erasure does in reverse: order is
  discovered, not declared.
- **Outboxes stay where the events happened.**

Open points for the contract and design review:

- Partitions for old years must exist in the destination cell; the import
  does not create them.
- A cross-cell `:verify` on the global control plane would run the import's
  checks against a destination without writing.

### T25 decisions and open points

What T25 built: cross-cell verification (ADR-0030), the step of a move
that was still outside the API. Each cell's control plane answers
`GET /control/v1/tenants/{id}/fingerprint` (`offboarding.TakeFingerprint`:
the rows of every table carrying a `tenant_id`, outboxes left out, and the
audit chain head, in one read-only snapshot in the tenant's scope). The
global control plane's `POST /directory/v1/tenants/{id}:verify` asks the
tenant's cell and the cell named with the operator's own token and answers
both fingerprints, `equal`, and one line per difference. `erp tenants
fingerprint` and `erp directory verify` (non-zero exit while the cells
differ) drive them. The exit criteria are `TestTenantFingerprint` (a
provisioned tenant's counts and chain head, stable across reads, no
outboxes, readers allowed, every database failure reported) and
`TestVerifyTenant` (equal cells; every kind of difference named; a cell
without the tenant, unreachable, refusing or never registered told apart;
unknown and same cells refused; roles).

Decisions taken while building it:

- **Counts and the chain head, not row hashes.** One statement per table;
  the chain head already hashes every audited change. Row hashing can be a
  deeper mode later.
- **Verification does not change the entry.** It runs in any status, before
  freezing (how far is the copy) and after (safe to switch).
- **The operator's token travels to both cells**, as provisioning's does;
  the global plane holds no credentials of its own.

Open points for the contract and design review:

- The move is five operator steps (freeze, export, import, verify, move,
  unfreeze); a long-running `:migrate` operation on the global plane could
  drive them, once the import can be called on a cell rather than run as a
  command.
- Fingerprints count rows in a snapshot per cell at different moments; a
  tenant still taking writes in its source cell differs by design until it
  is frozen.

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
