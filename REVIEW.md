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
  an API-consumer class below (ADR-0053), or a `CLAUDE.md` convention (errors, structure,
  tests, coverage, schema, outbox and audit).
- **Nit**: naming, wording, comment quality, a small simplification. Never blocking; at most
  three per round, the ones most worth the author's time.
- **Pre-existing**: a bug the diff did not introduce, found while reading around it. It is
  never held against the step and the author does not fix it in the step. It is reported, and
  the coordinator turns it into a tracker item with `"src": "review:<step>"`, or
  `"review:batch<N>"` for a batch (07-transition §7.5).

## Bug class

A finding names the class of the bug as well as the instance: "an activity that inserts
without conflict handling", not only "line 42 inserts twice". The reviewer looks for other
instances of the class in the diff and lists them; the author fixes every instance in the diff,
not only the one cited. Instances outside the diff are Pre-existing.

### Named bug classes

Classes the product owner has named after they reached a gate. A reviewer checks every diff for
them, beside whatever class a finding of its own names.

- **A tamper that can leave the value unchanged** (PO 2026-10-28). A test that alters a token,
  signature, ETag, HMAC, page token, state, nonce, hash or sealed value to prove it is refused
  must guarantee the altered value differs from the original; otherwise the test fails, or
  passes for the wrong reason, whenever the random original already holds what the tamper
  writes. Overwriting a character or byte with a fixed one (`"x"+state[1:]`) is the instance:
  a random base64url state starts with `x` one time in 64 (`internal/web/signin_test.go`,
  `TestCallbackRefusals`, fixed in batch 16 with `otherFirst`). Derive the tamper from the
  original so it cannot coincide (flip a bit with `^= 1`, pick a character the original does
  not hold there, append, truncate), or fail the test when the result equals the original. A
  constant that cannot have the original's form (`"!!"` for base64, a shorter value) is
  different by construction; replacing a whole digest or secret of 128 bits or more with a
  constant is accepted, since a collision is not a realistic event.
- **Records or events in database read order** (batch 52 review). A list, audit record, event payload
  or extract built from a query with no explicit `ORDER BY` (or from ranging over a map), or
  any result that depends on heap order, is **Blocking**: PostgreSQL returns rows in whatever
  order the plan and the table's pages give, so the same data comes out in another order after
  an update moves a row, a vacuum or another plan, and a consumer, a hash or a test comparing
  it sees a change that is not one. `readTerms` in T57b was the instance: after a PATCH moved
  one ledger's `asset_terms` row, the change's audit record listed the ledgers in another
  order, and only the race gate saw it (fixed in `c300e530`, `inLedgerOrder`); T57g and T57h's
  transfers (`transferredInLedgerOrder`) had the same shape. The fix is an explicit key, an
  `ORDER BY` ending in a unique column, or sorting the result through the owning port's order
  (the ledger port's: leading ledger first, then code), the order the API already shows.

### API-consumer classes

Named by the product owner on 2026-10-28 (ADR-0053): the product owner is not a gate on a
slice, so the reviewer reads every response and report the slice adds or changes as its
consumer would, an integration or an agent with only the contract in hand. Each class is
**Blocking** when the diff introduces it; an instance the diff did not introduce is
Pre-existing.

- **An identifier without the name or reference its consumer needs.** A response returns an id
  where the consumer also needs the name or reference of what it identifies, and would have to
  make one more call per row to get it: a payment's payee given only as a partner id.
- **An amount without its currency.** A money value in a response or a report that does not
  carry, or sit beside, the currency it is in.
- **A list without filters or paging where the collection can grow.** A list operation over
  rows that accumulate without cursor paging (04 §4.5) or without filters to find a
  consumer's rows.
- **An error without a problem code.** A refusal that is not an RFC 9457 problem with a stable
  `code` the consumer can branch on (04 §4.6).
- **A report without its totals.** A report whose consumer must add up its rows to get the
  totals.
- **An action whose result the consumer cannot find.** A response to an action that does not
  say what it created or started (the created object, or the `Operation` of a job, 04 §4.8)
  or how to reach it.

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
