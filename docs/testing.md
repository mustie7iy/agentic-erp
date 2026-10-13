# Testing strategy

Every Phase 1 requirement has functional tests, and the platform has
dedicated non-functional suites. CI (`.github/workflows/ci.yml`) runs all of
it on every push: formatting, `go vet`, every test under the race detector,
the coverage gate, fuzzing and the build.

```sh
make test        # functional + end-to-end + non-functional, race detector on
make test-short  # fast loop (skips performance budgets)
make coverage    # whole-suite statement coverage with the CI minimum
make fuzz        # fuzz every untrusted-input parser (FUZZTIME=15s each)
make perf        # performance budgets and benchmarks
```

## Coverage

Whole-suite statement coverage of production code is **100%**, enforced in CI
by `scripts/coverage.sh` (test-only packages excluded). Getting there meant
testing every reachable error branch with real failures (unwritable paths,
corrupt files, failing servers, refusals) and deleting branches that could
never run, such as error checks on encoding plain data into memory. Each
deletion is commented in the code. Coverage measures that every statement
runs under test; the assertions in the suites below are what show the
results are right.

At the time of writing the suites contain 457 test functions (many with
subtests) across 68 packages, all passing under the race detector.
Database-backed suites run against PostgreSQL (`make db-up`), each package
in its own database (`internal/platform/pgtest`), and
`apitest.EveryFailure` fails each database statement of an operation in
turn to prove nothing is left half done.

## Functional tests

