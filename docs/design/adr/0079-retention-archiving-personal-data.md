# ADR-0079: Retention, archiving and personal data

**Status:** accepted (T99a, 2026-11-13; decided by default under ADR-0053 d,
accepted once `go-reviewer` had no blocking finding). It decides the records
management context and the slices T99a, T99b and T100a to T100c
(`docs/design/plan-breadth-week3.md`, Lane C), each of which records what
it built under "As built". T99a builds §1 to §4; §5 states the security
posture the product owner gave, which the later slices build to.

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first). The
tracker's area Archiving and data retention has the placeholders Retention
rules and Archiving runs, and the privacy area the personal data
inventory, blocking and deletion. `plan-breadth-week3.md` details them as
T99a (retention rules and status), T99b (an archiving run), T100a (the
personal data inventory), T100b (personal data blocked) and T100c (a
deletion run) in a new context `records`, and asks T99a to write this
ADR.

What the design already settles:

- A leaf context reads others through ports and nothing imports it
  (ADR-0051 §2); the plan names `records` as one: "it reads the ledger and
  blanks a partner's data through a port the partner context publishes;
  the retention rules themselves are a section of the configuration
  document".
- The configuration document, its validation, tiers, change requests and
  approval (`internal/config`, `internal/configuration`), as dunning
  levels (ADR-0065 §2) and pricing (ADR-0067 §11) use them.
- The ledger and the audit trail are append-only (ADR-0006, ADR-0011).
- The company's date in its time zone (`config.Config.CompanyDay`,
  product owner 2026-10-31).
- The plan's default 12: retention rules ship empty; archiving writes a
  file and deletes nothing; a deletion run blanks a blocked partner's
  master data, touches no audit event and removes no row, and finds
  nothing without a rule the tenant set.
- The product owner's answers of 2026-11-12 to the plan's questions 2 and
  3 (§5).

Nothing settles where a fiscal year's retention is counted from, how a
year ending on 29 February is counted, what tier a rule's change is, or
how the status is read. This ADR decides them.

## Decision

### 1. A leaf context `records`

