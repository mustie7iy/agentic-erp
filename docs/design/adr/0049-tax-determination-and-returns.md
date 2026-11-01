# ADR-0049: Tax determination, calculation and returns

**Status:** accepted (T61, product owner 2026-10-20). The product owner took
every decision as recommended (P1-P8, recorded at the end); the steps
T61a-T61m are in the tracker.

## Context
The pilots are a Canadian and a UAE company (07 §7.6, Phase 2 decisions). Both
must file a sales tax return from the ledger: the GST/HST return (the GST34
lines) or, for a Quebec registrant, Revenu Québec's GST/HST-QST return
(FPZ-500), and the UAE's VAT 201. Today the ERP has only part of what a return
needs:

- **Tax codes** are `config.TaxCode` (code, description, one `rate_percent`,
  account), applied by `ledger.Setup` to `ledger.tax_code` as **input** codes
  only. The table is already effective-dated (`valid_from`/`valid_to`, a GiST
  exclusion on overlaps, a `direction` of `input | output`), but the setup
  writes a single rate valid from the activation day and ends the old one
  "today": a future rate cannot be configured ahead, and a second change on one
  day is refused (`checkTaxCodesChangedToday`). Country packs (ADR-0048) carry
  dated rates, but the applier writes only the rate in force on the date of
  application and notes the later ones "until T61".
- **Supplier invoices** carry one header tax amount, entered or captured, not
  calculated. `payables.invoiceLines` posts it as **one** input tax line on the
  account of the first item's tax code (else `account_determination.input_tax`).
  Nothing checks the amount against a rate: the tracker's
  `capture.checkTaxRate` no longer exists in the code (removed with T7/T8; the
  tracker entry is stale and is corrected by T61c). An invoice with GST and PST
  (British Columbia), or with a standard and a zero-rated item, cannot be posted
  correctly; ADR-0048 already notes "one tax code per line until T61".
- **The ledger** stores `tax_code` on journal lines (base lines and tax lines
  alike) and validates the code on the posting date (`checkTaxCodes`). Down
  payments (ADR-0037) post their tax with the request's code; cash discounts
  correct the input tax per tax line (T36d, 03-data-model, payments). There is no record of a
  tax **base** per code, of the rate applied, of the jurisdiction, or of which
  return an item was reported in.
- **There is no output tax source.** Sales and billing are phase 3; the pilots'
  sales are invoiced in their own billing systems. A return without output tax
  is not a return.
- Withholding tax (T46e/f) is a different tax (deducted from the supplier) and
  stays as it is; it is not part of this ADR.

The tracker asks for "Tax engine integration (e.g. Avalara, Vertex)", "Tax
returns", and finishing "input tax codes applied from the configuration with
effective dating" (07 §7.6 T61: "a tax-engine connector interface with built-in
calculation for the pilot countries (the provider is chosen in this step);
VAT/GST returns for the pilot countries").

### What the pilot countries require (at the level an ERP needs)
Sources are listed at the end; where I am unsure, it says so.

**Canada.**
- GST 5% in non-participating provinces; HST in participating provinces (13% ON;
  15% NB, NL, PE; NS 15%, then 14% from 2025-04-01); QST 9.975% administered by
  Revenu Québec; PST in BC (7%), MB (7%), SK (6%) administered by the provinces,
  with their own registrations and returns. The Canada pack (T62e) already
  carries these rates.
- **Place of supply** decides GST vs which HST rate: goods by where they are
  delivered or made available; services generally by the recipient's Canadian
  address obtained in the ordinary course (the contracting address where there
  are several), falling back to where the service is performed; real property
  and services in relation to it by where the property is (Place of Supply
  (GST/HST) Regulations, CRA B-103). For the pilot's **purchases** this means the
  tax charged depends on the supplier's place-of-supply decision, so the ERP
  checks what was charged rather than re-deriving the supplier's rule; for
  **sales** (output) the jurisdiction is a property of each sales line.
- **Return lines** (CRA, "Instructions for preparing a GST/HST return"): 101
  sales and other revenue (incl. zero-rated and exempt, excl. tax), 103 GST/HST
  collected or collectible, 104 adjustments increasing net tax, 105 = 103+104,
  106 ITCs, 107 adjustments decreasing net tax, 108 = 106+107, 109 = 105-108,
  110 instalments, 111 rebates, 112/113A/113B/113C paper subtotals, 205 tax on
  taxable real property or emission allowances self-assessed, 405 other GST/HST
  self-assessed (imported taxable supplies used < 90% commercially; the
  provincial part of HST on goods brought into a participating province), 114
  refund, 115 owing. Most registrants must file electronically.
- **Reporting periods** by annual taxable supplies: monthly above $6M, quarterly
  above $1.5M, annual otherwise (optional shorter); due one month after a
  monthly/quarterly period ends. (From memory of CRA guidance; confirm.)
- **Input tax credits** are the GST/HST paid or payable on inputs used in
  commercial activities, claimable in the period or later within the time limit
  (generally four years; confirm for the pilot). Documentary requirements by
  the total on the document: under $100 supplier name, date, total; $100-$499.99
  plus the supplier's registration number; $500 and over plus the recipient's
  name, terms and a description of each supply (Input Tax Credit Information
  (GST/HST) Regulations, s. 3). Some ITCs are restricted (50% of meals and
  entertainment, passenger vehicle limits), reported as line 104 adjustments or
  claimed at the restricted amount.
- **Imports**: GST/HST on goods is collected by the CBSA at the border and is
  claimed as an ITC from the customs documents; imported services and
  intangibles used less than 90% in commercial activities are self-assessed on
  line 405.
- **Quebec**: a Quebec registrant files GST/HST and QST together with Revenu
  Québec (FPZ-500), online since 2024-01-01; QST input tax refunds (ITRs)
  mirror ITCs. I have not found the FPZ-500 line list publicly (Revenu Québec
  does not post the form), so its boxes need the tax specialist.
- **Foreign currency**: amounts are converted at the Bank of Canada rate on the
  day the tax became payable, or another method the CRA accepts if used
  consistently (Excise Tax Act s. 159, as I recall it; confirm).
- **Rounding**: the CRA page states only that line 101 on a paper return is in
  whole dollars. I did not find an authoritative rule on rounding tax per line
  vs per invoice; the tax specialist confirms.
- No e-invoicing mandate (07 §7.6, T65).

**United Arab Emirates.**
- VAT 5% standard rate (Federal Decree-Law No. 8 of 2017), zero rate (exports,
  international transport, some education, healthcare, first supply of
  residential buildings, investment precious metals, crude oil and natural gas),
  exempt (some financial services, residential leases after the first supply,
  bare land, local passenger transport), and out of scope. Amended by Federal
  Decree-Law No. 16 of 2025 from 2026-01-01: **no self-invoice** is issued for
  the reverse charge any more (supporting documents are kept), and input tax may
  be denied where the recipient knew or should have known of evasion. A five-year
  limit on refunds is reported by commentators; its exact scope I could not
  confirm.
