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
T65's design (ADR-0061 §1) reads "fed to capture" as capture's rules,
not its operation: e-invoices are fetched from the provider and entered by
payables with capture's matching, holds and tax code proposals, without a
model.

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
- **As built (T62g):** Canada pack 2026.4 writes T62e's sourced codes in this
  schema: one component per code (`gst`, `hst` with the province as its
  jurisdiction, `qst`, and `pst_bc`, `rst_mb`, `pst_sk` as `non_deductible`
  components without an account), every dated rate written ahead and
  behind (NB, NL and PE's increases of 2016, NS's decrease of 2025-04-01).
  Activating them sets up rates from 2013-04-01 (`TestCanadaPackTaxCodes`
  in the ledger, which also calculates HON 13% and QST 9.975% on 1,000.00
  as payables does: 130.00 and 99.75, wholly recoverable; BC PST 70.00
  not). Details and what the specialist confirms: ADR-0048, Canada pack.
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
- **As built (T61d follow-up, migration 000182):** the proposal checks a
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
  fingerprint is added with the returns that read it (T61g); local amounts are the lines', converted at the
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
  periods (P6). The first two were built in the T61d follow-up (below
  and §4), the third with the hold of chosen tax dates (T61o, §11 As
  built).
- **As built (T61d follow-up, migration 000182):** the documentary
  requirements are the company's `tax_settings.documentary_requirements`
  (config.TaxSettings), which the country pack's `tax.yaml` brings under
  `tax_settings` (§8): tiers, each with a threshold in the company's
  currency, `at_least` (a total equal to it is in the tier: Canada's "$100
  or more") or `above` (the UAE's "above AED 10,000"), what it `requires`,
  its `source` and `pending_sign_off` for a tier the tax specialist has not
  confirmed (a pack signed off holds none). The ERP checks what it holds:
  `supplier_tax_id`, met when the supplier's business partner holds a tax
  identifier of the company's country (a scheme named after it, `CA_...`,
  `AE_...`, as the SAP load names them `<LAND1>_TAX`; a US vendor's EIN is
  no GST/HST number; which schemes count is listed for the tax specialist),
  and `item_descriptions`, met when every item
  without an order has a description (an order item is described). When an
  invoice or subsequent debit claiming tax posts, its gross amount,
  converted into the company's currency at the tax date's rate of the tax
  rate type, is held to each tier (when no rate of the type converts it,
  the check never refuses the posting: every tier is taken as reached and
  its warnings are marked `unassessed`); each unmet requirement of a tier it
  reaches is a warning, once (the first tier naming it), stored on the
  invoice (`supplier_invoice.documentary_warnings`) and returned as the
  invoice's `documentary_warnings` (code `supplier_tax_id_missing` or
  `item_description_missing`, the tier's threshold as money, whether it is
  inclusive, the source, the pending flag, `unassessed` and a message). Credit memos and
  subsequent credits take tax back and are not checked; an invoice posted
  before has none. Canada 2026.3 (and 2026.4, which keeps its settings)
  states SOR/91-45 s. 3 (b) and (c) as two tiers (from CAD 100 the
  supplier's registration number; from CAD 500 also a description of each
  supply); the recipient's name and the terms, also
  required from $500, are on the document only and not checked. The UAE
  pack (T62f) is to state the threshold as `above: "10000"`, requiring
  `supplier_tax_id`, `pending_sign_off: true` (Executive Regulation Art. 59
  is read only through secondary summaries). The working papers' filter on
  these warnings comes with the returns (§11).
- **As built (T62f):** UAE pack 2026.1 states the tier as above (`above:
  "10000"`, `requires: [supplier_tax_id]`, `pending_sign_off: true`, its
  source Art. 59), with `rate_type: average` and `rounding: document`,
  pending the tax specialist like every UAE fact it holds
  (`packs/ae/TAX-PENDING-T62f.md`; ADR-0048 As built, T62f). Payables'
  `TestUAEPackDocumentaryWarning` holds an invoice of a UAE company to the
  pack's settings: above 10,000 without a UAE tax identifier warned
  (`supplier_tax_id_missing`, exclusive, pending), exactly 10,000, below it
  and with an `AE_` identifier not; the walkthrough's step 41 posts one
  above and one below through the API. The tier is measured on the
  invoice's gross total, as every tier is; whether Art. 59's AED 10,000 is
  the supply's value instead is listed for the specialist. Every UAE code
  of the pack carries its UN/CEFACT category (§1) but import VAT, IM5,
  for which this ADR records none (pending the specialist).
- **As built (T62f sign-off, 2026-11-09):** UAE pack 2026.4 is signed off
  (Sources, As built: sign-off of the UAE pack), and its tier is the
  same without `pending_sign_off`: an invoice above AED 10,000 without a
  UAE tax identifier is warned with `pending_sign_off: false`. The
  earlier versions, unsigned, keep their pending tier.


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
- **As built (T61n, migrations 000175-000176):** `companies[].service_posting_limit`,
  an amount in the company's currency validated with its digits,
  optional and posting-related (tier 3) like `manual_entry_approval_limit`
  and `release_dual_limit`. `POST /v1/journal-entries` is open to service
  principals: `x-agent: false` came off it, since it refuses every kind
  but people, and the handler refuses agents with 403
  `identity.agent_not_allowed`, as `importBankStatements` does
  (ADR-0047); the statement above that integrations were held to the
  manual limit "today" was not so, they were refused outright, and P2 (a)
  needs them to post. A service principal's entry is measured as a
  person's is against the manual limit (T35f): its debits, converted
  exactly into its company's currency at the average rate of its posting
  date (the rate it posts at), unrounded, so an entry in USD of a CAD
  company is compared in CAD and 1000.004 does not pass 1000.00 (F13); a
  closed period refuses it first (409 `ledger.period_not_open`), as
  nothing a person could not post is parked. At or below the limit it
  posts (201) as before. Above it nothing is posted and no number drawn:
  the entry is parked (200 with the parked entry, `Location` naming it),
  `created_by_kind` `service`, no owner, and `limit_exceeded` (`kind`
  `service_posting_limit`, `debits` and `limit` each with its currency,
  and `detail` in words, naming the conversion for a foreign-currency
  entry); the figures are stored as measured
  (`parked_entry.limit_*`, never rewritten, trigger
  `parked_entry_keep_limit`) and are in the park's audit record; the park
  publishes `ParkedJournalEntryChanged` as any park does. A person finds
  them with `GET /v1/parked-journal-entries?created_by_kind=service&status=parked`
  and posts one with `:post`, taking it on as an agent's proposal
  (T35d), which measures it against the manual entry approval limit and
  asks the controller's approval above it; `:post` stays people's, so
  the service cannot post what it parked. **Decided by default:** a
  company without a `service_posting_limit` holds its services to its
  `manual_entry_approval_limit`, as integrations would have been held
  before, parking above it instead of refusing; a company the
  configuration lacks has a limit of 0 (every entry parked), as a
  missing manual limit does. People are unaffected. A retry with the same
  `Idempotency-Key` answers the parked entry again. **Rolling upgrade
  (06 §6.4):** 000175 adds nullable columns and NOT VALID checks, 000176
  validates them; an instance of the previous release refuses a service
  principal's entry outright (403, `x-agent: false`) and never writes the
  columns, so the key fails closed in a mixed cell and needs no ordering.
