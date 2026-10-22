# ADR-0045: Parallel ledgers

**Status:** accepted (product owner, 2026-10-19)

## Context
Every journal line already names a ledger (`ledger_id`), and posting
periods are kept per legal entity and ledger (`period_status`), but the
tenant has had one ledger, the leading ledger 0L under IFRS, created with
its first configuration. The pilot's companies report under a local GAAP
beside IFRS (HGB, US GAAP, the tax books), which SAP keeps with the ledger
approach: a leading ledger and non-leading ledgers assigned per company
code, ledger groups (FAGL ledger groups) naming the ledgers a posting goes
to, and a representative ledger deciding the posting period. T52 builds
that; T57 depreciates per area on top of it.

## Decision

**Ledgers are configuration.** The document lists the tenant's ledgers
(`ledgers`: code, name, accounting principle) and each company the ledgers
it keeps (`companies[].ledgers`), stored as `ledger.ledger` and
`ledger.company_ledger`. The leading ledger is 0L and 0L is the leading
ledger; every company keeps exactly one leading ledger, 0L, which the
database holds (a check on the ledger, a unique index and a commit-time
trigger on the assignment). A document without `ledgers` keeps 0L under
IFRS alone, as before. Changes go only through the configuration approval
loop (ledgers and company ledgers are structural, groups posting-relevant,
names cosmetic). What is activated stays: a ledger keeps its accounting
principle and is never removed, and a company keeps its ledgers. A ledger
assigned to a company later starts empty; its opening balances are
ledger-specific postings.

**Ledger groups.** `ledger_groups` (code, name, representative, ledgers)
are stored as `ledger.ledger_group` and `ledger.ledger_group_member`. The
representative is a member, and the leading ledger when it is one. Members
are never taken out and the representative never changes, so a posting's
group keeps meaning what it meant; names change and members are added. A
group's code differs from every ledger's.

**Periods per ledger.** `ledger.Setup` creates the periods of each fiscal
year in every ledger the company keeps, closed, and they are opened,
closed and locked per ledger (T32): a non-leading ledger may stay open for
adjustments after the leading ledger is locked.

**Read API.** `GET /v1/ledgers` (with the legal entities keeping each, of
those the caller may see) and `GET /v1/ledger-groups`, read with
`ledger.account.read`; at the default autonomy for reads.

**Postings (T52b, T52c).** A posting without a group goes to every ledger
of the company as linked entries sharing a `posting_group_id` and one
document number; document number uniqueness gains `ledger_id`. Manual and
parked entries may name a ledger group (`ledger_group`, its code) or a
ledger (`ledger_id`), posting to those ledgers only; every ledger posted
to must be one the company keeps (422 `ledger.ledger_not_kept`). A group
naming ledgers the company does not keep is narrowed to those it keeps, as
SAP does, and refused (422 `ledger.ledger_not_kept`) only when it keeps
none of them (product owner, 2026-10-20, T52c); when the representative is
narrowed away, the first member left by code stands for the posting. A
ledger-specific posting (naming a ledger, even the leading one, or a group)
takes no line on a supplier or customer reconciliation account (422
`ledger.ledger_specific_reconciliation`, product owner, 2026-10-20, T52d),
as SAP refuses it: open items are the same in every ledger, and a ledger's
own difference is posted on G/L accounts. A parked entry is held to it
when it is posted or approved, so one parked before the rule is changed
to post to every ledger, or deleted. The
first entry is the representative ledger's (the leading ledger's without a
group) and is the one returned, audited and referred to by open items and
clearings; each entry publishes its own `JournalEntryPosted` and is
projected. The period must be open in every ledger posted to, each held
FOR SHARE as before, so a parallel ledger closed or locked refuses the
posting (the representative's period is among them). A reversal reverses
every entry of the group, each copy by the reversal's copy in its ledger;
a copy is not reversed by itself. Every posting through the ledger,
including clearings and the contexts' postings, goes to every ledger of
the company. Open items are the leading ledger's (the open item list, the payment
proposal's items, GR/IR and the migration's totals read only its lines,
T52b).

**Clearing in every ledger (T52c).** A clearing is applied to the copies
of its lines in the same transaction, found through the `posting_group_id`:
in each ledger where the clearing document has a copy, a clearing by that
copy clears the copies of the items with the copies of their offsetting
lines; an item without a copy there (posted before T52b, or to the leading
ledger alone) is left with its offset, so a ledger never clears an item
without the line that offsets it. This holds for clearings, settlements
(payments), down payment clearings, reversals (the reversal's copies clear
the original's) and migrated clearings (whose clearing entry must be a
posting group's first entry in the leading ledger, where the open items
are, T52d, and whose date must fall in an open period of every ledger of
its copies). A clearing's own entry (AB, and the down
payment transfer) is narrowed to the ledgers every item was posted in, so a
0L-only item's clearing posts to 0L alone; payables, payments, GR/IR and
settlement postings go to every ledger of the company. The copies'
clearings are part of the clearing: audited and announced (`items_cleared`)
once, by the leading ledger's, and the clearing API shows only that one.
Reports read one ledger, the leading ledger by default: the journal list
takes `ledger_id`, defaulting to the leading ledger, so a posting's copies
show only when their ledger is asked for and readers that sum the list
(analytics) count each posting once. Agents keep proposing only: posting stays human-only and parking,
with a ledger or group, is how they propose.