A new bounded context `records` (package `internal/records`). It is a
leaf (`internal/layout`): no context imports it. It reads, through ports
it declares with the types their published APIs give, tenancy's legal
entities (`LegalEntities`, `tenancy.Directory`), the active configuration
(`Settings`, `configuration.Settings`) and the ledger's set-up fiscal
years (`Calendar`, `ledger.Calendar.Years`), wired by `app.Records`. In
T99a it has no schema and stores nothing: the rules are configuration,
and the status is computed on read. Its schema, with its outbox, comes
with the first table it needs (T99b's archiving runs).

### 2. Retention rules in the configuration

A company's retention rules are the list `companies[].retention[]` of
the configuration document, each a `kind` and `years`:

- `kind`: the kind of document the rule is for. The first version knows
  `journal_entry` (the company's journal entries) and `personal_data`
  (the personal data it holds, for T100c). Another is refused when the
  document is validated.
- `years`: the whole years a document is kept after the end of the
  fiscal year it belongs to, from 0 to 99. A rule without `years` is
  refused rather than read as zero, which would put every ended year past
  retention.

A kind has one rule per company (a second is refused). The list is keyed
by kind, so a change names `companies[3000].retention[journal_entry].years`.
A change of the rules is tier 3, with two approvers (the process owner
and the controller) after the simulation and regression evidence tier 3
asks for: a rule posts nothing, but it decides which years a later run
may archive or erase, and an erasure cannot be undone (product owner,
2026-11-13, security posture; T99a first built tier 2). The
rules change only through a change request and its approval, as every
setting does. No rule is shipped or defaulted for any jurisdiction, in
the product or in a country pack (the product owner's answer to
question 3, 2026-11-12): a company without a rule for a kind has nothing
of that kind past retention.

### 3. The retention status

For a company and a kind, each fiscal year set up for the company (the
ledger's regular periods of its fiscal calendar, the earliest year
first) is read with:

- its first and last day: the first day of its first regular period and
  the last day of its last;
- the day its retention ends: its last day plus the rule's years, the
  same day of the month that many years later, or the last day of
  February when the last day is 29 February and the later year has none;
- whether it is past retention: the company's date today, in its time
  zone, is after that day.

A document is kept through the day its retention ends, so a year is past
retention from the day after; under a rule of zero years, a fiscal year
is past retention once it has ended. Without a rule no day is given and
nothing is past. The status names the active configuration version the
rule was read from and the totals of the years listed and of those past
retention. It changes nothing: past retention says only that a later
step may archive (T99b) or erase (T100c) the year's documents of the
kind.

### 4. The API and its permission

`GET /v1/retention-status` with `legal_entity_id` and `document_kind`
gives the status of §3 (422 `request.validation_failed` naming
`document_kind` for a kind the first version does not know; 409
`config.not_bootstrapped` for a tenant without a configuration; 404 for
a legal entity the caller cannot act in, as for one that does not
exist). It needs `records.retention.read`, not an approval permission,
and is an agent tool as any read is. The years are not paged: a company
has one fiscal year a year.

### 5. Security posture of archiving and deletion

The product owner answered question 2 on 2026-11-12 as recommended:
archiving deletes nothing from the database; a deletion run blanks the
master data of blocked partners, keeps every ledger document and touches
no audit event, and is irreversible; it runs behind a retention rule the
tenant must set. The coordinator read "as recommended" as permission to
build deletion before a privacy lawyer has read this ADR, and asked once
more without blocking (product owner, 2026-11-12). T99b, T100b and T100c
record what they build under this section in "As built".

## Alternatives

- **Retention per document type or per account.** SAP's retention is per
  object and may differ by document type; the plan's first version is per
  company and document kind, and a finer rule comes later with the legal
  source for it.
- **Retention counted from the document's date.** A fiscal year is what
  the ledger closes and what an archiving run archives as a whole, so
  counting from the year's end lets a year be archived at once. Which
  date a jurisdiction counts from is a legal fact not stated here
  (question 3); another starting date comes later with its source.
- **Rules in a table of the context, changed through its own API.** The
  plan places them in the configuration so that they are versioned,
  approved and compared as every setting is, and a country pack can ship
  them later with their legal source.
- **Tier 2 for a change of the rules**, as T99a first built it: a rule
  posts nothing and a shorter rule only lets a later step act, itself
  behind its own permission, so the process owner's approval alone was
  taken to suffice. The product owner chose tier 3 (2026-11-13): a
  shorter rule leads to erasure, which cannot be undone, so it takes two
  approvers.

## Consequences

- A records administrator reads, per company and kind, which fiscal years
  may be archived or erased, and why (the rule and its configuration
  version).
- A tenant that sets no rule keeps everything: nothing is ever past
  retention, and the later archiving and deletion runs find nothing.
- A rule changed to fewer years puts years past retention at once; the
  change is a governed change with its approver and audit record.

## Defaults chosen (for the product owner's digest)

1. Retention is counted from the fiscal year's last day (the last day of
   its last regular period); a year is past retention from the day after
   its retention ends, on the company's date.
2. A year ending on 29 February keeps its documents until 28 February of
   a later year without one.
3. A change of the retention rules is tier 3, with two approvers
   (product owner, 2026-11-13, replacing tier 2).
4. A rule's years are 0 to 99 and must be given; zero keeps documents
   until the fiscal year ends.
5. The status lists every fiscal year set up for the company, past and
   future, without paging.

## Questions

None for the product owner from T99a: questions 2 and 3 of the plan were
answered on 2026-11-12. Which periods Canadian and UAE law require stays a
question for the legal specialists before any period is shipped (plan,
"Later").

## As built

### T99a Retention rules and status (migration 000309)

- `internal/config`: `companies[].retention[]` (`RetentionRule`,
  `retention.go`), its validation, its tier and its list key, and
  `config.RetentionEnds`.
- `internal/ledger`: `Calendar.Years`, a legal entity's set-up fiscal
  years with their first and last days, published for the port.
- `internal/records`: the leaf context with `StatusOf` and the handler of
  `GET /v1/retention-status`; `app.Records` wires it.
- Migration 000309: the permission `records.retention.read`
  (`db/records_test.go`).
- Walkthrough `step_retention` (step 79): Rhea, the records
  administrator, reads that Maple has no rule and nothing past
  retention; Ada sets ten years for journal entries through a tier 3
  change, replayed and approved by Priya and Carla, and the migrated prior year is kept until
  ten years after its last day; a rule of zero years, activated the same
  way, puts the prior year past retention and leaves the current year
  within it; personal data has no rule, and an unknown kind is refused.
- Decided by default while building: the defaults 1 to 5 above.
- Tier 3 (product owner, 2026-11-13, po:2026-11-13): the rule of
  `internal/config` `diff.go` makes every change of
  `companies[].retention[]` tier 3, so a change waits for the
  simulation and regression evidence and two approvers; T99a had built
  it tier 2 (`TestRetentionTier`, the walkthrough's `retention_rule`).
