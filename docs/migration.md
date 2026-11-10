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

## Amounts in SAP's internal form (TCURX)

SAP stores every amount with two decimals and shifts the amount of a
currency with other decimals by 2 − TCURX-CURRDEC: JPY 1,000 (CURRDEC 0) is
stored as `10.00`, KWD 1.234 (CURRDEC 3) as `12.34`. Table exports (SE16N,
SE16H, a custom report, raw dumps) write that internal form; OData writes the
amount itself. Following the pilot customer questionnaire's D10 planning
default (po:2026-11-09), a `sap_file` connector reads its extract's amounts
in internal form unless its settings say `"amount_format": "external"`, and
the extract carries TCURX (CURRKEY, CURRDEC), which the run converts with:
an amount of a currency TCURX lists is read with two decimals and shifted to
its CURRDEC, a currency it does not list has two. A `sap_odata` connector's
amounts are external, unshifted, and it takes no `amount_format`. The
quality gate never guesses:

- an extract in internal form that holds amounts in a currency of other
  than two decimals (ISO 4217, `ref.currency`) which its TCURX does not
  list, or that has no TCURX, is refused once, naming those currencies, and
  each such amount is refused where it is checked;
- an amount with more significant decimals than its form allows (two in
  internal form, the currency's in external), or than the currency's minor
  unit holds once converted (a TCURX that gives a currency more decimals
  than ISO's), is refused;
- a TCURX row whose CURRDEC is not a number of decimals from 0 to 9, or a
  currency listed twice with different decimals, is refused.

The reconciliation reads SAP's side through the same conversion, so its
`Open items in JPY total` compares `-100000 JPY` with the platform's, never
SAP's stored `-1000.00`. The configuration draft (`erp config generate`)
converts the same way: a file extract's amounts are internal unless
`-amount-format external` says otherwise, an OData source's external. The
price tolerance T169G-WERTO becomes the company's `price_absolute`
converted; one it cannot convert (no TCURX row for a currency of other
than two decimals, more than two decimals) keeps the 5.00 default with a
note, and a note quoting T042E's payment limits quotes them converted, or
as exported saying why not.

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

SAP's noted items are not migrated (review:batch63, ADR-0037 As built). A
noted item (ACDOCA-BSTAT `S`), such as a down payment request (special G/L
F), posts nothing to the G/L in SAP (A14 [H], A15 [M]), so in either mode
it loads as no journal entry and counts as no open item, in no total and
no check: the run's scope sets these lines aside, and `extract.acdoca`
counts the journal lines without them. The quality gate reports them as
warnings: one keyed by the company code with how many lines in how many
documents were left out and how many of them are open, and one per open
line (AUGBL blank) keyed `<BUKRS>/<GJAHR>/<BELNR>/<DOCLN>` with its vendor
(number and name), special G/L indicator, posting date and amount in
company currency, and in its document currency where that is another. A
request that is still wanted is entered on the platform as a down payment
request after the load. The extract must carry ACDOCA-BSTAT for this: an
extract without it (the OData mapping does not carry it) cannot tell a
noted item, which then stops the run as an unbalanced document or, if it
balances, would post.

SAP's trading partner is taken over through the supplier (ADR-0059 §4 and
As built; the SAP specialist, A2). A vendor that is a company of the group
names it in its master, LFA1-VBUND, a company ID; SAP gives the vendor's
journal lines the same (ACDOCA-RASSC) but does not guarantee it on a
document's other lines. So in either mode the load does not post RASSC
line by line: it links the supplier it creates to the legal entity that
company is (the supplier's `group_company`), and the ledger gives every
line naming the supplier that trading partner and, when a document's
vendor lines all name the same one, its other lines too, as it does for
every posting on the platform. A company ID is no company code: the load
takes the company code SAP assigns to it (T001-RCOMP, read from every
company code of the extract file, the scope's or not) and the legal
entity of that code, whatever its status, so the legal entity of the
other company only has to exist, not to be migrated or configured. In
`documents` mode an intercompany invoice's expense and tax lines carry
the trading partner with its vendor line, and its payment's lines keep
it; in `balances` mode the open vendor items carry it and the account
balances none, whatever the open items name: an opening entry gathers
lines that belong to no one document. The quality gate
warns and never refuses, a group having companies that are not on the
platform:

- a vendor whose trading partner names no legal entity, keyed by the
  vendor number: its company ID is assigned to no company code of the
  extract or to several, or its company code is no legal entity of the
  tenant. The supplier loads linked to no company and its documents
  without a trading partner, which a consolidation run lists as
  `no_trading_partner`. Create the legal entity before the run to have it
  linked.
- vendor lines whose own trading partner (RASSC) is not their vendor's
  master's, keyed `<BUKRS>/<LIFNR>` with how many lines carry which: they
  load with the master's (a vendor linked, relinked or unlinked since the
  posting).
- lines carrying a trading partner in documents without a vendor line
  (an intercompany loan or recharge posted between G/L accounts), keyed
  by the company code with their number: they load without one.
- other lines of a document with vendor lines that carry another trading
  partner than the load gives them, keyed by the company code with their
  number: they load with the one the document's vendors share, or with
  none where those name different ones, none or a company that is no
  legal entity, and with none in a `balances` run's opening entry.
- once, keyed `VBUND`, an extract that cannot carry trading partners: the
  OData mapping reads neither field, so an OData run links no supplier.

A supplier an earlier run loaded keeps the group company it has: a later
run does not link it, since a supplier's group company changes through a
partner change with its approval, and lines already posted keep theirs
(ADR-0006). The reconciliation then shows SAP's open payables under the
trading partner against none on the platform. Customers are not migrated,
so KNA1-VBUND is not read.

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
account, a movement other than a receipt or a
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

An order in another currency than the company's (EKKO WAERS) keeps its
currency, and its GR/IR positions are taken over in it (product owner,
2026-10-31; ADR-0005, ADR-0034 As built): the take-over entry is in the
order's currency, each item's balance there SAP's document amounts on its
GR/IR lines (WSL) and its company code balance (HSL) kept beside it,
unconverted, at the rate SAP's amounts imply, as a document SAP posted in a
foreign currency loads. A goods receipt or an invoice after cutover moves the
position as it moves one the platform posted in that currency: the invoice
clears it in both currencies, the GR/IR line at the value the position was
booked at and the exchange difference on its own line. A later run tops it up
in both currencies. SAP's documents holding such GR/IR lines load in the
order's currency, so its GR/IR account nets in that currency: in documents
mode unless SAP cleared one with documents in another currency, and in
balances mode a position's documents together, unless SAP cleared a vendor
line of any of them, in whatever currency it paid; those then stay in the
opening entry in company currency. Three cases leave the account netting in
company currency alone: in balances mode an open position with an invoice SAP
paid, and a position SAP paid only in part; in documents mode a document SAP
cleared together with documents in another currency (ADR-0005 As built lists
them; the product owner accepted them on 2026-11-02: nothing is revalued and no
value moves). The quality
gate refuses,
for such an order, a GR/IR line in another currency (RWCUR) than the order's,
a document amount that does not parse, a line with an amount in one currency
and none in the other or the two of opposite signs, a position with a balance
in one currency alone, document amounts that add up to more than an amount
holds, and a rate the take-over entry cannot record; a line of an open
position of an order in company currency that is in a foreign currency is
refused too.

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
document's fiscal year in the extract (ADR-0034 As built, tracker). The SAP
specialist confirmed the fields (po:2026-11-09, A3, [H]): ACDOCA-AUGDT and
ACDOCA-AUGGJ beside AUGBL, all three empty on an open item (AUGDT
`00000000`, AUGGJ `0000`); they are on ACDOCA from S/4HANA 1610, on BSEG
(AUGDT, AUGGJ) before. A clearing reset (FBRA) empties them again, so
whether an item was open at a year's end is read from AUGDT against that
day, never from AUGBL being filled today.

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
as SAP calculates it there (confirmed by the SAP specialist, po:2026-11-09,
A14, [H]: the request is a noted item, BSTAT `S`, special G/L F, posting
nothing to the G/L; its tax is posted only with the down payment itself,
special G/L A). Documents SAP cleared together post in one currency:
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
documents in another currency; a line of an open GR/IR position in another
currency than its order's (above); and, for a document that posts
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

## Cost centers, profit centers and segments

In either mode a run loads the controlling master data of its company codes
first (ADR-0050 §10, T56h): SAP's segments (FAGL_SEGM, with their names
from FAGL_SEGMT), the profit centers assigned to the scope's company codes
(CEPC, CEPCT, CEPC_BUKRS) and the company codes' cost centers (CSKS,
CSKT), each through the ledger's own operations, which check what a
controller's approved request is checked for. The run is their approval:
each center is created active at once, its creation recorded as an applied
center change (`GET /v1/center-changes`) naming the run in its reason,
without an approval request of its own, as the suppliers a run creates are:
ADR-0050 §10 has them "approved by the migration run's approval, not one
by one", and the run, started by a person with `integration.migration.run`
and reconciled, is the governed act (ADR-0050 As built). A
center's time slices (DATAB to DATBI) become its periods, valid_to the
day after DATBI and DATBI 99991231 open-ended, slices saying the same of
what the platform keeps joined into one: a cost center's category (KOSAR),
profit center (PRCTR) and locks (BKZKP actual costs, BKZER actual
revenues, PKZKP plan), a profit center's lock (LOCK_IND). A center's name
and description are its text in English (CSKT and CEPCT KTEXT and LTEXT,
FAGL_SEGMT NAME, language key E or EN) of its latest slice, else its text
in another language with a warning; its person responsible is VERAK of its
latest slice. Each center is created at its leaf of SAP's standard
hierarchy (below), or at the standard hierarchy's UNASSIGNED leaf, as a
center created without a node is, when the extract has none for its
controlling area.

