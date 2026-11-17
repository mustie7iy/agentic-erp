# Depth plan, first variant slices (from 17 November 2026)

**Status:** plan (product owner, 2026-11-17: breadth is built; the
variants start, pilot-relevant first). Each lane splits the two variants
its brief names out of a "later: …" leaf of `docs/tracker.json` into
slices, in the form of `plan-breadth-week3.md`, and narrows that leaf to
what remains. `docs/tracker.json` stays the only plan (07 §7.5); each new
leaf's `r` names its section below. What every slice reuses is
`plan-breadth-week3.md`'s list, unchanged.

## dunning

Collections, "later: dunning variants" (`plan-breadth-week1.md` §T76b,
Later; ADR-0065). SAP S/4HANA's dunning procedure (FBMP) is the reference:
grace days on the line items, charges per dunning level, interest per
level at a rate, a minimum amount below which a customer is not dunned,
and dunning blocks on the customer's company data and on a line item. The
company's dunning configuration (`companies[].dunning`) is its one
procedure; procedures per customer stay later.

### T76d Dunning charges, interest, grace days and minimum amounts

**Capability.** A credit controller's dunning run charges each notice its
level's dunning charge and interest on its items at the company's annual
rate, leaves out the items still within the company's grace days and the
notices below its minimum amount.

- **Reuses:** the proposal (`internal/collections/propose.go`), the
  configuration's dunning levels and their validation and tier
  (`internal/config/dunning.go`), `money` (rounding half away from zero,
  `money.Sum`), the run's events.
- **Adds:** configuration `companies[].dunning.grace_days`,
  `.minimum_amount`, `.interest_rate_percent`, `levels[].charge`,
  `levels[].interest` (tier 2, as the levels); the run keeps the settings
  it applied and its charge, interest and below-minimum totals; a notice
  its charge, interest and total; an item its interest; the events carry
  the charge and interest. No new endpoint or table.
- **Walkthrough:** `step_dunning_charges`: a new customer with a small
  invoice and a larger one on longer terms; Ada sets grace days, a minimum
  amount, an interest rate and the levels' charges through a tier 2
  change; a run within the grace days proposes nothing of the customer, a
  later one names its notice as below the minimum, a run once both
  invoices are overdue proposes it, is confirmed, and the next run
  charges the second level's charge and the interest of the days overdue.
- **Acceptance:** an item overdue by no more than the grace days is not
  proposed; a notice whose overdue amount is below the minimum amount is
  not proposed and its items are not recorded when the run is confirmed,
  the run counting it; the charge is the notice level's; interest is
  amount × rate × days overdue ÷ 365 per item of a level that charges
  interest, rounded half away from zero in the company's currency; the
  notice's total is its overdue amount, charge and interest; nothing is
  posted; a run without the settings proposes as before.
- **Depends on:** T76b (in `main`).

### T76e Dunning blocks

**Capability.** A credit controller blocks a customer, or one of its open
items, from dunning in a company with a reason and an end date, and
releases a block early; a dunning run honours the blocks.

- **Reuses:** the collections context; the open item check of dispute
  cases (T86a); the run's list of items left out (`disputed_items`, T86b);
  the confirmation's stale check (T76b); `etag`, audit.
- **Adds:** table `collections.dunning_block`; `POST /v1/dunning-blocks`,
  `GET /v1/dunning-blocks`, `GET /v1/dunning-blocks/{id}`,
  `POST /v1/dunning-blocks/{id}:release`; permission
  `collections.dunning.block`; the run's `blocked_items` with their count
  and amount.
- **Walkthrough:** `step_dunning_blocks`: Ruth blocks one invoice of
  `step_dunning_charges`' customer and a run names it as blocked; a block
  of the whole customer leaves both invoices out until its end date; a
  past end date and a second release are refused; a proposal confirmed
  after a block was placed is stale; the invoice's block released, the
  next run proposes it.
- **Acceptance:** a block is in force for a run while it is not released
  and the key date is on or before its end date; a blocked item is in no
  notice and in none of the run's totals, named in `blocked_items` with
  its block; an item block names an open item the customer owes; the end
  date is not before the company's today; releasing is once; a proposal
  with an item blocked since it was started is refused as stale (409).
- **Depends on:** T76d.

**Later for dunning (still after these):** letters and e-mails (output
management); procedures per customer; charges and minimum amounts per
currency and per level; posting dunning charges and interest to the
customer; blocks shown in the collection worklist; scheduled and large
runs as a workflow; cancelling a confirmed run.
