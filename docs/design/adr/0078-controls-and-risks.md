# ADR-0078: Controls and risks

**Status:** accepted (T98a, 2026-11-12; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T98a's last commit). It decides
the governance, risk and compliance context and the slices T98a to T98c
(`docs/design/plan-breadth-week3.md`, Lane C), each of which records what
it built under "As built". The defaults taken and the questions left are
listed at the end for the product owner's digest.

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first). The
tracker's area GRC and security has the placeholders Control library, SOX
control mapping and Risk register. `plan-breadth-week3.md` details them
as the slices T98a (a control library with test results), T98b (the
control matrix) and T98c (the risk register) in a new context `grc`, and
asks T98a to write this ADR.

What the design already settles:

- A leaf context reads others through ports and nothing imports it
  (ADR-0051 §2); the plan names `grc` as one: "it reads identity
  (permissions, SoD rules) and the configuration through ports; it posts
  nothing".
- ADR-0003 (RLS), ADR-0007 (outbox), ADR-0008 (changes with the caller's
  version), ADR-0010 (permissions, agents), ADR-0011 (audit), ADR-0053
  (slices, headless first).
- 04 §4.3: there are no hard deletes and no `DELETE`; a state changes
  through a custom method, a document's fields through `PATCH`.
- The plan's default 11: the control library, the control matrix and the
  risk register ship empty; framework references are the tenant's free
  text.
- Week 3's rules (product owner, 2026-11-12): no segregation-of-duties
  rule is added by a slice; events go to the context's outbox and none is
  offered as a webhook event until a consumer asks.

Nothing settles what a control and a test hold, what "the last test" is,
or what is closed to agents beyond the plan's "recording a test is closed
to agents". This ADR decides them at the level the slices need.

## Decision

### 1. A leaf context `grc`

A new bounded context `grc` (schema `grc`, package `internal/grc`) with
its own outbox and inbox, registered in `outbox.Contexts`. It is a leaf
(`internal/layout`): no context imports it, and it imports no other
context. What it reads of another context it reads through a port it
declares in types of its own, which the composition root wires
(`app.GRC`, `internal/app/grc.go`): principals' names, kinds and statuses
from identity (`People`). It posts nothing and changes nothing outside
its schema. Its data is the tenant's, not a company's: controls per
company come later.

### 2. A control

A control (`grc.control`) is one row of the tenant's control library:

- `code`: the tenant's own code, 1 to 40 letters, digits, dots, dashes
  and underscores, starting with a letter or digit. It is unique in the
  tenant whatever its case (`control_code_once`, 409
  `grc.control_code_taken`) and never changes: tests, links and events
  name the control by it.
- `objective`: what the control is to achieve, a text of up to 1,000
  characters.
- `process`: the process it belongs to, in the tenant's own words, up to
  100 characters without white space around it. It is free text, not a
  list: the product has no process catalogue to refer to, and the
  library is filtered by its exact text.
- `owner_id`: a person of the tenant who is active when named (422
  `grc.invalid_owner` for an agent, a service or a person not active).
  The owner is checked when a control is recorded and when a change
  names another owner, not later: a control whose owner has left stays
  readable and changeable, and shows the owner's name as it is.
- `frequency`: `continuous`, `daily`, `weekly`, `monthly`, `quarterly`,
  `yearly` or `ad_hoc`, an open enumeration.
- `nature`: `manual` (a person performs it) or `automated` (the system
  does).
- `status`: `active`, or `retired` for good.

`POST /v1/controls` records it active. `PATCH /v1/controls/{id}` changes
its objective, process, owner, frequency or nature at once with the
caller's version (ADR-0008): the row is read under a lock, the version
compared, and a change that leaves it as it is refused (422
`grc.nothing_to_change`). There is no approval of a change in the first
version. `POST /v1/controls/{id}:retire` retires it once (409
`grc.control_retired` after): a retired control changes no more, takes no
test, keeps its code, and is still read with its tests. The database
holds the rules that do not depend on another context: the checks on
each column, the unique code, the update grant without `code`, no delete
grant, and a trigger that refuses any change of a retired row
(`control_retired_fixed`).

No control is shipped: a tenant's library starts empty (the plan's
default 11).

### 3. A test

A test (`grc.control_test`) is the result of testing a control once:
`tested_on` (a date), `result` (`passed` or `failed`), `note` (what was
seen and where the evidence is kept, up to 2,000 characters) and
`tested_by`, the caller. It is insert-only (`platform.forbid_mutation`,
no update or delete grant): a correction is another test.

- `POST /v1/controls/{id}/tests` records one on an active control. The
  control's row is locked, so a test and a retirement never pass each
  other; the control's version does not move, since nothing the control
  says changed. The version, and the ETag it is, guard what the control
  says, not its last test: a conditional read of a control, should one
  be built, must not answer "not modified" by the version alone.
- `tested_on` is the database's date today or earlier (422
  `grc.test_in_future`), the date identity judges a grant's validity by
  (`database.Today`). The library is the tenant's, not a company's, so
  no company's time zone applies.