SAP's default account assignments (OKB9) are configuration, so they come
into the configuration draft (`erp config generate`, T56h3) rather than
the run, as the cost elements do. The SAP specialist described them (PO,
2026-11-09): TKA3A gives a company code's cost element (KSTAR, the G/L
account) a default cost center (KOSTL), internal order (AUFNR) or profit
center (PRCTR) in its controlling area (KOKRS), unless its detail
indicator REGEL says SAP takes the object per valuation area (1) or
business area (2) from TKA3C, or per profit center (3) from TKA3P; there
are no validity dates (a change overwrites the row) and no released OData
API, so only a file extract carries them. The platform keeps one default
cost center per company and account (`default_assignments`), and the
ledger activates a default only for a cost center it holds (ADR-0050 §4),
which the run loads after the tenant's first configuration is provisioned.
So the draft holds no default: its notes give, for a row that is the
general assignment (REGEL blank) of a cost center the migration loads in
the company (every CSKS slice of it in the company code and area, the
latest open-ended and not locked for actual costs, BKZKP) on an account of
the company's chart that needs a cost center (`primary_cost`), the entry
to propose in a configuration change once the run has loaded the cost
center (`propose after the migration: {account: "610000", cost_center:
"3100"}`), as the walkthrough's step 3 does. Every other row is a note
naming why it is not proposed: REGEL 1, 2 or 3 with the TKA3C or TKA3P
rows it reads (TKA3A's own cost center is never taken as a fallback), a
default naming only an order or a profit center, BSSUBST set (the
profitability segment found by substitution), a cost center the extract
does not have in the company or that ends or is locked, or an account not
in the chart or needing no cost center. SAP keeps no history of OKB9; the
configuration's versions keep the platform's.

Each SAP line the run posts as it stands carries SAP's account assignment
(ACDOCA RCNTR, PRCTR and SEGMENT; T56h4): the cost center, profit center
and segment of the same code the run migrated, each center the one of the
line's company code (CSKS-BUKRS, CEPC_BUKRS), as the extract reads no
controlling area on the line. The ledger checks them as it checks any
posting (ADR-0050 §5): a profit center other than the cost center's on the
posting date, a center not valid or locked on it, or a cost center on an
account that takes none refuses the document, and the run fails. A line without them is derived as any
line is. The quality gate refuses beforehand a line naming a cost center
that is not one of its company code's in the extract (CSKS), a profit
center not assigned to it (CEPC_BUKRS) or a center in two controlling
areas of it, so not migrated, and a segment that is not the line's profit
center's (CEPC-SEGMENT), or one on a line without a profit center. In
`documents` mode that is every line; in `balances` mode only the lines of
the documents posted as entries of their own (an open down payment's, an
open item's in a foreign currency), as the opening entry carries
balances, not SAP's lines, and so no centers (ADR-0050 §10).

SAP's cost and profit center groups (sets of class 0101 and 0106 of the
controlling area: SETHEADER with their texts from SETHEADERT, their
subgroups from SETNODE in line order and their values from SETLEAF) load
before the centers, as hierarchies (T56h2). The controlling area's
standard hierarchy of cost centers is the group TKA01-KHINR names; that
of profit centers is the one tree holding the groups the profit centers
name as their place (CEPC-KHINR), as the extract reads no field naming it.
Each loads below the root of the tenant's standard hierarchy of its kind
(created coded STANDARD when the tenant has none), so what the tenant's
tree already holds, an earlier run's UNASSIGNED leaf included, is kept.
Every other group no group includes tops an alternative hierarchy coded
as it. Each group is a node, its creation a hierarchy change the run
applied with no approval request of its own, as the centers are
(`hierarchies`, `hierarchy_nodes`). A group's values are ranges (VALSIGN I
includes, E excludes; VALOPTION EQ one value, BT a range, both ends
included), compared as SAP keeps codes (digits alone padded with leading
zeros to ten) and expanded against the extract's centers of the area: a
center sits at the leaves whose values take it and, in the standard
hierarchy, at the leaf its KHINR names, which must be one; it sits in an
alternative hierarchy at the leaf that takes it, if any (`hierarchy_places`
counts both). A later run keeps what is loaded: a group it finds loaded is
not renamed or moved, a group SAP added is added below its parent, and a
center an earlier run loaded keeps its places (a controller places it
through the API). A group an earlier run loaded as a leaf holding centers
that SAP has since given subgroups is refused by the load
(`ledger.hierarchy_change_invalid`, `has_centers`), and every run of such
an extract fails until a controller moves those centers to other leaves
through the API (`POST /v1/hierarchies/{id}:place`). The quality gate refuses a group code that is not one
(up to 15 of 0-9, A-Z, _ and -), a group without a name in any language
(warning of one only in another language than English), a group holding
both subgroups and values, a line naming a group the extract lacks or a
subgroup of another class or area, a value whose sign or option is
another (patterns, CP, are not expanded) or whose range ends before it
starts, groups that include each other, a tree deeper than 12 levels (11
below the standard root), a standard hierarchy (TKA01-KHINR) that is not
a group, profit centers' places in more than one tree, a group name in the
standard hierarchies of two controlling areas or topping alternative ones
in two, a center in no leaf of its standard hierarchy, in two leaves of
one hierarchy, or whose KHINR is not a leaf of it. It warns of a value
taking no center of the extract (it places nothing) and of an area with
groups but no standard hierarchy, whose centers go to the UNASSIGNED leaf.
An alternative hierarchy whose code the tenant already has is refused by
the load and fails the run. The SAP specialist's answers of 2026-11-09
change two of these refusals (tracker leaves, po:2026-11-09): SAP lets a
group hold values beside its subgroups, and a center's KHINR need not name
a lowest-level group, so values on inner groups are to load; and set names
are up to 24 characters in SETHEADER, not 15 (below, "What to check").

