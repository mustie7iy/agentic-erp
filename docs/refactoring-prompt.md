You are executing the code-quality refactoring programme for this repository (target: 9.5/10).
Work autonomously through the phases below, in order, one step at a time. Follow CLAUDE.md
in full — it is the standard every step is held to.

# Setup (do once)

1. Read CLAUDE.md, docs/design/06-engineering-process.md, and the ADRs named in each step.
2. Make sure the database is up (`make db-up`) and export
   DATABASE_URL='postgres://postgres:dev@localhost:55432/postgres?sslmode=disable'.
3. Create and switch to branch `refactor/quality-9.5` from main. Never push, never force,
   never rewrite history, never commit to main.
4. Progress lives in `docs/refactoring-progress.md`. If it exists, read it and resume from the
   first step not marked done. If not, create it with every step below as a checklist line
   (`- [ ] R1 …`) and a "Baseline" section.

# Rules for every step

- A refactoring step never changes behaviour. Phase 1 steps are bug fixes and DO change
  behaviour; each one comes with a test that fails before the fix and passes after.
- Never mix a bug fix into a refactoring commit. If you find a bug mid-refactor, note it in the
  progress file under "Found during refactoring", finish the step, then fix it as its own step.
- Never weaken a test, delete a test case, lower the coverage minimum, loosen .golangci.yml, or
  add `//nolint` to make a step pass. A `//nolint:<linter> // <reason>` is allowed only where
  the rule is genuinely wrong for that line, with a specific reason.
- Never edit api/gen by hand or existing migrations.
- Keep each commit reviewable (aim under ~400 changed lines unless purely mechanical); split a
  step into R<n>a, R<n>b… if needed.

# The loop for each step

1. Mark the step in progress in docs/refactoring-progress.md.
2. Record the "before" state: lint count for the scope
   (`go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0 run <pkgs>`),
   and that `go test -count=1 <pkgs>` passes.
3. Make the change.
4. Verify, all must hold:
   - `go build ./...`
   - `make lint` clean, and the targeted linter count for the scope went down as intended
   - `go test -race -count=1 <changed packages and their direct importers>`
   - `make coverage` still 100% (run at the end of each phase at minimum, and after any step
     that deletes or moves code)
5. Run the `go-reviewer` agent on the step's diff. Fix every blocking finding and re-run it
   until it returns APPROVE.
6. Commit with message `Refactor: <what> (R<n>)` or `Fix: <what> (R<n>)`, ending with the
   Co-Authored-By line from your instructions.
7. Update docs/refactoring-progress.md: tick the step, note lint before → after, and anything
   deferred. Commit that with the step.

# Stop and ask the user when

- a step needs a behaviour change that is not listed in Phase 1;
- a fix would change the API contract, a migration, or an accepted ADR;
- tests fail for a reason you cannot trace to your change (e.g. flaky perf budgets: rerun once
  in isolation as CI does; if still failing, stop);
- the reviewer and you disagree twice on the same finding;
- at the end of each phase: give a short report (steps done, lint counts before → after,
  anything found) and wait for "continue".

# Phase 0 — Baseline

- R0a Tune `.golangci.yml` errcheck noise only, via `errcheck.exclude-functions`: exclude
  fmt.Fprint/Fprintf/Fprintln, (net/http.ResponseWriter).Write, and pgx Rollback (it runs after
  Commit or on an already-failed transaction). Do NOT blanket-exclude Close: closing a writer
  can lose data, so those findings stay and get fixed. Justify each exclusion in a comment.
- R0b Record the baseline: whole-tree lint counts by linter and by package (all packages except
  api/gen), test status, coverage figure, into the "Baseline" section. Add the R-items to
  docs/tracker.json in the existing format.

# Phase 1 — Correctness bugs (behaviour changes, each with a failing-first test)

- R1 internal/approvals/escalate.go: the escalation UPDATE increments unconditionally, so a
  Temporal activity retried after a lost completion escalates twice and emits a duplicate
  approval_escalated event. Pass the expected escalation count (from the workflow) and guard
  `AND escalations = $n`; treat no-row as "already done" (idempotent success). Test: run the
  activity twice with the same input → one increment, one event, one audit record. ADR-0008,
  ADR-0012.
- R2 internal/workflows/approvals.go: the result of GetVersion is discarded. Branch on it so
  workflows started before this change replay correctly; add a replay or version test.
- R3 internal/platform/outbox/relay.go (`message`): `name[:strings.LastIndex(name, ".")]`
  panics on an event type without a dot. Handle the -1 case (publish under the full name or
  reject with an error and leave the row unpublished — choose consistent with ADR-0007, explain
  in the commit). Test with a malformed event type.
