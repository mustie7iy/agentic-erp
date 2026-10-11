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
| T7 | **Hardening** | Load, soak and resilience suites pass at the targets in 01 §1.3 |

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
