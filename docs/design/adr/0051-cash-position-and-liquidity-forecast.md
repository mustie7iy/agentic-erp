# ADR-0051: Cash position and liquidity forecast

**Status:** accepted (T53, product owner 2026-10-21: P1-P8 as recommended,
see "Decisions taken"). The steps T53a-T53l are in the tracker.

## Context
T53 is "Cash position and liquidity forecast" (07 §7.6): "A daily cash
position and a liquidity forecast from open items and orders; an agent
explains the variance between forecast and actual; the treasury connector
exports position and forecast only (deals are phase 9). Lane S." The
tracker holds four items for it: "Daily cash position", "Liquidity
forecast from open items and orders", "Forecast agent with variance
explanations" (module Cash management and liquidity forecast) and
"Treasury system connector" (module Treasury and risk management). The
pilots are a Canadian and a UAE company. Nothing in the code speaks of
cash, liquidity or a forecast; what a forecast can read exists, spread
over four contexts:

- **Bank balances** (payments, ADR-0047, 000120): each imported statement
  (`payments.bank_statement`) carries its currency, `opening_balance`,
  `closing_balance` and `closing_date`, signed minor units; a statement
  whose lines do not take the opening to the closing balance is refused,
  and one whose opening balance differs from the previous closing balance
  is `gap`. Lines (`bank_statement_line`) carry a signed amount, the
  booking date and the **value date**, the bank transaction code, the
  counterparty, and a status; a posted line names its match, which names
  the payments or the posting rule it cleared. Only booked entries are
  read (no intraday camt.052, no available balance).
- **House banks** (payments, ADR-0038): several per company, each with its
  bank clearing account (outgoing payments credit it), main bank account
  and incoming clearing account (ADR-0047; nothing posts to the incoming
  one yet). **A house bank's currency is
  always the company's local currency** (`payments.HouseBanks.upsert`
  writes `co.Currency`; `HouseBank` in the configuration has no currency),
  so a Canadian company's USD account or a UAE company's USD account
  cannot be a house bank today: its statements would be refused as not of
  the bank's currency. The routing and the statement import already
  compare currencies per bank, so the gap is the configuration and its
  validation.
- **Supplier open items** (ledger): open items on the supplier
  reconciliation accounts with their payment terms kept beside them
  (`ledger.open_item_terms`, T36b: term code, baseline date, net days, up
  to two discount tiers; `ItemTerms.NetDueDate`, `Until`), withholding
  (`open_item_withholding`, T46f), special G/L items for down payments
  (ADR-0037). Due dates are computed, not stored or indexed; the payment
  proposer's `dueDate` (`internal/payments/propose.go`) and the discount
  helpers (`payments/discount.go`) decide when a run takes an item. Items
  are in the entry's transaction currency with local and group amounts
  (ADR-0042). No journal line carries a value date or due date.
- **Supplier invoices and blocks** (payables): invoices that are `parked`
  or `on_hold` are not yet open items; their terms are kept beside them
  (`payables.supplier_invoice_terms`). A payment block exists only per
  supplier and company (`partner.supplier_company.is_payment_blocked`),
  not per invoice. Down payment requests (`payables.down_payment_request`,
  000079) carry an amount, currency and `due_date` and are `open` until
  paid.
- **Payment runs** (payments): runs `proposing | proposed | releasing |
  released | rejected | failed` with a run date, payments per supplier,
  house bank and method, bank files with their transmission status (no
  run or payment status says "sent" or "cleared"). A released payment
  credits its bank's clearing account on the run date; the statement's
  posting clears that line (ADR-0047). The open items of the clearing
  accounts are therefore exactly the **payments in transit**.
- **Purchase orders and requisitions** (procurement): released orders with
  currency, order date, payment term, items with quantity, price, tax code
  and account; received quantities from goods movements, invoiced ones
  from payables' invoice items (and `ledger.gr_ir_position` for GR/IR
  valued orders, T33); no stored "not yet invoiced" value. Requisitions
  have a header `needed_by` date and estimated prices. **Order items have
  no delivery date** (`procurement.purchase_order_item`, 000006), so there
  is no date on which an order's cash out could be expected.
- **Receivables**: none. Sales and receivables are phase 3 (07 §7.6);
  the configuration cannot even create a customer reconciliation account.
  Incoming payments post to the incoming clearing account and stay there
  until accounts receivable clears them (ADR-0047). A forecast built from
  documents alone shows outflows only.
- **Exchange rates** (ledger): rate types `average`, `bank_buying`,
  `bank_selling`, `planning`, loaded through `/v1/exchange-rates`; the
  latest valid rate of a type is looked up as revaluation does (ADR-0042).
  No forward rates.
- **Analytics** (ADR-0044): per-tenant Parquet extracts within fifteen
  minutes (p95), datasets `gl_lines`, `po_lines`, `ap_invoices`,
  `accounts`, `partners`, `open_items`; the semantic layer falls back to
  PostgreSQL. No statement or cash dataset, and the relay does not mark a
  tenant due for payments' events.
- **Calendars**: the configuration's calendars and `Config.Holiday`
  (ADR-0048) tell a company's working days, holidays included.
- **Contexts** import each other only along the allow-list of
  `internal/layout/layout_test.go` (`TestImportRules`); the integration
  context's connector kinds are a fixed list (`bank_sftp`, `bank_ebics`,
  `sap_odata`, ...), with no treasury kind.
- **Tax returns** (T61, ADR-0049) will compute a payable per return with a
  filing due date; not built yet.

### What SAP does (the parity target)
Sources at the end; where I rely on memory it says so. SAP Help pages are
partly rendered by script, and the facts below come from the pages'
indexed text; field and table names marked "from memory" need checking
against the pilot's release.

- **Cash position and liquidity forecast are one view over flows of
  different certainty.** Classic Cash Management (FF7A cash position,
  FF7B liquidity forecast) shows bank account movements in the cash
  position and subledger movements (payables, receivables) in the
  liquidity forecast, by value date, with a grouping that selects what to
  show, a display currency and a delta or cumulative presentation; an item
  planned on a public holiday is moved to the next working day. S/4HANA's
  Cash Management keeps both: the cash position covers bank accounts and
  bank clearing accounts, the medium-term forecast cards cover 90 days,
  and a **balance profile** (a set of planning levels) chooses the sources
  shown.
- **Planning levels** say what kind of transaction a flow comes from (a
  bank posting, a payment advice, a supplier item) and so how reliable it
  is; **planning groups** classify customers and suppliers by behaviour or
  risk (from memory: the supplier's planning group is `LFB1-FDGRV`, and
  each G/L account names its planning level, `SKB1-FDLEV`). In S/4HANA,
  **flow types** carry the lifecycle of a flow and a **certainty level**
  (actual, forecast, the sources such as purchase or sales orders).
- **Liquidity items** say where cash comes from or goes to (source and
  use), with a direction and a hierarchy. They are derived by **query
  sequences** at posting from the offsetting account and other
  characteristics, with a default when none hits; the **Flow Builder**
  traces a bank posting back through clearings to the original
  documents and splits the actual cash flow over their liquidity items
  (as far as I know, proportionally to the offsetting lines). Bank
  statement items not traced get default liquidity items (incoming or
  outgoing cash transfer).
- **One Exposure from Operations** (table `FQM_FLOW`) is the hub for
  forecast flows from operational sources: purchase requisitions, orders
  and scheduling agreements (forecasts revised as invoices arrive), sales
  orders (transaction date from the expected billing date and the payment
  terms), FI-CA, and **memo records**. Memo records are entered by hand
  (Manage Memo Records) with a planning level, an amount, a value date,
  an expiry date (from memory) and either a bank account (bank-related
  levels) or a planning group; they appear in the cash position and the
  forecast until they expire or are archived.
- **Bank Account Management** keeps bank accounts as master data with
  dual control for sensitive fields (the submitter cannot activate their
  own change), overdraft limits, signatories, bank account groups and
  cash pools.
- **Liquidity planning, cash pooling, in-house bank, hedging and exposure
  management** are separate products or components (SAP Analytics Cloud
  liquidity planning, In-House Cash/In-House Banking, Treasury and Risk
  Management with Hedge Management and Exposure Management 2.0). They are
  phase 9 here (§12).

## Decision

### 1. Scope of T53
In: house banks in a foreign currency (P2); a **daily cash position** per
house bank, currency and value date from statement balances, payments in
transit, payment runs and memo records; a **liquidity forecast** over
configurable horizons from supplier open items (with cash discount and
blocks), down payment requests, payment runs, purchase orders and, if
chosen, requisitions, and memo records (manual, recurring, uploaded);
classification by source, certainty and tenant-defined liquidity items;
conversion to a display currency; daily **snapshots** of the forecast and
the **variance** between a snapshot and the actual statement flows; the
analytics datasets; the **forecast agent** explaining variances and
proposing memo records; the **treasury connector** exporting position and
forecast; the SAP migration of liquidity items, planning groups and open
memo records (if the pilot uses them).

Out (and where it goes, §12): receivables and sales orders as sources
(phase 3 adds them as sources of this model); tax payables from returns
(added when T61's returns exist); payroll (phase 8); intraday statements
(camt.052) and available balances; bank calendars of their own; bank account management with signatories and dual control
beyond the configuration's change control; cash pooling, cash
concentration and in-house bank; money market, debt and investment deals;
FX exposure management and hedging; liquidity planning with versions and
plan/actual (phase 9 planning); bank fee analysis; payment factories. No
posting: T53 never moves money or writes the journal.

### 2. Ownership: a new `cash` context (decision P1)
A new context **`cash`** (schema `cash`, package `internal/cash`) owns
liquidity items, memo records, snapshots, variance comments and the
treasury export. It is a **leaf**: it reads through published ports of
the ledger (open items, account balances, rates), payments (statements,
house banks, runs, payments in transit), payables (blocks, down payment
requests) and procurement (open order values); nothing depends on it.

```mermaid
flowchart LR
  ledger --> cash
  payments --> cash
  payables --> cash
  procurement --> cash
  cash -.events.-> analytics
```

Reasons:
- The forecast reads four contexts; put in payments it would make payments
  depend on procurement, a new and backwards direction (procurement →
  payables → payments today). A leaf adds arrows only into itself, so no
  existing direction changes (CLAUDE.md: a new cross-context import needs
  the callee's published interface; this ADR is the record of the new
  arrows).
- Its data has its own lifecycle (memo records expire, snapshots age out)
  and its own users (treasurer, controller), unlike payments' runs.
- Phase 3 (receivables, sales orders), T61 (tax returns) and phase 9
  (deals, cash pools) become new sources of the same context without
  touching payments.

The ports are read-only, take the legal entity and a horizon, and run in
the request's transaction under RLS (ADR-0003), as `ledger.Partners` and
the other ports do. They return flows in the shape of §4, so `cash` never
reads another schema's tables.

### 3. House banks in a foreign currency (T53a, decision P2)
`companies[].house_bank(s)[].currency` (ISO 4217, default the company's
currency) is stored on `payments.house_bank.currency`, which exists and
is today always local. Validation (configuration, tier 3 posting-related):
- a foreign-currency bank's clearing, main and incoming accounts are
  flagged `revalue_foreign_currency` (ADR-0042), so period-end valuation
  covers them, and none of them is shared with a bank of another currency;
- a currency never changes under a house bank's code (a new account is a
  new code, as IBANs are, ADR-0038);
- the ranking (`payment_methods[].ranking[].currency`) may name the bank
  only for its currency; the routing already skips banks of another
  currency.
A run in USD then pays from the USD bank in USD (document currency,
ADR-0042), its statement in USD is accepted, and the posting converts at
the posting date's rate with realised differences, all of which T48 and
T49f built. Without this step a pilot's foreign-currency accounts are
outside the cash position, which is why it comes first (facts at the
end).

As built (T53a): no migration (the column existed); the currency never
changes under a code, checked as the IBAN is; the bank file names the
debtor account's currency (`DbtrAcct/Ccy`) for a foreign-currency bank;
the reconciliation report (T49h) compares the statement with the
ledger's balance of the bank account in the bank's currency (the
transaction amounts of the entries in it), so exchange differences and
valuations are not unexplained. ADR-0038's amendment has the details.

### 4. The flow model: sources, certainty, liquidity items
Every figure is a sum of **flows**: (company, house bank or none,
currency, value date, signed amount in minor units of the currency
(`money.Minor`, ADR-0005), source, certainty, liquidity item, reference to
the document). Flows are computed from their sources at read time (§6);
only memo records and snapshots are stored by `cash`.

**Sources** (SAP's planning levels, fixed by the code because each is a
reader of its own):

| Source | Read from | Certainty | Value date | Bank |
|---|---|---|---|---|
| `bank_balance` | latest statement's closing balance per house bank | actual | statement closing date | yes |
| `in_transit` | released payments whose line on the bank clearing account is still open | confirmed | run date + the bank's value days | yes |
| `payment_run` | payments of runs `proposed` or `releasing` | confirmed | run date + value days | yes |
| `payable` | supplier open items, not special G/L, not in a run | expected | expected payment date (§5) | the supplier's ranked bank if one fits, else none |
| `payable_blocked` | supplier open items of a supplier blocked for payment in the company | expected, shown apart | net due date | none |
| `invoice_unposted` | supplier invoices `parked` or `on_hold`, by their kept terms | planned | as `payable` | none |
| `down_payment` | down payment requests not yet paid (ADR-0037) | expected | the request's due date | none |
| `purchase_order` | released orders' open value, not invoiced (§5) | planned | expected invoice date + payment terms | none |
| `requisition` | released requisitions not ordered (P7) | planned | need date + terms, if chosen | none |
| `memo` | memo records (§7) | as entered: confirmed, expected or planned | as entered | optional |
| `unapplied_receipt` | the incoming clearing accounts' open balance | actual (already in the bank; shown for information, not added) | — | yes |

Phase 3 adds `receivable` and `sales_order`; T61's returns add
`tax_payable`; each is a new reader and a new row, nothing else.

**Certainty** (SAP's flow levels): `actual` (in the bank), `confirmed` (the
money is committed: released or proposed payments, confirmed memos),
`expected` (a posted document owes or is owed), `planned` (an order or a
plan). Reports show certainty as a dimension and can be cut at a level
("confirmed and above"), which is SAP's balance profile in one parameter.

**Liquidity items** (SAP's liquidity items, decision P4) are the tenant's
classification of cash by purpose, in the configuration:
```yaml
cash:
  liquidity_items:
    - {code: OP_SUPPLIERS, name: Supplier payments, direction: outflow, parent: OPERATING}
    - {code: OP_RENT, name: Rent, direction: outflow, parent: OPERATING}
    - {code: TAX, name: Taxes, direction: outflow, parent: OPERATING}
    - {code: CAPEX, name: Capital expenditure, direction: outflow, parent: INVESTING}
    - {code: FIN_LOAN, name: Loan repayment, direction: outflow, parent: FINANCING}
    - {code: REC_CUSTOMERS, name: Customer receipts, direction: inflow, parent: OPERATING}
    - {code: BANK_CHARGES, name: Bank charges and interest, direction: both, parent: OPERATING}
  derivation:                    # first hit wins, in this order
    accounts:                    # by the offsetting G/L account (SAP query sequence, origin C/D)
      - {from: "1500", to: "1599", item: CAPEX}
      - {from: "6100", to: "6100", item: OP_RENT}
    planning_groups:             # by the supplier's planning group (SAP FDGRV)
      - {group: TAX_AUTHORITY, item: TAX}
    posting_rules:               # statement lines posted by a posting rule (ADR-0047)
      - {rule: CHARGES, item: BANK_CHARGES}
    defaults: {outflow: OP_SUPPLIERS, inflow: REC_CUSTOMERS}
```
- A **document flow** (supplier item, order item, payment) takes its item
  from the **offsetting lines** of its document: a supplier invoice's
  expense and asset lines (tax lines follow their base line), an order
  item's account; a document with several offsetting accounts is split
  over their items in proportion to their amounts (`money.Allocate`), as
  SAP's Flow Builder splits a cash flow. Then the supplier's planning
  group, then the default for the direction.
- A **memo record** names its item.
- A **statement line** (an actual) takes the items of what its match
  cleared: the payments' supplier items (each with the split above), the
  posting rule's item, or for a line not posted the default of its
  direction (SAP's default items for untraced bank items).
- The supplier's **planning group** is a new optional field of the
  partner's company data (`planning_group`, a code from
  `cash.planning_groups`), added in T53b.
- Items form one hierarchy (parent codes, a tree, depth at most 4). The
  three top nodes OPERATING, INVESTING and FINANCING are suggested by the
  packs (ADR-0048), not required.
- Tiers: names and hierarchy cosmetic (1); derivation rules tier 2 (they
  change classification, never postings), with a simulation that lists
  the open flows whose item would change. Snapshots keep the item they
  were taken with; a rule change does not restate history (as P7 of
  ADR-0050).

As built (T53b):
- **Context and imports.** `internal/cash` imports only `config` (and the
  platform and `money`); `internal/layout` lists it with `config` and adds
  a rule that no context imports a **leaf** (`cash`), whatever the
  allowlist says. Its ports into the ledger, payments, payables and
  procurement are interfaces it declares and the composition root wires
  (T53c, T53e add them), so the four contexts gain no import either. The
  one question another context asks it, whether a planning group exists,
  goes through a port partner declares (`partner.PlanningGroups`,
  implemented by `cash.PlanningGroups`, wired in `app`), the way partner
  asks payments about payment methods: no context imports `cash`.
- **Stored or read.** Migration 000141 creates the `cash` schema (RLS,
  `tenant_id` first) with `liquidity_item` (code, name, direction, parent
  of the tenant) and `planning_group`, kept from the document by
  `cash.Setup` (inactive when dropped, never deleted). The derivation
  rules and the companies' settings are not copied: position, forecast and
  the snapshot workflow read them from the active document, as payments
  reads `bank_statements`.
- **Document.** `cash` at the top level (`liquidity_items`,
  `planning_groups`, `derivation` with `accounts` in order, then
  `planning_groups`, `posting_rules` naming a company's posting rule by
  its transaction code, and `defaults`) and `companies[].cash`
  (`take_discounts` default true, `payment_days` mon-sun, `value_days`
  per house bank and optional method 0-30, `buckets` of `day` (≤ 92),
  `week` (≤ 104) and `month` (≤ 36) in that order, default 14 days, 11
  weeks and 9 months, `rate_type` default `planning`, `rate_max_age_days`
  1-365 default 7, `order_lead_days` 0-365 default 30, `credit_lines` a
  positive amount in the bank's currency, `snapshot_time` HH:MM default
  07:00). Account ranges compare as SAP's ALPHA conversion keeps numbers
  (all-digit numbers zero-padded to ten). Rules name items without
  children; a default names an item of its direction or of both, and a
  planning group's rule one for outflows or both; missing
  defaults are a warning (a flow no rule assigns then has no item, shown
  unclassified), so a pack can bring a tree alone. The account rules are
  diffed by position, their order being a setting.
- **Derivation.** `cash.Derive` gives each offsetting line the first
  covering account rule's item, else the fallback (planning group, then
  posting rule, then default), and shares the amount out over the items
  in proportion to the lines' signed amounts with `money.Allocate`, so a
  credit line (a discount on its own account) takes its item's share
  down; lines that cancel out give the whole amount to the fallback.
- **Partner.** `planning_group` on supplier company data (migrations
  000142-000143, `SupplierCompany(Input).planning_group` in the contract,
  `partner.Supplier.PlanningGroup` in the port), checked against active
  groups (422 `partner.unknown_planning_group`); a document dropping a
  group a supplier has, or a pending change gives, is refused
  (`partner.PlanningGroupCodes`, 409 `partner.planning_group_in_use`).
- **Packs.** A pack may carry an optional `cash.yaml` of liquidity items,
  merged into the tenant's by code; the Canada pack 2026.2 (2026.1 plus
  that file and IAS 7 as a source) suggests OPERATING, INVESTING and
  FINANCING, each for both directions.
