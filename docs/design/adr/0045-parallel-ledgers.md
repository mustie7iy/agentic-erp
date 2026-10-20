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
parked entries may name a ledger group (or a ledger), posting to its
ledgers only, checked against the representative's period. Open items stay
on the leading ledger; a clearing is applied to every copy through the
`posting_group_id`. Reports read one ledger, the leading ledger by default.

**Revaluation and depreciation.** Foreign-currency revaluation runs per
ledger (`fx_valuation` keyed by `ledger_id`, T52d); depreciation runs per
depreciation area, each posting to its ledger (T57).

**Reconciliation (T52e).** `GET /v1/ledger-reconciliation` compares the
ledgers of a company per account and period, with drill-down to the
entries that differ, and the trial balance is checked per ledger.

**Analytics and agents.** Analytics read the leading ledger by default
(product owner). Agents read every ledger; ledger-specific and manual
postings by agents are propose-only.

**Migrations are expand-only.** New tables and nullable columns; unique
indexes built concurrently per partition and attached; the old document
number constraint is dropped by a later contract migration (T52e). Nothing
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
