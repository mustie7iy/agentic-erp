# ADR-0066: Credit limits

**Status:** accepted (T77a, 2026-11-12; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T77a's last commit). It decides
the credit management context and the slice T77a
(`docs/design/plan-breadth-week1.md`, Lane B), which records what it built
under "As built". The defaults taken and the questions left are listed at
the end for the product owner's digest.

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first). The
tracker's group Finance › Credit management has one placeholder, Credit
limits. `plan-breadth-week1.md` details it as the slice T77a (a credit
limit and its check) and asks T77a to write this ADR.

What the design already settles:

- The customer's open items are the ledger's: lines on a customer
  reconciliation account, each naming its customer
  (ADR-0063 §5, T75b). `ledger.OpenItems.Receivable` reads them for a
  company, "for the aging, dunning, the collection worklist and the credit
  exposure" (ADR-0063 As built), so an item posted by a manual or migrated
  entry counts as an invoice's does.
- A customer is a business partner with the customer role and, per
  company, customer data: a reconciliation account and a payment term
  (03 §3.6, T74a; `partner.Customers`).
- A leaf context reads others through ports and nothing imports it
  (ADR-0051 §2); `collections` (ADR-0065 §1) is the newest, and reads the
  ledger and the partner context by the types their published APIs give.
- ADR-0003 (RLS), ADR-0005 (money in minor units), ADR-0007 (outbox),
  ADR-0008 (changes with the caller's version), ADR-0011 (audit),
  ADR-0053 (slices, headless first). Events name a business partner by id
  and number, never by name (product owner, 2026-11-05).
- The plan's default 8: a credit limit is one amount per customer and
  company in the company's currency, set at once by a person holding the
  permission; exposure is the open items only; no limit means no
  restriction; nothing is blocked.

Nothing settles where a limit is kept, what exactly the exposure adds up
and what the check answers. This ADR decides them at the level the slice
needs.

## Decision

### 1. A leaf context `credit`

A new bounded context `credit` (schema `credit`, package
`internal/credit`) with its own outbox and inbox, registered in
`outbox.Contexts`. It is a leaf (`internal/layout`): no context imports
it. It posts nothing and changes nothing outside its schema. It reads

- the ledger's customer open items (`ledger.OpenItems.Receivable`),
- the partner context's customers in a company (`partner.Customers`) and
  partners' numbers and names (`partner.Payees.Names`) and
- tenancy's legal entities

through ports it declares, which the composition root wires
(`app.Credit`). As `collections` does, it imports those contexts'
published types (`ledger.ReceivableItem`, `partner.Customer`,
`partner.Named`, `tenancy.Entity`) rather than copying them. The
dependency points one way (ledger, partner, tenancy → credit); the layout
test lists the three imports and keeps `credit` among the leaves. It does
not read the configuration: a limit is not a setting of the tenant's
configuration but a fact about one customer, kept as a row (§2). No
foreign key leaves the schema (02 §2.1).

### 2. A credit limit

`credit.credit_limit` holds the most a customer may owe a company: one
amount, zero or more, in the company's currency (`money.Minor`,
ADR-0005). A customer has one limit per company
(`credit_limit_once`); zero is a limit, of nothing on credit. A customer
without a row has no limit, which is different from a limit of zero (§4).