- Recording a test is a person's: `x-agent: false` and a permission of
  its own, `grc.control.test`. A test states what a person saw; an
  agent's finding is a recommendation (T105f), which a person may then
  record as a test.

The control's **last test** is the test with the latest `tested_on` and,
among several of one date, the one recorded last (the latest id, a
UUIDv7). It is computed on read (`control_test_latest`), not stored on
the control: a test dated earlier than one already recorded never
becomes the last.

### 4. The library read

`GET /v1/controls` lists the tenant's controls by code in cursor pages,
each with its owner's name and its last test, filtered by `process`
(exact), `status`, `owner_id` and `last_result` (`passed`, `failed`, or
`none` for the controls never tested). Retired controls are listed
unless the status filter leaves them out. `GET /v1/controls/{id}` gives
one; `GET /v1/controls/{id}/tests` its tests, the last test first,
filtered by result.

### 5. Events and audit

Every change writes an audit record on the control
(`grc.control.create`, `.change` with the names of the fields changed,
`.retire`, `.test` with the test's id, date and result) and publishes
`ControlChanged` (`com.example.erp.grc.control_changed.v1`) through the
context's outbox in the same transaction, with `change` (`created`,
`changed`, `retired`, `tested`), the control's code, process, owner,
frequency, nature, status and version, and for a test its id, date,
result and tester. Neither carries the note, a person's free text that
may name people; people are named by id only. The event is not marked
`x-webhook` (week 3's rule).

### 6. Permissions and agents

`grc.control.read` (the library and the tests), `grc.control.write`
(record, change, retire) and `grc.control.test` (record a test). None is
an approval permission, and no segregation-of-duties rule is added
between them (product owner, 2026-11-12): the tenant's own rules apply.
The reads and the writes of a control are agent tools as any operation
is (an agent's writes within its autonomy level, 04 §4.13); recording a
test is closed to agents.

## Alternatives

- **The last test stored on the control.** It makes the library's filter
  by last result an index read, but a test recorded for an earlier date
  would have to decide whether it replaces it, and the control's version
  would move with every test. Computed on read, one index reads it, and
  a library holds hundreds of controls, not millions.
- **A process list in the configuration.** It would make `process` a
  reference. Nothing else in the product names processes yet, so a list
  would be configuration with one reader; free text filters as well and
  ships nothing.
- **Tests that can be corrected.** A test is evidence: what was recorded
  stays, and the later test is the correction, as an audit event's is.
- **`grc` importing identity's types**, as `credit` imports the
  ledger's. The plan asks for ports; the context needs three fields of a
  principal, and owning their type keeps `internal/layout`'s table
  without a row for it.

## Consequences

- A tenant can state its controls and what testing them showed, and an
  agent or an auditor reads both through the API.
- The library says nothing about whether a control is effective beyond
  its last test: test plans, sign-off, issues and remediation come later.
- `process` being free text, two spellings are two processes; the
  filter is exact.
- A person in a time zone ahead of UTC cannot date a test on their own
  today during the hours UTC is still on the day before; they date it
  the day before or record it later.

## Defaults chosen (for the product owner's digest)

1. A control's code is unique whatever its case and never changes; a
   retired control keeps it.
2. The process is free text; the frequency is one of seven values and
   the nature `manual` or `automated`.
3. The owner is an active person when named; an owner who leaves later
   stays on the control.
4. A control is changed at once by its permission, without approval;
   retired once, never deleted.
5. A test may be dated in the past, never after the database's date
   today; the last test is by test date, then by when it was recorded.
6. Recording a test has its own permission and is closed to agents;
   recording and changing controls is open to an agent with the
   permission and autonomy level 2.
7. `ControlChanged` is also published for a test, without the note; it
   is no webhook event.
8. Retired controls are listed unless filtered out.

## Questions

None for the product owner or a specialist from T98a.

## As built

### T98a Control library with test results (migration 000296)

- Migration 000296: schema `grc` with its outbox and inbox;
  `grc.control` and `grc.control_test` as §2 and §3 describe them; the
  permissions `grc.control.read`, `.write` and `.test`. Schema tests in
  `db/grc_test.go`.
- `internal/grc`: `Record`, `Change`, `Retire` and `RecordTest` with the
  handlers of `POST /v1/controls`, `GET /v1/controls`,
  `GET /v1/controls/{id}`, `PATCH /v1/controls/{id}`,
  `POST /v1/controls/{id}:retire`, `POST /v1/controls/{id}/tests` and
  `GET /v1/controls/{id}/tests`. The port `People` is wired to
  `identity.People` by `app.GRC`.
- `ControlChanged` in `api/asyncapi.yaml` (channel `grc`).
- Refusals beyond §2 and §3: a process with white space around it is 422
  `grc.invalid_process`; a frequency, nature or result the contract does
  not list, and a text of white space alone, are 422
  `request.validation_failed` naming the field.
- Walkthrough `step_controls` (step 66): Olive, the compliance officer,
  records the control PAY-01 on the release of payment runs, owned by
  Tara; a second control with the code in another case is refused; she
  records a test as passed with a note; a test dated after today is
  refused; the library, filtered by process and last result, shows the
  control with its last test.
- Decided by default while building: the defaults 1 to 8 above.
