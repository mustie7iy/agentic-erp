# ADR-0048: Country packs

**Status:** accepted (T62, product owner 2026-10-20); amended by T61a
(tax codes with components and all dated rates)

## Context
A company in a new country needs a chart of accounts, tax codes, a holiday
calendar, number ranges and payment terms that follow that country's law
and practice before it can post. SAP ships these as country versions
(localization), maintained by SAP and copied into customizing. Keying them
by hand for every tenant repeats the same research, invites mistakes in tax
rates, and leaves no record of which legal source a setting came from.
Tenants also change what they adopt, so a later version of the country's
settings cannot simply overwrite their document.

## Decision
- **A pack is a versioned YAML bundle** under `packs/<cc>/<version>/`: a
  `pack.yaml` manifest (country, version `YYYY.N`, `valid_from`, the legal
  `sources` each setting rests on, and the tax specialist's `sign_off`) and
  `accounts.yaml` (accounts and account determination), `tax.yaml` (tax
  codes, each with effective-dated rates), `calendar.yaml` (holidays),
  `numbering.yaml` (number range templates by document type) and
  `payments.yaml` (payment terms). The bundles are embedded with `go:embed`
  in the tiny `packs` package at the module root; `internal/countrypack`
  (pure Go, importing only `config`, the shared kernel and, for return
  definitions, the platform's CEL engine `platform/rules`) loads, validates
  and applies them, as `delivery/sapconfig` generates a draft from SAP.
- **Validation** refuses a pack with a rate outside 0-100% or with more than
  three decimals, overlapping effective dates of one tax code, an account
  reference (account determination, tax code account) not in the pack's
  accounts, no sources, duplicate keys, or a directory that does not match
  the manifest. A pack without a tax specialist's sign-off loads with a
  warning: **each pack is signed off before the pilot** (pilot gate).
- **Released versions are immutable.** `packs/released.sha256` lists the
  hash of every pack directory; loading refuses a pack whose hash is missing
  or differs and a listed pack that is gone. A change is a new version. The
  packs are not signed on their own: they ship inside the signed image
  (ADR-0032).
- **Applying a pack creates an ordinary configuration change request**: the
  applier produces a draft document and notes for the reviewers, which go
  through approval tiers and simulation like any other change (03 §3.5).
- **The API** (T62d): `GET /v1/country-packs` lists the packs in the image
  (country, version, the hash they were released with, the sign-off, the
  sources and how many settings of each kind), the same for every tenant,
  paged in country and version order (`config.change.read`).
  `POST /v1/country-pack-applications` (`applyCountryPack`, `x-autonomy:
  propose` like `createChangeRequest`, so an agent that may propose a change
  may apply a pack, and `config.change.propose`) takes the pack version, the
  company, the date of application and the fiscal year, locks the active
  version, applies the pack to its document and stores the draft as a
  change request in status `draft` based on it, as `createChangeRequest`
  does (Idempotency-Key, audit `config.change_request.create` with the
  pack, no event). The notes are stored on the change request
  (`config.change_request.notes`, the `notes` of `ChangeRequest`), where
  reviewers read them; they are not updated when the proposal is edited.
  Nothing is activated: submission, evidence, approval and activation are
  the change request's, and its tier is the draft's. The company must be
  the code of a legal entity the caller has company access to (422
  `config.unknown_company` otherwise, the two alike), and that legal entity
  active (422 `config.legal_entity_not_active`, as postings refuse it); a
  pack for another country is 422 `config.pack_not_applicable`, an older
  version than the adopted one 409 `config.pack_downgrade`. **In production
  a pack without its tax specialist's sign-off is refused** (409
  `config.pack_not_signed_off`, the server's production switch that also
  refuses EBICS, product owner 2026-10-20); elsewhere it is applied with a
  note, so a pack can be tried in test and staging before it is signed off.
- **Upgrades are a three-way merge**: base = the pack version the company
  adopted (empty on first adoption), the tenant's current document, and the
  new pack version, matched by account number, tax code, number range
  key, payment term key, account determination field and holiday date. A
  change only in the pack is taken; a change only in the tenant's document
  is kept; a change in both is kept as the tenant has it, with a conflict
  note (unless both made the same change); an item the new pack removed is
  kept, with a note.
- **Provenance**: the document's `packs` section records, per company, the
  country, the pack version and the date it was applied on; the next
  upgrade reads its base from there.
- **Holidays** live in the document's `calendars` section, one calendar
  per company of the document (`calendars[company].holidays[date]`), each
  holiday a `YYYY-MM-DD` date once, with a name (T62c). Applying a pack
  merges its `calendar.yaml` into the company's calendar by date, in date
  order. Holidays post nothing but move what falls due on business days
  (due dates, payment runs), as payment terms' days do, so a change is
  operational (tier 2) and a holiday's name cosmetic (tier 1). The section
  stays in the document, which owns no table for it: other contexts read
  it from the active document with `config.Config.Holiday(company, date)`.
  Which weekdays are a company's weekend is not in the section yet; the
  first context that counts business days decides it (see Consequences).
- **Tax rates** (amended by T61a, ADR-0049 §3): `tax.yaml` holds the tax
  codes in the configuration's own schema (`config.TaxCode`): direction,
  kind, UN/CEFACT category and components (key, jurisdiction, account key,
  recoverable share, account), each with **all** its dated rates, future
  ones included. Validation is the configuration's (`config.CheckTaxCodes`)
  against the pack's accounts, and a pack's code always has components (the
  undated single-rate form is not a pack's). The applier writes the codes
  as they are, whatever the date of application, and the ledger sets up
  every rate ahead; the earlier "rate in force, later rates noted, apply
  again" behaviour is gone.
- **Returns** (T61h, ADR-0049 §11): an optional `returns.yaml` holds the
  country's tax return definitions (each return type's boxes: selections
  of tax items or expressions over earlier boxes, and the prior-period
  threshold), validated (`countrypack.Validate`, the expressions compiled
  by the CEL engine of ADR-0046) and hashed with the pack like every file;
  packs released before have none. Applying a pack writes nothing of it:
  a draft return reads the definition from the pack version the company
  adopted (`packs`). **Unreleased packs** for a demonstration or a test
  are served from a directory (`ERP_PACKS_DIR`, `Library.AddUnreleased`:
  hashed but not checked against the hash list, logged as not released, a
  released directory refused), and production refuses the setting at
  start, so it applies only released packs; the walkthrough serves a copy
  of the latest Canada pack with an illustrative return through it.
- **Scope**: tax calculation and returns are T61, e-invoicing T65 and the
  regulatory agent that proposes new pack versions T71. Steps: T62a schema,
  loader and validation; T62b apply and merge with provenance; T62c the
  `calendars` section (company → holidays); T62d the API; T62e the Canada
  pack; T62f the UAE pack.

- **The Canada pack** (`packs/ca/2026.1`, T62e) has a starter chart of
  accounts with every account determination key (and the QST receivable
  and non-recoverable PST accounts its taxes will need), number ranges for
  every document type the ERP numbers, four net payment terms and the
  Canada Labour Code's general holidays of 2026 and 2027. Its tax codes
  wait for the tax code model of ADR-0049 (T61a): the GST/HST, QST and PST
  codes and rates sourced for it are in `packs/ca/TAX-PENDING-T61a.md`,
  outside the pack directory, which holds only the pack's files. It
  carries no sign-off, so production refuses to apply it until the tax
  specialist has confirmed the tax codes once written, and the day
  observed when a holiday falls on a weekend; which provincial holidays a
  company observes is open (the pack, per country, has the federal ones). A payment term with a cash discount is left out: the
  document's payment terms are shared by its companies, and a discount
  term would require a cash discount account in each of them.
- **As built (T62g): Canada 2026.4** is the version before it, 2026.3
  (accounts, holidays, number ranges, payment terms, `cash.yaml`, the tax
  settings of `tax.yaml` and the sources, SOR/91-45 among them; batch 33
  rebased T62g's version from 2026.2 onto it) plus the ten purchase codes
  of `packs/ca/TAX-PENDING-T61a.md` in `tax.yaml`: G5 (GST), HON, HNB,
  HNL, HNS, HPE (HST), QST and PBC, PMB, PSK (provincial sales taxes), each
  input, standard, category S, one component (`gst`, `hst`, `qst`,
  `pst_bc`, `rst_mb`, `pst_sk`) with its ISO 3166-2 jurisdiction and every
  dated rate of the file, the ended ones included (`valid_to` the day before
  the next rate). Decided by default: the recovery is the file's proposal,
  GST and HST to 1300, QST to 1310, wholly recoverable; PST
  `non_deductible` with no account, so it is expensed on the purchase's own
  line (ADR-0049 §3, §6), not on 6400, which the file proposed and a
  component cannot name (6400 stays in the chart); the QST and PST rates
  start on 2026-01-01, the conservative start the file states, as their
  earlier starts were not fetched from an authority; no combined GST+PST or
  GST+QST code, as the file has none. `TestCanadaPackTaxCodes` reads the
  file's table and the pack and fails on any difference in codes, rates,
  dates, recovery (account and recoverable share) or jurisdiction (the
  province the Tax column names), and checks that every source the file cites is in the
  manifest. The earlier versions stay released as they were.
  **Sign-off is a separate step.** 2026.4 carries no `sign_off`, so
  production refuses to apply it (409 `config.pack_not_signed_off`) and
  elsewhere it applies with a note. A released version never changes, so
  the specialist's sign-off is a later version (2026.4 plus `sign_off`, or
  with what the specialist corrects), its own tracker leaf split from T62g.
  For the specialist: the recovery accounts above (proposals); PST expensed
  on the item rather than 6400; the QST and PST start dates (reported
  elsewhere, not fetched: QST 9.975% from 2013-01-01, BC PST 7% from
  2013-04-01, MB RST 7% from 2019-07-01, SK PST 6% from 2017-03-23);
  whether combined codes are wanted for purchases in BC, MB, SK and QC,
  which carry GST and a provincial tax together; the holiday items above.
- **As built (T62h): Canada 2026.5** is 2026.4 signed off. The product
  owner reported on 2026-11-02 that the tax specialist, **Musto Ibrahim**,
  approved the items put to them, signed 2026-11-02; the approval covers
  what exists as written and builds nothing new. So 2026.5's files are
  2026.4's, byte for byte, but `pack.yaml`, which gains `sign_off:
  {tax_specialist: "Musto Ibrahim", date: 2026-11-02}`, its version and its
  header comment (`TestCanadaPack2026_5`); production applies it, and
  2026.4 stays released, unsigned. Confirmed as written: the ten codes and
  their rates; the recovery accounts above (no longer proposals); PST
  expensed on the purchase's own line; the QST and PST rates from
  2026-01-01, the start the pack states (the earlier starts reported
  elsewhere were among the items confirmed but are not in the pack: a
  later version, if a document dated before 2026 needs them); no combined
  codes for purchases in BC, MB, SK and QC, as built; and 2026.3's tax
  settings (ADR-0049 §6, §8 As built). The remarks in `tax.yaml` calling
  these proposals or pending stay, as the file is 2026.4's. **Still open**,
  as questions the approval of what exists does not answer: the day
  observed when a holiday falls on a weekend (the pack lists each holiday
  on its own date) and which provincial holidays a company observes (the
  tenant adds them). **Decided by default:** nothing pins a pack version
  (the API lists every released version and a change request names the
  one it applies), so new companies take 2026.5 by being offered it as the
  newest Canada version, and companies on 2026.4 move by applying 2026.5,
  an ordinary upgrade; the walkthrough's illustrative copy of the latest
  pack drops the `sign_off`, since the specialist reviewed the released
  pack, not the walkthrough's illustrative return.
- **As built (T61j): Canada 2026.6** is 2026.5 plus `returns.yaml`, the
  GST/HST return (ADR-0049 §11 As built, T61j): every other file is
  2026.5's byte for byte and the manifest gains the CRA's instructions for
  the return as its first source and drops the `sign_off`
  (`TestCanadaPack2026_6`), so production refuses to apply it until the
  tax specialist confirms the lines and their mapping
  (`packs/ca/TAX-PENDING-T61j.md`, `TestCanadaPackGST34`), and 2026.5
  stays the signed-off version. **Decided by default**, as for 2026.4: a
  version with content the specialist has not seen is released unsigned
  and signed off as the next version. It is the newest Canada version, so
  the API lists it first by version and a tenant in production applies
  2026.5 until the signed version exists. The walkthrough applies it
  instead of an unreleased copy with an illustrative return.
- **As built (T62f): the UAE pack, 2026.1** (`packs/ae/2026.1`): a starter
  chart with every account determination key (1300 VAT recoverable, 2300
  VAT payable), the Canada pack's number ranges and net payment terms (the
  same keys and terms, so a document holding both merges them without a
  conflict), no holidays and no `returns.yaml` (the VAT 201 is T61k). Its
  `tax.yaml` holds nine VAT codes in ADR-0049's component schema, every
  component of AE with one rate from 2026-01-01: purchases V5 (5%, wholly
  recovered), VB5 (5%, blocked: recovering 0), RC5 (reverse charge on
  imported services and goods without a customs declaration), DR5 (the
  domestic reverse charge) and IM5 (import VAT from a customs declaration),
  each self-assessed pair owed on 2300 and recovered on 1300; sales S5 (5%,
  one code levying the emirate each line names, ADR-0049 §7 As built), Z0,
  E0 and OS (zero-rated, exempt and out of scope, rate 0); and the tax
  settings of ADR-0049 §6 As built (above AED 10,000 the supplier's TRN,
  `pending_sign_off: true`), `rate_type: average` and `rounding: document`.
  Each fact's basis is in `packs/ae/TAX-PENDING-T62f.md` as ADR-0049
  records it, nothing fetched again and nothing from memory;
  `TestUAEPackTaxCodes` compares its table with the pack and its links with
  the manifest's sources. The manifest lists the sources ADR-0049 links (the
  Ministry of Finance on the 2026 amendments, the FTA's Designated Zones
  guide and its clarification on electronic devices, the two secondary
  VAT 201 summaries); the VAT Law and its Executive Regulation, which
  ADR-0049 names without a link, are cited in the pending file alone. It
  carries no `sign_off`, so production refuses to apply it (409
  `config.pack_not_signed_off`) and elsewhere it applies with a note; the
  sign-off is a later version, its own tracker leaf split from T62f.
  **Decided by default:** every rate starts on the pack's `valid_from`,
  2026-01-01, as Canada's QST and PST did, the earlier start not being
  recorded; the pack holds sales codes, unlike Canada's (whose sourced file
  had purchase codes only), since ADR-0049 records the UAE's output
  categories, §7 As built designed the emirate-levying code and T61k's boxes
  select them; recovery wholly on 1300, blocked input a fixed share of 0
  (P8); a self-assessed code keys its owed part `vat_due` and its claim
  `vat`, the key of the ordinary component, as Canada's return mapping keys
  a claim; IM5 carries no UN/CEFACT category (ADR-0049 records none for an
  import); the rate type and rounding as Canada's, both pending (VAT Law
  Art. 69 is cited from memory in ADR-0049, no rounding rule is recorded);
  no holidays, as no source is recorded (several follow the Hijri calendar)
  and the company's weekend has no place yet (Consequences). The
  walkthrough's step 39 creates the UAE company (5000, AED, Asia/Dubai),
  adds it to the configuration with a house bank and the accounts its
  account determination needs (the cash discount account among them, as
  the document's terms grant discounts), named as the pack names them,
  applies the pack (two tier 4 changes), records an illustrative AED/CAD
  rate (the ledger converts into the tenant's currency) and posts a
  supplier invoice of AED 12,600.00 without the supplier's TRN (warned)
  and one of AED 4,200.00 (none).
- **As built (T61k): UAE 2026.2** is 2026.1 plus `returns.yaml`, the VAT
  201 (ADR-0049 §11 As built, T61k): every other file is 2026.1's byte for
  byte, and the manifest's two VAT 201 sources (the secondary summaries
  ADR-0049 cites) name the boxes they now back (`TestUAEPack2026_2`). It
  carries no `sign_off`, as 2026.1 does not, so production refuses both
  until the specialist confirms T62f's items and the boxes and mapping of
  `packs/ae/TAX-PENDING-T61k.md` (`TestUAEPackVAT201`); the sign-off is one
  later version for both. **Decided by default**, as T61j did for Canada:
  the return is a new version rather than a change to 2026.1, which is
  released. It is the newest UAE version; the walkthrough's step 39
  applies it.

## Consequences
- A pack upgrade never silently overwrites a tenant's decision; reviewers
  see each conflict as a note in the change request.
- Adding or correcting a pack is a release, reviewed and signed with the
  image; there is no runtime download of packs.
- A known future rate is in the pack and set up once, ahead (T61a); a rate
  change not yet in the pack is a new pack version.
- The base of an upgrade must still be in the image, so released versions
  are never removed (the hash list enforces it).
- Holidays are dated, not recurring: a pack lists them year by year, and a
  new version that stops listing a past year's holidays leaves them in the
  calendar with a note each, so a pack keeps the years it listed.
- Applying a pack is open to anyone who may propose a configuration
  change, and to agents at the propose level; what it changes still needs
  the approvals of its tier, and the company access check keeps a person
  to the companies they work in.
- A business day needs the company's weekend as well as its holidays
  (the UAE's differs from Canada's); it is added to the calendar, with
  its tier, by the first step that schedules by business days.