- **VAT 201 boxes** (FTA return guide; secondary sources agree, but the guide
  predates EmaraTax, so the live portal's labels must be checked): 1a-1g
  standard-rated supplies (amount, VAT, adjustment) **by emirate** (Abu Dhabi,
  Dubai, Sharjah, Ajman, Umm Al Quwain, Ras Al Khaimah, Fujairah) of the
  registrant's fixed establishment, 2 tourist refunds, 3 supplies subject to the
  reverse charge (imported services, goods imported without a customs
  declaration), 4 zero-rated supplies, 5 exempt supplies, 6 goods imported into
  the UAE (pre-filled from customs declarations), 7 adjustments to goods
  imported, 8 totals; 9 standard-rated expenses (amount, recoverable VAT,
  adjustment), 10 supplies subject to the reverse charge (recoverable VAT), 11
  totals; 12 total due tax, 13 total recoverable tax, 14 payable (or refundable)
  tax. Filed on EmaraTax within 28 days of the period's end; periods are
  generally quarterly, monthly for some.
- **Reverse charge**: imported services and goods (Box 3/6 output, Box 10
  input); domestic reverse charge between registrants for gold and diamonds
  (widened to precious metals and stones by Cabinet Decision No. 127 of 2024
  from 2025-02-25), crude oil and natural gas, and electronic devices (mobile
  phones, smartphones, computers, tablets and their necessary parts, Cabinet
  Decision No. 91 of 2023, from 2023-10-30) where the registered recipient
  resells or uses them in production; the supplier keeps the recipient's
  declaration.
- **Designated zones** (Cabinet Decision No. 59 of 2017 lists them; Executive
  Regulation Art. 51): certain supplies of goods within or between zones are
  outside the UAE for VAT, unless the goods are consumed; services are taxed
  normally; goods leaving a zone for the mainland are imports. For the ERP this
  is a tax code (out of scope, designated zone) chosen by a person or a rule,
  plus the recipient's statement kept as evidence.
- **Tax invoices** (Executive Regulation Art. 59): "Tax Invoice", the supplier's
  and (registered) recipient's names, addresses and TRNs, a sequential number,
  issue and supply dates, descriptions, quantity, unit price, the rate and tax
  per supply in AED, discounts, gross and tax payable in AED, the reverse charge
  statement where it applies; a simplified tax invoice where the recipient is
  not registered, or is registered and the supply is AED 10,000 or less. For
  T61 this is what the **input** check needs from a captured invoice (TRN,
  tax in AED); issuing invoices is phase 3 and e-invoicing is T65.
- **Input tax**: recoverable in the period the tax invoice is received, provided
  the supplier is paid or intended to be paid within six months of the agreed
  date (VAT Law Art. 55, as I read it; confirm); blocked for entertainment,
  motor vehicles available for personal use and employees' personal goods and
  services (Executive Regulation Art. 53); apportioned where the business also
  makes exempt supplies.
- **Foreign currency**: AED at the UAE Central Bank rate on the date of supply
  (VAT Law Art. 69, as I recall it; confirm).
- **Corrections**: an error in a filed return whose tax effect is AED 10,000 or
  less may be corrected in the next return; above, a voluntary disclosure (Tax
  Procedures Executive Regulation, as I recall; confirm, the procedures law was
  also amended for 2026).
- **E-invoicing** is mandatory from 2027-01-01 for businesses with revenue of
  AED 50m or more (pilot from 2026-07-01, voluntary adoption allowed; smaller
  businesses later), through accredited service providers on a Peppol-based
  five-corner model (Ministerial Decisions No. 243 and 244 of 2025). That is
  T65; it matters here because the e-invoice carries the tax category and tax
  per line that T61 determines.

## Decision

### 1. Scope of T61
In: effective-dated tax codes with components; calculation of input tax on
supplier invoices, down payments, cash discounts, credit memos and manual
entries; output tax on entries that bring in the pilots' sales (decision P2);
reverse charge and self-assessment; recoverable and non-recoverable tax; a tax
item per code written with every entry; tax registrations and tax periods;
returns computed from the ledger with boxes per country (GST/HST lines, VAT
201), their review, approval and recording as filed; a connector port for an
external engine.

Out: issuing sales invoices (phase 3); e-invoicing and the UAE's FTA Audit File
and other statutory reports (T65); monitoring law changes and proposing new
pack versions (T71); US sales tax and other jurisdictions (an engine, when a
country needs one); withholding tax (done, T46e/f); customs (phase 4, GTS);
electronic filing to the CRA or EmaraTax (decision P3); partial-exemption
apportionment beyond a fixed recoverable share (decision P8).

**Boundary with T65.** T61 decides the tax (code, category, rate, base, amount,
jurisdiction, reverse charge flag) and keeps it on the tax items; T65 renders
and transmits documents (PINT AE e-invoices via an accredited provider, received
e-invoices fed to capture) and produces statutory reports other than the sales
tax return. T65 reads T61's tax items and codes; T61 never calls a transmission
provider. Each UAE tax code carries its UN/CEFACT tax category (S, Z, E, O, AE)
from T61a so T65 needs no second mapping.

**Boundary with T71.** T61 defines the pack schema for tax codes, rates and
return definitions; T71's agent proposes new pack versions in that schema
(ADR-0048), which still go through release, sign-off and the change request.

### 2. In-house determination and calculation; an engine behind a port
**Recommendation: build determination and calculation in-house for the pilot
countries, behind a `Calculator` port, and choose no external provider now.**

Reasons:
- **The pilots' tax is simple to calculate.** Canada is a handful of federal and
  provincial rates with dated changes; the UAE one rate and a few categories.
  The country packs already hold them with sources and a tax specialist's
  sign-off. Avalara and Vertex earn their price on US sales tax (thousands of
  jurisdictions, product taxability, exemption certificates), which is phase 3
  at the earliest.
- **Posting stays inside one transaction.** Every posting runs in the unit of
  work and every external effect goes through the outbox (ADR-0007, ADR-0012).
  A synchronous call to an engine while the posting holds its locks would add a
  network dependency, a latency budget and a failure mode to every invoice; a
  call outside it would need a quote stored on the document and a commit call
  afterwards (both engines work this way), which is the port's shape below, not
  something the pilot needs.
- **Data and residency.** An engine receives every invoice line with partner
  data; for the UAE tenant it would be a new processor outside the cell's region
  (ADR-0003, ADR-0036's redaction discipline). Not a blocker, but a cost.
- **Returns are the hard part**, and engines do not file Canadian or UAE returns
  from our ledger; the return engine (§7) is needed either way.
- **SAP parity.** SAP calculates in-house with the condition technique and
  offers external engines for US jurisdictions only through its tax interface.
  We mirror the in-house half: a **tax procedure** per country (SAP `TAXCA`
  for Canada, with jurisdiction codes; the UAE's procedure, `TAXAE` as far as I
  know), **tax codes** whose **components** are SAP's condition types with an
  **account key** (SAP `MWS` output, `VST` input, `NVV` non-deductible to the
  expense line, `ESA`/`ESE` self-assessed output and input), and a
  **jurisdiction** per component (SAP `TXJCD`: the province or emirate). The
  tax item (§5) is SAP's `BSET`; the return selection by "not yet reported" is
  RFUMSV00's; the box mapping is Document and Reporting Compliance's.

**The port.** `tax.Calculator` takes a document (company, partner country and
region, ship-from/ship-to, tax date, currency, lines with base, code or tax
class, quantity) and returns tax per line and component. The built-in
calculator is pure Go (no database), in `internal/tax`, like `internal/money`
and `internal/countrypack`. An external connector (later) would be called when
a document is parked or captured, outside the posting transaction, its quote
stored on the document with the engine's reference, and posting uses the stored
quote (and refuses a stale one); the commit to the engine goes through the
outbox. The choice of provider is recorded when a country needs one (decision
P1).

### 3. Tax codes: components, effective-dated rates, categories
`config.TaxCode` grows (T61a), backwards compatible (a code with one
`rate_percent` and no `rates` reads as one component valid from the company's
first fiscal period, as today):

```yaml
tax_codes:
  - code: I5                 # BC purchase: GST + PST
    description: GST 5% + BC PST 7%
    direction: input         # input | output
    kind: standard           # standard | zero | exempt | out_of_scope | reverse_charge | import
    category: S              # UN/CEFACT 5305 for e-invoicing (T65)
    components:
      - key: gst             # the return boxes map components, not codes
        jurisdiction: CA
        account_key: input   # input | output | non_deductible | self_output | self_input
        recoverable_percent: "100"
        account: "1180"
        rates:
          - {valid_from: 2026-01-01, rate_percent: "5"}
      # an output code may levy its lines' subdivision instead:
      # jurisdiction: AE, jurisdiction_on_line: true (T61f)
      - key: pst_bc
        jurisdiction: CA-BC
        account_key: non_deductible   # added to the base line's account
        recoverable_percent: "0"
        rates:
          - {valid_from: 2026-01-01, rate_percent: "7"}
```

- **As built (T61a, migration 000124):** `ledger.tax_code_definition` is the
  code (description, direction, kind, category), `ledger.tax_component` its
  components (jurisdiction, account key, recoverable share, account, and the
  former accounts whose lines are still its tax lines) and `ledger.tax_rate`
  the components' dated rates (`valid_from`, exclusive `valid_to`, one per
  component and date, a GiST exclusion per component over the live rows). A
  code is valid on a date when one of its components has a rate then. The old
  `ledger.tax_code` (one row per validity, one rate and account) was copied
  into them, each code one component `tax`, and is no longer read or written;
  a later release drops it (contract, with T61c: dropped by 000145). Rates in the future are
  written when the configuration is activated, so a known change (NS HST 15%
  to 14%) is set up once, ahead. The "changed today" refusal goes: a rate's
  dates are explicit, a different rate is a new row (a rate's value and start
  never change; one the document no longer has is withdrawn), and the
  **database** refuses withdrawing a rate, or moving its end, past a posting
  of its code, and a new rate over dates already posted with its code (409
  `ledger.tax_rate_in_use`), so the past is never re-rated. Until T61c's tax
  items named the rate they applied, "posted" was a journal line with the
  code by posting date; since T61c it is a tax item of the code by tax date; every posting holds its codes' definitions `FOR KEY SHARE`
  and the setup locks them `FOR UPDATE` first, so a setup waits for postings
  in flight and then sees them, and a posting checks its codes' rates only
  after taking the lock. A tax amount a context worked out before posting
  (payables' down payment share) can still rest on a rate a setup replaced
  meanwhile; T61c's tax items, which name the rate applied, close that. A code's direction and a component's
  account key never change (409 `ledger.tax_code_fixed`). The single-rate form
  keeps its meaning: in force from the first fiscal period, a changed rate
  from the activation day, a removed code's rates ending that day; but such a
  change is now refused on a day the code already has a posting dated that
  day or later (it would re-rate it), where it used to pass. Codes copied
  from `ledger.tax_code` have the component `tax`: moving one to the dated
  form keeps that key, since another key would add rates over posted dates.
  **Submission predicts the refusal** (T61a follow-up): `ledger.Setup.Check`
  plans the change as the activation would, on the day of submission, and
  reports each component whose planned withdrawal, moved end or new rate
  covers a posting of its code (422 finding on
  `companies[…].tax_codes[…].components[…].rates`, or on the code's
  components or the list when the proposal removes the component or code,
  naming the code's last posting), so a change request that would fail with
  409 at activation is refused before two approvers approve it. A change in
  the single-rate form, or a removal, starts "today", so one refused for a
  posting dated today passes when submitted after it. A posting made, or a
  later day of activation, between submission and activation can still make
  the activation refuse what passed; the database stays the rule.
- **As built (the activation day is the company's date; product owner
  2026-10-31):** "today" at a tax setup, the day a changed or re-added
  single-rate code's new rate starts, the day a removed code's rates end,
  the rate in force kept open, and a new code's start when the calendar has
  no period, is the company's date in its `companies[].time_zone` at the
  activation (ADR-0051 as built: `clock.DayIn`, one instant per company,
  shared with the controlling checks), no longer the database's UTC
  `current_date`: at 22:00 on 16 April in Toronto (02:00 UTC on the 17th)
  a change starts on the 16th, at 01:00 on 17 April in Dubai (21:00 UTC on
  the 16th) on the 17th. The submission's prediction plans on the same
  date and its finding names it and the zone ("it is 2026-04-16 (the
  company's date in America/Toronto)"); the activation's 409
  `ledger.tax_rate_in_use` adds the date it dated the change on, as the
  rates themselves are not in a response. A stored zone the binary cannot
  load fails the activation rather than dating the change in UTC; the
  submission leaves the rates unchecked then, as validation reports the
  zone (`companies[<code>].time_zone`), as it does the controlling checks.
- **Operational constraint (accepted, coordinator 2026-10-21):** a tax code
  change is activated only after a rolling upgrade to T61a has completed on
  every instance. Until then an instance of the previous release still reads
  and writes `ledger.tax_code`, which T61a no longer maintains, so a rate
  activated meanwhile would be invisible to it (it would post with the old
  rate) and its setup would not write `ledger.tax_rate`. The runbook for the
  release says so; no code enforces it.
- A rate change is **posting-relevant** (tier 3, two approvers), as rate
  changes were: the diff carries no date, so a change only of future rates is
  not told apart (a lower tier for those would need the date of activation in
  the diff; not built). **Accepted** (coordinator 2026-10-21): future-only rate
  changes stay tier 3. The payables simulation of rate changes (07, T22)
  compares the rate in force on each replayed invoice's date, per component.
- Until T61d posted every component, supplier invoices and down payments took
  only a code with one component on an account of its own, wholly recoverable;
  since T61d supplier invoices take every input code, and down payments still
  take only such a code (422 `payables.tax_code_components`,
  `ledger.tax_code_components`), and an input code (`payables.unknown_tax_code`, `procurement.unknown_tax_code` for
  an output code).
- Country packs (ADR-0048, amended) write all their rates; the "later rates
  noted" note and the re-application after a change are gone. T62f (the UAE
  pack) and T62e's tax codes are written in this schema.
- Validation adds: components unique per code; a `non_deductible` component has
  no account of its own; `self_output`/`self_input` come in pairs on a
  `reverse_charge` or `import` code; recoverable 0-100% with three decimals;
  rates as today (0-100%, three decimals; the database's `rate_pct < 100` stays).

### 4. Which rate applies: the tax date
Every taxable document has a **tax date** (SAP's tax reporting date), which
decides the rate and the return period; the posting date keeps deciding the
fiscal period.

- Supplier invoices and credit memos: the invoice (document) date by default,
  which is when Canadian tax generally becomes payable and close to the UAE's
  date of supply; editable by a person within the open return periods (decision
  P6). A credit memo or subsequent debit naming an invoice uses the **named
  invoice's tax date and rates**, so a credit after a rate change credits the
  tax charged (the NS change of 2025-04-01 is the test case).
- Down payments: the payment date (the tax point of a prepayment in both
  countries), as the run date already decides validity (T46).
- Manual and imported entries: given, defaulting to the posting date.
- **Open documents when a rate changes**: nothing stored is re-rated. A parked
  or captured invoice is calculated when it is posted, at its tax date; if the
  supplier's stated tax then differs beyond tolerance, posting is refused (§6)
  and a person decides. A purchase order's tax is an estimate shown, never
  posted. A down payment request's stated tax is checked at the run date's rate
  as part of the proposal (today it checks only validity), and a request whose
  tax no longer matches is left out with `tax_code_invalid` (its existing
  reason). Reversals negate the original's tax items exactly.
- **As built (T61d follow-up, migration 000180):** the proposal checks a
  down payment request's tax with `payments.checkTax`: a request with a code
  whose code is valid on the run date as an input code of one component is
  paid only if its stated tax is the tax its amount includes at the run
  date's rate (`tax.Included`, amount × rate / (100 + rate) rounded once,
  the calculation the request itself uses, moved there from payables);
  otherwise, and when the code is no longer valid and the request states
  tax, it is left out with `tax_code_invalid`, its exclusion naming the code,
  the code's rate on the request's due date (the rate its tax was
  calculated at; not stored on the request, so read again, which a rate
  edited before any posting used it would change) and the run date's rate
  (`request_exclusion.tax_code`, `request_tax_rate`, `run_tax_rate`, absent
  when not valid; `GET /v1/payment-runs/{id}/exclusions` returns them as
  `tax_code`, `request_tax_rate_percent`, `run_date_tax_rate_percent`). A
  request with a code that is not valid and states no tax posts none and is
  paid, as before. A proposed run whose request's rate changed after the
  proposal is refused at release with 409 `payments.proposal_outdated`
  naming both rates. Exact equality is the test: no tolerance applies to
  a request, which states the tax the ERP calculated.

### 5. The tax item: one per entry, code and component
The ledger writes a `ledger.tax_item` with every entry that carries a tax code
(T61c), in the same transaction, insert-only like the journal (ADR-0006):
entry and line, tax code, component, jurisdiction, direction, kind, account
key, tax date, rate applied, base and tax in transaction and local currency,
the recoverable and non-recoverable parts, the partner's tax id fingerprint
(not the id, ADR-0036), and `reported_in` (the return that reported it, set
once). Reversals write negated items. Returns read items, never journal lines,
so a base that is not on a tax line (zero-rated, exempt, reverse charge with
nothing posted) is still reported, and a cash discount's tax correction (T36d)
writes items too. A schema test enforces that every tax line has its item and
that items balance with the tax lines per entry and account (the ledger's
commit-time check, like the balance trigger).

- **As built (T61c, migrations 000144-000145):** `ledger.tax_item`, keyed by
  entry and item number, `tenant_id` first, RLS, fixed but for `reported_in`
  (set once, `tax_item_fixed`; the application may update that column only).
  An item names its code, component, jurisdiction, direction, kind, account
  key, the tax line's account (instead of a line: a component's tax may be on
  several lines, and a line carries one code), the tax date, the rate row
  applied (`tax_rate_id`) and its percentage, the recoverable share, base,
  tax and recoverable part in transaction and local currency; the
  non-recoverable part is derived (`TaxItem.NonRecoverable`). **Written on
  every posting path** in the posting's transaction, by the one insert all
  paths share (`post`): from the entry's lines by default
  (`ledger.write_tax_items`: per code its lines carry and per component with
  a rate on the tax date, the tax the lines on the component's account, the
  base the code's lines on no account of its components, once per
  component); a reversal writes the original's items negated at their tax
  date; a down payment transfer the down payments' items negated at its own
  tax date; a cash discount's correction one item per item of the paid
  entries with a share, at their codes and rates and the payment's tax date,
  its local amounts the converted lines' shared out (`money.Allocate`). For
  the base to be reported, a down payment's net and an invoice's GR/IR and
  price difference lines now carry the code (as invoice expense lines did),
  and the discount's net that is a code's base is posted on `cash_discount`
  with the code, the untaxed rest without one. The cash discount split reads
  the paid entries' items, so it needs no account history
  (`former_account_ids` is read only by 000144's migration of the entries
  posted before, which writes their items from their lines at their posting
  date, former accounts included), and allocates with `money.Allocate`.
  **Commit-time check** (`journal_entry_tax_items`, `tax_item_checked`,
  deferred): each code the lines carry has an item (`tax_item_missing`),
  items are of the entry's legal entity (`tax_item_entity`), and per account
  and code the lines on an item's or a component's account add up to the
  items' tax in both currencies (`tax_item_lines`). **The tax date** is on
  the entry (`journal_entry.tax_date`, also on parked entries and in the API,
  `tax_date`): a manual entry's as given, else its posting date; a supplier
  invoice's the one a person set, else its invoice date, or for a credit
  memo naming an invoice that invoice's (P6, T61d §6); a reversal's the original's; other postings' their
  posting date. Codes are validated on the tax date. The in-use check of
  rates (`ledger.tax_code_posted`) reads items by tax date, and `ledger.tax_code`
  and the index of lines by tax code are dropped (000145). `ledger.TaxItems`
  is the port: `Of` an entry, `Dated` by legal entity and tax date range.
  **Parallel ledgers (ADR-0045):** tax items are the leading ledger's
  entry's alone. Returns are statutory and read the leading ledger; a
  posting's copy in a parallel ledger carries the same tax lines and has
  no items, which would count the tax twice. The commit-time check holds a
  parallel ledger's entry to having none (`tax_item_leading`), and the
  migration writes items for the leading ledger's entries only. **A posting
  that leaves out the leading ledger may carry no tax code** (product owner,
  2026-10-21, as SAP is often configured), so tax only ever comes from
  postings the returns see: a manual entry to a parallel ledger, or to a
  ledger group without the leading ledger, with a taxed line is refused
  (422 `ledger.tax_code_off_leading`), and the commit-time check refuses a
  parallel ledger's entry with tax codes that is no copy of a leading
  ledger's entry in its posting group (`tax_code_off_leading`). A ledger's
  difference on tax is posted without a code. Entries posted to a parallel
  ledger alone before the rule keep their codes; reversing one fails the
  commit-time check (409 `request.rule_violated`; none exist before the
  pilot).
  A tax line names its account and code, not its component, so the ledger
  refuses on a posting that derives its items from its lines a code two
  of whose components with a rate on the tax date post to one account (422
  `ledger.tax_code_components`; the database's `tax_item_lines` is the
  backstop); a posting that gives its items (a supplier invoice, T61d) may
  carry it. A code that posts to no account
  (non-deductible only) has no item and is not required to have one. A
  reversal of an entry without items (such a code) takes its items from its
  own lines. An entry posted before T61c has the items of the components
  rated on its posting date, the date its codes were validated on, so each
  code it carries that posts to an account has one, and its reversal posts. **Rolling upgrade (06 §6.4):** an instance of the previous
  release writes neither tax dates nor items; the commit-time check skips
  an entry without a tax date, the rate check does not see such postings
  (T61a's constraint holds: tax code changes wait until the rollout is
  complete), and the next release writes their items (tracker, T61c
  follow-up). The migration of the entries posted before writes their items
  in the migration, one statement, not as a batched operation: pre-pilot
  volumes (decision recorded for the coordinator). A reversal posted before
  T61c has items from its own lines at its posting date and rate, not the
  original's negated. **Not yet:** a derived item set (a manual entry's)
  has no item for a non-deductible component, which has no line of its
  own (a supplier invoice gives one, T61d); the partner's tax id
  fingerprint is added with the returns that read it (T61g); a manual
  entry's tax date is not yet held to the open return periods (P6; with the
  return periods, T61g/T61i); local amounts are the lines', converted at the
  posting date's average rate as every posting is, not yet at the
  authority's rate type on the tax date (§8: T61d's calculation converts at
  it).