**Tax items (T61c, ADR-0049 §5).** A posting's tax items are its leading
ledger's entry's alone: returns are statutory and read the leading ledger,
so a copy, with the same tax lines, has none (`tax_item_leading`). A
posting that leaves out the leading ledger may carry no tax code (product
owner, 2026-10-21; 422 `ledger.tax_code_off_leading`, and the commit-time
check `tax_code_off_leading`): tax comes only from postings the returns see.

**Revaluation and depreciation.** Foreign-currency revaluation runs per
ledger (T52f, as built): a run names its ledger (`ledger_id`, the leading
ledger by default, one the company keeps or 422 `ledger.ledger_not_kept`),
values that ledger's own lines (in a parallel ledger the copies of the
open items, cleared with the leading ledger's, and its ledger-specific
postings) and posts its valuation and reset entries in that ledger alone,
which the rule above allows because its lines are on the valuation and
unrealised gain/loss accounts, a reconciliation account's valuation on its
adjustment account (ADR-0042), never on the reconciliation account itself.
Cancelling reverses them in that ledger alone. Everything else is T48's
per ledger: one posted run per legal entity, ledger and key date (409
`ledger.revaluation_exists`), both periods open in the run's ledger, the
tenant's entry buckets taken first, the run read `FOR UPDATE` to cancel,
audit and `RevaluationRunPosted`/`Cancelled` naming the ledger. The
valuation area: a parallel ledger may value at a rate type of its own
(`companies[].fx_valuation.ledgers`, code and rate type;
`ledger.fx_valuation_ledger`, posting-related), else at the legal
entity's, as the leading ledger always does; the valuation accounts are
the legal entity's for every ledger, since each run posts in its own
ledger. A run's ledger is one its legal entity keeps, which the database
holds too (`revaluation_run_ledger_kept`, T52f). Out of scope until a
pilot needs them (coordinator, 2026-10-21: the pilots, Canada and the UAE,
report under IFRS alone): valuation accounts per ledger, and valuation
methods per accounting principle beyond the rate type, such as HGB's
imparity principle valuing only losses. A ledger assigned to a company
later holds no copies of the items posted before, so its runs value what
was posted to it since, its opening balances included. Depreciation runs
per depreciation area, each posting to its ledger (T57).

**Reconciliation (T52e).** `GET /v1/ledger-reconciliation` compares two
ledgers a company keeps per account and transaction currency, over a
period or the year, from the balances the trial balance reads (projected
balances plus the lines not yet projected, so exact at the read), listing
only the accounts that differ in transaction, local or group currency.
Its first page checks each of the two ledgers' trial balance: debits equal
credits in local and group currency. `GET
/v1/ledger-reconciliation/entries` drills down from an account to the
lines behind its difference: the entries of either ledger whose posting
group has no entry in the other (ledger-specific postings, and entries
posted before T52b, which have no posting group). Both are reads with
`ledger.entry.read`, as the balances, at the default autonomy for reads.
No check looks for leading-ledger-only items on reconciliation accounts
posted before the T52d rule: they cannot exist in real data, because the
rollout rule below assigns no company a parallel ledger until every
replica runs T52b, which ships with the T52d rule; before a parallel
ledger is assigned, every posting is the leading ledger's alone
(coordinator, 2026-10-20).

**Analytics and agents.** Analytics read the leading ledger by default
(product owner): they read the journal list and open items, both the
leading ledger's unless asked (T52c). Agents read every ledger; ledger-specific and manual
postings by agents are propose-only.

**Migrations are expand-only.** New tables and nullable columns; unique
indexes built concurrently per partition and attached (the runner's
`migrate:partitioned-index`, T52b). T52e's contract step is empty: the
old document number constraint is dropped in T52b (000136), not in T52e as
first planned, and nothing else of the expand is contracted
(`posting_group_id` stays nullable: entries posted before T52b have
none). The constraint went early because it would refuse every copy, and the previous release never relies on it (numbers come from
the number range), so dropping it is safe during a rolling deploy. The copies are not: a release before
T52b lists a copy's supplier lines as open items and reverses a copy
alone, so no company is assigned a parallel ledger until every replica
runs T52b. The journal
list's ledger filter needs no index: a page follows the page indexes
(000055) and reads at most its entries' copies besides (the filter is
written so the planner does not underestimate it, T52c). Nothing
is backfilled: a company is assigned its ledgers by the next activation, so
until then `GET /v1/ledgers` lists 0L without legal entities. Until T52b a
posting may still name any ledger of the tenant; T52b limits it to the
company's ledgers.

## Consequences
- Every company's postings are written once per ledger it keeps, which
  multiplies journal volume by the number of ledgers; tenants with one
  ledger see no change.
- Open items exist once (leading ledger), so the subledgers never double;
  the parallel copies carry the open item's amounts for the balance only.
- A ledger, a company's ledger and a group's members cannot be undone by
  configuration; a mistake is corrected by adding, not removing.
- Ledger master data is read with the account permission rather than a new
  one; a dedicated permission can be split off later without a break.