The configuration draft (`erp config generate`) takes each profit and loss
account's cost element from SAP's cost element category (CSKB-KATYP of its
latest slice, in the controlling areas of the company's centers; T56h2): 01
`primary_cost` on an expense account, 11 or 12 `revenue` on a revenue
account, and `none` for an account that is not a cost element in SAP (no
CSKB row), so its lines take no cost object (ADR-0050 §4). A category the
account's type cannot take, a secondary or statistical one (which the
platform does not hold until T60) or areas that disagree leave the type's
default, with a note for the reviewer, as does a company whose areas have
no cost elements or whose area the extract does not tell (a company code
without centers: TKA02 is not read). To change after the SAP specialist's
answers of 2026-11-09 (tracker leaves, po:2026-11-09): the slice valid at
the cutover date is taken rather than the latest, and TKA02 is read.

A later run keeps what an earlier one loaded: it assigns a profit center
it finds there to the run's company codes it is not yet assigned to
(`profit_centers_extended`; refused, failing the run, while a change of
the profit center requested through the API is pending), and a change SAP made to a loaded center
since shows as a difference in the reconciliation rather than being
applied. The quality gate refuses what the platform cannot hold rather
than guess: a code that is not one (up to 10 of 0-9, A-Z, _ and -), a
category the configuration does not have (`controlling.cost_center_categories`;
add it there first), a center without a person responsible or a name in
any language, slices that do not read, do not adjoin or put a cost center
in two company codes, a profit center whose slices name different
segments (a reorganisation) or a segment not in FAGL_SEGM, a profit center
code in two controlling areas (the tenant holds one of a code; prefixing
codes per area is the migration lead's to decide and not built), and a
cost center's profit center that is unknown (not in CEPC), not assigned to
its company code or not valid over its slice. It warns of a cost center in
another currency (WAERS) than its company's, which the platform does not
keep. A center or segment whose code a tenant already has, created through
the API, is refused by the load and fails the run.

## Reconciliation controls

Each control is computed twice: from the raw SAP extract and from the
platform, read through the owning contexts.
They must be equal.

SAP holds a journal line once per ledger (ACDOCA RLDNR), and the other
ledgers (local GAAP such as 2L or L1, extension ledgers such as E1, which
hold delta lines only) must never be added to the leading one. A run takes
the leading ledger's lines alone: 0L, the SAP specialist's answer (PO
2026-11-09), to be confirmed on the pilot system in FINSC_LEDGER (its
"Leading ledger" flag); the scope's `Ledger` setting names another
(internal/sap `Scope`; not yet in the API's run scope). The export is to
hold the leading ledger's lines alone (`WHERE RLDNR = '0L'`), so the
quality gate refuses, as a safety net, an extract holding lines of
another ledger or of none named, listing each ledger with its number of
lines, which are left out and never posted; the reconciliation, which
extracts again without the gate, refuses such an extract without
computing any control (review:T56h4). Plan data is in ACDOCP, not
ACDOCA, so no plan filter is needed.

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
- Open payables by trading partner (ordinary items and down payments), in
  total and per trading partner, where either side has any: SAP's open
  vendor items under the company their vendor's master names (LFA1-VBUND,
  by its company code; none for a vendor whose trading partner is the
  company itself or no legal entity), against the platform's open supplier
  items as their lines carry a trading partner, which a consolidation run
  reads (differences listed by `trading partner <company code>`, or
  `trading partner <id>` for a legal entity no vendor of the extract
  names; ADR-0059)
- Vendor count per company code
- Cost centers and profit centers per company code, where either side has
  any: SAP's of the company code against the platform's active ones that
  runs loaded in it, and each one's periods (validity, category, profit
  center and locks of a cost center; validity, lock and segment of a
  profit center) as SAP's slices give them against the platform's
  (differences listed by `cost center <KOKRS>/<KOSTL>` and
  `profit center <KOKRS>/<PRCTR>`; T56h)