- **Set** (`POST /v1/credit-limits`) by a person holding
  `credit.limit.write`, for a company they may act in and a business
  partner that is a customer with customer data in that company. The
  limit holds at once: there is no approval, no validity period and no
  review date (the plan's "Later").
- **Changed** (`PATCH /v1/credit-limits/{id}`) with the caller's version
  of it (`If-Match`, ADR-0008): the row is read `FOR UPDATE`, the version
  compared, the amount set. Only the amount changes: the customer, the
  company and the currency of a limit stay, which the database enforces
  by granting the application the update of the amount alone.
- **Never deleted**: the contract has no DELETE, and the application
  holds no delete grant. A customer that should get nothing on credit has
  a limit of zero.
- **In the company's currency.** A limit in another currency is refused;
  nothing is converted, so the limit and the exposure are compared in one
  currency without a rate.

### 3. The exposure

A customer's credit exposure in a company is what its open items add up
to: the open items on the company's customer reconciliation accounts in
the leading ledger, in the company's currency as they were booked
(`amount_lc`), credit items netted (a credit memo or a payment on account
reduces it). It is negative when the customer's credits exceed what it
owes. Nothing is revalued, and whether an item is due does not matter:
the customer owes it either way. Open orders, deliveries and billing do
not count (the plan's "Later", with week 2's sales orders).

The exposure is computed when it is asked for and stored nowhere, so it
is never stale and there is nothing to keep in step with the ledger.

### 4. The check

`GET /v1/credit-exposure?legal_entity_id=&customer_id=&amount=` answers,
for one customer in one company: the exposure and the count of its open
items; whether a limit is set (`has_limit`), the limit and what is
available, the limit less the exposure; and whether an amount, zero or
more in the company's currency, is within the limit
(`is_within_limit`): the exposure and the amount together are no more
than the limit. Without an amount, the exposure alone is compared.

A customer without a limit is within it whatever the amount: no limit
means no restriction, and the answer says that none is set
(`has_limit` false, no `limit`, no `available`).

The check changes nothing and blocks nothing (plan default 8): it writes
no row, no audit record and no event, and no other operation asks it.
What a consumer does with the answer is its own. Order credit blocks are
a later leaf.

### 5. Events and audit

Setting and changing a limit each write an audit record
(`credit.credit_limit.create`, `.change`) and publish
`CreditLimitChanged` through the context's outbox in the same
transaction (ADR-0007, ADR-0011), both with the limit after and the
limit before (none when it was set). The event names the company by id
and code and the customer by id and number, never by name. It does not
carry the exposure.

### 6. Permissions and agents

`credit.limit.read` reads limits and the exposure with its check;
`credit.limit.write` sets and changes limits. Every read and write is
held to the companies the caller may act in: another company's limit is
not found (404), naming it in a request is answered as a missing
reference (422), and its exposure is not found (404).

Setting and changing a limit are people's (`x-agent: false`): a limit is
a money rule a person sets (plan default 8), as starting a dunning run
and entering receivables documents are people's (ADR-0065 §7, ADR-0063
§8). The reads, the exposure and its check among them, are agent tools:
the check is what an agent that takes an order asks first.

## Alternatives

- **The limit as part of the customer's company data in `partner`.**
  Rejected: a limit changes on its own, by other people (credit
  management, not master data) and without the partner's approval
  process, and the later slices (group limits, reviews, risk classes)
  belong to credit management (02 §2.2).
- **The limit as configuration.** Rejected: configuration is the
  tenant's settings, changed by governed change requests; a limit is one
  customer's, changed often.
- **The exposure stored and kept current by the ledger's events.**
  Rejected for the first version: a read of one customer's open items is
  cheap (§3, As built), and a stored figure can be stale.
- **A check as a POST** (`/v1/credit-limits:check`). Rejected: it changes
  nothing, the contract allows GET for it, and a GET is cacheable and an
  agent's read.
- **No limit read as a limit of zero.** Rejected by plan default 8: no
  limit means no restriction.

## Consequences

- A company gets credit limits by setting them; nothing is posted, so a
  limit can be set at any time and takes effect on the next check.
- The check is advice. Nothing in the system refuses an order or a
  posting because of it until order credit blocks are built.
- The exposure is as current as the ledger: an invoice posted or a
  payment applied changes the next answer.
- A customer invoiced in a foreign currency is measured at the booked
  amounts, not at today's rate.

## Defaults chosen (for the product owner's digest)

1. One limit per customer and company in the company's currency, set at
   once by a person holding the permission (plan default 8).
2. Exposure is the open items only, as booked, credits netted; it may be
   negative.
