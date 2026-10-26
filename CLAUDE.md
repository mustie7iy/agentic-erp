# agentic-erp

Multi-tenant ERP in Go: a modular monolith on PostgreSQL 17 (row-level security per tenant),
a contract-first HTTP API generated with ogen, a transactional outbox to NATS, and Temporal for
long-running workflows. The design lives in `docs/design/` (01–07) and the decisions in
`docs/design/adr/`; read the relevant ADR before changing anything it covers. Work items are
tracked in `docs/tracker.json` (T-numbers).

## Commands

```bash
make db-up                    # PostgreSQL 17 in Docker on :55432 (needed by most tests)
export DATABASE_URL='postgres://postgres:dev@localhost:55432/postgres?sslmode=disable'
go build ./...
make lint                     # structural lint of code changed since main (.golangci.yml)
go test -short ./internal/<pkg>/...   # fast loop for one package
make test                     # vet + whole suite with -race
make coverage                 # 100% statement coverage gate (CI enforces it)
make ci-local                 # every gate CI has, locally, before a PR is merged (06 §6.5)
make generate                 # regenerate api/gen after editing api/openapi.yaml
```

Never edit `api/gen/` by hand: it is ~610k lines generated from `api/openapi.yaml`. Change the
spec, run `make generate`, and commit both.

## How quality is kept: four layers

Rules are only as good as what enforces them. This repository uses four layers, from advice to
hard gate. Follow all of them; the later ones will stop you if you skip the earlier ones.

### 1. These conventions (read before writing code)

**Data and transactions**
- Request work runs inside the unit of work (`internal/platform/uow`). Get the transaction and
  principal with `tx, p, err := reqctx.Request(ctx)` and **handle `err`**; never `_` it.
- Tenant isolation is PostgreSQL RLS (ADR-0003). Never bypass it, never add a query that takes a
  tenant id from user input instead of the scoped transaction.
- Money is `money.Minor` (integer minor units, ADR-0005). No `float32/float64` for amounts or
  quantities, and no raw `int64` where a `money.Minor` is meant.
- State-changing rows are read `FOR UPDATE` (or updated with a guard on the expected
  `version`/state) — ADR-0008. Every external effect goes through the outbox in the same
  transaction (ADR-0007), and every state change writes an audit record (ADR-0011).
- SQL is parameterized (`$1`…). Identifiers go through `pgx.Identifier{…}.Sanitize()`. Never build
  SQL with `fmt.Sprintf` from values.

**Workflows (Temporal, ADR-0012)**
- Every activity must be idempotent: running it twice, including after a commit whose completion
  was lost, leaves the same state and publishes no duplicate event. Guard updates with the
  expected prior state (`WHERE escalations = $n`, `WHERE status = 'pending'`, an idempotency key).
- `workflow.GetVersion` results must be branched on; a discarded result is a bug.
- Workflow code is deterministic: no `time.Now`, no randomness, no I/O outside activities.

**Errors**
- Wrap errors that cross a package boundary with context:
  `fmt.Errorf("payments: release run %s: %w", id, err)`. API-facing errors are
  `problem.*` values; register database constraint → problem mappings as the existing packages do.
- No `panic` outside `main` and init-time wiring. No silent fallbacks: a missing security
  setting (keys, issuers) fails startup in production rather than logging a warning.
- Never discard an error without a comment saying why it is safe. The one accepted exception is
  pgx's `rows, _ := tx.Query(…)`: a failed Query returns rows whose error `pgx.CollectRows`,
  `pgx.ForEachRow` or `rows.Err()` reports, so the error is handled there.

**Structure**
- Functions ≤ 60 lines, cognitive complexity ≤ 20. Split long orchestration into named steps.
- Before writing a helper, search for an existing one (`internal/platform/*`, `money`, `etag`).
  Never copy a helper into another package; move it to `internal/platform` and reuse it.
- A context may not import another context's internals. A new cross-context import needs an
  interface in the callee's published API and, if it changes a dependency direction, an ADR.
- Comments explain *why*, not what. Each package has a doc comment citing its ADRs.

**Tests**
- Real PostgreSQL via `internal/platform/pgtest`; never mock the database.
- Cover every database error path with `pgtest.FailingTx` (see `TestDatabaseFailures` tests).
- Table cases run in `t.Run(name, …)`, one clear assertion message per check
  (`got %v, want %v`), not long `||` chains.
- No `time.Sleep` in tests: inject a clock (`Now func() time.Time`) or poll for the condition.
- Workflow activities get a run-twice test proving idempotency.
- Coverage stays at 100% of production statements; untestable code is a design smell, not an
  exclusion.

**Schema and API** (docs/design/06-engineering-process.md §6.3–6.4)
- Migrations are immutable numbered up/down pairs; expand → migrate → contract; `tenant_id`
  leads every key; RLS on every table; a schema test for every database-enforced rule.
- API changes start in `api/openapi.yaml` / `api/asyncapi.yaml`; no breaking changes without an
  entry in `api/breaking-accepted.txt` and a reason.

### 2. Machine checks (automatic)

- `.golangci.yml` enforces function length, complexity, duplication, error wrapping, banned calls
  (`time.Sleep` in tests, `panic`, floats for money) and test-helper hygiene. `make lint` checks
  code changed since `main` (the fast loop); `make lint-all` checks the whole tree except api/gen,
  which is at zero issues, and CI runs it on every push and pull request.
- Claude Code hooks (`.claude/settings.json`) run after every Go edit (`gofmt` and lint of the
  edited package) and before you finish (build, lint, short tests of changed packages). If a hook
  reports a problem, fix the cause; do not suppress it.
- A `//nolint:<linter> // <reason>` is allowed only with a specific reason, and the reviewer
  will challenge it.
- `make ci-local` runs every gate locally at CI's pinned versions: vet, staticcheck, gosec,
  govulncheck, gitleaks, contract lint, the breaking-change check, `-race` three times
  shuffled, the coverage gate, fuzzing and performance budgets. GitHub then gives a pull
  request a short independent check (static checks, the suite without `-race`) and runs
  the full suite on every push to `main` (06 §6.5). Merge with `scripts/merge-pr.sh <n>`,
  which requires both.

### 3. Review before reporting done

After any non-trivial change (new feature, workflow, migration, anything touching money,
tenancy, auth or concurrency), run the **`go-reviewer`** agent on the diff and resolve every
blocking finding before telling the user the work is done. Report what it found and what you
changed. For large branches the user may also run `/code-review high` or `/code-review ultra`.

### 4. Small, specific tasks

- Work one tracker item (T-number) at a time per lane, with its acceptance criteria stated up
  front. Lanes run in parallel on code that does not overlap (07-transition §7.7).
- Every step is held to the bar the refactoring programme reached (9.5/10) and built with the
  loop in `docs/design/07-transition.md` §7.7; `docs/tracker.json` is the only plan (§7.6).
- If a request is broad ("build approvals"), propose a split into tracker-sized steps first.
- Each step ends green: lint, tests, coverage, reviewer. Commit per step with a message naming
  the T-number, as the existing history does.
- Update `docs/tracker.json` and any affected design doc or ADR in the same change.

## Definition of done

`make lint` clean · `go build ./...` · `make ci-local` passes (tests with `-race`, coverage at 100%) ·
GitHub's `ci-ok` green ·
`go-reviewer` has no blocking findings · contract, data model, ADRs and tracker match what was
built (06-engineering-process §6.8).
