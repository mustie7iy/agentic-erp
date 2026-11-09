# ADR-0061: E-invoicing and statutory reports

**Status:** accepted (T65 design step, 2026-11-05; decided by default under
ADR-0053 d; `go-reviewer` approved in round 3 with no blocking finding).
The decisions taken by default and the questions for the product owner
(still open) are listed at the end; the slices T65a-T65h are in the
tracker.

## Context
T65 is "E-invoicing and statutory reports" (07 §7.6): "UAE e-invoicing
through a certified provider or access point; Canada has no e-invoicing
mandate, so statutory reports only; the statutory reports of both pilot
countries. Lane S." The phase 2 decisions of the product owner (07 §7.6,
2026-10-18) add: "Peppol and e-invoicing go through a certified provider,
not a network connector of our own", and "archiving stays in phase 9".

The tracker's group "E-invoicing and statutory reporting" has three leaves,
not four: "Peppol network connector" and "SAF-T and statutory reports",
both in the pilot scope (product owner 2026-10-31), and "Country formats
(XRechnung, FatturaPA, ...)", outside it. This ADR details those three and
adds nothing beyond them.

### What the repository records about the law
Every regulatory statement in this ADR comes from ADR-0049 or 07, or is
marked **to confirm** and listed under "Facts for a specialist" at the
end. ADR-0049 itself took the UAE facts from secondary sources (its
Sources: EY's summary of the Ministerial Decisions, Zoho and ClearTax for
the VAT 201), and says so. No external source was read for this ADR (no
network in the lane).

- **UAE** (ADR-0049, Context): "E-invoicing is mandatory from 2027-01-01
  for businesses with revenue of AED 50m or more (pilot from 2026-07-01,
  voluntary adoption allowed; smaller businesses later), through
  accredited service providers on a Peppol-based five-corner model
  (Ministerial Decisions No. 243 and 244 of 2025)." T65 "renders and
  transmits documents (PINT AE e-invoices via an accredited provider,
  received e-invoices fed to capture)" (ADR-0049 §1, boundary with T65).
  A tax invoice carries, among others, the supplier's and registered
  recipient's TRNs, a sequential number, the rate and tax per supply in
  AED and the reverse charge statement where it applies (Executive
  Regulation Art. 59, as ADR-0049 records it from secondary summaries, for
  the specialist to confirm).
- **UAE statutory reports**: ADR-0049 §1 puts "the UAE's FTA Audit File
  and other statutory reports" in T65. It records nothing of the audit
  file's content, format or source data.
- **Canada**: "No e-invoicing mandate (07 §7.6, T65)" (ADR-0049, Context).
  Neither 07 nor any ADR names a Canadian statutory report other than the
  GST/HST return (and the FPZ-500 for a Quebec registrant), which are T61's.
- **Pilot facts not known yet** (ADR-0049, "Information needed from the
  pilot"): "whether the UAE company's revenue reaches AED 50m (e-invoicing
  from 2027-01-01, T65)" and its billing system. The revenue is now known
  (below AED 50m, the next bullet); the billing system has a planning
  default (D8, next bullet).
- **The mandate's timeline** (a fact, not a decision; the UAE tax
  specialist, po:2026-11-09, from Ministerial Decision No. 244 of 2025 on
  the Implementation of the Electronic Invoicing System as amended,
  Ministerial Decision No. 66 of 2026 the amendment the specialist names):
  a business with revenue of AED 50 million or more had to appoint an
  accredited service provider by 30 October 2026 and must go live by
  1 January 2027; a business below AED 50 million appoints one by
  31 March 2027 and goes live on 1 July 2027.
- **The pilot's UAE company** (product owner, po:2026-11-09; confirmed facts,
  not defaults): its revenue is **below AED 50 million**, and it has
  appointed no accredited service provider yet. Its dates are therefore to
  appoint one by **31 March 2027** and to go live on **1 July 2027**. This
  overrides the pilot customer questionnaire's planning default for D7
  (AED 50 million or more). Its billing system (D8) has a planning default
  only, to be confirmed by the customer before go-live: both companies bill
  from SAP S/4HANA SD, one journal per billing document read through the
  API.

### What the ERP has today
- **Capture** (T34e, 03 §3.9, `internal/capture`,
  `internal/payables/capture.go`): `POST /v1/supplier-invoices:capture`
  sends a PDF or image through the model gateway (ADR-0036), which
  extracts a fixed JSON schema (supplier name and tax id, number, date,
  purchase order, currency, lines with description, quantity, amount,
  order line and a printed tax rate, tax and gross); payables maps the
  lines to the order's items (by the printed order line, or by position
  when the counts agree; each item takes the order item's unit and tax
  code), proposes a code for a line whose printed rate its order item's
  code does not charge (`capturedTax`: the one input code charging it,
  else the invoice is held for a person), and enters the invoice `parked`
  (or `on_hold` with the reasons) with source `capture`; the document is
  kept write-once in object storage (ADR-0016, `documents/<sha256>`) and
  served by `getSupplierInvoiceDocument`, byte for byte, XML included.
  Capture needs an order (`payables.capture_needs_order`) and refuses a
  document captured before (`payables.already_captured`). The supplier
  invoice's `source` is an open enumeration.
- **Tax** (ADR-0049, T61): dated tax codes with components; each code has
  a `category`, the UN/CEFACT 5305 code an e-invoice carries
  (`config.TaxCode.Category`: S, Z, E, AE, K, G, O, L, M, B), put there in
  T61a "so T65 needs no second mapping"; tax items per code with every
  entry (`ledger.tax_item`, `ledger.TaxItems`); the calculator
  (`internal/tax`); the tax date (P6); the supplier's stated tax held to
  the calculated tax within a per-company tolerance (P5).
- **Tax filing** (`internal/taxfiling`, schema `tax`, 03 §3.20):
  registrations per authority (CRA, RQ, FTA), their periods computed on
  read, and returns prepared, approved and recorded as filed by people
  (P3: no electronic filing).
- **Connectors** (`integration.connector`, 03 §3.13): kinds in an open
  enumeration; settings without secrets, the secret by `secret_ref`
  (ADR-0017) read at use; fail closed; a kind not certified for production
  refused there (`integration.ebics_not_certified`, ADR-0038).
- **A fetch pattern**: bank statements fetched by SFTP (ADR-0047, T49d):
  the workflow dispatcher starts one `FetchBankStatements` workflow per
  interval and channel, each file recorded once and archived on the
  server only after the import committed, a lost acknowledgement repaired
  by the next fetch.
- **Egress** (`internal/platform/egress`, ADR-0031, ADR-0041): the way out
  of the cell to tenant-named addresses: public addresses checked at dial
  time, no proxy or redirects, system roots, bounded time and a 64 KiB
  response.
- **Country packs** (ADR-0048): Canada released; the UAE pack is T62f, not
  built, and with it the UAE's input codes and their categories.
- Nothing issues sales invoices: "issuing sales invoices (phase 3)"
  (ADR-0049 §1); the pilots' sales arrive as journal entries from their
  billing systems (P2, T61f).