- R4 Every `_` discard of the error from `reqctx.Request(ctx)` (about 15 sites; find them with
  grep). Return the error. Add one test proving a call outside a request fails cleanly instead
  of using a nil transaction.
- R5 Fail closed in production: refuse to start when the Temporal payload codec key is missing,
  or when the dev token issuer is enabled, in a production environment (see how
  internal/platform/config distinguishes environments; if it does not, stop and ask). Tests for
  both refusals and for the allowed development case.
- R6 Remaining correctness linters across the tree: nilerr, errorlint, staticcheck, bodyclose,
  exhaustive — to zero. Each finding is either a real fix (with a test if behaviour changes) or a
  justified nolint.

# Phase 2 — Shared helpers (refactor only)

- R7 Move the `ifMatch` helper duplicated across ~11 packages into internal/platform/etag (e.g.
  `etag.IfMatch(header) func(int32) error`), with tests; replace every copy; delete the copies.
- R8 The injectable `Begin` / `Now` fields plus `begin()` / `now` fallback pattern repeated in ~18
  structs: introduce one small helper in internal/platform (name it to fit the codebase), with
  tests; migrate the structs package by package (R8a, R8b… if large).
- R9 Clear remaining `dupl` findings.

# Phase 3 — Error context (refactor only, the largest phase)

- R10… One step per package, in this order: internal/platform/* , internal/ledger,
  internal/payments, internal/procurement, internal/payables, internal/partner,
  internal/configuration, internal/integration, internal/analytics, then the rest, then cmd/*.
- Wrap errors from other modules where they leave a function with context:
  `fmt.Errorf("<pkg>: <operation> <key id>: %w", err)`. Do not double-wrap; do not wrap
  problem.* values; keep sentinel/constraint matching working via errors.Is/errors.As.
- Existing tests on problem codes and FailingTx paths must stay green — that proves the
  database-error → problem mapping survived. If a test asserts an exact error string, update
  the assertion to errors.Is / problem code rather than the string, and say so in the commit.
- Parallelism: packages are independent. You may run up to 4 subagents at once, each in its own
  git worktree (isolation: worktree), each owning exactly one package and following this loop.
  Merge their commits into refactor/quality-9.5 one at a time, re-running build and tests after
  each merge. Do not let two agents touch the same package or shared file.
- Target: wrapcheck = 0 in production code.

# Phase 4 — Long and complex functions (refactor only)

- R20 cmd/erp-server/main.go `run()` (~262 lines): split into named steps, e.g. load
  configuration, open stores, wire domains, wire workflows, serve. Startup order and behaviour
  must be identical; cmd/erp-server tests must pass unchanged.
- R21 analytics `Execute`, platform/config `Load`, integration quality checks, payables
  `validate`: extract named steps, each doing one thing.
- R22 Remaining funlen and gocognit findings to zero.
- Rule: extraction only. If a split reveals a bug, log it and fix it as a separate step.

# Phase 5 — Test quality

- R30 Add a polling helper (e.g. `eventually(t, timeout, cond)`) in an existing test-support
  package, and use injected clocks where code already supports `Now`. Remove every time.Sleep
  from tests. Run each touched package with `-count=5` to show it is not flaky.
- R31 Convert map/slice-driven case loops to `t.Run(name, …)` subtests with
  `got %v, want %v` messages; split long `||` assertion chains into separate checks with their
  own messages. One package per commit.
- R32 thelper and noctx findings to zero.
- R33 Add a nightly job step (.github/workflows/nightly.yml) running
  `go test -shuffle=on -count=3 ./...` against the database.

# Phase 6 — Make it permanent

- R40 For each linter now at zero across the tree, the rule already holds for new code; when all
  production findings are zero, switch the CI step from `make lint` to `make lint-all`
  (excluding api/gen as the Makefile does) so the whole tree stays clean.
- R41 Add the import-rule test that 06-engineering-process.md §6.7 describes: a Go test that
  parses imports and fails if a context imports another context's internals. Encode today's
  allowed cross-context imports explicitly (e.g. payments → partner) as an allowlist so new
  ones fail; update §6.7 to describe the actual flat package layout.
- R42 Final verification: whole-tree lint, `make test`, `make coverage`, `make fuzz
  FUZZTIME=10s`. Then run a fresh review of the whole codebase (not just the diff) with the
  go-reviewer agent and score it per dimension: architecture, error handling, concurrency and
  tenancy, SQL safety, tests, readability, duplication. Write the result, before/after
  numbers and remaining gaps into docs/refactoring-progress.md.

# Final report to the user

When everything is done (or you stop), report: steps completed, commits on the branch, lint
counts before → after by linter, coverage, the new score with reasoning, anything deferred or
found, and the exact command to review the branch (`git log --oneline main..refactor/quality-9.5`).
Do not merge or push; the user decides.
