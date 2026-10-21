# ADR-0048: Country packs

**Status:** accepted (T62, product owner 2026-10-20)

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
  (pure Go, importing only `config` and the shared kernel) loads, validates
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
- **Tax rates**: `config.TaxCode` keeps one rate until effective-dated tax
  codes come with T61, so the applier writes the rate in force on the date
  of application and notes the future rates; applying the pack again once a
  new rate is in force takes it (the base is converted at the recorded
  application date, so the change is seen as the pack's).
- **Scope**: tax calculation and returns are T61, e-invoicing T65 and the
  regulatory agent that proposes new pack versions T71. Steps: T62a schema,
  loader and validation; T62b apply and merge with provenance; T62c the
  `calendars` section (company → holidays); T62d the API; T62e the Canada
  pack; T62f the UAE pack.

- **The Canada pack** (`packs/ca/2026.1`, T62e) has a starter chart of
  accounts with every account determination key; purchase tax codes for
  GST (5%) and the HST of Ontario, New Brunswick, Newfoundland and
  Labrador, Nova Scotia (15%, then 14% from 2025-04-01) and Prince Edward
  Island, with the dates of the CRA's rate table, recovered on the input
  tax account; QST (9.975%, recovered on its own account) and the PST of
  British Columbia, Manitoba and Saskatchewan (7%, 7%, 6%, expensed as
  non-recoverable); number ranges for every document type the ERP numbers;
  four net payment terms; and the Canada Labour Code's general holidays of
  2026 and 2027. It carries no sign-off, so production refuses to apply it
  until the tax specialist has confirmed: the start dates of the PST and
  QST rates (given from 2026-01-01, as far as the cited pages vouch);
  whether PST is expensed and QST recovered as written; the
  day observed when a holiday falls on a weekend; and the provincial
  holidays and harmonized combinations a company needs (one tax code per
  line until T61, so a purchase in British Columbia, Manitoba, Saskatchewan
  or Quebec takes two codes, GST and the provincial tax). A
  payment term with a cash discount is left out: the document's payment
  terms are shared by its companies, and a discount term would require a
  cash discount account in each of them.

## Consequences
- A pack upgrade never silently overwrites a tenant's decision; reviewers
  see each conflict as a note in the change request.
- Adding or correcting a pack is a release, reviewed and signed with the
  image; there is no runtime download of packs.
- Until T61, a rate change needs the pack applied again after it takes
  effect; the notes of the earlier application name the date.
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
