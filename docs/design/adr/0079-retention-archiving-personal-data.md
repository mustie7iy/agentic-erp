# ADR-0079: Retention, archiving and personal data

**Status:** accepted (T99a, 2026-11-13; decided by default under ADR-0053 d,
accepted once `go-reviewer` had no blocking finding). It decides the records
management context and the slices T99a, T99b and T100a to T100c
(`docs/design/plan-breadth-week3.md`, Lane C), each of which records what
it built under "As built". T99a builds §1 to §4; §5 states the security
posture the product owner gave, which the later slices build to; T99b
builds §6 to §8, T100a §9 and §10.

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
T99a it had no schema and stored nothing: the rules are configuration,
and the status is computed on read. Its schema, with its outbox, came
with the first table it needs, T99b's archiving runs (§6), with two more
ports: the ledger's journal (`Journal`, `ledger.Archive`) and whether a
requester may still act (`Requesters`, `identity.Actors`), and the
object store (ADR-0016).

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

### 6. Archiving runs (T99b)

An archiving run archives one company's documents of one kind and one
fiscal year; the first version archives journal entries. It is asked for
by a person with `records.archive.run` and is accepted only when:

- the year is past retention under the company's rule in the active
  configuration, as §3 reads it on the company's date (422
  `records.year_not_past_retention`; a year not set up, or a company
  without a rule, is not past retention);
- the year is closed: it has posting periods and none of them, regular
  or special, in any of the company's ledgers is open (422
  `records.year_not_closed`). Postings go only into open periods, so the
  year's entries cannot change while it stays closed. The check holds the
  year's period rows `FOR SHARE` to the end of its transaction
  (`ledger.Archive.YearClosed`, in the carry-forward's order: by year,
  period, the leading ledger first, then by code), so a reopening, which
  takes its period `FOR UPDATE`, waits for the archiver's transaction and
  no entry is posted between the check and the read of the entries
  (ADR-0008). The archiver locks its run row first and the periods
  after; reopening locks periods only, so the two do not deadlock. A year-end close's
  sign-off locks every period (ADR-0060 §8), which closes the year in this
  sense; a year whose periods were closed by other means is closed too.

The run is queued (`records.archiving_run`, `queued`) in the request's
transaction with the rule's years and the configuration version, and
audited. A company, kind and year has one run that is queued or
completed: a second request answers it (200), whoever asks, before the
checks above, so a completed run is answered after the rule or the
periods changed; a failed run leaves room for another. The archiver (`records.Archiver`), a worker of
every replica as the cash exporter is (ADR-0051 §14), claims a tenant's
queued runs in order (`FOR UPDATE SKIP LOCKED`) and, in that one
transaction: refuses a requester blocked or deprovisioned since the
request (`identity.principal_blocked`, product owner 2026-11-03), and a
year opened since (`records.year_not_closed`); builds the file and its
manifest (§7) from the ledger's one read of the year (`ledger.Archive`,
one statement, so the entries are those of one moment); writes both to
object storage under the run's id; and completes the run with the file's
SHA-256 and size, the manifest's SHA-256 and the counts and debit total,
audited and published as ArchivingRunCompleted. A refusal fails the run
with its problem code, audited; the store not answering leaves it queued
for the next round; as for the cash exporter, a run that keeps failing so
(a year too large for the statement timeout) stays queued and is
claimed first each round, which a later size bound or failure count
addresses (Later). Objects are written once (ADR-0016): a round run again
after a commit whose acknowledgement was lost finds the same bytes under
the run's keys and takes them as written, so a run has one file and one
event; other bytes there fail the run (`records.archive_changed`). No row
of the ledger, the audit trail or any other context is changed or removed
(§5).

### 7. The archive file and its manifest

