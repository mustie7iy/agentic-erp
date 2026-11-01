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
GR/IR account (T030 WRX) with lines when it is not the legal entity's `gr_ir`
account, an order in another currency than the company's (the extract carries
the company code amount, HSL, only), a movement other than a receipt or a
receipt quantity that does not parse on an item with a position, an open
quantity without a balance, and an item received without a GR/IR line on an
order whose other items post one; a GR/IR line naming an order of another
company of the scope is refused too. A later run, such as the final delta run
at cutover, tops the positions of orders an earlier run loaded up to SAP's:
each item's take-over moves what earlier runs took over of it to SAP's balance
and open quantity, in an entry of its own per order (counted as
`gr_ir_top_ups`), so the open GR/IR check passes after SAP posted a receipt or
an invoice on a loaded order, and a run of the same extract again posts
nothing. It tops up only an order loaded by a run of its own fiscal year and
whose take-overs are all such a run's (its extract holds that year's journal
only); every external reference names the run that loaded it, so an order
loaded with no position open is topped up too. An order loaded or taken over
by another year's run, or loaded with no position open before references named
their run (migration 000168), is warned about and stays a difference. What the
platform posted on the item after the load (a receipt, an invoice, a GR/IR
clearing) stays beside what was taken over, and shows as a difference from SAP
in the reconciliation. Moving an order already loaded valued directly onto
GR/IR is not done (tracker: "Move open direct-valued orders onto GR/IR
(optional)"): its position stays a difference.

In `documents` mode the load clears the vendor items SAP cleared (AUGBL) by
SAP's clearing document, or by the latest document of the clearing when the
clearing document is not in the extract, once every document of the
clearing is on the platform: a later run, such as the final delta run at
cutover, clears an open item an earlier run loaded with the payment it
loads. It also clears a clearing whose documents earlier runs all loaded.
A clearing already done is left as it is, so a run of the same extract
again clears nothing; an item the platform has cleared or reversed since,
or that a payment proposal holds, is refused and fails the run rather than
being cleared twice. A clearing whose counter-item is
another fiscal year's (a prior-year invoice paid this year) is not in the
extract, which holds the scope's fiscal year only and not SAP's clearing
date (AUGDT): its clearing does not net to zero and fails the run, as the
cleared items of such a year do in `balances` mode. Decided (product owner,
2026-10-31) and not built yet: such an item is loaded open at its fiscal
year's end, as SAP's key-date reading by the clearing date does, and the
later year's run clears it; this needs SAP's clearing date and the clearing
document's fiscal year (AUGDT, AUGGJ, for an SAP specialist to confirm) in
the extract (ADR-0034 As built, tracker).

A document whose lines are all in one document currency (ACDOCA RWCUR, SAP's
transaction currency) other than the company's posts in that currency, as the
platform posts a document of its own in it (product owner, 2026-10-31): each
line's document amount (WSL) is its amount and its company code amount (HSL)
is kept as its local amount, unconverted; the entry records the rate SAP's
amounts imply (the company code amount over the document amount of the line
with the largest document amount), since the extract does not carry the
document's own rate. So an open vendor item or open down payment in USD of a
CAD company is a USD item: a USD payment or final invoice clears it, a payment
run in USD pays it, and a revaluation values it as one posted on the platform,
while the trial balance and the open payables stay SAP's in CAD. A down
payment's request is in its document currency, and its tax is checked in it,
on the assumption that SAP calculates it there (for an SAP specialist to
confirm). Documents SAP cleared together post in one currency:
in `documents` mode the documents cleared together (directly or through
others) post in the currency an earlier run posted those of them it loaded in,
else in their document currency when they share one, and a set that mixes
currencies (a USD invoice paid in CAD) posts in company currency as before. A
document that would have to clear with an earlier run's documents in a
currency it is not in (a USD item loaded open, then paid in CAD) is refused by
the quality gate. In `balances` mode the documents holding an open
item in a foreign currency load whole on their own beside the opening entry,
as down payments do, and the others are part of the opening balances.

The quality gate refuses, naming the document and line: an open item without a
document currency (an OData extract does not carry RWCUR and WSL until they are
mapped against the pilot system's `$metadata`; they then load as a file
extract's do), rather than assume the company's; an open item whose document's
lines are not all in its currency, or whose document SAP cleared together with
documents in another currency; a line of an open GR/IR position in a foreign
currency, which is not migrated yet (tracked); and, for a document that posts
in a foreign currency, a currency the platform does not know, a document amount
that does not parse, a line with a document amount and no company code amount
or the two of opposite signs, a vendor line without a document amount, a
document that does not balance in its currency or whose document amounts add
up to more than an amount holds, and a rate an entry cannot record (zero at
its 12 decimals, or 10^16 and above). An open item with a document amount and
no company code amount is refused whatever its document posts in, in both
modes: the load would leave it out, posting no line of no company code amount
(review:batch31; ADR-0005 As built). So is any other line with a document
amount and no company code amount, in a document that posts as an entry of its
own in company currency (in `documents` mode every document not in a foreign
currency; in `balances` mode a down payment's): a G/L line of a document whose
lines mix currencies, or a cleared vendor line, would be left out and its
document amount lost; the opening entry of `balances` mode carries company
code balances alone. A cleared vendor line of no amount in either currency is
not an item of the clearing the load takes over. A line with a company code amount and no
document amount (an exchange difference SAP posted in local currency) is
loaded as one (ADR-0005, As built). A company whose payables hold such items
values its payables account at period end (`fx_valuation.accounts`, with an
adjustment account; ADR-0042), or its revaluation is refused naming the
account.

## Reconciliation controls

Each control is computed twice: from the raw SAP extract and from the
platform, read through the owning contexts.
They must be equal.

- Trial balance per account (differences listed by account)
- Total debits posted (`documents` mode)
- Open payables, in total and per vendor, without the down payments
- Open payable items: the number of open vendor items (without the down
  payments and lines of amount 0), in total and per vendor. A clearing SAP
  recorded and the platform did not leaves both of its items open, which
  the open payables net to zero and the count shows
- Open down payments (special G/L A), in total and per vendor, where either
  side has any (T72c)
- Open items in each foreign document currency (ordinary items and down
  payments), in total and per vendor, in that currency, where either side has
  any: the controls in company currency cannot tell an item loaded with the
  wrong document amount
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
- Each house bank account (T012K) is generated in its currency, T012K-WAERS:
  an account in another currency than the company's is noted, and the draft
  validates once its ledger account is flagged `revalue_foreign_currency`
  with its unrealised gain and loss; an account without WAERS is drafted with
  `currency: UNKNOWN`, which validation refuses until a reviewer sets it
  (ADR-0038). Confirm that the extract's T012K carries WAERS, and what an
  empty one means in your system, before go-live: a bank's currency never
  changes once it is created.
- Payment methods come into the configuration draft from T042Z (per
  country, named in the company's country), T042E (per company code) and
  T042A (the bank ranking per method and currency, in RANGF order; a row
  without a currency ranks its bank for its account's currency); a method
  without a name or a ranking, or a ranked bank the company does not have or
  in another currency, is noted and refused by validation until a reviewer
  corrects it. T042E amount limits and T042I bank subaccounts have no
  counterpart and are noted. Suppliers' methods come from LFB1-ZWELS, in
  order: the quality gate refuses a method the company's configuration
  lacks, or one listed twice (ADR-0038). Confirm the tables and fields in
  your release, that a lower RANGF ranks first, and that ZWELS's order is
  the order of preference.
- Amounts for currencies with no decimals or three decimals (JPY, KWD) come in
  external format.
- **SAP digital-access licensing:** reading data is usually fine, but agents
  creating documents in SAP may count as indirect use. This design never
  writes to SAP. Confirm with the contract owner anyway.
