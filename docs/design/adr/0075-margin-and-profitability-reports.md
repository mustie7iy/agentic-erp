# ADR-0075: Margin and profitability reports

**Status:** accepted (T92b, 2026-11-12; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T92b's last commit). It decides
the two reports of the tracker's group Controlling › Profitability and
margin analysis that `docs/design/plan-breadth-week3.md` details, the
margin report from the general ledger (T92b, built first) and net revenue
by profitability segment (T92a). Each slice records what it built under
"As built". The defaults taken and the questions left are listed at the
end for the product owner's digest.

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first).
`plan-breadth-week3.md` details the group as two slices and asks T92b,
the first built, to write this ADR.

What the design already settles:

- Profit centers are the ledger's master data, and every profit and loss
  line of a company with profit center accounting carries one, entered or
  derived at posting (ADR-0050 §5). The ledger keeps balances by account
  (`ledger.period_balance`) and by assignment
  (`ledger.assignment_balance`, ADR-0050 §7), and its reports add the
  lines of entries the projector has not reached yet, so they are exact at
  the read.
- The controlling context reads the ledger only through the ledger's
  published port `ledger.AssignmentBalances`, in the request's transaction
  under row-level security (ADR-0050 §2, §8; ADR-0003).
- An account has a type (asset, liability, equity, revenue, expense) in
  the configuration's chart, one chart per company, named after the
  company's code (03 §3.5, §3.7).
- No stock is kept and nothing posts a cost of goods sold: cost of sales
  is posted by a person, by journal entry, at period end (planning
  default, product owner 2026-11-12; `plan-breadth-week3.md`,
  Boundaries). The margin report shows what the ledger holds.
- A report read has a budget of 3 s at p95; no segregation-of-duties rule
  is added; no event is offered as a webhook (`plan-breadth-week3.md`,
  What every slice reuses). ADR-0005 (money in minor units), ADR-0053
  (slices, headless first), REVIEW.md's API-consumer classes (an amount
  with its currency, a report with its totals, paging where rows grow).
- The plan's default 8: profitability is net revenue by customer and item
  key from posted billing and credit memos; the margin report takes
  revenue and the named cost of sales accounts from the ledger by profit
  center.

Nothing settles how the cost of sales accounts are named, what exactly
the report adds up, what happens to lines without a profit center, and
who may read it. This ADR decides them at the level the slices need.

## Decision

### 1. Two reports of the controlling context, computed on read

Both reports are the controlling context's (`internal/controlling`) and
store nothing.

- **The margin report** (`GET /v1/margin-report`, T92b) is from the
  general ledger: a company's revenue, cost of sales and margin by profit
  center over a range of periods of a fiscal year. It is the report with
  a cost side.
- **Net revenue by profitability segment**
  (`GET /v1/profitability-report`, T92a) is from the sales documents: per
  customer and item key the billed gross value, discounts, credit memos
  and net revenue. It has no cost side (no cost of goods sold is posted
  per item), and says so.

The two are not reconciled line by line: the first reads what was posted
to revenue accounts, whatever its source, the second what was billed.

### 2. The cost of sales accounts are configuration

Revenue needs no setting: it is the balance of the chart's accounts of
type `revenue`. Which expense accounts are cost of sales is the tenant's
choice, in the configuration document's controlling section:

```yaml
controlling:
  margin_report:
    - chart: "3000"                 # a company's code: one chart per company
      cost_of_sales_accounts:
        - from: "5000"              # one account
        - from: "5100"              # or a range, inclusive
          to: "5199"
```

- A chart is named at most once and is a company of the document. It
  names at least one account or range.
- `from` and `to` are account numbers compared as the cash derivation's
  account ranges are (`config.AccountDerivation.Covers`: all-digit
  numbers padded to ten places); `to` is not before `from`.
- Each account or range covers at least one account of the chart, and
  every account it covers is of type `expense`: a revenue account would
  count on both sides of the margin, and a balance sheet account is no
  cost. Both are validation errors, so a mistyped number does not
  silently report a cost of sales of zero.
- A change of the section is tier 2 (operational): it posts nothing and
  changes what a report says, as a metric's definition does. The rest of
  the controlling section stays tier 3.
