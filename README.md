# agentic-erp

Move off SAP one company code at a time, with every step proven by
reconciliation, and run the business processes on an agentic layer instead of
SAP screens.

Two objectives drive the design:

1. **Replace SAP's capabilities.** The [capability map](docs/capability-map.md)
   covers 120 capabilities across S/4HANA and SAP's cloud products, each built
   in the native core, rebuilt with agents, or integrated with a specialist product.
2. **Cut consultant and engineering cost.** Agents configure the system,
   migrate it (configuration as well as data) and run it day to day, working on
   configuration as code with validation, simulation and human approval. See
   [docs/agentic-delivery.md](docs/agentic-delivery.md).
3. **Answer any question on demand.** No catalogue of static reports: people
   ask in plain language and an agent builds the report or dashboard on the
   spot from governed data. See [docs/analytics.md](docs/analytics.md).

Written in Go. The only dependency is the Anthropic Go SDK.

## Why this approach

A big-bang SAP replacement is expensive and risky because everything has to
work on cutover day. This project follows a **strangler** pattern, combined
with the "Headless ERP" model from Rimini Street's *The Rise of Agentic AI ERP*:

1. **Connect.** Read SAP through its standard APIs. Nothing in SAP changes.
2. **Migrate a scope.** Load one company code and fiscal year into the native core.
3. **Prove it.** Reconcile the core against SAP independently: trial balance,
   open payables per vendor, POs, invoices. Any difference blocks cutover.
4. **Run it.** Process that scope's work natively. Rules handle the routine
   path, Claude agents investigate exceptions, and people approve.
5. **Repeat** for the next scope, then decommission SAP.

The savings come from shrinking what you license, upgrade and staff on SAP,
scope by scope, and from agents doing the configuration, migration, testing and
support work that consultants do today.

| Document | Contents |
|---|---|
| [docs/plan.md](docs/plan.md) | Two-track roadmap (business modules and delivery agents), exit criteria, risks |
| [docs/capability-map.md](docs/capability-map.md) | Every SAP capability, its approach, phase and status |
| [docs/agentic-delivery.md](docs/agentic-delivery.md) | Consultant roles mapped to agents, configuration as code, change tiers |
| [docs/analytics.md](docs/analytics.md) | On-demand reports and dashboards: how requests are answered, guardrails, components |
| [docs/architecture.md](docs/architecture.md) | Platform layers and full tech stack |
| [docs/migration.md](docs/migration.md) | Cutover protocol, migration modes, reconciliation controls |
| [docs/testing.md](docs/testing.md) | Functional and non-functional test suites, fuzzing, coverage gate |

## Quick start

```sh
make db-up      # PostgreSQL 17 in Docker on :55432
make test       # functional, end-to-end and resilience suites, race detector on
make coverage   # whole-suite statement coverage gate
make demo       # the CLI against a real API server: mock SAP -> migrate -> invoices -> agents -> analytics
make load       # the load suite against the p95/p99 budgets (make soak, make resilience likewise)
```

`erp` is a thin client of the API: everything it reads or changes goes
through `erp-server` with your token, under the same permissions, company
access and audit as any other client. Point it at a server with
`ERP_API_URL` (default `http://localhost:8080`), a person's bearer token in
`ERP_TOKEN` and the agents' token in `ERP_AGENT_TOKEN`.

A tenant is provisioned once by an operator, with its first administrator and
its first configuration; from then on the administrator manages it through
the API. Locally, the server can mint tokens for any subject
(`ERP_DEV_ISSUER=true`; never in production):

```sh
make db-up
export DATABASE_URL=postgres://postgres:dev@localhost:55432/postgres?sslmode=disable
export ERP_PAGE_TOKEN_KEY=$(head -c 32 /dev/urandom | base64) ERP_FIELD_KEY=$(head -c 32 /dev/urandom | base64)
export ERP_DEV_ISSUER=true ERP_MIGRATE_ON_START=true ERP_EXTRACT_DIR=$PWD/data ERP_OBJECT_DIR=$PWD/data/objects
go build -o bin/erp ./cmd/erp && go build -o bin/erp-server ./cmd/erp-server && go build -o bin/erp-router ./cmd/erp-router
mkdir -p data/objects && ./bin/erp gen-mock-sap -out data/extract.json
./bin/erp config generate -source file:data/extract.json -company 1000,2000 -out data/config.yaml
./bin/erp-server &                                   # migrates, then serves :8080 (API) and :9090 (admin)
./bin/erp-server provision -name Acme -region eu-west -currency EUR \
  -admin-subject ada -admin-name "Ada" -config data/config.yaml     # prints the tenant and administrator ids
export ERP_TOKEN=$(./bin/erp token -tenant <tenant_id> -subject ada)
./bin/erp call listLegalEntities                                    # the configuration's companies, active
./bin/erp call createConnector -body '{"kind": "sap_file", "name": "SAP PRD", "settings": {"path": "extract.json"}}'
./bin/erp migrate -connector <id> -legal-entities <id>,... -year 2026 -rehearsal   # load, reconcile, roll back
./bin/erp migrate -connector <id> -legal-entities <id>,... -year 2026              # fails unless the run reconciles
./bin/erp invoices                     # the rule: match parked invoices, post clean ones within the auto-post limit
./bin/erp call listRecommendations -p outcome=open
./bin/erp call listBalances -p legal_entity_id=<id> -p fiscal_year=2026
./bin/erp call verifyAuditChain -body '{}'   # from the last anchor the server wrote to object storage
```