- Cost center, profit center and segment totals per company code
  (`documents` mode), where either side has any: SAP's lines on profit and
  loss accounts (SKB1-XBILK blank) added up by RCNTR, PRCTR and SEGMENT in
  company code currency, against the center reports' actuals of the
  centers runs loaded over the fiscal year in the leading ledger (as `GET
  /v1/cost-center-balances` and `/v1/profit-center-balances` read them), a
  segment's the sum of its profit centers' (differences listed by
  `cost center total <KOKRS>/<KOSTL>`, `profit center total
  <KOKRS>/<PRCTR>` and `segment total <SEGMENT>`; T56h4). A line SAP left
  without a profit center that the ledger derives from its cost center,
  or without a segment that the ledger derives from its profit center,
  shows as a difference rather than being assumed. A `balances` run has
  no such control: its opening entry carries the profit and loss balances
  without centers
- Each vendor's payments bank account: SAP's first LFBK row as it loads
  (scheme, bank country, Canadian routing, last four characters) against the
  supplier's account valid today (differences listed by vendor; T72d). The
  account loads valid from the day before UTC's: on a cutover evening in
  Toronto UTC is already tomorrow, and a payment run dated on the company's
  day must find it. Partner allows that one day only to the suppliers a
  run creates (product owner 2026-11-02; ADR-0051 as built)
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
  where either side has any (T72f); for an order in a foreign currency the
  balance in that currency too, as `-1350.00 CAD (-1000.00 USD), quantity -4`,
  and a total per such currency (`Open GR/IR in USD total`).
  The take-over entries move balances
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
  `journal_entries`, `down_payments`, `clearings`, `gr_ir_positions`,
  `gr_ir_top_ups`, `segments`, `profit_centers`, `profit_centers_extended`
  and `cost_centers`. A kind it created none of is absent. A rehearsal, and a
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
name, are keyed `partner <id> (<name>)`. A trading partner is named by the
SAP company code its company ID is assigned to (`trading partner 2000`),
never by the platform's name of that company, which a person running the
migration may not be allowed to read; one no vendor of the extract names
is keyed by the legal entity's id alone.

When the run ends, `reconciled` or `failed`, its `MigrationRunCompleted`
event (api/asyncapi.yaml) carries the report's headline figures as
`summary`: `passed`, null when the run failed before it was reconciled, and
`loaded`, the same counts as the report's, null when the quality gate
stopped the run before the load.

## What to check on a real SAP system

An SAP specialist answered the open points of this section on 2026-11-09
(product owner, po:2026-11-09; answers A1-A16), from the S/4HANA
on-premise 2020-2023 data dictionary as generally shipped, without a live
system. Each answer carries the specialist's confidence: **[H]** standard,
stable behaviour (still worth one SE11 check); **[M]** standard with known
exceptions, to verify on the pilot system; **[L]** best understanding
only, to verify in SE11 or `$metadata` before relying on it. An answer
that changes what is built is a tracker leaf (src po:2026-11-09); the
others record a confirmation.

- OData services are activated (`/IWFND/MAINT_SERVICE`) and the technical user
  has read authorization only. Several released APIs reached on-premise
  only in later releases than S/4HANA Cloud: activate them and confirm
  against the pilot's `$metadata` (A7).
- Entity and field names in `internal/sap/odata.go` match your release (check `$metadata`).
  The specialist's names (A7, **[L]** unless marked, to verify against the
  pilot's `$metadata`; the tracker leaf "Validation against a live system's
  $metadata" records them): `API_JOURNALENTRYITEMBASIC_SRV`,
  `A_JournalEntryItemBasic` with `TransactionCurrency` and
  `AmountInTransactionCurrency` (RWCUR, WSL) [M], `CostCenter`,
  `ProfitCenter`, `Segment` [M], `PurchasingDocument`,
  `PurchasingDocumentItem` [M], `SpecialGLCode`, `TaxCode` [M],
  `PartnerCompany` (RASSC) [L], `ClearingDate`, `ClearingDocFiscalYear`,
  `ClearingAccountingDocument` [L] (on-premise alternative:
  `API_OPLACCTGDOCITEMCUBE_SRV`, `A_OperationalAcctgDocItemCube` [L]);
  cost and profit center read APIs exist with release-dependent names
  (e.g. `API_COSTCENTER_SRV`, `A_CostCenter`, `A_CostCenterText`) [L].
  None is known for T030 (GR/IR account determination, configuration) [H],
  segments (FAGL_SEGM) [M], center groups (sets 0101/0106) and CSKB [M]:
  these come from the file extract only.
- The supplier invoice indicators RBKP-XRECH (invoice or credit memo) and
  RSEG-TBTKZ (subsequent debit/credit item) are in the extract. The OData
  mapping does not carry them yet, so the quality gate refuses an OData
  extract's parked invoices until their properties are mapped (T72b), and
  its posted ones too, whose invoiced quantities the load takes over by
  their direction for matching (T72b2). Confirmed (A13(d), [H]): XRECH `X`
  is an invoice and blank a credit memo, TBTKZ `X` a subsequent
  debit/credit. Their OData properties (A7, [M], to verify on the pilot
  system): `API_SUPPLIERINVOICE_PROCESS_SRV`, `A_SupplierInvoice`'s
  `IsInvoice` (true an invoice, false a credit memo) and
  `A_SuplrInvcItemPurOrdRef`'s `IsSubsequentDebitCredit`.