### 6. Input tax (supplier invoices, capture, down payments)
- Items may carry different codes; the invoice's tax is calculated per code
  (§8) and **posted as one tax line per code and component**, replacing
  "the first item's code". A `non_deductible` part is added to the base line it
  belongs to (SAP `NVV`), so a BC PST purchase expenses its PST on the item.
- The supplier's stated tax (entered, or captured from the PDF) is compared with
  the calculated tax per document: within a tolerance from the company's
  configuration (default: one minor unit per tax code), the **supplier's
  amount** is posted and the difference allocated to the codes (input tax is
  what was charged); beyond it posting is refused with 422
  `payables.tax_mismatch`, naming each code's calculated and stated tax
  (decision P5). The capture agent proposes codes and flags mismatches; it
  posts nothing (ADR-0036).
- Documentary checks for recovery (Canada's $100/$500 tiers, the UAE's TRN
  on a tax invoice above AED 10,000) are warnings on the invoice and a filter on
  the return's working papers, not refusals: the business may still post and
  decide not to claim. The supplier's tax id comes from `partner.tax_identifier`.
- A recoverable share below 100% (meals 50%, blocked UAE input) splits each item
  into recoverable (on the tax account, in the return's input box) and
  non-recoverable (on the expense).
- **As built (T61d, migration 000146):** payables calculates an invoice's tax
  when it posts (`calculateTax`, `internal/tax`) per item code at the tax
  date: `supplier_invoice.tax_date`, set by a person only (403
  `payables.tax_date_human_only` for an agent that sets, changes or clears
  it; an agent's change that keeps it passes), else the invoice date, else,
  for a credit memo or subsequent debit or credit naming an invoice, that
  invoice's, so it is calculated at its rates. One tax line per code and
  component with an account carries the input tax recovered (or a self-assessed
  output tax); a non-deductible component's tax and the part of a
  component's tax not recovered (a self-assessed claim's too) go on the
  item's line (for an item on a
  GR/IR order, into what it invoices, so onto the price difference). A
  reverse charge code posts its self-assessed pair, the supplier owed the
  base only. The supplier's stated tax is held to the calculated within
  `companies[].invoice_tolerance.tax_units_per_code` minor units per code
  that charges tax (default 1): within it the stated amount posts, its
  difference shared out (`money.Allocate`) among the components putting
  tax on an account, in proportion to what they put there (an input
  component's recovered part), and recovered; with
  none, among those whose tax is a cost (non-deductible, or recovering
  none), onto their items' lines (a BC PST invoice's stated PST is its
  cost). Beyond the tolerance, with no component to take it, or where it
  would recover more than a component's tax or turn its sign, 422
  `payables.tax_mismatch` names the calculated tax per code; tax out of
  range is 422 `payables.amount_too_large`. The ledger writes the calculated items as given
  (`Posting.TaxItems`, with the component's account and the rate of the tax
  date), and keeps the tax lines' local amounts at the tax date's rate
  (`PostingLine.LC`): the other lines are converted at the posting date's
  rate as ever, and what that leaves is an exchange difference on
  `fx_gain`/`fx_loss`. The rate is the average rate of the tax date; the
  authority's rate type a country pack would name is not yet in the pack
  schema, nor its `rounding` (per document, the default). An invoice whose
  items carry no code posts its stated tax on `input_tax` as before, without
  items. What a tax line posts on its account is what the ledger's check
  holds the items to: the output tax, or the input tax recovered (T61c's
  derived items recover all their account's tax). The capture reads each
  line's printed tax rate (`capture.Line.TaxRate`): a rate its order item's
  code charges keeps it; a rate one input code charges on the tax date
  proposes it (a warning in the operation's result); none, several or no
  number hold the invoice for a person to choose; and a stated tax the
  codes do not bear out holds it with the figures (eval suite capture,
  version 3). **Rolling upgrade (06 §6.4):** 000146 adds a nullable
  column the previous release neither reads nor writes (its statements name
  their columns), so it runs ahead of the code; but an instance of the
  previous release posts an invoice with the first item's code at the
  invoice date, so invoices are not posted while the rollout is under way
  (a step for the release notes). **Not yet:** down payment requests' tax checked at the run
  date's rate, the documentary-requirement warnings for Canada and the UAE
  (tracker, T61d follow-up), and the tax date held to the open return
  periods (P6; with the return periods, T61g/T61i). The first two were
  built in the T61d follow-up (below and §4).
- **As built (T61d follow-up, migration 000180):** the documentary
  requirements are the company's `tax_settings.documentary_requirements`
  (config.TaxSettings), which the country pack's `tax.yaml` brings under
  `tax_settings` (§8): tiers, each with a threshold in the company's
  currency, `at_least` (a total equal to it is in the tier: Canada's "$100
  or more") or `above` (the UAE's "above AED 10,000"), what it `requires`,
  its `source` and `pending_sign_off` for a tier the tax specialist has not
  confirmed (a pack signed off holds none). The ERP checks what it holds:
  `supplier_tax_id`, met when the supplier's business partner holds a tax
  identifier (any scheme), and `item_descriptions`, met when every item
  without an order has a description (an order item is described). When an
  invoice or subsequent debit claiming tax posts, its gross amount,
  converted into the company's currency at the tax date's rate of the tax
  rate type, is held to each tier; each unmet requirement of a tier it
  reaches is a warning, once (the first tier naming it), stored on the
  invoice (`supplier_invoice.documentary_warnings`) and returned as the
  invoice's `documentary_warnings` (code `supplier_tax_id_missing` or
  `item_description_missing`, the tier's threshold as money, whether it is
  inclusive, the source, the pending flag and a message). Credit memos and
  subsequent credits take tax back and are not checked; an invoice posted
  before has none. Canada 2026.3 states SOR/91-45 s. 3 (b) and (c) as two
  tiers (from CAD 100 the supplier's registration number; from CAD 500 also
  a description of each supply); the recipient's name and the terms, also
  required from $500, are on the document only and not checked. The UAE
  pack (T62f) is to state the threshold as `above: "10000"`, requiring
  `supplier_tax_id`, `pending_sign_off: true` (Executive Regulation Art. 59
  is read only through secondary summaries). The working papers' filter on
  these warnings comes with the returns (§11).


### 7. Output tax and the pilot's sales
The ERP issues no sales invoices until phase 3, so the pilot's output tax
enters as **journal entries with output tax codes** (T61f): manual entries and,
for volume, entries posted by the pilot's billing system through the existing
journal-entry API (`source` the integration, idempotent by its reference). The
ledger calculates the tax of each output base line from its code at the tax
date and checks the entry's tax lines against it within the tolerance; a line
carries its jurisdiction (province or emirate) when the code's components need
one. Zero-rated and exempt bases carry their codes with no tax line, so lines
101 and boxes 4/5 see them. How granular the pilot's import is (one entry per
sales invoice or a daily summary per code and jurisdiction) is decision P2.
- **As built (T61f, migration 000150):** a manual or integration entry
  (`POST /v1/journal-entries`, idempotent by its `Idempotency-Key`; parked
  ones alike) posts output codes on its base lines (the sales, credits) and
  tax lines (on the code's component accounts). The ledger calculates each
  output code's tax over its base lines, per jurisdiction, at the rates of
  the tax date (`checkOutputTax`, `internal/tax`, rounded once per code and
  component), and holds the tax lines on each component's account to it
  within one minor unit per base line (422 `ledger.output_tax_mismatch`
  with the figures): a billing system rounds each invoice or line it sums,
  the calculator the sum once, so four invoices of 100.10 at 5% post 20.04
  where the sum gives 20.02. **Decided (product owner, 2026-10-22):** a
  tax summary from a billing system carries a base line per invoice it
  sums, and the tolerance stays one minor unit per base line; a summary
  of many invoices on one base line is refused when its rounding exceeds
  that (the pilot's billing integration is built to it, no count of
  invoices is added to the contract). A code with tax lines and no base
  line is an adjustment of tax alone, recorded as posted (base 0). Where a component levies its lines'
  subdivision (`jurisdiction_on_line`, its jurisdiction the country: one
  UAE code for every emirate, VAT 201 boxes 1a-1g), every line with the
  code names it (`tax_jurisdiction`, ISO 3166-2, 422
  `ledger.jurisdiction_required`), a subdivision of that country, and no
  other line names one (422 `ledger.jurisdiction_not_levied`; database
  trigger `journal_line_tax_jurisdiction_code`, which a reversal passes, as
  it repeats the original's); the items are then per jurisdiction, the
  line's on the component that levies it and its own on the others. A
  component has one rate, so it levies its lines' subdivisions only where
  they share it (the UAE's emirates); provinces with rates of their own
  keep codes of their own. A code
  of a province's own (Ontario's HST, `CA-ON`) needs none. Zero-rated and
  exempt bases carry their codes without tax lines and are items at rate 0
  (GST34 line 101, VAT 201 boxes 4/5). The posted event's lines do not
  carry the declared base or the jurisdiction yet (tracker); nor do
  postings that build lines of their own (a cash discount's tax
  correction, a down payment's transfer, `ledger.Posting`): all are
  supplier-side today, and a customer cash discount on such a code will
  need it. **Rolling upgrade (06 §6.4):** 000150 is additive and runs ahead
  of the code; an instance of the previous release posting a code that
  levies its lines' subdivision fails the trigger (409), and its parked
  line upserts leave `tax_jurisdiction` as it was, so
  `jurisdiction_on_line` is configured only once the rollout is complete,
  and an entry parked before T61f with output tax lines off its base is
  refused when approved (release notes). An integration posting sales
  entries is held to the company's manual entry approval limit like a
  person today (409 `ledger.approval_required` above it), which a billing
  system cannot answer. **Decided (product owner, 2026-10-22), planned as
  T61n:** service principals get a posting limit of their own per
  company, and an entry above it is parked for a person's approval
  rather than refused.

### 8. Calculation and rounding
- Base × rate is exact (`big.Rat`, as `money.Convert`), rounded **once per
  document, tax code and component**, half away from zero, in the transaction
  currency, then allocated to the lines by largest remainder, so line tax adds
  up to document tax (the method T36d and T46f already use). A pack may set
  `rounding: line` for a country whose law or practice wants per-line rounding;
  neither pilot is known to (see Context; to be confirmed by the specialist).
- Tax-inclusive bases (a captured gross, a down payment) are split as T46 does:
  tax = amount × rate / (100 + rate), rounded once.
- Local currency: the tax in local currency is the tax converted at the rate of
  the tax date, of the rate type the country pack names (Bank of Canada; UAE
  Central Bank), rounded once per item; the ledger already stores the
  conversion (ADR-0005). Which rate type the ledger has for these authorities
  is checked in T61c, with T48's document currency: as built, an item's local
  amounts are its lines', at the posting date's average rate; the
  authority's rate on the tax date comes with T61d's calculation.
- All amounts `money.Minor`; rates and percentages decimal strings as today.
- **As built (T61b):** `internal/tax` (imports only `money`) holds the
  `Calculator` port and `BuiltIn`, which calls the pure `tax.Calculate`. A
  `tax.Document` carries the tax date, the codes its lines name with their
  components' dated rates (from inclusive, until exclusive, the ledger's
  dates; one undated rate for a code already resolved on the tax date), an
  inclusive flag, the rounding mode (`document` by default, or `line`) and
  an optional conversion to local currency; the partner and place fields
  are for an engine and unused. The result is per line and per code and
  component (the items): base, tax, recoverable and non-recoverable parts,
  in transaction and local currency. Rules as built: rates are below 100%
  and recoverable shares 0-100%, both plain decimals; a zero, exempt or
  out-of-scope code's rates are 0; self-assessed components are exactly
  one `self_output` and one `self_input` at the same rate; an inclusive
  line's divisor is 100 plus the rates charged (self-assessed ones are not
  charged), its base the gross less the tax charged, and the self-assessed
  pair is on that base. The recoverable part is the tax × the share rounded
  the same way (once per item, allocated). Tax has the sign of the line's
  amount, except `self_output`, which is the negation of `self_input`, so a
  pair nets to zero on every line (on an inclusive document of tiny grosses
  every component's allocated unit can fall on one line, whose base then
  turns negative). The allocation is `money.Allocate` (largest remainder
  in the direction of the sum, so a negated document gives the negated
  result; each result between the floor and ceiling of its exact amount).
  Local currency: each item's base and tax converted once (exactly, then
  allocated to the lines, which add up to the item); a line's local
  recoverable part is its share of the line's local tax, rounded as the
  tax is, so 0 ≤ recoverable ≤ tax holds per line in both currencies.
  Determination (which code a line has) stays the caller's. The country
  pack's `rounding` setting is not yet in the pack schema: T61c/T61d add it
  with the first caller.
- **As built (T61d follow-up):** a pack's `tax.yaml` carries
  `tax_settings` (`rate_type`, `rounding`, `documentary_requirements`),
  validated as the document's are (thresholds in whole units there) and
  merged on application as one item, three-way like the pack's lists: the
  pack's taken where the company kept the adopted pack's, the company's own
  kept with a conflict note. They become `companies[].tax_settings`
  (tier 3). Payables calculates an invoice's tax with the settings'
  `rounding` (`document` by default, or `line`) and converts it into local
  currency at the tax date's rate of `rate_type` (`average` by default,
  `Rates.Conversion` as before; another of the ledger's rate types its
  latest rate on the tax date, 422 `ledger.no_exchange_rate` without one).
  No new rate type was added: the authority's rates (the Bank of Canada's,
  the UAE Central Bank's) are recorded under one of the ledger's types,
  which the pack names. Canada 2026.3 names `average` and `document`, both
  pending the specialist (the conversion and rounding rules are cited from
  memory above).