The file is JSON Lines (`application/x-ndjson`), named `<run id>.jsonl`
under the tenant's prefix (`tenants/<tenant>/archives/`): one journal
entry of the year per line, of every ledger, by posting date and then id,
each an object with `entry_id`, `ledger` (its code), `document_type`,
`document_number`, `fiscal_year`, `fiscal_period`, `document_date`,
`posting_date` (dates `YYYY-MM-DD`), `currency`, `local_currency`,
`group_currency`, `rate_to_local`, `rate_to_group` (decimal strings as
stored), `reference`, `header_text`, `source_context`,
`source_document_id`, `reversal_of_id`, `reversal_of_year`, `posted_at`
(RFC 3339, UTC), `posted_by` and `lines`; each line has `line_no`,
`account` (its number), `amount`, `amount_local`, `amount_group` (decimal
strings in their currency's digits, a debit positive and a credit
negative), `partner_id`, `tax_code`, `quantity`, `unit` and `text`. A
value the entry does not have is `null`. The manifest,
`<run id>.manifest.json` beside it, states the format
(`agentic-erp.records.journal_entry.v1`), the run, the company, the kind,
the year, the rule's years, the configuration version, who asked and when,
the file's name, SHA-256 and size, the counts of entries and lines and the
sum of the lines' debits in the company's currency (`debit_total`,
`currency`). Every value of the manifest is the run's or the file's, so a
run written again writes the same bytes. The API gives the manifest's figures and
its SHA-256 on the run; the manifest itself stays in object storage
beside the file.

### 8. The API and its permission

`POST /v1/archiving-runs` (201 queued, 200 for the run there already; 503
while the server has no object storage), `GET /v1/archiving-runs`
(filtered by company, kind, fiscal year and status, newest first),
`GET /v1/archiving-runs/{id}` (each run with its requester's id and name,
read from the identity context) and `GET /v1/archiving-runs/{id}/file` (the
file, checked against the run's SHA-256 before it is handed out, 409
`records.archive_changed`; 409 `records.archive_not_completed` before the
run completes; 503 without object storage). All need
`records.archive.run`; the file holds every entry of the year whole, so it
also needs `ledger.entry.read`. Starting a run and reading the file are
human-only (`x-agent: false`). The retention status gives each year's
`archived`: whether a completed run holds it.

### 9. The personal data inventory (T100a)