## Decision

### 1. Scope of T65
**In:**
- **Receiving** UAE suppliers' e-invoices and credit notes through the
  tenant's accredited provider, entering each as a supplier invoice by
  capture's rules, from the structured data instead of a model's reading
  of a picture (§4-§7).
- **The provider as a port** behind a connector of a new kind, with a
  stand-in for tests and the walkthrough; the adapter of a real provider
  once the product owner names one (§3, question Q1).
- **The FTA Audit File** of a UAE registration, built when the specialist
  supplies its specification (§10).
- **The design of sending** (§9), built with issuing sales invoices in
  phase 3.

**Out:**
- Issuing sales invoices, and so sending e-invoices before phase 3 (§9,
  question Q2 for the 2027 date).
- A Peppol access point of our own (07 §7.6, phase 2 decisions).
- Formats other than PINT AE (the leaf "Country formats"): XRechnung,
  FatturaPA, SAF-T and the like are built when a country pack needs one,
  as one more format behind the same port (§3).
- Canadian statutory reports until they are named (question Q4); the
  GST/HST return and the FPZ-500 stay T61's.
- Electronic filing to an authority (ADR-0049 P3), archiving (phase 9),
  retention policies (T66), monitoring regulatory change (T71).
- Screens in the web workspace (headless first, ADR-0053).

**Refining ADR-0049's boundary.** ADR-0049 §1 says received e-invoices
are "fed to capture". Here they are fed to capture's *rules*, not its
operation: `captureSupplierInvoice` takes a picture to a model; an
e-invoice needs no model and arrives by fetch, not by a person's upload.
Payables shares its entering code between the two (§6), so an e-invoice
is matched, held and entered exactly as a captured document is. ADR-0049
§1 carries a note pointing here.

### 2. The `einvoicing` context: a leaf
A new context `einvoicing` (schema `einvoicing`, `internal/einvoicing`)
owns e-invoice documents, their statuses and the provider port. It is a
leaf, as `assets` and `closing` are (ADR-0057, ADR-0058): no context
imports it; it reaches others through ports the composition root adapts:

- `einvoicing.Connectors` over integration: the connector's settings and
  resolved secret, read at use, failing closed.
- `einvoicing.Payables` over payables' published API, two functions:
  `EInvoices.Enter` enters a supplier invoice from structured fields (§6);
  `EInvoices.Attach` makes an e-invoice the document of an invoice a
  person entered by hand (§7).
- `einvoicing.Companies` over tenancy and tax filing: a company's code,
  name and currency and its FTA registration's number (compared, never
  shown).
- `einvoicing.Suppliers` over partner: suppliers by tax identifier
  fingerprint (`partner.tax_identifier.value_fingerprint`), never the
  number in clear, and their names.

Nothing else changes direction. `internal/layout` records the leaf and
its allowed imports.

### 3. The provider port and its connector
```go
// Provider is an accredited e-invoicing provider as one tenant's access
// point. Implementations: the stand-in (tests, walkthrough) and, once
// chosen, one adapter per provider (Q1).
type Provider interface {
    // Pending lists the documents received and not yet acknowledged.
    Pending(ctx context.Context) ([]Message, error)
    // Fetch reads one document's bytes, at most MaxDocument.
    Fetch(ctx context.Context, id string) ([]byte, error)
    // Acknowledge tells the provider the document is kept; acknowledging
    // twice is not an error.
    Acknowledge(ctx context.Context, id string) error
    // Send and Status are phase 3's (§9).
}
```
- **Connector kind `einvoice_provider`** (added to the open enumeration).
  Settings: `adapter` (the implementation; `standin` until Q1 names a
  provider), `base_url` (https), `participants` (per company code, the
  company's participant identifier at the provider; its scheme is to
  confirm), `format` (`pint_ae`, the only one); `secret_ref` names the
  credentials. Created and changed with `integration.connector.manage`,
  as every connector.
- **Production refuses the stand-in**: creating or enabling an
  `einvoice_provider` connector whose adapter is `standin` there answers
  422 `integration.adapter_not_certified`, as EBICS is refused until
  certified with a bank (ADR-0038); a stored one is refused at use.
- **Egress**: the adapter calls the provider through the egress client
  (public addresses only, no redirects, system roots), with its own
  response limit, `MaxDocument` = 10 MiB (the egress client gains a
  per-client limit; the webhook client keeps 64 KiB), and timeouts of
  5 s to connect and 60 s per call. The provider's real limits are
  checked against these when Q1 is answered.
- **Pull, not push.** The ERP polls; it opens no endpoint for the
  provider to call. An adapter for a provider that only pushes needs an
  inbound endpoint, which is a security-posture decision (question Q3).
- **Formats behind the port.** A `Format` (parse, and in phase 3 render)
  per document syntax; `pint_ae` is the only one in T65. A later format is
  one more implementation and a connector setting, not a change to the
  context.

### 4. Receiving: the fetch
A fetch has two kinds of activity, each in its own transaction, so that
keeping the bytes never waits on (or is undone by) entering them:

- **Record** (per pending message): fetch the bytes, keep them write-once
  in object storage (`documents/<sha256>`, ADR-0016; a `Put` of a key
  already there is not an error), insert the document
  (`einvoicing.einvoice`, unique per tenant, connector and the provider's
  message id, `ON CONFLICT DO NOTHING`) as `received`, with the audit
  record and `EInvoiceChanged`; after the commit, acknowledge it to the
  provider and set `acknowledged_at`.
- **Process** (per `received` document of the connector's companies,
  oldest first): read it `FOR UPDATE` where its status is still
  `received`, check it (§5) and enter it (§6) or refuse or hold it, with
  the audit record and the event. A document already processed is
  skipped, so processing runs any number of times. Every fetch processes
  every `received` document, so documents recorded before a later slice
  added a check or the entering are processed by the first fetch after
  it.

**Idempotent** (ADR-0012), each with a run-twice test:
- A message already recorded is not inserted again; its acknowledgement
  is sent again, so a lost acknowledgement is repaired by the next fetch.
- A message whose bytes differ from the recorded ones under the same id
  is not recorded again: the row keeps the first bytes and sets
  `changed_hash` to the new bytes' hash with an update guarded by
  `changed_hash IS DISTINCT FROM $new`; the audit record and
  `EInvoiceChanged` are written only when that update changed a row, so a
  redelivery of the same changed bytes (its acknowledgement lost)
  publishes nothing again.
- Processing changes a document only from `received` (the guard above).