- **For T53e3 (batch 6 review, T61e; built, see As built T53e3 in §9):** a supplier invoice's
  self-assessed tax (T61e's `self_output`/`self_input` pair, reverse charge)
  posts an offsetting +x/−x that moves no cash; if the two accounts derive
  to different liquidity items, the flow would be split into a positive and
  a negative share. The forecast reader nets self-assessment pairs out of
  the offsetting lines before `cash.Derive`, so they classify nothing.
- **Deferred to T53e3:** the derivation simulation (the open flows whose
  item a rule change moves) needs the flow readers T53e builds; until
  T53e3 a rule change was approved on its diff alone, at tier 2. Built in
  T53e3 (§9, As built T53e3).

### 5. Dates and amounts of document flows
**Supplier open items** (decision P5). The expected payment date of an
item is:
1. if a discount tier is still open on the read date and the company takes
   discounts (`cash.take_discounts`, default true, which is what the
   proposer does when a run pays within a tier, T36), the last day of the
   earliest open tier, at the gross amount less the discount;
2. otherwise the net due date, at the gross amount;
3. moved back to the company's last **payment day** on or before that
   date (`cash.payment_days`, weekdays the company runs payments, e.g.
   `[tue, thu]`; none configured: the date itself), not before the read
   date; an overdue item is expected on the next payment day;
4. plus the value days of the bank the supplier's first method ranks for
   the item's currency (`cash.value_days` per house bank and method,
   default 0), and moved to the next working day of the company's
   calendar (`Config.Holiday`, ADR-0048: weekends and holidays).
The rule reuses the proposer's due-date and discount-tier logic, moved
to a shared place (the ledger's `ItemTerms`, beside `NetDueDate` and
`Until`) rather than copied (CLAUDE.md). Withholding at payment (T46) is deducted from the supplier flow; its
remittance to the authority is not forecast until the tax payable source
exists (a memo record covers it meanwhile). Credit memos are negative
flows on the same date rule, so a supplier nets as the run nets it.
Items in a payment run are the run's flows, not the item's (never twice).
Items of a blocked supplier are shown apart on their net due date and are
not in the totals unless asked (`include_blocked`); a per-invoice block
does not exist and is not added by T53.

**Down payment requests**: their due date and amount, until paid.

**Purchase orders** (decision P7): the open value of a released order
item is (ordered quantity − invoiced quantity) × price (the invoiced
quantity read from payables per order item, so `cash` reads both
contexts and procurement gains no dependency), plus the tax its
tax code gives at the read date's rate (T61's engine port once it is
there; until then the code's rate), in the order's currency. Its expected
invoice date is the item's **delivery date** (a new optional
`delivery_date` on order items, SAP `EKET-EINDT`, added in T53g as an
additive contract change, copied from the requisition's need date), else
the order date plus the company's `cash.order_lead_days` (default 30);
the cash date then follows the order's payment terms from that date as
for an item (net, or the discount tier if taken), then payment days and
value days. Received but not invoiced quantities are in GR/IR and still
here (they are not yet open items). Orders closed, cancelled or fully
invoiced add nothing. Requisitions, if chosen, are at their estimated
value, the header's `needed_by` plus terms, certainty `planned`.

**Payment runs and in-transit payments**: each payment's amount in its
currency on its bank, value date the run date plus value days (a run
date in the past with the payment still unmatched: tomorrow, flagged
`late`).

As built (T53g, the purchase orders, decision P7; decided by default where
§4, §5, §9 and §11 leave it open):
- **The delivery date.** `delivery_date` on order items (migration 000205,
  `procurement.purchase_order_item.delivery_date`, nullable; the contract's
  `PurchaseOrderItem.delivery_date`, optional, additive), SAP's EKET-EINDT
  for an order of one schedule line. An item that gives none takes the
  `needed_by` of the requisition its order was created from (on creation
  and on a draft's change of items alike); an order change's item that
  gives none keeps its own date first, one it adds takes the need date;
  a released order's item changes it only through an order change, as its
  quantity and price (`check_order_item_editable` leaves the column free).
  No rule ties it to the order date: nothing settles one, and SAP accepts
  any. Requisitions are no source (P7).
- **One port, the run's rules** (T53e2's precedent). The orders reach the
  forecast through the payments context's `CashPosition.Payables` as a
  fourth kind (`CashOrder`), from a port payments declares (`Orders`)
  that payables implements (`payables.Endpoints.OpenOrders`), wired in
  `internal/app`: payables already reads orders through procurement's
  published port (now with `Orders.Released`) and knows what is invoiced
  on an order item and how an invoice's tax is calculated, so "cash reads
  both contexts" (§5) is payables reading procurement for it; procurement
  gains no dependency and no context an import. Each order becomes, per
  day its items' invoices are expected, the open item invoicing it would
  give (`ledger.PayableItem`), so the supplier's routing, block, due date,
  discount tiers, payment days and value days are the run's for it, as for
  an unposted invoice. Source `purchase_order`, certainty `planned` (§4),
  netted per supplier and currency among themselves (not with posted or
  unposted documents, another certainty), its bank the supplier's ranked
  one as an unposted invoice's (§4's table says none; T53e2 routed the
  unposted invoices, and an order's invoice is paid by the same run); a
  blocked supplier's orders are `payable_blocked` (planned) on their net
  due date, apart, as T53e2 shows everything no run pays.
- **The open value** (§5). Per item: the ordered quantity less what its
  supplier documents posted, parked or on hold invoice, net of credit
  memos, with what SAP invoiced before migration
  (`payables.migrated_invoice_item`; one query for every released order of
  the company, `quantitiesInvoiced`, which posting's `invoicedQuantities`
  now calls for one order), so an order and its parked invoice are not
  counted twice (T53e2's note); credits beyond what is invoiced leave the
  item open as ordered, and invoicing beyond the order leaves nothing; at
  the item's price per price unit (`money.Scale`, half away from zero).
  Received and not invoiced stays in (§5). Released orders only: draft,
  pending, closed and cancelled ones add nothing.
- **Down payments on an order** (review round 1). A down payment request
  names its order (`purchase_order_id`), and the order's invoice is
  cleared against it (ADR-0037): while one is open it is forecast as
  `down_payment`, and once paid the cash is out, so the order's documents
  are reduced by the order's requests `open` or `paid` (not `cleared` nor
  `cancelled`, tax included, in the order's currency), the earliest first
  and none below nothing; a document they cover in full is left out
  (`orderAdvances`). Without this the order would be forecast twice. What
  is left of them after the order's last open document is not carried to
  the order's invoices already posted or parked: those are forecast as a
  run would pay them, in full until the clearing (review round 2).
- **The tax** (§5: "T61's engine port once it is there"; it is, ADR-0049).
  The tax the supplier would charge on the open values, calculated as an
  invoice's is (`codesDocument`, `tax.Calculate`, `chargedSum`: every
  component but the self-assessed ones, which the supplier does not
  charge) on the read date with the company's tax settings, each code
  read once per forecast and calculated on its own (the calculator rounds
  each code apart anyway). A code that cannot be calculated then (not an
  input code of the legal entity, no rate, amounts out of range) adds no
  tax rather than refuse the forecast, as a read's calculated figures leave
  them out (`readCharged`, shared with `readFigures`); the other codes of
  the document keep theirs (review round 1). Nothing new is
  stated about tax: the rates are the configuration's.
- **Dates and amount.** An order's items expected on one day are one
  document (the drill-down's `purchase_order`, the order's number): their
  open values plus their tax, invoiced on the item's delivery date, else
  the order date plus `cash.order_lead_days` (default 30), a past date as
  it is (the invoice is late; an overdue due date moves to the next
  payment day as an item's). The order's payment term counts from that day
  (a term not configured: none, so the supplier's net days, as an unposted
  invoice's); the cash discount of a tier taken is deducted from the
  amount as for an item (§5: "as for an item"). Withholding at payment is
  not deducted: §5 names the open value plus tax, and a supplier's
  withholding applies to the invoice once there is one.
- **Liquidity items** (§4: "an order item's account"). The document's lines
  are its items' accounts and open values with their tax codes, signed as
  an invoice posts them, their numbers from the company's chart as an
  unposted invoice's, so tax follows its base and an order of several
  accounts is split over their items.
- **Drill-down and visibility** (T53e4's rule, not widened). A flow names
  each order (`LiquidityForecastDocument.kind` `purchase_order`, to read
  with `getPurchaseOrder`) only to a caller who may use
  `procurement.order.read`, as `authz.MayUse` decides it.
- **Snapshots and the variance's match key** (T53f, T53f3). A snapshot's
  line within 31 days names the order (000205 widens
  `snapshot_flow_document_kind_check` `NOT VALID`, 000206 validates it).
  Order lines need a match key, since an order is paid through an invoice
  the snapshot did not name: a statement line's piece is also known by the
  purchase order its invoice was entered on, after the payment, the
  journal entry and the invoice (`payments.CashPaid.Order`, from payables'
  `InvoiceOrders` through the same `Orders` port), so an order forecast
  and paid through its invoice is `amount` or `timing`, not `missing` and
  `new`; an invoice the snapshot named itself matches first.
- **Budget** (01 §Performance, rule 15d). `TestLiquidityForecastBudget`
  adds 10,000 orders of five items (8,000 closed, 2,000 released with a
  posted invoice each, two items taxed) to its 100,000 open items. At
  `ERP_BUDGET_SCALE=2` on the development laptop (load 8-16 from parallel
  lanes) the forecast answered in 2.2 s median, 2.9 s p95 (2.4 s and
  2.5 s on the base without orders, load 12-22), the drill-down in 2.3 s
  and 2.7 s (2.2 s and 2.3 s): within p95 3 s unscaled. No index added:
  the released orders are read by legal entity and status from the
  tenant's orders.
- **Not built.** The SAP migration loads no delivery date (EKET), and
  `PurchaseOrderReleased` does not carry it; neither was asked for.
- **Evidence.** `cash.TestLiquidityForecastOrders` (Maple Inc: an item
  taxed at 19% net of posted and parked quantities and a cancelled invoice
  ignored, less the order's open down payment request (a cancelled one
  not), in its discount tier from its delivery date; two items without
  a date by the lead days in one document, one of a code Maple does not
  have, one credited beyond what is invoiced; an order of a term not
  configured by its supplier's net days; an item invoiced beyond its
  order and one invoiced in SAP adding nothing; a blocked vendor's order
  apart; draft and closed orders nothing; the certainty cut; the CAPEX
  row by the item's account; the snapshot's lines naming the orders),
  `TestLiquidityForecastOrdersFailures` (every database failure),
  `TestForecastVarianceOrder` (an order paid 50.00 less through its
  invoice: `amount`), `payables.TestOpenValue`, `TestWithTax`,
  `TestOpenOrderTax` (a code not calculable beside one that is),
  `TestOpenOrderAdvance`, `procurement.TestOrderDeliveryDates`; the
  walkthrough's step 33: Ben orders desks with HST for a delivery date,
  Tom drills the order's flow down to it on the last payment day before
  NT30's due date, and Rita sees the flow without the order.

### 6. Where it is computed, and freshness (decision P3)
**Position and forecast are computed at read time from PostgreSQL**
through the ports of §2, in one `REPEATABLE READ` read-only transaction,
so position, items, runs and orders are of one database state and exact
at the read (as the trial balance is). The ports are bounded per company
and horizon; the open items read gains a functional index on the net due
date (`baseline_date + net_days`) if the performance budget needs it
(T53e measures against 05: a company with 100,000 open items answers
within 2 s p95, a budget to confirm; T53e4 measured it, declared p95 3 s
and left 2 s to the flow table, see As built T53e4 in §9).

**Snapshots** are stored: once a day per company, after the statements of
the day are usually in (`cash.snapshot_time`, local time, default 07:00),
a workflow `TakeCashSnapshot` (id from company and date, so replicas and
retries start it once; ADR-0012's dispatcher pattern, as the statement
fetch) writes the forecast as it is then:
- `cash.snapshot (tenant_id, id, legal_entity_id, snapshot_date, taken_at,
  horizon_end, status)`, unique per company and date, insert-only;
- `cash.snapshot_flow (tenant_id, snapshot_id, line_no, house_bank_id,
  currency, value_date, source, certainty, liquidity_item, amount,
  document_ref)`: **at document level for value dates within 31 days**
  (what explains a variance), aggregated per bucket beyond.
Taking it twice for the same company and date finds the first and writes
nothing (run-twice test, CLAUDE.md). A person may take an extra snapshot
(`POST /v1/cash-snapshots`, e.g. before a board meeting), named and kept
like the daily ones. Retention: 25 months (two years of comparison), then
deleted by the workflow that takes them; a snapshot referenced by a
variance comment is kept with it.

**The analytical store** (ADR-0044) gets the snapshots and the classified
actuals as datasets (T53j): history, accuracy trends, dashboards (T59)
and the agent's ad-hoc questions go there, with its fifteen-minute
freshness and PostgreSQL fallback. The position a treasurer acts on is
never read from the store: a payment released ten minutes ago must be in
it.

As built (T53j1, the actuals in the analytical store; split from T53j,
src `split:T53j`): dataset `cash_actuals` and metric `actual`, read
through `cash.ActualShares` (the semantic layer's database path, as the
caller, in a consistent read) and `cash.ExtractActualShares` (the store's
extract, in its run), which the analytics context declares as ports and
the composition root wires, so `cash` stays a leaf (§2). The
classification is §11's, done once here. Decisions in ADR-0044 (As built
T53j1). The snapshots' dataset and the forecast are T53j2's, the
variance and accuracy metrics T53j3's.

As built (T53j2, the forecast in the analytical store; split from T53j,
src `split:T53j`, product owner 2026-11-04): dataset `cash_snapshots`, one
row per line at document level of each company's snapshot of each date
(the daily one, else the last taken on demand, as the variance chooses),
each with `lag_days`, its value date less the snapshot's date; metrics
`forecast_lag_0`, `forecast_lag_7` and `forecast_lag_30`, each value
date's flows from the snapshot taken that many days before it, blocked
suppliers' flows left out, as §11's lags. Read through
`cash.ForecastShares` and `cash.ExtractForecastShares`, through the same
ports as the actuals. Decisions in ADR-0044 (As built T53j2). The product
owner decided on 2026-11-04 that the forecast is read at one lag from
daily snapshots and accepted that its figures by week or month differ
from `getLiquidityForecastVariance`, which compares the one snapshot taken
the lag before the period's start over the whole period: the metric's
week adds seven days each forecast by its own snapshot. The same day
compared alone gives the same figure. Accuracy (T53j3) is 100 less WAPE,
Σ|actual − forecast| / Σ|forecast| over item × currency × day cells,
rounded as `money.AbsPercentOf` (for one cell, 100 less the variance
report's `variance_percent`), as the product owner decided the same day.

Rejected for now: a stored flow table maintained from events (SAP's
`FQM_FLOW`, decision P3 b). It would make reads cheap at any size, at the
cost of a second copy of every open item and order kept in step by event
handlers, with its own reconciliation; it is the path if the read budget
fails at a larger tenant, and the flow shape of §4 is its schema.

As built (T53f, the first slice: snapshots; split into T53f and
T53f2-T53f4 in the tracker, src `split:T53f`; decided by default where §6,
§11 and §15 leave it open):
- **The split.** T53f is one slice per thing a treasurer can use, in
  §11's order: snapshots (this slice, taken, read and kept); the actuals
  (T53f2, `GET /v1/cash-actuals`, which stand alone on the statements;
  built, see As built T53f2 in §11);
  the variance with lag and attribution (T53f3, which needs both; built,
  see As built T53f3 in §11); and
  variance comments by people (T53f4, which need the variance; built, see
  As built T53f4 in §11; an agent's proposals are T53i's).
- **Tables.** Migration 000198 creates `cash.snapshot` and
  `cash.snapshot_flow` (RLS, `tenant_id` first, insert-only: no version,
  so no update is granted, and no delete; deleted only by retention, the
  lines with their snapshot) and seeds `cash.snapshot.take`. §6's columns, with three
  decisions: `kind` (`daily`, one per company and date, `snapshot_daily`;
  `on_demand`, named, `snapshot_name`) in place of `status`, since a
  snapshot is written whole in one transaction and has no state to track;
  `time_zone`, the zone of `snapshot_date` (the company's read date,
  T72n1); and `config_version`, the configuration whose derivation
  classified the flows, since §4 says snapshots keep the item they were
  taken with and a rule change does not restate them. `document_ref` is
  `document_kind`, `document_id` and `document_number` (the drill-down's
  `LiquidityForecastDocument`, the number as it was).
- **Document level.** A line within 31 days of the snapshot's date
  (`document_level_until`, the date plus 30) is what one forecast flow
  (`flow_no`) pays of one document under one liquidity item: a supplier
  payment's documents (its invoices and credit memos netted, T53e) each
  classified on its own with the flow's direction (`partsShares`, the same
  shares `classify` sums, so the lines of a flow add up to it), a memo
  record's occurrence, a down payment request; a run's payment, or a
  payment in transit, is split by the items it settles, which are not its
  document, so its lines are its shares by item each naming its run. Each
  line keeps the flow's day at the bank, source, certainty, lateness,
  supplier, house bank (a payment's bank, a memo record's, a supplier
  item's ranked bank by its code, none for the rest, as §4's table) and,
  for run and in-transit flows, the payment (`payment_id`), which the
  variance's attribution (T53f3) matches against statement lines. A share
  of nothing is no line.
- **Beyond.** After the 31st day, one line per bucket of the company's
  buckets, currency, source, certainty, item and bank (`bucket_end` set,
  `flow_count` the flows with a share in it), naming no document,
  supplier or payment; a bucket that holds the 31st day is summed from the
  day after it. Flows after the last bucket (the forecast's `beyond`) are
  not kept: no period they could be compared over is in the horizon. The
  opening is not kept either: the variance compares flows, and the
  position of a past day is its statements' (T53f2).
- **What is kept.** The forecast as `getLiquidityForecast` reads it with
  the company's buckets, every certainty, and blocked suppliers' flows by
  their source (`payable_blocked`), so the variance can count them or not
  as the forecast does. The snapshot's totals per currency (`total`,
  without `payable_blocked`, as the forecast's default; `blocked`;
  `line_count`) are summed at the read from its lines.
- **One database state, and no connection waited for while one is
  held** (review round 1). A write cannot run whole at `REPEATABLE READ`:
  every write of a tenant moves its audit chain head, so any write
  committed during the read would fail it; and `x-snapshot` stays on
  `GET`s (04 §4.7). So the forecast is read in a read-only `REPEATABLE
  READ` transaction and the snapshot written in a `READ COMMITTED` one.
  The workflow's activity reads first (`ReadDailySnapshot`, which returns
  what writes it), ends that transaction, then writes in a transaction of
  its own: it never holds a connection while it waits for another, however
  many companies are due in one minute. A request taking a snapshot on
  demand already holds the request's transaction, so its read
  (`uow.Consistent`) takes a connection of a pool of its own
  (`app.Config.Consistent`, two connections, opened by erp-server beside
  the main pool): those reads wait on nothing, so a request holding a
  main connection while it waits there always gets one, and the main
  pool is never waited on twice. Without that pool, snapshots on demand
  fail (500) rather than read inconsistently.