The administrator holds the `tenant_admin` role (every permission) and
provisions colleagues and agents through the API (`erp call createPrincipal`,
`createRole`, `assignRole`); an agent's token (`ERP_AGENT_TOKEN`) is minted
for the agent principal's subject the same way.

Operators work through the control plane ([`api/control.yaml`](api/control.yaml))
on the admin listener instead of the server command: an operator token
(`erp token -subject ops`, no tenant) and `erp tenants`:

```sh
export ERP_OPERATOR_TOKEN=$(./bin/erp token -subject ops)
./bin/erp tenants create -name Beta -region eu-west -currency EUR \
  -admin-subject bea -admin-name "Bea" -config data/config.yaml   # same result as erp-server provision, with an Idempotency-Key
./bin/erp tenants list -status active
./bin/erp tenants suspend -id <tenant_id>     # the tenant's requests answer 403 tenancy.tenant_inactive until resume
./bin/erp tenants resume -id <tenant_id>
./bin/erp tenants rotate-key -id <tenant_id>  # new field-encryption key; bank details re-sealed under it
```

`erp call OPERATION` calls any operation of [the contract](api/openapi.yaml)
by its operationId: `-p name=value` for parameters, `-body` JSON or `@file`,
`-if-match` the ETag the answer printed.

Agents (need Anthropic credentials, `ANTHROPIC_API_KEY` or an `ant auth login`
profile; the default model is `claude-opus-5-5`, override it with
`ERP_AGENT_MODEL`) work through the API as their tools, in sessions recorded
through the API, and only make recommendations or change requests:

```sh
./bin/erp agent config -draft draft.yaml "raise the price tolerance in company 1000 to 3%"
./bin/erp agent migration -connector <id> -legal-entities <id>   # rehearsals only
./bin/erp agent test -change-request <id> -out uat.md                  # captures a regression suite when one is missing
./bin/erp call createRegressionSuite -body '{"name": "Baseline"}'    # pin today's invoice decisions for tier 3-4 changes
```

On-demand analytics reads the data you may see through the API (see
[docs/analytics.md](docs/analytics.md)):

```sh
./bin/erp metrics                            # governed metrics
./bin/erp query -measures revenue -by fiscal_year,fiscal_month -where "fiscal_month <= 9" -companies 1000
./bin/erp report -file examples/monthly-sales.json -companies 1000   # example dashboard, no agent
./bin/erp ask -companies 1000 "monthly sales vs last year this time"
```

Against a real S/4HANA system, create a `sap_odata` connector instead
(`settings`: `base_url`, `client`, `page_size`; `secret_ref`: `env:NAME` with
`user:password`). `config generate -source odata:https://my-s4.example.com`
reads customizing with `SAP_USER`, `SAP_PASSWORD` and `SAP_CLIENT`.

## Layout

| Package | Role (white-paper layer) |
|---|---|
| `internal/sap` | SAP table structs (`ACDOCA`, `LFA1`, `EKKO`, `RBKP`, ...), file and OData V2 sources, mock generator. *Composable Enterprise Platform* |
| `internal/integration` | Connectors and migration runs: quality gate, load through the other contexts' services, independent reconciliation. Two modes: `documents` (replay every journal entry) and `balances` (opening entries plus open items) |
| `internal/tenancy`, `identity`, `configuration` | Tenants and legal entities; principals, roles, company access and segregation of duties; configuration versions and change requests. *Governance & Security* |
| `internal/partner`, `ledger`, `procurement`, `payables`, `payments`, `approvals` | The system of record: business partners, double-entry ledger with open items and clearing, requisitions to receipts, supplier invoices and three-way match, payment runs, approval requests. *System of Record* |
| `internal/audit` | Audit events and verification of the hash chain |
| `internal/agents` | Agent sessions, their tool calls and recommendations, decided by people |
| `internal/apitools` | The public API as agent tools: one tool per operationId, never operations marked `x-agent: false`; sessions recorded through the API |
| `internal/agent` | The invoice exceptions agent: investigates held invoices and writes a recommendation for a person. *AI Agent + Orchestration* |
| `internal/delivery/*` | Configuration generated from SAP customizing, and the configuration, migration and test agents |
| `internal/analytics` | On-demand analytics: governed semantic layer, facts read through the API, query engine with row-level security, analytics agent, narrative check, dashboard renderer, saved reports |
| `internal/config` | Configuration as code: the typed YAML document, validators and change tiers; `internal/configuration` holds versions, change requests, evidence and the simulation worker |
| `internal/bankfile` | ISO 20022 pain.001 credit transfer files |
| `internal/capture` | Invoice capture: Claude structured extraction from PDF |
| `internal/llm` | Shared Claude agent plumbing; `llmtest` is a scripted Messages API for tests |
| `internal/nfr` | Load, soak and resilience suites over the whole stack; performance budgets of the client-side engines |
| `cmd/erp` | The CLI, a thin API client; `erp tenants` drives the control plane |

