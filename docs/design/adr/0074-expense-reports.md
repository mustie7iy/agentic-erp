# ADR-0074: Expense reports

**Status:** accepted (T87a, 2026-11-13; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T87a's last commit). It decides
how an employee's expenses are entered and posted in the first version
(`docs/design/plan-breadth-week3.md` §T87a, Lane A) and records what was
built under "As built". The defaults taken and the questions left are
listed at the end for the product owner's digest.

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first). The
tracker's leaf Expense postings is detailed by the plan as T87a: an
accountant enters an employee's expense report and posts it; the
employee's open item is paid by the payment run.

What the design already settles:

- An employee is a business partner with the role supplier (the plan's
  boundaries; human capital's employee master is outside the required
  scope, question 6).
- The posting is a supplier invoice's without an order (03 §3.9,
  `internal/payables/post.go`, `tax.go`): Dr expense per line, input tax
  on the code's account, Cr the supplier's reconciliation account with
  the partner, document type KR, `source_context` `payables`, the open
  item kept with its payment terms (T36b) for the payment run (ADR-0038).
- The tax a gross amount holds is the calculator's inclusive split
  (`internal/tax`, ADR-0049 §8), rounded as the company's tax settings
  say.
- ADR-0003 (RLS), ADR-0005 (money), ADR-0007 (outbox), ADR-0008 (changes
  with the caller's version, rows locked), ADR-0011 (audit), ADR-0050 §6
  (a document item's assignment checked as its posting will check it).
- Week 3's rules (product owner, 2026-11-12): events go to the context's
  outbox and none is offered as a webhook event until a consumer asks;
  no supplier or person name in an event (product owner 2026-11-05).

Nothing settles where the expense types live, what a report's dates
mean, or what happens when the tax changes between entry and posting.

## Decision

1. **A document of payables.** `payables.expense_report` and
   `expense_report_line` (03 §3.9). A report is `entered`, then `posted`
   once; it is not changed, cancelled or reversed in the first version.
   Its entry is not reversed through the ledger either (409
   `ledger.entry_owned_elsewhere`, which names the supplier invoice's
   path and says an expense report's entry is not reversed).
2. **Expense types are configuration.** `companies[].expense_types[]`:
   a code (1-20 capital letters, digits or underscores), a name and an
   expense account of the company's chart that reconciles nothing; at
   most 100; none shipped in a pack. A type's name is cosmetic, the rest
   posting-related (tier 3). A line keeps the account its type named
   when the report was entered.
3. **The report's date and each line's tax date.** `report_date` is
   the company's date in its time zone when the report is entered: the
   entry's document date and the baseline of the open item's payment
   term. A line's `expense_date` is when the money was spent, on or
   before the report date, and it is the line's tax date: its tax code is
   read, its rate chosen and its tax calculated on it, and its tax items
   are dated by it, so its tax is reported in that date's period
   (decided by the product owner 2026-11-13, src po:2026-11-13; the
   first build used the report date for every line).
4. **Gross amounts in the company's currency.** A line's amount is what
   was spent, tax included, above zero. Its tax code, optional, is an
   input code of the company on the line's date; the tax it holds is
   calculated when the report is entered and kept on the line. At posting
   it is calculated again, each line at its date; a line whose code now
   gives another tax is
   refused (409 `payables.tax_changed`) and the report is entered again,
   as a customer invoice is (ADR-0063 §5).
5. **The posting.** Each line debits its account with its net amount and
   the tax its code does not let the company recover, on its cost center
   (the account's default when it names none); the recovered tax goes to
   the codes' accounts, one line per code, component and expense date
   (decision 3), as a supplier
   invoice's does; the employee's reconciliation account is credited with
   the gross, naming the employee, kept with the employee's payment term
   in the company. No withholding tax is deducted.
6. **Permissions** `payables.expense.read`, `.write`, `.post`; entering
   and posting are human-only (`x-agent: false`). Each change is audited
   and publishes `ExpenseReportChanged`, naming the employee by id and
   number.
7. **Number range** `expense_report`, per company and fiscal year.

## Consequences

- An employee is paid by the ordinary payment run, from the open item
  the report posts; nothing new in payments.
- A report a line of which has its rate on its date changed after entry
  must be entered again; there is no edit in the first version.
- A report's tax may fall in several tax periods: an expense dated in a
  period whose return is already prepared is that period's late item,
  for its amendment or the next return (ADR-0049 P7), as a supplier
  invoice dated there is.

## As built

- **T87a (2026-11-13).** Migration 000329; `internal/payables`
  (`expenses.go`, `expensepost.go`, `expenses_api.go`), the configuration
  section in `internal/config/expenses.go`; `POST /v1/expense-reports`,
  `GET /v1/expense-reports`, `GET /v1/expense-reports/{id}`,
  `POST /v1/expense-reports/{id}:post`; walkthrough step 97
  (`step_expense_report`). The employee is checked as a supplier invoice's
  supplier is (`payables.supplier_not_usable` naming `employee_id`). The
  representation names the company, employee, account and cost center by
  number or code and name as they are now, and a line's expense type by
  its name in the configuration while it is configured. The Canada and
  UAE packs bring no `expense_report` range: the walkthrough's pilot
  settings add one, as they add the returns orders'.
- **Each line's tax date (product owner 2026-11-13).** The lines' tax is
  calculated per expense date, one calculation per date with that date's
  rates and the company's rounding (`calculateExpenses`), on entry and
  again at posting (`payables.tax_changed` per line). The ledger takes a
  tax item's own date (`ledger.PostingTaxItem.TaxDate`, nil for the
  entry's): it writes the item with the rate in force on it and checks
  the code on each date its items carry (`checkTaxCodes`); the entry's
  own tax date stays the report date. A report spanning a rate change
  posts each side at its own rate, tested with two dated rates
  (`TestExpenseReportLinesTaxedAtTheirOwnDates`).

## Defaults for the product owner's digest

- Each expense's own date is its tax date; the report date is the
  document date and the payment term's baseline (decision 3, decided by
  the product owner 2026-11-13, no longer a default).
- No withholding tax is deducted from an employee (decision 5).
- An expense type posts to an expense account only (decision 2).
- A pack version that brings the `expense_report` range is left to the
  next pack change (the signed-off packs are not changed here).