**Scheduling.** The workflow dispatcher starts, once per
`ERP_EINVOICE_FETCH_INTERVAL` (default 1h, 0 none) and active
`einvoice_provider` connector, the `FetchEInvoices` workflow under an id
naming the connector and the interval, so replicas start each once
(ADR-0047's pattern). `POST /v1/e-invoices:fetch` (`connector_id`)
starts one now and answers an Operation whose result counts the
documents it recorded, found already recorded, found changed, entered,
held and refused, and names itself on each document it recorded
(`fetch_operation_id`, a filter of the list).

The fetch runs as a system principal acting for the connector's
companies (as the statement fetcher does). Each activity is retried (5
attempts) on infrastructure errors only (the provider unreachable, the
store or the database failing); one message's failure does not stop the
others, and the workflow fails naming them for the next fetch. A
disabled connector or an unresolved secret fails the fetch without
retries. Gauges: `erp_einvoices_held` and `erp_einvoices_unacknowledged`
(recorded over an hour ago, not yet acknowledged).

### 5. Validation
The provider's own validation of PINT AE before delivery is to confirm;
the ERP does not rely on it. A recorded document is **refused** (status
`refused` with a problem code, kept, never entered) when:

- it is above `MaxDocument` (`einvoicing.document_too_large`, recorded
  without its bytes: hash and size null, the provider's declared size in
  the problem's detail when it gives one), not well-formed XML, or declares a DTD or an
  external entity (parsed with no DTD processing, so no entity expansion:
  `einvoicing.unreadable`);
- it is not an invoice or a credit note in the configured format
  (`einvoicing.unsupported_document`);
- a field the ERP reads is missing or malformed
  (`einvoicing.missing_field`, naming it): the document's type, number and
  issue date, its currency, the seller's and buyer's TRNs, the order
  reference if any, a credit note's invoice reference if any, and per
  line its description, quantity, unit code, net amount, tax category,
  rate and order-line reference if any, the tax total per category and
  the document totals. Which PINT AE elements carry each is taken from
  the specification the specialist or provider supplies (to confirm),
  cited by T65c;
- an amount has more decimals than the currency's minor unit
  (`einvoicing.amount_precision`), or the arithmetic that involves no
  tax rule does not hold: the lines' net amounts sum to the net total,
  and net plus tax is the gross (`einvoicing.totals_inconsistent`, naming
  the figures);
- its buyer is not a company of the connector (the receiving participant
  is none of `participants`) or the buyer's TRN is not that company's FTA
  registration number (`einvoicing.not_addressed_to_company`).

Whether the tax per category is right is **not** a ground for refusal:
the supplier's stated tax is held to the calculated tax by P5's
tolerance when the invoice posts, and before that by capture's check,
which holds the invoice (§6). How UAE tax is rounded (per line or per
category) is on ADR-0049's list for the specialist and stays there.

Validation against PINT AE's full rule set (its published validation
artefacts) is to confirm: if the provider does not guarantee it, a slice
vendors the artefacts from a cited source. Refused documents are not
answered to the supplier: whether the UAE model expects a buyer's
response through the network is to confirm; until then a person takes it
up with the supplier.

**Answered by the UAE tax specialist (po:2026-11-09, C1-C8; [H] from an
official source, [M] official source plus interpretation, [L] not found in
an official source, to confirm with the FTA, the Ministry of Finance or the
provider).** What changes T65c-T65e is in their tracker leaves (src
po:2026-11-09).
- **The binding specification (C1, [H]).** The Ministry of Finance sets
  the requirements; the specification is PINT AE, published by OpenPeppol
  on its behalf. Documents to cite: Ministerial Decision No. 243 of 2025 on
  the Electronic Invoicing System
  (https://mof.gov.ae/wp-content/uploads/2025/09/Ministerial-Decision-no.-243-of-2025-on-the-Electronic-Invoicing-System.pdf);
  Ministerial Decision No. 244 of 2025 on its implementation, amended by
  Ministerial Decision No. 66 of 2026
  (https://mof.gov.ae/wp-content/uploads/2025/09/Ministerial-Decision-No.-244-of-2025-on-the-Implementation-of-the-Electronic-Invoicing-System.pdf);
  UAE Electronic Invoice, Mandatory Fields, V1.0, 23 February 2026
  (https://mof.gov.ae/wp-content/uploads/2026/02/UAE-Electronic-Invoice-mandatory-fields_V-1.0-23Feb2026.pdf);
  the UAE Electronic Invoicing Guidelines (V1.1 reported June 2026,
  https://mof.gov.ae/einvoicing/); and PINT AE Billing, invoice and credit
  note (https://docs.peppol.eu/poac/ae/2025-Q2/pint-ae/). The provider's
  validation is not the binding specification: ask it which PINT AE
  release it validates against.
- **Version and syntax (C2).** UBL 2.1 XML [H]. PINT AE Billing was first
  released on 9 June 2025 (OpenPeppol's 2025-Q2 release, 1.0.0), with
  v1.0.1 on 31 July 2025 [M]; no replacement date was found [L].
  Record the document's specification identifier (IBT-024) and the release
  validated against, and check https://docs.peppol.eu/poac/ae/ for a newer
  release folder before go-live.
- **The fields §5 reads (C3; business terms [H], UBL paths [M], to confirm
  against the syntax binding; a credit note uses `CreditNote` element
  names):** document type IBT-003 `cbc:InvoiceTypeCode` or
  `cbc:CreditNoteTypeCode` (M); number IBT-001 `cbc:ID` (M; also BTAE-07,
  the unique identifier number, M); issue date IBT-002 `cbc:IssueDate` (M);
  currency IBT-005 `cbc:DocumentCurrencyCode` (M); seller's TRN IBT-031
  `cac:AccountingSupplierParty/cac:Party/cac:PartyTaxScheme/cbc:CompanyID`,
  tax scheme `VAT` (M for a VAT-registered seller); buyer's TRN IBT-048
  `cac:AccountingCustomerParty/cac:Party/cac:PartyTaxScheme/cbc:CompanyID`
  (M on a B2B tax invoice to a registered buyer); order reference IBT-013
  `cac:OrderReference/cbc:ID` (O); the invoice a credit note refers to
  IBG-03, IBT-025
  `cac:BillingReference/cac:InvoiceDocumentReference/cbc:ID`, its date
  IBT-026 (C); per line the name IBT-153 `cac:Item/cbc:Name` and
  description IBT-154 `cac:Item/cbc:Description` (M, both), quantity
  IBT-129 `cbc:InvoicedQuantity` with its unit IBT-130 `@unitCode` (M), net
  amount IBT-131 `cbc:LineExtensionAmount` (M), tax category IBT-151
  `cac:Item/cac:ClassifiedTaxCategory/cbc:ID` and rate IBT-152
  `…/cbc:Percent` (M), order line reference IBT-132
  `cac:OrderLineReference/cbc:LineID` (O; IBT-183 the line's order
  reference); tax total per category IBG-23, IBT-116
  `cac:TaxTotal/cac:TaxSubtotal/cbc:TaxableAmount`, IBT-117
  `…/cbc:TaxAmount`, IBT-118 `…/cac:TaxCategory/cbc:ID`, IBT-119
  `…/cbc:Percent` (M); totals IBT-109
  `cac:LegalMonetaryTotal/cbc:TaxExclusiveAmount` (IBT-106 the lines' sum),
  IBT-110 `cac:TaxTotal/cbc:TaxAmount` in the document's currency, IBT-112
  `cac:LegalMonetaryTotal/cbc:TaxInclusiveAmount` and IBT-115
  `cbc:PayableAmount` (M). Also mandatory: the electronic addresses
  IBT-034 and IBT-049, the seller's and buyer's names, address line 1,
  city, country subdivision and country code, IBT-023, IBT-024 and the
  payment means IBT-081. (M mandatory in an electronic tax invoice by the
  Ministry's list, O optional, C conditional.)
- **Codes and credit notes (C4, [M]).** An invoice is `380`, a credit note
  `381`, sent as a UBL `CreditNote`; other codes of the PINT AE code list
  (such as self-billing) stay refused. A tax credit note must name the
  invoice it corrects (IBG-03, IBT-025), and routing one that names none to
  a person is safer than refusing it, even if the rules allowed it empty.
- **Amounts in AED (C5; terms [H], paths [M]).** The tax accounting
  currency IBT-006 `cbc:TaxCurrencyCode` (`AED`) and the invoice's total VAT
  in it IBT-111 (a second `cac:TaxTotal/cbc:TaxAmount` with
  `@currencyID="AED"`), required when IBT-005 is not AED; the exchange rate
  BTAE-04 (0..1); per line BTAE-08, the VAT line amount in AED, and
  BTAE-10, the line amount in AED (IBG-39), both listed as mandatory in the
  Ministry's field list (#48, #49); BTAE-20 the total with VAT in AED (0..1).
  The VAT 201 reads IBT-111, and BTAE-08 reconciles it per line; whether the
  rules enforce the AED fields only on a document not in AED is to confirm
  in the Schematron.
- **The buyer's identifier (C6, [H]).** Scheme `0235` (businesses
  registered in the UAE); the identifier is the TIN, the first 10 digits of
  the 15-digit TRN (the Corporate Tax TRN): TRN `100123456700003` is
  endpoint `0235:1001234567`. A member of a VAT tax group uses the first 10
  digits of its own Corporate Tax TRN, not the representative's. So
  `not_addressed_to_company` is decided on the IBT-049 endpoint (0235 and
  the company's TIN), and IBT-048 (the buyer's VAT TRN, a tax group's for a
  member) is checked apart, not required to equal it digit for digit.
- **Validation rules (C7).** Published [H]: Schematron files for invoices
  and credit notes and a resources package, linked from
  https://docs.peppol.eu/poac/ae/2025-Q2/pint-ae/. Their licence [L]:
  OpenPeppol's validation artefacts are usually under an open licence
  (Mozilla Public License 2.0 for Peppol BIS artefacts); read the package's
  LICENSE before shipping them.
- **Responding to a refused e-invoice (C8, [L]).** No business-level
  response obligation was found: the Peppol network has a Message Level
  Response between providers, but no Ministry requirement that the buyer
  accept or reject was found. A person contacting the supplier stays (6.
  under "Decided by default"); ask the provider whether it returns such
  responses automatically on a validation failure.

### 6. Entering: capture's rules without the model
A valid document is entered through `payables.EInvoices.Enter`, in the
processing transaction. Payables builds its input as capture builds an
extraction's and enters it with capture's code from there on:

- **No model call**: the fields are read, not extracted, so the AI policy,
  the model gateway's budgets and `documents_to_models` do not apply.
  Source `einvoice` (added to the open enumeration); the invoice's
  document is the XML, served by `getSupplierInvoiceDocument` as today.
- **Kind**: an invoice is `invoice`; a credit note is `credit_memo`,
  naming as its reference the invoice whose number it cites, by
  capture's rule (03 §3.9: a memo citing none corrects none; one citing a
  number that is no posted invoice of the supplier is entered `on_hold`).
- **Order and supplier**: the order reference names the purchase order,
  as capture's `purchase_order` does; the seller's TRN must be a tax
  identifier of the order's supplier (compared by fingerprint).
- **Items**: as `capturedItems` maps an extraction's lines: by the
  order-line reference, or by position when the counts agree; each item
  takes the order item's unit and tax code; the line's quantity and net
  amount as stated. The document's unit code is kept in the fields read
  and not mapped to the ERP's units (its code list is to confirm).
- **Tax codes from categories** (ADR-0049 §1): capture's `capturedTax`
  with the category as a further filter. The order item's code stands
  when its category is the line's and it charges the line's rate on the
  tax date (the invoice date, P6); else the line gets the one input code
  of the company with that category charging that rate, with a warning;
  with none or several, the invoice is entered `on_hold` with capture's
  reason, for a person (or the capture agent's proposal, T61d) to choose.
  A reverse-charge line (category AE) is matched among codes of kind
  `reverse_charge` by the same rule. The stated tax beyond the tolerance
  holds the invoice as capture's does (`payables.tax_mismatch`'s figures).
- **Amounts**: the stated net, tax and gross in the document's currency,
  as `money.Minor`.
- **Every refusal is a hold, never a failure**: any problem
  `EInvoices.Enter` answers (no order: `payables.capture_needs_order`;
  an unknown order or item, the supplier's TRN not the order's supplier,
  `payables.duplicate_invoice`, `payables.already_captured`,
  `payables.negative_amount`, a closed period, and any other) moves the
  document to `held` with that problem's code and detail; only
  infrastructure errors fail the activity. `Enter` runs in a savepoint (a
  nested transaction, as integration's load step does): on a problem the
  savepoint is rolled back, so neither a constraint error (several of
  these refusals are raised by the database) nor `Enter`'s partial writes
  survive, and the hold is written in the outer transaction, which
  commits. An unforeseen deterministic refusal therefore never blocks the
  fetch. T65d tests it with a refusal the database raises
  (`supplier_invoice_no_duplicates`), not only a pre-check. `:retry` does
  the same in its request's transaction.
- The document becomes `entered`, naming the invoice; the invoice is
  `parked` or `on_hold` and is then matched and posted as any other
  (`SupplierInvoiceCaptured` as today, with source `einvoice`).

### 7. Statuses and what people do
`einvoicing.einvoice.status`: `received` (recorded, not yet processed) →
`entered` | `held` | `refused`; `held` → `entered` | `held` (processed
again by `:retry`, with the new problem code and detail) | `dismissed`.
`refused`, `entered` and `dismissed` are final. A trigger holds the
transitions; the problem code and detail are set exactly when held or
refused, the reason exactly when dismissed. An entered or linked document
names its supplier invoice, and at most one document names an invoice:
a partial unique index on (`tenant_id`, `supplier_invoice_id`) where it
is set, mapped to 409 `einvoicing.invoice_already_linked`, with a schema
test.

- `POST /v1/e-invoices/{id}:retry` processes a held document again (after
  the order or the supplier's tax identifier was corrected) and answers
  the document: `entered` with its invoice, or still `held` with the
  problem of this attempt (200 either way; the problem is the document's
  state, not the request's).
- `POST /v1/e-invoices/{id}:link` (`supplier_invoice_id`) records that a
  person entered the held document by hand: the invoice is of the same
  company, supplier (by the seller's TRN) and supplier reference, and has
  no document of its own (422 `einvoicing.link_mismatch` naming which
  differs); `EInvoices.Attach` makes the XML its document, whatever its
  status, as a document never changes what was posted.
- `POST /v1/e-invoices/{id}:dismiss` (`reason`) closes a held document
  that will not be entered (sent in error, replaced by the supplier).
- Every write reads the document `FOR UPDATE` and checks its status
  (ADR-0008); 409 `einvoicing.einvoice_not_held` otherwise.

### 8. Archiving
The document's bytes are the legal original and are kept as received:
write-once under `documents/<sha256>` (ADR-0016), the row carrying the
hash, every read comparing them. They are kept as long as the tenant's
data (01: ten fiscal years online); retention periods and legal holds are
T66's, archiving phase 9's. Whether the UAE requires a longer period or a
particular form of keeping is to confirm; a compliance-mode retention
lock (as audit anchors have) is not applied by default, since erasure of
an offboarded tenant (ADR-0021) must still remove the documents.

### 9. Sending (phase 3)
Issuing sales invoices is phase 3 (ADR-0049 §1), so the ERP sends no
e-invoice before it; the pilots' sales invoices are issued, and in the UAE
transmitted, by their billing systems (P2). When sales invoices exist:

- an issued sales invoice is rendered as PINT AE from its lines and tax
  items (code, category, rate, base, tax: ADR-0049's boundary, no second
  mapping), checked by §5's rules before it is sent, and given to the
  provider (`Provider.Send`) in the outbox's pattern: the document is
  recorded with the invoice in one transaction and sent after the commit,
  idempotent by the document's id;
- what the provider and the authority report back (statuses, rejections
  and their reasons) and how a rejected or wrong e-invoice is corrected
  are provider-specific and to confirm with Q1 and the specialist; the
  ERP records each report as it comes (`Provider.Status`, polled);
- phase 3's sales design slices it; T65h is its tracker leaf.

### 10. Statutory reports: the FTA Audit File
- The repository records only the audit file's name (ADR-0049 §1). Its
  sections, fields, format and encoding, the data it is built from, the
  granularity it is asked for (a registration's periods, a company, a
  date range) and when it must be produced are **to confirm**. T65g
  starts only when the specialist supplies the FTA's specification; its
  contract follows that specification, and its golden test is signed off
  by the specialist as the returns are (ADR-0048).
- What is decided whatever the content: the tax filing context owns it
  (it owns the FTA registrations); generating answers an Operation whose
  `resource` is the stored file; the file is built by a workflow, stored
  write-once and recorded in `tax.audit_file` (what it covers, status
  `queued` → `running` → `ready` | `failed`, the file's hash and size, the
  format version, who asked); its data is read through the published
  ports of the contexts that own it (the ledger's `TaxItems` and journal
  reads; if the specification needs party master data, T65g adds a
  published partner port and the `internal/layout` entry for tax filing,
  and records the new dependency here).
- People only: an agent is refused with 403 `identity.agent_not_allowed`,
  since the file exports bookkeeping data in bulk; generating is audited
  and publishes `TaxAuditFileGenerated`; reading is not audited, as other
  downloads.
- **Canada**: no statutory report is named in 07 or an ADR beyond the
  returns; one is added as another report of this kind once named
  (question Q4). **SAF-T**: the leaf's name; neither pilot country is
  recorded as requiring it, so it is a format for a later country pack.
- **Answered by the UAE tax specialist (po:2026-11-09, C9).** The FTA Audit
  File (FAF) specification FAFv1.0.0, part of the FTA's requirements for
  tax accounting software [M]; no current public link was found on
  tax.gov.ae, so it is to be requested from the FTA (or through an
  FTA-registered tax agent) and kept on file. Format: one CSV file,
  comma-separated [M]; encoding UTF-8 in practice (English and Arabic
  text), not stated explicitly [L]; seven sections in the one file: company
  information, purchase listing, supply listing and general ledger, with
  their totals rows [M]; scope: a date range (period start and end in the
  header) for one taxable person (TRN) [M]; deadline: none fixed for the
  file itself, the FTA sets it in its request during an audit (it gives
  notice of a field audit in advance under the Tax Procedures law), so
  design for production within one working day [L]. T65g still starts from
  the specification itself; its tracker leaf records these facts.

### 11. Permissions
| Permission | Allows | Who |
|---|---|---|
| `einvoicing.einvoice.read` | list and read e-invoices; `/content` is `x-agent: false` | AP clerks, controllers; agents (fields only) |
| `einvoicing.einvoice.manage` | `:fetch`, `:retry`, `:link` (with `payables.invoice.read`), `:dismiss` | AP clerks; people and integrations, never agents (`x-autonomy: none`) |
| `tax.audit_file.generate` | generate an audit file | tax accountants; people only |
| `tax.audit_file.read` | read and download audit files | tax accountants, controllers; people only |

Migrations create them and give them to the roles that hold the
neighbouring payables and tax permissions, as earlier slices did.

### 12. Audit and events
Every status change of a document and every audit file generated writes an
audit record in its transaction (ADR-0011) and an outbox event (ADR-0007),
delivered to webhooks (ADR-0041): `EInvoiceChanged` (document, company,
status, problem code or reason, the invoice once entered, and the change
of bytes) on the `einvoicing` channel, and `TaxAuditFileGenerated` on
`tax`. Payables' `SupplierInvoiceCaptured` is published as today. Events
carry no document content and no TRN in clear (the last four characters,
as registrations show).

### 13. Agents
Agents propose only. The capture agent's code proposals (T61d) apply to
an e-invoice's invoice held for its codes, as to any held invoice; it may
read e-invoices' fields (`einvoicing.einvoice.read`) to recommend the
order for a held one, which a person acts on with `:link` or by
correcting the order and `:retry`. Agents never fetch, retry, link or
dismiss e-invoices, never send one (phase 3), and never generate or read
an audit file. No new agent type and no new evaluation suite in T65.

### 14. Budgets
- **Model**: none; the structured path makes no model call, so receiving
  costs no AI budget (ADR-0036), and the capture agent's proposals stay
  within its own budget.
- **Performance**: an nfr budget test (`nfr.Budget`, scripts/budgets.sh)
  for the processing transaction (check and enter one document) and one
  for an audit file at the pilot's volumes; the slices set them from
  measured medians and p95, as earlier budgets were set.
- **Provider cost**: per-message fees, if any, are part of Q1.

### 15. API and events (contract first)
- `GET /v1/e-invoices`: filters company, connector, supplier, status,
  `fetch_operation_id`, received between; cursor paging, newest received
  first. Each item carries the company's code and name, the supplier's id
  and name (once known), the document's number, kind, issue date,
  currency and gross, the status with its problem code and detail or
  reason, and the entered invoice's id, document number and supplier
  reference.
- `GET /v1/e-invoices/{id}` (the same, plus the lines read),
  `GET /v1/e-invoices/{id}/content` (the XML, as received),
  `POST /v1/e-invoices:fetch` (an Operation with the counts of §4),
  `:retry`, `:link`, `:dismiss` (each answering the document).
- The audit file's endpoints follow the specification (§10): at least
  generation as an Operation whose `resource` is the file, the file's
  record and content, and a list per registration with cursor paging,
  newest first, filtered by status.
- Problem codes as named above, each registered with its status. Events
  in `api/asyncapi.yaml`. Every response is checked against REVIEW.md's
  API-consumer classes.

## Alternatives
- **Our own Peppol access point.** Rejected by the product owner
  (07 §7.6, phase 2 decisions).
- **E-invoices through the capture operation, as uploaded XML** (the
  literal reading of ADR-0049's "fed to capture"). No new context, but a
  person would download from the provider and upload; no fetch, no
  acknowledgement, no record of refused documents. Rejected; capture's
  rules are shared through payables instead (§1, §6).
- **E-invoicing inside payables.** Payables owns supplier invoices, but
  sending in phase 3 is a sales document, and the provider's statuses
  are neither. Rejected for a leaf that calls payables (and later sales).
- **E-invoicing inside integration.** Integration owns connectors and
  migration, not business documents with statuses people act on.
  Rejected; integration keeps the connector.
- **Recording and entering in one transaction.** One activity fewer, but
  a refusal or bug in entering would leave the bytes unacknowledged and
  fetched again forever. Rejected (§4).
- **Push from the provider (an inbound webhook).** Faster, but a new
  public endpoint and credential to defend. Not by default; question Q3.
- **Refusing e-invoices without an order, as capture does.** Simple, but
  drops a valid statutory document. Rejected for `held`.
- **Refusing a document whose tax per category is off.** Would decide a
  rounding rule the specialist has not given and bypass P5. Rejected.
- **Statutory report layouts as pack definitions** (as returns.yaml).
  The audit file is a structured file, not boxes over items, and a
  format change needs the specialist anyway. Rejected; a format is code
  with a golden test, selected by the registration's authority.

## Consequences
- A new leaf context and schema; payables gains two published functions
  and a source value; integration a connector kind; the egress client a
  per-client limit; tax filing an audit file.
- A UAE supplier's e-invoice reaches the AP queue without a model,
  without a person's upload, with its order items and tax codes matched
  by capture's rules.
- Nothing is sent before phase 3: the UAE pilot company's outgoing
  e-invoices, if it is in the mandate, are its billing system's (Q2).
- The audit file and any Canadian report wait for the specialist; their
  slices are blocked on it, not on code.
- Until Q1 is answered only the stand-in exists, and production refuses
  it, so no tenant receives e-invoices in production.

## Boundaries with the other steps
- **T61 (tax)**: categories on codes, the calculator, the tolerance and
  the tax date are T61's; T65 reads them (ADR-0049 §1).
- **T62f (UAE pack)**: the UAE input codes and their categories; the
  walkthrough uses an unreleased illustrative UAE pack until it lands.
- **T34e (capture)**: shares payables' entering rules (§6).
- **T66 (compliance)**, **phase 9 (archiving)**: retention of documents
  and audit files.
- **T71 (regulatory agent)**: proposes pack changes when the UAE's
  e-invoicing rules change.
- **Phase 3 (sales)**: issuing sales invoices, and with it T65h.

## Steps (slices)
Each is a slice (ADR-0053): contract, migration, domain code, tests (real
PostgreSQL, `FailingTx` on every database path, run-twice tests of every
activity, 100% coverage), docs and a walkthrough step. Lane S, in this
order, about half a day each. The walkthrough's new `step_einvoice` comes
after `step_capture` and before `step_close` (which stays last). Its
company is a UAE company of the walkthrough's tenant (code AE01, AED,
Asia/Dubai, with an FTA registration) that T65a adds by a configuration
change; the provider is the stand-in on 127.0.0.1, as the SFTP and model
stand-ins are. Ada, the administrator, Alex, the AP clerk, and Carla,
the controller, are the named users; the capture agent the API consumer
of §13.

| Step | Work | Acceptance criteria and walkthrough evidence |
|---|---|---|
| T65a | **Fetching e-invoices on demand** | Connector kind `einvoice_provider` (§3, `integration.adapter_not_certified` in production); the `einvoicing` leaf (`internal/layout`) and schema with `einvoice` (RLS, `tenant_id` first, unique per connector and message id, status trigger allowing `received` only, schema tests); the `Provider` port, the stand-in (an HTTP server on loopback, refused in production) and the egress client's per-client limit; `FetchEInvoices`'s record activity, run by `POST /v1/e-invoices:fetch` (Operation with counts), acknowledgement after commit (run-twice test); `GET /v1/e-invoices` and `/{id}` (§15); `einvoicing.einvoice.read` and `.manage`; `EInvoiceChanged`; audit. Walkthrough: Ada adds AE01 and the connector; the stand-in holds an e-invoice; Alex fetches and finds it in the list, `received`, under the fetch's operation; a second fetch counts it as already recorded |
| T65b | **Scheduled fetch, changed bytes and the original** | The dispatcher and `ERP_EINVOICE_FETCH_INTERVAL`; `changed_hash` with the guarded update and one event (run-twice test); the gauge `erp_einvoices_unacknowledged` (`erp_einvoices_held` comes with T65d); `GET /v1/e-invoices/{id}/content` (`x-agent: false`). Walkthrough: Alex downloads the e-invoice, its hash equal to the stand-in's; the stand-in redelivers it with other bytes and a fetch counts it as changed, once |
| T65c | **Validation and refusals** | The process activity with §5's checks: safe parsing (no DTD, no entities), the fields read citing the specification supplied (to confirm), decimals, the arithmetic without tax rules, the buyer; `refused` with each problem code (the trigger now allows it); valid documents stay `received` until T65d; fuzz test of the parser; nfr budget of the processing transaction. Walkthrough: a malformed document and one addressed to another TRN are refused with their reasons; the valid one stays received |
| T65d | **Entering e-invoices as supplier invoices** | `payables.EInvoices.Enter` sharing capture's code: source `einvoice`, order, supplier by TRN fingerprint, items by order line or position with the order item's unit and code, codes by category and rate as §6, every problem a hold (`held` with its code) through a savepoint, tested with a constraint-raised refusal; `entered`; the invoice's document the XML; the gauge `erp_einvoices_held`; credit notes left `received` until T65e. Walkthrough: the valid e-invoice, still received, becomes a parked invoice of source `einvoice` on the next fetch, its line's code the order item's (category S at 5%), posts after the three-way match, and `getSupplierInvoiceDocument` returns the XML; one without an order is held with `payables.capture_needs_order` |
| T65e | **E-invoice credit notes** | A credit note entered as a `credit_memo` naming the invoice it cites, by capture's rule (none, a posted invoice, or `on_hold` naming the number); credit notes recorded before, left `received` by T65d, are processed by the next fetch. Walkthrough: the stand-in delivers a credit note citing the e-invoice of T65d; a fetch enters it as a credit memo referencing that invoice |
| T65f | **Held e-invoices resolved by people** | `:retry` (`held` → `held` allowed by the trigger, schema test), `:link` with `payables.EInvoices.Attach` and the partial unique index (schema test, `einvoicing.invoice_already_linked`), `:dismiss` (§7, `FOR UPDATE`, `einvoicing.einvoice_not_held`, `einvoicing.link_mismatch`); agents refused. Walkthrough: Alex retries the held e-invoice before the order is fixed and it stays held; he enters it by hand and links it, the invoice's document becomes the XML; linking a second e-invoice to it is refused with `einvoicing.invoice_already_linked`; another is dismissed with a reason; an agent's `:dismiss` is refused |
| T65g | **FTA Audit File** (when the specialist supplies the specification) | As §10, the contract from the specification; `tax.audit_file`; `tax.audit_file.generate` and `.read`; people only; `TaxAuditFileGenerated`; golden test signed off by the specialist; nfr budget. Walkthrough: Carla generates AE01's audit file as the specification asks it and downloads it; an agent is refused |
| T65h | **Sending sales e-invoices** (phase 3, after issuing sales invoices) | §9; sliced with phase 3's sales design |

Each slice updates 02 §2.2 (T65a: the context's row from planned to
built), 03 §3.25 (and §3.20 with T65g) and this ADR's "As built".

## Decided by default (ADR-0053 d), for the product owner to overrule
1. A leaf context `einvoicing`, calling payables through two new
   published functions (§2).
2. ADR-0049's "fed to capture" read as capture's rules, not its
   operation (§1, §6); ADR-0049 §1 notes it.
3. The provider is a port behind a connector kind `einvoice_provider`
   with a stand-in; production refuses the stand-in (§3).
4. The ERP polls the provider; it opens no inbound endpoint (§3, Q3).
5. Fetched hourly by default and on demand; recording (acknowledged after
   its commit) apart from processing; a changed redelivery recorded once
   (§4).
6. Refused (unreadable, inconsistent without any tax rule, not ours)
   versus held (any refusal of the entering); refused documents get no
   response to the supplier until the specialist confirms one is
   expected (§5, §6). The specialist found no such obligation on
   2026-11-09 (C8, [L]; §5).
7. Tax codes from the lines' categories by capture's rule, ambiguity
   holding the invoice for a person (§6).
8. Documents kept write-once as received, no retention lock; retention
   is T66's (§8).
9. The FTA Audit File in the tax filing context, generated by people
   only, as an Operation, audited; its content from the specification
   (§10).
10. No new agent type; the capture agent proposes codes and orders (§13).
11. PINT AE is the only format in T65; other formats when a country pack
    needs one (§1, §3).

## Questions for the product owner
- **Q1. The provider** (new scope, cost, security posture). Options: (a)
  the accredited provider the UAE pilot company already uses or
  chooses, one adapter for it; (b) one provider the platform contracts
  for every tenant; (c) no provider yet: the port and the stand-in only.
  Recommendation: (c) now and (a) as soon as the pilot names its
  provider, judged on: accredited by the FTA (to confirm the list),
  an API the ERP can poll, where it keeps the data, and its fees. No
  provider is named here: the repository records none.
- **Q2. Timing against 2027-01-01** (legal). Whether the UAE pilot
  company's revenue reaches AED 50m is not known (ADR-0049). If it does,
  its sales e-invoices from 2027-01-01 cannot come from the ERP, which
  issues none before phase 3. Options: (a) its billing system (and its
  provider) sends them, the ERP receives only; (b) bring phase 3's sales
  invoicing forward; (c) send from the billing system's journal entries
  (they lack buyer, description and quantity: new scope).
  Recommendation: (a), and T65a-T65f before 2027-01-01 if the company is
  in the mandate; please tell us its revenue and billing system.
  **Fact (po:2026-11-09):** the company is below AED 50m and has no
  provider yet, so its dates are 31 March 2027 (appoint) and 1 July 2027
  (go live).
- **Q3. Push from the provider** (security posture). If the chosen
  provider cannot be polled, accept an inbound endpoint authenticated by
  the provider's signature? Options: (a) no, choose a provider that can
  be polled; (b) yes, behind the router with the provider's signature
  and source checks. Recommendation: (a) unless Q1's provider requires
  push; decide with Q1.
- **Q4. Canadian statutory reports** (legal). None is named in 07 or an
  ADR beyond the GST/HST return. Options: (a) the tax specialist lists
  them and each becomes a slice like T65g; (b) none for the pilot.
  Recommendation: (a), nothing built until named.

## Gaps (out of scope, not built, no question asked)
Neither the roadmap entry nor a PO decision asks for these; each becomes a
tracker item only when one does (07 §7.5).
- Responses to suppliers through the network (acceptance or rejection).
- Mapping the document's unit codes to the ERP's units.
- E-invoices of other countries received by the UAE company.
- Notifications of held or refused documents beyond the event and gauges.
- Screens in the web workspace.

## Facts for a specialist (stated from secondary sources or memory, not verified here)
Answered by the UAE tax specialist on 2026-11-09 (po:2026-11-09): the
current Ministerial Decisions and the timeline (Context); PINT AE's
publisher, version, syntax, elements, validation artefacts, credit notes
and amounts in AED, the participant identifier scheme and whether a buyer's
response is mandated (§5, C1-C8); the FTA Audit File (§10, C9, the
specification itself still to obtain). Still open: how the AED 50m
threshold is measured, the five-corner model's reporting, the accredited providers, retention, UAE
rounding, the code lists (units, categories), sending (phase 3), Canadian
reports and Art. 59.
- The UAE mandate (ADR-0049, from EY's summary): the AED 50m threshold's
  measure, the dates for smaller businesses, whether receiving and
  buyer's responses are mandated, and the current Ministerial Decisions.
- The five-corner model: who each corner is and what the provider
  reports to the FTA.
- PINT AE: its version, syntax (UBL 2.1 XML, from memory), the elements
  for each field §5 reads, its validation artefacts, its code lists
  (units, categories), credit notes, and amounts in AED on a document in
  another currency.
- How UAE tax is rounded on an e-invoice, per line or per category
  (already on ADR-0049's list).
- Participant identifiers: their scheme (TRN-based or not).
- The FTA's accredited providers and how a taxpayer appoints one.
- How long and in what form a UAE e-invoice must be kept.
- What the provider and the FTA report back on a sent e-invoice, and how
  a rejected or wrong one is corrected (phase 3).
- The FTA Audit File: format, sections, fields, encoding, source data,
  the scope a request covers, and when it must be produced.
- Canadian statutory reports, if any, beyond the GST/HST return.
- The UAE tax-invoice contents (Executive Regulation Art. 59), already on
  ADR-0049's list.

## Sources
- The repository: 07-transition §7.6 (T65's entry, the phase 2
  decisions), the tracker's "E-invoicing and statutory reporting" leaves,
  ADR-0049 (Context, §1 boundary with T65, §6, P2, P3, P5, P6, Sources),
  ADR-0048, ADR-0038 (connectors, certification), ADR-0047 (the fetch
  pattern), ADR-0016, ADR-0017, ADR-0021, ADR-0036, ADR-0041, ADR-0053,
  ADR-0057 and ADR-0058 (leaf contexts), 03 §3.9, §3.13, §3.20,
  `internal/capture`, `internal/payables/capture.go` (`capturedItems`)
  and `capturetax.go` (`capturedTax`, `proposeCode`),
  `internal/config/taxcode.go` (`Category`), `internal/platform/egress`,
  `api/openapi.yaml` (`captureSupplierInvoice`,
  `getSupplierInvoiceDocument`, connector kinds, supplier invoice
  `source`).
- No external source was read for this ADR (no network in the lane).

## As built
(Filled in by the slices T65a-T65h.)

**T65a (fetching e-invoices on demand).** Built as §2-§4, §11, §12 and
§15 for recording: migrations 000231 (schema `einvoicing` with its
outbox, `einvoicing.einvoice`, the permissions `einvoicing.einvoice.read`
and `.manage`, and the connector kind check widened NOT VALID to
`einvoice_provider`) and 000232 (that check validated). The leaf
`einvoicing` (`internal/einvoicing`; `internal/layout` lists it among the
leaves, importing no context) declares the ports `Connectors` (over
integration's published `EInvoiceProviders`: a connector resolved with
its settings and credentials at each use, failing closed),
`Companies` (tenancy) and `Runs` (operations), adapted in
`internal/app/einvoicing.go`; `Suppliers`, `Payables` and the
registrations' half of `Companies` come with the slices that read them
(T65c, T65d). `POST /v1/e-invoices:fetch` resolves the connector as a
fetch would and queues the fetch as its Operation (kind
`einvoicing.fetch`); the dispatcher starts `FetchEInvoices` for it
(`operations.QueuedOf`, the workflow id `{tenant}-einvoice_fetch-{operation}`;
T65b's scheduled fetch names the connector and its period), and a cell
without a workflow engine runs the same steps in process
(`workflows.EInvoiceFetcher` over `operations.StillQueuedOf`: queued and
taken by no workflow; a cancelled fetch holds nothing to release, so it
is not taken up). The workflow reaches the context through
`workflows.EInvoices`: `PendingEInvoices`, then `RecordEInvoice` per
message (the bytes fetched with no transaction open and kept under
`documents/<sha256>`, an existing key being the same bytes; the row
inserted `ON CONFLICT ON CONSTRAINT einvoice_message_once DO NOTHING`
with its audit record `einvoicing.einvoice.received` and
EInvoiceChanged; the acknowledgement after that commit and
`acknowledged_at` set where still null), and `FinishEInvoiceFetch`; each
is idempotent, with run-twice tests (`TestRecordEInvoice`). A message
already recorded counts as `recorded` when this fetch recorded it on an
earlier attempt and `already_recorded` otherwise. Decided by default:

- **The Operation.** Its `resource` is `/v1/connectors/{id}` while it is
  queued (the dispatcher's hand-off) and the list of what it recorded,
  `/v1/e-invoices?fetch_operation_id={id}`, once it succeeds; its
  `result` counts `recorded` and `already_recorded` (the slices that find
  changed, entered, held and refused documents add those counts). Messages
  not recorded fail it with `einvoicing.fetch_incomplete`, the others
  recorded: each is a field error `messages.{id}` with the code that
  stopped it, `platform.unavailable` for the provider failing (which the
  next fetch may get past) or a refusal such as
  `einvoicing.document_too_large` (a document above `MaxDocument`, not
  recorded until T65c records it refused, §5, which the next fetch meets
  again), so a consumer tells the two apart (review round 1). A provider
  that cannot be listed fails it with `platform.unavailable`; a connector
  refused since the fetch was queued with its problem. The workflow tries
  each step five times; the in-process fetcher of a cell without an
  engine tries each once and leaves the retry to the person, a fetch whose
  Operation cannot be ended staying queued for its next round. The
  acknowledgement sets `acknowledged_at` and moves the version without an
  event: it changes no status (§12).
- **The company.** The provider's listing names the participant a
  message is addressed to; the document takes the connector's company of
  that participant, or none (T65c refuses it,
  `einvoicing.not_addressed_to_company`). The list and the document show
  a company's documents to whoever may act in it, and those of no company
  only to a caller who may act in every company; `:fetch` checks no
  company access, since it records what the provider holds for the
  connector and shows nothing (as the statement fetch, ADR-0047). The
  fetch acts as the system principal `einvoicing.Fetcher`.
- **The stand-in** (`einvoicing.StandIn`, `internal/einvoicing/einvoicetest`)
  speaks a protocol of its own, no provider's: `GET /messages`,
  `GET /messages/{id}`, `POST /messages/{id}/acknowledge`, the credentials
  as a bearer token. It is called through the egress client as an adapter
  of a real provider will be (`einvoicing.NewClient`: the egress client
  with `egress.ResponseLimit(MaxDocument)` and `egress.CallTimeout(60 s)`,
  the per-client limit §3 asks for; the webhook client keeps 64 KiB and
  10 s), so outside the walkthrough it reaches public addresses alone. The
  walkthrough's stand-in (`providerstub`) is the one loopback address it
  may reach, named by `ERP_WALKTHROUGH_PROVIDER` and
  `ERP_WALKTHROUGH_PROVIDER_CA`, which `config.Load` refuses outside
  development, exactly as ADR-0041 As built's webhook receiver
  (`egress.WalkthroughReceiver` now serves both). Production refuses the
  stand-in when an `einvoice_provider` connector is created or enabled
  (422 `integration.adapter_not_certified`, beside
  `integration.ebics_not_certified` under the same production flag) and at
  use (`EInvoiceProviders.RefuseStandIn`); a change of an active
  connector's settings to the stand-in is refused at use, as §3 says of a
  stored one.
- **Permissions.** Migration 000231 creates the two permissions and gives
  them to no role: no migration since 000013 grants a role a permission
  (§11's "as earlier slices did" is not what the repository does); the
  tenant's administrators receive every permission at provisioning, and
  other roles by grant, as for `consolidation.run.manage`.
- **The list.** `GET /v1/e-invoices` and `/{id}` carry what a fetch
  records (the connector by id and name, the company by id, code and
  name, the message id and its participant, the bytes' SHA-256 and size,
  the status, the fetch and when received and acknowledged), filtered by
  company, connector, status, fetch and when received, newest received
  first, cursor-paged. What a document says (number, kind, issue date,
  currency, gross, lines), its supplier (and the supplier filter) and the
  entered invoice's number and reference are added, as fields and
  columns, by the slices that read and enter documents (T65c, T65d):
  nothing in T65a writes them. EInvoiceChanged carries the document, its
  connector and message id, its company by id and code, its status, the
  bytes' SHA-256, the fetch and the version.
- **The walkthrough.** `step_einvoice` (step 43, after `step_year_end`
  and before `step_close`) uses the UAE pilot company that step 41
  already sets up with its FTA registration (5000, Maple Gulf Trading,
  T62f), not a new AE01: T62f built it after this ADR was written. Ada
  connects the stand-in; Alex fetches the supplier's e-invoice (an
  illustrative document), finds it `received` for 5000 under the fetch's
  operation with the stand-in's SHA-256 and acknowledged; the stand-in
  loses the acknowledgement, and a second fetch counts the document as
  already recorded and acknowledges it again.