The inventory is a declared list in `internal/records`
(`records.Inventory`), shipped with the product rather than configured:
each entry is a column of the database that holds personal data, by
schema, table and column, with its category (`name`, `address`,
`contact`, `bank_details`, `identifier`, `network_address`,
`free_text`), why it is kept, and, per kind of data subject
(`business_partner`, `principal`), the column of its table that names the
subject. It covers the business partner's name, address and bank
details, a principal's name, e-mail and identity at the provider, the
names of people a document keeps as it was decided (a credit release, a
year sign-off), the audit trail's client addresses (ADR-0026 keeps
personal data out of the audit details, not this column), a proposed
partner change's payload, output messages' recipients and subjects and
bank statement lines' counterparties, and a partner's tax identifier (a
sole trader's social insurance number). Its tests hold it to the database:
every entry and link is a column; every column whose name is of a known
personal kind (names, addresses and contacts, bank details, subjects,
client addresses, encrypted values and their fingerprints and last four
characters) is named by the inventory or set aside with why (an
account's or a cost center's name, the company's own house bank), so a
migration adding one fails until it is; every schema has an entry or
says why it holds none (`records.SchemasWithout`). Free text a person
types (descriptions, comments, references), event payloads and object
storage are not inventoried in the first version (plan, "Later").

### 10. Where a subject's data is held

`GET /v1/personal-data-inventory` lists the entries (`records.privacy.read`).
`GET /v1/personal-data-inventory/subjects/{kind}/{id}` counts, for each
entry naming the kind of subject, the rows that name the subject and hold
a value in the column, one statement per table, in the request's
transaction under RLS. It returns counts, never a value. The counts are
of every company of the tenant, so the caller needs access to all
companies (403 `records.all_companies_required`); a subject the tenant
does not have is 404. Both are closed to agents (`x-agent: false`). The
records context reads these tables by SQL rather than through ports: the
inventory is about where data is stored, the columns are its own
declaration and the identifiers are sanitised; it reads no value and
writes nothing. The audit trail is counted by actor without an index of
its own: 200,000 changes of one person read in about 0.1 s
(`TestPersonalDataSubjectBudget`).

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

## Defaults chosen in T99b (for the product owner's digest)

6. A year is closed for archiving when none of its posting periods,
   regular or special, in any ledger of the company is open; a signed-off
   year close is not required.
7. A second request for a company, kind and year answers the run queued
   or completed, whoever asks; a failed run may be asked for again.
8. The archive holds every entry of the year of every ledger, in JSON
   Lines with amounts as decimal strings; its manifest's debit total is
   in the company's currency.
9. Listing and reading runs needs `records.archive.run`; the file needs
   `ledger.entry.read` besides; starting a run and reading the file are
   human-only.
10. The archive files are not locked in object storage (no retention
    lock): destroying an archive at the end of its retention is "Later",
    and the tenant's erasure (ADR-0021) erases them with its other
    objects.

## Defaults chosen in T100a (for the product owner's digest)

11. The inventory is declared with the product and refuses an unnamed
    column of a known personal kind; free text, event payloads and object
    storage are outside the first version.
12. A subject read counts across every company and needs access to all
    of them; it reads the other contexts' tables by SQL, counts only.
13. Names a document kept as decided (a credit release, a year
    sign-off) and the proposed partner changes are inventoried as
    personal data of the principal or partner they name.

## Questions

None for the product owner from T99a: questions 2 and 3 of the plan were
answered on 2026-11-12. Which periods Canadian and UAE law require stays a
question for the legal specialists before any period is shipped (plan,
"Later").

## As built

### T99a Retention rules and status (migration 000316)

- `internal/config`: `companies[].retention[]` (`RetentionRule`,
  `retention.go`), its validation, its tier and its list key, and
  `config.RetentionEnds`.
- `internal/ledger`: `Calendar.Years`, a legal entity's set-up fiscal
  years with their first and last days, published for the port.
- `internal/records`: the leaf context with `StatusOf` and the handler of
  `GET /v1/retention-status`; `app.Records` wires it.
- Migration 000316: the permission `records.retention.read`
  (`db/records_test.go`).
- Walkthrough `step_retention` (step 90): Rhea, the records
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

### T99b Archiving runs (migration 000317)

- Migration 000317: the schema `records` with its outbox and inbox, the
  table `records.archiving_run` (§6; its checks, the partial unique index
  `archiving_run_once`, the trigger `archiving_run_settled_fixed` and the
  column grants, `db/records_test.go`) and the permission
  `records.archive.run`.
- `internal/ledger`: `Archive`, the port's `YearClosed` and `Entries` (one
  statement over the year's entries and lines).
- `internal/records`: `RequestRun`, `Run`, `Runs`, `File` and their
  handlers (`archive.go`, `archive_api.go`); the file and manifest
  (`archive_file.go`); the worker `Archiver` (`archiver.go`), started by
  `erp-server` on every replica with object storage; `archived` in the
  status. `blobstore.KindArchives`. ArchivingRunCompleted in
  `api/asyncapi.yaml` (not a webhook event).
- Walkthrough `step_retention` (step 90, `retention_archive`): under the
  zero-year rule Rhea archives the prior year, closed by step 42's
  sign-off; the archiver completes the run, the file downloaded matches the
  run's SHA-256 and has one line per entry, as many as the ledger lists
  for the year; a second request answers the run; the year reads as
  archived; the current year is refused as not past retention.
- Decided by default while building: the defaults 6 to 10 above.
- Security posture (§5, product owner 2026-11-12): archiving deletes
  nothing; the run writes two objects and one row of its own.

### T100a Personal data inventory (migration 000318)

- `internal/records`: `Inventory` and `SchemasWithout` (`inventory.go`),
  `Subject` and the handlers of `GET /v1/personal-data-inventory` and
  `GET /v1/personal-data-inventory/subjects/{kind}/{id}`;
  `TestInventoryCoversTheDatabase`, `TestPersonalDataSubjectBudget`
  (p95 0.1 s for a principal with 200,000 audit events).
- Migration 000318: the permission `records.privacy.read`
  (`db/records_test.go`).
- Walkthrough `step_privacy` (step 91): Petra, the privacy officer, reads
  the inventory (the partner's name and bank details, the audit trail's
  client addresses among it) and Tyrell's rows per entry, counts and no
  value; her own name is counted once; an unknown kind is refused.
- Decided by default while building: the defaults 11 to 13 above.