- A chart the section does not name has no cost of sales accounts: the
  report shows the revenue, a cost of sales of zero and
  `cost_of_sales_named: false`.

No country pack ships the section: which accounts are cost of sales
depends on the company's own chart.

### 3. What the report adds up

For one legal entity, one fiscal year and a range of fiscal periods
(1 to 16 by default, the special periods included), in the leading
ledger, in the legal entity's local currency:

- **revenue**: credits less debits on the accounts of type `revenue`;
- **cost of sales**: debits less credits on the named accounts;
- **margin**: revenue less cost of sales.

A row per profit center with a revenue or a cost of sales that is not
zero, by the profit center's code, with its id, code and name. The profit
center of a line is the one the profit center report shows it on
(`GET /v1/profit-center-balances`): entered, or derived at posting.

The lines that carry no profit center (a company without profit center
accounting, or lines posted before it was turned on) are **one last row,
`unassigned`**, present when its revenue or cost of sales is not zero. It
is the company's figures less every profit center's, so the rows add up
to the **totals**, the company's revenue, cost of sales and margin, which
every page carries.

The figures are integers of minor units added and subtracted, so they are
exact (ADR-0005). The company's balances (as the trial balance reads
them), the profit centers' (as the profit center report reads them) and
the unassigned row are read in **one statement**
(`ledger.AssignmentBalances.Margin`), so they are of one database state:
a posting committed between two statements cannot make the rows and the
totals differ. A sum beyond a 64-bit amount fails the read; it is never
shown wrapped.

Amounts are in the company's local currency alone. The report in the
group's currency and across companies is later (the plan's "Later").

### 4. The read

`GET /v1/margin-report` takes `legal_entity_id`, `fiscal_year`,
`fiscal_period_from`, `fiscal_period_to`, `page_size` and `page_token`.
The answer carries the selection, the currency, `cost_of_sales_named`,
the numbers of the chart's accounts counted as cost of sales, the totals
and one page of rows. Profit centers are master data that grows, so the
rows are cursor-paged (04 §4.5) by code; the unassigned row is the last
row of the last page, and no page holds more rows than asked for.

A legal entity that does not exist or that the caller may not act in is
answered as missing (422 `request.reference_not_found`), as a plan batch
answers it. A tenant without a leading ledger has no figures.

### 5. Permissions and agents

- `controlling.margin.read` reads the margin report;
  `controlling.profitability.read` the profitability report (T92a).
  Neither is an approval permission, and no role holds either until it is
  granted.
- The margin report's figures are ledger amounts, so the caller also
  needs `ledger.entry.read`, as for the trial balance and the order
  balances (`x-also-permissions`).
- Both are reads and so agent tools, as the other reports are. Nothing in
  this ADR writes, so no event is published and nothing is audited.
- No segregation-of-duties rule is added (product owner, 2026-11-12).

## Alternatives

- **Cost of sales by account type or cost element.** Rejected: a chart
  has many expense accounts that are no cost of sales, and nothing in the
  chart tells them apart; a new account attribute would be a structural
  change of every chart for one report.
- **A financial statement version with margin levels.** That is the
  plan's "Later" (several margin levels and their account groups). One
  list of accounts per chart is the first version, and the section's list
  form leaves room for further keys per chart.
- **The margin in the semantic layer** (a governed metric). Rejected for
  the first version: `revenue` as a governed metric stays the ledger's,
  and the rows adding up to totals with an unassigned row is a report's
  shape, not a metric's. Nothing prevents a metric later.
- **Reading the profit centers' rows and the company's totals in two
  statements.** Rejected: under read committed a posting between them
  would make the unassigned row wrong by its amount.
- **The unassigned row always present.** Rejected: a company with profit
  center accounting has none, and a row of zeros on every report is noise;
  `unassigned` is a flag on the row, so a consumer finds it without
  relying on its position.

## Consequences

- The report is as good as the postings: a period without a posted cost
  of sales shows a margin equal to the revenue. Automatic cost of goods
  sold needs stock valuation, which is outside the required scope.