| Area | Where | What is proven |
|---|---|---|
| Provisioning | `internal/provisioning`, `cmd/erp-server` | A tenant provisioned with its administrator, roles, active legal entities and configuration v1 in one transaction; the administrator sets the tenant up through the API; every statement failing in turn leaves nothing; the development issuer mints tokens the API accepts |
| Object storage | `internal/platform/blobstore`, `payables`, `payments`, `audit`; direct uploads: the signer's presigned URLs match the AWS example and the S3 double enforces content type and validity; an upload through the API's signed path or the store's presigned URL is captured by id, with every refusal (empty, expired, used, too large, wrong type, conflicting bytes, bad signature) and every failing statement reported | Directory and S3 stores write once (an existing key is refused), serve content and type, report every failure; the S3 signer matches the AWS Signature Version 4 test vectors and a test double of the service checks signatures, conditional writes and lock headers; captured documents stored under their hash and downloaded byte for byte, refused when tampered; bank files stored at release, transmitted and downloaded from the store against the recorded hash, never to agents; the anchoring worker writes locked anchors, recovers from a round that did not commit, refuses one that disagrees with the chain; verification starts from the last anchor and detects a tampered anchor object, event or row; direct uploads: the signer's presigned URLs match the AWS example and the S3 double enforces content type and validity; an upload through the API's signed path or the store's presigned URL is captured by id, with every refusal (empty, expired, used, too large, wrong type, conflicting bytes, bad signature) and every failing statement reported; direct uploads: the signer's presigned URLs match the AWS example and the S3 double enforces content type and validity; an upload through the API's signed path or the store's presigned URL is captured by id, with every refusal (empty, expired, used, too large, wrong type, conflicting bytes, bad signature) and every failing statement reported |
| Key management | `internal/platform/fieldcrypt`, `controlplane`, `provisioning` | Master keys wrap and unwrap tenant keys bound to their tenant; tenants without a key seal with the legacy key and move to their own on rotation; the sealed format names its key and every corrupt, foreign or truncated value is refused; rotation re-seals house banks, bank accounts and tax identifiers in one transaction, keeps retired keys readable, fails when a master is missing and reports every database failure; through the control plane with `If-Match`, refused for offboarded tenants, recorded as the operator; a KMS master (against a test double of AWS KMS that checks signatures) wraps new tenant keys bound to their tenant, is called once per key per replica, and reports the service's refusals by type; re-wrapping moves a tenant's keys, retired ones too, onto the current master without rewriting a row, and `erp-server rewrap` does it for a cell |
| Offboarding | `internal/offboarding`, `controlplane`, `blobstore` | An offboarded tenant is exported to locked objects (every table, a manifest with rows, hashes and the audit chain head), left alone through the grace period, then erased: rows gone from every table but the audit trail, objects gone but the locked ones, other tenants untouched, the tenant row recording each stage; an export retried after a failure keeps and describes what was stored; exports written in parts of bounded size, each locked and listed with its hash and summing to the table's rows; every store, state and database failure reported with nothing half done; erasure on request through the control plane under `If-Match`, recorded as the operator; both stores list and delete, refusing while retention holds |
| Tenant directory and router | `internal/directory`, `router`, `cmd/erp-router`, `controlplane`, `cmd/erp-server` | The directory stores, looks up and updates entries, keeps an operator's `read_only` through the cells' writes, refuses bad statuses and reports a database it cannot open or reach; the cache answers hits and misses for its TTL and forgets what it writes; the sync records every tenant of a cell and logs a directory that refuses; the router forwards each tenant's request intact to its cell and names the cell, refuses missing, invalid and operator tokens, unknown, suspended and moving (writes) tenants, a bad entry, a cell down and a directory down, each with the right status; the binary serves, routes and stops with the right exit codes; the control plane registers every change; the server records its tenants |
| Global control plane | `internal/globalplane`, `directory`, `cmd/erp-router`, `cmd/erp` | Cells register with their load; provisioning is placed in the emptiest accepting cell of the region (or the cell named), forwarded with the operator's token and key, entered in the directory at once, kept on the same cell for a retry, and refused for a full, unknown or absent cell; the cell's refusal and silence pass through as such; a move freezes, moves to a registered cell of the same region and unfreezes under `If-Match`, each wrong state and region refused; a verification fetches both cells' fingerprints with the operator's token and names every difference, telling a cell without the tenant from one unreachable, refusing or unregistered; listing filters, orders and pages; every operation of the contract served; the CLI drives every command and reports every refusal |
| Ambient AWS credentials | `internal/platform/awscreds`, `sigv4`, `blobstore`, `fieldcrypt`, `cmd/erp-server` | Static keys, the environment, a web identity exchanged at an STS double (success, denied, garbage, missing token, unreachable) and an instance metadata double (v2 token, role, document; absent service, v1 only, no role, garbage) each yield or decline; the chain takes the first; the cache renews before expiry and keeps long-lived keys; session tokens are signed in headers and presigned queries and verified; the S3 store and the KMS master sign with a provider and report one with nothing to give; the server anchors through the S3 double with the `AWS_*` environment and no keys in settings; half a key pair is refused by name |
| Scheduled audit verification | `internal/audit`, `cmd/erp-server` | A verified chain is recorded with its head, events and anchors; an event rewritten behind the chain's back is found at its sequence, logged and published once, and counted until a round verifies again; every statement and transaction failing in turn is reported; a failing anchor store is reported and no store tolerated; the server runs the worker, or warns when the interval is 0 |
| Operator roles and rotation policy | `internal/platform/authn`, `controlplane`, `globalplane`, `cmd/erp-server` | A roles claim as an array, a scope string or absent is read, with a custom claim name per issuer; readers read, provisioners provision, operators operate and admins do all on both control planes, each refusal named; the rotation worker rotates only active tenants whose key is older than the age, once, as a system actor with the policy in the audit detail, reports every statement and transaction failure, and the server runs it when an age is set |
| Re-onboarding | `internal/offboarding`, `cmd/erp-server` | An erased tenant comes back from its export with every table's count and the audit chain head equal to the manifest's, its row active again, or is created in a cell that never had it; a live tenant and present rows are refused; a part with a wrong hash, a wrong count or missing, rows that stay referenced, a chain head missing or different and an unreadable tenant row are each refused by name; outboxes are skipped; every statement failing in turn is reported; the command runs it and reports its refusals |
| Secrets | `internal/platform/secrets`, `config`, `integration` | References to the environment, files and a Vault double resolve, miss and fail honestly; token and Kubernetes login with renewal once; settings loaded from references with every misconfiguration refused by name; connector credentials from the vault, 422 when the reference names nothing |
| Control plane | `internal/controlplane`, `cmd/erp-server`, `cmd/erp` | Operators list, provision (idempotently, one tenant for racing requests with one key), suspend, resume and offboard tenants through `api/control.yaml` on the admin listener; tenant tokens refused there and operator tokens on the public API; a suspended tenant refused at the door; every change in the tenant's outbox and hash chain as the operator; the read-only control role; a tenant's fingerprint (rows per table without outboxes, audit chain head) stable across reads and open to readers; the tenant showing its active key's master and its latest audit verification; every operation of the contract routed; every database statement failing in turn reported |
| Tenancy, identity, configuration | `internal/tenancy`, `identity`, `configuration` | Legal entities and org units, principals, effective-dated role and permission grants, company access, SoD refused at grant time and enforced per operation, configuration versions and change requests by tier; the simulation worker records simulation and regression evidence and routes tier 3 changes to approval, failing or retrying operations honestly; regression suites captured, listed and read |
| Configuration as code | `internal/config` | Every validation rule (one test per rule), change tiers, diffs |
| Configuration from SAP | `internal/delivery/sapconfig` | T001/SKB1/T030/T007A/T169G/T012K/T052/AGR_* mapped; number ranges per fiscal year; gaps, defaults and role notes |
| Partner and ledger | `internal/partner`, `internal/ledger` | Governed partner changes, encrypted bank accounts; balanced postings in three currencies, reversal, periods, projector balances, open items, clearing, settlement with realized exchange differences |
| Procure-to-pay | `internal/procurement`, `payables`, `approvals` | Requisition → approval → PO → receipt → invoice → match → posting; orders closed when nothing more is expected; contracts, expired by a job past their validity; over-delivery; exceptions decided on approval requests; no self-approval; the simulation replays invoices under a proposed configuration (decisions, posting rules, breaks); regression suites are stratified samples replayed with pinned decisions |
| Workflows | `internal/workflows`, `internal/platform/workflow` | A payment proposal built by a workflow whose one activity is the proposer's unit of work; a release posting one payment per activity with progress, writing the bank file at the end, failing the operation on a business problem (nothing posted: the run proposed again; postings: the run releasing, resumed by the next release), left resumable when the machinery fails, every statement failing in turn leaving nothing half done; the in-process releaser doing the same steps; a change request's simulation (with its regression evidence) run as a workflow whose activity is the simulation worker's unit of work, dispatched under the request's number and attempt; a migration run's start and reconciliation run as workflows when the API has an engine, an unreadable extract failing the operation and queueing the run again, dispatched under the run and attempt; the CLI waiting for a running operation; a pending approval request escalated each period up to the limit with the event and audit entry, the deadline workflow ending once the request is decided, one deadline per request; payloads sealed by the codec and opened again, refused under another key or tampered; a run already built left alone; a cancelled operation failing the run; a cancelled workflow still settling it; queued operations dispatched once under the run's business key and recorded on the operation; the engine's duplicate and unknown ids tolerated; against the SDK's test environment and the CLI's headless development server |
| Payments | `internal/payments` | Proposal worker with every exclusion, release by another person, payment entries clearing the items, bank files, transmission and the bank's acknowledgement or rejection |
| Migration | `internal/integration` | Connectors, quality gate, both modes, loads through the owning services, refused records failing the run, rehearsals rolled back, idempotent reruns, reconciliation with differences and missing documents |
| Agents runtime | `internal/agents`, `internal/apitools` | Sessions, tool calls and recommendations through the API; tools generated from the contract, `x-agent: false` refused, every call recorded |
| Agents | `internal/agent`, `configagent`, `migrationagent`, `testagent` | Full tool loops against a scripted model through the real API; agents only recommend or propose; the migration agent starts rehearsals only |
| Audit | `internal/audit`, `internal/platform/audit` | Audit events listed, the hash chain verified over a range, tampering reported; a detail carrying personal data (by key, or an IBAN or email value) refused at write time |
| Invoice capture | `internal/capture` | Structured-output request shape, prompt injection inert |
| Bank files | `internal/bankfile` | pain.001 structure, control sums, IBAN vs domestic accounts, escaping, schema field lengths |
| Analytics | `internal/analytics` | Facts read through the API, semantic queries, guardrails, row-level security, narrative check, rendering, reruns |
| End to end | `cmd/erp/e2e_test.go` | The CLI against a real API server: mock SAP extract, configuration draft, a scope migrated and reconciled through the integration API, the invoice rule, all four agents through API tools, analytics |

