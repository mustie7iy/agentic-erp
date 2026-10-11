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

At the time of writing the suites contain 305 test functions (many with
subtests) across 47 packages, all passing under the race detector.
Database-backed suites run against PostgreSQL (`make db-up`), each package
in its own database (`internal/platform/pgtest`), and
`apitest.EveryFailure` fails each database statement of an operation in
turn to prove nothing is left half done.

## Functional tests

| Area | Where | What is proven |
|---|---|---|
| Tenancy, identity, configuration | `internal/tenancy`, `identity`, `configuration` | Legal entities and org units, principals, effective-dated role and permission grants, company access, SoD refused at grant time and enforced per operation, configuration versions and change requests by tier |
| Configuration as code | `internal/config` | Every validation rule (one test per rule), change tiers, diffs |
| Configuration from SAP | `internal/delivery/sapconfig` | T001/SKB1/T030/T007A/T169G/T012K/T052/AGR_* mapped; number ranges per fiscal year; gaps, defaults and role notes |
| Partner and ledger | `internal/partner`, `internal/ledger` | Governed partner changes, encrypted bank accounts; balanced postings in three currencies, reversal, periods, projector balances, open items, clearing, settlement with realized exchange differences |
| Procure-to-pay | `internal/procurement`, `payables`, `approvals` | Requisition → approval → PO → receipt → invoice → match → posting; contracts; over-delivery; exceptions decided on approval requests; no self-approval |
| Payments | `internal/payments` | Proposal worker with every exclusion, release by another person, payment entries clearing the items, bank files and transmission |
| Migration | `internal/integration` | Connectors, quality gate, both modes, loads through the owning services, refused records failing the run, rehearsals rolled back, idempotent reruns, reconciliation with differences and missing documents |
| Agents runtime | `internal/agents`, `internal/apitools` | Sessions, tool calls and recommendations through the API; tools generated from the contract, `x-agent: false` refused, every call recorded |
| Agents | `internal/agent`, `configagent`, `migrationagent`, `testagent` | Full tool loops against a scripted model through the real API; agents only recommend or propose; the migration agent starts rehearsals only |
| Audit | `internal/audit`, `internal/platform/audit` | Audit events listed, the hash chain verified over a range, tampering reported |
| Invoice capture | `internal/capture` | Structured-output request shape, prompt injection inert |
| Bank files | `internal/bankfile` | pain.001 structure, control sums, IBAN vs domestic accounts, escaping, schema field lengths |
| Analytics | `internal/analytics` | Facts read through the API, semantic queries, guardrails, row-level security, narrative check, rendering, reruns |
| End to end | `cmd/erp/e2e_test.go` | The CLI against a real API server: mock SAP extract, configuration draft, a scope migrated and reconciled through the integration API, the invoice rule, all four agents through API tools, analytics |

## Non-functional tests

| Suite | What is proven |
|---|---|
| Performance (`perf_test.go`, `make perf`) | Budgets for analytics over a large fact set and configuration validation with 2k number ranges; database budgets live with their contexts (trial balance, payment proposal over 100,000 items) |
| Concurrency, reliability, security | Per context on PostgreSQL: every statement failing in turn (`EveryFailure`), the race detector, permission, company access and SoD checks on every operation |
| Fuzzing (per package) | Amount parsing round-trips; arbitrary configuration YAML never panics validation or diff; any commentary text is safe for the narrative check; bank files stay well-formed XML with exact control sums for any names; OData values |

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
