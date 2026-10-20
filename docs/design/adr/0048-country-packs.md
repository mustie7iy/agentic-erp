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
- A business day needs the company's weekend as well as its holidays
  (the UAE's differs from Canada's); it is added to the calendar, with
  its tier, by the first step that schedules by business days.
