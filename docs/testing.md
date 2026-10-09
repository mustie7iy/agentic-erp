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

At the time of writing the suites contain 342 tests (including subtests)
across 24 packages, all passing under the race detector.

## Functional tests

| Area | Where | What is proven |
|---|---|---|
| Ledger, AP, three-way match | `internal/core` | Balanced postings, vendor rules, every match issue, tolerances, index refresh |
| Procurement and payments (core) | `internal/core/procure_test.go` | Contract pricing/validity/target value, receipts and reversals, over-delivery, payment proposals with every exclusion, reserved items, all-or-nothing execution |
| Procure-to-pay service | `internal/p2p` | Requisition → approval → PO → receipt → invoice → posting; two-step approvals; contracts; vendor creation and bank changes with approval; payment holds; block/unblock; release failure rollback; EDI output |
| Approval workflow | `internal/workflow` | Sequential steps, self-approval, agents cannot approve, double approval, reject/cancel |
| Configuration as code | `internal/config` | Every validation rule (one test per rule), change tiers, full change lifecycle, evidence gates, stale changes, rollback, tamper detection, persistence |
| Configuration from SAP | `internal/delivery/sapconfig` | T001/SKB1/T030/T007A/T169G/T012K/T052/AGR_* mapped; gaps and defaults noted; the SAP role design's SoD conflict surfaced |
| Simulation and regression | `internal/delivery/simulate`, `regress` | Sandbox replay never touches live data; decision and ledger impact of a change; stratified suites cover every decision and issue kind; suites replay their own snapshot |
| Evidence | `internal/delivery/changes` | Tier 3 changes become approvable only with simulation and regression evidence; stale suites refused |
| Delivery agents | `configagent`, `migrationagent`, `testagent` | Full tool loops against a scripted model; tool surfaces fixed; agents never approve, activate or load into production |
| Invoice capture | `internal/capture` | Vendor/PO matching, arithmetic and tax-rate checks, low confidence held, duplicates refused, prompt injection inert, structured-output request shape |
| Migration | `internal/migration` | Quality gate, both modes, reconciliation, idempotent reruns, account classification confidence, document-level difference explanations, cleansing proposals |
| Bank files | `internal/bankfile` | pain.001 structure, control sums, IBAN vs domestic accounts, escaping, schema field lengths |
| Analytics | `internal/analytics` | Semantic queries match the ledger, guardrails, row-level security, narrative check, rendering, reruns |
| End to end | `cmd/erp/e2e_test.go` | All of Phase 1 through the CLI: migrate, generate configuration from SAP, fix SoD, bootstrap, tier-3 change with evidence and two approvals, activation, rollback, invoices, procure-to-pay, supplier changes, payment with bank file, capture, all three delivery agents, audit verification |

## Non-functional tests (`internal/nfr`)

| Suite | What is proven |
|---|---|
| Performance (`perf_test.go`) | Budgets for the invoice pipeline (2k invoices against 20k history), payment proposal over 20k open items, payment execution for 1k vendors, migration, analytics over 60k journal lines, configuration validate+diff with 2k users; benchmarks for matching and queries. These budgets found an O(n²) match (29 s, now 74 ms) and a per-vendor journal scan (3.7 s, now 13 ms). |
| Concurrency (`concurrency_test.go`, run with `-race`) | Mixed workload on one store; ten simultaneous payment proposals claim items once; two releasers post a run once; twenty concurrent configuration changes, one activation wins |
| Security (`security_test.go`) | Exhaustive authorization matrix over every configured user and P2P operation; agents can never approve anything; SoD enforced at run time even if validation were bypassed; audit-trail edits and deletions detected |
| Reliability (`reliability_test.go`) | Atomic snapshots survive a crashed write; failed saves keep the old file; corrupt snapshots are errors; an unauditable action fails instead of happening silently; simulation, suite capture and migration are deterministic; Phase 1 records round-trip through snapshots |
| Fuzzing (per package) | Amount parsing round-trips; arbitrary configuration YAML never panics validation or diff; any commentary text is safe for the narrative check; bank files stay well-formed XML with exact control sums for any names; OData values; capture never posts whatever the model returns |

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