3. No limit means no restriction; a limit of zero means nothing on
   credit.
4. The amount checked is added to the exposure; without one, the
   exposure alone is compared with the limit.
5. A limit is never deleted; it is set to another amount.
6. Setting and changing a limit are people's; agents read limits and the
   exposure.
7. `CreditLimitChanged` carries the limit before and after and the
   customer by id and number; it is offered to webhooks, as the
   collections events are.

## Questions

1. **Credit limit scope and exposure** (product owner; plan question 4):
   per company or across the group, and whether open orders count once
   sales orders exist (week 2).
2. **Exposure at booked amounts** (product owner): a foreign-currency
   item counts at the amount booked in the company's currency, not at the
   current rate. Should the exposure be revalued?

## As built

### T77a Credit limit and its check (migration 000279)

- **Context.** `internal/credit` with the ports `LegalEntities`,
  `OpenItems`, `Customers` and `Partners`, wired by `app.Credit`;
  `credit` in `outbox.Contexts`; `internal/layout` allows its three
  imports (ledger, partner, tenancy) and lists it among the leaves.
  Nothing in `internal/ledger` or `internal/partner` changed.
- **Schema.** `credit.credit_limit` (the application may update
  `amount`, `version` and `updated_by` alone and delete nothing), the two
  permissions; the checks are in 03 §3.28 and `db/credit_test.go`.
  `customer_number` is the customer's business partner number, which
  never changes: the limits are listed by it and the event names the
  customer with it.
- **Operations.** `POST /v1/credit-limits` (201 with `Location`; 422
  `request.reference_not_found` naming `legal_entity_id` or
  `customer_id`, `credit.customer_not_set_up`, `credit.currency_mismatch`
  naming `limit.currency`, `credit.invalid_amount` naming `limit.amount`;
  409 `credit.limit_exists`, from the constraint, so two requests racing
  each other set one limit), `PATCH /v1/credit-limits/{id}` (a merge
  patch of `limit`; 412 on another version; 422
  `credit.nothing_to_change` for the amount it has, as a center's texts
  answer), `GET /v1/credit-limits` (by customer number; filters company
  and customer), `GET /v1/credit-limits/{id}`, `GET /v1/credit-exposure`
  (`x-snapshot: true`: the limit and the items are read from one database
  state). Every amount carries its currency; a limit and an exposure name
  the company by code and name and the customer by number and name.
- **Schema names.** The contract's schemas are `CustomerCreditLimit`,
  `…Create`, `…Update`, `…Page` and `CreditExposure`: `CreditLimit` was
  taken by payables' refusal of a credit memo above its invoice.
- **Who may be asked about.** The exposure is read for any business
  partner the tenant holds, with customer data in the company or not: an
  item posted to it by a manual entry is owed all the same (ADR-0063
  §5), and a partner without items has an exposure of zero. A limit is
  set only for a customer with customer data in the company; whether the
  partner is active or blocked for posting does not matter, since a
  credit manager limits a blocked customer too. Decided by default.
- **The amount checked** is a query parameter, a decimal string in the
  company's currency with exactly its digits (400 `request.invalid_amount`
  otherwise), zero or more (422 `credit.invalid_amount`); the answer
  repeats it with its currency. Amounts that add up to more than an
  amount holds (the items' sum, the limit less the exposure, the exposure
  with the amount) are refused, 422 `credit.amount_too_large`, never
  compared wrongly.
- **Event and audit.** `CreditLimitChanged` (`x-webhook: true`;
  `limit_before` null when the limit is set), audit actions
  `credit.credit_limit.create` and `.change` with `limit` and
  `limit_before`.
- **No budget test.** The exposure is not a report read (07 §7.7 item
  7): it reads one customer's open items of one company through the
  ledger's index by partner (`journal_line_partner`) and one limit by
  its key, as `GET /v1/open-items?partner_id=` does, and returns one
  object; it scans no company and lists nothing. Its latency class is the
  default for a GET, not `report`.
