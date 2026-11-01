# ADR-0056: Recurring and template entries: saved parked entries, parked per due date for their owner, never posted automatically

**Status:** accepted (scope and posture decided by the product owner on
2026-11-01 on the lane's proposal; the rest decided by default under
ADR-0053 d).

## Context

The pilot leaf "Recurring and template entries" (tracker, pilot
po:2026-10-31) had a title and nothing else: no design document, ADR or
tracker item described it. The lane proposed one slice built from the
general ledger's existing patterns (parked entries and their approval,
T35; posting periods, T32; the workflows dispatcher's statement fetch,
ADR-0047; company-local days, T72n), and the product owner approved it
on 2026-11-01 with these answers:

1. Entries a schedule generates are **parked only**, never posted
   automatically: a person posts them, within the approval limit or with
   someone else's approval. Posting in a person's name without their act
   would change the segregation-of-duties posture.
2. A template is usable **without a schedule**: a person parks an entry
   from it once.
3. Frequencies **monthly, quarterly and yearly**, on a day of the month
   clamped to the month's last day, on company-local days, ending on a
   date, after a count, or never.
4. **Fixed amounts** per date, editable on the parked entry before it is
   posted; no formulas, no variable amounts, no automatic reversal.
5. When a recurring entry is parked, the template's **owner gets an inbox
   item** as well as the event.

A new ADR rather than an amendment: ADR-0006 (append-only ledger) and
its T33f amendment are about the journal, which this leaves untouched,
and no ADR covers parked entries' lifecycle; the template is a new
aggregate with its own schedule semantics, a new system principal that
parks for a person, and an inbox decision, which want one record.

## Decision

**A template is a saved parked-entry body.** `ledger.entry_template`
holds a parked entry's header without its dates (legal entity, ledger or
ledger group, document type, currency, reference, header text), a name
unique in its legal entity, an owner, a status `active | retired` and
an optional schedule; `ledger.entry_template_line` holds its lines
exactly as `ledger.parked_entry_line` does (amounts in minor units of the
template's currency, centers, segment, partner, tax code and base,
jurisdiction, quantity). Row-level security, the audit trail and the
outbox apply as for every ledger aggregate: `EntryTemplateChanged` on
saving, changing, retiring and a refused date. It is checked as parking
checks an entry (`prepare` and `parkable`) on its schedule's first date
or, without one, its company's date today, and again on every use,
since accounts and centers may be blocked later.

API (`/v1/entry-templates`): `createEntryTemplate`, `listEntryTemplates`,
`getEntryTemplate`, `updateEntryTemplate` (merge patch, If-Match),
`retireEntryTemplate` (If-Match) and `parkFromEntryTemplate` (If-Match:
park once, with a posting date and optionally a document date and
reference). Writes need `ledger.entry.park`, reads `ledger.entry.read`:
a template is a parked entry kept for reuse, so no new permission. People
and agents (autonomy level 1 and up, `x-autonomy: propose`) save
templates, as they park entries. A person owns what they save; an agent's
or a service's template has no owner until a person takes it on by
changing it, and its schedule parks nothing until then. Only the owner
changes or retires it (`ledger.not_template_owner`), by the parked
entries' rule (`takenOn`). Anyone who may park in its company may park
from it: the entry is theirs, as any parked entry is.

**The schedule.** Every 1, 3 or 12 months on `day_of_month` (the month's
last day when shorter), from `starts_on` (its month's date, or the next
period's when that date is before it), until `ends_on`, for
`occurrences` dates, or without end. A date is due once it is today or
past on the template's company's day in its time zone
(`companies[].time_zone`). The template keeps `next_due_on` (the first
date after the last it parked or refused) and `last_occurrence_on`; a
changed schedule goes on after the last date dealt with, so it parks
nothing for dates before it.

**Parking per due date, in order.** The workflows dispatcher has a
`recur` step on the statement fetch's pattern (ADR-0047): it asks, as the
worker role across tenants, which tenants have active owned templates
whose `next_due_on` is at most UTC's date plus one (a superset: no zone is
more than a day ahead of UTC), then in each tenant, as the system
principal `ledger.Recurrer` (`urn:agentic-erp:entry-recurrer`), which
templates' next date is today or past on their company's day. Per
template it starts `ParkRecurringEntry` for that one date, with id
`{tenant}-entry_template-{template}-{date}-v{version}`: the engine starts
one per date however many replicas and rounds dispatch it, and a template
changed meanwhile (a new version) gets a fresh start instead of an id the
engine refuses for good. Dates are dealt with strictly in order: the
template moves to its next date only once this one is parked or refused,
so a missed run catches up one date per template and round, oldest first.
The workflow's one activity, `ParkOccurrence`, locks the template FOR
UPDATE and skips a date before its `next_due_on` (parked, refused, or no
longer one of the schedule's) and a date of a template retired, without a
schedule or without an owner; a date after `next_due_on` (an earlier one
still pending) or not yet due on the company's day is an error the
activity's retry policy retries, so no date is passed over unseen. A due
date is checked for the owner (below) and parked dated that day (document
and posting date) as the recurrer, owned by the template's owner, naming
the template and the date (`created_by_kind` `system`); the template then
moves past it. A second run, or one whose completion the engine lost,
finds the date behind `next_due_on` and does nothing, publishing nothing
twice, and `parked_entry(tenant_id, template_id, occurrence_date)` is
unique besides. A cell without a workflow engine runs the same activity
in process (`workflows.Recurrer`), as the payment release does.

**The owner's standing.** The recurrer acts for the owner, so each date
checks the owner's standing that day: an active principal (identity's
`Actors.Active`, a port), holding `ledger.entry.park` unbarred, with
access to the template's company. Otherwise the date is refused with
`ledger.owner_not_permitted` (below). Tenancy needs no such check: the
recurrer's transaction is scoped to the tenant the worker role read, and
a template's company never changes.