- The cost of sales accounts are read from the active configuration at
  each read; a change applies to every period from then on, the past
  included, since nothing is stored.
- A read computes every profit center's figures to page them; the budget
  test holds it to 3 s at p95 at the trial balance's volume.

## Defaults chosen (for the product owner's digest)

1. The cost of sales accounts are a list per chart under
   `controlling.margin_report`, each entry one account (`from`) or a
   range (`from`, `to`); a chart is named by its company's code.
2. An account or range that covers no account of the chart, or a
   non-expense account, is a validation error, not a warning.
3. A change of the section is tier 2.
4. The report reads the leading ledger, in the company's local currency,
   every period of the year by default, the special periods included.
5. The unassigned row is shown only when it is not zero; a profit center
   with neither revenue nor cost of sales has no row.
6. Rows are paged by the profit center's code, the totals on every page.
7. An inaccessible legal entity is answered as missing (422), not as an
   empty report: the report's heading needs the company's currency.
8. The margin report needs `ledger.entry.read` beside its own permission.

## Questions

None for the product owner. For the accounting specialist, not blocking:
whether the first version's single margin (revenue less cost of sales)
should be labelled gross margin in the pilot's reports, and which of the
pilot chart's accounts are its cost of sales (the pilot's own
configuration, not shipped).

## As built

### T92b Margin report from the general ledger (migration 000308)

- **Configuration** `controlling.margin_report` (`config.MarginChart`,
  `config.AccountRange`; `internal/config/margin.go`): validated as §2
  says (`validator.marginReport`), tier 2
  (`^controlling\.margin_report`), its charts compared by `chart`
  (`listKeys`). `Config.CostOfSalesAccounts(company)` gives the numbers
  of the company's accounts the section covers, in number order, and
  whether the chart is named.
- **The ledger's port** `ledger.AssignmentBalances.Margin`
  (`internal/ledger/marginbalances.go`): one statement over
  `ledger.period_balance` and `ledger.assignment_balance`, each with the
  lines still queued for projection, the profit centers' rows through the
  profit center report's own query (`profitCenterBalances.rows`), planned
  per call. It returns a page of profit centers after a key, the
  company's figures and the unassigned figures.
- **`GET /v1/margin-report`** (`getMarginReport`,
  `internal/controlling/margin.go`; `x-slo-class: report`): §3 and §4.
  Problems: 400 `request.invalid_parameter` (periods), 400
  `request.invalid_page_size`, `request.invalid_page_token`, 422
  `request.reference_not_found` (`legal_entity_id`), 409
  `config.not_bootstrapped`.
- **Permission** `controlling.margin.read` (migration 000308; not an
  approval permission; `TestMarginReadPermission`). No table.
- **Tests:** `TestMarginReport` (projected and queued entries, a credit
  note, expenses that are no cost of sales, a period range, a profit
  center netting to zero), `TestMarginReportPages`,
  `TestMarginReportWithoutNamedAccounts`, `TestMarginReportRefusals`,
  `TestMarginReportIsExactOrFails`, `TestPlanDatabaseFailures` (every
  statement failing in turn), `TestMarginReportValid`,
  `TestMarginReportRules`, `TestMarginReportTier`.
- **Budget:** `TestMarginReportBudget`: 100 profit centers and the
  unassigned row over 360,000 balances by account and 360,000 by
  assignment (5,000 accounts, three years, two currencies) with 100
  entries still queued, in pages of 50: median 0.60 s, p95 0.74 s at
  scale 1 and median 0.56 s, p95 0.61 s at scale 2 on a development
  machine under load (2026-11-12), against 3 s.
- **Walkthrough** step 68 (`step_margin`): the report without named
  accounts says so; account 5000 named through a governed tier 2 change;
  the revenue equals the trial balance's revenue accounts; a cost of
  sales of CAD 700.00 posted by journal entry on a cost center of PC-MTL
  shows at once in PC-MTL's row; the rows add up to the totals; a reader
  without the permission is refused.
- **Not built** (the plan's "Later"): several margin levels, the report
  by segment and by hierarchy node, comparison with the plan and the
  previous year, the group's currency, drill-down to journal entries,
  export.