- **Walkthrough.** `step_credit_limit` (step 59): Ruth reads Northwind's
  exposure in Maple, which equals its open items added up, with no limit
  set and so within it; she sets a limit 500.00 above the exposure, a
  second limit and one in another currency refused; 400.00 is within the
  limit and 600.00 is not; the open items and the limit are unchanged by
  the checks; she raises the limit with her version of it, a stale
  version refused, and 600.00 is within it; the audit trail has the
  limit's two records.

### T77b Order credit block (migration 000300)

The check that blocked nothing now blocks a sales order
(`docs/design/plan-breadth-week2.md`, T77b). The product owner decided on
2026-11-12 what the plan's default 10 and questions 10 and 11 left open;
§3 and §4 above read with these changes.

- **The exposure has two parts** (product owner, 2026-11-12; it replaces
  "open orders … do not count" in §3 and answers question 1's second
  half). A customer's exposure in a company is its open items, as T77a
  reads them, plus the gross value of its confirmed sales orders not yet
  billed. `GET /v1/credit-exposure` gives both beside their sum:
  `open_items` with `open_item_count`, `open_orders` with
  `open_order_count`, and `exposure`, the two together, from which
  `available` and `is_within_limit` follow.
- **What an order counts.** Of each order of the company that is
  `confirmed` and not blocked for credit, the part not yet posted as a
  customer invoice: per item, its net value less what posted billing
  documents billed of it, plus the output tax on those open amounts.
  What is posted is the customer's open item from then on, so nothing
  counts twice. No billing document is posted before T79b: until then a
  confirmed order counts in full, a created (unposted) billing document
  changing nothing.
- **Gross with gross** (product owner, 2026-11-12; plan question 10). The
  amount is net plus output tax, calculated by `internal/tax` on the
  order's date over the items that carry a tax code, rounded as the
  company's tax settings say: the calculation receivables makes for an
  invoice (ADR-0063 §4), with the order's date standing in for the
  invoice date billing will have. Gross is net plus tax exactly
  (`money.Sum`). A tax code that is no longer an output tax code of the
  company on the order's date refuses the valuation (422
  `sales.unknown_tax_code`, naming the order and the item) rather than
  being read as no tax.
- **In the company's currency.** An order in another currency is
  converted at the average rate valid on the order's date
  (`ledger.Rates.Conversion`, the rate `ExactToCompany` uses, which the
  plan names), rounded once to the company's minor units: what is
  compared is what `GET /v1/credit-exposure` shows. Without a rate the
  valuation is refused (422 `ledger.no_exchange_rate`). The rate is the
  order date's for as long as the order is open; nothing is revalued.
  Decided by default.
- **Who owns what, and how the two contexts ask each other.** The sales
  context owns the orders and their valuation: `sales.Endpoints.OpenOrders`
  is its published read of a customer's open orders in a company (a
  count and a gross amount in the company's currency). Credit declares
  the port `credit.OpenOrders` with plain values, so it imports nothing
  of sales. Sales asks credit whether an order is within the limit
  through a port sales declares, `sales.Credit`, which the composition
  root answers from `credit.Endpoints.Check` (`app.salesCredit`), as
  partner asks cash about planning groups. Neither context imports the
  other; `credit` stays a leaf and `internal/layout`'s import table is
  unchanged. The composition root builds the two together
  (`app.salesAndCredit`). No dependency direction changed.
- **The check at confirmation** (`sales.Endpoints.ConfirmOrder`, in the
  confirming transaction). The order's whole gross value, in the
  company's currency, is checked with the exposure, which does not hold
  the order yet: it is within the limit when exposure and amount together
  are no more than it. A customer without a limit is never blocked. An
  order over the limit is confirmed with `credit_block` `blocked`; one
  within it with `none`. The order is not checked again (plan default
  10).