- ACDOCA-EBELN and EBELP (a GR/IR line's order item) and T030 are in the
  extract. The OData mapping carries neither yet: the quality gate refuses an
  OData extract's lines on the legal entity's `gr_ir` account, and warns for
  each company with goods receipts that SAP's GR/IR account (T030 WRX) cannot
  be told, so lines on it are recognised only when it is the legal entity's
  `gr_ir` account (T72f). Confirm that every line your system posts there
  names its order item. Answered (A13(a), [M], to verify on the pilot
  system): lines SAP's materials management posts (MIGO, MIRO, MR11, MR8M)
  carry it; manual G/L postings to the GR/IR account (possible when the
  account is not "post automatically only", SKB1-XINTB), legacy or
  migration balances and some reclassification postings do not, and the
  gate refuses those: check SKB1-XINTB and profile the GR/IR account's
  lines before the first run. A goods receipt's line carries the order's
  currency, but an invoice's carries the invoice's, which MIRO allows to
  differ from the order's (A13(b), [M]): the gate refuses such a line
  today (tracker leaf). SAP clears GR/IR by quantity, and the difference
  between receipt and invoice valuation goes to the exchange rate
  difference account (transaction key KDM) or, by price control and stock
  coverage, to stock or price difference (PRD), not always one account
  (A13(c), [M]).
- Vendor bank accounts from OData: `A_BusinessPartnerBank` keys an account by
  its business partner, which the extraction links to its supplier through
  `A_BusinessPartner`'s `Supplier`, so the numbers need not be equal; a
  partner that is no supplier has its accounts left out, and the quality gate
  warns of each vendor in scope that no partner links to, which loads without
  bank accounts (review:T72d, ADR-0034 As built). The entity and property are
  stated from memory: confirm them in `$metadata`, and that every supplier's
  business partner names it. Answered (A7): every S/4HANA supplier has a
  business partner [H] (customer-vendor integration makes it mandatory, link
  table CVI_VEND_LINK), and the two numbers may differ, so the link is
  joined, never assumed equal, as the load does. The properties (A7, [M],
  to verify on the pilot system): `API_BUSINESS_PARTNER`,
  `A_BusinessPartnerBank` with `BusinessPartner`, `BankIdentification`,
  `BankCountryKey`, `BankNumber`, `BankAccount`, `IBAN`, `SWIFTCode`,
  `BankControlKey` and `BankAccountHolderName`; `A_BusinessPartner`'s
  `Supplier`, and in reverse `A_Supplier`'s `Supplier`.
- Canadian vendor bank accounts (LFBK, bank country CA) load as scheme `ca`
  only with the bank key (BANKL) in the Canadian Payments Association's
  electronic routing form, 0 + institution (3 digits) + transit (5 digits),
  and an account number of 7 to 12 digits; the quality gate refuses others.
  Check which form your system's bank keys for Canada are kept in (bank
  master BNKA). A Canadian house bank (T012K without an IBAN, bank country CA)
  is generated into the configuration from the same bank key and its account
  number; another form is noted and left for the reviewers (T72d). Confirm
  that the extract's T012K carries the house bank's bank country and key
  (T012-BANKS/BANKL) and the account number (T012K-BANKN). Answered (A6,
  [M], to verify on the pilot system): the routing form (`000112345`) is the
  usual one, since CPA-005 and EFT files need it, but legacy data also holds
  the paper (MICR) form transit-institution with a hyphen (`12345-001`) and
  sometimes eight digits without the leading zero (`00112345`). The
  hyphenated form converts without guessing (`12345-001` is `000112345`)
  and is accepted (sapvend) for vendor accounts and house banks alike;
  ambiguous forms stay refused by the quality gate (vendors) or noted for
  the reviewers (house banks). Before the load, profile LFBK-BANKL and
  BNKA-BANKL for BANKS `CA` on the pilot system to see which forms it holds,
  for example by the pattern of each value (nine digits with a leading
  zero, five digits-hyphen-three, other) and their counts. The
  fields are confirmed (A5, [H]): T012K-BANKN the account number (an IBAN,
  if used, in TIBAN), T012-BANKS the bank country, T012-BANKL the bank key.
- Each house bank account (T012K) is generated in its currency, T012K-WAERS:
  an account in another currency than the company's is noted, and the draft
  validates once its ledger account is flagged `revalue_foreign_currency`
  with its unrealised gain and loss; an account without WAERS is drafted in
  its G/L account's currency (SKB1-WAERS of T012K-HKONT, which the extract's
  SKB1 should therefore carry), noted, and with `currency: UNKNOWN` where
  SKB1 gives none either, which validation refuses until a reviewer sets it
  (ADR-0038). Confirm that the extract's T012K carries WAERS, and what an
  empty one means in your system, before go-live: a bank's currency never
  changes once it is created. Answered (A5): T012K-WAERS is the currency
  [H]. An empty one [M, to verify on the pilot system] means no currency was
  maintained, so SAP does not restrict the account; the binding currency is
  then that of its G/L account (T012K-HKONT, SKB1-WAERS), usually the
  company code's, which the draft takes when WAERS is blank (sapvend; to
  verify on the pilot system). From S/4HANA 1610 house bank accounts are kept in Bank Account
  Management (FI-BAM: FCLM_BAM_AMD, linked by FCLM_BAM_ACLINK2); T012K is
  still filled and read, and BAM's currency list and validity are a useful
  cross-check.
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
  the order of preference. Answered (A4): the tables and fields are as
  stated, with T042I to add [H]: the account per house bank, method and
  currency (HBKID to HKTID, the T012K account), without which the bank is
  known but not its account; an item may fix its method (BSEG/ACDOCA-ZLSCH)
  and LFB1-HBKID may fix the house bank. A lower RANGF ranks first [H]; a
  T042A row without a currency applies to every currency without a row of
  its own [M]. ZWELS's letter order is **not** the order of preference [M]:
  the payment run (F110) tries the methods in the order of its own
  parameters per company code, restricted to those ZWELS allows, and an
  item's fixed method wins over both. The method order and T042I's account
  are a tracker leaf; verify both on the pilot system.
