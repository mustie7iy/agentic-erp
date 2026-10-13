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
- [x] R1 approvals escalation guarded by expected count (idempotent activity).
  TestEscalateTwice failed first (2 escalations/events/audits, want 1), passes now. Unexplained
  count fails the activity non-retryably. No GetVersion bump: commands unchanged (reviewer agreed).
  Lint approvals+workflows 51 → 51 (new code clean; one justified nolint:wrapcheck on a
  Temporal error constructor). Reviewer: APPROVE.
- [x] R2 approvals workflow branches on GetVersion. Version 2 passes the R1 guard's count;
  Default/1 replay the T29 input (kept unguarded: their count may already be inflated).
  TestApprovalDeadlineReplay replays a history recorded from T29 (18c4f10) on a dev server;
  TestApprovalDeadlineVersions fails on the pre-R2 code. R1 and R2 must ship together.
  Lint approvals+workflows 51 → 51. Reviewer: APPROVE.
- [x] R3 outbox relay: event type without a dot. Published under the name it has (rejecting
  would stall the context's ordered queue; ADR-0007). TestRelayPublishesAnUnversionedType: the
  old code panicked at relay.go:111. Lint outbox 11 → 11. Reviewer: APPROVE.
- [x] R4 reqctx.Request errors returned, not discarded. 15 sites: 13 helpers now receive tx and
  principal from the caller that already checked the request (a second lookup would be an
  unreachable branch); partner CreateBusinessPartner and payables CreateUpload/GetUpload check
  the request first. TestOutsideARequest / TestUploadsOutsideARequest are regression guards:
  no site was reachable outside a request before, so they pass on the old code too. Needed R22a
  first (funlen on the changed signatures). Lint: whole tree unchanged except R22a's −2.
  Reviewer: APPROVE.
- [x] R4b reqctx.PrincipalFrom / TxFrom ok flags not discarded (found in R4). All seven: the
  helpers (payments createFile, render chain; procurement insertMovement, setStatus; payables
  transition; approvals complete; authz checkSoD) receive the principal, tenant or tx from callers
  that hold it; no exported signature changed. No reachable failure before, so no failing-first
  test; make test passes. Reviewer: APPROVE (after widening from four sites to all seven).
- [x] R5 Fail closed in production (codec key, dev token issuer). ERP_ENV (development default |
  production); production refuses ERP_DEV_ISSUER and an engine without ERP_TEMPORAL_CODEC_KEY.
  TestProductionFailsClosed (9 cases); its refusal cases fail with the check disabled. Lint
  config unchanged. Reviewer: APPROVE.
- [x] R6 nilerr, errorlint, staticcheck, bodyclose, exhaustive to zero (R6a production, R6b tests).
  35 → 0 (nilerr 2, errorlint 11, staticcheck 13, bodyclose 6, exhaustive 3). One justified
  nolint (problem.Cause identity check). Behaviour change: S3 ErrLocked and KMS ErrCorrupt keep
  their cause in the chain (tests added). Reviewer: APPROVE (R6b after one fix: globalplane
  assertion kept strict with errors.Is plus not-a-problem).

### Phase 2 — Shared helpers
- [x] R7 etag.IfMatch replaces the per-package ifMatch copies: 11 copies deleted, 51 calls
  moved; parameters named ifMatch untouched. Lint unchanged (one-liners were below dupl's
  threshold). Reviewer: APPROVE.
- [x] R8 One helper for injectable Begin / Now
  - [x] R8a database.BeginFunc and BeginOr: 16 begin methods delegate in one line, 16 fields
    typed BeginFunc. Lint unchanged. Reviewer: APPROVE.
  - [x] R8b internal/platform/clock.Now: 13 now() methods delegate, 7 inline fallbacks and 3
    variants (awscreds, blobstoretest, payables simulate) replaced; no `.Now != nil` left.
    Reviewer: APPROVE. Noted: some code reads time.Now() with no Now field (controlplane
    rotator, procurement expiry, fieldcrypt keyring, analytics session/render): R30 candidates.
- [x] R9 Remaining dupl findings
  - [x] R9a identity listGrants, integration runStep: dupl 4 → 0. Reviewer: APPROVE.
  - [x] R9b audit.Record and audit.ObjectTypeOf replace ten copied record helpers (four deleted,
    41 calls moved; six delegate with their object-type rule); the panicking LastIndex copies
    from R3's note are gone. Reviewer: APPROVE.

### Phase 3 — Error context
Convention (settled during batch 1): a row scanner wraps as `scan <kind>` without the package
prefix and its caller adds `<pkg>: <operation> <key id>`; internal/platform/errs.Wrapf keeps a
single-return path whose failure no test can provoke from becoming its own branch.
- [x] R10 wrapcheck: internal/platform/* 32 → 0 (worktree agent, 155c6eb; follow-up: Ping and
  Worker error paths tested, relay and keyring messages, two nolints replaced by code; errs.Wrapf
  added). Reviewer: APPROVE after the coverage fix.
- [x] R11 wrapcheck: internal/ledger 59 → 0 (worktree agent, e8a5c92). Reviewer: APPROVE.
- [x] R12 wrapcheck: internal/payments 40 → 0 (worktree agent, b2e7339; follow-up: local wrapf removed
  for errs.Wrapf on scanners and covered branches elsewhere, load/getFile/createFile name their
  ids, pgtest.FailingTx.FailCommit tests the proposer's failed commit). Reviewer: changes required,
  then fixed.
- [x] R13 wrapcheck: internal/procurement 42 → 0 (worktree agent; cherry-picked as a99637e, re-verified
  on the branch; expiry message de-duplicated). Reviewer: APPROVE.
- [x] R14 wrapcheck: internal/payables 25 → 0 (worktree agent, 098db94; match insert named in the review follow-up). Reviewer: APPROVE.
- [x] R15 wrapcheck: internal/partner 36 → 0 (worktree agent, b72cd60; date parse error no longer echoes input). Reviewer: APPROVE.
- [x] R16 wrapcheck: internal/configuration 31 → 0 (worktree agent, a5af3f3; failed-commit test asserts the job stays queued). Reviewer: APPROVE.
- [x] R17 wrapcheck: internal/integration 15 → 0 (worktree agent, 82375bd; write paths named in the review follow-up). Reviewer: APPROVE after fixes.
- [x] R18 wrapcheck: internal/analytics 11 → 0 (worktree agent, 72218e0; render and page-save failures tested in the review follow-up). Reviewer: APPROVE after fixes.
- [x] R19 wrapcheck: remaining internal packages and cmd/* (worktree agents; reviewer APPROVE after fixes)
  - R19a identity 22, directory 12, audit 15, offboarding 22, workflows 20 → 0. Follow-ups: 27
    errs.Wrapf single returns made plain branches; workflows' nolints checked against the Temporal
    SDK (7 replaced by wraps, 7 workflow results kept with accurate reasons, 2 error constructors);
    tests for branches only the exact count exposed.
  - R19b approvals, agents, tenancy, controlplane, provisioning, operations, sap, llm, apitools,
    capture, config, db: 62 → 0, no nolint.
  - R19c cmd/erp 41, erp-server 3, erp-router 2 → 0 (parseFlags helper for the 27 flag parses).
  - Gate: scripts/coverage.sh compared a total rounded to 0.1%, hiding up to ~8 uncovered
    statements (2 at the branch start, up to 12 mid-phase). It now counts statements exactly:
    16367 of 16367.
  Phase 3 total: wrapcheck 490 → 0 across the tree.

### Phase 4 — Long and complex functions
- [ ] R20 cmd/erp-server run() split into named steps
- [ ] R21 analytics Execute, config Load, integration quality checks, payables validate
- [ ] R22 Remaining funlen and gocognit to zero
  - [x] R22a (pulled forward for R4) configuration SimulationWorker.process → process/evidence/
    advance; payments build → build/propose/proposal.insert. funlen −2. Reviewer: APPROVE.
- [ ] R23 Unowned findings to zero: errcheck, forbidigo (production float/panic), gocritic
  (added at the end of Phase 0 with the user's agreement)

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

## Decisions

- 2026-10-13, end of Phase 0: R5 adds `ERP_ENV` (development | production) to
  internal/platform/config; production refuses to start with the dev token issuer or without the
  Temporal codec key. R23 owns the findings the brief left unassigned.
- R5: ERP_ENV defaults to development (local runs, tests and the README flows unchanged), so the
  protection is opt-in: every production deployment manifest must set ERP_ENV=production. The
  server logs its environment at start and its two insecure-setting warnings name ERP_ENV.

## Found during refactoring

- R1: `ERP_APPROVAL_ESCALATIONS` has no upper bound (internal/platform/config); above 32767 the
  workflow's int16 count wraps and the smallint column overflows. Fix: cap it in config (own
  step, behaviour change).
- R19c: cmd/erp queryCmd ignores tw.Flush() and closes the -csv file with a plain defer, so a
  failed close (truncated CSV) goes unreported; token loses the JSON decode error behind "answered
  %d". Both are errcheck/R23 work.
- R3: the same `x[:strings.LastIndex(x, ".")]` pattern derives ObjectType in
  internal/payments/payments.go:305 and internal/integration/integration.go:137 (only literal
  inputs today, so no panic is reachable): a copied helper for R9 / Phase 2.
- R4: `p, _ := reqctx.PrincipalFrom(ctx)` / `tx, _ := reqctx.TxFrom(ctx)` discard the ok flag in
  7 places: payments/release.go:347 and :376, platform/authz/authz.go:101,
  procurement/receipts.go:132, procurement/requisitions.go:368, payables/invoices.go:216,
  approvals/approvals.go:316: step R4b.
- R4: TestMigrationWorkflow failed once in the stop hook (Temporal test suite's default 3 s
  real-time timeout) while a reviewer ran the same database-heavy packages concurrently; it
  passed 5× alone and 3× under the hook's load with and without the change. Tests whose
  activities hit the database should set a test timeout: R30.
- Phase 3: internal/nfr TestLoadShedsAHotPrincipal fails under concurrent load (relies on the
  rate limiter refilling at 20/s; slow requests are never refused); passes alone: R30.
- Phase 4 (analytics, found while splitting; not fixed):
  - cellEval.eval keeps the last currency of a cell's map for the divisor: a Count metric
    with aggregate sum over facts in, say, JPY and USD is divided by 1 or 100 depending on map
    order (Money and Ratio end in the mixed-currency error; Count is exempt). Real bug.
  - Model.index checks defaults ranging over a map: with several invalid defaults the error
    reported varies between runs.
  - Render indexes columns without checking for -1 and ignores r.sum errors; it relies on
    Spec.Validate having run.
  - aggCell.seen is set and never read; orderRows computes `less` before the Desc check.
- Phase 4 (integration, sap, apitools, sapconfig, found while splitting; not fixed):
  - apitools contract.parameter: a parameter with a description but no schema panics (nil map
    write); several unchecked type assertions panic on a malformed contract.
  - sap ODataSource.fetch puts company code and year into $filter as '%s' without escaping
    quotes.
  - sapconfig invoiceTolerance ignores fmt.Sscanf's error: a PROZ of "2.5" becomes 2%, a
    non-number 0%, with no note.
  - sapconfig alpha turns an all-zero account into "", integration's alpha into "0".
  - integration reconcile recomputes duplicateInvoices(ext) for every company code (slow only).
- R8a: TestResilienceWorkerRestarts failed once in the full -race suite ("an entry answered 201
  without a readable id"); it passed 4× alone with and 4× without the change. The harness
  (internal/nfr/harness.go Consistent) comments that such a 201 is "counted, not fatal" yet adds
  it to the errors that fail the run: a flaky check (or a lost response body under load) to look
  at in R30.
- Phase 3: TestKeyRotationPolicy (internal/controlplane) fails under heavy parallel load; it
  waits time.Sleep(50ms) before checking a log: R30.
- Phase 3: five test packages (procurement, offboarding, workflows, controlplane, audit) each
  define a private brokenCommit; pgtest.FailingTx.FailCommit can replace them (R31). A second
  mode that commits and then reports failure would let run-twice tests cover a lost commit.
- Phase 3: a go-reviewer subagent ran `git checkout` in the shared checkout during a review and
  restored it; no commit was lost. Reviews now run on `git show`/exports and are told not to
  check out.
