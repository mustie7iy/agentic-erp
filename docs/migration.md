# Migration strategy

## Objective

Migrate off SAP without disruption, at a fraction of the cost of a
conventional S/4HANA migration or ERP replacement.

## Where the cost of a conventional program comes from

| Cost driver | Conventional approach | This approach |
|---|---|---|
| Big-bang cutover risk | Everything must work on day one, so testing and contingency costs grow | One scope at a time. Each cutover is small and can be reversed (SAP keeps running until the scope is proven) |
| Re-implementing SAP screens and transactions | Thousands of transactions rebuilt or retrained | Users work through review queues and agents; most SAP screens are never rebuilt |
| Consultants for mapping and testing | Manual mapping specs and test cycles | Mapping in code, automated data-quality gate, automated reconciliation on every rehearsal |
| SAP licences during transition | Full licence until the final cutover | Licensed scope shrinks as each company code moves (subject to the SAP contract) |
| Upgrade treadmill | ECC → S/4HANA migration program | Avoided. SAP only has to stay readable until each scope is migrated |

The size of the saving depends on the client's SAP contract, the number of
scopes, and how much custom ABAP has to be replaced. Measure it per scope
rather than promising one number up front.

## Cutover protocol per scope (company code + fiscal year)

1. **Profile.** Run `erp migrate -rehearsal` (a migration run that loads,
   reconciles and rolls back, keeping only its report). Fix data-quality *errors* in
   SAP. Get business sign-off on each *warning* (duplicate vendors, missing tax
   numbers, blocked vendors with open items).
2. **Rehearse.** Repeat full migrations until reconciliation passes with no
   differences, at least three times in a row.
3. **Parallel run.** Process the scope in both systems for one period-end.
   Compare the outputs (open items, trial balance, invoices posted).
4. **Cut over.** Freeze postings in SAP for the scope, run the final delta
   migration, reconcile, and sign off. Switch users to the new flow.
5. **Read-only SAP.** Keep SAP readable for audit until the retention period
   allows archiving.

A scope may only be cut over when `erp migrate` exits 0: no load rejections
and every reconciliation check passing.

## Choosing a mode

| Mode | Loads | Use for |
|---|---|---|
| `documents` | Every Universal Journal document, with SAP numbers kept in the IDs | The current fiscal year, when auditors want document-level history in the new system |
| `balances` | One opening entry per company, with open vendor items kept as individual lines | Closed years, or when history stays in archived SAP |

In either mode an open SAP down payment (a vendor line with special G/L
indicator A, UMSKZ, not cleared) loads as the platform posts its own down
payments (T72c, ADR-0037): its SAP document is an entry of its own, the down
payment on the legal entity's `down_payment` account (from T074) as the
supplier's special G/L item A on its tax code, beside its input tax, and it is
recorded as a paid down payment request. A payment run never nets it, and the
supplier's final invoice clears it and reverses its input tax
(`supplier-invoices/{id}:clearDownPayments`). The quality gate refuses what it
cannot read rather than guess: another special G/L indicator, an indicator off
the down payment account or a line on it without one, a vendor line without
one on an account T074 names or no vendor has as its reconciliation account
(LFB1-AKONT), more than one open down
payment in a document, a credit, a tax code that is not one deductible input
tax, and a tax that is not the code's rate of the down payment (SAP's gross
display, where the down payment includes its tax). A down payment SAP cleared
loads in `balances` mode only, since SAP's clearing of it is not taken over.