- Vendor addresses: the load first took the vendor's street with house
  number, city, postal code and region from LFA1 STRAS, ORT01, PSTLZ and
  REGIO (product owner 2026-11-04, ADR-0054 As built), field names written
  from memory. The SAP specialist answered (A1, [H]): those fields exist
  and are filled, but as a redundant copy of the central address, and
  LFA1-STRAS (35 characters) holds street and house number together. The
  load now reads ADRC (sapvend): LFA1-ADRNR = ADRC-ADDRNUMBER in the
  extract's client, the row with ADRC-NATION blank (the international
  version; other values are other scripts) and ADRC-DATE_FROM <= the run's
  day <= ADRC-DATE_TO, the run's day being the company's date in its time
  zone when the run starts, not UTC's (review:batch63, ADR-0051 As built:
  for a vendor set up in several company codes a row valid on any of
  their days counts, so an address SAP changes between two companies'
  days is one of several and is warned of; the address starts on the
  platform on UTC's day, partner's rule); street ADRC-STREET followed by ADRC-HOUSE_NUM1 and
  ADRC-HOUSE_NUM2, further lines ADRC-STR_SUPPL1/2/3 and ADRC-LOCATION as
  the address's second line, city ADRC-CITY1, postal code ADRC-POST_CODE1,
  region ADRC-REGION, country ADRC-COUNTRY (LFA1-LAND1 when blank). A blank
  HOUSE_NUM1 is normal: many systems hold the house number in STREET. The
  extract exports ADRC for the vendors' ADRNR (the run reads only those of
  the scope's vendors). A vendor without ADRNR, as every vendor of an OData
  extract is (the mapping does not carry it), loads without an address; a
  vendor naming an address the extract holds no valid row of, or several,
  one without a street name or a city, or a text longer than an address
  holds is warned of by the quality gate and loads without one. A vendor an
  earlier run created keeps the address it has. **To verify on the pilot
  system:** whether its countries print the house number before the street
  (the load keeps SAP's field order, street then number, and does not
  apply SAP's per-country address layout, which the answers do not give).
- The trading partner (ADR-0059 §4, the migration leaf). Answered (A2):
  the vendor's is LFA1-VBUND ("company ID of trading partner"; KNA1-VBUND
  for a customer) [H] and the journal line's ACDOCA-RASSC [H] (BSEG-VBUND
  on the entry view); SAP does not guarantee RASSC on every line of a
  document [M, to verify on the pilot system]: the vendor line always has
  it, lines from materials management (MIRO, GR/IR), generated lines (tax,
  exchange and price differences, cash discount), documents with several
  vendors and lines a user cleared often lack it. The load therefore
  derives a document's trading partner from its vendor line's supplier.
  Run the specialist's query on the pilot's data to see how far SAP's
  lines differ (documents whose vendor line has a trading partner that
  another line lacks):

  ```sql
  SELECT a.rbukrs, a.gjahr, a.belnr, b.docln, b.racct, b.ktosl
  FROM acdoca a JOIN acdoca b
    ON b.rldnr = a.rldnr AND b.rbukrs = a.rbukrs AND b.gjahr = a.gjahr AND b.belnr = a.belnr
  WHERE a.koart = 'K' AND a.rassc <> '' AND b.rassc = '' AND a.rldnr = '0L';
  ```

  **Not in the answer, stated from memory, for the specialist:** that the
  company a company code belongs to is T001-RCOMP (check table T880), the
  field the load reads to tell which company code a VBUND is; that
  several company codes may share one company (the gate then warns, the
  company ID naming no one legal entity); and that VBUND, RASSC and RCOMP
  hold the company ID in the same ALPHA form (the load compares them
  without leading zeros and blanks). The extract must export T001 for
  every company code, not only the scope's, with RCOMP, and LFA1 with
  VBUND; an extract without either links no supplier and says nothing,
  since a blank VBUND is also what a vendor outside the group has. Also to
  check on the pilot's data: intercompany documents posted without a
  vendor line (loans, recharges between G/L accounts), which load without
  a trading partner and are warned of, and vendors whose VBUND was set or
  changed after documents were posted. The OData mapping carries none of
  the three fields: the answer names the tables' fields, and of the APIs
  only `A_JournalEntryItemBasic`'s `PartnerCompany` for RASSC [L] (A7),
  which the load does not post; a property of `A_Supplier` for VBUND and
  of `A_CompanyCode` for RCOMP is to be found in the pilot's `$metadata`.
  Until then an OData run warns once and links no supplier.
