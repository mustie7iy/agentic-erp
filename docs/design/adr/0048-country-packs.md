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
  new pack version, matched by account number, tax code, number range key,
  payment term key and account determination field. A change only in the
  pack is taken; a change only in the tenant's document is kept; a change in
  both is kept as the tenant has it, with a conflict note (unless both made
  the same change); an item the new pack removed is kept, with a note.
- **Provenance**: the document's `packs` section records, per company, the
  country, the pack version and the date it was applied on; the next
  upgrade reads its base from there.
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
- A known future rate is in the pack and set up once, ahead (T61a); a rate
  change not yet in the pack is a new pack version.
- The base of an upgrade must still be in the image, so released versions
  are never removed (the hash list enforces it).