- **The daily snapshot.** A company is due once its local time in its
  zone reaches its `cash.snapshot_time` (default 07:00) and its daily
  snapshot of that date is not taken (`cash.Endpoints.DueSnapshots`). The
  dispatcher asks at most once a minute, of the tenants that manage cash
  (an active liquidity item; 000198 grants `erp_worker` the two columns
  it needs), and starts `TakeCashSnapshot` with the id
  `{tenant}-legal_entity-{legal entity}-cash_snapshot-{date}`, so replicas
  and rounds start it once. Its two activities take the snapshot as the
  system principal `cash-snapshotter`, who acts in every company of the
  tenant, then delete the old ones (the take is read, then written, as
  above). Taken twice it finds the first and
  writes and publishes nothing (`TestTakeSnapshotTwice`); two replicas at
  once leave one (`ON CONFLICT` on `snapshot_daily`,
  `TestCashSnapshotDailyRace`). An activity that runs after the company's
  midnight skips the date, since a past day's forecast cannot be read
  again. A date that does not parse is not retried. Without a workflow
  engine the in-process `Snapshotter` takes them each minute with the
  same activities, as the recurring entries do (ADR-0056).
- **Retention.** The workflow then deletes the company's snapshots dated
  more than 25 months before its date (daily and on demand alike, as §6
  keeps them alike), each audited (`cash.snapshot.delete`, reason
  `retention`); run again it deletes nothing. The application may delete
  no row (`TestDesignRules`), so the deletion is a function of the
  database, `cash.delete_expired_snapshots` (run as the tables' owner,
  still under row-level security), which computes the 25 months itself
  and refuses a day after UTC's tomorrow: no caller reaches a snapshot
  younger than its retention. §6's exception, a snapshot
  a variance comment refers to, is kept with it: migration 000201 (T53f4)
  replaces the function with one that skips such a snapshot, and the
  comment's foreign key refuses deleting it any other way.
