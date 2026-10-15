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
- [x] R20 cmd/erp-server run() split into named steps: a server value with openPool, prepareSchema,
  authenticate, openObjects, openDirectory, controlPlane, dialEngine, newAPI, startWorkers, connectBus,
  startRelay, listen, serve, wait (complexity 49 → 13); startup and close order unchanged, tests
  unchanged.
- [x] R21 analytics Execute 127 → 2, config Load 96 → a loader of named steps, integration
  checkQuality 84 → one function per table, payables validate 81 → 11.
- [x] R22 Remaining funlen and gocognit to zero (64 findings; four worktree agents, each merge
  reviewed twice for the money and startup paths). Coverage 16784 of 16784 statements.
  - [x] R22a (pulled forward for R4) configuration SimulationWorker.process → process/evidence/
    advance; payments build → build/propose/proposal.insert. funlen −2. Reviewer: APPROVE.
- [x] R23 Unowned findings to zero: errcheck, forbidigo (production float/panic), gocritic
  (added at the end of Phase 0 with the user's agreement). Four worktree agents; every merge
  reviewed (tests: APPROVE; production: APPROVE on the second pass).
  - Production: errcheck 57 → 0, forbidigo 10 → 0 (each nolint with its reason), gocritic 1 → 0;
    every dropped error carries a reason; llm.Must (unused) removed.
  - Tests: errcheck 247 → 0 and gocritic 2 → 0 across cmd/*, db and internal/*: setup the test
    relies on now stops it with the step named.
  - Fixes found on the way, each with a failing-first test: directory tenant list status filter
    (200 → 400), `erp call` unreadable answer, `erp migrate` report print failures (whole report,
    via bufio.Writer), SAP OData numbers keep every digit (and a long or out-of-range literal is
    kept), pgtest reports a failed database drop.
  - Fixes the user asked for after the agents' reports: blobstore Dir.Put writes a temporary file
    and links it under the key (a key holds a whole object or nothing; erasure removes leftovers,
    also under a locked key and with "#" in Root); db/migrate unlock gives up after 10s.
  - Lint tree-wide 451 → 132 (left: forbidigo 34 test time.Sleep → R30, thelper 62 and noctx 36
    → R32). Coverage 16947 of 16947 statements; make test passes with -race.
  - Deferred (logged): Dir sidecar written after the link (an anchor's retention can be lost on
    a retried Put), no directory fsync after the link, temporary name length (245+ byte key
    segments); cmd/erp other writes to out ignore errors (dev token at main.go, about 15 status
    lines); pgtest scan helper (about 45 repeated admin Scan checks → R31); globalplane and
    controlplane test do() helpers duplicated → apitest.

### Bug fixes found during Phases 3–4 (user: "fix all identified bugs first", before R23) — done
Four worktree agents, each fix with a failing-first test, every merge reviewed on the branch.
Final: make test passes with -race, coverage 16899 of 16899 statements, all 7 fuzz targets clean.
Each a separate Fix commit with a test that fails before the fix.
- [x] B1 SECURITY offboarding import: manifest table names checked and quoted. a41300f, 23ab2d6; reviewed as security: no manifest string reaches SQL.
- [x] B2 analytics: Count sum over several currencies divided by a map-order currency. ed80ceb, b4fd1a7 and a count-only-ratio follow-up: a sum over several currencies is refused for every metric type; count ratios may span currencies; money columns labelled from their sums.
- [x] B3 analytics: nondeterministic default and commentary errors; dead aggCell.seen. 01000be.
- [x] B4 erp-server: workers stopped before the pool closes when the bus fails at startup. 6f37e39, 6f0e32c, c435017 (listener-failure twin found in review), 172901b.
- [x] B5 config: ERP_APPROVAL_ESCALATIONS capped to what the smallint count holds. 84898b8.
- [x] B6 config: directory setting errors in a fixed order. e3e01bf.
- [x] B7 cmd/erp: CSV close/flush and token decode errors reported. 7d15587, 35deea1.
- [x] B8 nfr harness: cleanup registered before workers start. 9a9463c, c754f71, d1efd35, b326467.
- [x] B9 ledger createClearing: deterministic partner in the net-zero error. 2fec07b.
- [x] B10 apitools: malformed contract refused instead of panicking. 04f37e2, 82d15c9 ($ref cycle), 5044241 (dangling $ref).
- [x] B11 sap: OData $filter values quote-escaped. 4e35fc5, FuzzODataString in make fuzz.
- [x] B12 sapconfig: unparseable tolerance and payment-term numbers reported. fa59178, 8234aa4, d988a6c, baca448 (an unreadable payment term now fails validation instead of becoming due at once).
- [x] B13 sapconfig: all-zero account conversion consistent with integration. 02e80e4, a25ace9 (one sap.Alpha).

### Phase 5 — Test quality
- [x] R30 Polling helper; no time.Sleep in tests. pgtest.Eventually (and pgtest.SyncBuffer for
  logs a worker writes while the test reads); three worktree agents and a review follow-up, every
  part reviewed. time.Sleep in tests 34 → 0 (forbidigo 34 → 0); each touched package run with
  -race -count=5.
  - Load-sensitive tests: TestMigrationWorkflow gets a minute instead of Temporal's 3 s test
    timeout; TestLoadShedsAHotPrincipal freezes the limiter clock and checks the exact split;
    TestResilienceWorkerRestarts and both TestKeyRotationPolicy fixed by the fixes below;
    globalplane TestVerifyTenant compares the token it sent, not one minted a second later.
  - Fix (failing-first): the key rotator stopped at the first tenant whose rotation failed, so
    no tenant after it in id order (every new one) was ever rotated. It now skips and reports it.
  - Fix (failing-first, test harness): nfr Consistent finds a 201 whose body was cut off at
    shutdown by its idempotency key instead of reporting it as lost.
  - Workers are stopped by t.Cleanup when a wait fails; a uow test can no longer hang.
- [x] R31 t.Run subtests and one assertion per check. Four worktree agents by package group
  (about 90 commits, test files only); every part reviewed, three reviews asked for changes and
  approved the fixes.
  - Case tables run as subtests with got/want messages; || chains of 3+ conditions split; RUN
    counts rose everywhere (e.g. procurement 12 → 95, ledger 22 → 110, identity 10 → 87).
  - The five private brokenCommit helpers replaced by pgtest.FailingTx{FailCommit: true}, with
    exact errors.Is(err, pgtest.ErrInjected) assertions (offboarding's per-transaction variant
    is built on it).
  - Review findings fixed: comma-ok assertions that let a missing field pass, and Errorf ahead of
    an index or assertion that would panic. New apitest.Object/List/String read a JSON field and
    fail the test when it is missing; approvals' always-true Contains(x, "") now checks
    newest-first order.
  - Deferred: a few closures in subtests still hold the parent t (approvals as, uow a.do,
    tenancy and integration clients); some dependent steps continue after an Errorf (ratelimit,
    router); subtest names containing "/".
- [x] R32 thelper and noctx to zero (thelper 62 → 0, noctx 36 → 0). testing.TB parameters named
  tb, t.Helper first; requests, listeners and commands bound to the test's or caller's context;
  production: erp-router listens with the run context, PresignPut builds its request with ctx.
  Reviewer: APPROVE.
- [x] R33 Nightly shuffled run: a shuffle job in .github/workflows/nightly.yml runs
  go test -race -shuffle=on -count=3 -timeout 45m ./... with CI's database and Temporal setup.
  A shuffled rerun found five tests that relied on a fresh database or another test's leftovers
  (erp-server import, db revert, two controlplane tests, directory); fixed with
  pgtest.EmptyDatabase and per-run scoping. Reviewer: APPROVE.
  Phase 5 gates: make lint-all 0 issues; coverage 16950 of 16950 statements; gofmt and vet
  clean; go test -race -shuffle=on -count=3 ./... passes in all 64 packages.

### Phase 6 — Make it permanent
- [x] R40 CI lints the whole tree: the ci.yml lint step runs `make lint-all` (api/gen left out
  as the Makefile does) instead of `make lint` over the pushed range; CLAUDE.md and the Makefile
  comment say lint is the fast local loop and lint-all what CI holds the tree to.
- [x] R41 Import-rule test; §6.7 describes the real layout. internal/layout's test parses every
  production file under internal/ (skipping testdata, vendor, _ and . directories, as go does)
  and fails when production code imports test support, a platform package imports a context
  (except the composition root platform/httpapi and the test support platform/fixture), or a
  context imports another outside an exact allowlist of today's 71 dependencies (the R41 commit
  message says 74; 71 is right). money is the shared kernel; an unused allowlist entry fails too.
  Checked by mutation. §6.7 rewritten for the flat layout. Reviewer: changes asked (an incomplete
  cross-schema list, a stale Makefile comment), fixed.
  - Gap, not covered by the test: SQL that reads or writes another context's schema. Closed in
    R53: the tenant row goes through tenancy (control plane, key rotator, offboarding,
    provisioning, directory, rewrap), with the key from fieldcrypt, the latest verification
    from audit and the chain head from platform/audit; payments, configuration and workflows
    read who they act as through identity.Actors; workflows finds its work through the owners'
    listings (payments.Queued and Releases, configuration.QueuedSimulations,
    integration.QueuedRuns, approvals.Unwatched and Watched); provisioning creates the first
    administrator through identity. New imports: workflows -> operations; controlplane ->
    audit, tenancy; directory -> tenancy; offboarding -> tenancy. What remains are the named
    exceptions of 06 §6.7: the platform schema (operations, idempotency keys), ref, offboarding's
    catalog-driven export and import, cmd/erp-server's backlog gauges, and nfr.
- [x] R42 Final verification and scored review.
  - Gates at the end: `make lint-all` 0 issues; `make test` (vet, -race) passes in 65 packages;
    `make coverage` 16950 of 16950 statements; `make fuzz FUZZTIME=10s` 7 of 7 targets clean;
    gofmt clean; `go test -race -shuffle=on -count=3 ./...` passes (end of Phase 5).
  - Lint before → after (whole tree, api/gen excluded): 1238 → 0. wrapcheck 496 → 0,
    errcheck 491 → 0, thelper 61 → 0, gocognit 50 → 0, forbidigo 46 → 0, noctx 38 → 0,
    funlen 16 → 0, staticcheck 11 → 0, errorlint 11 → 0, bodyclose 6 → 0, dupl 4 → 0,
    exhaustive 3 → 0, gocritic 3 → 0, nilerr 2 → 0. Coverage gate: rounded 100.0% (hiding up to
    12 statements) → exact, 16950 of 16950. time.Sleep in tests 34 → 0.
  - Fresh whole-codebase review (go-reviewer, static reading): **8.2 / 10**, short of the 9.5
    target. Architecture 8.5, error handling 8.5, concurrency and tenancy 7.0, SQL safety 9.5,
    tests 8.5, readability 8.5, duplication 6.5. The gaps are correctness in payment release and
    the ledger, and duplication, not style. Ranked gaps (not fixed: behaviour changes and a
    migration need the user's go-ahead under the brief):
    1. A reversed supplier invoice can be paid: Reservations.RunOf ignores runs in releasing,
       and the ledger does not refuse to clear lines of a reversed entry.
    2. Payment-release activities are not safe against overlap or a lost completion: Releaser
       reads the run without FOR UPDATE, FinishRelease updates without a status guard, bank_file
       is not unique per run (needs a migration), operations.finish has no status guard; a
       retried FinishRelease on a released run fails instead of succeeding.
    3. Duplicated helpers that belong in internal/platform: mergePatch ×4, today ×4, the event
       amount helper ×3, API adapter helpers (ptr, optUUID, optString, ...), the worker Run loop
       ×12, the system unit of work ×6.
    4. Money typing: domain structs use int64 where money.Minor is meant (ADR-0005); conversion
       rounding lives in the ledger; sums are not overflow-checked.
    5. Ledger reversal and clearing can race (no lock shared between them).
    6. Security settings that fail open: a malformed operator roles claim grants every role;
       production starts without an object store or with audit verification off (warnings only).
    7. Test structure: scenario tests over 150 lines without t.Run steps; 211
       `err == nil || !strings.Contains` chains; the release-worker test depends on other tests'
       runs.
    8. Architecture housekeeping: the composition root lives in platform/httpapi; 26 package docs
       cite no ADR; the layout test does not cover cmd/; cross-schema SQL (R41 gap).

### Phase 7 — To 9.5 (user: "fix until 9.5 is achieved", after R42 scored 8.2)
Fixes are Fix commits with a failing-first test; a new migration is allowed, existing ones are not
edited. Re-review after each group until the whole-codebase score reaches 9.5.
- [x] F1 A reversed supplier invoice cannot be paid (review gap 1). RunOf counts a releasing run's
  unposted payments; failing-first flow test TestReverseDuringResumedRelease.
- [x] F2 Payment release safe against overlap and a lost completion; bank file unique per run (gap 2).
  Release activities serialise on the run (FOR UPDATE), finished steps are no-op successes, a
  superseded operation's steps do nothing, operations.finish and Report are guarded, a cancelled
  release stays cancelled and is resumed with :release (posted payments stand). New migration
  000033: one bank file per run, built concurrently; the migration runner polls for its lock so a
  concurrent index build cannot deadlock. Design docs 03/04/07 updated. Open: a cancelled
  release's FinishRelease can leave an orphaned bank file in object storage.
- [x] F3 Ledger refuses to clear a reversed entry; reversal and clearing serialise (gaps 1, 5).
  Advisory locks per entry (journal_entry is insert-only, so no row lock without a migration);
  409 ledger.entry_reversed. A reversal now clears the reversed pair (the reversal entry is the
  clearing document), so the pair leaves open items. The contract's descriptions say so (user
  approved; description-only). No backfill of pairs reversed before (user: no real data yet).
- [x] F4 Security settings fail closed (gap 6). A malformed operator roles claim holds no role;
  production refuses to start without an object store or with ERP_AUDIT_VERIFY_INTERVAL=0.
  ADR-0016, ADR-0025 and ADR-0027 amended (user approved).
- [x] R50 Duplicated helpers moved to internal/platform (gap 3). platform/worker.Loop replaces 12
  worker Run loops; uow.Act the system unit of work (6 copies and apitest.Committed);
  platform/mergepatch, database.Today, money.EventAmount and platform/apitypes (Ptr, OptUUID,
  OptString, OptNilString, Decode/EncodeObject, Money, Page, ETagOf) replace the copies in the
  contexts. Kept apart on purpose: integration fromSettings (nil vs empty map reaches storage),
  erp-server maintain() (waits before its first round). Reviewers: APPROVE.
- [x] R51 Money typed as money.Minor; conversion and checked sums in money (gap 4). Amounts in
  ledger, payments, payables, approvals, procurement, analytics, integration and agents are
  money.Minor (counts, number ranges, percentages and string quantities stay as they are).
  money gained ErrOverflow, Convert/ConvertAll (exact, half away from zero; the ledger's
  rounding-difference balancing), Scale, Sum (exact 128-bit), Diff, checked Extend/Percent, and
  FuzzConvert in make fuzz. Overflow was reachable from the API (18-digit amounts), so 11 of 21
  commits are Fixes with failing-first tests; the security-relevant one: two items at the maximum
  price summed negative and chose the lowest procurement approval tier, and consumed + order
  wrapped past a contract's limit. Reviewer: changes asked (4), APPROVE after fixes. Follow-up
  in progress: payments proposal sums, analytics aggregation, payables near(), reconciliation.
- [x] R52 Test structure (gap 7). pgtest.WantErr/CheckErr replace 187 hand-written
  `err == nil || !strings.Contains` checks (same substring, Fatal stays Fatal); 22 of 23 test
  functions over 150 lines split into ordered t.Run steps (TestDispatcher left: it drives the
  worker loop); the release-worker test counts its own tenant exactly; subtests report to their
  own t. Also: polling waits no longer open a connection per check (pgtest.Scan), which had made
  the coverage gate hit "too many clients". Reviewer: APPROVE.
- [x] R53 Architecture housekeeping (gap 8). The composition root moved from platform/httpapi to
  internal/app (apitest to internal/app/apitest), so no platform package imports a context but
  test-support fixture; the layout test covers cmd/; every package doc cites an ADR or design
  section (no ADR covers agents or LLM use: a gap in the ADRs themselves). Cross-schema SQL moved
  behind the owners' Go APIs (identity.Actors, tenancy's tenant row, owners' queued-work queries
  for the workflow dispatcher, identity.CreateFirstAdministrator, fieldcrypt and audit reads);
  the named exceptions that remain are listed in §6.7. Reviewers: APPROVE.
- [x] R54 Deferred items from earlier phases. blobstore Dir repairs a sidecar a failed Put left
  out (anchor retention no longer lost); every erp command fails when its output cannot be
  written; ledger's trial-balance budget scales with ERP_BUDGET_SCALE; .gitignore covers cmd
  binaries; discarded errors carry reasons. Overflow follow-ups of R51: payment proposal sums,
  analytics aggregates, payables near(), reconciliation sums (all reachable, Fixes). Open: a
  blocked supplier whose items overflow fails every proposal of its legal entity (a new
  exclusion reason needs a migration); a cancelled release's FinishRelease can orphan a bank
  file in object storage; analytics keeps every term per group in memory.
- [ ] R55 Re-review until 9.5. First re-review after Phase 7: **9.0 / 10** (architecture 9.2,
  error handling 9.1, concurrency and tenancy 8.4, SQL 9.7, tests 9.3, readability 9.2,
  duplication 8.8, money 9.3). Round two below.
- [x] F5 The ledger API cannot reverse or clear another context's document or a reserved item.
  :reverse refuses payables entries (409 ledger.entry_owned_elsewhere, naming
  /v1/supplier-invoices/{id}:reverse); integration entries stay reversible. Items a payment run
  holds are reserved in ledger-owned state (migration 000034, ledger.item_reservation), set by
  payments through the Postings port it already used (no new dependency, no ADR); manual and
  other clearings refuse a reserved item (409 ledger.item_reserved); Payable leaves reserved
  items out, so a stale reservation holds back one item; entry locks are 64 buckets per tenant
  (no tenant holds another's clearings, a transaction stays within max_locks_per_transaction),
  and a reversal takes no second lock. :reverse is an allowlist (ledger, integration, payments).
  Follow-up F5b (tracker, status T): the contract-step migration reconciling reservations with
  live runs (the next free number in the release after this one; F7 took 000037-000038,
  require_tenant 000039 and F13 000040), to ship once
  000034 runs everywhere (03-data-model §3.10). Second round (f5f6b): entry locks fail without a
  tenant (platform.require_tenant, migration 000039); a migration load takes every entry bucket
  before it posts (Postings.LockAllEntries; it deadlocked against a reservation); refusals of
  contexts without a reversal say so; tests pin tenant-scoped buckets and version 7 entry ids.
- [x] F6 A releasing run cannot get stuck; a proposal cannot race an invoice reversal. RunOf takes
  the proposals lock; a release drops a payment that can never post (supplier excluded with
  item_reversed or item_not_open, migrations 000035/000036; new additive PaymentExcluded event
  and the two codes in reason_code's enum, user approved); a retried release keeps one bank file
  under the run's key and accepts it when it pays the same, so a data correction cannot strand
  the run. Reviews: four rounds of blocking findings fixed (deadlock risk, cross-tenant lock key, lock
  table, contract migration on the same branch); an independent review then found a lock-order cycle with
  the migration load (fixed: the load takes every bucket up front, before any number range);
  entry locks fail without a tenant (migration 000039); final review: APPROVE.
- [x] F7 Security defaults fail closed (user: ERP_ENV required; production refuses an operator
  token without roles, ADR-0027 amended; principals bound to issuer + subject, with a migration).
  Migrations 000037 (column, backfill to erp-dev) and 000038 (unique index, concurrently); the
  contract step waits for a later release, once no replica of the previous release runs: it adds
  the issuer check NOT VALID, re-runs the backfill for principals the previous release inserted
  with a subject and no issuer after 000037 ran (creator's issuer, else the single tenant issuer),
  VALIDATEs in the next migration and drops principal_subject (03-data-model, F10).
  Independent review: APPROVE. Follow-ups: the principal rate-limit bucket is keyed by issuer too;
  a provisioner wired with no issuer fails as a wiring error (500), several give 422; contract
  descriptions name the issuer binding; tests prove the server and router pass ERP_ENV through.
- [x] R56 Duplication and money residue: money.ParseRat, platform/currency.Exponent (six lookups;
  a currency read failure is no longer a 422), new(v) for ptr/ptrTo, app builds each port once,
  uow.Job; pain.001 control sum checked; Minor.String prints raw minor units; money.Diff.
- [x] R57 Error-handling and readability residue: CreateRate refuses a non-decimal rate (was a
  nil dereference); the loader's discards say why; CLAUDE.md accepts pgx's rows, _ := tx.Query;
  erp-server's workers stop on one WaitGroup; TestDispatcher and the key-rotation test in steps.
- Second re-review after round two: **9.2 / 10** (architecture 9.3, error handling 9.2,
  concurrency and tenancy 8.9, SQL 9.7, tests 9.4, readability 9.3, duplication 9.0, money 9.1).
  Round three below.
- [x] F8 A posting cannot land in a period being closed: postingPeriod reads the period status
  FOR SHARE, so a posting waits for a close and is then refused (409 ledger.period_not_open);
  lock order buckets → period → number range. A close can be starved by steady postings in its
  period (accepted, documented: closes come after posting stops). Follow-up F8b: the trigger's
  own read FOR SHARE, in the next free number in the release after this one, after F5b's. Reviewer: APPROVE.
- [x] F9 Analytics rounds money with the currency's exponent; no hard-coded exponent tables.
  Analytics carries money as exact values (money.Minor, scale, currency) to the page; tiles and
  charts refuse to add two currencies. config.Validate takes exponents from ref.currency
  (currency.Load in a transaction; currency.Seeded from the embedded migration seed for the
  offline CLI, held equal to the database by a test). Requisition thresholds are read with each
  company currency's digits and compared with a requisition exactly (a dropped parse error sent
  1000.12 USD to the next tier). The float lint exclusion narrowed to render.go and narrative.go.
  Reviewer: changes asked (one), APPROVE after the fix.
- [x] F10 Issuer rollout: the contract step re-backfills; CreatePrincipal refuses an actor without
  an issuer (422 identity.creator_without_issuer, named in the contract's description).
  Reviewer: APPROVE.
- [x] F11 A failed workflow worker stops the server (or fails readiness) instead of serving without it:
  spawnCritical; the other workers keep logging and retrying a failed round, on purpose.
  Reviewer: APPROVE.
- [x] R58 Round-three residue: one open-item check (checkItem), one reversed-line condition,
  sorted partners, checked sums (ledger totals, payables simulation), Minor.Decimal removed, CLI
  read errors reported, combined assertions split, reverseJournalEntry in named steps.
- Third re-review after round three: **9.3 / 10** (architecture 9.3, error handling 9.3,
  concurrency 9.3, SQL 9.7, tests 9.4, readability 9.4, duplication 9.1, money 9.2). Round four
  below. User decision: configured money amounts (requisition thresholds, invoice tolerances,
  the auto-post limit) mean the company's currency; a document in another currency is converted
  at the ledger's rate for its date before comparing, and a missing rate refuses the step.
- [x] F12 Configured amounts compared in the company's currency (thresholds, tolerances, auto-post
  limit, simulation evidence), exactly, with parse errors handled: ledger.Rates converts at the
  posting rate (average, valid on the document's date), approvals takes it through a Rates port,
  payables reads a company's limits once with ParseDecimalRat; a missing rate is 422
  ledger.no_exchange_rate for a submission or a match, and an invoice a simulation or suite
  cannot convert is reported (broken / not_evaluable) instead of failing the tenant's
  governance. Regression suites captured before F12 decided foreign-currency invoices
  unconverted: re-capture them after deploying.
- [x] F12b The auto-post decision is taken on the server: the match answers `decision`
  (auto_post | review | not_evaluable, with a reason; additive contract change the user
  approved) from payables.decide, the one rule simulations and regression suites apply, and
  `erp invoices` acts on it instead of comparing the raw gross with the limit; a server without
  the field leaves the invoice parked, decision unavailable. Without a rate the match is still
  refused (422 ledger.no_exchange_rate, user decision), which the CLI reports, leaving the
  invoice parked; a gross out of range in the company's currency is matched and not_evaluable.
  Regression cases list not_evaluable in the contract.
- Fourth re-review after round four: **9.3 / 10** (architecture 9.3, error handling 9.3,
  concurrency 8.9, SQL 9.7, tests 9.4, readability 9.4, duplication 8.9, money 9.4). Round five
  below.
- Round five paused (2026-10-15): the account's weekly usage limit stopped all three agents
  mid-step. Nothing from round five is merged into this branch; the work sits unreviewed on local
  branches:
  - f13: done and merged (see F13 below).
  - f14 (d43b2f0, c8bcabe, f4f6fe4, 0b3496e after the rebase): F14 and R61 items 1-4; go-reviewer
    APPROVE, no blocking findings.
  - r60: finished and rebased onto this branch (R60 below).
  Resume: review each branch, finish the open items, merge, run the gates and the scored review.
- [x] F13 A clean match only authorises posting the invoice version it evaluated; tolerances and
  the auto-post limit compared exactly, before rounding. match_result.invoice_version (migration
  000040, expand step: nullable, check NOT VALID) records the version the match leaves the
  invoice at; :post needs the latest match clean and of the current version (409
  payables.match_required; a match without a version counts for none); `erp invoices` posts or
  holds with the ETag it sent to :match. The absolute tolerance and the auto-post limit are
  compared with amount × rate exactly (money.Exact, ledger.Conversion.Exact), shown rounded; an
  invoice without a rate is decided without being matched. F5b and F8b take the next free number
  in the release after this one. Reviewer: APPROVE. Follow-ups: a clean match is trusted
  against the order, receipts and configuration as they were at match time; the review reason
  can show 50000.00 above a 50000.00 limit when rounding hides the difference; the 000040 check
  stays NOT VALID (validated in F5b's migration); match.go reads hold_reason without a nil check.
- [x] F14 A configuration's company currency must equal the legal entity's local currency
- [x] R60 Round-five duplication: extension keys (platform/extensions), control/global plane HTTP
  helpers (httpapi.Operator), reference errors (problem.Reference), positive quantities
  (money.PositiveQuantity, the plain grammar Scale reads; payables accepted a captured "1/3" or
  "1e3"), migrated clearing record (ledger recordClearing); analytics plainDecimal documented as
  deliberately stricter than money's grammar. Review fixes: a padded quantity passed the
  quality gate then failed the load; an operator request without claims ran with every role.
- [x] R61 Round-five residue: agent budget read with ParseAmount, scanPrincipal checks the scan
  error first, an unknown legal entity in ToCompany is a reference problem, injected clocks for
  analytics pages and key rotation, a reverse-rate test; match.go uses cmp.Or and documents the
  hold_reason invariant. Reviewer: APPROVE.
- Fifth re-review after round five: **9.4 / 10** (architecture 9.5, error handling 9.4,
  concurrency 9.4, SQL 9.6, tests 9.5, readability 9.3, duplication 8.9, money 9.3). Round six
  below.
- [x] F15 Analytics and SAP reconciliation extend document quantities at their 6 decimals; rates
  read in money's plain grammar. Both extend with money.Scale from the decimal string (a 0.0005
  line failed a tenant's whole analytics build); money.ParseQty and Extend retired, Qty moved
  into the SAP mock; ledger.CreateRate validates with money.ParsePlain (422 ledger.invalid_rate;
  1e9 and .5 were accepted). Reviewer: APPROVE. Open: CreateRate does not check digit counts
  against numeric(28,12) (only the API pattern does).
- [x] F16 An approved invoice exception only authorises posting the invoice version it was approved
  at: supplier_invoice.exception_version (migration 000041, expand step, check NOT VALID) is set
  in the statement that releases the invoice; :post needs it equal to the version or a clean
  match of the current version, NULL failing closed (409 payables.match_required). Replaying an
  invoice refuses a missing order or legal entity with 422 instead of an empty value (it
  panicked); `erp invoices` checks the read, leaves a gone or changed invoice parked, and settles
  every page (apitools.List). Reviewer: changes asked (409 → 422), APPROVE.
- [x] R62 Round-six duplication and clocks: tenancy.Accessible replaces six copies of the
  accessible-legal-entity refusal; problem.ReferenceNotFound/Reference replace the procurement and
  payments copies (one client-visible change: an unknown requisition_id on an order now reports
  field code not_found, not invalid; top-level code unchanged); Simulate reads through
  invoicesOnOrders; one perPartner netting check; KeyRotator and ContractExpirer take a clock;
  constant identifiers quoted through pgx.Identifier. Reviewer: APPROVE.
- [x] R59 Round-four residue: createClearing through clearWithEntry; currency.LoadCodes beside
  Load (one loader); money.Rat; MonthlyBudget as *money.Minor; Render validates the spec (was a
  panic); a period close locks only a period the caller may see; provision and rewrap report a
  keyring they cannot build (rewrap panicked on no master); analytics compares numeric values
  exactly (17-digit codes compared equal). Reviewer: APPROVE.

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
- Phase 4 (offboarding, startup, found while splitting; not fixed):
  - SECURITY: offboarding import puts a manifest-supplied table name into SQL unquoted, in
    checkEmpty (`SELECT count(*) FROM `+name) and in insert (INSERT INTO %s ... NULL::%s); the
    manifest is not hash-checked and only the .outbox suffix is filtered, so a crafted name runs
    SQL as the app role inside the import transaction. Keep only names tables(ctx, tx) returns
    and render them with pgx.Identifier{schema, table}.Sanitize().
  - erp-server: when the bus connection or EnsureStreams fails, run returns and the pool closes
    while the already-started workers may still use it.
  - nfr harness: tb.Fatal on a failed NATS start happens before t.Cleanup(s.Stop), leaking the
    test server and goroutines.
  - config: the directory step reports its errors in map order.
  - ledger createClearing names whichever partner map iteration reaches first when several fail
    to net to zero.
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