- The controlling master data (T56h; ADR-0050 §10 states the tables from
  memory, and internal/sap's types say which fields are not even in its
  list): that CSKS carries KOKRS, KOSTL, DATAB, DATBI, BUKRS, KOSAR, VERAK,
  PRCTR, KHINR, BKZKP, BKZER, PKZKP and WAERS; that CSKT is keyed by
  KOKRS, KOSTL, DATBI and SPRAS with KTEXT the name and LTEXT the
  description; that CEPC carries KOKRS, PRCTR, DATAB, DATBI, SEGMENT,
  KHINR, LOCK_IND and the person responsible VERAK; that CEPCT is keyed as
  CSKT, CEPC_BUKRS by KOKRS, PRCTR and BUKRS, and FAGL_SEGMT names a
  segment by SEGMENT, LANGU and NAME. What the load takes them to mean:
  DATAB and DATBI both days included, 99991231 open-ended; BKZKP the lock
  of actual primary costs, BKZER of actual revenues, PKZKP of plan primary
  costs (the platform's one plan lock), LOCK_IND a profit center's lock,
  each "X" set and blank not; E as SPRAS and LANGU English. That a company
  code belongs to one controlling area. Which KOSAR keys the pilot uses,
  for the configuration's categories of the same codes (the mock's are
  the walkthrough's ADMIN and SALES). The OData mapping reads none of
  these tables yet: an OData run loads no centers until the cost and
  profit center read APIs are mapped against the pilot's `$metadata`.
  Answered (A8, every point [H]): the tables, fields and meanings are as
  stated. The keys are KOKRS, KOSTL, DATBI for CSKS (DATAB is no key
  field) and KOKRS, PRCTR, DATBI for CEPC; CEPCT is keyed by SPRAS, PRCTR,
  DATBI, KOKRS; KTEXT holds 20 characters and LTEXT 40. Further locks SAP
  keeps, not read: BKZKS (actual secondary costs), PKZKS (plan secondary
  costs), PKZER (plan revenues); VERAK_USER holds the responsible user's
  ID. The language key is stored as `E` (SE16 may show `EN`, its ISO
  form): the extract reads raw values. A company code belongs to exactly
  one controlling area, which TKA02 (BUKRS to KOKRS) names; it is to be
  read (tracker leaf). Which KOSAR keys the pilot uses waits for the
  pilot customer (below).
- The centers on journal lines (T56h4; ADR-0050 §10 names the fields
  from memory): that ACDOCA carries a line's cost center, profit center
  and segment as RCNTR, PRCTR and SEGMENT, in the same form as CSKS-KOSTL,
  CEPC-PRCTR and FAGL_SEGM-SEGMENT (numeric codes zero-padded alike); that
  SAP fills PRCTR wherever it fills RCNTR, and SEGMENT from the profit
  center's master; and whether the extract should read the line's
  controlling area (the load takes the one of the line's company code).
  The OData mapping reads none of the three: an OData extract's lines load
  without centers until `A_JournalEntryItemBasic`'s properties for them
  are confirmed in the pilot's `$metadata` (T56h5). Answered (A9): RCNTR,
  PRCTR and SEGMENT share the master data's domains and ALPHA form [H].
  PRCTR is filled wherever RCNTR is, with exceptions [M]: a cost center
  without a profit center gets the dummy profit center, and a line keeps
  the derivation of its posting date, so it is reconciled against the
  master valid then, not today's. SEGMENT comes from CEPC-SEGMENT unless
  the BAdI FAGL_DERIVE_SEGMENT or a manual entry set it, or the profit
  center's segment changed after posting [M]. The controlling area is the
  company code's [H]; ACDOCA also carries KOKRS, worth reading, as it
  shows company codes without CO active (KOKRS blank).
- The center groups and cost elements (T56h2; ADR-0050 §10 names the
  tables from memory): that TKA01 carries the cost centers' standard
  hierarchy as KHINR, and whether it (or another table) names the profit
  centers' standard hierarchy, which the load derives from CEPC-KHINR
  instead; that SETHEADER keys a set by SETCLASS, SUBCLASS (the
  controlling area for classes 0101 and 0106) and SETNAME, SETHEADERT
  gives its text as DESCRIPT by LANGU, SETNODE its subsets as SUBSETCLS,
  SUBSETSCLS and SUBSETNAME by LINEID, and SETLEAF its values as VALSIGN,
  VALOPTION, VALFROM and VALTO by LINEID; that values hold centers in
  their internal form (numeric codes zero-padded to ten), that I and E
  and EQ and BT are the signs and options cost and profit center groups
  use, and that CSKS-KHINR and CEPC-KHINR name a leaf of the standard
  hierarchy; how long a set name may be (the platform holds 15
  characters); that CSKB carries KOKRS, KSTAR, DATBI and KATYP, with 01
  primary costs and 11 and 12 revenues and sales deductions; and whether
  the extract should read TKA02 to tell a company code's controlling area.
  The OData mapping reads none of these tables. Answered (A10-A12):
  TKA01-KHINR names the cost centers' standard hierarchy [H]; the profit
  centers' is in TKA01-PHINR [L, to verify in SE11 on the pilot system],
  and deriving it from CEPC-KHINR (walking SETNODE up to the root), as the
  load does, is a sound fallback. SETHEADER, SETHEADERT, SETNODE and
  SETLEAF are as described [H], SUBCLASS the controlling area for classes
  0101 and 0106; subsets are ordered by SETNODE-SEQNR where the release has
  it, else LINEID, and values by LINEID. CSKS-KHINR and CEPC-KHINR need
  **not** name a lowest-level node [M]: a set may hold subsets and values
  on the same node, so the tree must allow values on inner nodes (tracker
  leaf; the gate refuses such a group today). Set values hold numeric codes
  zero-padded to ten [H], alphanumeric codes left-justified and unpadded,
  the same ALPHA rule as CSKS-KOSTL, which is the load's. Groups kept in
  KSH2/KCH2 use only I with EQ or BT [M]; sets edited with GS02 may hold
  other options (CP), which stay refused. A group name holds at most 15
  characters in the CO transactions [M], but SETHEADER-SETNAME holds 24,
  the size to hold (tracker leaf). CSKB carries KOKRS, KSTAR, DATAB, DATBI
  and KATYP [H]; KATYP 01, 11 and 12 mean what the draft takes [H], and
  others met are 22 (external settlement), 41-43 (secondary) and 90
  (statistical, for balance sheet accounts, asset accounts in S/4HANA). An
  account without a CSKB row is no cost element [H]; S/4HANA's G/L
  account type SKA1-GLACCOUNT_TYPE decides it (`N` non-operating without
  CSKB, `P` primary costs or revenue, `S` secondary, `X` balance sheet), a
  cross-check worth reading. The slice to use is the one valid at the
  cutover date, not the latest [M]: the latest only when none covers it,
  and the account flagged, since an expired cost element means CO postings
  were stopped on purpose (tracker leaf).
- The default account assignments (OKB9, T56h3; tables and fields from the
  SAP specialist, PO 2026-11-09, to be validated in SE11 and SE37 on the
  pilot's release): TKA3A keyed by MANDT, BUKRS and KSTAR with KOKRS,
  KOSTL, AUFNR, PRCTR, REGEL (data element CO_MASCHK), XGSBER and BSSUBST;
  TKA3C keyed by MANDT, BUKRS, KSTAR, BWKEY and GSBER (blanks kept) with
  KOKRS, KOSTL, AUFNR and PRCTR; TKA3P keyed by MANDT, BUKRS, KSTAR and
  PRCTR with KOKRS, KOSTL and AUFNR (to be confirmed). Whether SAP falls
  back to TKA3A's own objects when REGEL is set is to be tested in the
  target system with K_ACCOUNT_ASSIGNMENT_GET and test postings (the
  specialist's tests A to E); the draft assumes it does not. TKA3A and
  TKA3C were pooled tables in ECC (check TKA3P too): check the table
  category before choosing how to extract them. The draft checks a
  default's cost center against the extract's CSKS (in the company, its
  latest slice open-ended without BKZKP); the ledger checks it again when
  the change activates. The orders and profit centers a row names (AUFK
  with JEST/JCDS, CEPC) are not checked, as no such row is proposed.
- The ledger of the journal lines (ACDOCA RLDNR; review:T56h4): the SAP
  specialist answered (PO 2026-11-09) that the standard leading ledger is
  0L, which the run takes; confirm it on the pilot system in FINSC_LEDGER
  (flag "Leading ledger"), and export its lines alone (`WHERE RLDNR =
  '0L'`). Which other ledgers the pilot keeps is for its SAP team. The
  OData mapping does not read the line's ledger and labels every line the
  leading ledger, so the gate cannot tell an OData extract's ledgers apart
  until `A_JournalEntryItemBasic`'s ledger property is mapped (T56h5).
- SAP's internal amount form (built, see "Amounts in SAP's internal form";
  stated from memory, for the SAP specialist to confirm): that TCURX keys a
  currency by CURRKEY and gives its decimals as CURRDEC; that a currency
  TCURX does not list has two decimals (the run relies on it only for
  currencies of two ISO decimals and refuses the others without a row);
  that every amount the run reads (ACDOCA-HSL and WSL, EKPO-NETPR,
  RBKP-RMWWR and WMWST1, RSEG-WRBTR) is stored with two decimals in that
  form; that SE16H and the custom extract report write the internal form
  unless asked for the external one; and that the OData APIs deliver
  external amounts.
- **SAP digital-access licensing:** reading data is usually fine, but agents
  creating documents in SAP may count as indirect use. This design never
  writes to SAP. Confirm with the contract owner anyway.
- **Vendor reconciliation accounts** (ADR-0060 §9): apart from open vendor
  items, other things can make up a reconciliation account's balance in SAP
  (A15, [M], to verify on the pilot system), each told by a field: special
  G/L postings on alternative reconciliation accounts (ACDOCA-UMSKZ not
  blank, the account from T074); noted items such as down payment requests,
  which are open items with no G/L effect (BSTAT `S`); items left on an old
  account after the vendor's LFB1-AKONT changed (LFB1-AKONT against
  ACDOCA-RACCT); balances on an account flagged as a reconciliation account
  after it had one, or loaded without vendor items (SKB1-MITKZ `K` with
  lines whose ACDOCA-LIFNR is blank); a balance carried forward into period
  000 without items from before S/4HANA; a year-end valuation posted to the
  reconciliation account rather than an adjustment account (by
  configuration: the valuation runs' AWTYP or BLART, T030H); and items open
  at the key date but cleared later (AUGDT after the key date). **Built
  (sapvend):** the quality gate checks each vendor reconciliation account
  (SKB1-MITKZ `K`, T074's special G/L accounts left out) at the key date,
  the company's latest posting date (BUDAT) in the extract: the open vendor
  items, lines with LIFNR filled, UMSKZ blank and BSTAT not `S`, open at
  the key date (AUGBL blank, or AUGDT after it), against the account's
  balance, its lines without the noted items. A difference is a `warning`
  keyed `<BUKRS>/<account>`, never an error, and splits it into lines
  without a vendor, special G/L lines and cleared vendor items that do not
  net to zero in the extract (a clearing in another year or on another
  account), and counts the open items of vendors whose LFB1-AKONT is now
  another account. The extract should carry ACDOCA-AUGDT and BSTAT (both
  optional: without AUGDT an item with AUGBL counts as cleared). The
  period 000 carry-forward and a valuation posted to the account are named
  but not told apart (the extract carries no POPER, AWTYP or T030H); verify
  the check against the pilot's data.
- **Noted items and the document status (BSTAT)** (review:batch63): the
  load leaves out the lines with ACDOCA-BSTAT `S` and reports them, on the
  specialist's answers A14 and A15. Still to ask the SAP specialist, none
  of it assumed by the code: (a) whether ACDOCA on the pilot's release
  holds noted items at all, or only BSEG and the vendor open item views
  do, in which case the extract never has one and BSTAT need not be
  exported; (b) which other values of BSTAT can appear in ACDOCA and
  whether any of them marks lines that must not be loaded as journal
  lines either (from memory, to be confirmed and not relied on: BKPF-BSTAT
  also knows parked, recurring, sample and clearing documents, and
  S/4HANA has values for postings to one ledger group and for balance
  carry-forward lines); (c) whether a noted item's line carries the
  reconciliation account or the special G/L account of T074, and an amount
  in HSL; (d) whether the OData service used
  (API_JOURNALENTRYITEMBASIC_SRV) returns noted items and exposes their
  status. Until (d) is answered an OData extract cannot tell a noted item.

**Facts still awaited from the pilot customer** (the customer questionnaire
of 2026-11-09, not answered yet). Each has a **planning default, to be
confirmed by the customer before go-live** (the questionnaire's defaults,
po:2026-11-09; ⚠ marks a default that could do harm if wrong):
- **D17, the licence: ⚠ a gate.** The default (read-only extraction is no
  indirect or digital access) is **not** applied: no extraction from the
  customer's SAP system until its contract owner confirms in writing.
- D9: S/4HANA 2023 on-premise (FPS02 or later), OData for a read-only
  technical user, `$metadata` sent; the OData names above are checked
  against it.
- D10: SE16H for small configuration tables, a custom ABAP extract report
  (or SAP Data Services) for ACDOCA and other large tables; CSV in UTF-8,
  separator `;`, a header row of technical field names, dates `YYYYMMDD`,
  decimal point `.`, no thousands separators; amounts in SAP's **internal**
  format, not external, so the extract also takes TCURX, which the run
  converts the amounts of currencies with other than two decimals with
  (built: "Amounts in SAP's internal form"; a connector set to
  `amount_format` external reads an export in external form instead).
- D12: SAP's standard cost center categories E (development), F
  (production), H (service), L (management), V (administration) and W
  (sales), to be created in the configuration; the extract's distinct
  CSKS-KOSAR values then add any customer keys.
- D15: SAP Asset Accounting used (the fixed asset take-over, ADR-0057 §11,
  is then likely needed; it stays the product owner's open decision),
  Cash Management not.
- ⚠ D11: the Canadian company holds 100% of the UAE company, which it
  incorporated, group currency CAD: no non-controlling interest, goodwill
  or fair value adjustment for the pilot (ADR-0059).
