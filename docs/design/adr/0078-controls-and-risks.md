# ADR-0078: Controls and risks

**Status:** accepted (T98a, 2026-11-12; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T98a's and T98b's last commit). It decides
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
from identity (`People`) and, from T98b, the permission catalog and the
tenant's segregation-of-duties rules (`Access`) and the active
configuration version (`Settings`, §8). It posts nothing and changes nothing outside
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

### 7. A link

A link (`grc.control_link`) says what enforces a control in the product.
It has a `kind` and a `target`:

- `permission`: a permission code of the platform's catalog
  (`ref.permission`), from which every operation's `x-permission` is
  taken (`api`'s `TestOperations` holds the contract to it). This is what
  "exists in the contract" means for a permission.
- `sod_rule`: the id of a segregation-of-duties rule of the tenant
  (`identity.sod_rule`).
- `configuration_path`: a path of the configuration, written as a
  configuration change names a setting (`config.Change.Path`):
  `companies[3000].release_dual_limit` for a setting,
  `companies[3000]` for a list item, `payments` for a section.

`POST /v1/controls/{id}/links` makes a link on an active control, with
exactly the field of its kind (422 `grc.invalid_link`). What it names
must be there when it is made: a permission of the catalog (422
`grc.unknown_permission`), a rule the tenant has (422
`request.reference_not_found`), a path the active configuration version
has a value at (422 `grc.configuration_path_without_value`). A rule that
is not in force yet, or has ended, can be linked: the tenant may link a
rule it has scheduled, and the matrix shows the link as broken until the
rule starts. A control has one active link to a target (409
`grc.control_link_exists`).

There is no `DELETE` in the API (04 §4.3, `TestOperations`), so the
plan's `DELETE /v1/controls/{id}/links/{link_id}` is built as
`POST /v1/controls/{id}/links/{link_id}:remove`: the link moves to
`removed` once, at the link's own version (`If-Match`), is kept, is left
out of the matrix, and its target can be linked again. Only a person or
an agent removes a link: one that no longer holds stays, shown as
broken. A link does not move its control's version; it has an audit
record on the control (`grc.control.link`, `.unlink`) and
`ControlChanged` (`linked`, `unlinked`, with the link's id, kind and
target). A retired control's links stay as they are.

No foreign key leaves the schema: what a link names may go (a permission
dropped by a later release, a rule ended, a setting removed by a
configuration change) and the link must outlive it to be shown as
broken.

### 8. The control matrix

`GET /v1/control-matrix` is the tenant's active controls by code, each
as `getControl` gives it (with its framework references and its last
test), with its active links and, for each link, whether it **holds** on
the database's date today (`as_of`):

| Kind | Holds while | Otherwise `broken_reason` |
|---|---|---|
| `permission` | the catalog holds the code | `permission_not_in_catalog` |
| `sod_rule` | the rule is in force: `valid_from` ≤ today < `valid_to`, as `authz` judges a rule | `sod_rule_not_started`, `sod_rule_ended`, `sod_rule_not_found` |
| `configuration_path` | the active configuration version has a value at the path or below it | `configuration_path_without_value` |

A value is there when the configuration document, flattened as a
configuration change's paths are (`config.Config.Present`), has a
non-empty setting at the path or below it; custom field definitions
(`extensions`) are not looked at, as `config.Diff` compares them apart.
A permission link shows the permission's description, a rule link the
rule's pair, description and validity, a path link the number of the
configuration version it was looked up in.

Each row has a `link_state`: `holding` (every link holds, and there is
one), `broken` (at least one does not) or `none`. The matrix is filtered
by `process`, by one `framework_reference` and by `link_state`, and
carries `totals` of the controls and links the filters select, over
every page. It is computed at the read and stores nothing: the links
selected are all judged first (identity and the configuration are each
asked once), then the page is read among the controls selected. It is a
report (`x-slo-class: report`, a budget of 3 s at p95) read from one
database state (`x-snapshot: true`).

The matrix shows segregation-of-duties rules and whether the
configuration has a value at a path, which are identity's and the
configuration's to show, so it and the two link operations also need
`identity.access.read` and `config.version.read` (`x-also-permissions`;
04 §4.4: an answer never crosses permissions).

The context reads identity through the port `Access` (the catalog's
descriptions, the tenant's rules) and the configuration through the port
`Settings` (the active version's number and which paths have a value),
both in types of its own, wired by `app.GRC` to `identity.Catalog` and
to `configuration.Settings` with `config.Config.Present`.

### 9. Framework references

`framework_references` on a control is a list of up to 50 texts of up to
200 characters each, the tenant's own references to a framework it
names, in the order it gives them, set when the control is recorded and
replaced as a whole by a change. No framework's text or numbering is
shipped and nothing checks a reference against one (the plan's default
11): a shipped mapping needs a compliance specialist and comes later.

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
- A link's state is as good as what it names: a permission link says the
  permission exists, not who holds it; a path link says the setting has
  a value, not which.
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
9. A link is removed with `POST …/links/{link_id}:remove` and kept as
   removed, not with `DELETE` as the plan wrote it: the API has no
   `DELETE` (04 §4.3).
10. A link to a segregation-of-duties rule holds only while the rule is
    in force today; a rule that has not started is shown as broken
    (`sod_rule_not_started`), and can be linked.
11. A configuration path is written as a configuration change names a
    setting; it has a value when a setting at or below it is not the
    empty text, so a number or a switch always has one while its item
    exists. Custom field definitions are not looked at.
12. What a link names must be there when the link is made.
13. The matrix and the link operations also need `identity.access.read`
    and `config.version.read`.
14. The matrix lists active controls only, with totals of what its
    filters select; `ControlChanged` is also published for a link made
    or removed.

## Questions

None for the product owner or a specialist from T98a or T98b. One fact
for the coordinator: the plan's walkthrough line for T98b ("linked ... to
the SoD rule of the walkthrough's tenant; the matrix shows both links as
holding") cannot be met on a tenant's first day. That tenant has no
`identity.sod_rule` row, and identity refuses a rule that a principal
would break on its first day while the administrator's role holds every
permission and a grant ends tomorrow at the earliest. The step shows the
rule's link as not started (As built, T98b); T105f's step meets the same
limit.

## As built

### T98a Control library with test results (migration 000304)

- Migration 000304: schema `grc` with its outbox and inbox;
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

### T98b Control matrix (migration 000305)

- Migration 000305: `framework_references` on `grc.control`
  (`grc.framework_references_valid`) and `grc.control_link` as §7
  describes it, with a trigger that refuses any change of a removed
  link. Schema tests in `db/grc_test.go`.
- `internal/grc`: `Link`, `Unlink` and the matrix (`selected`, `judged`)
  with the handlers of `POST /v1/controls/{id}/links`,
  `POST /v1/controls/{id}/links/{link_id}:remove` and
  `GET /v1/control-matrix`; `framework_references` on `createControl`,
  `updateControl` and every read of a control.
- Published by other contexts for the ports: `identity.Catalog`
  (`Permissions`, `SodRules`) and `config.Config.Present`.
- `ControlChanged` gains `link` and the changes `linked` and `unlinked`.
- `TestMatrixBudget`: 2,000 active controls with 6,000 links and 20,000
  tests, each read within 3 s at p95.
- Walkthrough `step_control_matrix` (step 67): Olive gives PAY-01 a
  framework reference and links it to the permission
  `payments.run.release` and to the setting
  `companies[3000].release_dual_limit`, both holding. The walkthrough's
  tenant has no segregation-of-duties rule: the rule that whoever
  proposes a payment run does not release it is built into payments, not
  a row of `identity.sod_rule`. Ada makes one (administering access
  against releasing payment runs): identity refuses a rule its
  administrator would break today, and a grant ends tomorrow at the
  earliest, so the rule starts tomorrow and its link is shown as broken
  (`sod_rule_not_started`) today. The matrix shows the three links, two
  holding and one broken, with the last test and the totals; a link made
  by mistake is removed at its version.
- Decided by default while building: the defaults 9 to 14 above.