- **The lock.** `credit.Endpoints.Check` takes the customer's key in
  the company (a transaction advisory lock on tenant, company and
  customer, `credit.hold`) and holds it until the confirming transaction
  ends, whether the customer has a limit or not; it then reads the limit
  row `FOR UPDATE` and, after both are held, the exposure. Two
  confirmations of two orders of one customer therefore run one after
  the other, and the second is checked against an exposure that holds
  the first: they cannot both take the room only one fits. Setting a
  limit takes the same key, so a limit is not set beside a confirmation
  under way that found none (review round 1: the next order would have
  been checked against an exposure without the first); changing a limit
  waits on the row. The order's own row is locked first, the key second,
  the limit's row third, and nothing takes them the other way round.
  Within the check the orders are read before the open items, so an
  invoice posted between the two reads is counted in both, never in
  neither.
- **A blocked order does not count in the exposure** until it is
  released. Decided by default: the product owner counted "confirmed
  orders not yet billed"; a blocked order cannot be delivered, so it
  commits no credit, and there is no way to reject or cancel one in the
  first version, so counting it would block every later order of the
  customer until someone released an order nobody means to deliver. Once
  released it counts, and may take the exposure above the limit: that is
  the credit manager's decision. Listed for the product owner.
- **Every confirmation values the order**, whether the customer has a
  limit or not, since the order counts in its exposure from then on and a
  limit may be set later: a tax code or a rate that is missing refuses
  the confirmation instead of surfacing at a later read. Decided by
  default.
- **The block and its release** are the sales context's (ADR-0067, As
  built T77b): `credit_block` on `sales.sales_order`, the refusal of a
  delivery, `POST /v1/sales-orders/{id}:releaseCreditBlock` with
  `sales.order.credit_release`, and the events. No rule sets the person
  who releases apart from the one who confirmed (product owner,
  2026-11-12; plan question 11). The limit stays per customer and
  company.
- **Tests.** `internal/credit/check_test.go` (the two parts, the check,
  amounts out of range), `internal/sales/credit_test.go` (the block, the
  lock with `apitest.Serialized`, the release, currencies, tax);
  walkthrough `step_credit_block` (step 68), which also raises step 59's
  last limit so that step 63's order of 1824.41 CAD gross is within it.
- **With T79b** (posting a billing document as a customer invoice;
  batch 70): `sales.openOrders` (`internal/sales/credit.go`) subtracts
  the items of billing documents whose `status` is `posted`, which is the
  status T79b writes (`sales.Posted`). `TestOpenOrdersAndBilling` posts
  the document: the open orders fall by its gross, the open items rise by
  the invoice's gross and the exposure stays; an order billed and posted
  in full no longer counts. In the walkthrough step 63's order has left
  the exposure by step 68, its billing document posted and paid in step
  64, and step 67's order, confirmed and not billed, counts at its gross
  value, 2756.25 CAD.
  `TestOpenOrdersAndBilling` (`internal/sales/credit_test.go`) asserts
  today's half, a created document changing nothing, and says where the
  posted half is added: after the post, `open_orders` falls by the
  document's gross, `open_items` rises by the invoice's, and `exposure`
  stays when the invoice's tax is the order date's.
- **What an order that cannot be valued does.** A confirmed order whose
  tax code or rate is later taken away refuses the customer's exposure,
  and with it every confirmation of that customer's orders once it has a
  limit, until the code or the rate is restored: the refusal names the
  order. Nothing is guessed.
- **The read is not bounded yet.** `sales.openOrders` reads every
  confirmed, unblocked order of the customer and leaves out those with
  nothing open; an order never leaves `confirmed`. Once T79b posts
  billing documents, fully billed orders are still read, under the
  customer's key. T79b, or the depth phase, bounds it: a flag or status
  on an order billed in full, or a partial index on orders with
  something open. Not a tracker item (review round 1, a nit).
- **Not built, by the plan's "Later":** checking again at delivery or
  when an order changes, automatic release on payment, rejecting a
  blocked order, a worklist of blocked orders, approval of a release
  above an amount, group limits, risk classes.
