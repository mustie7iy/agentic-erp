# Code-quality refactoring progress (target 9.5/10)

Programme: `docs/refactoring-prompt.md`. Branch: `refactor/quality-9.5`.
Each step: lint before → after for its scope, tests, reviewer verdict, commit.

## Steps

### Phase 0 — Baseline
- [x] R0a Tune errcheck exclusions (fmt.Fprint*, ResponseWriter.Write, pgx Rollback) — 9a99827.
  Whole tree 1238 → 1063, errcheck 491 → 314. Also: make lint/lint-all leave out api/gen
  (analysing it took > 30 min; totals identical).
- [x] R0b Record baseline; add R-items to docs/tracker.json (Quality engineering →
  Code-quality refactoring programme)

### Phase 1 — Correctness bugs
- [ ] R1 approvals escalation guarded by expected count (idempotent activity)
- [ ] R2 approvals workflow branches on GetVersion
- [ ] R3 outbox relay: event type without a dot
- [ ] R4 reqctx.Request errors returned, not discarded
- [ ] R5 Fail closed in production (codec key, dev token issuer)
- [ ] R6 nilerr, errorlint, staticcheck, bodyclose, exhaustive to zero

### Phase 2 — Shared helpers
- [ ] R7 etag.IfMatch replaces the per-package ifMatch copies
- [ ] R8 One helper for injectable Begin / Now
- [ ] R9 Remaining dupl findings