**Platform foundation** (API-first; design in [docs/design](docs/design/), ADR-0014 for the request pipeline):

| Package | Role |
|---|---|
| `api/openapi.yaml`, `api/asyncapi.yaml`, `api/control.yaml`, `api/directory.yaml` | The API, event, control-plane and global control-plane contracts; `api/gen` is the server generated from the API contract by ogen (`make generate`) |
| `db/migrations` | The data model as versioned PostgreSQL migrations, applied by the embedded runner (`db.Migrate`) |
| `internal/platform/*` | Request pipeline: `telemetry`, `authn` (OIDC), `ratelimit` (quotas per tenant and principal), `uow` (transaction, tenant and actor, idempotency, audit flush), `authz` (contract permissions), `problem` (RFC 9457), `audit` (hash chain), `outbox` (writer and relay), `natsbus` (JetStream), `blobstore` (write-once object storage: directory or S3), `secrets` (references to the environment, mounted files or Vault), `pagination`, `etag`, `config`, `httpapi` (wiring) |
| `internal/operations` | Long-running operations: the first context on the foundation (`/v1/operations`) |
| `internal/provisioning`, `internal/controlplane` | Tenant provisioning in one transaction, and the operators' control plane (`/control/v1/tenants`: list, create, suspend, resume, offboard) on the admin listener |
| `cmd/erp-server` | The API server: public listener, admin listener (`/livez`, `/readyz`, `/metrics`, `/control/v1`), outbox relay and workers (balance projector, payment proposer, configuration simulator, audit anchorer, offboarding, directory sync) |
| `internal/directory`, `internal/router`, `internal/globalplane`, `cmd/erp-router` | The global tenant directory (its own database, written by the cells), the router that verifies a request's token and forwards it to the tenant's cell, and the global control plane over the directory (ADR-0022, ADR-0023) |

## Guardrails built in

- **Money is never a float.** Amounts are integer minor units; quantities are integer milli-units.
- **The ledger validates everything**, including migrated SAP documents: balanced, known accounts, vendor on AP lines.
- **Agents cannot post exceptions.** Only a clean three-way match can be posted by a non-human, and only within the auto-post limit (default 50,000.00).
- **The exception agent has read-only and escalate tools only.** It cannot post, pay or change master data. Bank details are masked in what it sees, and vendor-supplied text is treated as untrusted data.
- **Segregation of duties** applies to agents: no identity can both post invoices and release payments.
- **Every decision is audited** in an append-only, SHA-256-chained log. `verifyAuditChain` detects edits and deletions.
- **Reconciliation recomputes the SAP side from the raw extract**, so a transform bug shows up as a difference instead of being copied into both sides.

## Running the API server

```sh
make db-up                      # PostgreSQL 17 in Docker on :55432
export ERP_OIDC_ISSUERS='[{"issuer":"https://your-idp.example.com","audience":"erp-api"}]'
export ERP_PAGE_TOKEN_KEY=$(head -c 32 /dev/urandom | base64)
export ERP_NATS_URL=nats://localhost:4222   # optional; without it events wait in the outbox
export ERP_OBJECT_DIR=$PWD/data/objects      # or ERP_OBJECT_S3_ENDPOINT, _BUCKET, _ACCESS_KEY, _SECRET_KEY for an S3-compatible store
make run-server                 # migrates, then serves :8080 (API) and :9090 (admin)
```