## Non-functional tests (`internal/nfr`)

The suites of 05-scalability §5.8 run over a harness that runs the platform
the way `erp-server` does: the public API on a real listener over a
configured tenant, the balance projector and the outbox relay to an embedded
JetStream, and virtual users with a principal each. They are plain `go test`
runs sized by environment variables, so they use the same fixtures and
assertions as the functional suites.

| Suite | What is proven | How to run |
|---|---|---|
| Load (`load_test.go`) | The mix of posting, lists, single reads, approval decisions and trial balance meets the p95 and p99 budgets of 01-requirements §1.3; the workers drain; the ledger is consistent afterwards. A hot principal is refused at the door while others are served | `make load` (`ERP_LOAD_USERS`, `ERP_LOAD_DURATION`, `ERP_LOAD_RPS`) |
| Soak (`soak_test.go`) | Over windows of sustained load: no failed request, p95 within 1.5× of the first window or within budget, stable goroutines and heap, workers keeping up | `make soak` (`ERP_SOAK_DURATION`, `ERP_SOAK_WINDOW`); nightly for hours |
| Resilience (`resilience_test.go`) | Database connections killed under load: 503s for milliseconds, recovery within the RTO, retried posts post once; JetStream node stopped and started: events wait and arrive exactly once; workers killed mid-work (projector, relay, proposal worker): each entry projected, event delivered and proposal built once; shutdown under load: complete answers or connection errors, never half-written | `make resilience`; also in `make test` |
| Performance (`perf_test.go`, `make perf`) | Budgets for analytics over a large fact set and configuration validation with 2k number ranges; database budgets live with their contexts (trial balance, payment proposal over 100,000 items) | `make perf` |
| Fuzzing (per package) | Amount parsing round-trips; arbitrary configuration YAML never panics validation or diff; any commentary text is safe for the narrative check; bank files stay well-formed XML with exact control sums for any names; OData values | `make fuzz` |