### 9. Reverse charge and self-assessment
A `reverse_charge` or `import` code has a `self_output` and a `self_input`
component: posting writes the tax payable (output) and the recoverable share
(input), and two tax items; the supplier is owed the base only. This covers
the UAE's imported services and goods without a customs declaration (Box 3 and
Box 10), the domestic reverse charge for electronics, precious metals and
hydrocarbons (a code the clerk or a rule chooses; the recipient's declaration
is evidence on the invoice), Canada's self-assessed imported services (line
405, recoverable share as ITC on 106) and the provincial part of HST on goods
brought into a participating province (405), and BC PST self-assessment where
the vendor charged none. **Import VAT/GST on goods** paid to customs (CBSA, UAE
customs) is posted from the customs declaration as an entry to the customs
broker or authority with an `import` input code, its base the declared value:
the UAE's Box 6 for registrants who account for import VAT in the return
(output and input), Canada's ITC on 106. From 2026 the UAE needs no self-invoice
(Federal Decree-Law No. 16 of 2025), so none is generated.
- **As built (T61e, migration 000147):** reverse charge and self-assessment
  post from supplier invoices of `reverse_charge` codes as T61d built them:
  the supplier is owed the base, the `self_output` tax is owed on its
  account and the `self_input` part recovered on its own, two items, and the
  part not recovered is a cost on the item's line. Worked cases (payables
  `TestSelfAssessment`): Canada line 405, imported services used 60% in
  commercial activities, HST 13% on 1,000.00: 130.00 owed, 78.00 recovered
  (line 106), 52.00 on the expense; BC PST self-assessed where the vendor
  charged none, 7% recovering nothing: 70.00 owed and expensed; the UAE's
  imported services and its domestic reverse charge, 5% on AED 4,000.00:
  200.00 owed (box 3) and recovered (box 10), netting to zero. The domestic
  reverse charge is a code the clerk chooses; the buyer's declaration is
  kept as the invoice's document, as any evidence is (no field of its own).
  **Import tax from a customs declaration** is a journal entry of tax lines
  alone (`POST /v1/journal-entries`, or parked first): each line of an
  `import` code on one of its components' accounts names the declared value
  as its `tax_base`, which no line of the entry carries
  (`journal_line.tax_base_tc`/`tax_base_lc`, the local base at the entry's
  rate; `parked_entry_line.tax_base`). The ledger refuses a base on any
  other line (422 `ledger.tax_base_not_import`, database trigger
  `journal_line_tax_base_import`), a zero one, and a line whose amount is
  not the tax on the base at the component's rate on the tax date (the
  part recovered, or the self-assessed output tax) within one minor unit
  (422 `ledger.tax_base_mismatch`); a code one line declares a base for
  has the same base on every line carrying it, and no base line beside it,
  which would count the base twice (422 `ledger.tax_base_inconsistent`).
  The component's item takes the base; a reversal or a parallel ledger's
  copy repeats it on the original's account, which the trigger accepts as
  a component's former account after the component moved on.
  Worked cases (ledger `TestImportFromCustomsDeclaration`): the UAE's box 6,
  AED 10,000.00 declared, 500.00 self-assessed on 240000 and recovered on
  140000; Canada's GST paid to the CBSA through a broker, CAD 13,000.00
  value for duty, 650.00 claimed (line 106) against the customs clearing
  account. A partly recovered import posts the part recovered on the input
  account and the rest on an expense line without a code; the entry's items
  are given (`importItems`, as a supplier invoice's are), so the claim item
  carries the whole tax at the rate and the part recovered, as a supplier
  invoice's self-assessed claim does: one meaning for an item on every path
  (product owner, 2026-10-21). An entry declaring bases carries its import
  codes alone (422 `ledger.tax_base_inconsistent`). A reversal
  negates the bases with the lines. **Rolling upgrade (06 §6.4):** 000147
  adds nullable columns and keeps `write_tax_items`' signature (a null
  base counts as none), so it runs ahead of the code; an instance of the
  previous release neither writes nor resets the bases: an edit of a parked
  import line there keeps its stale base, posting a parked import entry
  there drops the base (its items' base 0), and its reversals of new
  import entries leave the lines' bases out (their items, negated from the
  original's, are right). Import entries are not entered while the rollout
  is under way (a step for the release notes; no release runbook exists yet). **Not yet:** the base on the posted
  event (`journal_entry_posted` lines), and a customs declaration as a
  document of its own (GTS, phase 4).

### 10. Tax registrations and periods
A company has **tax registrations** in the configuration (T61g): authority
(CRA, Revenu Québec, a province, FTA), number (BN with program account RT0001,
QST number, TRN, a UAE tax group's TRN), the return it files (from the pack),
the frequency (monthly, quarterly, annual) and the first period's start. Tax
periods are generated from it, independent of fiscal periods (a quarterly UAE
period may start in any month the FTA assigned). A UAE **tax group** is one
registration across several legal entities, so a return may read several
companies (all in the tenant, under RLS). A registration change is financial
(tier 3).
- **As built (T61g, migration 000172):** the tax filing context is
  `internal/taxfiling` (schema `tax`): the package name `tax` is the shared
  kernel's pure calculator, and a context under `internal/tax/` would be
  part of the shared kernel, which may import no context. A company's
  `companies[].tax_registrations[]` has a `code` (unique in the tenant,
  `^[A-Z0-9_]{1,20}$`), `authority` (a code such as `CRA`, `RQ`, `FTA`;
  no list of authorities is held), `number`, `return_type` (the pack's code
  of the return; T61h checks it against the pack's `returns.yaml`, which
  does not exist yet), `frequency` (`monthly`, `quarterly`, `annual`),
  `first_period_start` (day 1-28 of a month, so every period starts on the
  same day; a start on the 29th-31st is refused for want of a rule for the
  shorter months) and optional `last_period_end` (the last day of one of
  its periods) and `group_members` (a UAE tax group: the other companies'
  codes, the registration listed under the representative member).
  Validation refuses a code twice in the tenant, a company covered by two
  registrations of one authority whose periods overlap, and one number
  (as fieldcrypt normalizes it) on two registrations of one authority
  whose periods overlap, so a return reports an item once (§11). The
  number must be printable ASCII (its last four characters are shown)
  of at most 40 characters without spaces; its format is not checked: no pack or primary source defines the
  BN/RT or TRN format here (a tracker item for the tax specialist). The
  registration is kept by `taxfiling.Setup` in `tax.registration`, the
  number sealed with the tenant's key, fingerprinted and shown by its last
  four characters (fieldcrypt, as tax identifiers and house bank accounts
  are, and re-sealed on key rotation). What a registration's periods and
  returns follow from (company, group, authority, number, return type,
  frequency, first period) never changes under its code: the setup's
  check names each changed field on submission, activation refuses it
  (409 `tax.registration_fixed`, read FOR UPDATE), and a trigger
  (`registration_fixed`) backs it up for every field but the number, which
  only the application can open. A changed frequency or group is a new
  code starting the day after the old one's `last_period_end`. A
  registration the document no longer lists becomes inactive, never
  deleted. Because the periods follow from fixed settings, they are
  computed on read (`config.TaxPeriods`), not stored: `GET
  /v1/tax-registrations` (`tax.registration.read`; a caller sees the
  registrations covering a company they may act in, by registrant or
  member; companies named by code) and `GET
  /v1/tax-registrations/{id}/periods` (paged by start date, up to
  `start_date_to` or the period under way on the registrant's date in its
  `time_zone`, which the answer names). T61i stores a return per
  registration and period start. Audit and events are the configuration
  activation's (`config.version.activate`, `configuration_activated`);
  the context has no outbox until returns move (T61i). Not built: due
  dates (the authorities' deadlines are facts for the specialist), period
  status (T61i), and the pilot's actual registrations and frequencies,
  which the pilot supplies.

### 11. Returns: computed from the ledger, boxes per country
- A **return definition** per country and return type is part of the country
  pack (`returns.yaml`, versioned and signed off with the pack): its boxes, each
  either a sum of tax items selected by direction, kind, component key,
  jurisdiction and measure (base, tax, recoverable tax, adjustment), or an
  expression of other boxes (109 = 105 - 108; 14 = 12 - 13), evaluated by the
  CEL engine of T58 (ADR-0046) with no tenant code. The return for the period
  selects items of the registration's companies with `tax date ≤ period end`
  and `reported_in` empty, so a late item lands in the next return (SAP's
  "already reported" flag), shown separately as a prior-period adjustment.
- **Lifecycle** (T61j): `draft` (computed on demand, never stored as truth) →
  `prepared` (a snapshot: box values, the ids and hash of every item, the
  definition's pack version; the items' `reported_in` set in the same
  transaction under a lock on the registration, so an item is reported once) →
  `approved` (a person other than the preparer, through the approvals context,
  role `tax_manager` or as configured) → `filed` (a person records the
  authority's confirmation number and date; P3) → `amended` by a further return
  of kind `amendment` for the same period, when a correction must not wait
  (above the UAE's AED 10,000 threshold, or the business's choice in Canada).
  A prepared return that is withdrawn releases its items. Payment or refund of
  the net tax is a payables-like item on the authority, paid by the payment run
  (a later step, not the pilot's must-have).
- **Period lock (T32).** Preparing a return needs every fiscal period its items'
  posting dates touch to be closed in the leading ledger (so no posting is under
  way), and recording it filed **locks** those periods if they are not yet
  locked, through the ledger's `:lock` (people only, T32). An item can
  still arrive later, dated in the filed period (an invoice received late, a
  reversal): posting into a locked period is refused by T32, so it is posted in
  an open period with its true tax date and reported in the next return as a
  prior-period adjustment; the tax date may lie in a filed period, the posting
  date may not. T64's close checklist gets "tax return prepared" as a task.
- **Working papers**: every box drills down to its items, entries and documents
  (T59's drill-through), and exports (T63) as CSV/PDF for the filing person and
  the auditor.
- **Adjustments** that are not documents (bad debt adjustments, instalments,
  rebates, ITC restrictions for the year) are entered as manual entries with a
  tax code of kind `adjustment` mapped to their box (104, 107, 110, 111; UAE
  Box 1 and 9 adjustment columns), so they too are items with a document.

### 12. Audit
Every change of a registration, code or rate is a configuration change request
(audit, approvals, simulation); every tax item is written with its entry (the
journal's audit); every return transition writes an audit record
(`tax.return.prepare|approve|file|withdraw`, ADR-0011) and an outbox event
(`TaxReturnPrepared`, `TaxReturnFiled`), and the filed snapshot is immutable
(a trigger, as released packs are immutable by hash). Retention follows the
longer of the countries' periods (Canada six years from the end of the year;
UAE five years, longer for real estate), set by T66.

### 13. Agents
Agents may: propose tax codes on captured and parked documents; prepare a draft
return and explain its variances against the previous period and against the
ledger's tax accounts; propose adjustment entries by parking them; propose
configuration changes (rates, registrations) as change requests, as packs are
applied (`x-autonomy: propose`). Agents may **never** approve, mark filed,
withdraw or amend a return, change a tax date, or lock a period: those
operations are human-only (403 `identity.agent_not_allowed`), whatever autonomy
T43 grants. The return endpoints carry `x-autonomy: none` for the transitions.

## Alternatives
- **An external engine now (Avalara AvaTax, Vertex O Series).** Covers Canada
  and the UAE and would carry rate changes for us, but adds a per-transaction
  cost, a processor outside the region, and a call on the posting path, and it
  does not produce our returns. Deferred behind the port (§2).
- **Rates on the configuration only, no tax items; returns from journal
  lines.** Less schema, but bases without a tax line (zero-rated, exempt,
  reverse charge) and the "reported once" rule cannot be represented, and every
  return would re-derive what posting knew. Rejected.
- **Returns as hard-coded Go per country.** Simpler for two countries, but a
  third country or a changed box (the VAT 201 changed with EmaraTax) would be a
  code release; the pack already carries sign-off and versioning. Rejected in
  favour of pack definitions with CEL expressions.
- **Tax as its own context owning codes and items.** Cleaner ownership, but tax
  lines are written with the entry under the ledger's locks and balance rules,
  and the ledger already owns `tax_code`. Chosen: the ledger keeps codes, rates
  and items; a new `tax` context (schema `tax`) owns registrations' periods and
  returns and reads items through a ledger port (`ledger.TaxItems`), with its
  interface in the ledger's published API (CLAUDE.md, structure); the pure
  calculator is `internal/tax`.

## Consequences
- Supplier invoices with several tax codes, GST+PST and non-deductible tax post
  correctly; the "first item's code" rule and ADR-0048's "one tax code per line
  until T61" go away.
- Every taxed entry writes more rows (one item per code and component); the
  returns never scan the journal.
- A rate known in advance is configured once; the past is never re-rated, so a
  rate correction after use is a new code or an adjustment, not an edit.
- The pilot's output tax depends on the pilot's billing system sending entries;
  until it does, the return lacks line 101/103 and Box 1.
- Packs gain `returns.yaml` and component schemas; T62f and T71 build on them.
- No return is ever filed by the system; the filed state is a person's record.

## Steps
Ordered so the pilot's must-haves come first; each ends green (lint, tests,
coverage 100%, reviewer) like every step.

| Step | Work | Acceptance criteria |
|---|---|---|
| T61a (done) | **Dated tax codes with components** (config + ledger) | `config.TaxCode` with direction, kind, category, components (key, jurisdiction, account key, recoverable %, account, dated rates); old one-rate codes still load; validation as §3; `ledger.tax_rate` with a GiST exclusion per component; setup writes future rates, never edits a used rate (409 `ledger.tax_rate_in_use`, enforced by the database), no "changed today" refusal; `Accounts.TaxCode` returns the components at a date; the payables rate simulation reads dated rates; the pack schema carries the components and `countrypack.Apply` merges them; schema tests for each database rule; ADR-0048 amended (packs write all rates, the note goes) |
| T61b (done) | **The calculator** (`internal/tax`, pure) | Exclusive and inclusive bases; per document, code and component rounding half away from zero with largest-remainder allocation (and `line` mode); recoverable split; reverse-charge pairs; LC conversion at a given rate; the `Calculator` port; table tests incl. NS 15→14%, BC GST+PST, UAE 5%/0%/exempt; fuzz test that line tax sums to document tax and recoverable + non-recoverable = tax |
| T61c | **Tax items in the ledger** | `ledger.tax_item` (insert-only, RLS, `tenant_id` first) written by every posting path that carries a tax code (journal entries, invoices, GR/IR, down payments, settlements' discount correction, reversals negated); commit-time check that items match tax lines per entry and account; tax date on the entry (default as §4); `ledger.TaxItems` port; `FailingTx` tests on every path; the stale `capture.checkTaxRate` tracker entry corrected |
| T61d | **Input tax on supplier invoices** | Tax calculated per item code at the tax date; one tax line per code and component; non-deductible tax on the item's line; supplier's stated tax posted within the configured tolerance, else 422 `payables.tax_mismatch` with each code's figures; credit memos at the named invoice's tax date and rates; down payment requests' tax checked at the run date's rate; capture agent proposes codes (eval cases added) and flags mismatches; documentary-requirement warnings for CA/AE |
| T61e | **Reverse charge, self-assessment and import tax** | `reverse_charge` and `import` codes post output and input lines and two items, the supplier owed the base; import tax from a customs declaration entry; UAE domestic RCM codes with the declaration as evidence; Canada line-405 codes; tests that a fully recoverable reverse charge nets to zero and a partly recoverable one leaves the blocked share on expense |
| T61f | **Output tax from the pilot's sales** | Output codes accepted on manual and integration journal entries; the ledger calculates and checks the tax lines within tolerance; jurisdiction required where a component needs it; zero-rated and exempt bases recorded as items; idempotent by the integration's reference; the shape decided in P2 |
| T61g | **Registrations and tax periods** | `companies[].tax_registrations` (authority, number encrypted as tax ids are, return type, frequency, first period; UAE tax group across companies); tier 3; tax periods generated; API to list registrations and periods |
| T61h | **The return engine** | `returns.yaml` in the pack schema (boxes as item selections or CEL expressions over boxes), validated and hashed with the pack; a draft return for a registration and period computed from unreported items, with prior-period items shown apart; every box drills to items; `GET /v1/tax-returns:compute` (contract first, `tax.return.read`) |
| T61i | **Return lifecycle** | `tax.tax_return` with snapshot (box values, item ids, hash, pack version); prepare (needs closed periods; sets `reported_in` under a registration lock, once), approve (another person, approvals context), file (a person records the confirmation; locks the periods via T32), withdraw, amendment; human-only transitions; audit and outbox events; run-twice tests for any worker; immutability trigger on filed snapshots |
| T61j | **GST/HST return for Canada** | The GST34 lines in the Canada pack's `returns.yaml` (101-115 incl. 205 and 405), golden tests from a worked set of invoices, entries and adjustments, signed off by the tax specialist; the FPZ-500 (GST/HST + QST) if P4 includes it |
| T61k | **VAT 201 for the UAE** | Boxes 1a-14 in the UAE pack (with T62f), emirate by the registration's fixed establishment, Box 6/7 from import entries, golden tests, specialist sign-off; checked against the EmaraTax form labels |
| T61l | **Tax agent** | Prepares draft returns, explains variances against the last period and the tax accounts, proposes adjustment entries (parked) and code corrections; evaluation suite (ADR-0039); never transitions a return |
| T61m | **Engine connector port** (deferred unless P1 says otherwise) | Quote stored on parked documents, posting uses the stored quote, commit through the outbox; a fake engine in tests; provider recorded in this ADR when chosen |

T61a-T61f and T61g-T61k are the pilot's must-haves (T61f's shape and T61j's
QST part depend on P2 and P4); T61l and T61m can follow the pilot's first
filing. T61a should land before T62f, so the UAE pack is written once in the
new schema. Lane S, with T61c touching the ledger (lane F's code): T61c runs
when lane F is not changing `journal.go`.

## Decisions for the product owner
| # | Decision | Options | Recommendation |
|---|---|---|---|
| P1 | Tax engine provider | (a) in-house for CA/AE behind a port, no provider now; (b) Avalara now; (c) Vertex now | (a): the pilots' rates are few and in the packs; an engine adds cost, a processor outside the region and a call on posting, and files nothing; choose a provider when a country needs one (US, phase 3) |
| P2 | Where the pilot's output tax comes from (no billing until phase 3) | (a) per sales invoice entries from the pilot's billing system through the journal-entry API; (b) daily or period summary entries per code and jurisdiction; (c) manual entries only; (d) bring a minimal billing forward | (a), with (b) allowed for low-value retail volume: per-invoice entries give drill-down for the return and are what T65's UAE e-invoicing will need; (d) is phase 3 scope |
| P3 | Filing | (a) the ERP prepares, a person files on My Business Account / Revenu Québec / EmaraTax and records the confirmation; (b) electronic filing (CRA GST/HST NETFILE or EDI; EmaraTax) from the ERP | (a) for the pilot: no authority API is needed, the agent rule (never file) holds trivially, and the filed record is a person's; revisit (b) after the pilot |
| P4 | Canadian returns in scope | (a) GST/HST only; (b) plus QST via Revenu Québec's FPZ-500 if the pilot company is in or registered in Quebec; (c) plus provincial PST returns | (b), depending on the pilot's registrations (please tell us its province and registrations); PST: self-assessment and a report of PST paid, but no PST return in T61 |
| P5 | Supplier's tax differs from the calculated tax | (a) refuse posting beyond a tolerance; (b) post and block the invoice for payment; (c) post the supplier's amount with a note | (a), tolerance configured per company (default one minor unit per tax code); within it the supplier's amount is posted, because input tax is what was charged |
| P6 | Tax date | (a) posting date; (b) a separate tax date, default the invoice date, editable by a person within open returns | (b): matches SAP's tax reporting date, lets Canadian ITCs be claimed later within the limit, and keeps rate and return period right when posting lags the invoice |
| P7 | An item dated in a filed period | (a) always the next return as a prior-period adjustment; (b) always an amendment; (c) next return up to a threshold per country (UAE AED 10,000 of tax), above it a person must choose amendment or voluntary disclosure | (c), the threshold in the country pack, zero for Canada (always next return unless a person amends) |
| P8 | Partial recovery | (a) fixed recoverable % per code (meals 50%, blocked UAE input 0%); (b) plus annual apportionment for businesses with exempt supplies | (a) for the pilot unless a pilot company makes exempt supplies; (b) is a later step |

**Decided (product owner, 2026-10-20), all as recommended:**
- **P1** (a): calculation in-house for Canada and the UAE behind the
  `Calculator` port; no provider now. T61m stays deferred until a country
  needs an engine.
- **P2** (a) with (b): the pilot's output tax comes as journal entries from
  its billing system through the journal-entry API, one per sales invoice;
  summary entries per code and jurisdiction are allowed (low-value volume).
- **P3** (a): the ERP prepares and approves returns; a person files on the
  authority's portal and records the confirmation. No electronic filing.
- **P4** (b): the GST/HST return, plus the QST through Revenu Québec's
  FPZ-500 only if the company is registered in Quebec; PST is self-assessed
  and reported (PST paid), with no PST return.
- **P5** (a): the supplier's stated tax is posted when it is within a
  per-company tolerance (default one minor unit per tax code); beyond it
  posting is refused with 422 `payables.tax_mismatch`.
- **P6** (b): a separate tax date, defaulting to the invoice date, editable
  by a person while its return is open, never by an agent.
- **P7** (c): a late item goes into the next return as a prior-period
  adjustment up to a per-country threshold in the pack (UAE AED 10,000 of
  tax; zero for Canada); above it a person chooses amendment or voluntary
  disclosure.
- **P8** (a): a fixed recoverable percentage per code; apportionment is a
  later step, only if a pilot company needs it.

Information needed from the pilot (not decisions): each pilot company's
province or emirate, registrations (BN/RT, QST, PST, TRN, tax group), filing
frequency, whether it is in a designated zone, its billing system, and whether
the UAE company's revenue reaches AED 50m (e-invoicing from 2027-01-01, T65).

## Sources
- CRA, Instructions for preparing a GST/HST return:
  https://www.canada.ca/en/revenue-agency/services/tax/businesses/topics/gst-hst-businesses/calculate-prepare-report/instructions-preparing-return.html
- Input Tax Credit Information (GST/HST) Regulations, SOR/91-45, s. 3:
  https://laws-lois.justice.gc.ca/eng/regulations/SOR-91-45/section-3.html
- Place of Supply (GST/HST) Regulations, SOR/2001-170:
  https://laws-lois.justice.gc.ca/eng/regulations/SOR-2001-170/ ; CRA
  Technical Information Bulletin B-103 (place of supply, revised 2010)
- CRA GST59 (self-assessment by non-registrants):
  https://www.canada.ca/en/revenue-agency/services/forms-publications/forms/gst59.html
- Revenu Québec, FPZ-500 and FPZ-2034.CD:
  https://www.revenuquebec.ca/en/online-services/forms-and-publications/current-details/fpz-500-v/
- UAE Ministry of Finance (WAM), VAT amendments from 2026-01-01 (Federal
  Decree-Law No. 16 of 2025): https://www.wam.ae/en/article/bn10b09-ministry-finance-implement-vat-law-amendments ;
  DLA Piper summary: https://www.dlapiper.com/en/insights/publications/gulf-tax-insights/2025/gulf-tax-insights-december-2025/uae-announces-amendments-to-vat-law-effective-1-january-2026
- FTA, Designated Zones VAT Guide: https://u.ae/-/media/Documents-2023/Designated-Zones-VAT-Guide.ashx
- FTA public clarification on the reverse charge for electronic devices (WAM):
  https://wam.ae/en/details/1395303213742 ; Baker McKenzie on VATP034 /
  Ministerial Decision No. 262 of 2023
- UAE e-invoicing, Ministerial Decisions No. 243 and 244 of 2025 (EY):
  https://taxnews.ey.com/news/2025-1964-uae-ministry-of-finance-publishes-ministerial-decisions-on-e-invoicing
- VAT 201 box descriptions: the FTA's VAT return user guide (v. 2021, before
  EmaraTax) as summarised by Zoho https://www.zoho.com/ae/books/vat/guides/vat-return.html
  and ClearTax https://cleartax.com/ae/vat-return-uae — secondary; the tax
  specialist checks the EmaraTax form.
- UAE Executive Regulation (Cabinet Decision No. 52 of 2017) Art. 51, 53, 59 —
  read through secondary summaries; the specialist confirms the current text.

**Unsure, for the tax specialist:** rounding per line or per invoice in either
country; the FPZ-500 lines; the exact VAT 201 labels on EmaraTax; the scope of
the 2026 five-year limit; on an invoice mixing a recoverable code with a
cost one (V1 with BC PST), whether a rounding difference to the supplier's
tax may be recovered as the recoverable code's input tax, as T61d posts it
(the recovering components take it before the cost components), or should
stay a cost (product owner, 2026-10-21: to the tax specialist list); the UAE six-month payment rule and the AED 10,000
correction threshold after the 2026 procedures amendments; the CRA's reporting
period thresholds and ITC time limit as stated here from memory; the currency
conversion articles cited from memory.