In either mode an order SAP valued through GR/IR (a line on its GR/IR account,
T030 WRX, names the order item, ACDOCA EBELN/EBELP) loads valued through GR/IR
(T72f, ADR-0034): its receipts (MSEG 101) are recorded without posting again,
as SAP's journal carries them, and each item's open position is taken over in
an entry of its own per order (document type WE): SAP's balance on the item's
lines and its open quantity, what SAP's posted invoice documents invoiced less
what it received, on the platform's order item, against the GR/IR account's
lines that SAP's documents (or the opening entry) loaded without their order
items. The GR/IR balances show it (`migrated_quantity`), an invoice after
cutover clears it at the order price instead of booking the expense again, and
a closed order's residual is cleared by a GR/IR clearing (SAP MR11). The
quality gate refuses what it cannot read rather than guess: a line on the
GR/IR account without its order item or naming one outside the extract, SAP's
GR/IR account (T030 WRX) with lines when it is not the legal entity's
`gr_ir` account, an order in another currency than the company's (the
extract carries the company code amount, HSL, only), a movement other than a
receipt or a receipt quantity that does not parse on an item with a position,
an open quantity without a balance, and an item received without a GR/IR
line on an order whose other items post one; a GR/IR line naming an order of
another company of the scope is refused too. A later run, such as the final
delta run at cutover, tops the positions of orders an earlier run loaded up to
SAP's: each item's take-over moves what earlier runs took over of it to SAP's
balance and open quantity, in an entry of its own per order (counted as
`gr_ir_top_ups`), so the open GR/IR check passes after SAP posted a receipt or
an invoice on a loaded order, and a run of the same extract again posts
nothing. It tops up only an order whose take-overs are all a run's of its own
fiscal year (its extract holds that year's journal only): an order taken over
by another year's run, or loaded with no position open, is warned about and
stays a difference. What the platform posted on the item after the load (a receipt, an
invoice, a GR/IR clearing) stays beside what was taken over, and shows as a
difference from SAP in the reconciliation. Moving an order already loaded
valued directly onto GR/IR is not done (tracker: "Move open direct-valued
orders onto GR/IR (optional)"): its position stays a difference.

In either mode every migrated line posts in company code currency (ACDOCA HSL),
so the quality gate refuses an open item whose document currency (ACDOCA RWCUR,
SAP's transaction currency; its amount, WSL, is carried and not yet read) is
not the company's: an open vendor item, an open down payment or a line of an
open GR/IR position, named by its document and line. Loaded in local currency
only, it could not be cleared in its own currency (`ledger.mixed_items`).
Foreign-currency open items are not migrated yet. An open item without a
document currency is refused too, rather than assumed to be in the company's
(an OData extract does not carry RWCUR yet). Lines SAP reports as cleared
(AUGBL), lines of no amount and lines on other accounts are not checked
(ADR-0005, As built).

## Reconciliation controls

Each control is computed twice: from the raw SAP extract and from the
platform, read through the owning contexts.
They must be equal.

- Trial balance per account (differences listed by account)
- Total debits posted (`documents` mode)
- Open payables, in total and per vendor, without the down payments
- Open down payments (special G/L A), in total and per vendor, where either
  side has any (T72c)
- Vendor count per company code
- Each vendor's payments bank account: SAP's first LFBK row as it loads
  (scheme, bank country, Canadian routing, last four characters) against the
  supplier's account valid today (differences listed by vendor; T72d)
- Purchase order count and value
- Parked supplier documents by kind: invoices, and credit memos, subsequent
  debits and subsequent credits where either side has one (posted ones are
  carried by their FI documents). The kind comes from RBKP-XRECH and
  RSEG-TBTKZ (T72b), so a credit memo loaded as an invoice fails two checks
- Quantities invoiced in SAP per order item: what the posted invoice
  documents' ordinary items invoiced, credit memos negative, against what the
  load took over for matching (differences listed by order item; T72b2),
  where either side has any
- Open GR/IR per order item: SAP's balance on the GR/IR account and open
  quantity against the platform's position, in total and per order item,
  where either side has any (T72f). The take-over entries move balances
  within the GR/IR account and have no SAP document, so the total debits
  leave them out
- Every SAP document in scope present on the platform (missing ones listed)

## The run's report

A migration run's `report` (`GET /v1/migration-runs/{id}`) is declared in
the contract as `MigrationRunReport` (api/openapi.yaml), which describes every
key; `erp migrate` prints its counts, findings, checks and differences. It
grows with the run:

- When the run starts: `extract`, the rows read per SAP table, each keyed by
  the table's name in lower case as the API names fields in snake_case
  (`t001`, `lfa1`, `agr_define`), and `quality`,
  the data-quality gate's findings (`error` stops the run before anything
  loads, `warning` needs a business sign-off).
- Once the gate passed and the load ran: `load`, the rows it refused or
  noted, and `loaded`, what it created by kind: `suppliers`,
  `suppliers_extended`, `suppliers_blocked`, `purchase_orders`,
  `goods_receipts`, `supplier_invoices`, `posted_invoices_invoiced`,
  `journal_entries`, `down_payments`, `clearings`, `gr_ir_positions` and
  `gr_ir_top_ups`. A kind it created none of is absent. A rehearsal, and a
  load with an `error` refusal, are rolled back: their counts are what the
  load created before the rollback, and nothing of it stays. A kept load
  counts only what no earlier kept run loaded, so a run of the same extract
  again shows `loaded` empty.
- Once the run is reconciled (a rehearsal reconciles in its start):
  `checks`, the controls above, each with SAP's figure, the platform's and
  whether they agree; `diffs`, the differences behind failed checks;
  `missing_documents`, the first 100 SAP documents not on the platform
  (`documents` mode);
  `issues`, SAP values the reconciliation could not use; and `passed`.

A finding or a difference names what it is about by SAP's reference where
there is one: a refused supplier, its posting block included, by the vendor
number (`LFA1`, `<LIFNR>`), a vendor's open items by `vendor <LIFNR>`. Open
items of a platform supplier that is no migrated vendor, which SAP cannot
name, are keyed `partner <id> (<name>)`.

When the run ends, `reconciled` or `failed`, its `MigrationRunCompleted`
event (api/asyncapi.yaml) carries the report's headline figures as
`summary`: `passed`, null when the run failed before it was reconciled, and
`loaded`, the same counts as the report's, null when the quality gate
stopped the run before the load.

## What to check on a real SAP system

- OData services are activated (`/IWFND/MAINT_SERVICE`) and the technical user
  has read authorization only.
- Entity and field names in `internal/sap/odata.go` match your release (check `$metadata`).
- The supplier invoice indicators RBKP-XRECH (invoice or credit memo) and
  RSEG-TBTKZ (subsequent debit/credit item) are in the extract. The OData
  mapping does not carry them yet, so the quality gate refuses an OData
  extract's parked invoices until their properties are mapped (T72b), and
  its posted ones too, whose invoiced quantities the load takes over by
  their direction for matching (T72b2).
- ACDOCA-EBELN and EBELP (a GR/IR line's order item) and T030 are in the
  extract. The OData mapping carries neither yet: the quality gate refuses an
  OData extract's lines on the legal entity's `gr_ir` account, and warns for
  each company with goods receipts that SAP's GR/IR account (T030 WRX) cannot
  be told, so lines on it are recognised only when it is the legal entity's
  `gr_ir` account (T72f). Confirm that every line your system posts there
  names its order item.
- Canadian vendor bank accounts (LFBK, bank country CA) load as scheme `ca`
  only with the bank key (BANKL) in the Canadian Payments Association's
  electronic routing form, 0 + institution (3 digits) + transit (5 digits),
  and an account number of 7 to 12 digits; the quality gate refuses others.
  Check which form your system's bank keys for Canada are kept in (bank
  master BNKA). A Canadian house bank (T012K without an IBAN, bank country CA)
  is generated into the configuration from the same bank key and its account
  number; another form is noted and left for the reviewers (T72d). Confirm
  that the extract's T012K carries the house bank's bank country and key
  (T012-BANKS/BANKL) and the account number (T012K-BANKN).
- Amounts for currencies with no decimals or three decimals (JPY, KWD) come in
  external format.
- **SAP digital-access licensing:** reading data is usually fine, but agents
  creating documents in SAP may count as indirect use. This design never
  writes to SAP. Confirm with the contract owner anyway.
