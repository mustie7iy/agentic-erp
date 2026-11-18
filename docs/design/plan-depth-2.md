# Depth plan 2: second variant slices (17 November 2026)

**Status:** plan (product owner, 16–17 November 2026: breadth is built;
the variants go on, pilot-relevant first). Each lane splits two named
variants of its placeholder into slices in the form of
`plan-breadth-week3.md` (Adds, Acceptance, Later) and builds them, or,
where its slice is a tracker leaf already, writes the leaf's section.
`docs/tracker.json` stays the only plan (07 §7.5); each leaf's `r` names
its section below. Each lane writes its own `## <lane>` section.

## coll

Collections, "later: collection worklist variants"
(`plan-breadth-week1.md` §T76c, Later; ADR-0065). SAP S/4HANA's
Collections Management (FSCM-COL) is the reference: a collection
specialist works the worklist, records a customer's promise to pay for
invoices (promised amount and date; the promise is kept when paid,
broken when the date passes unpaid), and records each contact with the
customer (whom, when, the result, a note) with a resubmission date that
takes the customer off the worklist until then. Two variants are split
here; assigning customers to collectors and strategies and priorities
stay in the placeholder.

### T76f Promises to pay

**Capability.** A collections specialist records a customer's promise to
pay an amount by a date for some of its open items; the promise reads
open, kept or broken by the company's today; the worklist shows each
customer's open and broken promises, and a dunning run holds the
promised items back up to the promised date.

- **Reuses:** the collections context and its ports; the open item check
  of dispute cases and blocks (`customersItem`); the run's lists of items
  held back (`disputed_items`, `blocked_items`) and the confirmation's
  stale check (T76e); `config.DocumentDay` and `Endpoints.Now` (the
  company's today); `ledger.OpenItems.Closed` (whether a promised item is
  still open); cursor paging; audit.
- **Adds:** tables `collections.promise_to_pay` and
  `collections.promise_to_pay_item` (insert-only); `POST
  /v1/promises-to-pay`, `GET /v1/promises-to-pay`, `GET
  /v1/promises-to-pay/{id}`; permission `collections.worklist.write`
  (recording a worklist's work: promises here, contacts in T76g; read with
  `collections.worklist.read`); the run's `promised_items` with their
  count and amount; on the worklist per customer `promised_amount`,
  `open_promises`, `next_promised_date` and `broken_promises`, and the
  worklist's `promised_amount` total. No event (as a block: nothing
  outside the context changes).
- **Walkthrough:** `step_promise_to_pay`: Lakeshore Hardware (steps 111
  and 112) promises CAD 30.00 for the gasket invoice and CAD 600.00 for
  the pump invoice by two dates; a second promise of the pump, an amount
  above the items and a date in the past are refused; a run at a key
  date before the dates names both invoices as promised and duns neither;
  Lakeshore pays the gasket and that promise reads kept; the worklist
  after the pump's date shows its promise broken, and a run at that date
  duns the pump again.
- **Acceptance:** the amount is in the company's currency, above zero
  and at most what the items amount to in it (422 naming the field); each
  item is an open item of the company the customer owes, named once
  (422); an item in another promise whose date has not passed is refused
  (409), and two promises of one item racing let one win; the date is
  from the company's today to 2100-12-31 (422); kept is the promised
  items no longer open adding up to the promised amount at least, broken
  is not kept once the company's today is past the date, open otherwise;
  a run at a key date up to the date leaves the promised items out of its
  notices and totals and names them, a block taking precedence and a
  dispute coming after; a proposal with an item promised since is stale
  on confirmation; the worklist judges promises at its key date; schema
  tests for both tables.
- **Depends on:** T76e (in `main`).

### T76g Contact notes and resubmission dates

**Capability.** A collections specialist records a contact with a
customer of a company (when, whom they spoke to, the outcome, a note)
with an optional resubmission date; the worklist shows each customer's
last contact and leaves out a customer whose last contact's resubmission
date is after the key date, until that date.

- **Reuses:** the collections context; `config.DocumentDay` and
  `Endpoints.Now`; the people port (who recorded the contact, by name);
  cursor paging; audit; `collections.worklist.write` (T76f).
- **Adds:** table `collections.customer_contact` (insert-only: a contact
  is a record of what happened, corrected by a later one); `POST
  /v1/customer-contacts`, `GET /v1/customer-contacts`, `GET
  /v1/customer-contacts/{id}`; on the worklist per customer
  `last_contacted_at`, `last_contact_outcome` and `resubmission_date`,
  and the worklist's `resubmission_count` and
  `resubmission_overdue_amount` of the customers left out; the contact's
  person and note in the personal data inventory. No event.
- **Walkthrough:** `step_collection_contacts`: Ruth calls Lakeshore about
  the broken promise, reaches its accounts payable clerk and sets a
  resubmission date; a contact in the future is refused; the worklist
  before that date leaves Lakeshore out and counts it, at that date it is
  back with its last contact; a later contact without a resubmission
  date brings it back at once; the contacts read latest first.
- **Acceptance:** `contacted_at` is not after the clock (422 naming the
  field); the outcome is one of the open enumeration (422); the
  resubmission date is after the company's today and at most 2100-12-31
  (422); the customer is a business partner of the tenant (422); the last
  contact is the latest by `contacted_at`; texts of white space alone
  are refused; schema tests for the table and its insert-only rule.
- **Depends on:** T76f.

**Later (still in the placeholder):** assigning customers to collectors
and collection groups; strategies and priorities (the worklist's order by
rule); a promise in instalments, changed or withdrawn, and its tolerance
days; a promise kept by a payment's date rather than by the items being
open when read; contact outcomes from configuration; reminders to the
collector on the resubmission date.