Every suite ends with the consistency check: each reference posted once,
every entry answered 201 in the journal, projected balances equal to the
journal, audit chain intact. Load and soak skip under `-short` and the race
detector. The nightly workflow (`.github/workflows/nightly.yml`) runs load
for two minutes, the resilience suite, a two-hour soak and five minutes of
fuzzing per target. The runner is several times slower than a laptop (about
110 requests/s with the database in a sidecar container), so its budgets are
scaled by `ERP_BUDGET_SCALE` (4 nightly, 2 for the performance step of the
push pipeline, 1 by default): the scaled budgets catch a regression, the
consistency checks still gate, and the budgets at scale 1 are for a
cell-sized environment, which the largest tenant's peak needs anyway. Both
workflows raise the service container's `max_connections` to 300 before
the suites run, as the local database has.

## Model and SAP stand-ins

- `internal/llm/llmtest` is a scripted Claude Messages API. Agent tests drive
  exact tool sequences, check what reaches the model (tool results, system
  prompt guards, fallback configuration) and cover refusals and API errors.
- The mock SAP system (`internal/sap/mock.go`) produces consistent extracts,
  including customizing tables and deliberate data-quality and SoD problems.

## Not covered by automated tests

- Calls to the live Claude API (no credentials in CI). Request shapes are
  verified against the scripted server; behavior quality needs evals with
  real models before agents get autonomy above L0.
- A live S/4HANA system. OData paging, filters and value conversion are
  tested against a local server; entity and field names must be checked
  against the client's `$metadata`.