- **API.** `POST /v1/cash-snapshots` (`takeCashSnapshot`,
  `cash.snapshot.take`, Idempotency-Key, `x-agent: false`: §13 gives the
  agent `getCashSnapshot` only), `GET /v1/cash-snapshots` (the companies
  the caller may act in, newest first, by legal entity, kind and dates,
  paged), `GET /v1/cash-snapshots/{id}` and
  `GET /v1/cash-snapshots/{id}/flows` (one currency, by item or the
  unclassified, source and value dates, paged by line, with the total and
  count of every page), all read with `cash.forecast.read`. A line's
  supplier, payment and document are shown as the drill-down shows them
  (T53e4: `partner.supplier.read`, `payments.run.read` and each document's
  context's read permission, `authz.MayUse`), else left out, with
  `has_document` saying whether it names one; its house bank by its code.
  A snapshot, its page of lines and the event give the legal entity's
  `company_code` beside its id (review round 1), read once per legal
  entity of a page. The write is audited (`cash.snapshot.take`) and
  published as `CashSnapshotTaken`; its webhook (`x-webhook`) is T53h's.
  The activities are registered by name (`TakeSnapshot`,
  `PruneSnapshots`): `Snapshots.Due`, the dispatcher's question, is no
  activity, and `Recurrences` registers a `Due` already.
- **Budget.** §6 names none for snapshots: the read is the forecast's
  (01 §Performance, p95 3 s at 100,000 open items), and the lines are
  written in one statement.
- **Evidence.** `db.TestCashSnapshotRules`, `TestCashSnapshotPrivileges`, `TestCashSnapshotRetentionRule`;
  `cash.TestCashSnapshotOnDemand` (a run's payment of two items, a credit
  memo netted into an invoice's payment kept as two lines of one flow,
  memo records, a down payment request, two supplier items summed in
  their weeks after the 31st day and a series' occurrences in their
  months), `TestCashSnapshotLines`, `TestCashSnapshotList`,
  `TestCashSnapshotDaily`, `TestCashSnapshotDailyRace`, `TestBucketSums`,
  `TestCashSnapshotRefusals`, `TestCashSnapshotRetention`,
  `TestCashSnapshotFailures`; `uow.TestConsistentRead`;
  `workflows.TestTakeCashSnapshot`, `TestTakeSnapshotTwice`,
  `TestDispatchSnapshots`, `TestSnapshotter`; the walkthrough's step 26.

### 7. Memo records
`cash.memo_record (tenant_id, id, legal_entity_id, house_bank_id NULL,
currency, amount, value_date, expires_on, certainty, liquidity_item,
description, reference, recurrence, status, origin, session_id NULL,
batch_key NULL, version, created_by, ...)`:
- **Kinds**: a one-off expected receipt or payment (a customer's receipt
  before receivables exist, a loan instalment, a tax payment, payroll), on
  a house bank (it then counts in the cash position) or not (forecast
  only).
- **Recurrence** (`monthly` on a day, `weekly` on a weekday, `quarterly`,
  with an end date or a count) is expanded at read time over the
  horizon; an occurrence can be overridden or skipped by a child record
  for that date (`recurrence_of`, `occurrence_date`).
- **Expiry**: a record stops counting the day after `expires_on` (default
  its value date + 7 days, SAP's expiry), so a forgotten receipt does
  not stay forecast forever; expired records are listed for a person to
  confirm, move or cancel. **Actualisation**: a person (or the agent's
  proposal, accepted) marks a record `realised` by a statement line,
  which makes it stop counting at once and feeds variance.
- **Upload**: CSV or XLSX through T63's import or the API, idempotent by
  `batch_key` (a batch replaces the records of its key that are not
  realised), so the pilot can load expected receipts from the system that
  issues its invoices until phase 3.
- Statuses: `active`, `proposed` (an agent's, not counted until a person
  accepts it), `realised`, `expired`, `cancelled`. Changes are versions
  (ETag, If-Match, ADR-0008), audited; no approval (they post nothing), a
  decision the product owner may revisit for large amounts (facts).
- Permissions: `cash.memo.write` (people, integrations), `cash.memo.propose`
  (agents, `x-autonomy: propose`), `cash.memo.read`.

As built (T53d):
- **Table.** Migration 000152 creates
  `cash.memo_record` with the columns above, and `cash.outbox`/`inbox`
  (`cash` joins `outbox.Contexts`, a NATS stream and the event contract's
  `cash` channel). `expired` is never stored: an `active` record whose
  `counts_until` (its last occurrence's expiry; a one-off record's
  `expires_on`; null for a series without end) is before the read date is
  shown and listed `expired`, and counts nowhere until a person moves,
  extends or cancels it. `rejected` is the status of a proposal a person
  turned down. The database checks every rule of the table
  (`TestMemoRecordRules`), and a trigger (`cash.check_memo_item`) that the
  item is active and of the amount's direction when the item or amount is
  written; the item's row is locked, so a configuration deactivating it
  waits; the trigger fires too when a cancelled record becomes active again
  (an upload's row again). `cash.Setup` refuses a document that takes away
  an item an open (proposed or active) record names, or turns it against
  such a record's direction (finding, 409 `cash.liquidity_item_in_use`). No foreign key crosses a schema: the legal
  entity, house bank, statement lines and session are checked through
  ports (`Payments.HouseBanks`, `Payments.StatementLine` adapted from
  `payments.CashPosition`, `agents.Sessions`), and a record's house bank is
  an active one of its company in the record's currency.
- **The house bank's code (review:batch24).** A memo record gives
  `house_bank_code` beside `house_bank_id`, present exactly when it is,
  read through the port's `HouseBankCodes` (by id, active or not) once per
  page or single read (ADR-0038 As built, "every resource that names a
  house bank gives its code").
- **Series.** `recurrence` is `weekly`, `monthly` on the value date's day
  (the month's last day when shorter: 31 January, 28 February, 31 March) or
  `quarterly`, until a day or for 2-520 occurrences, or neither (expanded
  at most 1,040 occurrences). Each occurrence expires as many days after
  its own date as the first after the value date. An override is a one-off
  record naming the series and occurrence (`POST /v1/memo-records` with
  `recurrence_of`, `occurrence_date`), once per occurrence; `:cancel` or
  `:realise` with `occurrence_date` closes one occurrence by such an
  override (201), and a series is realised only that way. While a series
  has overrides its value date, rule and currency stay (409
  `cash.series_has_overrides`), so an override's occurrence stays one and
  counts once; an override keeps its series' currency. Cancelling a whole
  series leaves its overrides, records of their own, as they are.
- **Operations.** `listMemoRecords` (filters legal entity, status as of the
  read date, house bank, batch key, value dates; sorted by `value_date` or
  `created_at`), `createMemoRecord`, `getMemoRecord`, `updateMemoRecord`
  (merge patch on a proposed or active record, checked as a new one; a new
  value date keeps the days to the expiry), `cancelMemoRecord`,
  `realiseMemoRecord` (by a statement line of the record's company,
  currency, bank if any, and direction, or by the person),
  `proposeMemoRecord` (`POST /v1/memo-record-proposals`, `x-autonomy:
  propose`, in the agent's running session as T49g's proposals, with its
  reason and up to 50 statement lines of the company as evidence),
  `acceptMemoRecordProposal` (checked again) and `rejectMemoRecordProposal`
  (with a reason), and `uploadMemoRecords` (`POST /v1/memo-record-uploads`).
  Every write but the proposal is `x-agent: false`: an agent never creates,
  changes, accepts or closes a memo record; an uploaded record's reference
  does not change. Writes take If-Match on a
  record (ADR-0008, rows read `FOR UPDATE`) and an Idempotency-Key, are
  audited (`cash.memo_record.<verb>`) and publish `MemoRecordChanged`
  (`created`, `proposed`, `updated`, `accepted`, `rejected`, `realised`,
  `cancelled`). A legal entity the caller cannot act in is 404 (fail
  closed). The contract's collection custom methods start operations
  (202), so the proposal and the upload are collections of their own
  rather than `:propose`/`:upload`.
- **Upload.** A UTF-8 CSV of at most 1 MB and 5,000 rows with a header row
  (`reference`, `value_date`, `amount` signed with the currency's digits,
  `currency`, `liquidity_item`; optional `certainty`, `house_bank` by code,
  `expires_on`, `description`), by legal entity and `batch_key`, under the
  key's lock. Every row is checked before anything is written; one wrong
  row refuses the batch (422 `cash.memo_upload_invalid`, each wrong field
  as `rows[N].column`, N the file's line, at most 100). A reference new to
  the key is created, one whose record differs is changed (a cancelled one
  made active again), one the same is left, a realised one kept, and an
  active record of the key whose reference the file no longer has is
  cancelled, so the same file again writes and publishes nothing. XLSX and
  T63's import channel are T63's.
- **Position.** `GET /v1/cash-position` adds source `memo` at each record's
  certainty: the active records on the company's banks not expired at the
  read, each occurrence up to the last day once (an occurrence an override
  replaces left to the override, whatever its status), on its value date
  moved to the next working day; one dated before the read date is `late`,
  on the working day after it, as a late payment is. Records without a
  bank are the forecast's (T53e).
- **Approval.** None, as above: a memo record posts nothing. Whether large
  memo records should need approval stays the product owner's question
  (facts); the conservative default the ADR states is none.

### 8. Cash position (the daily view)
`GET /v1/cash-position?company=…&from=…&days=…` (`cash.position.read`;
days ≤ 31, default 14):
- per house bank (its currency) and day: the **opening balance** (the
  latest statement's closing balance before the day, with its date and
  whether the bank has a `gap`), the flows of the day by source and
  certainty (in transit, runs, bank memo records, and if asked
  `include_forecast` the `payable`/`purchase_order` flows assigned to the
  bank), and the **closing balance**; the first day's opening is flagged
  stale if the latest statement is older than one working day;
- per currency: the sum of its banks;
- a **book balance** check per bank: the main bank account's G/L balance
  plus the clearing accounts' open items against the statement balance,
  showing what is posted and not yet on the statement and the reverse
  (the reconciliation report of ADR-0047 is the detail);
- optional **credit lines** (`cash.credit_lines` per house bank, an
  amount in the bank's currency) to show available liquidity; no limits
  are enforced (Bank Account Management's overdraft limits are
  information here too).

As built (T53c):
- **Request.** `GET /v1/cash-position?legal_entity_id=…&from=…&days=…&display_currency=…`
  (`cash.position.read`, migration 000149; `x-slo-class: report`; agents
  read it as people do, banks by code and last four characters). `from`
  defaults to the read date, the company's date in its time zone (T72n1,
  below; the clock's date in UTC when T53c was built), and is never before
  it (422): past days are actuals,
  whose balances come from the statements of those days (T53f), not from
  what is open now, `days` 1-31 default 14, the display currency the
  company's. A legal entity the caller cannot act in is 404 (fail closed).
  No paging: a company has a handful of banks and at most 31 days.
- **One snapshot.** The contract marks the operation `x-snapshot: true`;
  the unit of work runs such a `GET` at REPEATABLE READ (read-only), so the
  payments, the ledger and the configuration are read as of one state
  (`uow.Middleware.Snapshot`, `api.Operation.Snapshot`, 04 §4.7). A
  separate transaction was rejected: it would hold a second connection
  per request.
- **Ports.** `cash` declares `Payments` (house banks with their opening
  statement, posted payments by id, payments of runs not posted),
  `Ledger` (open lines on accounts, a rate of a type), `LegalEntities` and
  `Settings`; the composition root adapts `payments.CashPosition`,
  `ledger.Accounts.OpenLines`, `ledger.Rates.OfType` and
  `tenancy.Directory` to them (`internal/app/cash.go`), so `cash` still
  imports only `config` (and `api/gen`, the platform, `money`) and the
  layout allow-list is unchanged.
- **Opening.** The latest statement closing before the first day, its
  `gap` (a statement of the bank up to it in `gap`), and `stale` when it
  closes before the working day before the read date, or the bank has
  none (opening zero). Working days are the
  company's calendar: weekends and its holidays (ADR-0048). The flows
  dated before the first day are added to it.
- **In transit** (`in_transit`, `confirmed`): the leading ledger's open
  lines (neither cleared nor reversed) on each bank's clearing account
  whose entry was posted by payments for a payment
  (`source_context = payments`, the payment as source document), at the
  line's amount in its currency, which must be the bank's. A
  revaluation's valuation and reset lines there name the ledger as source
  and never count (ADR-0038, T53a); parallel ledgers' copies are never
  read (ADR-0045). A payment a statement line up to the opening statement
  is matched to (`statement_match.status = matched`, its posting perhaps
  waiting for approval) is already in the opening balance and left out.
- **Runs** (`payment_run`, `confirmed`): the unposted payments of runs
  `proposed` or `releasing`, paid by the payment's bank or the run's.
- **Dates.** A payment's day at the bank is its run date plus the bank's
  `cash.value_days` for its method, moved to the next working day; one
  before the read date is `late`, on the working day after the read date.
- **Book balance check.** The reconciliation report's figures on the
  opening statement's date (`payments.Figures`: the main bank account's
  balance through `ledger.Accounts.Balances` in the bank's currency, the
  statement lines not posted, excluded or posted later, and what is left
  unexplained), beside the payments in transit: booked and not yet at the
  bank. Absent without a statement.
- **Credit lines** are parsed in the bank's currency and added to each
  day's closing balance as `available`.
- **Conversion.** Per currency first (the banks summed), then each
  currency's days in the display currency at one rate: the latest of the
  company's `cash.rate_type` valid on the read date, or, where that type is
  `planning` and there is no planning rate, the latest `average` rate
  (the rate type used is shown); `stale` beyond `cash.rate_max_age_days`;
  a currency with no rate is listed in `incomplete_currencies` and left
  out. A sum out of the range of an amount refuses the position (422
  `cash.amount_too_large`); an unknown display currency is 422
  `cash.unknown_currency`.
- **Budget.** `TestCashPositionBudget`: p95 2 s (§6's number) for 100,000
  payments posted to two ledgers, 2,000 of them in transit, 1,000 in
  proposed runs and a statement a day, over 31 days; measured about
  0.5 s median, 0.9 s p95 on a laptop. The open-line read scans the
  clearing accounts' history (index `journal_line_account`); a partial
  index of open lines, or the flow table of §6, is the path if it grows.
- **Not yet.** With `include_forecast`, the forecast's flows assigned to a
  bank (T53e) join the days as a further source (memo records on a bank
  did in T53d, §7); drill-down to the payments of a day is through payments' own
  API (`GET /v1/payment-runs`).

As built (T72n1, the company's time zone; Pilot readiness "Company time
zone", split into T72n1-T72n6):
- **Where the zone lives.** `companies[].time_zone` in the configuration
  document, an IANA zone name (`America/Toronto`, `Asia/Dubai`), beside the
  company's `country`, `currency` and `fiscal_year_start_month` and the
  calendar of its working days (ADR-0048), which are configuration too: the
  existing behaviour settles it, so no new ADR. Not the legal entity row:
  the business-date rules (working days, value days) are read from the
  document already, and a change to the zone is approved and versioned as
  they are. Not derived from `country`: Canada spans six zones.
- **Validation.** A name `time.LoadLocation` loads; the zone database is
  built into the binary (`time/tzdata`), so the standard names load on any
  host, an image without zoneinfo included. LoadLocation tries the host's
  zoneinfo first, so the names only some hosts accept are refused: `Local`
  (the reading host's zone) and any name not in IANA form (every part
  starting with a capital), so `localtime`, `posixrules`, the `posix/` and
  `right/` trees and a lower-case name a case-insensitive filesystem would
  open; offsets such as `-05:00` do not load
  (`companies[<code>].time_zone`). Empty is UTC, as every date was before:
  documents activated earlier keep their meaning (tolerant read), and the
  response says which zone was used. Since 2026-10-31 a new or changed
  document must set it (see "the zone required" below). A change is tier 3
  (posting-relevant): it moves which day is today for the company by up
  to a day, and the later slices date checks by it.
- **The read date.** The cash position's read date is the clock's instant
  in the company's zone, as a date (`clock.DayIn`): at 22:00 on 17 April
  in Toronto (02:00 UTC on the 18th) and at 01:00 on 17 April in Dubai
  (21:00 UTC on the 16th) it is the 17th. Everything judged by the read
  date follows it: the default and earliest `from`, `stale` (the working
  day before the read date), `late` payments and memo records, and the
  conversion rate's validity and age. The response gives `time_zone`
  beside `read_date` (UTC when the company has none), so a consumer knows
  which day "today" was. A stored zone the binary cannot load (only a
  zone database that lost it) fails the read rather than falling back to
  UTC.
- **Memo records' status (T72n2).** A memo record's `expired` is judged
  on its legal entity's date at the read in its company's zone, the
  date the position reads that company on (one function,
  `companyDay`), so a record the position counts at 22:00 in Toronto is
  listed `active`, not `expired` by the next UTC day. Every response
  carrying a record (create, read, change, the closing and proposal
  decisions, the list) gives `read_date` and `time_zone` beside `status`,
  so a consumer knows which day judged it. The list judges each record by
  its own legal entity's date in the `status` filter and in the items
  alike, from one instant: the dates of the companies with a zone are
  read once per list (their legal entities found by company code through
  the tenancy port, `LegalEntities.ByCode`; a tenant without zones reads
  none, and a list filtered by legal entity reads only its), and every
  other legal entity is judged by UTC's date, as before. The upload's
  response gives counts, not statuses, and an upload judges nothing by
  date (it compares values, and realised and cancelled are stored), so
  nothing in it changes. A stored zone that does not load fails the read
  and the list, as it fails the position.
- **Kept in UTC** (product owner 2026-10-31, as recommended): the
  suppliers a migration run creates are dated by UTC's day. The date is
  their payments bank account's `valid_from`, which partner refuses
  before its own UTC day (the backdating control,
  `partner.backdated_bank_account`) and payments read to choose the
  account a payment goes to; a supplier serves companies in several
  zones, so a bank account's days are judged by one rule for the whole
  supplier, UTC's, not by any one company's zone. No code changes: the
  run passed `database.Today` as before (amended by the product owner
  on 2026-11-02: the migration's bank accounts start the day before;
  see "the migration's bank accounts on a cutover evening" below). Every other
  document is dated by its caller, or by the company's date where the
  caller leaves the date out (the posting dates and the tax activation
  day, below). The payment release's discount check (T72n3), center
  changes' effective day (T72n4, ADR-0050 §11) and purchasing contracts'
  expiry and its notice (T72n6) are built: see below.

As built (T72n5, a requisition's date; product owner 2026-10-31: a
requisition is routed at the rate of the day it was raised in its
company's time zone):
- **The date.** `procurement.requisition.requisition_date` (migration
  000170) is the company's date when the requisition is created: the
  clock's instant in the zone of the legal entity's company
  (`config.CompanyLocation`, `clock.DayIn`; UTC when the company has
  none), so at 22:00 on 31 December in Toronto (03:00 UTC on 1 January) it
  is the 31st and at 01:00 on 17 April in Dubai (21:00 UTC on the 16th) it
  is the 17th. Stored, not derived from `created_at` on each submission: a
  later change of the zone (tier 3) would otherwise move the day a
  requisition already raised, and resubmitted, is routed at. The response
  gives it (`requisition_date`, read-only), so a consumer sees which day's
  rate routed it. A stored zone the binary cannot load fails the creation
  rather than dating it in UTC.
- **What follows it.** Submission converts the amount into the company's
  currency at the average rate of that day (03-data-model, F12, F19), and the
  requisition's number is drawn from that day's fiscal year's range: the
  same document date, so a Toronto requisition of 31 December is numbered
  in the year it is dated in.
- **Earlier requisitions.** 000170 dates the rows raised before it as they
  were routed, `created_at`'s date in UTC, without touching their version
  or `updated_at`; the previous release's inserts in a mixed cell take the
  column's default, its own UTC rule; the not-null rule is validated in
  000171.
- **Not this rule.** Procurement's other dates are different decisions,
  each read on the company's date by its own step: a goods movement
  reversal's default posting date since 2026-10-31 (below), and a
  contract's number since T72n6's step (see below). The account
  assignment check at a
  requisition's submission judges on the submission day, not the
  requisition's, in the company's zone since T72n4.
  Contracts' expiry and its notice are judged on the company's date
  since T72n6.

As built (T72n3, a payment run's cash discount judged on the company's
date; product owner 2026-10-31):
- **The rule.** The release of a proposed run refuses a payment whose cash
  discount's last day (`discount_until`) is before the paying company's
  date at the release, in its `time_zone` (UTC when it sets none): 409
  `payments.discount_lapsed`. At 22:00 on the last day in Toronto (02:00
  UTC the next day) the discount is still earned; at 01:00 the next day in
  Dubai (21:00 UTC on the last day) it has lapsed. ADR-0038 records it
  with the release.
- **Every place a lapse is judged.** Only the release reads a today for a
  discount. The proposal judges each item's tier, and whether it would
  lose its discount by the due-by date, on the run's `run_date`, the date
  the payments post and are executed on, which the proposer chooses: no
  clock and no zone are involved, so nothing there changes.
- **The shared helper.** `config.(*Config).CompanyLocation(code)` is a
  company's zone by its code, UTC when the document has no such company
  or the company sets none; payments' `companyDay` takes it with
  `clock.DayIn`, as procurement's `today` (T72n5, T72n4), the ledger's
  center checks (T72n4) and, since batch 26's integration, the cash
  position's and memo records' `companyDay` (T72n1, T72n2) do.
- **What the consumer sees.** The problem's `detail` names the discount's
  last day, the company's date that judged it and the zone (`… earned
  until 2026-03-15; it is 2026-03-16 in Asia/Dubai, the time zone the
  paying company's date is judged in …`; UTC when the company sets none);
  the problem schema has no extension members, so the date
  and zone are in the detail, as other payments problems give their
  facts.
- **Determinism.** The check runs in the `:release` request
  (`StartRelease`), before the operation is queued; the release workflow
  and its activities do not judge the discount, so no workflow code reads
  a clock.

As built (T72n6, purchasing contracts' expiry and its notice on the
company's date; Pilot readiness "Company time zone"):
- **The rule.** The contract expirer judges each active contract on its
  legal entity's company's date at the round, in the company's
  `time_zone` (`config.CompanyLocation`, `clock.DayIn`; UTC when the
  company sets none or the tenant has no configuration yet): a contract
  expires once that date is after `valid_to`, and is alerted once
  `valid_to` is within `purchasing.contract_expiry_notice_days` of it,
  `days_left` counted from it and `expiry_alerted_on` set to it. At 22:00
  on 17 April in Toronto (02:00 UTC on the 18th) a contract whose last
  day is the 17th is still active; at 01:00 on 18 April in Dubai (21:00
  UTC on the 17th) it has expired. The decision is the one the dates
  already made (ADR-0051 as built, T72n1: business dates are the
  company's), so no new ADR; the database's `current_date` is no longer
  read.
- **One round, one instant.** The round's instant comes from the
  expirer's clock (`Now`), not the database: the dates of the legal
  entities holding active contracts are told once per tenant round
  (their company codes through the tenancy port, as the requisition's
  date reads them) and joined to the statements that pick the contracts,
  so the expiry and the alert of one round judge on the same date. The
  cell's list of tenants, read by the worker role that can read neither
  the zones nor the notice, is a superset: a contract ending on UTC's
  date (no zone is more than a day ahead of UTC) or within 366 days of
  it.
- **What the consumer sees.** `ContractExpired` gives `expired_on`, the
  company's date that judged the contract past `valid_to`, and
  `time_zone`; `ContractExpiring` gives `alerted_on` (the contract's
  `expiry_alerted_on`) and `time_zone`, the day `days_left` counts from.
  Both are added as optional properties (a producer that always sends
  them; `eventcompat` refuses newly required ones within a version), and
  the audit entries record the same two. The contract's
  `expiry_alerted_on` is described as the company's date.
- **Failure.** A stored zone the binary cannot load, or a contract's
  legal entity the directory does not know, fails the tenant's round, so
  nothing is expired or alerted on a UTC date; the round is retried at
  the next interval and the failure logged. The other tenants' rounds go
  on: the cell's round collects each tenant's failure and returns them
  all (`errors.Join`), as the audit anchorer and the key rotator do, so
  one tenant's zone cannot stop expiry for the rest.
- **Determinism.** The expirer is a scheduled job, not a workflow, and
  takes its instant from an injected clock, as before.
- **A contract's number** (review:batch26) is drawn from the range of
  the fiscal year the company's date is in at its creation, procurement's
  `today` as for a requisition (T72n5), and so is a renewal's (the same
  draft path): a Toronto contract created at 22:00 on 31 December takes
  that year's range, not the next. The contract stores no document date
  (its validity is the caller's), so the number is what the consumer
  sees of it; nothing in the contract changes. A zone that does not load
  fails the creation, as it fails the requisition's.

As built (the zone required; product owner 2026-10-31, Pilot readiness
"Company time zone"):
- **Strict write, tolerant read** (06 §6.4). `config.Validate` refuses a
  company without `time_zone` (`companies[<code>].time_zone`: "time zone
  is required: the IANA name of the zone the company's business dates
  are in, such as America/Toronto or Asia/Dubai, as the company states
  it"). Validate runs on a change request's submission and a tenant's
  bootstrap (the
  control plane's `createTenant`, `erp-server provision`), and offline in
  `erp config validate` and `erp config generate`; the consumer gets 422
  `config.validation_failed` with one error per company on
  `proposed.companies[<code>].time_zone` (on a bootstrap,
  `configuration.companies[<code>].time_zone`: each field error is under
  the request's own field that carries the document, createTenant's
  `configuration`; review:batch26), the code already declared on
  `submitChangeRequest`, so the contract gains no code. Reading the
  active document (`config.ParseActive`) does not validate: a document
  activated before the rule keeps its meaning, its companies' dates in
  UTC, and the cash position and the release still name `UTC` as the
  zone used. The first change to such a document must set every
  company's zone, since the proposal is validated whole, and is tier 3
  (posting-relevant) like any change to the zone, `""` to `UTC` included;
  no data migration rewrites the stored versions, which are immutable.
  Activation does not validate again, so a change request submitted
  before the rule and approved after it activates without a zone, read
  as UTC like any older document.
- **The configuration agent** (review:batch26). Its prompt says every
  company needs its zone and that a zone the request does not give is
  asked of the person, never guessed from the country, nothing filed
  until they answer. Evidence: `configagent`'s test reads the
  instruction in the prompt the model is sent, and the configuration
  suite's replays still reach level 2 (`TestEvalSuites`); replays script
  the model's calls, so the agent's behaviour on a request without a zone
  is shown only by a live run of `evals/configuration` (ADR-0039), left
  to the operator.
- **The pilots.** Canada in the company's own zone (`America/Toronto` in
  the walkthrough; Canada spans six zones, so none is derived from the
  country), the UAE in `Asia/Dubai`.
- **SAP drafts.** `erp config generate -time-zone CODE=ZONE,…`
  (`sapconfig.Options.TimeZones`) gives each company's zone; the draft
  notes it as given. The extract carries no time zone for a company code
  (its T001 rows hold `BUKRS`, `BUTXT`, `WAERS`, `LAND1`), so a company
  given none is drafted without one, with a note, and the draft does not
  validate until a reviewer sets it. Whether the company code's address
  in SAP carries a time zone that could be extracted reliably is not
  settled here (no primary source checked); it is listed for an SAP
  specialist rather than assumed.
- **Shipped documents.** The test fixture's companies
  (`internal/config/testdata/valid.yaml`) are in `UTC`, as their dates
  were judged before; the evaluation configurations' German and French
  companies are in `Europe/Berlin` and `Europe/Paris`; the walkthrough
  generates its pilot company with `-time-zone 3000=America/Toronto` and
  shows the draft without it refused.

As built (posting dates the caller leaves out, and the tax activation
day, on the company's date; product owner 2026-10-31, "yes, as
recommended"):
- **The rule.** A posting date the caller leaves out is the company's
  date at the request in its `time_zone` (UTC in a document activated
  before 2026-10-31 that sets none), no longer the database's UTC
  `current_date`: a supplier invoice's at `:post` (after the date the
  request gives, then the invoice's own), its reversal's at `:reverse`,
  and a goods movement reversal's (`reverseGoodsReceipt`, posting and
  document date alike). At 22:00 on 31 March in Toronto (02:00 UTC on 1
  April) each is the 31st and posts in March's period; at 01:00 on 1
  April in Dubai (21:00 UTC on 31 March) the 1st, in April's. The date
  decides the period and the rate a foreign-currency amount posts at (the
  tax rates follow the tax date, the invoice date unless set), so it is
  the posting day the people posting see. The tax setup's activation day follows the same rule (ADR-0049 as
  built, the activation day is the company's date).
- **Every place in the API.** A sweep of the API's posting paths for a
  date defaulting to a UTC today (`database.Today`, `current_date`, `clock.Day`) found only
  these three and the tax activation day; a journal entry's reversal
  defaults to the original entry's date, a statement line's posting to
  its booking date, and every other posting (manual and parked entries,
  receipts, payment runs, GR/IR clearings) takes the caller's date, which
  the contract requires. A
  migration run's suppliers are dated in UTC, a supplier's bank account
  days being one UTC rule for the whole supplier (product owner
  2026-10-31, above); a contract's number is on the company's date (T72n6's
  section above); master-data validity defaults (role and
  company access grants, partner changes, `valid_from` today) are not
  posting dates and are not this rule. The web workspace's forms
  pre-filled their dates (a manual entry's, a payment run's, a report's
  key date) from its server's clock, UTC, found in batch 27's review;
  since the step webdates they are the company's date as the API tells
  it (`getLegalEntityToday`, below).
- **The helpers.** Payables' `companyDay` (the invoice's legal entity,
  `config.CompanyLocation`, `clock.DayIn` on `Endpoints.Now`, the API's
  clock) and procurement's `todayOf` (the legal entity's code, then
  `today`, T72n5), which the requisition's submission check (T72n4) now
  shares.
- **What the consumer sees.** The invoice returned by `:post` and the
  goods movement reversal returned carry the `posting_date` used; an
  invoice's reversal names its entry (`reversal_entry_id`), whose
  `posting_date` and period are the reversal's, and its
  `SupplierInvoiceReversed` event carries `reversal_posting_date`. The
  contract's descriptions say which day "today" is.
- **Failure.** A stored zone the binary cannot load fails the post or
  the reversal rather than dating it in UTC. No workflow posts these
  documents without a date, so no workflow code reads a clock.

As built (one company-date helper; review:batch27, Pilot readiness
"Company time zone"; a refactoring, no new decision of the product owner):
- **One helper.** Every company date is told by `internal/config`
  (`companyday.go`): `(*Config).CompanyDay(code, instant)` is a company's
  date and zone by its code (`CompanyLocation`, `clock.DayIn`), and
  `config.EntityDay(ctx, documents, codeOf, legal entity, instant)` a legal
  entity's: its company code through the context's port, then the active
  configuration, then `CompanyDay`; `(*Config).EntityDay` is the same for a
  caller that has read the document already. Six lookups had grown, one per
  context (procurement's `todayOf` and the contract expirer's
  `companyDays`, the ledger's `companyDays.of`, payments' `companyDay`,
  cash's `readDaysOf`, payables' `companyDay`), differing on a legal entity
  the directory does not know. It lives in `config`, whose published API
  every context already imports and which holds `CompanyLocation`: in
  `internal/platform` it would make the platform depend on a context, a
  new dependency direction (ADR-0001). The configuration knows companies
  by code, not legal entities, so each context hands in its port as
  `config.EntityCode`; `tenancy.CompanyCode` adapts a tenancy
  `EntityReader`, which the procurement, ledger, payments and payables
  ports are, and cash adapts its own `Company` port. Neither `config` nor
  `tenancy` imports the other (the adapter returns a plain function type).
- **One rule for a legal entity not found.** Refused, never dated in UTC:
  its company, and so its zone, cannot be told, and UTC's date may be
  another day. Legal entities are never deleted and every caller passes
  one that a request or a stored row names, so the refusal reports broken
  data: 422 `request.reference_not_found` naming the legal entity (a code
  in the document's common list; every operation that can give it lists
  the 422 status, `getMemoRecord` since batch 28's integration, which found
  it by the operations whose call graph reaches `config.EntityDay`;
  `listMemoRecords` reaches it only through the filter, which never
  refuses, below), with no field error, and
  `config.ErrNoLegalEntity` as its cause for `errors.Is`. Before, the
  release's discount check, the ledger's center checks, procurement's
  dates and payables' posting dates failed with a 500, and a memo record
  read alone (create, read, change, the closing and proposal decisions)
  was judged on UTC's date; each now gives the 422. The contract expirer's
  round fails, as before, with the refusal in its log.
- **The memo list keeps its answer, explicitly.** A memo-record list
  filtered by a legal entity the directory does not know reads no date for
  it: it matches no record, so nothing is judged on a date, and the list
  answers an empty page, as for a legal entity the caller cannot access, so
  whether one exists does not leak (`tenancy.Accessible`'s rule). Refusing
  the filter would tell a caller which ids exist. Every other legal entity
  of the list without a zone of its own is judged by UTC's date, the rule
  for a company that sets none (above).
- **Unchanged.** Every date for a known legal entity: the same zone, the
  same instant, the same errors for a zone that does not load or a
  configuration that cannot be read (their wording now the helper's). The
  ledger's tax setup and controlling settings judge a proposed document's
  company (`dayOfCompany`, `config.(*Company).Location`), not a legal
  entity looked up, so they keep that path.

### 9. Liquidity forecast
`GET /v1/liquidity-forecast?company=…&horizon=…` (`cash.forecast.read`):
- **Buckets** from the company's configuration (`cash.buckets`, default:
  daily for 14 days, weekly to 13 weeks, monthly to 12 months, the
  "13-week cash flow" practice with a year's tail); a request can ask for
  one grain over a range;
- rows by liquidity item (and its hierarchy), source and certainty;
  columns per bucket; an opening position (§8's cash position total of
  the first day) and a cumulative closing balance per bucket;
- `min_certainty` cuts the sources (SAP's balance profile);
- **drill-down**: a bucket and item to its flows and documents (open
  items, runs, order items, memo records), through the owning contexts'
  APIs (orders since T53g, As built in §5);
- per currency always; a converted total in a display currency (§10) on
  request; across companies the caller has, in group currency.

As built (T53e, the first slice: supplier open items and runs; split
into T53e and T53e2-T53e4 in the tracker, src `split:T53e`):
- **Request.** `GET /v1/liquidity-forecast?legal_entity_id=…&min_certainty=…`
  (`cash.forecast.read`, migration 000189; `x-slo-class: report`,
  `x-snapshot: true`, so it reads one database state as the cash position
  does). A treasurer, or an agent holding the permission, reads the flows
  per currency, never converted, in the company's `cash.buckets` from the
  read date (the company's date in its time zone, T72n1). A legal entity
  the caller cannot act in is 404. `min_certainty` is an open enumeration
  (`actual`, `confirmed`, `expected`, `planned`, default `planned`);
  another value is 422 `request.validation_failed`.
- **Buckets.** Consecutive from the read date, each grain's periods after
  the previous grain's: a day, seven days, or a month running to the same
  day of the next month (its last day when shorter, the memo records'
  `addMonths`). Each currency gives its flows per bucket by source,
  certainty and lateness with the bucket's total (the cash position's
  `CashFlowTotal`, its source list gaining `payable`), the total over the
  horizon, and what is expected after the last bucket (`beyond`, amount
  and count, in no bucket and not in the total), so nothing owed drops out
  unseen.
- **Sources built.** `payable` (`expected`): the ledger's open supplier
  items in every currency (`ledger.OpenItems.Owed`, read as the proposer's
  `Payable` reads them: leading ledger, not special G/L, not reversed, not
  held by an item reservation), less those a live run's active payment
  items hold. `payment_run` (`confirmed`): the unposted payments of runs
  `proposed` or `releasing` (`payments.CashPosition.Pending`), on the run
  date plus the paying bank's value days, `late` before the read date, as
  in the cash position (one `valueDate`). An item is never counted twice:
  the run's payment counts instead of the items it holds. The payments in
  transit and memo records join with the opening position (T53e4).
- **One rule with the proposer.** The proposer's discount-tier and
  due-date logic moved to the ledger (`ItemTerms.TierOn`, `LosesDiscount`,
  `DiscountOn`, `PayableItem.DueDate`), the discount and withholding at
  payment to one payments helper (`deductionsOn`), and the reserved items,
  the bank ranking and the supplier blocks to shared functions
  (`reservedItems`, `rankedRouter`, `blockedReason`); the proposer calls
  them unchanged in behaviour, its tests green, the tier tests moved to
  the ledger with the rule. `payments.CashPosition.Payables` gives each
  item its due date, its supplier's block, and the bank and method a run
  proposed without a house bank would route it through;
  `CashPayable.Dates` and `Pays` say when and what. The composition root
  adapts it to `cash.Payments.Payables`, so `cash` still imports only
  `config` and the layout allow-list is unchanged.
- **Expected date** (§5, decided by default where §5 leaves it open).
  The days tried are an invoice's discount tiers' last days, earliest
  first, while the company takes discounts, then its net due date; each
  is moved back to the company's last payment day on or before it, and
  the first not before the read date is taken, so a tier whose payment
  day has passed gives way to the next tier or the net due date; when
  none is left (overdue, or due before the next payment day) the item is
  expected on the next payment day on or after the read date, the read
  date itself when it is one. No payment days: any day. A credit memo
  (a debit on the supplier account) tries its due date only, its document
  date, so it is due at the next payment day, as the proposer takes it
  (T34d). Then the value days of the house
  bank the supplier's first payment method ranks for the item's currency
  (`cash.value_days` by bank and method; none ranked: 0), and the next
  working day of the company's calendar; the payment day itself is not
  moved off a holiday, only the day at the bank, as §5 step 4 reads.
- **Netting as the run nets** (review round 1). A run pays a supplier one
  payment per currency, of every item it takes netted, and pays nothing
  to a supplier whose items owe nothing net (`DebitBalance`). So the
  forecast nets per supplier and currency: on each of its payment days, the
  items due then and those carried from earlier days are summed, each at
  what a run that day pays for it; a day that nets to nothing or less pays
  nothing and its items are carried to the supplier's next day; what is
  still a debit balance after its last day is no cash flow and is left
  out. A credit memo alone is therefore never forecast as money coming in,
  and one beside an invoice in a discount tier nets on the tier's day, as
  the run that takes the invoice nets it. A flow is one supplier payment
  (`count` counts payments).
- **Amount.** What a run on that payment day pays the supplier: what each
  item owes less the cash discount of the tier open that day and the
  withholding tax deducted at payment (`ledger.PaymentWithholding` after
  the discount), netted as above, negative in the forecast. The
  proposer's refusal of a withholding that takes all of an item
  (`payments.withholding_too_large`) is a run's, not the forecast's.
- **Blocked suppliers.** A supplier not set up in the company, inactive,
  blocked for posting or for payment (the proposer's `SupplierBlocked` and
  `PaymentBlocked`, `blockedReason`) is left out of the totals in this
  slice; showing its items apart (`payable_blocked`, `include_blocked`) is
  T53e2 (below).
- **Not yet** when T53e was built: liquidity items, the
  self-assessment netting and the derivation simulation (T53e3, below);
  converted totals, the opening position and cumulative closing, memo
  records and payments in transit, one grain over a range, drill-down and
  the budget at 100,000 open items (T53e4, below).
- **Evidence.** `cash.TestLiquidityForecast*` (Maple Inc: a tier taken,
  a tier passed over, an overdue item and a credit memo netted on the next
  payment day, a debit balance paying nothing and a credit carried to the
  supplier's discount day, a USD item a value day later, an item held by a proposed
  run counted as the run's payment, a blocked supplier left out, an item
  beyond the horizon, the certainty cut, a company taking no discounts and
  its own buckets, a late run, refusals and every database failure);
  `payments.TestCashPayable`; `ledger.TestDiscountRounding`, `TestDueDate`;
  the walkthrough's step 13 reads Maple's forecast through the API.

As built (T53e2, parked and held invoices, down payment requests and
blocked suppliers; decided by default where §4 and §5 leave it open):
- **Request and response.** `GET /v1/liquidity-forecast` takes
  `include_blocked` (default false) and echoes it; each currency gains
  `blocked` (amount and count of its `payable_blocked` flows, in the buckets
  and after the last, whether counted or not). `CashFlowTotal.source` gains
  `payable_blocked`, `invoice_unposted` and `down_payment` (an open
  enumeration, additive). No migration: `cash.forecast.read` covers it.
- **One port.** The payments context's `CashPosition.Payables` gives every
  document a run would pay, each with its kind (`CashItem`, `CashUnposted`,
  `CashDownPayment`): the posted items as before, the unposted invoices
  from payables (`payables.Endpoints.Unposted`) and the open requests
  (`payables.DownPaymentRequests.Open`), through two ports payments declares
  (`Unposted`, `Requests`) and the composition root wires, so the routing,
  the supplier blocks, `Dates` and `Pays` are the run's for all three. The
  query of the requests a live run pays moved to one helper
  (`heldRequests`) the proposer uses too. No context gains an import.
- **Unposted invoices** (`invoice_unposted`, `planned`). §4's "kept terms"
  do not exist before posting: `supplier_invoice_terms` is written when an
  invoice posts. So an invoice, credit memo or subsequent debit or credit
  parked or on hold is read as the open item posting it now would give,
  by posting's own functions (`invoiceTerms`: the order's term, else the
  supplier's, from the invoice date, a correction naming an invoice that
  invoice's, a credit memo none; `withholding`: the supplier's codes, what
  is withheld at invoice off what it owes, the rest deducted at payment).
  What would refuse the posting (a 422: a term or withholding code not
  configured, withholding of the whole invoice) does not refuse the
  forecast: the item is then without terms (due on its supplier's net
  days) or without withholding; a database failure fails the read. The
  contract's `x-problem-codes` of the forecast list those three payables
  codes all the same: `TestProblemCodes` derives codes from the call graph
  without branch conditions, a superset by ADR-0052 G7 as built, so a
  consumer may see them listed but never answered. Their
  expected day and amount follow `payable`'s rule (tiers, payment days, the
  ranked bank's value days). They net per supplier and currency among
  themselves, not with the posted items: a different certainty is a
  different row, and a run cannot net them before they post. A parked
  credit memo alone is therefore no flow. Matching, approvals and the
  match tolerance are not judged: a held invoice is forecast as if it
  posts. T53g must count an order's open value net of the quantities of
  its unposted invoices, as of its posted ones, so an order and its parked
  invoice are not counted twice.
- **Down payment requests** (`down_payment`, `expected`). The open requests
  in every currency less those a live run's payment pays (the run's
  payment counts instead, `payment_run`), each whole and never netted, as
  the run pays a request in a payment of its own (T46c), on its due date
  moved to the next working day, `late` before the read date (expected the
  working day after it, as a late payment is). A request's day is not
  moved to the payment days: §4 names the due date, and a run pays every
  request due by its due-by date. Its amount includes the tax it claims
  (T46c).
- **Blocked suppliers** (`payable_blocked`). Everything a supplier no run
  pays is owed (its items, unposted invoices and requests) is shown apart,
  each keeping its own certainty (`expected`, or `planned` for unposted
  invoices), on its net due date (a request's due date), the read date when
  overdue, not moved to a payment or working day (no run pays it), at what
  it owes on that day (after its withholding at payment; its discount
  tiers have lapsed by then), netted per supplier, currency and certainty
  as a run would net it once the block is lifted (a request alone). Its
  flows are listed in their buckets and summed in `blocked`; the buckets'
  totals, the currency's total and `beyond` count them only with
  `include_blocked=true`. `late` is not set on them: an overdue blocked
  item is not expected at the bank the next working day.
- **Evidence.** `cash.TestLiquidityForecastSources` (an invoice on an order
  in its discount tier netted with a held one; a parked credit memo alone;
  withholding at payment; a term and a withholding code not configured; a
  supplier not set up in the company; requests on time, late, held by a
  proposed run and cancelled; a blocked vendor's item, parked invoice and
  request, one after the last bucket; `include_blocked`; the certainty
  cut), `TestLiquidityForecastSourcesFailures` (every database failure),
  `TestLiquidityForecastUnpostedFailure` (an order not found fails the
  read), `payables.TestDownPaymentRequestPorts`; the walkthrough's step 13
  parks an invoice and requests a down payment, reads them and Massive
  Dynamic's blocked items, with and without `include_blocked`, then cancels
  both.

As built (T53e3, liquidity items in the forecast; decided by default
where §4 and §9 leave it open):
- **Request and response.** No new parameter and no migration
  (`cash.forecast.read` covers it). Each currency of
  `GET /v1/liquidity-forecast` gains `items`, its rows by liquidity item
  and its hierarchy (`LiquidityForecastItem`, additive): the item's code,
  name, parent and level, its shares and its descendants' per bucket, over
  the horizon and after the last bucket, counted as the buckets' totals
  count them (a `payable_blocked` share only with `include_blocked`), and
  its own shares by source and certainty
  (`LiquidityForecastItemFlows`), listed whether counted or not. Rows are
  depth first, siblings by code; an item is listed when it or a descendant
  has a share; the row of what no rule assigns and no default covers has
  no `liquidity_item` and comes last. The top rows add up to each bucket's
  `total`, the currency's `total` and `beyond`. Lateness is not a
  dimension of the rows (§9 names item, source and certainty). The rows
  carry no document id or supplier, as the rest of the forecast.
- **What a flow is split over.** A supplier payment (netted per supplier,
  T53e) is split over the documents it pays, each at what the payment pays
  of it on that day (after its discount and withholding at payment; a
  credit memo negative), each document derived on its own (`cash.Derive`)
  and the shares of one item summed, so the shares add up to the flow; a
  run's payment likewise over the items it settles (`payment_item`: amount
  less discount and withholding), a payment of no item (a down payment
  request's) whole. The direction a fallback takes is the flow's (an
  outflow for every flow built so far), so a credit memo netted into a
  payment takes the outflow default, not the inflow one.
- **Offsetting lines** (§4's "offsetting lines of its document"). A
  posted item's are its entry's lines in the leading ledger but those on a
  supplier or customer reconciliation account and those on the account of
  a withholding tax withheld at invoice (`ledger.OpenItems.Offsetting`):
  what the document owes, and what it owes the authority instead, are not
  what the payment pays for. A GR/IR line of an invoice on an order valued
  through GR/IR is classified by the GR/IR account's rule like any line;
  resolving it to the order item's account would need procurement's order
  items, which is left for the order source (T53g). An unposted invoice's
  are its items (account, filled from the order item on an order, and net
  amount signed as posting signs it; `payables.Endpoints.Unposted`), their
  accounts' numbers read from the company's chart; its tax is not
  calculated for the split (the calculation can refuse, and within one
  code tax follows its base in proportion anyway). A down payment request
  has none: its supplier's planning group, else the default.
- **Tax follows its base; self-assessment netted** (the T53e3 note in
  §4). A tax line is a line on the account of one of its tax code's
  components, as the entry's tax items record (`ledger.tax_item`,
  `account_key`). A self-assessed pair's lines (`self_output`,
  `self_input`) are left out before the split, so they classify nothing;
  a part of the self-assessed tax not recovered stays on its base line as
  posted (T61d), as a cost of it. Every other tax line is shared out over
  its code's base lines in proportion to their amounts; a tax line whose
  code has no base line (import tax posted alone, T61e), or whose base
  lines cancel out, stays a line of its own account.
- **Rounding.** Two allocations, each `money.Allocate`: a tax line over
  its base lines, and a document's amount over its items (`cash.Derive`,
  T53b). Each rounds the sum once, to minor units, and gives the residue
  by largest remainder, ties to the earlier line or the item named first,
  so each split adds up exactly and the same input splits the same way
  (`TestOffsetting`, `TestDerive`, `TestClassify`).
- **The planning group.** `partner.Payee.PlanningGroup` (the supplier's in
  the company) reaches the forecast with each payable and each run's
  payment (`payments.CashPayable.PlanningGroup`, `CashPosition.Paying`).
- **The derivation simulation** (§4, tiers; deferred from T53b). A change
  of `cash.derivation` needs a simulation whatever its tier
  (`config.Evidence`, as an analytics definition does). The cash context's
  `Simulation`, run with the change request's simulation through
  `configuration.Simulators` (adapted in `internal/app`, as `cash` imports
  no context), classifies each company's open flows (the forecast's, every
  source and certainty, after the last bucket too, dated by the active
  document) under the active and the proposed rules, and records under
  `liquidity_items_by_company` per company the count of flows whose shares
  change, the first 100 by day (currency, day, source, certainty, amount,
  shares before and after) and per currency and item what those flows
  give it before and after. Like the analytics simulation (ADR-0044) it
  runs as the requester, who must hold `cash.forecast.read` (else the
  operation fails, 403), and covers the companies the requester may act
  in; the listed flows name no document or supplier, as the forecast does
  not, so no partner data is read and no `authz.MayUse` is needed. Each
  reader of the change request is shown the companies they may access
  alone, the summary counting over those (`cash.Disclose`, as the ledger's
  assignment simulation). The simulation lists, it never fails a change.
- **Evidence.** `cash.TestLiquidityForecastItems` (Maple Inc: rent and
  equipment with input tax following them, a reverse charge pair netted
  out, a tax authority by its planning group, a request and a run's
  payment of no item by the planning group, withholding at invoice left
  out, a run's payment of an item and a credit memo, a parked invoice by
  its item, a blocked vendor's share counted only with `include_blocked`,
  the certainty cut), `TestLiquidityForecastUnclassified`,
  `TestLiquidityForecastItemsFailures`, `TestDerivationSimulation`,
  `TestDerivationSimulationFailures`, `TestOffsetting`, `TestClassify`,
  `config.TestCashDerivationChanges`; the walkthrough's step 21: an
  invoice for services parked on 6100, a change request adding an item and
  a rule for 6100 simulated (the parked invoice's flow moves) and
  approved, and the forecast's rows by item read before and after.

As built (T53e4, the forecast converted and drilled down; decided by
default where §7, §9 and §10 leave it open):
- **Request and response.** `GET /v1/liquidity-forecast` takes `grain`
  with `to`, and `display_currency` (all optional, additive); each
  currency gains `opening` (`LiquidityForecastOpening`: amount, banks,
  `stale`, `gap`) and each bucket `closing`; `converted`
  (`LiquidityForecastConverted`) is given when a display currency is
  asked for. The drill-down is a list of its own,
  `GET /v1/liquidity-forecast-flows` (`listLiquidityForecastFlows`,
  `LiquidityForecastFlowPage`), not a sub-resource: the forecast is not a
  stored resource with an id. No migration: `cash.forecast.read` covers
  both. `CashFlowTotal.source` already named `in_transit` and `memo`; the
  item rows' source enumeration gains them.
- **Opening** (§9: "the cash position total of the first day"). Each
  currency opens at the cash position's opening of the read date: the sum
  of its active house banks' latest statement closing balances before the
  read date (`Payments.Banks`, the position's own read, `reader.opening`
  shared with it), `stale` and `gap` when one bank's are, zero without a
  bank. A currency with an active house bank is listed even without a
  flow. A statement closing on the read date itself opens the next day, as
  in the position. The cut by `min_certainty` keeps the opening (it is
  `actual`).
- **Closing.** Each bucket closes at the opening plus the `total`s of the
  bucket and those before it, so it counts what the totals count (blocked
  suppliers' flows only with `include_blocked`, the certainty cut).
- **Payments in transit** (`in_transit`, `confirmed`): the position's
  (`Endpoints.inTransit`, now shared): the released payments' open lines
  on their active bank's clearing account that no statement line up to
  the bank's opening statement matches, on the run date plus value days,
  `late` before the read date, each line a flow. They are classified as a
  run's payment is, by the open items the payment settled
  (`Payments.Paying`, which reads any payment of the legal entity), each
  line at its own amount: where a flow's documents do not add up to it
  (a second open line of a payment), they are shared out over it in
  proportion (`money.Allocate`, rounded once, ties to the earlier
  document), so the rows by item always add up to the buckets
  (review round 1). They name the payment's run.
- **Memo records** (`memo`, at the record's certainty). Every `active`
  record of the legal entity, with a house bank or none, not expired at
  the read, on its value date moved to the next working day (`late`
  before the read date), as the position takes the ones on a bank
  (`memoOccurrences`, now shared). A series is expanded up to the last
  bucket's end (a drill-down's: its `to`, else the company's last bucket),
  each occurrence once, an override in place of its occurrence; a one-off
  record after the last bucket is in `beyond`, a series' occurrences after
  it are not (a series without end has no finite beyond). Each takes the
  liquidity item it names, whole, derived from nothing. A proposed,
  realised, cancelled or rejected record counts nowhere.
- **One grain over a range.** `grain` and `to` together: periods of the
  grain from the read date up to the one holding `to`, at most the
  company buckets' limit of the grain (92 days, 104 weeks, 36 months,
  `config.MaxPeriods`); one without the other, a grain not known, a `to`
  before the read date or more periods are 422
  `request.validation_failed` on the field.
- **Conversion** (§10). On request only (§9: "a converted total in a
  display currency on request"), so a read without it reads no rate. One
  rate per currency for every bucket, the cash position's rule and code
  (`Endpoints.converter`, shared): the latest rate of `cash.rate_type`
  valid on the read date, `average` where that type is `planning` and no
  planning rate is recorded, through the ledger's rate service
  (`ledger.Rates.OfType`, which revaluation's rate lookup shares,
  ADR-0042), `stale` beyond `cash.rate_max_age_days`. **Rounding:** each
  currency's opening, each bucket's total and `beyond` are converted on
  their own and rounded once to the display currency's minor unit, half
  away from zero (`ledger.Conversion.Convert`), then summed over the
  currencies; the converted total is the sum of the converted bucket
  totals and each converted closing the converted opening plus the
  converted totals so far, so the converted figures add up as each
  currency's do (two buckets of USD -0.02 at 1.25 are -0.03 CAD each,
  -0.06 together, where USD -0.04 at once would be -0.05;
  `TestLiquidityForecastConverted`). **A missing rate** leaves the
  currency out of every converted figure and lists it in
  `incomplete_currencies`, as §10 settles ("rather than refused"): never
  counted as zero. An unknown display currency is 422
  `cash.unknown_currency`; an amount out of range 422
  `cash.amount_too_large`. Across companies in group currency (§9, §10,
  §15) is T53e5 (below).
- **Drill-down** (§9). The flows of one currency from `from` (default the
  read date) to `to` (default no end, so `beyond` is reachable), computed
  exactly as the forecast computes them from the same database state, by
  day; with `liquidity_item`, the flows with a share of that item or its
  descendants, each with that `share` (so a bucket's drill-down `total`
  is the item row's cell); with `unclassified=true` the shares of the row
  without an item (not with `liquidity_item`, 422); else every flow, its
  share its amount. `total` adds the counted shares over every page,
  `count` the flows. A flow is what the forecast counts as one: a
  supplier payment netted as a run nets it, a run's payment, a payment in
  transit's line, a down payment request, a memo record's occurrence.
  Each names its supplier (`PartnerName`: id, number, name) and the
  documents it pays (`LiquidityForecastDocument`: kind, id, number, what
  the flow pays of it) to read through their own context: a posted item's
  journal entry (its document number, now read with the open item,
  `ledger.PayableItem.Document`, `Number`), a parked or held invoice, a
  down payment request, the payment run (run and in-transit flows), a
  memo record (its reference). Paged by cursor (04 §4.5), 50 by default,
  at most 500; the token holds the last flow's key, its day and a digest
  of its source, certainty, documents and identity (a run's payment, a
  payment's line in transit, a series' occurrence), so it orders the pages
  by day, names no document, and stays with its flow between two reads:
  a payment leaving the run between two pages neither skips nor repeats
  another (review round 1, `TestLiquidityForecastDrillDownPages`). One
  limit is known (review round 2): a payment's line in transit is told by
  its place among the payment's open clearing lines, so if one payment has
  two open lines and the earlier is cleared between two pages, the later
  may be skipped or repeated; naming the line itself needs its entry and
  line number in the ledger's `OpenLine` port. An
  unknown currency is 422 `cash.unknown_currency`, as the display
  currency is.
- **What the drill-down reveals** (security posture, decided as the most
  restrictive reasonable option and put to the product owner). §15 gives
  `cash.forecast.read` to people and agents, and §9 drills "through the
  owning contexts' APIs", but neither says what a reader of the forecast
  alone may see of a flow. As built, `cash.forecast.read` shows the flows
  (day, source, certainty, amount, share) and `document_count`; a flow's
  supplier is shown only to a caller who may use `partner.supplier.read`,
  and each document only to one who may use its owning context's read
  permission (`ledger.entry.read`, `payables.invoice.read`,
  `payments.run.read`, `cash.memo.read`), as `authz.MayUse` decides it
  (held, not barred by a segregation-of-duties rule), the permissions read
  once a page: otherwise they are left out, never refused (04 §4.4, as
  T45e's bank details and the expansions). The question for the product
  owner: whether a single supplier payment's amount and day, unnamed,
  should need more than `cash.forecast.read`.
- **Budget** (§6). `TestLiquidityForecastBudget`: 100,000 open supplier
  items (two suppliers set up in the company and a blocked vendor, each
  item with terms and an expense line), the forecast in the company's
  buckets converted into CAD, and a month's drill-down by item, 20 reads
  each after a warm-up. §6's 2 s was "a budget to confirm"; measured on
  the development laptop (load 19-26 from parallel lanes) the forecast
  answers in 2.0-2.3 s median, of which the payments port is 1.5-2.0 s:
  the ledger's open-item read (`OpenItems.Owed`, about 1.0 s: an index
  probe per item into the terms, the entry, the clearings and the
  reservations) and the offsetting lines (`OpenItems.Offsetting`, about
  0.5 s); the flows themselves are classified in about 10 ms. A due-date
  index would not help: the forecast reads every open item (`beyond`
  included). The budget is declared at **p95 3 s** (scaled by
  `ERP_BUDGET_SCALE`, 01 §Performance), and §6's 2 s is reached by P3 (b),
  the stored flow table, which the product owner deferred: listed for the
  product owner. On the way the forecast stopped asking the database for
  a house bank's code once per item (`withBankCodes`), and the derivation
  compares an account with the rules once per read (`rules`), taking a
  document whose lines all fall to one item whole without the
  proportional split (the same shares, `TestDerive`).
- **Evidence.** `cash.TestLiquidityForecastOpeningAndSources`,
  `TestLiquidityForecastGrain`, `TestLiquidityForecastConverted`,
  `TestLiquidityForecastDrillDown` (pages, item and descendants, beyond,
  two lines of one payment classified each at its own amount, refusals),
  `TestLiquidityForecastDrillDownPages`, `TestClassify` (parts shared
  out over a line), `TestLiquidityForecastDrillDownGated`
  (not held, and barred by a segregation-of-duties rule),
  `TestLiquidityForecastBlockedDrillDown`, `TestLiquidityForecastDrillFailures`
  (every database failure), `TestLiquidityForecastBudget`; the existing
  forecast, position, memo and simulation tests unchanged in what they
  assert (a bank's currency listed without flows). The walkthrough's step
  23: a USD memo record without a bank, the forecast opened and closed
  per bucket and converted into CAD, a range of weeks, the drill-down of
  two weeks of customer receipts to their memo records, and a person
  holding `cash.forecast.read` alone shown the same flows without their
  documents or suppliers; step 21 now lists step 13's memo records under
  Customer receipts.

As built (T53e5, the forecast across companies in group currency; decided
by default where §9, §10 and §15 leave it open):
- **Request and response.** `GET /v1/group-liquidity-forecast`
  (`getGroupLiquidityForecast`, `GroupLiquidityForecast`;
  `cash.forecast.read`, agents too, `x-slo-class: report`,
  `x-snapshot: true`) takes `min_certainty`, `include_blocked`, and `grain`
  with `to`, as the company's forecast does; no legal entity and no display
  currency. An operation of its own rather than `getLiquidityForecast` without
  `legal_entity_id`: the answer has another shape (its companies, no rows
  by item), and the company's operation keeps its required parameter. No
  migration: `cash.forecast.read` covers it.
- **Which companies** (§9 "the companies the caller has", §15 "the
  caller's access to each company"). Every legal entity of the tenant the
  caller may act in (`authz.Companies`: its company access, or every one
  with `all_companies`), from the tenancy directory
  (`tenancy.Directory.Entities`, through the cash context's
  `LegalEntities.Entities`, adapted in `internal/app`), whatever its
  status, as `getLiquidityForecast` answers for each of them; a legal
  entity outside the caller's access is never read, so it reaches no
  figure, list, count or error. Permissions are tenant-wide (ADR-0010):
  holding `cash.forecast.read` is the operation's check, company access
  the per-company one. Listed in `companies` by company code (sorted, not
  read order) with its name, read date and zone, the rates it was
  converted at and its currencies without a rate. A caller in no company
  reads an empty forecast from today's date in UTC, not a refusal.
- **Group currency** (§10). The tenant's (`tenancy.tenant.group_currency`,
  `LegalEntities.GroupCurrency`), the third currency of every journal
  line (ADR-0005); there is no display currency parameter (§10's "group
  currency across companies").
- **One conversion path.** Each company's forecast is read exactly as
  `getLiquidityForecast` reads it (`readForecastOf`: its sources, read date
  in its zone, certainty cut, `include_blocked`, memo series expanded to
  the group's last day), summed into the group's buckets (`byCurrency`)
  and converted into the group currency exactly as its own forecast
  converts it with `display_currency` set to it (`convertForecast`: the
  latest rate of the company's `cash.rate_type` valid on its read date,
  planning else average, one rate per currency for every bucket, each
  currency's opening, buckets' totals and `beyond` rounded once, half away
  from zero, `stale` beyond its `cash.rate_max_age_days`); the group's
  converted opening, totals and `beyond` are those summed over the
  companies, its total and closings recomputed from them
  (`sums.close`, shared with `convertForecast`). So the group's converted
  figures are the sum of what each company's forecast converted into the
  group currency gives over the same buckets
  (`TestGroupLiquidityForecast`), and a currency is converted at each
  company's own rate type: two companies setting different
  `cash.rate_type`s convert their USD at different rates, each listed
  with its company. No group-level rate setting exists to settle one rate
  per currency; one would be configuration, not this slice.
- **A missing rate** (§10, T53e4's rule). A company's currency without a
  rate into the group currency is left out of that company's converted
  figures and listed in its `incomplete_currencies`, and the group's
  `incomplete_currencies` lists every such currency once, sorted: never
  counted as zero, never refused.
- **Buckets and dates.** Companies may set their own `cash.buckets` and
  read dates differ by time zone, so the group's buckets are the default
  buckets (`config.DefaultBuckets`), or one grain over a range, from the
  earliest of the companies' read dates (`read_date`): every company's
  flows are on or after its own read date, so none falls before the
  first bucket, and a Toronto evening is in the first bucket when UTC is
  already on the next day (`TestGroupLiquidityForecastReadDates`). Each
  company still opens and judges lateness on its own read date, as its
  own forecast does.
- **Per currency** (§9 "per currency always"). `currencies` sums the
  companies' figures of each currency, never converted, by code: the
  openings (banks counted, `stale` and `gap` when one company's are), each
  bucket's flows by source, certainty and lateness, its `total` and
  `closing` (recomputed, `sums.closings`, shared with `byCurrency`), the
  `total`, `beyond` and `blocked`. Rows by liquidity item and the
  drill-down stay per company (`getLiquidityForecast`,
  `listLiquidityForecastFlows`): §9 does not ask for them across
  companies, and no per-company subtotals are given beyond what each
  company's own forecast already answers.
- **Refusals.** A `min_certainty` not known is 422 before any company is
  read, a caller in none included; sums out of the range of an amount, a
  company's or the companies' together, are 422 `cash.amount_too_large`.
- **Evidence.** `cash.TestGroupLiquidityForecast` (Maple Inc, Acme Corp US
  and Acme GmbH in EUR, the group currency: the companies by code, each
  currency summed, the converted figures equal to the companies' own
  converted into EUR, USD without a rate listed and then with one),
  `TestGroupLiquidityForecastAccess` (a reader of two companies sees
  neither the third's company, currency nor amounts; tampering with the
  access filter fails it; a reader of none, and its refusals),
  `TestGroupLiquidityForecastReadDates`, `TestGroupLiquidityForecastTooLarge`,
  `TestGroupLiquidityForecastFailures` (the directory, the group
  currency, a zone, a payment and every database failure),
  `tenancy.TestDirectory`; the walkthrough's step 36: Acme Corp US created
  in USD with a memo record, the group forecast in CAD equal to the two
  companies' own converted forecasts summed, and a person with company
  access to Maple alone shown Maple's figures alone.

### 10. Currency conversion
Amounts are kept and shown in their own currency first: a bank's balance
is never converted in its own row. Totals in a display currency (the
company's local currency by default, group currency across companies,
or any currency asked for) convert each currency's bucket at the
**latest rate of `cash.rate_type`** (default `planning`, falling back to
`average` if the company sets none) valid on the read date, the same
rate for every bucket: no forward rates, which a treasury system or the
phase 9 exposure management supplies. A rate older than
`cash.rate_max_age_days` (default 7) is used and flagged; a missing rate
leaves that currency unconverted and the total marked incomplete
(`incomplete_currencies`), rather than refused, since the per-currency
figures stand without it. Snapshots store transaction-currency amounts,
so a later conversion uses the rate of its own read.

### 11. Actuals and variance
**Actual flows** are the posted and unposted statement lines by **value
date**, per house bank and currency, classified as in §4 (a line not yet
matched takes its direction's default item and is shown as
`unclassified`, so variance is visible before reconciliation finishes).
`GET /v1/cash-actuals` lists them by item and bucket.

As built (T53f2, the actuals; decided by default where §4, §11 and §15
leave it open):
- **Two operations.** `GET /v1/cash-actuals` (`getCashActuals`, the
  report §13 names as the agent's tool) gives the actuals by item and
  bucket; `GET /v1/cash-actual-lines` (`listCashActualLines`) lists the
  lines behind a cell, as the forecast has its drill-down (T53e4) and a
  snapshot its lines (T53f): a report a treasurer reads shows what it adds
  up. Both read with `cash.forecast.read` (§15 names no permission for the
  actuals; §13 gives `getCashActuals` to the forecast's readers, and the
  variance (T53f3) compares the two), `x-slo-class: report`, `x-snapshot:
  true`. No migration: nothing is stored.
- **The lines.** Every line of the legal entity's statements whose value
  date is in the range, posted or not, of every statement status and of
  banks active or not, read through one port the payments context
  publishes (`payments.CashPosition.Actuals`, adapted in `internal/app`,
  so no context gains an import): each with its statement and number,
  house bank, currency, signed amount, value date, status in
  reconciliation, and what its **decided** match clears (the line's live
  `statement_match` that is `matched` or `excluded`; an `open` match, a
  rule's or an agent's proposal, decides nothing). A reversal is a line
  like any other, by its sign.
- **Classification** (§4, `match` on each line and each row). `payment`:
  over the supplier items its payments settled (`Payments.Paying`, as a
  run's payment is classified, T53e3), each item derived on its own and
  shared out over the line's amount in proportion (`money.Allocate`, the
  forecast's `classify`), so what the bank took beside a payment (a
  charge) moves the items' shares in proportion; a
  payment of no item by its supplier's planning group, else the default.
  `posting_rule`: the rule's item (`cash.derivation.posting_rules`), else
  the direction's default. `incoming` (matched to the incoming payments
  clearing account; receivables clear it in phase 3) and `excluded` (a
  person excluded it from reconciliation): the direction's default, since
  nothing traces them further. `unmatched` (no decided match): the
  direction's default too, counted `unclassified` (§11). A line whose
  amount is positive is an inflow, any other an outflow. The kinds of
  match are named by each context (`payments.CashMatch*`, `cash.Match*`)
  and mapped in the composition root.
- **The report.** Buckets of `grain` (`day` by default, `week`, `month`)
  from `from`, the last ending on `to` rather than at its grain's end, so
  the totals are those of the lines asked for; at most the company
  buckets' limit of the grain (`config.MaxPeriods`: 92 days, 104 weeks,
  36 months), else 422 `request.validation_failed`, as one grain over a
  range of the forecast. Per currency: per bucket and over the range the
  inflow, outflow, total, count and unclassified (amount and count); per
  house bank its buckets, total and count (every active bank by code,
  with lines or not, then each bank no longer active that has lines,
  `active: false`); and rows by item and its hierarchy as the forecast's
  (T53e3), each with its own shares by `match` instead of source and
  certainty. Amounts per currency only, never converted (§10 converts on
  request; nothing asked for it here).
- **The lines** of one currency in a range of at most 36 months, by value
  date; filters `house_bank_id`, `liquidity_item` (its subtree, `share`
  that subtree's part of each line), `unclassified` (the unmatched lines;
  not with an item, 422); `total` and `count` over every page; paged by a
  cursor of the value date and a digest of the statement and line, so the
  token names no statement (as T53e4's). A currency of no line lists
  nothing (two decimals), as a snapshot's lines do, rather than 422.
- **What the lines reveal** (security posture: the rule T53e4 set, not
  widened). `cash.forecast.read` shows a line's day, bank (by code),
  status, match, posting rule, amount and shares; its statement (id, line
  number, the statement's reference and the line's bank reference, which
  tell the line at the bank without reading the statement; review round 1)
  only to a caller who may use `payments.statement.read`, its payments
  (id, run and run number, what each paid) only with `payments.run.read`,
  and the suppliers (each payment's own, and once each in `suppliers`,
  which a caller who may not see the payments still gets; review round 1:
  a line of two payments names who received what) only with `partner.supplier.read`,
  each as `authz.MayUse` decides it, read once a page, else left out
  (`payment_count` counts them all). The product
  owner's open question of T53e4 covers these lines too: whether a single
  line's amount and day, unnamed, should need more than
  `cash.forecast.read`.
- **Budget.** §6 and §11 name none for the actuals. The port reads every
  line of the company's statements in the range (no index on the value
  date; a company's statements are read by legal entity under row-level
  security); a pilot books a few thousand lines a month. A budget, and an
  index on `(tenant_id, value_date)` of the lines, are for when a tenant's
  size asks.
- **Evidence.** `cash.TestCashActuals` (Maple Inc: a line posted for a
  payment of rent and equipment split by them, a payment of no item by
  its supplier's planning group with the bank's extra in it, a posting
  rule's line, a receipt unmatched and a line only proposed counted
  unclassified, an excluded line and one to the incoming clearing account
  by their defaults, lines before and after the range left out, a week, a
  day without a bank's lines, a bank no longer active),
  `TestCashActualsUnclassified`, `TestCashActualLines` (pages whose tokens
  name no statement, an item's subtree and its share, a bank, a currency
  of no line), `TestCashActualLinesGated` (barred by a
  segregation-of-duties rule, and not held), `TestCashActualsRefusals`,
  `TestCashActualsTooLarge`, `TestCashActualsOutOfRange`,
  `TestCashActualsFailures` (every database failure),
  `TestCashActualLinesTwoPayments` (a line of two suppliers' payments);
  the walkthrough's
  step 27: the week of step 8's statement by item, every line matched to
  a payment, and Rita shown the lines without their statements, payments
  or suppliers.

**Variance** (`GET /v1/liquidity-forecast-variance?company=…&from=…&to=…&lag=…`):
for a period, the forecast of the snapshot taken `lag` days before the
period's start (default 7; 0, 7 and 30 offered) against the actual
flows, per liquidity item, currency and bucket: forecast, actual,
variance, and absolute percentage. Within the 31 days kept at document
level, the variance is **attributed**:
- `timing`: a forecast document paid in another bucket (a run moved, a
  discount not taken);
- `amount`: paid in the bucket at another amount (discount, partial
  payment, FX on a foreign-currency payment from a local bank);
- `new`: an actual flow whose document did not exist or was not forecast
  at the snapshot (an invoice posted after it, an unforecast receipt);
- `missing`: a forecast flow not paid (blocked later, cancelled order,
  memo record not realised);
- `unclassified`: unmatched statement lines.
Beyond 31 days the variance is per item only. A **forecast accuracy**
measure per item and lag over the last periods comes from the analytical
store (T53j).

As built (T53f3, the variance; decided by default where §6, §11 and §15
leave it open):
- **Request.** `GET /v1/liquidity-forecast-variance?legal_entity_id=…&from=…&to=…&lag=…&grain=…`
  (`getLiquidityForecastVariance`, the report §13 names as the agent's
  tool; `cash.forecast.read`, `x-slo-class: report`, `x-snapshot: true`),
  the legal entity as every cash read names it rather than §11's
  `company`. `lag` is 0, 7 (default) or 30, the three §11 offers, so the
  accuracy per lag (T53j) compares like with like; another is 422 on
  `lag`. `grain` and the period's limit are the actuals' (`day` by
  default, at most 92 days, 104 weeks or 36 months). `to` is on or before
  the company's date (a variance compares what has happened: a day not
  yet passed would show every flow still due as missing) and the
  snapshot's `horizon_end` (nothing was forecast after it), else 422 on
  `to`. No migration: nothing is stored.
- **The snapshot.** The legal entity's daily snapshot dated `lag` days
  before `from`, else the last one taken on demand that day (a tenant
  whose snapshotter missed a day, or a walkthrough before 07:00, still
  compares); none is 409 `cash.snapshot_not_taken`. The response names
  it (id, kind, name, date, `document_level_until`, configuration
  version). Its lines are read as kept, without `payable_blocked`, as the
  forecast's totals and the snapshot's `total` leave blocked suppliers
  out; every certainty counts. Its document lines are read whatever the
  period's end, so a document forecast after `to` and paid in the period
  is `timing`, as it is with a later `to` (review round 1); its bucket
  sums up to `to`.
- **The actuals** are T53f2's lines (`readActuals`, classified by
  `actualShares`), read from the snapshot's date to the later of `to` and
  the document level's end, so a document forecast in the period and paid
  outside it, or the reverse, is told apart from one never paid. The
  classification keeps each line's shares by the document behind them
  (`actual.pieces`: what `partsShares` gives per part, which the
  actuals' shares sum), so the actuals' figures are unchanged.
- **Matching.** A snapshot line at document level names its document by a
  key: a run's or in-transit flow's payment (`payment_id`), a memo series'
  occurrence by the series and its day, else its document (kind and id).
  A statement line's piece is known by, in order: each payment it was
  matched to, the journal entry of the item that payment settled, the
  supplier invoice that entry was posted for (an invoice forecast while
  parked or held, `ledger.OpenItems.Sources`, through
  `payments.CashPosition.Paying`, whose `CashPaid` gains `Entry` and
  `Invoice`), or, for a payment of no item, the down payment request it
  pays (`CashPaying.Request`); and, for a line that realised memo records
  (`cash.memo_record.realised_statement_id`), each record, an override of
  a series' occurrence also as the series and that occurrence's day at
  the bank as the snapshot dated it (its value date moved to the next
  working day, `atBank` on the snapshot's date). Each piece is matched to
  the first key the snapshot names; a line whose payments' items net to
  nothing is one piece known by its payments. A snapshot line naming no
  document (none is written so today) matches nothing and is `missing`.
- **Attribution** (within the document level: `from` to the earlier of
  `to` and `document_level_until`, in buckets of the grain). Each forecast
  line and each paid piece contributes to its currency, bucket and
  liquidity item (and the item's ancestors); its part of the variance
  (actual less forecast) is: for a forecast line, `missing` when no piece
  matched its document anywhere in the window, `amount` when one did in
  the same bucket, else `timing`; for a piece, `new` when it matched
  nothing the snapshot forecast (`unclassified` when its line has no
  decided match, §11), `amount` when its document was forecast in the same
  bucket, else `timing`. So a document paid in its bucket at another
  amount is `amount` (its difference), one paid in another bucket is
  `timing` on both sides, whole, and the five always add up to each
  cell's variance (`TestForecastVariance` checks every cell). A document
  is matched in whatever liquidity item each side classified it: the
  snapshot keeps the items it was taken with (§4), the actuals are
  classified by today's rules, and a rule changed meanwhile shows in the
  rows, not in the attribution.
- **Beyond** the document level the snapshot keeps bucket sums naming no
  document (T53f), so the variance is per item only (§11), without
  attribution, over the days from `document_level_until` plus one up to
  `to`, or up to the day before a snapshot bucket that holds `to` and ends
  after it, whose sum cannot be split by day; `beyond` gives those days,
  absent when there are none. No proration.
- **Figures.** Per currency (of a flow forecast or paid in the days
  compared; never converted, §10) per bucket, over the buckets and
  beyond; rows by liquidity item and its hierarchy as the forecast's
  (depth first, siblings by code, the unclassified row last), today's
  names and tree. `variance_percent` is the variance's size in percent of
  the forecast's size (§11: "absolute percentage"), two decimals, half
  away from zero, absent when nothing was forecast, as controlling's plan
  against actual computes its percent (ADR-0050 §8); both use the one
  exact computation in `money` (`money.AbsPercentOf` here,
  `money.PercentOf` there; batch 50's integration kept T64h's home in
  `money` and dropped T53f3's copy in `apitypes`). A sum out of
  the range of an amount is 422 `cash.amount_too_large`.
- **Visibility.** The variance names no document, supplier, payment or
  statement, so `cash.forecast.read` is all it needs (T53e4's rule; the
  documents behind a cell are read through the snapshot's lines and the
  actuals' lines, each as those show them).
- **Known limits.** A run rejected and proposed again pays the items by a
  new payment, which the snapshot did not name: its forecast is `missing`
  and the new payment `new` (the snapshot keeps a run's payment, not its
  items). A memo record realised by a person, not by a line, matches
  nothing. A flow the snapshot dated late (after its read date) matches
  as any other. Listing the documents behind an attribution, which the
  agent's citations are checked against (§13), is the agent's step
  (T53i) to add on these keys.
- **Budget.** §6 and §11 name none; the read is the actuals' over at most
  period widened to the snapshot's date and its document level's end (at
  most 30 days each side) plus a snapshot's lines (a company's 31 days
  of documents and its buckets). `Paying` gains one query
  (`OpenItems.Sources`, by entry), which the forecast's budget measures
  too.
- **Evidence.** `cash.TestForecastVariance` (Maple Inc's snapshot of 17
  April against the lines to 21 May by week: a run's payment as forecast,
  a receipt short, a run a week late, a series' occurrence and a down
  payment request paid as forecast, a credit memo and a planned payment
  missing, a parked invoice paid once posted, an invoice posted after the
  snapshot new, a receipt not matched unclassified, a payment in transit,
  the days after the document level per item; then by day),
  `TestForecastVarianceLags` (the daily snapshot before one on demand, a
  lag of 30 days, a bucket holding `to`), `TestForecastVarianceEarly` (a
  memo record forecast after `to` realised in the period; a line naming no
  document), `TestForecastVarianceNettedPayment`,
  `TestForecastVarianceRefusals`, `TestForecastVarianceTooLarge`,
  `TestForecastVarianceFailures` (every database failure);
  `money.TestPercentOfSigns`; the walkthrough's step 28: step 26's day
  against its statement lines, each cell's attribution adding up, the
  actual the actuals' and the forecast the snapshot's lines, Rita shown
  the same figures, a lag of 7 refused for want of a snapshot and one of
  5 as not offered.

**Variance comments** (`cash.variance_comment`: company, period, item,
lag, text, status `proposed | accepted | rejected`, author, session) are
written by people or proposed by the agent (§13); T64's close can ask for
them on variances above a threshold.

As built (T53f4, variance comments by people; decided by default where
§11, §13 and §15 leave it open):
- **Table.** Migration 000201 creates `cash.variance_comment` (RLS,
  `tenant_id` first): the legal entity, the period (`period_from`,
  `period_to`, forwards, `variance_comment_period`), the lag (0, 7 or 30,
  the lags the variance offers), the row it explains (`liquidity_item`, a
  tenant's item active or not, by foreign key; null for the row of what
  no rule assigns, as the variance's rows and a snapshot's lines name it),
  the snapshot that variance compares with (`snapshot_id`, of the
  comment's company by a foreign key on `(tenant_id, snapshot_id,
  legal_entity_id)`, for which 000201 adds the unique index
  `snapshot_company`), `text` (1 to 2,000 characters, not blank, as a memo
  record's comment, `variance_comment_text`; the application refuses a
  blank text first, by `unicode.IsSpace`, which is wider than the
  database's `[:space:]`, review round 1), `status`, `decided_by`, `decided_at`, `created_at` and
  `created_by` (the author). §11's `session` is the agent's, so T53i adds
  it with the proposals. Insert-only (no version, so no update is
  granted) and never deleted; T53i's decisions on proposals add the
  version and its grant.
- **The status, ready for proposals.** `proposed | accepted | rejected`,
  all three in the check and in the contract's open enumeration now, so
  T53i adds proposals without a breaking change; a comment is decided
  exactly when it is not proposed (`variance_comment_decided`). A
  person's comment is decided as it is written: `accepted`, decided by
  its author, and published as `VarianceCommentDecided`, the event §15
  names; T53i's proposal is `proposed` until a person accepts or rejects
  it with `cash.variance.comment`, as §13 says.
- **The snapshot kept.** The comment names the snapshot the variance it
  explains compares with (`snapshotOn`: the daily one `lag` days before
  `from`, else the last taken on demand that day) when it is written.
  `cash.delete_expired_snapshots` skips a snapshot any comment refers to,
  whatever the comment's status (a rejected proposal is evidence of what
  the agent said), so the variance a comment explains can be read as long
  as the comment exists; a snapshot deleted at its retention while a
  comment naming it is written is the foreign key's
  (`variance_comment_snapshot`, 409 `cash.snapshot_not_taken`); the other
  way round, a retention run racing a comment not yet committed fails on
  the same key once the comment commits, keeps the snapshot, and on its
  retry finds the comment and skips it (review round 1).
- **API.** `POST /v1/liquidity-forecast-variance/comments`
  (`createForecastVarianceComment`, `cash.variance.comment`, Idempotency-Key,
  `x-agent: false`: §13 gives the agent a proposal, never a comment),
  `GET /v1/liquidity-forecast-variance/comments` (`listForecastVarianceComments`:
  the companies the caller may act in, newest first by `created_at`, by
  legal entity, item or the unclassified row, lag, status and periods
  overlapping `from`..`to`, paged) and `GET …/comments/{id}`
  (`getForecastVarianceComment`), both read with `cash.forecast.read`, the
  variance's own (§15 names no read permission for comments). The path is
  the one §13 names; the agent's proposals take a collection of their own
  (T53i), as memo records' did (T53d, `/v1/memo-record-proposals`),
  since one operation has one permission. The request names the row as
  the snapshot's lines do (`liquidity_item`, or `unclassified: true`,
  exactly one, else 422) and is checked as the variance's request is
  (`checkLag`, `checkPassed`, `checkHorizon`, which the variance now
  calls too): a lag not offered, a period backwards, longer than 36 months
  (the longest grain's limit) or ending after the company's date or the
  snapshot's horizon is 422; a period whose snapshot was not taken 409
  `cash.snapshot_not_taken`; a legal entity the caller cannot act in 404.
  A comment gives its company's code, its item's code and name, and its
  snapshot's id and date. Audited (`cash.variance_comment.create`, with
  the text); the event names the row, period, lag, snapshot, status and
  who decided, not the text, which the comment's read returns (as
  `MemoRecordChanged`). No webhook (`x-webhook`): §14 names one for
  snapshots only. The operations and schemas are named for the forecast
  variance (`ForecastVarianceComment`, `ForecastVarianceCommentCreate`,
  `ForecastVarianceCommentPage`), as their path is: T64i's close variance
  comments (ADR-0058 §10), built in the same batch, own the plain
  `VarianceComment` names with `/v1/variance-comments`, and one contract
  cannot hold two schemas or operations of one name; the list's page
  token is scoped to `listForecastVarianceComments` (batch 51's merge).
- **What it does not do.** A comment does not carry the variance's
  figures (they are read again from the snapshot it keeps); T64's
  threshold on which variances need a comment is T64's.
- **Evidence.** `db.TestCashVarianceCommentRules`,
  `TestCashVarianceCommentPrivileges`,
  `TestCashSnapshotRetentionKeepsCommented`; `cash.TestVarianceComments`
  (an item's row at lag 0 against the snapshot on demand, the unclassified
  row at lag 7 against the daily one, read again, listed by each filter
  and in pages, published and audited, hidden from a reader of another
  company), `TestVarianceCommentRefusals` (every 422, the 409, the 404s,
  an agent and a person without the permission refused),
  `TestVarianceCommentKeepsSnapshot` (retention keeps the commented
  snapshot and deletes one of the same age), `TestVarianceCommentFailures`
  (every database failure, the directory down, a zone lost, the race's
  constraint); the walkthrough's step 30: Tom comments on a row of step
  28's variance, the comment names that variance's snapshot, and Rita
  reads it and is refused writing one.

### 12. Out of scope, and where it goes
- **Receivables and sales orders** as sources: phase 3, as readers of §4;
  until then expected receipts are memo records (decision P6).
- **Tax payables** from T61's returns: a `tax_payable` reader once returns
  are filed with a due date (a small step after T61's returns).
- **Intraday statements** (camt.052) and available balances: a later
  step if the pilot's banks send them; the parser model of ADR-0047
  takes them.
- **Bank calendars** distinct from the company's: the company's calendar
  (ADR-0048) is used for every date; a bank's own calendar is later.
- **Bank Account Management** (signatories, opening and closing workflow,
  dual control): house banks remain configuration under its change
  control (tier 3, a second approver), which is the dual control we have.
- **Cash pooling, concentration and in-house bank**, **money market,
  debt and investment deals**, **FX exposure management and hedging**
  (hedge accounting under IFRS 9): phase 9 (treasury), in a `treasury`
  context next to `cash`, reading its positions; the connector of §14
  exports to a treasury system meanwhile. No hedge designation, no deal
  capture, no forward rates in T53.
- **Liquidity planning** (versions, plan against actual by item, rolling
  plans): phase 9 planning; T53's snapshots and items are its actuals
  and structure.

### 13. Agent
The **forecast agent** (agent type `cash_forecast`, `erp agent
cash-variance -company CODE -period …`) runs on read-only tools
(`getLiquidityForecast`, `getLiquidityForecastVariance`, `getCashActuals`,
`getCashSnapshot`, `listMemoRecords`, `listPaymentRuns`, supplier open
items, `getBankStatement` masked as ADR-0047 shows it to agents,
`listPurchaseOrders`, the semantic layer) and two that propose:
- `POST /v1/liquidity-forecast-variance/comments` (`cash.variance.propose`,
  `x-autonomy: propose`): an explanation per item and period naming the
  documents behind the attributed variance ("supplier X's invoices of
  CAD 120,000 paid on the 14th, forecast on the 7th: the run of the 7th
  was released a week late"), checked against the attribution: every
  document it cites must be one the variance lists;
- `POST /v1/memo-records` with status `proposed`: expected flows it sees
  in history and not in the forecast (a monthly receipt from a customer,
  quarterly tax instalments), with the evidence (statement lines).
A person accepts, edits or rejects each (`cash.variance.comment`,
`cash.memo.write`, `x-agent: false`). The agent never confirms a memo
record, changes configuration, releases or proposes a payment run,
blocks an item or moves money. Its evaluation suite `evals/cash`
(ADR-0039): a late run, a discount not taken, an unforecast receipt, an
instruction planted in a statement's text ignored, a variance it cannot
explain left as "not explained". Model calls go through the gateway
(ADR-0036) with IBANs redacted.

### 14. Treasury connector (decision P8)
Export only, as 07 says: no deals, no positions or payments come back.
- **Content**: per snapshot (and on demand), the cash position per bank
  and day and the forecast per item, bucket, source, certainty and
  currency, in a documented, versioned schema
  (`api/schemas/cash-export.v1.json`, JSON lines and a CSV of the same
  columns), bank accounts by house bank code and last four characters.
- **Channels**: pull through the API (the reads above), push through an
  integration connector of kind `treasury_export` (the SFTP transport of
  ADR-0038, a directory of the treasury system's choice; the file named by
  company, snapshot date and content hash so a resend is recognised), and
  a new connector kind added to `integration.connector.kind` by an
  expand migration; and the webhook event `CashSnapshotTaken` (ADR-0041) pointing at the
  snapshot for systems that pull.
- A format of a named treasury system (Kyriba, SAP TRM's memo record
  import, others) is a mapping on top of this schema, built when the pilot
  names one (facts).

As built (T53h; decided by default where §14 and §15 leave it open):
- **What a file holds.** One snapshot of one company (§6), so the same
  snapshot always gives the same file. A row is what the snapshot kept of
  one currency, day or bucket, house bank, source, certainty and liquidity
  item, summed (`cash.exportRows`): within its 31 days one row per value
  date (`granularity` `day`), beyond one per bucket (`bucket`), so §14's
  "forecast per item, bucket, source, certainty and currency" and "per bank
  and day" are one table; lateness is not a column. A snapshot keeps no
  opening balance (T53f), so the file carries the flows of the position,
  not its balances: a treasury system takes balances from the bank
  statements it receives, and the position as of now stays the API's
  (`getCashPosition`), which a file named by its content could not be. A
  row names **no document, supplier or payment**, only a bank by its code
  and the last four characters of its account (§15, through
  `payments.CashPosition.HouseBankAccounts`), so what leaves for a treasury
  system needs none of the read permissions the snapshot's lines show
  those by (`partner.supplier.read` and the documents' contexts', T53e4):
  the export raises no question of posture. The file carries no totals:
  the snapshot's are `getCashSnapshot`'s, which the webhook points at.
- **The schema** is `api/schemas/cash-export.v1.json`, as §14 names it: a
  JSON Schema of one row, its columns in order in `x-columns`; JSON lines
  (LF, nulls as `null`) or RFC 4180 CSV (CRLF, a header of the columns,
  nulls empty); amounts signed decimals in the currency's minor-unit
  digits, `taken_at` in UTC; rows ordered by currency, period, bank code,
  source, certainty and item. Within v1 a change is a column added at the
  end; anything else is a v2 published beside it. The golden files are
  `internal/cash/testdata/cash-export.v1.jsonl` and `.csv`
  (`TestExportGolden`, `TestExportSchema`, which holds the code, the schema
  and the files to the same columns).
- **The channel.** An integration connector of kind `treasury_export`
  (migration 000209 widens `connector_kind_check` NOT VALID, 000210
  validates it): the SFTP server's `host`, `port`, `user`, pinned
  `host_key` and `directory`, checked as a `bank_sftp` channel's
  (`integration.sftpServer`, shared), without statement directories, the
  key by `secret_ref`; `format`, `jsonl` or `csv`, one per connector; and
  `companies`, the company codes it is sent, named one by one and at least
  one, so a company's figures go only to a channel set up for it.
  `integration.TreasuryChannels` publishes them to the cash context as
  `BankChannels` does to payments, wired by the application
  (`app.cashTreasury`); its endpoint fails closed as a bank channel's
  (`integration.resolve`, shared).
- **When it is sent.** Taking a snapshot, daily or on demand, queues in
  its transaction one `cash.export` per active channel naming its company
  (`cash.export.request`, `with_snapshot` true); a person may ask for one
  again (`POST /v1/cash-snapshots/{id}/exports`, `createCashExport`, 201,
  Idempotency-Key, `x-agent: false`), to a channel set up later or for a
  treasury system that lost the file; a connector that is not an active
  `treasury_export` connector naming the snapshot's company is 422
  `cash.export_channel_invalid`. The **exporter** (`cash.Exporter`, a
  worker of every replica, as the bank file transmitter is, T44e) claims a
  due export in a short transaction (FOR UPDATE SKIP LOCKED), builds the
  file and resolves the channel, uploads with no transaction open, and
  records the outcome in a second transaction guarded by the row version
  its claim wrote (a claim taken over, or an outcome recorded already,
  moved it, so a stale claimant records nothing; the attempt number cannot
  tell a remade last attempt from the one it remakes, T53h review round 2,
  `TestExporterFailures`); a failed attempt waits 1, 5 and 15 minutes, 1 and 4 hours, a
  refusal (a problem below 500, such as a disabled channel) or the sixth
  failure is final (`failed`, the problem code kept). Its principal is the
  system actor `cash-exporter`. Each claim reads the channel again: one
  that no longer names the snapshot's company, or no longer takes the
  export's format, refuses it (`cash.export_channel_invalid`), so an export
  waiting for its next attempt never goes where it is no longer set up for
  (review round 1, `TestCashExportChannelChanged`).
- **Resend recognised.** The file is named
  `cash-<company code>-<snapshot date>-<sha256 of its content>.<format>`
  (§14's company, date and hash; `cash-` names its kind in a shared
  directory). Uploaded under a hidden name and renamed, it appears whole;
  SFTP's rename refuses an existing target, which `sftpclient.Upload` now
  reports as `sftpclient.ErrExists` (leaving the file there and removing
  its own copy; payments, which names files by id, still counts it a
  failure). A name that holds the hash is the same file, so the exporter
  takes `ErrExists` as delivered: a claim whose outcome was lost (the
  replica stopped, the commit unacknowledged) is claimed again after five
  upload bounds and delivered once (`TestCashExportRetries`); a last
  attempt whose claim lapsed is made again as the last rather than taken
  for a failure, as sending it again either delivers it or finds it there
  (review round 1), once: the remake's claim sets `last_error` to
  `cash.export_outcome_unknown`, and a remake whose claim lapses too fails
  the export with that code, so a replica that stops on every upload of
  the file does not have it claimed again at every lease without end
  (T53h review round 2, `TestCashExportLastAttemptLapsing`). Only a
  pending export at the sixth attempt can be a remade one, as the sixth
  attempt's own failure is final, so the code needs no column of its own.
  Each upload's hidden copy has a name of its own, so two uploads of one
  file at once never write into each other's copy. A delivered
  export is `resend` when its channel was delivered the same hash before:
  the same snapshot sent again, the same name, which the treasury system
  recognises (`TestCashExport`); deliveries of one file to one channel are
  decided in turn under an advisory lock of the transaction, so of two
  finished at once exactly one is the first (`TestCashExportSameFileAtOnce`); the audit of the delivery says whether
  the file was still there (`found_there`).
- **Access, audit and events.** `cash.export.read` (§15) reads the
  exports (`GET /v1/cash-exports`, by snapshot, legal entity and status,
  paged newest first; `GET /v1/cash-exports/{id}`), of the companies the
  caller may act in; asking for one is a write and takes a permission of
  its own, `cash.export.send` (000209), a person's. Requests, retries,
  failures and deliveries are audited (`cash.export.request`, `.retry`,
  `.fail`, `.deliver`); a delivery publishes `CashExportDelivered` (§15),
  not offered to webhooks, as §14 offers `CashSnapshotTaken`; a failure is
  the export's status, as §15 names no event for it. An export goes with
  its snapshot when retention deletes it (ON DELETE CASCADE); its audit
  stays.
- **The webhook.** `CashSnapshotTaken` is marked `x-webhook` (ADR-0041):
  its payload names the snapshot and company and carries no bank or
  personal data; `TestCashSnapshotTakenWebhook` fans one out to a
  subscription of `cash.*` and the dispatcher delivers it signed.
- **Walkthrough.** Step 35 connects a treasury system (a stand-in SFTP
  server, `sftptest/sftpstub`, the transport's test double on a port of
  its own), takes a snapshot, shows its CSV delivered and its hash in its
  name, and sends it again as a resend. It does not deliver a webhook: the
  walkthrough runs without the event bus and the webhook client never
  dials this machine (egress, ADR-0041); whether a development server may
  is a question of posture for the product owner.

### 15. Access, audit and events
- Company-level access as everywhere (ADR-0010); permissions
  `cash.position.read`, `cash.forecast.read` (agents too),
  `cash.memo.read|write|propose`, `cash.variance.comment|propose`,
  `cash.snapshot.take`, `cash.export.read`. Bank accounts appear by code
  and last four; full account numbers are payments' and never in `cash`.
- Every write (memo records, comments, snapshots, exports) is audited
  (ADR-0011) and publishes through the outbox (ADR-0007):
  `MemoRecordChanged`, `CashSnapshotTaken`, `VarianceCommentDecided`,
  `CashExportDelivered`.
- Cross-company views (group cash position) need the caller's access to
  each company; they sum in group currency (§10).
  As built (T53e5): the group liquidity forecast,
  `GET /v1/group-liquidity-forecast` (§9 as built), reads every legal
  entity the caller may act in and no other.

### 16. SAP migration (T53k, if the pilot uses SAP Cash Management)
From memory, to check against the pilot's release: liquidity items and
their hierarchy (`FLQLITEM`/`FLQLITEMT` or the S/4 liquidity item master),
planning levels (`T036`) and groups (`T035`) mapped to our certainty and
planning groups by the migration lead, the supplier's planning group
(`LFB1-FDGRV`) onto partner company data, the G/L accounts' planning
levels (`SKB1-FDLEV`) as derivation hints, and open memo records (classic
`FDES` or the S/4 memo flows in `FQM_FLOW`) with a value date after the
cutover as memo records with origin `migration`. Liquidity item query
sequences are not migrated: they are rebuilt as §4's rules and reviewed.
Snapshots and history are not migrated; variance starts at go-live.

As built (a legal entity's date today for clients; review:batch27, step
webdates; Pilot readiness "Company time zone"):
- **The operation.** `GET /v1/legal-entities/{id}/today`
  (`getLegalEntityToday`, `tenancy.org.read`, as reading the legal entity
  is) gives the company's date at the API's clock in its time zone and
  the zone (`date`, `time_zone`; UTC when the active configuration has no
  company of the legal entity's code, or one activated before the zone
  was required that sets none, as `CompanyLocation` reads it for every
  date the API judges), the day the API defaults a posting
  date left out to. A client that pre-fills a date (the web workspace,
  ADR-0052) asks it rather than its own clock, so the form and the API
  agree. A legal entity the caller cannot act in is 404, as every
  tenancy read; without an active configuration, 409
  `config.not_bootstrapped`, as the tax periods' read answers.
- **Where it lives.** The tenancy context serves it (the legal entity's
  path and access rule); the date comes from the configuration context
  through a port, `tenancy.Days`, which `configuration.Settings`
  implements (`CompanyDay`: the active document's
  `(*config.Config).CompanyDay`). The configuration context already
  imports tenancy; tenancy imports nothing of it, so no dependency
  direction changes and no new ADR is needed. The smallest contract
  addition: the cash position and memo records name a date and zone
  already, but only for readers of cash; the legal entity's
  representation would carry a value its `version` and ETag do not
  cover.

As built (the migration's bank accounts on a cutover evening; product
owner 2026-11-02, option A, review:batch37):
- **The gap.** A migration run dated each supplier's first payments bank
  account on UTC's day (`database.Today`). On a cutover evening in
  Toronto UTC is already tomorrow, so a payment run dated on the
  company's day found no migrated account (`no_bank_account`). The
  walkthrough therefore dated its runs on UTC's day and stopped on New
  Year's Eve.
- **The exemption.** Partner accepts a bank account that starts on UTC's
  day minus one in one case only: from `partner.Migration.Create`, for
  the partner that call creates. It applies in both places the control
  judges: the request check (`sealBanks`) and the apply clamp
  (`insertBank`, for the create change that call made). It is carried by
  an unexported type, `partner.backdating`, that no input type holds.
  `CreatePartner`, `RequestBankAccount` and `ApplyChange` always pass
  `noBackdating`; the API, agents (through the API) and the approval
  workflow call them. Two days before is refused even from the migration
  (`partner.backdated_bank_account`, "yesterday (UTC) or later").
- **Who reaches it.** No request can set the date. The one route is a
  migration run (`startMigrationRun`, permission
  `integration.migration.run`, open to a person or an agent holding it):
  the suppliers that run creates start on the server's `database.Today`
  minus one, computed by the run, never read from the extract.
- **What it does not cover.** Every other request by people, agents and
  the API keeps the control unchanged (today or later). So does a migration's later change
  to an existing partner: `Migration.AddCompanies` is an update, which
  carries no bank account. Every bank change has its own request
  (`RequestBankAccount`), which is never exempt. Addresses stay dated on
  UTC's day: the create check is unchanged, and an update's address is
  still refused before it (`partner.backdated_address`).
- **The migration.** `integration.supplierBank` dates the account
  `database.Today` minus one. That is never later than the company's day
  in any zone (offsets run from UTC-12 to UTC+14). No migration file: the
  date is data.
- **What stays.** The create change's payload and the
  `bank_account_changed` event (published with the apply's audit record)
  give the date the migration sent. The audit records are the same
  actions with the same details. The reconciliation still compares the
  account in force on `current_date`, which starts on or before it. The
  payment hold depends on `BankChanged` (the account came from a bank
  change), not on the date, so a migrated first account starts no hold.
  `extendSupplier` (a vendor a run sees again) touches no bank account.
- **Evidence.** `partner.TestMigrationBackdating`: the migration's
  yesterday is accepted; two days before is refused; the API's create
  and bank change, and `CreatePartner` called directly, are refused
  yesterday; every database failure of `Migration.Create` is reported.
  `integration.TestCutoverEveningBankAccount`: a migrated supplier is
  found by the payment run's payee lookup on the Toronto company's day at
  23:30, and the test fails on the old code. `cmd/erp.TestWalkthroughDates`
  covers winter, summer and New Year's Eve. The walkthrough dates its
  payment runs on the company's day and no longer stops on 31 December.

## Alternatives
- **Cash position and forecast inside payments.** Fewer packages, but
  payments would read procurement against today's direction, and phase 3
  and 9 would pile receivables and deals into it. Rejected (P1).
- **A stored flow hub fed by events** (One Exposure's `FQM_FLOW`). Cheap
  reads and SAP's shape, but a second copy of every document's cash
  effect to keep in step and reconcile. Deferred as the scaling path (P3).
- **Forecast from the analytical store only.** No new read paths in the
  operational database, but a position fifteen minutes behind a payment
  release, and no document-level variance. Rejected (P3).
- **No liquidity items, sources only.** Simpler, but a treasurer reads
  cash by purpose (operating, investing, financing; rent, payroll, tax),
  and the agent's explanations need it. Rejected (P4).
- **A statistical or learned forecast now** (time series over statement
  history). Useful for receipts before receivables exist, but opaque and
  untestable as a figure; the agent proposes memo records from history
  instead, and a person accepts them (P6).

## Consequences
- A new context and schema with ports into four contexts; payments gains a
  configuration field (currency), partner company data a planning group,
  procurement an optional delivery date on order items, all additive.
- The pilot sees outflows from documents and inflows only as memo records
  until phase 3; the forecast is only as good as the receipts someone
  records, which the variance makes visible within a week.
- Variance needs history: the first meaningful variance is a week (lag 7)
  after go-live, and accuracy trends a quarter after.
- Every statement imported and every run released changes the position
  at once; snapshots fix what was forecast, so explanations are about a
  forecast as it was, not as recomputed.
- T64 (close) can require variance comments; T59 dashboards show the
  position and accuracy from the store; phase 3 and T61 add readers;
  phase 9 builds treasury on these positions.

## Boundaries with the other steps
- **T49 (bank statements)**: T53 reads statements, matches and the
  reconciliation; it never matches or posts. Statement lines' value date
  is the actual's date; their classification follows their match.
- **T45/T44 (house banks, transmission)**: T53a adds the currency; the
  connector of §14 reuses the SFTP transport.
- **T48 (FX)**: rates and rate types; revaluation of foreign-currency bank
  accounts is ADR-0042's, T53 only requires the flag.
- **T52 (parallel ledgers)**: cash is ledger-independent; book balances
  read the leading ledger.
- **T55/T59 (analytics, dashboards)**: datasets and semantic metrics of
  T53j; dashboards are T59's.
- **T56 (centers)**: none needed; a forecast by profit center is a later
  question (documents carry centers since T56d if the pilot asks).
- **T61 (tax)**: tax on order values through the engine port; tax
  payables a later reader.
- **T63 (delivery)**: CSV/XLSX upload of memo records, scheduled delivery
  of the forecast report.
- **T64 (close)**: variance comments as a close task.

## Steps
Ordered so the pilot's must-haves come first; each ends green (lint,
tests, coverage 100%, reviewer). Lane S. T53a touches payments and the
configuration; T53g touches procurement; the rest is the new context.

| Step | Work | Acceptance criteria |
|---|---|---|
| T53a | **Foreign-currency house banks** (P2) | `currency` on house banks in configuration and `payments.house_bank`; validation (accounts flagged for revaluation, one currency per code, ranking only in the bank's currency); a USD run paid from a USD bank of a CAD company, its file, its USD statement imported, matched and posted with the realised difference; revaluation at period end covers the bank accounts; ADR-0038 amended |
| T53b | **Cash context and configuration** | `cash` schema and package with its imports added to `TestImportRules`, RLS, `tenant_id` first; `cash.liquidity_items` (tree, direction), `derivation` (accounts, planning groups, posting rules, defaults), `planning_groups`, per company `take_discounts`, `payment_days`, `value_days`, `buckets`, `rate_type`, `rate_max_age_days`, `order_lead_days`, `credit_lines`, `snapshot_time`; tiers and the derivation simulation; `planning_group` on partner company data; packs suggest the operating/investing/financing tree |
| T53c | **Cash position** | Ports from payments (statement balances, in-transit payments, runs) and ledger (book balances, rates); `GET /v1/cash-position` per bank, currency and day with opening, flows by source and certainty, closing, staleness and gap flags, book balance check, credit lines; converted totals with incomplete currencies; one-snapshot read; `FailingTx` on every path |
| T53d | **Memo records** | Table, API (create, change with If-Match, cancel, realise by a statement line, list), recurrence expanded at read with overrides, expiry, upload idempotent by batch key, agent proposals not counted until accepted; audit and events; schema tests for each database rule |
| T53e | **Forecast from payables and payments** (P5) | Ports for supplier open items (terms, discount tiers, supplier blocks, withholding, credit memos, items in runs excluded), parked and held invoices, down payment requests; the proposer's due-date and tier logic moved to `ledger.ItemTerms` and used by both; expected-date rule of §5 with payment days, value days and the company calendar; liquidity item derivation with proportional split; `GET /v1/liquidity-forecast` with buckets, certainty cut, per currency and converted, drill-down; performance budget measured at 100,000 open items |
| T53f | **Snapshots, actuals and variance** | `TakeCashSnapshot` workflow per company and day (run-twice test, document level within 31 days), on-demand snapshots, retention; `GET /v1/cash-actuals` classifying statement lines by their matches; `GET /v1/liquidity-forecast-variance` with lag and attribution (timing, amount, new, missing, unclassified); variance comments by people |
| T53g | **Forecast from procurement** (P7) | Optional `delivery_date` on order items (contract first, additive, copied from requisitions' need date), open value net of invoiced quantity plus tax, expected dates from delivery or lead days and the order's terms; requisitions if chosen; ADR-0034 note |
| T53h | **Treasury connector** (P8) | Export schema v1 (JSON lines, CSV); `treasury_export` connector on the SFTP transport, files named by company, date and hash, resend recognised; `CashSnapshotTaken` webhook; export audited |
| T53i | **Forecast agent** | Agent type, read tools, variance comment and memo proposals (`x-autonomy: propose`), citations checked against the attribution; `evals/cash` with the cases of §13; never confirms or posts |
| T53j | **Analytics** | Datasets `cash_snapshots` and `cash_actuals` (contract files, extraction in the tenant's run; the relay marks a tenant due for payments' and cash's events), semantic dimensions (liquidity item, source, certainty, house bank) and metrics (forecast, actual, variance, accuracy); isolation tests passing |
| T53k | **SAP migration** (if the pilot uses SAP Cash Management) | Liquidity items, planning groups, supplier planning groups, open memo records extracted and loaded through the services; quality-gate issues for items with no mapping |
| T53l | **Deferred** | Intraday camt.052, bank-specific calendars, tax payable reader (after T61 returns), receivables reader (phase 3): each its own step when its trigger arrives |

T53a-T53f are the pilot's must-haves (a daily position and a forecast
from what is posted, with variance from the first week); T53g follows as
soon as orders carry delivery dates; T53h and T53i complete the exit
criterion; T53j and T53k can follow the pilot's first month.

## Decisions taken (product owner, 2026-10-21)
All eight as recommended:
- **P1** (a): a new leaf context `cash` reads ledger, payments, payables
  and procurement through their published ports; nothing depends on it.
- **P2** (a): house banks may hold a currency other than the company's,
  built first (T53a), before the cash position.
- **P3** (a): position and forecast are computed on read from PostgreSQL,
  plus a stored daily snapshot; a stored flow table kept from events only
  if a larger tenant breaks the read budget.
- **P4** (a): fixed sources and certainty levels plus the tenant's
  liquidity items, derived by rules from the offsetting account, the
  supplier's planning group and posting rules.
- **P5** (a): a supplier item is expected on the last day of its earliest
  open discount tier while the company takes discounts, else on its net
  due date, moved to the company's payment days, plus the bank's value
  days.
- **P6** (a): inflows before receivables exist are memo records (manual,
  recurring, uploaded), with agent proposals from statement history.
- **P7** (a): purchase order items gain an optional delivery date;
  requisitions are not a source.
- **P8** (a): our own versioned export schema by API pull, SFTP push and a
  webhook; a named treasury system's format is a mapping when the pilot
  names one.

The options as proposed:

| # | Decision | Options | Recommendation |
|---|---|---|---|
| P1 | Where cash management lives | (a) a new leaf context `cash` reading four contexts through ports; (b) inside payments | (a): payments would otherwise depend on procurement against today's direction, and receivables (phase 3) and treasury (phase 9) join `cash`, not payments |
| P2 | Foreign-currency bank accounts | (a) house banks in any currency, in T53a before the position; (b) local currency only, other accounts outside the system | (a), if the pilots hold USD (or other) accounts, which Canadian and UAE companies usually do: without it those balances are not in the cash position |
| P3 | Where position and forecast are computed | (a) on read from PostgreSQL through ports, exact, plus stored daily snapshots, the store for history; (b) a stored flow table kept from events (SAP `FQM_FLOW`); (c) the analytical store only | (a): exact at the read with no second copy to reconcile; (b) stays the path if the read budget fails at a larger tenant |
| P4 | Classification of cash | (a) fixed sources and certainty levels plus tenant liquidity items derived by rules from the offsetting account, the supplier's planning group and posting rules; (b) sources and certainty only | (a): cash by purpose is what a treasurer and the variance explanations need; rules are tier 2, posting nothing |
| P5 | When a supplier item is expected to be paid | (a) the discount date while a tier is open (if the company takes discounts), else the net due date, moved to the company's payment days, plus value days; (b) the net due date only; (c) learned from each supplier's payment history | (a): it is what the payment run will do; (c) is the agent's to suggest as memo records or rule changes later |
| P6 | Inflows before receivables exist (phase 3) | (a) memo records, one-off, recurring and uploaded, with agent proposals from statement history; (b) a statistical forecast of receipts; (c) outflows only until phase 3 | (a): a forecast without receipts misleads; people own the figures and the agent helps fill them |
| P7 | Purchase orders and requisitions | (a) order items gain an optional delivery date (else order date + lead days), requisitions not included; (b) as (a) with requisitions at `planned`; (c) orders by order date + lead days only, no contract change | (a): delivery dates are what make order outflows land in the right week, and requisitions add noise before approval; (b) if the pilot's purchasing approves requisitions long before ordering |
| P8 | Treasury connector | (a) our versioned export schema (JSON lines, CSV) by API pull, SFTP push per snapshot and a webhook; a named system's format as a mapping when the pilot names one; (b) build to one treasury system's import format now | (a): no pilot treasury system is named, and 07 limits the connector to export |

Information needed from the pilot (not decisions): which bank accounts
each company holds, in which currencies, and whether each bank sends
statements daily (camt.053 or MT940) and intraday (camt.052); whether
the companies use a treasury system today (which, and what it imports);
whether SAP Cash Management (FF7A/FF7B, liquidity items, memo records,
One Exposure) is used in the source and with what planning levels,
groups and items; how receipts are forecast today (spreadsheet, CRM,
SAP SD) and from which system expected receipts could be uploaded; the
payment run days and the banks' value days; whether the companies' calendars in the packs
hold the bank holidays that matter; credit lines or overdraft
facilities per account; the forecast horizon and grain the treasurer
works in (13 weeks? 12 months?) and the display currency (CAD, AED, USD
or group currency); whether purchase orders carry delivery dates in the
source; whether large memo records should need approval.

## Sources
- SAP Help, Cash Management (S/4HANA Cloud; cash position over bank
  accounts and bank clearing accounts, 90-day forecast cards, balance
  profile as a set of planning levels):
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/6b39bd1d0e5e4099a5b65d835c29c696/b54be33ef65c45a6be2dbb67487e432e.html
- SAP Help, Cash Position:
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/ac319d8fa4ea4624b40a58d23e3c4627/f07b8854c46ee75ee10000000a44176d.html
- SAP Help, Cash Flow Analyzer (Basic Cash Management: cash position,
  medium- and long-term forecast and actual flows; memo records from One
  Exposure; no liquidity items in basic):
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/ac319d8fa4ea4624b40a58d23e3c4627/1eb9cfdc987742b993b5045cafbe5227.html
- SAP Help, Planning Levels and Planning Groups:
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/ac319d8fa4ea4624b40a58d23e3c4627/12106156c4b61e58e10000000a4450e5.html
- SAP Help, Flow Type (certainty levels):
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/186460fdc35a4b64a713da9bb00deb1e/6b02798d81064337abb6bc738914f30c.html
- SAP Help, Liquidity Item; Liquidity Item Hierarchy; Configuration Tips
  for SAP Liquidity Planner Customers (query sequences, origins C/D/X,
  default liquidity item, default items for bank statement items):
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/48b1ed26c54c440abd913f4a174d7b4d/70491a523ec59e60e10000000a44176d.html ;
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/186460fdc35a4b64a713da9bb00deb1e/53648a542f2b0165e10000000a44538d.html ;
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/ac319d8fa4ea4624b40a58d23e3c4627/d2566556f2441d58e10000000a4450e5.html
- SAP Help, Flow Builder (tracing bank flows to original documents; MM
  flows from requisition to invoice):
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/186460fdc35a4b64a713da9bb00deb1e/835cc0f9ff554154bf06245b70c4c79b.html
- SAP Help, One Exposure from Operations; Sales and Distribution in One
  Exposure (dates from billing date and payment terms):
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/186460fdc35a4b64a713da9bb00deb1e/abd32554225fea6fe10000000a423f68.html ;
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/ac319d8fa4ea4624b40a58d23e3c4627/b57e3b57bd2b3b70e10000000a44147b.html
- SAP Help, Memo Records; Manually Created Memo Records; Manage Memo
  Records (stored in One Exposure, `FQM_FLOW`; planning level; bank
  account for bank-related levels):
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/ac319d8fa4ea4624b40a58d23e3c4627/0ad7bd57f103122be10000000a441470.html ;
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/ac319d8fa4ea4624b40a58d23e3c4627/095440df66294129bf044b8016039bec.html ;
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/186460fdc35a4b64a713da9bb00deb1e/92cd53859b23418eb6bfc4b95c773878.html
- SAP Help (SAP ERP), SAP Cash Management (cash position for bank
  accounts, liquidity forecast for subledgers; FF7B grouping, display
  currency, value date moved off holidays):
  https://help.sap.com/docs/SAP_ERP/495b05f6dbb24b1da46e4027d3a28fef/8a3102521aa53c3ae10000000a441470.html
- SAP Help, Manage Bank Accounts; Configuration for Dual Control (the
  submitter cannot activate their own change):
  https://help.sap.com/docs/SAP_S4HANA_CLOUD/186460fdc35a4b64a713da9bb00deb1e/fd6073c2b5db4e8fa19e4efce7629a96.html ;
  https://help.sap.com/docs/SAP_S4HANA_ON-PREMISE/ac319d8fa4ea4624b40a58d23e3c4627/b643769c27fa47f79e070c1e86b9949b.html

**From memory, to check against the pilot's release:** FF7A as the cash
position transaction (only FF7B was confirmed); `LFB1-FDGRV` and
`SKB1-FDLEV`; tables `T035`, `T036`, `FDES`, `FLQLITEM`/`FLQLITEMT`; memo
records' expiry date; the Flow Builder's proportional split over
offsetting lines; the product names of phase 9's components (In-House
Banking, Exposure Management 2.0, Hedge Management). The Flow Type,
Planning Levels and Memo Records pages render by script and were read
from their indexed text only.