**Never posted automatically.** The generated entry is an ordinary parked
entry: its owner changes it (amounts included), posts it with
`postParkedJournalEntry` (within the limit, or with someone else's
approval above it) or cancels it. Parking checks no open period, as
today; a date in a closed or locked period still parks, and posting it
is refused with `ledger.period_not_open` until the period opens, so no
date is skipped silently.

**A refused date** (an account blocked or a company inactive since the
template was saved, the GR/IR account named on a line, an owner who may no
longer park there) is parked in a savepoint; the refusal is recorded on the template (`last_refusal`: date,
problem code and detail; the latest one kept), audited, published
(`EntryTemplateChanged`, `refused_on`, `refusal_code`) and passed over,
so later dates go on: the template is corrected and that date parked
with `parkFromEntryTemplate`; earlier refused dates are in the audit trail
and their events. The template's owner never changes once set, so a
template whose owner left parks nothing more; transferring templates is
not built (a gap for the product owner). A failure that is not a business
problem (the database unavailable, a lock timeout) is retried by the
activity's retry policy, which has no attempt limit.

**The owner's inbox item.** The platform has no inbox for anything but
approval requests (`listApprovalRequests?decidable_by=me`, ADR-0052 G1)
and no notification store. The smallest consistent extension is the same
kind of server-computed filter on the resource that waits for the person:
`listParkedJournalEntries?owned_by=me` (with `status=parked`: what waits
for the caller to post it), plus `template_id`. The parked entry, owned by
the template's owner, is the inbox item; it leaves the inbox when it is
posted or cancelled. The parked entry and `ParkedJournalEntryChanged` carry
`template_id`, `template_name` and `occurrence_date`; the event is the
owner's notice. An agent's or a service's
`owned_by=me` is empty, as their approval inbox is.

## Alternatives

- **Post automatically within the limit.** Rejected by the product owner:
  a service would post in a person's name without that person acting.
- **A notification store** (`platform.notification` with a recipient,
  read and dismissed states, an endpoint). A new platform capability for
  one producer; the parked entry already is the work item and has an
  owner. Kept for when e-mail or chat notifications come (ADR-0052 §12).
- **Expand the schedule at read time** (as cash memo records do,
  ADR-0051 §7). A forecast can; a ledger entry is a document someone
  posts and must exist once, with its own lifecycle.
- **One long-lived workflow per template** (a timer loop). Long histories
  to version and continue-as-new for years; a template change would have
  to reach a sleeping workflow. A short workflow per date keeps the
  engine's state small and the business key in its id.
- **Templates in jsonb.** Their lines refer to accounts, centers and
  units the database keeps referential; the parked entry's line table
  already has the shape.

## Consequences

- One new aggregate and two tables, a new parked entry kind (`system`)
  that the API's open enumeration admits, and a cross-context import
  (`workflows` → `ledger`), in the direction `workflows` → `payments` →
  `ledger` already ran.
- A failed `ParkRecurringEntry` (a non-retryable error other than a
  business refusal, which only an unreadable date input is) leaves its date
  as the template's next: no later date parks before it, and the next
  change of the template starts it again under its new version; operators
  see the failed workflow (ADR-0012 exception queue). No date is passed
  over without a parked entry or a recorded refusal.
- Variable amounts, formulas, allocation templates (ADR-0050 cost
  allocation), automatic reversal of accruals, weekly schedules and
  screens in the web workspace are not built; each would be a new leaf.

## As built (2026-11-01)

- Migration 000183 (`ledger.entry_template`, `ledger.entry_template_line`,
  `parked_entry.template_id`/`occurrence_date`, the kind check widened to
  `system`, NOT VALID) and 000184 (validation); the integration agent
  renumbers them in the batch.
- `internal/ledger`: `schedule.go` (dates), `templates.go`,
  `templates_api.go`, `recur.go` (`RecurringTenants`, `DueOccurrences`,
  `ParkOccurrence`, `ownerMayPark`); `identity.Actors.Active`; line writes shared with parked entries
  (`putLines`); `internal/workflows/recur.go` (`ParkRecurringEntry`,
  `Recurrences`, the dispatcher's `recur`, `Recurrer`); `app.Ledger`;
  erp-server wires the activity, the dispatcher step and, without an
  engine, the in-process worker.
- Decided by default: the template's permissions are the parked entries'
  (no new permission); `parkFromEntryTemplate` needs `If-Match`, as every
  custom method does (06 §6.3); the template's version moves when the ERP
  parks or refuses a date (its representation changes); the first date is
  in `starts_on`'s month unless before it; a refused date is recorded and passed
  over rather than retried (it would block the template's later dates);
  one date per template per round, in order, rather than all due dates at
  once (go-reviewer round 1: a later date must never move the template
  past an earlier one not dealt with); the workflow id carries the
  template's version; the recurrer checks the owner's standing each date
  (go-reviewer round 1, ADR-0010: a system principal acting for a person).
- Workflow determinism: `ParkRecurringEntry` issues one activity and
  reads no clock; its first version needs no `GetVersion` marker, and
  `TestParkRecurringEntryReplay` replays a recorded version 1 history
  (testdata) so a later change must add one.
