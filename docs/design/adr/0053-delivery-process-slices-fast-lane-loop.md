# ADR-0053: Delivery process: slices, a fast lane loop, full gates once per batch, agent acceptance

**Status:** accepted (product owner, 2026-10-28).

## Context

The product owner found on 2026-10-28 that the delivery process had become
too slow, "a waterfall of waterfall", and decided to change it while keeping
the quality bar: every step is still held to 9.5/10 (07 §7.7). The same
decision sets the priority: a headless ERP, the API and the back end, proven
to work end to end. The user interface is deferred.

What batch 17 measured:

- A lane step took 60–96 minutes, of which about 35–50 minutes were gates
  that the batch's `make ci-local` runs again before the pull request.
- The batch's `make ci-local` took about 70 minutes: the race suite 24, fuzzing
  11, the images 7, coverage 6 and the infrastructure checks 5.5.
- GitHub's pull request check took about 32 minutes.
- About eight questions in the one batch waited on the product owner, and
  every one was answered with the agent's recommendation.
- A demonstration found gaps in the API and the product that no gate had
  caught: the gates proved the code correct, not that a consumer could use it.

Until now a lane ran on its own step nearly every gate the batch runs again
(07 §7.7 item 8, whole-suite race run and whole-repository coverage included),
steps were sized by tracker leaf rather than by what a user can do, contract,
migration and ADR changes waited for the product owner's approval (07 §7.7
item 1), and nothing reviewed a response as its consumer would read it.

## Decision

### a. A fast loop in the lane, the full gates once per batch

A lane runs on its step:

- `gofmt`, `go build ./...`, `go vet`, the lint of changed code, `staticcheck`,
  `gosec` and `gitleaks`;
- `make generate` leaving no diff, and the contract lint and the
  breaking-change check when the contract changes;
- the tests of the changed packages and of the packages that import them
  directly, against real PostgreSQL, with `-race`, once;
- 100% statement coverage of the changed packages;
- the `go-reviewer` agent, under `REVIEW.md`.

The whole-suite race run three times in random order, the whole-repository
coverage gate, fuzzing, the performance budgets, the images, the signing round
trip and the infrastructure checks run once per batch, in `make ci-local`,
unchanged, before every pull request (06 §6.5).

**Rollback trigger.** The coordinator records the cause of each batch's
`make ci-local` failures. If more than one batch in three fails `make ci-local`
on something the old full lane gates would have caught, the lanes return to
the full gates.

### b. A step is a slice

A step that adds or changes a capability is a slice: one capability that a
named user, or an API consumer, can use end to end. It closes one or more
tracker leaves and is sized for one agent in about half a day. It includes the
contract, the domain code, the migration, the tests, the updates to the design
documents and ADRs, and at least one new or extended step of the pilot
walkthrough (`scripts/walkthrough.sh`) that exercises the capability through
the public API.

The walkthrough runs in every batch, as it does now (`cmd/erp`'s
`TestWalkthrough` in `make ci-local`), and is the slice's acceptance
evidence: the slice's step is among the steps the run's summary lists as
passed, not skipped, which the coordinator reads from a `make walkthrough`
run on the batch for the digest (the test prints the summary only when it
fails).

A step that adds or changes no capability (a fix, a refactoring, a CI,
process or documentation change) is held to everything else in this
decision and needs no walkthrough step.

### c. Acceptance by agents

A slice is accepted by the `go-reviewer` agent, which also checks the
API-consumer bug classes in `REVIEW.md`. The product owner is not a gate:
agents act on the product owner's behalf, and no story or demonstration waits
for their approval. After each batch the coordinator publishes a digest: what
changed, the walkthrough steps that passed, the decisions taken and the
process metrics (f). Reading it is optional.

### d. Decide by default

When an ADR, a design document or existing behaviour settles a choice, the
agent decides and records the choice in an "As built" section of the ADR
concerned. The digest lists those decisions so that the product owner can
overrule any of them after the fact; an overrule becomes a tracker item.

When nothing settles the choice, the agent decides it too: a decision that
needs an ADR (06 §6.2) is written as a new ADR, accepted once `go-reviewer`
has no blocking finding on it, and listed in the digest in the same way.

Only four kinds of question block on the product owner: new scope, money or
tax rules, legal or regulatory facts, and the security posture. They are
batched into one question.

### e. Three lanes

Three lanes run at once; the coordinator's `lanegate.sh`, outside the
repository, gives them three slots. `make ci-local` still has the machine to
itself.

### f. Metrics per batch

The coordinator tracks, per batch:

- escaped defects: `main` red after a merge (target 0);
- `make ci-local` failures that a full lane gate would have caught (the
  rollback trigger, a);
- the reviewer's blocking findings per slice;
- the walkthrough steps added;
- the lead time from a lane's start to the merge.

### g. User interface deferred

Changes to the web workspace (ADR-0052) are not the priority. Its existing
tests keep passing.

## Alternatives

- **Keep the full gates in every lane.** Rejected: the lane's gates repeated
  most of what the batch's `make ci-local` runs, and they took the larger part
  of a step's time (Context).
- **Lower the bar to go faster** (fewer race runs, a coverage target below
  100%, mocks for the database). Rejected: the product owner kept the 9.5/10
  bar. The heavy gates move to the batch; none is dropped.
- **Keep the product owner as the approver of stories and demonstrations.**
  Rejected: every blocking question in batch 17 was answered with the
  recommendation, so the wait added time and no decision.

## Consequences

- Lane steps spend their time on the slice, and the full gates run once per
  batch. A fault that only the whole suite or the whole-repository gates find
  is found at the batch, not in the lane, and costs that batch a fix; the
  rollback trigger (a) bounds how often that may happen.
- A step is judged by what a consumer can do through the API, and the
  walkthrough grows with every slice.
- Decisions taken by default are visible after the fact, in the ADRs' "As
  built" sections and the digest, rather than waiting before the work.

What is unchanged:

- 100% statement coverage of production code, the whole-suite race run three
  times in random order, real PostgreSQL and never a database mock, and a
  run-twice test for every workflow activity;
- the reviewer on every step and on the batch's merge, under `REVIEW.md`;
- the red-`main` rule (06 §6.5);
- every tracker item names its source (07 §7.5);
- ADRs and design documents kept current with what was built;
- migrations immutable once merged.

Known deviations from common CI practice:

- The heavy gates run on the developer's machine (`make ci-local`), not on a
  CI service, with GitHub's light check and the full suite on `main` on demand
  as the second line (06 §6.5).
- GitHub branch protection is not available on a free private repository. The
  product owner approved it on 2026-10-28, and GitHub refused it: it needs
  GitHub Pro or a public repository. `scripts/merge-pr.sh` enforces `ci-ok` by
  convention instead.
