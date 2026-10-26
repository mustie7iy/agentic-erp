# Review rules

How a step's diff is reviewed here, by the `go-reviewer` agent (`.claude/agents/go-reviewer.md`)
or by a person. `CLAUDE.md` is the standard the diff is held to; this file says what a finding
is, how it is labelled and how it is reported. Adopted by the product owner on 2026-10-26
(06-engineering-process §6.9).

## What is reviewed

- The step's diff (`git diff main...HEAD` and uncommitted changes), each changed file read in
  full, and the callers of changed functions.
- Ignored: `api/gen/` and other generated files (`*_gen.go`, `*.pb.go`, anything headed
  `Code generated … DO NOT EDIT`), except to confirm they were regenerated when their source
  (`api/*.yaml`) changed.
- Rounds after the first review only the changes since the previous round (`git diff <last
  reviewed commit>..HEAD`), plus anything those changes touch.

## Labels

Every finding carries one label.

- **Blocking**: the step is not done until it is fixed. A finding is blocking when it is about
  correctness, money (ADR-0005), tenancy and RLS (ADR-0003), authorization (ADR-0010),
  concurrency (ADR-0008), idempotency of requests or workflow activities (ADR-0008, ADR-0012),
  a contract break (an unaccepted breaking change, a spec and its generated code out of step),
  or a `CLAUDE.md` convention (errors, structure, tests, coverage, schema, outbox and audit).
- **Nit**: naming, wording, comment quality, a small simplification. Never blocking; at most
  three per round, the ones most worth the author's time.
- **Pre-existing**: a bug the diff did not introduce, found while reading around it. It is
  never held against the step and the author does not fix it in the step. It is reported, and
  the coordinator turns it into a tracker item with `"src": "review:<step>"` (07-transition
  §7.5).

## Bug class

A finding names the class of the bug as well as the instance: "an activity that inserts
without conflict handling", not only "line 42 inserts twice". The reviewer looks for other
instances of the class in the diff and lists them; the author fixes every instance in the diff,
not only the one cited. Instances outside the diff are Pre-existing.

## Rounds

A round is one review of the step and the author's fixes for it. After three rounds on one
step that still end with a Blocking finding, the author stops fixing finding by finding,
reconsiders the approach, and tells the coordinator before a fourth round. Each report states
its round number.

## Report

- Each finding cites `path:line` (the line in the reviewed tree), says what is wrong with a
  concrete scenario, and the fix.
- The report opens with a summary: the verdict, the round, and the count of findings by label
  (`Blocking 0 · Nit 2 · Pre-existing 1`), or an explicit `No issues`.
- Then the findings grouped by label, Blocking first, and the machine checks the reviewer ran.