All settings are environment variables, documented in
[`internal/platform/config`](internal/platform/config/config.go): among them
the request quotas per tenant and principal (`ERP_RATE_LIMIT_TENANT`,
`ERP_RATE_LIMIT_PRINCIPAL`), the request deadline (`ERP_REQUEST_TIMEOUT`)
the pool size (`ERP_DB_MAX_CONNS`) and object storage (`ERP_OBJECT_DIR` or
`ERP_OBJECT_S3_*`: captured documents, bank files and the audit anchors the
server writes, locked for `ERP_OBJECT_RETENTION`). Field encryption uses a key per tenant,
wrapped by a master key: one in AWS KMS (`ERP_FIELD_KMS_KEY` with its
region and endpoint, [ADR-0019](docs/design/adr/0019-kms-master.md))
or the local masters in `ERP_FIELD_KEYS` (`id=base64,...`, the first for
new keys; `ERP_FIELD_KEY` alone is one master); operators rotate a
tenant's key through the control plane, and `erp-server rewrap` moves every
tenant's keys onto the current master. Settings that carry
secrets (`DATABASE_URL`, the keys, the S3 and KMS credentials) may name where the
secret lives instead: `env:NAME`, `file:/run/secrets/name` or
`vault:secret/erp/db#url` with `ERP_VAULT_ADDR` and a token or a Kubernetes
role ([ADR-0017](docs/design/adr/0017-secrets.md)); connectors' `secret_ref`
uses the same references. The S3 store and the KMS master need no access
keys at all on a deployment with roles: unset, they sign with the ambient
credentials (the `AWS_*` environment, a web identity exchanged at STS, the
instance metadata service; [ADR-0024](docs/design/adr/0024-ambient-aws-credentials.md)). The admin listener serves
`/livez`, `/readyz` (schema version and partition horizon), `/metrics`
(request latency per operation, worker backlogs) and, with
`ERP_OPERATOR_ISSUERS` set, the operators' control plane under
`/control/v1` ([ADR-0015](docs/design/adr/0015-control-plane.md)). With
`ERP_DIRECTORY_URL` and `ERP_CELL_API_URL` set, the cell records its tenants
in the global directory, and `erp-router` (same `ERP_OIDC_ISSUERS`,
`ERP_DIRECTORY_URL`, `ERP_ROUTER_ADDR`) forwards each request to its
tenant's cell ([ADR-0022](docs/design/adr/0022-tenant-directory-and-router.md));
with `ERP_OPERATOR_ISSUERS` and `ERP_PAGE_TOKEN_KEY` its admin listener
(`ERP_ROUTER_ADMIN_ADDR`) serves the global control plane
([`api/directory.yaml`](api/directory.yaml): tenants and cells across the
platform, provisioning placed in a cell of the region, moves between cells;
`erp directory`, [ADR-0023](docs/design/adr/0023-global-control-plane.md)).
Each cell registers itself with `ERP_CELL_REGION`, `ERP_CELL_CONTROL_URL`
and `ERP_CELL_ACCEPTING`. Tokens must
carry the tenant id in the `tenant_id` claim, and the subject must be a
provisioned principal of that tenant; operator tokens name no tenant and
work only on the control plane. Every operation in the contract is
served: implemented ones run, the rest answer `501` until their context moves
onto the foundation ([docs/design/07-transition.md](docs/design/07-transition.md)).

## Status

Phases 0 and 1 are built, and the prototype has moved onto the API platform
(transition steps T1 to T20 in [docs/design/07-transition.md](docs/design/07-transition.md)):
configuration as code with change requests and tiered approvals, the
configuration, migration and test agents, procure-to-pay (requisitions with
configured approvals, POs, goods receipts, governed supplier changes, supplier
invoices with three-way match, payment runs with ISO 20022 files), SAP
migration with reconciliation, and invoice capture. On-demand analytics is
built ahead of its Phase 2 slot. The Phase 1 exit criteria that need a client
(a pilot company code cut over, measured consultant hours) cannot be met on
mock data. See [docs/plan.md](docs/plan.md) and [docs/testing.md](docs/testing.md).
Known gaps:

- Tenants are provisioned, suspended, offboarded and erased by operators
  through the control plane on the admin listener (or `erp-server provision`
  to bootstrap); an offboarded tenant's data is exported to locked object
  storage and erased after `ERP_OFFBOARD_GRACE` (default 30 days,
  [ADR-0021](docs/design/adr/0021-offboarding-lifecycle.md)). There is no
  self-service signup, and the development issuer is for local use only.
- Documents above 5 MB go through `POST /v1/uploads` (a presigned `PUT`
  to the store, or a signed path on the API with a directory store), up to
  50 MB. Without object storage configured the server keeps only hashes,
  serves no uploads and does not anchor the audit chain.
- Single currency per company code. No FX revaluation yet.
- The OData entity and field mappings follow SAP API Business Hub names but have
  not yet been validated against a live system. Check them against your `$metadata`.
- Agents are tested against a scripted model; their behavior with the live API
  needs evals before any agent gets autonomy above L0.
- Tier 3 and 4 configuration changes need simulation and regression
  evidence; both replay supplier invoices only, so changes to other areas
  pass with nothing to decide on.
- The load suite meets its budgets at the rate a laptop or CI runner
  reaches (about 500 requests/s); the largest tenant's peak needs a
  cell-sized environment.
- No AR, inventory valuation, tax engine or payroll yet (later phases).