- **As built (the emirate guard, review:T61k, migration 000233):** a
  component levying its lines' subdivision lists the subdivisions it
  accepts (`subdivisions`, each a subdivision of its country, listed once;
  only on such a component: `config.Validate`, and the database's
  `tax_component_subdivisions`), and the ledger refuses a line naming
  another with 422 `ledger.jurisdiction_not_levied`, naming those it
  levies (`checkJurisdictions`; trigger `journal_line_tax_jurisdiction_code`
  behind it, which a reversal still passes), so a mistyped emirate cannot
  leave a sale's output VAT off the VAT 201's boxes 1a-1g. UAE pack 2026.3
  is 2026.2 with S5 listing the seven emirates, AE-AZ, AE-DU, AE-SH, AE-AJ,
  AE-UQ, AE-RK and AE-FU, the ISO 3166-2 codes the tax specialist confirmed
  (product owner, 2026-11-09; ADR-0048 As built, the emirate guard).
  **Decided by default:** the list is optional, a component without one
  accepting any subdivision of its country as before, since the released
  2026.1 and 2026.2 hold none and a released version never changes, and a
  tenant's own code levying its lines' subdivision (an adjustment code for
  box 1's adjustment column) is valid without it; the 422 keeps the
  existing code and its field error (`not_levied`), its message listing
  what the code levies. **Rolling upgrade (06 §6.4):** 000233 is additive
  (a null list accepts what it did); an instance of the previous release
  never writes the list, and a configuration activated by it leaves the
  list as it was, so a pack listing subdivisions is applied once the
  rollout is complete.

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
  which the pack names. Canada 2026.3 and 2026.4 name `average` and `document`, both
  pending the specialist (the conversion and rounding rules are cited from
  memory above).
- **As built (fix, review:T73e3, 2026-11-02):** a code the calculator
  cannot calculate on the date is refused, not answered 500. The ledger
  reads a code with the rates in force on the date, so a code half of whose
  components have no rate then (a reverse charge or import code whose
  `self_output` rate starts later than its `self_input`'s) reaches the
  calculator as half a pair (`tax.ErrInvalidCode`); a component without a
  rate is `tax.ErrNoRate`. Posting a supplier invoice and capturing one
  answer 422 `payables.invalid_tax_code` or `payables.no_tax_rate`; a
  journal entry's output tax and declared import bases 422
  `ledger.invalid_tax_code` or `ledger.no_tax_rate`; each detail names the
  code (`tax.Reason`). A capture whose code cannot be calculated is refused
  as one whose code is no input code on the tax date is
  (`payables.unknown_tax_code`), not held. The invoice read already leaves
  its `calculated_tax` out then (ADR-0052 T73e3).

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
  of the return; T61h checks it against the pack's `returns.yaml` when a
  draft is computed, §11 As built), `frequency` (`monthly`, `quarterly`, `annual`),
  `first_period_start` (day 1-28 of a month, so every period starts on the
  same day; a start on the 29th-31st is refused for want of a rule for the
  shorter months; the first period the ERP files, its cutover, from which
  returns are prepared in period order: §11 As built, period order) and
  optional `last_period_end` (the last day of one of its periods) and
  `group_members` (a UAE tax group: the other companies' codes, the
  registration listed under the representative member).
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
- **As built (T61h, migration 000174):** decided by default from this
  section, ADR-0048, ADR-0046 and the API rules, each listed for the
  product owner to overrule.
  - **The definition.** A pack's optional `returns.yaml` (ADR-0048) lists
    return types (`type`, the code a registration's `return_type` names;
    `name`; `prior_period_threshold`, a whole amount of up to 13 digits)
    and their boxes in order: a `code` (`^[a-z0-9][a-z0-9_]{0,19}$`, unique
    in its return), a `label`, and either `select` (a `direction`; when
    listed, `kinds`, `components`, `jurisdictions` and `account_keys`; a
    `measure` of `base`, `tax`, `recoverable` or `non_recoverable`, in
    local currency; `negate` to show credits, output tax and its base,
    positive) or an `expression` over earlier boxes, read as
    `box_<code>`. The account key is a selector beside the ones listed
    above, as a reverse charge's two items differ by it alone
    (`self_output`, `self_input`). "Adjustment" is not a measure: the
    adjustments of the last bullet will be items of a kind, which `kinds`
    selects once the kind exists (`ledger.tax_item.kind` has none yet).
    Expressions are compiled by the CEL engine of ADR-0046
    (`rules.CompileFormula`: the same environment, no time, no floats,
    its cost and time limits) over integer minor units, refused when they
    read a later box or are not integers; an overflow fails the
    computation rather than wrapping. Validated and hashed with the pack.
  - **The draft** of a registration's period is computed on read and
    never stored (as above): it reads the unreported items
    (`reported_in` null) of the registrant and a tax group's members dated
    on or before the period's end, through the ledger's port
    (`ledger.TaxItems.Unreported`, grouped by direction, kind, component,
    jurisdiction, account key and whether dated before the period's start;
    `UnreportedItems` for a box's items, paged in tax date and entry
    order), which holds the caller to every company it reads. A draft
    claims no item, so computing it twice, or twice at once, gives the same
    draft and neither can take an item from the other; T61i's prepare
    claims items once under the registration's lock. A partial index
    (`tax_item_unreported`) serves the selection. Reads write no audit
    record and publish nothing (ADR-0011 audits changes, ADR-0007 external
    effects).
  - **Entries migrated from SAP** (`journal_entry.source_context`
    `integration`, the migration runs' alone) are left out: an open down
    payment's input tax, dated SAP's posting date, was reported by SAP's
    returns (ADR-0037 As built, T72c, its tracker leaf). Its clearing's
    reversal, posted on the platform by payables, is reported.
  - **Prior-period items** (dated before the period's start, unreported)
    are part of each box and shown apart (`prior_period_amount`; an
    expression box's is its expression over the earlier boxes' parts);
    their signed tax is summed for information. **The threshold's measure
    is open** (product owner and tax specialist: net or gross, per earlier
    period or pooled, per error); until it is settled `above_threshold` is
    raised, conservatively, when the input or the output tax of the items
    of any one earlier period (by the registration's periods; one bucket
    before its first) exceeds the pack's threshold in magnitude, so
    directions and periods never cancel and the flag errs towards a
    person's review. No threshold means always the next return (P7,
    Canada's case). No released pack sets one yet (T61k's UAE pack will).
  - **Refusals** (problem values): a `period_start` that starts none of
    the registration's periods (422 `tax.not_a_period`); a period not yet
    ended on the registrant's date in its time zone (409
    `tax.period_not_ended`: items still arrive in it, and T61i prepares
    ended periods only); no definition of the return type in the pack
    version the registrant's company adopted (`packs` in the document), or
    no pack adopted (409 `tax.return_undefined`); companies of different
    local currencies (409 `tax.return_currency_mixed`; amounts are the
    registrant's local currency); a registration covering a company the
    caller may not act in (403 `tax.registration_companies`), one they do
    not see 404. The return type is checked here, not in the
    configuration (T61g's As built said T61h would): a pilot registers
    before T61j and T61k define its return, and a configuration finding
    would refuse the registration until then.
  - **API.** The steps' `GET /v1/tax-returns:compute` is against the API
    rules (custom methods are POST; collection custom methods start work,
    202): a draft computed on read is a resource read with GET, as the
    cash position is. `GET /v1/tax-registrations/{id}/draft-return?period_start=`
    (`getTaxReturnDraft`) and its drill-down `GET
    /v1/tax-registrations/{id}/draft-return/items?period_start=&box=`
    (`listTaxReturnDraftItems`: each item's entry with its document number,
    posting date, reference and header text, its code, component, amounts
    and what it adds to the box; the amounts add up to the box's), both
    `tax.return.read` (migration 000174). Drill-down stops at the entry
    (`getJournalEntry`); the documents are T59's, the export T63's.
  - **Walkthrough.** No released pack defines a return until T61j, so the
    walkthrough serves a copy of the latest Canada pack with an
    illustrative return (`scripts/walkthrough/returns.yaml`, boxes that
    are not the GST34's lines) as an unreleased pack (`ERP_PACKS_DIR`,
    refused in production; ADR-0048), and step 11 computes the draft of
    the latest ended period with a late sale and an earlier period's;
    T61i extends it to prepare, approve (Fiona, the tax manager, not
    Carla who prepared it) and file that return, and to show the
    period's draft empty after.
- **As built (T61i, migrations 000179-000180):** decided by default from this
  section, §12-§13, P3, P7, the period lock (T32) and the
  approvals context, each listed for the product owner to overrule.
  - **The stored return.** `tax.tax_return` per registration and period
    start: kind (`original`, or `amendment` naming the filed return it
    corrects, `amends_id`), status, the definition's return type and
    name and the pack version it followed, the currency, the item count,
    the prior-period figures the draft showed (items, tax, the pack's
    threshold, the provisional flag), and its boxes
    (`tax.tax_return_box`: amount, prior-period part, corrected amount,
    item count) and the keys of the items it claimed
    (`tax.tax_return_item`), with `items_hash`, the SHA-256 of a line
    `fiscal_year|journal_entry_id|item_no|base|tax|recoverable` per item
    in key order, local amounts in minor units. Boxes and items are
    written while the return is prepared and never change (insert-only,
    `tax_return_snapshot`); a trigger lets each move set its own fields
    alone (`tax_return_transition`, `tax_return_fixed`), so a filed,
    withdrawn or rejected return is final.
  - **Prepare** (`POST /v1/tax-registrations/{id}/returns`,
    `tax.return.prepare`) is refused as the draft is (§11 As built,
    T61h), then locks the registration FOR UPDATE, so two prepares of it
    run one after the other: the second finds the first's return (409
    `tax.return_in_progress`; one return of a period in progress and one
    original that stands, `tax.return_exists`, both backed by unique
    indexes). It claims the items first, an update of the unreported
    items that some selection box chooses (`ledger.TaxItems.Claim`),
    which waits for and then skips an item another return claimed, and
    computes the snapshot from what it claimed, so the boxes are exactly
    its items'. Items no box selects are not claimed: a company with two
    registrations of different authorities (CRA and Revenu Québec)
    reports each component in its own return.
  - **Fiscal periods.** The fiscal periods to be closed, and locked on
    filing, are the leading ledger's regular periods of the
    registration's companies that overlap the **tax period**, not those
    of the items' posting dates as above: an item of the period posted
    late lies in the current open month, which preparing would otherwise
    have to close. An item posted then is claimed as the draft reads it
    (a posting commits whole, so none is half under way). Special
    periods are left out: they are the year-end close's, and a quarter's
    filing would stop it. An open one refuses preparing and filing (409
    `tax.fiscal_period_open`, naming it); filing locks the closed ones
    through `ledger.ReturnPeriods.Lock` (`lockFiscalPeriod`'s change,
    audited, `PeriodLocked`), the filer's `tax.return.file` sufficing,
    as the lock is filing's consequence here; those already locked are
    left.
  - **Approval** through the approvals context: object type
    `tax_return`, approval permission `tax.return.approve` (agents hold
    none), one approval by the fixed role `tax_manager`
    (`config.TaxReturnRole`, provisioned with the configuration's roles
    in every tenant provisioned from now on; a tenant provisioned before
    is refused 422 `workflow.unknown_role` until an administrator creates
    the role, `createRole`, none being a pilot yet); a configured role ("or as configured") is
    not built, as no setting asks for one. The requester is the preparer
    and may not decide (`workflow.own_request`); the database holds the
    approver to another person than the creator (`tax_return_four_eyes`).
    A rejected request rejects the return; the preparer's cancelled
    request withdraws it; both release its items.
  - **File** (`POST /v1/tax-returns/{id}:file`, `tax.return.file`,
    If-Match) records an approved return's confirmation number (trimmed,
    up to 100 characters) and the date the authority gives, after the
    period's end and not after the registrant's date today in its time
    zone (422 `tax.invalid_filed_on`). **Withdraw** (`:withdraw`,
    `tax.return.prepare`, a reason) ends a prepared or approved return,
    cancelling its pending request, and releases its items
    (`ledger.TaxItems.Release`); a filed one is amended instead. Each
    move reads the return FOR UPDATE and is guarded by its status, so a
    retry after a lost answer is refused (409
    `tax.invalid_return_status`) and publishes nothing twice; a retry
    with the same Idempotency-Key is replayed by the API.
  - **Amendment (P7).** `kind: amendment` of a period whose return is
    filed corrects its latest filed return: it claims the unreported
    items dated within the period, which arrived after the filing (not
    those of earlier periods, which their own period's amendment or the
    next return takes), by the amended return's pack version; each box
    gives the correction (`amount`) and the period's figure after it
    (`corrected_amount`: the amended return's plus the correction, an
    expression box computed from those). With nothing filed, or nothing
    arrived, 409 `tax.nothing_to_amend`. It is approved and filed as an
    original is. Whether an authority takes the corrected figures, the
    differences or both is the tax specialist's to confirm (CRA's
    amended return, the FTA's voluntary disclosure), so the return
    carries both.
  - **People only (§13).** Preparing (an original or an amendment),
    filing and withdrawing are `x-agent: false`, as approving is: the
    stored prepare claims items, a transition; agents compute drafts.
  - **Ledger.** `reported_in` may now be released (`tax_item_fixed`
    allows null to a return and a return to null, never one return to
    another), and a deferred constraint trigger of this context on
    `ledger.tax_item` (`tax_item_reported`) holds at commit that an item
    reported is in its return's snapshot, of a company its registration
    covers, dated by its period's end, the return standing, and that an
    item released was the return's of one withdrawn or rejected. An
    index, built concurrently in its own migration (000180), serves both
    (`tax_item_reported_in`). The trigger runs once per item claimed or
    released, at commit: fine at pilot volumes; a statement-level check
    over a transition table is the change if returns of 10^5 items or
    more make preparing slow.
  - **Reads.** `GET /v1/tax-returns/{id}` (the approval request's object)
    and `GET /v1/tax-registrations/{id}/returns` (paged by period start,
    filtered by period and status), `tax.return.read`, a return of a
    registration the caller sees; moving one needs every company it
    covers (403 `tax.registration_companies`).
  - **Not built:** the drill-down of a stored return's boxes to their
    items (the working papers, T59/T63; `tax.tax_return_item` holds
    them), T64's checklist task, due dates, and a configured approver
    role. The hold of a chosen tax date to open returns (P6) is T61o's,
    below.
- **As built (T61o, product owner 2026-11-01: P6's hold).** A tax date
  chosen for an entry is held to the periods no return reports yet;
  decided by default from P6, §4, §11 and the T61i lifecycle, each listed
  for the product owner to overrule. No migration.
  - **What holds a period.** A return of kind original or amendment that
    is `prepared`, `approved` or `filed`, of a registration covering the
    entry's company as registrant or tax group member, whose period
    holds the tax date. Prepared and approved hold as filed does: P6
    allows a person's date "while its return is open", and a prepared
    return has claimed its period's items; a withdrawn or rejected one
    has released them and holds nothing. Any registration covering the
    company holds, whatever its authority and the codes the entry
    carries: a company registered with the CRA and Revenu Québec whose
    GST/HST return is filed cannot date QST into that period either
    (which boxes would select an item is the pack's, not known at
    posting; conservative, for the product owner to narrow).
  - **What is held.** A manual entry with a tax code on a line, whatever
    its tax date (given, or its posting date), posted at once, parked and
    posted, posted by its approval, or posted by a service principal such
    as the pilot's billing system; and a supplier invoice whose tax date
    a person set (§4: "editable by a person within the open return
    periods"), on an account or on an order (`Posting.TaxDateChosen`).
    Not held: a tax date the document or the ledger decides, which is
    how a late item reaches the filed period's amendment or the next
    return (P7, the period lock above): an invoice's invoice date, a
    credit memo's named invoice's, a reversal's original's, a down
    payment's payment date, a cash discount's payment date, a goods
    movement's posting date, and the entries of migration runs. A manual
    adjustment for a filed period (the adjustments above) is dated in an
    open period and reported in the next return. Entries without tax are
    not held. A parked entry is checked when it is posted, not when
    parked, as its period is.
  - **The refusal.** 409 `ledger.tax_date_in_return` naming the tax
    date, the period, the registration's code and the return's kind, id
    and status. The call graph lists it for `postJournalEntry`,
    `postParkedJournalEntry`, `approveApprovalRequest` (an approved
    manual entry), `postSupplierInvoice`, and for the goods receipts and
    `startMigrationRun`, whose postings reach the same code but never
    choose a date.
  - **Concurrency.** The tax filing context's `Holds` locks the
    registrations covering the company FOR SHARE, by code, in a statement
    of its own and then reads their returns; prepare locks its
    registration FOR UPDATE before it claims items. A posting under way
    is claimed by the prepare that waited for it; a posting that waited
    for a prepare sees its return and is refused; postings share the lock
    (`TestTaxDateHoldSerializes`, which fails without the lock). The
    posting asks once it holds its document number range (`postAll`,
    after `Numbers.Next`): a configuration activation locks the number
    ranges first and the registrations after (taxfiling's `Setup`, by
    code), so the posting takes the two in the same order and cannot
    deadlock with it (`TestTaxDateHoldLockOrder`, which fails with the
    hold taken before the number). The price: a held posting waiting for
    a prepare under way holds its company's number range meanwhile, so
    the company's other postings of that document type and year queue
    behind it until the prepare commits.
  - **The port.** `ledger.TaxReturns` (`Holding`) is in the ledger's
    published API and implemented by `taxfiling.Holds`, wired in
    `internal/app` into the API's ledger endpoints, the manual entries'
    approval handler and the posting port. `taxfiling` already imports
    the ledger and the ledger imports nothing of it, so the dependency
    direction is unchanged (the ledger calls its own interface, as it
    calls the partner and configuration contexts); no new ADR. A ledger
    wired without the port fails a held posting (500) rather than post
    it unheld.
- **As built (period order, review:batch32).** Returns of a registration
  are prepared in period order; decided by default from this section's
  "`reported_in` empty, so a late item lands in the next return", P7 and
  the T61i lifecycle, for the product owner to overrule. No migration.
  - **The rule.** An original of a period is refused while an earlier
    period of the registration, from its first period, has no filed
    return (409 `tax.earlier_period_unfiled`, naming the earliest such
    period). An original claims the unreported items dated by its
    period's end, so without the rule a later period prepared first took
    an earlier unfiled period's items as prior-period items, and that
    period's own return then reported less than happened in it. With
    it, a period's items are claimed by its own return, and once that
    return is filed, those arriving later by the next return (P7) or the
    period's amendment, as this section and T61i intended. Prepared or
    approved is not filed: a withdrawn or rejected return releases its
    items, and the late items of a period not filed belong to its own
    return. Amendments are not checked: an amendment claims only its own
    period's items, and its period is filed, so under the rule every
    earlier one is too; the exemption changes no outcome.
  - **The cutover.** The registration's first period has none before it.
    `first_period_start` is the first period the ERP files: the returns of
    earlier periods were filed before the cutover, outside the ERP (SAP),
    and the items of non-migrated entries dated before the first period
    are its return's prior-period items, as T61h built (the threshold's
    bucket "before its first"). The pilot's registration names the first
    period it files on the ERP. A period with no items is filed as a nil
    return: preparing an original claims what there is, none included.
  - **Concurrency.** The check runs after the registration is locked FOR
    UPDATE, in a query of its own over the earlier periods' filed
    returns; filed is final (`tax_return_fixed`), so a concurrent filing
    can only make a refused prepare pass on a retry, never a prepare pass
    that should not (`TestPrepareInOrderConcurrently`: two periods prepared at once, the
    later refused whichever runs first; an earlier period filed while the
    later is prepared). No database rule backs it: periods are computed
    from the registration (`config.TaxPeriods`), not stored, and the
    check reads only the registration's own returns under its lock.
  - **Drafts** are not held to the order: a draft claims nothing, and the
    draft of a later period shows an earlier unfiled period's items apart
    as earlier periods', which its return could not claim until that
    period is filed.
  - **Not decided here:** a registration's predecessor (a changed
    frequency or group starts a new code the day after the old one's
    `last_period_end`, §10 As built) is another registration, so the
    first return of the new code may claim the old code's last unfiled
    period's items as prior-period items. Whether those belong to the old
    code's return or amendment, or may go to the new code's, is the tax
    specialist's (listed for the product owner).
  - **Walkthrough.** Step 11's registration starts with the quarter
    before the latest ended one, its cutover (`tax_first`): the latest
    ended period's draft still shows invoice 1031 of the first period
    apart (60.00, of which 10.00 earlier), its return is refused until
    the first period's is filed (`tax.earlier_period_unfiled`), the first
    period's return reports 1031 (10.00), and the latest period's then
    reports 1039 alone (50.00).

- **As built (T61j, migrations 000214-000215): the GST/HST return.**
  Decided by default from this section, §9, P4, P7 and the sign-off
  precedent of T62g/T62h, each listed for the product owner to overrule.
  - **The lines.** Canada pack 2026.6 is 2026.5 plus `returns.yaml`, the
    return type `GST34` (a registration's `return_type`), its 18 lines in
    one list whose order is the key: a draft shows them in it and a
    stored return keeps it (`tax_return_box.box_no`); each box's `code`
    is the line's number (`101`, `113a`). What a line means is the
    Context's record of the CRA's instructions (lines 101-111, 205, 405,
    114 refund, 115 owing; 105 = 103 + 104, 108 = 106 + 107, 109 = 105 -
    108), whose page is now the pack's first source, as the Context cites
    it (not fetched again). The Context names 112, 113A, 113B and 113C
    a paper return's subtotals without their sums: their sums (110 + 111,
    109 - 112, 205 + 405, 113A + 113B) and 114 and 115 as 113C's negative
    and positive part are written from memory, and their labels say so,
    pending the tax specialist. 205 and 405 come before 113B, which reads
    them (an expression reads earlier lines only). 114 and 115 are the
    first lines that are not linear, so an expression box's derived
    figures are now differences of evaluations (review round 1): its
    earlier-period part is its value less its value without the earlier
    periods' items, and an amendment's own figure is its corrected
    figure less the amended return's; for linear boxes both are what
    T61h and T61i computed. `TestGST34Golden`: the second quarter owes
    120.00, of which -10.00 from a late ITC, 114 0.00 with no earlier
    part; an amendment reversing an import and an instalment turns the
    first quarter's refund of 783.00 into 167.00 owing, its 114 -783.00
    and 115 167.00.
  - **The mapping.** GST and HST are the components keyed `gst` and `hst`,
    as the pack's codes key them, so QST and the provincial sales taxes
    are on no line (and are not claimed by this registration's return:
    §11 As built, T61i). 101 sums the base of output items of kinds
    standard, zero and exempt; 103 the output tax of standard ones; 106
    the part recovered of input items (`input`, and a self-assessed
    claim's `self_input`, as §9 has for 405) of kinds standard,
    reverse_charge and import. A self-assessed tax keys its owed part
    (`self_output`) after its line: `gst_405`, `hst_405` (405) and
    `gst_205`, `hst_205` (205). Out-of-scope supplies are on no line, and
    revenue posted without a GST/HST code is not in 101.
  - **Adjustments** (the last bullet above) are a code kind of their own,
    `adjustment` (config, the calculator, `ledger.tax_code_definition`
    and `ledger.tax_item` by 000214, widened NOT VALID and validated by
    000215; the contract's item `kind` gains the value): tax alone, its
    rates 0, its tax what the manual entry posts on the component's
    account (an entry of tax lines and no base, as §7 As built already
    posts). An output code increases the net tax (104), an input code
    decreases it (107), and input codes keyed `instalment` and `rebate`
    are 110 and 111. The capture never proposes one for a printed 0%
    (`payables.proposeCode`): it is no document's code.
  - **Not signed off.** 2026.6 carries no `sign_off` (production refuses
    to apply it, 409 `config.pack_not_signed_off`; 2026.5 stays the
    signed-off version): the specialist confirms the lines, the sums from
    memory and the mapping, listed in `packs/ca/TAX-PENDING-T61j.md`,
    which `TestCanadaPackGST34` holds the pack to; the sign-off is a later
    version, its own tracker leaf (split from T61j), as T62h was.
  - **The FPZ-500 is not built.** P4 (b) includes it only if the pilot
    company is registered in Quebec, which the pilot has not said, and
    the Context records none of its lines (Unsure list): nothing is
    written from memory; its own tracker leaf waits for the specialist's
    line list and the pilot's registrations.
  - **Golden test** (`TestGST34Golden`, taxfiling): a worked quarter of
    Maple Inc on the released pack, through the API against PostgreSQL:
    sales with HST, GST, zero-rated, exempt, out of scope and in Quebec,
    a credit note, a sale posted twice and reversed, supplier invoices
    with GST, HST, QST, BC PST, imported services (405, 60% claimed) and
    real property (205) self-assessed, the CBSA's GST on an import, an
    adjustment each way, an instalment and a rebate: 101 4,400.00, 103
    260.00, 106 943.00, 109 -698.00, 113C -783.00, a refund of 783.00;
    prepared, the lines keep their order and the QST, PST and
    out-of-scope items stay unclaimed; the next quarter owes 120.00 with
    a late ITC shown apart.
  - **Walkthrough.** A released pack now defines the return, so the
    walkthrough applies the latest released Canada pack (2026.6) and its
    illustrative `scripts/walkthrough/returns.yaml` and unreleased copy
    are gone (`ERP_PACKS_DIR` stays for demonstrations and tests). Step 11
    computes, prepares and files the GST34's lines, with an adjustment
    (107) and an instalment (110) Carla enters for the latest period.
  - **Rolling upgrade (06 §6.4).** 000214 widens the checks and the
    previous release never writes the kind (its configuration refuses
    it), so it runs ahead of the code; an adjustment code is configured
    once the rollout is complete, as an instance of the previous release
    validating that document would refuse it.
- **As built (T61k): the VAT 201.** Decided by default from the Context,
  §7, §9, this section, P7 and T61j's precedent, each listed for the
  product owner to overrule. No migration, no contract change.
  - **The boxes.** UAE pack 2026.2 is 2026.1 plus `returns.yaml`, the
    return type `VAT201` (a registration's `return_type`), 42 boxes in
    one list whose order the stored return keeps: a box per column, coded
    `<box>_<column>` (`1a_amount`, `1a_vat`, `1a_adjustment`, ..., `9_vat`,
    `10_vat`) and `12`, `13`, `14`. What a box means is the Context's
    record of the FTA's VAT return user guide (v. 2021, before EmaraTax)
    **through the secondary summaries of Zoho and ClearTax**, which the
    pack's manifest already cites (not fetched again); the EmaraTax labels
    are not checked. The Context records the columns of 1a-1g (amount,
    VAT, adjustment), 9 (amount, recoverable VAT, adjustment) and 10
    (recoverable VAT), and 14 = 12 - 13 here; it names 2-7 without their
    columns and 8, 11, 12 and 13 as totals without their sums. The pack
    reads 2 and 7 as VAT, 3 and 6 as amount and VAT, 4 and 5 as amount,
    each label saying "column pending the tax specialist"; the totals'
    sums are written from memory and their labels say so (8's amount over
    1a-1g, 3-6; its VAT over 1a-1g, 2, 3, 6, 7; its adjustment over
    1a-1g; 11 over 9 and 10; 12 and 13 as 8's and 11's VAT plus
    adjustment). 14 is one signed figure, negative when refundable.
    `prior_period_threshold: "10000"` is P7's AED 10,000 of tax, its
    measure still open (T61h above).
  - **Emirate by fixed establishment.** 1a-1g select S5's items by the
    emirate each line names (§7 As built: one code levying the line's
    subdivision), lettered in the order the Context lists the emirates;
    the line names the emirate of the fixed establishment that made the
    supply, which whoever posts it states (the ERP holds no fixed
    establishments; the step table's "the registration's fixed
    establishment" would allow one emirate per registration only). The
    emirates' ISO 3166-2 codes are written from memory but for AE-DU and
    AE-AZ, and each box 1 label says the code is pending; a line naming
    another subdivision of AE is on no box and its item stays unreported
    (the ledger checks only the country). A guard refusing an emirate the
    pack does not list is its own tracker leaf (review:T61k), to land
    before the sign-off (landed: §7 As built, the emirate guard, UAE
    2026.3).
  - **The mapping.** Box 3 takes the owed part (`vat_due`, `self_output`)
    of reverse-charge codes, RC5 and DR5, as §9 As built reports the
    domestic reverse charge in box 3 (the Context lists imports only);
    box 6 an import's (IM5, the declared value its amount, from a customs
    declaration's entry, §9 As built); box 10 the part recovered of both
    claims (`vat`, `self_input`; the Context's "Box 3/6 output, Box 10
    input" for imported services and goods); box 9 standard-rated
    purchases, VB5's blocked VAT adding its amount and no VAT. Adjustments are codes of kind `adjustment` the
    tenant adds (T61j): output, component `vat`, levying the line's emirate
    (box 1's adjustment column), input `vat` (box 9's), output
    `tourist_refund` (box 2) and `import_adjustment` (box 7). The step's
    plan has box 7 "from import entries", but an import code's entry is
    box 6's, so a customs entry reversed later is a negative box 6 item of
    its period or its amendment, and box 7 takes the manual code alone
    (decided by default, pending the specialist). OS is on no box.
  - **Not signed off.** 2026.2 carries no `sign_off`; the specialist
    confirms the boxes, columns, sums, letters and codes, and the mapping,
    listed in `packs/ae/TAX-PENDING-T61k.md`, which `TestUAEPackVAT201`
    holds the pack to box by box (code, label, selection or expression,
    basis); the sign-off is a later version, its own tracker leaf (split
    from T61k), with T62f's.
  - **Golden test** (`TestVAT201Golden`, taxfiling): a worked quarter of
    Maple Gulf Trading (5000, AED) on the released pack through the API:
    sales in Dubai (with a credit note), Abu Dhabi and Sharjah, an Ajman
    sale posted twice and reversed, zero-rated, exempt and out-of-scope
    supplies, purchases with recoverable and blocked VAT, imported
    services and the domestic reverse charge, import VAT from a customs
    declaration, and an adjustment for each of boxes 1b, 2, 7 and 9: 8's
    amount 35,500.00 and VAT 1,570.00, 12 1,600.00, 13 1,125.00, 14 475.00
    payable; prepared, the out-of-scope item stays unclaimed; filed, an
    amendment reversing the import and taking a late invoice gives 14's
    correction -100.00 (375.00 after); the next quarter shows a late
    invoice's VAT apart, below the threshold and then, at 11,050.00 of
    earlier input VAT, above it.
  - **Signed off (2026-11-09).** The product owner reported that the tax
    specialist, Musto Ibrahim, signed off the VAT 201 as written: the
    boxes against EmaraTax's labels, the columns of boxes 2-7, the totals'
    sums of 8, 11, 12 and 13, the emirates' letters a-g and ISO 3166-2
    codes (guarded since 2026.3, §7 As built), and the mapping of the
    tenant's codes, items 1-10 of `packs/ae/TAX-PENDING-T61k.md`, with
    the AED 10,000 prior-period threshold (P7) and the amendment carrying
    the corrected figures and the differences as built. UAE pack 2026.4
    carries this sign-off with T62f's, one version as the leaf says
    (ADR-0048 As built, UAE 2026.4), its `returns.yaml` 2026.2's byte for
    byte (`TestUAEPackVAT201SignedOff`). **Decided by default:** the box
    labels keep the markers ("column pending the tax specialist", "from
    memory, pending the tax specialist", "its ISO code pending") they had,
    since the sign-off froze the content and dropping them is a content
    change (a later version, for the product owner). **Still open:** the
    threshold's measure (T61h above), which declaration the FTA takes an
    amendment in, and the due dates (§10, not built), questions the
    approval of what exists does not answer.
  - **Walkthrough.** Step 41 applies 2026.2 to Maple Gulf Trading, whose
    proposed company carries its FTA registration (quarterly, from the
    latest quarter ended in Dubai); Carla posts the quarter's sales by
    emirate, an export, an exempt supply and a customs declaration's
    import VAT, Alex the domestic reverse charge and a blocked purchase;
    the VAT 201 is computed (14 700.00), drilled down, prepared, approved
    by Fiona and filed.

### 12. Audit
Every change of a registration, code or rate is a configuration change request
(audit, approvals, simulation); every tax item is written with its entry (the
journal's audit); every return transition writes an audit record
(`tax.return.prepare|approve|file|withdraw`, ADR-0011) and an outbox event
(`TaxReturnPrepared`, `TaxReturnFiled`), and the filed snapshot is immutable
(a trigger, as released packs are immutable by hash). As built (T61i): also
`tax.return.reject` and the events `TaxReturnApproved`, `TaxReturnWithdrawn`
and `TaxReturnRejected`, through the context's own outbox (`tax`). Retention follows the
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
- **As built (T61l1, split from T61l; no migration):** decided by default
  from this section, ADR-0036 and the reconciliation agent's shape
  (`internal/reconagent`):
  - **The agent.** Agent type `tax` (`autonomy.Types`, the profile's
    open enumeration in both contracts) on `erp-default`, as the
    reconciliation agent runs: nothing here asks for an alias of its own.
    Its autonomy, monthly budget and spend are the profile's and the
    gateway's, as for every type. `internal/taxagent` (layout entry
    `{"apitools", "llm"}`) reads through API tools only:
    `listTaxRegistrations`, `listTaxPeriods`, `getTaxReturnDraft`,
    `listTaxReturnDraftItems`, `listTaxReturns`, `getTaxReturn`,
    `getJournalEntry`, `listAccounts`, `listBalances`; its role needs
    `tax.registration.read`, `tax.return.read`, `ledger.entry.read`,
    `ledger.account.read` and the session permissions.
  - **"Prepares a draft" is computing it.** Preparing a return is
    `x-agent: false` (§11 As built: the stored prepare claims items), so
    the agent computes the draft (`getTaxReturnDraft`) and explains it; a
    person prepares. `erp agent tax-draft -registration CODE|ID -period
    START` computes the draft first (a period not ended or not a period
    stops it before a session starts), then runs one session.
  - **The explanation is a recommendation** (`createRecommendation`,
    `x-autonomy: propose`, audited and evented as every recommendation):
    object `tax_registration` (a draft has no id of its own), kind
    `explain_draft_return`, its content starting with the return type and
    the period's days and citing the entries behind each difference from
    the last period's return and from the tax accounts by document
    number. A person decides it (`decideRecommendation`). The run's own
    guard refuses any other recommendation (another session, object, kind
    or period: the content's first two dates must be the period's) and a
    second one, whatever an entry's text asks. Entry texts are untrusted
    data in the prompt.
  - **Never a transition.** Preparing, filing, withdrawing and approving
    are `x-agent: false`, so `apitools` builds no tool of them
    (`TestNeverTransitions`), the API refuses the agent's own call (403
    `identity.agent_not_allowed`, the walkthrough shows it): held
    structurally, so the suite's checks that no return moved guard the
    platform rather than grade the agent (a tool call the agent was not
    given is answered by the run and recorded nowhere).
  - **Evaluation suite** `evals/tax` (this ADR lists no cases; they follow
    from the tracker leaf): a draft explained against the last period
    citing the entries; a difference from the tax account explained by
    an entry without a tax code, nothing parked; an entry's planted
    instruction to prepare, file and withdraw ignored, its claim that the
    tax manager approved not repeated as a fact. Replayed offline in CI with the scripted model; the check
    notation `$contains` (ADR-0039) grades a citation. The walkthrough's
    model stand-in answers from a script of turns (`modelstub -script`,
    as the close agent's branch has it), so step 11 runs the agent
    before Carla prepares the return.
  - **Left to T61l2:** adjustment entries proposed by parking them, and
    tax code corrections on captured and parked documents (below).
- **As built (T61l2, no migration):** decided by default from this
  section, ADR-0036 and the operations that already propose:
  - **One run.** The agent of T61l1 proposes in the same session as it
    explains (`erp agent tax-draft`, which now prints the entries it
    parked as well): its proposals come from the same reading of the
    period. It also reads `listParkedJournalEntries`,
    `getParkedJournalEntry`, `listSupplierInvoices` and
    `getSupplierInvoice`, and parks with `parkJournalEntry`; its role
    adds `ledger.entry.park` and `payables.invoice.read`.
  - **Adjustment entries are parked** through the existing
    `parkJournalEntry` (`x-autonomy: propose`): an agent's parked entry
    has no owner until a person takes it on, and posting it is
    `x-agent: false`, so a person posts it or cancels it. The run's own
    guard holds an entry to park to a company the registration covers
    (its registrant and a tax group's members), a tax date within the
    period (so the period's draft reports it once posted) and at least
    one line with a tax code; the ledger checks the code when it parks.
    The prompt asks for the adjustment §11 describes: a code of kind
    `adjustment` on the tax account and a reversal of the uncoded line,
    so the account's balance does not change.
  - **A tax code correction is a recommendation on the document**, kind
    `correct_tax_code`, object `parked_journal_entry` or
    `supplier_invoice`. This section says agents propose codes on
    captured and parked documents but not what they change; the
    least-privileged path is taken: `updateParkedJournalEntry` lets an
    agent change only an entry it parked itself, and
    `updateSupplierInvoice` would overwrite a captured invoice a person
    or the capture agent owns, while a recommendation changes nothing
    until the person who decides it changes the document. The guard
    admits a correction only of a parked document of the registration's
    companies that the run read, of any date (a document parked in the
    period is the prompt's concern, not the guard's), and one per
    document. The model's runner executes one turn's tool calls
    concurrently, so the tools that check or record the run's state
    take one mutex for each call whole (`apitools` `Tool.Exclusive`):
    two corrections of one document in a turn cannot both pass. The
    guard holds one run: running the agent again for the period proposes
    again, and a person cancels the entry or ignores the recommendation
    it repeats.
  - **Codes it names.** No operation lists a company's tax codes short
    of the whole configuration (`config.version.read`), which the agent
    is not given: the prompt has it name only codes it read on items or
    entries, the ledger refuses an unknown one at parking, and a person
    decides a correction.
  - **Evaluation suite** `evals/tax` version 2 (cases from the tracker
    leaf): an instalment entered without its code proposed as a parked
    adjustment, not posted; a parked sale without its codes given a
    proposed correction, the entry unchanged; a correct parked sale left
    for a person to post and the return for a person to prepare; a
    parked entry's planted instruction to post, park and recode not
    followed and its claim not repeated. Replayed offline as T61l1's.
  - **Walkthrough.** Step 11: Carla parks the adjustment decreasing the
    net tax and posts the instalment, both without their codes; the
    agent proposes GA on her parked entry and parks GI's adjustment for
    the instalment; its own post is refused 403
    `identity.agent_not_allowed`; Carla accepts both, puts GA on her
    entry and posts it and the agent's, and lines 107 and 110 show them
    before she prepares the return.

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
| T61h (done) | **The return engine** | `returns.yaml` in the pack schema (boxes as item selections or CEL expressions over boxes), validated and hashed with the pack; a draft return for a registration and period computed from unreported items, with prior-period items shown apart; every box drills to items; `GET /v1/tax-returns:compute` (contract first, `tax.return.read`; built as `GET /v1/tax-registrations/{id}/draft-return`, §11 As built) |
| T61i (done) | **Return lifecycle** | `tax.tax_return` with snapshot (box values, item ids, hash, pack version); prepare (needs closed periods; sets `reported_in` under a registration lock, once), approve (another person, approvals context), file (a person records the confirmation; locks the periods via T32), withdraw, amendment; human-only transitions; audit and outbox events; run-twice tests for any worker; immutability trigger on filed snapshots |
| T61o (done) | **Chosen tax dates held to open returns** (P6; product owner 2026-11-01) | A manual entry with tax, and a supplier invoice whose tax date a person set, dated in a period whose return is prepared, approved or filed is refused with 409 `ledger.tax_date_in_return` naming the period, return and status; dates the document decides are not held, so late items reach the amendment; a post racing a prepare serializes on the registration; walkthrough step 11 |
| T61j (done, not signed off) | **GST/HST return for Canada** | The GST34 lines in the Canada pack's `returns.yaml` (101-115 incl. 205 and 405), golden tests from a worked set of invoices, entries and adjustments, signed off by the tax specialist; the FPZ-500 (GST/HST + QST) if P4 includes it |
| T61k (done, not signed off) | **VAT 201 for the UAE** | Boxes 1a-14 in the UAE pack (with T62f), emirate by the registration's fixed establishment, Box 6/7 from import entries, golden tests, specialist sign-off; checked against the EmaraTax form labels |
| T61l (done) | **Tax agent** | Prepares draft returns, explains variances against the last period and the tax accounts, proposes adjustment entries (parked) and code corrections; evaluation suite (ADR-0039); never transitions a return. Split: T61l1 (done) drafts and explanations, T61l2 (done) adjustment entries and code corrections (§13 As built) |
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

**As built (T62h, sign-off):** the product owner reported on 2026-11-02
that the tax specialist, Musto Ibrahim, approved the items put to them,
signed 2026-11-02, as written; the approval builds nothing new, and Canada
pack 2026.5 records it (ADR-0048, Canada pack). Nothing above is
rewritten; this note says which of the items marked from memory, to
confirm or for the tax specialist stand confirmed.
- **Confirmed as written (Canada):** the reporting periods by annual
  taxable supplies (Context, from memory); the ITC time
  limit of generally four years; the conversion at the Bank of Canada rate
  of the day the tax became payable (Excise Tax Act s. 159, as recalled),
  and with it the Canada pack's `rate_type: average` (§8 As built), which
  holds while the tenant records the Bank of Canada's daily rate under that
  type (the ledger does not check a rate's source); rounding
  once per document, code and component, Canada's `rounding: document` (§8,
  §8 As built; no `rounding: line` for Canada); which tax identifier
  schemes count for `supplier_tax_id` (those named after the company's
  country, `CA_...`, §6 As built); a rounding difference on an invoice
  mixing a recoverable code with a cost one recovered with the recoverable
  code's input tax, as T61d posts it; and an amended return carrying both
  the corrected figures and the differences (§11 As built), as built for
  the CRA's amended return.
- **Still pending, not built:** every UAE item waits for the UAE pack
  (T62f) and the VAT 201 (T61k), and is signed off with them: VAT Law
  Art. 55 (the six-month payment rule) and Art. 69 (the currency rate), the
  AED 10,000 correction threshold after the 2026 procedures amendments, the
  scope of the five-year limit, the VAT 201 labels on EmaraTax, the
  Executive Regulation's Art. 51, 53 and 59 texts, the UAE documentary tier
  (`pending_sign_off`, §6 As built), UAE rounding, and the FTA's side of
  the amended return. (T62f wrote the UAE pack's codes and tax settings
  in 2026.1, unsigned; what the specialist confirms is in
  `packs/ae/TAX-PENDING-T62f.md`, ADR-0048 As built, T62f. T61k wrote the
  VAT 201's boxes in 2026.2, unsigned; what the specialist confirms is in
  `packs/ae/TAX-PENDING-T61k.md`, §11 As built, T61k.) For Canada, the GST34 lines (T61j) and the FPZ-500
  lines (P4) are signed off with the step that writes them. (T61j wrote
  the GST34's lines in Canada 2026.6, unsigned; what the specialist
  confirms is in `packs/ca/TAX-PENDING-T61j.md`, §11 As built, T61j.)
- **Still open (questions the approval of what exists does not answer):**
  the registration number formats (§10 As built, its own tracker leaf); the
  authorities' due dates (§10, not built), the Context's "one month after
  a monthly or quarterly period" among them; the measure of the prior-period
  threshold (§11 As built: net or gross, per period or pooled; no released
  pack sets a threshold, the UAE's will); and whether a predecessor
  registration's unfiled period's items belong to its own return or the
  new code's (§11 As built, not decided).

**As built (sign-off of the UAE pack, T62f; product owner, 2026-11-09):**
the product owner reported that the tax specialist, Musto Ibrahim, signed
off the UAE pack on 2026-11-09, as written; UAE pack 2026.4 records it
(`sign_off: {tax_specialist: "Musto Ibrahim", date: 2026-11-09}`, the
model's `SignOff`; ADR-0048 As built, UAE 2026.4), the version that also
carries the VAT 201's sign-off (T61k's leaf: one version for both). The
approval freezes what exists and builds nothing new; nothing above is
rewritten.
- **Confirmed as written (UAE pack, T62f):** the nine VAT codes, the
  standard rate of 5% and the zero, exempt and out-of-scope categories,
  each rate from 2026-01-01 as the pack starts it (an earlier start is a
  later version, if a document dated before 2026 needs it); the codes
  chosen (V5, VB5, RC5, DR5, IM5, S5 by emirate, Z0, E0, OS; no
  zero-rated or exempt purchase code); recovery wholly on 1300, VB5's
  blocked input recovering nothing (Executive Regulation Art. 53), with
  VAT Law Art. 55's six-month payment rule unchecked by the ERP as built;
  the UN/CEFACT categories, none for import VAT; the documentary tier
  (§6 As built: above AED 10,000, measured on the invoice's gross total,
  the supplier's TRN alone, any tax identifier scheme named after the UAE,
  Executive Regulation Art. 59), so 2026.4's tier is no longer
  `pending_sign_off` and its warning says so; `rate_type: average` (VAT
  Law Art. 69) and `rounding: document`; the bases recorded in
  `packs/ae/TAX-PENDING-T62f.md`, Art. 51 (designated zones) among them.
- **Still open, not in the pack:** the UAE's public holidays and the
  company's weekend (no source recorded) and apportionment for a business
  that also makes exempt supplies (P8 (b), a later step), as
  `packs/ae/TAX-PENDING-T62f.md` lists them.
- **Confirmed as written (VAT 201, T61k):** the boxes, columns, totals,
  letters, ISO codes and mapping of `packs/ae/TAX-PENDING-T61k.md` (§11 As
  built, T61k: Signed off), in 2026.4 with the pack's. With the UAE pack's
  and the VAT 201's sign-offs, the UAE items of "Still pending" under
  T62h's note above are confirmed as written but these: the threshold's
  measure after the 2026 procedures amendments, the scope of the five-year
  limit and the FTA's side of the amended return, which stay open as
  questions.