### Phase 3 — Error context
- [ ] R10 wrapcheck: internal/platform/*
- [ ] R11 wrapcheck: internal/ledger
- [ ] R12 wrapcheck: internal/payments
- [ ] R13 wrapcheck: internal/procurement
- [ ] R14 wrapcheck: internal/payables
- [ ] R15 wrapcheck: internal/partner
- [ ] R16 wrapcheck: internal/configuration
- [ ] R17 wrapcheck: internal/integration
- [ ] R18 wrapcheck: internal/analytics
- [ ] R19 wrapcheck: remaining internal packages and cmd/*

### Phase 4 — Long and complex functions
- [ ] R20 cmd/erp-server run() split into named steps
- [ ] R21 analytics Execute, config Load, integration quality checks, payables validate
- [ ] R22 Remaining funlen and gocognit to zero

### Phase 5 — Test quality
- [ ] R30 Polling helper; no time.Sleep in tests
- [ ] R31 t.Run subtests and one assertion per check
- [ ] R32 thelper and noctx to zero
- [ ] R33 Nightly shuffled test run

### Phase 6 — Make it permanent
- [ ] R40 CI lints the whole tree
- [ ] R41 Import-rule test; §6.7 describes the real layout
- [ ] R42 Final verification and scored review

## Baseline

Recorded 2026-10-12 at 9a99827 (R0a config), PostgreSQL 17 via `make db-up`.

- Tests: `make test` (vet + `go test -race -count=1 ./...`) passes, 61 packages, 1m51s.
- Coverage: `make coverage` 100.0% of production statements (minimum 100.0%).
- Lint: `make lint-all` (every package except api/gen). "Before R0a" is the original
  configuration; production code means files other than `_test.go`.

| Linter | Before R0a | After R0a | of which production code |
|---|---:|---:|---:|
| wrapcheck | 496 | 496 | 496 |
| errcheck | 491 | 314 | 64 |
| thelper | 61 | 61 | 39 |
| gocognit | 50 | 50 | 50 |
| forbidigo | 46 | 46 | 12 |
| noctx | 38 | 38 | 8 |
| funlen | 16 | 16 | 16 |
| staticcheck | 11 | 13 | 3 |
| errorlint | 11 | 11 | 3 |
| bodyclose | 6 | 6 | 0 |
| dupl | 4 | 4 | 4 |
| exhaustive | 3 | 3 | 3 |
| gocritic | 3 | 3 | 1 |
| nilerr | 2 | 2 | 2 |
| **total** | **1238** | **1063** | **701** |

| Package | total | wrap | errc | thlp | cogn | forb | noctx | flen | stat | elint | body | dupl | exh | crit | nilerr |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| api | 3 |  |  | 1 |  | 1 |  |  | 1 |  |  |  |  |  |  |
| cmd/erp | 72 | 41 | 26 |  | 2 | 1 |  | 1 |  |  |  |  |  |  | 1 |
| cmd/erp-router | 16 | 2 | 6 |  |  |  | 5 | 1 |  |  | 2 |  |  |  |  |
| cmd/erp-server | 72 | 3 | 38 | 1 | 1 | 11 | 14 |  |  |  | 4 |  |  |  |  |
| db | 33 | 7 | 26 |  |  |  |  |  |  |  |  |  |  |  |  |
| internal/agents | 10 | 10 |  |  |  |  |  |  |  |  |  |  |  |  |  |
| internal/analytics | 30 | 11 | 7 |  | 9 |  |  | 1 |  |  |  |  | 2 |  |  |
| internal/apitools | 6 | 1 | 3 |  | 2 |  |  |  |  |  |  |  |  |  |  |
| internal/approvals | 14 | 11 |  | 1 |  |  |  |  | 2 |  |  |  |  |  |  |
| internal/audit | 30 | 15 | 7 | 1 |  | 3 |  | 1 | 1 | 1 |  |  |  |  | 1 |
| internal/capture | 1 | 1 |  |  |  |  |  |  |  |  |  |  |  |  |  |
| internal/config | 4 | 1 |  |  | 2 |  |  |  |  |  |  |  | 1 |  |  |
| internal/configuration | 37 | 33 | 1 |  |  | 1 |  | 2 |  |  |  |  |  |  |  |
| internal/controlplane | 19 | 8 | 7 |  |  | 2 | 1 |  | 1 |  |  |  |  |  |  |
| internal/delivery/configagent | 1 |  | 1 |  |  |  |  |  |  |  |  |  |  |  |  |
| internal/delivery/migrationagent | 1 |  | 1 |  |  |  |  |  |  |  |  |  |  |  |  |
| internal/delivery/sapconfig | 3 |  | 2 |  | 1 |  |  |  |  |  |  |  |  |  |  |
| internal/directory | 26 | 12 | 13 |  |  | 1 |  |  |  |  |  |  |  |  |  |
| internal/globalplane | 15 |  | 11 |  |  |  | 1 | 1 |  | 1 |  |  |  | 1 |  |
| internal/identity | 24 | 22 |  |  |  |  |  |  |  |  |  | 2 |  |  |  |
| internal/integration | 27 | 15 |  | 1 | 7 | 2 |  |  |  |  |  | 2 |  |  |  |
| internal/ledger | 92 | 59 | 12 | 9 | 5 | 2 |  | 3 |  |  |  |  |  | 2 |  |
| internal/llm | 3 | 2 |  |  |  | 1 |  |  |  |  |  |  |  |  |  |
| internal/llm/llmtest | 4 |  | 4 |  |  |  |  |  |  |  |  |  |  |  |  |
| internal/nfr | 16 |  | 3 | 4 |  | 7 |  | 1 | 1 |  |  |  |  |  |  |
| internal/offboarding | 39 | 22 | 11 |  | 1 | 2 |  | 2 | 1 |  |  |  |  |  |  |
| internal/operations | 7 | 3 | 4 |  |  |  |  |  |  |  |  |  |  |  |  |
| internal/partner | 39 | 36 | 1 |  | 2 |  |  |  |  |  |  |  |  |  |  |
| internal/payables | 40 | 25 | 6 |  | 7 |  | 1 |  | 1 |  |  |  |  |  |  |
| internal/payments | 52 | 44 | 2 | 1 |  | 1 |  | 1 |  | 3 |  |  |  |  |  |
| internal/platform/audit | 6 | 1 | 4 |  | 1 |  |  |  |  |  |  |  |  |  |  |
| internal/platform/authn | 5 | 2 | 3 |  |  |  |  |  |  |  |  |  |  |  |  |
| internal/platform/authn/authntest | 4 |  | 2 | 2 |  |  |  |  |  |  |  |  |  |  |  |
| internal/platform/awscreds | 9 |  | 9 |  |  |  |  |  |  |  |  |  |  |  |  |
| internal/platform/blobstore | 24 |  | 21 |  |  |  | 2 |  |  | 1 |  |  |  |  |  |
| internal/platform/blobstore/blobstoretest | 2 |  |  | 1 | 1 |  |  |  |  |  |  |  |  |  |  |
| internal/platform/config | 3 |  | 2 |  | 1 |  |  |  |  |  |  |  |  |  |  |
| internal/platform/database | 7 | 1 | 6 |  |  |  |  |  |  |  |  |  |  |  |  |
| internal/platform/fieldcrypt | 24 | 7 | 12 |  |  |  | 1 |  |  | 4 |  |  |  |  |  |
| internal/platform/fieldcrypt/kmstest | 6 |  | 4 | 1 |  |  |  | 1 |  |  |  |  |  |  |  |
| internal/platform/fixture | 8 |  |  | 8 |  |  |  |  |  |  |  |  |  |  |  |
| internal/platform/httpapi | 1 |  |  |  |  |  |  |  | 1 |  |  |  |  |  |  |
| internal/platform/httpapi/apitest | 15 | 3 |  | 11 |  |  |  |  | 1 |  |  |  |  |  |  |
| internal/platform/natsbus/natstest | 6 |  | 1 | 4 |  |  | 1 |  |  |  |  |  |  |  |  |
| internal/platform/outbox | 11 | 2 | 8 |  |  | 1 |  |  |  |  |  |  |  |  |  |
| internal/platform/pagination | 1 | 1 |  |  |  |  |  |  |  |  |  |  |  |  |  |
| internal/platform/pgtest | 15 | 3 | 6 | 6 |  |  |  |  |  |  |  |  |  |  |  |
| internal/platform/problem | 2 |  | 1 |  |  |  |  |  |  | 1 |  |  |  |  |  |
| internal/platform/secrets | 4 | 1 | 2 |  |  |  |  |  | 1 |  |  |  |  |  |  |
| internal/platform/secrets/vaulttest | 4 |  | 3 | 1 |  |  |  |  |  |  |  |  |  |  |  |
| internal/platform/sigv4 | 9 |  |  |  |  |  | 9 |  |  |  |  |  |  |  |  |
| internal/platform/telemetry | 2 | 1 | 1 |  |  |  |  |  |  |  |  |  |  |  |  |
| internal/platform/uow | 21 | 8 | 11 | 1 |  | 1 |  |  |  |  |  |  |  |  |  |
| internal/platform/workflow | 4 | 2 |  |  |  | 2 |  |  |  |  |  |  |  |  |  |
| internal/platform/workflow/workflowtest | 5 |  | 1 | 2 |  |  | 2 |  |  |  |  |  |  |  |  |
| internal/procurement | 50 | 42 | 1 |  | 4 | 2 |  |  | 1 |  |  |  |  |  |  |
| internal/provisioning | 7 | 2 | 2 | 2 | 1 |  |  |  |  |  |  |  |  |  |  |
| internal/router | 4 |  | 3 |  |  |  | 1 |  |  |  |  |  |  |  |  |
| internal/sap | 19 | 6 | 8 |  | 3 | 2 |  |  |  |  |  |  |  |  |  |
| internal/tenancy | 12 | 10 | 1 | 1 |  |  |  |  |  |  |  |  |  |  |  |
| internal/workflows | 37 | 20 | 10 | 2 |  | 3 |  | 1 | 1 |  |  |  |  |  |  |

Abbreviations: wrap wrapcheck, errc errcheck, thlp thelper, cogn gocognit, forb forbidigo,
flen funlen, stat staticcheck, elint errorlint, body bodyclose, exh exhaustive, crit gocritic.

Not assigned to any step in the brief (for R40 "every linter at zero" they need an owner):
- errcheck 314 (64 production: listener/file `Close`, `Shutdown`, multipart writes,
  `json.Unmarshal`, `Exec`; 250 in tests).
- forbidigo in production, 12: `float64` in cmd/erp-server (metrics), cmd/erp/analytics.go,
  internal/integration (connectors, runs), internal/sap/odata.go; `panic` in api/spec.go,
  internal/llm/llm.go, internal/sap/mock.go, internal/nfr/harness.go. (The 34 in tests are
  `time.Sleep`, owned by R30.)
- gocritic 3 (1 production: argOrder in internal/globalplane/globalplane.go:43).
- staticcheck: 3 production + 10 test style findings (QF1008, ST1023, QF1001), owned by R6.

## Found during refactoring

(none yet)
