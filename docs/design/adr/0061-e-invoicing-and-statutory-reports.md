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
- **Sending** (§9): from billing documents and customer credit memos,
  through Evatra (po:2026-11-19, E2-E4), no longer phase 3's.

**Out:**
- ~~Issuing sales invoices, and so sending e-invoices before phase 3~~:
  superseded by the product owner on 2026-11-19 (§9).
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
  `secret_ref`, read at use in the caller's transaction, failing closed,
  and its credentials, resolved from the vault apart with no transaction
  open (`Credentials`, As built E3).
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
// point. Implementations: Evatra's adapter (the first provider, Q1
// answered, po:2026-11-19), the stand-in (tests, walkthrough); a second
// provider is one more adapter.
type Provider interface {
    // Pending lists the documents received and not yet acknowledged.
    Pending(ctx context.Context) ([]Message, error)
    // Fetch reads one document's bytes, at most MaxDocument.
    Fetch(ctx context.Context, id string) ([]byte, error)
    // Acknowledge tells the provider the document is kept; acknowledging
    // twice is not an error (local only at a provider that keeps no
    // acknowledgement, such as Evatra).
    Acknowledge(ctx context.Context, id string) error
    // Send gives the provider one rendered document, as it is archived
    // (§9), and answers the provider's tracking id; Status reads what the
    // provider reports of it since (built in E3, As built E3).
    Send(ctx context.Context, d Outgoing) (Submission, error)
    Status(ctx context.Context, trackingID string) (Report, error)
    // Delivery reads what Peppol and the authority report of a completed
    // submission; TaxData asks for its tax data document to be sent again
    // or withdrawn (built in E4, As built E4).
    Delivery(ctx context.Context, trackingID string) (Delivery, error)
    TaxData(ctx context.Context, trackingID, request string) error
}
```
- **Connector kind `einvoice_provider`** (added to the open enumeration).
  Settings: `adapter` (the implementation: `evatra`, Evatra's, or
  `standin`, the stand-in's), `participants` (per company code, the
  company's participant identifier at the provider: at Evatra `0235:` and
  the company's TIN, §5 C6), `format` (`pint_ae`, the only one); the
  stand-in adds `base_url` (https); Evatra adds `environment` (`stage`,
  its sandbox, or `prod`), from which its base URL is derived (one of its
  two hosts, never a setting), and `company_id` (the company's id at
  Evatra); `secret_ref` names the credentials (Evatra's API key and the
  account's email). Created and changed with
  `integration.connector.manage`, as every connector.
- **The `evatra` adapter** (E1, As built): logs in (`POST /auth/login`),
  keeps the token until its `expirationDate` and sends it as `R-Auth`,
  logging in once again on a 401; lists received documents
  (`POST /invoice/search-documents`, `documentType` INCOMING) and reads
  one (`GET /invoice/get-document/{id}/XML`, Base64); acknowledges
  locally; sends (`POST /invoice/send-document-async`), looks a document
  up (`POST /invoice/document-process/search`) and reads its status
  (`GET /invoice/document-status/{trackingId}`) (E3, As built E3); reads
  a submission's message level responses (`GET /peppol/invoice-mls-list`)
  and tax data document (`GET /peppol/get-tdd`) and asks for that document
  to be sent again or withdrawn (`POST /peppol/tdd/retry-failed`,
  `/peppol/tdd/withdraw`) (E4, As built E4).
- **Production refuses the stand-in**: creating or enabling an
  `einvoice_provider` connector whose adapter is `standin` there answers
  422 `integration.adapter_not_certified`, as EBICS is refused until
  certified with a bank (ADR-0038); a stored one is refused at use.
  Evatra's adapter is allowed there in its `prod` environment; its
  `stage` sandbox is refused there the same way (E1, As built).
- **Egress**: the adapter calls the provider through the egress client
  (public addresses only, no redirects, system roots), with its own
  response limit, `MaxDocument` = 10 MiB (the egress client gains a
  per-client limit; the webhook client keeps 64 KiB), and timeouts of
  5 s to connect and 60 s per call. The provider's real limits are
  checked against these when Q1 is answered.
- **Pull, not push.** The ERP polls; it opens no endpoint for the
  provider to call. An adapter for a provider that only pushes needs an
  inbound endpoint, which is a security-posture decision (question Q3).
- **Formats behind the port.** A `Format` (parse, and from E2 render)
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

### 9. Sending (E3, E4; product owner 2026-11-19)
The product owner asked on 2026-11-19 for UAE e-invoices to be sent as
well as received inside the ERP ("companies do not want to operate on
different platforms"), through Evatra (po:2026-11-19), superseding
"sending with phase 3's sales invoices" (po-2026-11-09 items 1-2; item 3,
no inbound endpoint, stands). Sending is no longer phase 3's: the ERP's
billing documents and customer credit memos (ADR-0049's sales side, T75)
are its source. The plan is the coordinator's (einvoicing-evatra, slices
E1-E4 under T65h):

- **E2** renders a posted customer invoice (a billing document's is the
  invoice it posted as) as a PINT AE tax invoice (380) and a customer
  credit memo as a tax credit note (381, reason BTAE-03) from their items
  and tax items (code, category, rate, base, tax: ADR-0049's boundary, no
  second mapping), checks them by the mandatory-field rules of §5's
  machinery before anything is kept or sent, and archives the rendered XML
  write-once (§8); As built E2 says what it renders and leaves invalid;
- **E3** gives it to the provider (`Provider.Send`) in the outbox's
  pattern: a submission recorded before the call, sent after the commit;
  Evatra has no idempotency key, so after a lost answer the document is
  looked up before any resend; statuses are polled (`Provider.Status`) in
  a workflow per submission; a worklist of billing documents not
  transmitted within 14 business days;
- **E4** records what the provider and the authority report back (the
  Peppol message level responses and the tax data document's status) on
  the invoice, puts a rejection on a worklist to correct by credit note,
  and offers the tax data document's retry and withdrawal to people only.

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
| `einvoicing.outgoing.send` | `:send` an outgoing e-invoice to the accredited provider (As built E3); `:retryTaxData` and `:withdrawTaxData` of a transmitted one (As built E4) | people only, never agents (`x-agent: false`); given to no role, as E2's |
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
dismiss e-invoices, never send one (E3), never retry or withdraw a tax
data document (E4), and never generate or read
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
- Sending comes with E2-E4 (§9, po:2026-11-19); until then the UAE pilot
  company's outgoing e-invoices are its billing system's (Q2).
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
- **Sales and billing (T75)**: the billing documents and customer credit
  memos E2 renders and E3 sends (T65h, split E1-E4, po:2026-11-19).

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
| T65h | **E-invoices through Evatra** (po:2026-11-19) | §3, §9; split into E1 (the Evatra connector and inbound), E2 (PINT AE rendering), E3 (sending), E4 (delivery status and FTA reporting) and Later in the tracker |

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
- **Q1. The provider** (new scope, cost, security posture). **Answered
  (po:2026-11-19):** Evatra (Hamt Information Technology L.L.C, the
  Ministry of Finance's accredited service provider no. 144576), built in
  as the first adapter behind the provider-neutral port, for sending and
  receiving (§3, §9; E1-E4). The question as asked: Options: (a)
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

**T65b (scheduled fetch, changed bytes and the original).** Built as §4,
§8, §11 and §15 for the schedule, a changed redelivery and the original:
migrations 000233 (`changed_hash`, `changed_at` and
`changed_fetch_operation_id` on `einvoicing.einvoice`, set together, never
to the bytes kept, never before receipt, `einvoice_changed` and
`einvoice_changed_hash` added NOT VALID; the trigger replaced so they may
be set and never undone; the worker role's read of `received_at` and
`acknowledged_at`), 000234 (the checks validated), 000235 (the cell-wide
partial index `einvoice_unacknowledged`, built concurrently as the other
gauges' indexes are) and 000236 (`einvoice_by_changed_fetch`). Decided by
default:

- **A scheduled fetch is an Operation too.** The dispatcher starts, once
  per `ERP_EINVOICE_FETCH_INTERVAL` (default 1h, 0 none) and active
  `einvoice_provider` connector of an active tenant
  (`integration.EInvoiceConnectors`, through `database.ActiveTenants` as
  every worker's read: a suspended or offboarded tenant's provider is not
  polled), the workflow
  `ScheduledEInvoiceFetch` under `{tenant}-connector-{connector}-einvoices-{period}`
  (ADR-0047's pattern: replicas start each once; a period started without
  a failure is not looked at again). Its first step records the fetch's
  Operation (kind `einvoicing.fetch`, created by `einvoicing.Fetcher`,
  the connector as its resource) under an id the dispatcher derives from
  the tenant, connector and period, a UUIDv7 of the period's start, so the
  step is idempotent and the Operation sorts by its period; the Operation
  names the workflow at once (`operations.StartTaken`), so no dispatcher
  starts another for it. The fetch then runs `FetchEInvoices`'s steps: the
  documents name that Operation as their `fetch_operation_id`, and its
  counts and failures are an on-demand fetch's. Every step of both
  workflows runs through `perform` with `database.BeginActive`, so a fetch
  whose tenant is suspended mid-way waits until it is active again
  (ADR-0015 As built, po:2026-11-09), and the in-process fetcher takes up
  the queued fetches of active tenants only (`operations.StillQueuedOf`;
  integration, batch 61). A connector refused since
  the dispatch (disabled, its secret gone) fails that Operation with the
  problem, and a server without object storage fails it at its first
  step, before the provider is asked (`platform.unavailable`), as it
  refuses a fetch on demand. A cell without a workflow engine fetches on demand only, as it
  fetches no statements on a schedule (ADR-0047).
- **A changed redelivery.** A message recorded before whose bytes are
  neither the bytes kept nor the change last recorded sets
  `changed_hash`, `changed_at` and the fetch, guarded by `sha256 <> $new
  AND changed_hash IS DISTINCT FROM $new` (ADR-0008), with the audit
  record `einvoicing.einvoice.bytes_changed` and EInvoiceChanged
  (`changed_sha256` and `changed_fetch_operation_id` added, optional in
  the v1 schema as ADR-0041 requires of a field added within a version,
  and carried by every event; integration, batch 61; the status
  does not move, so `status_before` is the status); the outcome is
  `changed`, a fetch's new count. The fetch that found the change is the
  document's `changed_fetch_operation_id`, and the list's
  `fetch_operation_id` filter, a fetch's Operation's `resource`, matches
  it as well as the recording fetch, so a fetch's result leads to the
  documents it found changed (review round 1). The same
  changed bytes again, or the first bytes again, are `already_recorded`;
  a step run again after its commit counts the change as its fetch's, as
  a recording does (`changed_fetch_operation_id`). The row keeps its first
  bytes, the original (§8). The new bytes are kept in object storage
  under their own hash too, since they are kept before the row is read;
  nothing serves them, and `changed_sha256` names them. The list and the
  document carry `changed_sha256`, `changed_at` and
  `changed_fetch_operation_id`.
- **The original.** `GET /v1/e-invoices/{id}/content` serves the bytes
  as first received, `application/octet-stream` as
  `getSupplierInvoiceDocument` serves a captured document, visible as the
  document is, compared with the recorded SHA-256 on every read (409
  `einvoicing.document_changed` otherwise), human-only (`x-agent: false`,
  §11), and not audited, as other downloads are not (§10).
- **The gauge.** `erp_einvoices_unacknowledged` counts the documents
  recorded over an hour ago and not yet acknowledged, cell-wide as the
  worker role (ADR-0043's business gauges), reading only those two
  columns.
- **The walkthrough.** `step_einvoice` goes on: Alex downloads GOI-2001,
  its SHA-256 the stand-in's; the stand-in delivers it again with other
  bytes and a fetch counts it `changed`, the document keeping its first
  bytes and naming the new ones' hash, found through the fetch's list;
  the same bytes delivered once more
  are counted already recorded, the change named once.

### The fetch's request audited (review:T106a)

- `fetchEInvoices` audits its request in the caller's name
  (`einvoicing.fetch.request`, its Operation the object, with the
  connector) in the transaction that queues the fetch (ADR-0011), so the
  trail shows who fetched; what the fetch then records it records as the
  system, naming the Operation as `fetch_operation_id`. A scheduled fetch
  has no requester and records no request. The access agent's
  `accessagent.Unaudited`, which classed `einvoicing.einvoice.manage` as
  "use not recorded" until then, is removed (ADR-0072).

### E1: the Evatra connector and inbound (po:2026-11-19, plan einvoicing-evatra.md)

Built as §3 for Evatra's adapter and the
receiving side of §4; no migration (the connector's settings are JSON,
validated by integration). Evatra's API as the plan read it on 2026-11-19
(https://evatra.com/api; its page needs JavaScript, so it was not fetched
again here) is the source of every path and field below; what that page
does not say is decided by default and listed for the specialist and
Evatra. Decided by default:

- **Settings.** `adapter` is `evatra` or `standin`; the keys are the
  adapter's own (`base_url` is the stand-in's and refused on an Evatra
  connector). Evatra's take `environment` (`stage` or `prod`) and
  `company_id` (its `companyId`, 1-100 letters, digits or `._-`: its form
  is not documented). Its participants are `0235:` and 10 digits (§5 C6),
  since the listing is asked per company by its TIN.
- **The hosts.** The base URL is derived from the environment,
  `einvoicing.EvatraHosts()`, a copy on each call: `https://api-stage.evatrauae.com` and
  `https://api.evatrauae.com`, the only hosts the adapter calls, through
  the provider's egress client (public addresses only, no redirects,
  system roots). No setting names a URL; only the walkthrough replaces both
  with its stand-in (`app.Config.EvatraHosts`, set by erp-server from
  `ERP_WALKTHROUGH_PROVIDER`, which `config.Load` refuses outside
  development). The cell's network policy allows the server 443 outward
  (ADR-0035); there is no per-host list there to extend.
- **Production.** Evatra's `prod` is allowed; its `stage`, the sandbox, is
  refused there as the stand-in is (422 `integration.adapter_not_certified`
  on create or enable, and at use): a sandbox document entered in a
  production tenant would not be a statutory one. A security-posture
  default, for the product owner to overrule.
- **Credentials.** The tenant-scoped connector secret (po-2026-11-16 H2)
  holds a JSON object `{"apiKey", "email"}`, the body of Evatra's
  `POST /auth/login`. One not of that form is refused at use, 422
  `integration.secret_unavailable` (a fetch's first step fails its
  Operation; the request no longer resolves the secret, As built E3); Evatra answering the login 401 or 403
  fails the fetch with the same code, and is not retried. The secret and
  the token are never logged, audited or put in an error: errors name the
  path and status only.
- **The token.** `jwt` is sent as `R-Auth`; it is kept in memory
  (`einvoicing.EvatraSessions`, one per server, keyed by the connector and
  a SHA-256 of its credentials, so a rotated secret logs in again) until a
  minute before its `expirationDate`, read as an RFC 3339 string; one
  Evatra dates otherwise (a number, another string, none), or already
  past, serves its call and is not kept (review round 1). A call
  answered 401 drops the token and is sent once more after a new login; a
  second 401 fails the step (retried by the workflow). No lock is held
  across a call: two steps racing may both log in.
- **Pending.** `POST /invoice/search-documents` with `companyId`, the
  participant's TIN as `tin`, `documentType` INCOMING, `startDate` 60 days
  back and `endDate` tomorrow (UTC dates, `YYYY-MM-DD`), `page` from 1 and
  `size` 200, until `totalCount` is reached; each `invoiceList` entry's
  `documentId` (a string or an integer: its type is not documented) is the
  message id (the name get-document's path gives it) and the participant
  asked for its receiver, so the document takes that company; a document
  listed again on a later page (the pages shifted by one received
  meanwhile) is listed once, where it was first (review round 1), and
  counted once against `totalCount`, so the listing reads on to the page
  the shift pushed a document to (batch 84 integration).
  Twenty-five pages (5,000 documents of one company in the lookback) fail
  the step rather than read on: one fetch workflow records a document per
  activity, about six history events each, within the engine's 51,200. The 60 days bound the listing, as
  Evatra keeps no acknowledgement; a document a fetch outage kept unread
  longer than that is not listed (the mandate's 14 business days to
  transmit fall well within it).
- **Acknowledgement, local only.** Evatra has no acknowledgement
  (`Acknowledge` does nothing; the adapter is a
  `LocalAcknowledgement`), so `PendingEInvoices` leaves out the documents
  of the connector recorded and acknowledged, in a transaction of its own
  after Evatra answered; one whose local acknowledgement was lost is
  listed again and found already recorded, as §4's lost acknowledgement
  is repaired. Bytes Evatra changed under a document id already
  acknowledged are therefore not seen (§4's changed redelivery is the
  stand-in's).
- **Fetch.** `GET /invoice/get-document/{id}/XML` answers the XML in
  Base64, read as text or as a JSON string; content that is not Base64 is
  refused `einvoicing.unreadable` and not recorded (the fetch names it, as
  it names a document too large). The provider client reads answers of up
  to `MaxAnswer` (a Base64 `MaxDocument` with 4 KiB to spare); each adapter
  holds the document to `MaxDocument` (10 MiB) once decoded.
- **The port.** `Send` and `Status` are in §3 as signatures only; the Go
  interface gains them with E3, which builds them, so no adapter carries a
  method nothing calls.
- **The stand-in** (`einvoicetest.Evatra`) emulates these three calls
  under `/evatra` of the test server and of the walkthrough's
  `providerstub` (`-evatra` the account, `-evatra-inbox` its documents,
  apart from the stand-in protocol's inbox, which acknowledges): login
  with the account's API key and email, tokens of an hour, 401 to any
  other, search by `companyId`, `tin` and INCOMING with pages, the
  document in Base64.
- **Not built here:** validation and entering (T65c, T65d) do not exist
  yet, so a document received through Evatra is `received`, as the
  stand-in's are, until those slices process it.
- **The walkthrough.** `step_evatra_inbound` (step 144, before
  `step_close`): Ada connects the UAE company 5000 to Evatra's sandbox
  (participant `0235:1001234567`, the first 10 digits of its FTA
  registration 100123456789003, Evatra's company id illustrative); Evatra
  has received an illustrative PINT AE invoice GOI-2002 as its document
  EVA-77001; Alex fetches, it
  is recorded `received` for 5000, its SHA-256 Evatra's decoded bytes',
  acknowledged locally, and downloaded as received; a second fetch counts
  nothing, leaving it out.
- **For the specialist and Evatra (not in Evatra's docs as read):** the
  response fields of `search-documents` (`documentId` assumed) and the
  date its `startDate`/`endDate` filter (issue or receipt) and their
  format; `expirationDate`'s format; whether `get-document` answers Base64
  as plain text or JSON; the form of `compId`/`companyId`; whether
  `search-documents` with `tin` returns documents received for that
  participant; the sandbox's behaviour; rate limits.

### E2: PINT AE rendering (po:2026-11-19, plan einvoicing-evatra.md)

Built as §9 for rendering, no sending (E3): migration 000415
(`einvoicing.outgoing_einvoice`, the permissions `einvoicing.outgoing.read`
and `.render`, given to no role as T65a's), `POST /v1/outgoing-e-invoices`
(`createOutgoingEInvoice`, human-only), `GET /v1/outgoing-e-invoices`,
`/{id}` and `/{id}/content` (human-only), the event
OutgoingEInvoiceRendered and the audit record `einvoicing.outgoing.rendered`.
The format's files are `pintae.go` (the semantic model, Document, and
`ParsePINTAE`), `pintae_ubl.go` (the UBL binding read), `pintae_render.go`
(`RenderPINTAE`), `pintae_rules.go` (`Check`, the rule machinery) and
`outgoing*.go` (the endpoints and the build from the customer document);
`ParsePINTAE` reads back what the format renders, so E2's round trip is
render, parse back, `Check` again, in-house only. Receiving is unchanged:
documents stop at `received`; §5's validation of received documents,
with its refusals and the round trip through it, is T65c's (not built,
coordinator 2026-11-19), which may build on this parser. Decided by
default:

- **What is rendered.** A posted customer document of receivables, not the
  billing document: a customer invoice as a tax invoice 380 (UBL
  `Invoice`), a customer credit memo as a tax credit note 381 (UBL
  `CreditNote`) naming the invoice it credits (IBT-025). A billing
  document's e-invoice is the customer invoice it posted as (T79b), named
  as `source_document_id`, and a credit memo request's its customer credit
  memo (T85d), as SAP's Document and Reporting Compliance creates the
  e-document from the accounting document. A debit memo is refused (422
  `einvoicing.unsupported_document`; PINT AE has no debit note type, plan
  default 6, a specialist question); a draft (409
  `einvoicing.document_not_posted`); a company not in the UAE or not in AED
  (422 `einvoicing.not_a_uae_company`).
- **Where each field comes from** (ADR-0049's boundary, no second mapping):
  the seller is the company (tenancy's name, the configuration's
  `postal_address` with its region as the emirate, and the number of its
  FTA registration in force on the invoice date, from its first period's
  start to its last period's end, opened by tax filing's
  `InvoiceNumbers`); the
  buyer the customer (partner's `InvoiceParties`: its legal name or name,
  its legal address on the invoice date with its region, its `AE_TRN` tax
  identifier, opened); the endpoints (IBT-034, IBT-049) scheme `0235` and
  the TRN's first ten digits (C6); each item a line with its tax code's
  UN/CEFACT category and rate on the invoice date (ledger's `TaxCode`),
  the VAT per category and rate the items' tax as receivables apportioned
  it; the VAT in AED (IBT-006, IBT-111) on a document in another currency
  the sum of the entry's output tax items in the company's currency, as
  the posting converted it. An emirate is held as ISO 3166-2 (AE-DU, as the
  emirate guard names it) and rendered as PINT AE's code (DXB); a region
  already holding a PINT AE code keeps it.
- **Values no ERP field supplies**, each a question for the specialist:
  every line is quantity 1 of unit `C62` at its net amount (a customer
  invoice's items carry no quantity, and units are not mapped to UN/ECE
  Recommendation 20, §Gaps); the payment means is `30` (credit transfer);
  BTAE-02 is `00000000` (none of its special cases is rendered); a credit
  note's reason (BTAE-03) is the code and text the caller gives, rendered
  as `cac:DiscrepancyResponse/cbc:ResponseCode` and `cbc:Description`, not
  checked against a code list yet. BTAE-07 (the unique identifier number)
  is read as carried by IBT-001, as C3 lists them together (to confirm).
  Not rendered: BTAE-04, the per-line BTAE-08 and BTAE-10 (listed as
  mandatory by the Ministry, C5, their UBL paths unknown here), BTAE-20,
  BTAE-13, BTAE-15/16, IBT-030, exemption reasons (IBT-120/121) and
  IBT-026. A mandatory term not rendered keeps the document invalid
  (review round 1): a document not in AED is invalid with a finding for
  BTAE-08 and one for BTAE-10, and an E, O or AE line with one naming
  IBT-120 or IBT-121 (either carries the reason in EN 16931, to confirm
  for PINT AE)
  (`einvoicing.pint_ae.not_rendered`), so no e-invoice the provider would
  reject is kept as valid and final; they become valid once the
  specialist confirms the terms and a slice renders them. E2 therefore
  e-invoices AED documents of standard-rated and zero-rated items.
- **Validation before keeping.** `Check` runs the mandatory-field rules
  (§5 C3-C6): the specification and profile, the type code, number, BTAE-02,
  the tax accounting currency, a credit note's invoice and reason, each
  party's name, 15-digit TRN, `0235` endpoint (the seller's its TRN's first
  ten digits), address line 1, city, country AE and emirate, the payment
  means, each line's id, quantity, unit, name, description, category and
  rate (above zero for S only), and the arithmetic with no tax rule (lines
  to the net, net and VAT to the gross payable, one subtotal per category
  and rate of its lines' net amounts, the subtotals to the VAT). A document
  that fails is recorded `invalid` with its problems (rule, term, message)
  and no XML, and is rendered again, in its row, once the master data is
  corrected; a company whose VAT TRN is its tax group's is named, since its
  own TIN is not recorded (C6). One that passes is rendered, parsed back
  by `ParsePINTAE` and checked again (a render its own parser or rules
  refuse is a defect, never kept), kept write-once under
  `documents/<sha256>` (ADR-0016), and recorded `valid`: final, a second
  render answering it unchanged with 200 (201 when the row is new). The
  XML carries no time of rendering, so the same document renders the same
  bytes. The store is called before the row is written, with no row lock
  held; the upsert is guarded on `status = 'invalid'`, so a row another
  request made valid meanwhile is answered as it is.
- **TRNs.** The XML carries both TRNs in clear, as a tax invoice must;
  `/content` is human-only (`x-agent: false`), and the event and audit
  record carry no content and no TRN.
- **Gaps found** (for the product owner, not built): a sales document's item
  cannot carry an emirate (`sales.tax_jurisdiction_not_supported`), so a
  UAE billing document cannot bill the standard-rated code S5, which levies
  each line's emirate; until it can, the UAE company's standard-rated
  sales e-invoiced here are customer invoices entered in receivables. The
  walkthrough therefore renders a customer invoice and a credit memo, not
  a billing document. (Closed in batch 85: ADR-0067 As built, the emirate
  on a sales item; step 148 renders a billing document's customer invoice
  as a valid 380. E2 needed no change: it renders the parties' emirates
  from their addresses and the VAT by category from the invoice's tax
  items.)
- **The walkthrough.** `step_einvoice_render` (step 145, after E1's
  `step_evatra_inbound` and before `step_close`): Ada gives 5000 a customer reconciliation account and its
  postal address in Dubai (one governed change); Ruth creates a UAE
  customer with its TRN and legal address in Dubai, approved by Carla,
  posts an invoice of a standard-rated item in Dubai and a zero-rated one
  and renders it, valid 380, downloading the XML with its hash and the
  parties' TRNs, endpoints and emirates; a second render answers it
  unchanged; a credit memo's render without its reason is refused, with
  one it is a valid 381 naming the invoice; both are listed for the
  company.

### E3: sending (po:2026-11-19, plan einvoicing-evatra.md)

Built as §9 for sending: migration 000421 (`einvoicing.outgoing_submission`,
the permission `einvoicing.outgoing.send`, given to no role as E2's),
`POST /v1/outgoing-e-invoices/{id}:send` (`sendOutgoingEInvoice`,
human-only, an Operation of kind `einvoicing.send`), the e-invoice's
`transmission`, the list's `transmission_status` filter,
`GET /v1/e-invoice-transmission-deadlines`
(`listEInvoiceTransmissionDeadlines`), the event
OutgoingEInvoiceTransmissionChanged, the audit records
`einvoicing.outgoing.send` (the person's request) and
`einvoicing.outgoing.transmission` (each change the sender makes), the
workflow `SendEInvoice` and, in a cell without a workflow engine, the
in-process `EInvoiceSender`. The port's `Send` and `Status` are built
(§3); the stand-in's own protocol sends nothing (`Sends`, by the
adapter's name: 422 `einvoicing.adapter_cannot_send`), and Evatra's emulation
(`einvoicetest.Evatra`, the walkthrough's `providerstub -evatra-outbox`)
answers the three calls below. Decided by default:

- **The outbox.** The request reads the valid e-invoice FOR UPDATE (two
  sends of it wait for each other), reads the connector without a call
  (a `connector_id` the tenant has not is 422
  `request.reference_not_found`, the body's reference; review round 1)
  and checks that its adapter sends and that it names the company's
  participant, and writes in its transaction the submission (`queued`),
  the Operation (its resource the e-invoice), the audit record and the
  event: the submission is the outbox of the call (ADR-0007), which the
  workflow makes after that commit. One submission per e-invoice
  (`outgoing_submission_once`); a failed one is queued again in its row by
  a person's new send, with a new Operation; a completed one is final (409
  `einvoicing.already_transmitted`; a correction is a credit note), one in
  progress refused (409 `einvoicing.transmission_in_progress`).
- **The ETag.** An e-invoice's ETag is its version plus its submission's
  (both only grow), so it moves with its transmission and a send asked
  twice on one reading is refused 412; the representation carries it as
  `etag`, so a send from a listing takes its `If-Match` from there
  (review round 1).
- **One step, idempotent.** The workflow repeats one activity,
  `AdvanceSubmission`, and sleeps as long as it answers (a timer, so the
  workflow stays deterministic), continuing as new after 1,000 steps; the
  in-process sender runs the same step each round for every send the
  einvoicing context lists as due or cancelled (`UnfinishedSends`, in
  each tenant with send Operations, as the depreciation runner's). A step
  reads the submission and prepares its call: the connector read in a
  transaction of its own and, once that committed, its credentials
  resolved from the vault with no transaction open, a reference that
  names nothing or is not the tenant's refused (`integration.secret_unavailable`)
  and any other vault failure an error the step is tried again on, so a
  passing outage fails nothing (review round 2) (review round 1: the
  vault was called inside the read's transaction; E1's fetch steps had
  the same fault, fixed the same way, src review:batch85), the XML read
  from the store and compared with its SHA-256: a failure of the
  machinery there changes nothing. Then, under the row's lock, it claims a
  due send (`sending`, the call counted in `attempts`, held five call
  timeouts), commits, calls Evatra with no transaction open, and records
  the answer in a third transaction only if the row is at the version the
  claim wrote; a step run twice records nothing twice, and a claim whose
  outcome never came (the worker stopped) lapses and is claimed again.
- **The duplicate guard.** Evatra has no idempotency key (plan default 4):
  every call after the first (`look_up`, set by the first claim, by a
  resend after SENDING_FAILED and by a person's new send) first asks
  `POST /invoice/document-process/search` (`companyId`, `direction`
  OUTGOING, `filename` the e-invoice's number, `pageNumber` 0) and takes a
  submission found whose status is not SENDING_FAILED, FAILED or
  PERMANENT_FAILED and whose tracking id is not among those Evatra failed
  to send (`failed_tracking_ids`), instead of sending again.
- **The send.** `POST /invoice/send-document-async` with `compId` (the
  connector's `company_id`), `base64Document` (the XML as kept, Base64),
  `invoiceType` UAE_PINT, `businessType` as asked (B2B or B2G; B2C is
  outside the mandate) and `invoiceSource` ERP; the answer's `trackingId`
  is recorded (`submitted`), its `status` read as PENDING when it has none
  of the form. A 4xx other than 401 and 429 is Evatra refusing the
  document (`einvoicing.send_refused`, failed at once); anything else is
  tried again after 1, 5, 15 and 60 minutes, and the fifth failure fails it
  (`einvoicing.provider_unavailable`). A connector or XML refused since the
  request (disabled, its secret not resolving or not Evatra's, the bytes
  not their hash) fails a send with that problem, no call made; the
  request resolves no secret, so one not Evatra's fails the first step.
- **The status.** `GET /invoice/document-status/{trackingId}`, its
  `status` read, first 30 seconds after the submission and then doubling
  to an hour (plan default 3): COMPLETED completes it (the Operation
  succeeds, its resource the e-invoice); FAILED and PERMANENT_FAILED fail
  it (`einvoicing.provider_failed`, `einvoicing.permanent_failure`);
  SENDING_FAILED queues it to be sent again at once, looked up first, up
  to three times (`resends`), the fourth failing it
  (`einvoicing.sending_failed`); any other status, or a read that failed,
  is read again; so is a read refused (the connector disabled since, its
  credentials not resolving, Evatra refusing them): Evatra holds the
  document, so it is not failed for it (review round 1); and 500 reads (about three weeks at the hour) without an
  end fail it (`einvoicing.status_unconfirmed`). A failed submission fails
  its Operation with the same problem; the failed ones are the worklist
  `listOutgoingEInvoices?transmission_status=failed`, which a person
  sends again once corrected, its attempts and resends counted afresh.
- **Cancellation.** `cancelOperation` on a send ends it at its next step:
  each claim, and the recording of each answer, reads the Operation under
  its lock, and one cancelled fails the submission with
  `einvoicing.send_cancelled` (the Operation stays cancelled), keeping
  what Evatra answered (its tracking id and status), unless the answer is
  final (Evatra refusing the document, a status of COMPLETED, FAILED or
  PERMANENT_FAILED), which is recorded as it is: a delivered e-invoice
  stays completed (review round 2), and the Operation stays cancelled
  either way; a send cancelled while the vault cannot be reached (or the
  store read fails) stays in progress until they answer, as its step
  prepares the call before it reads the Operation (review round 3); a
  claimed call whose
  claim has not lapsed is waited for, as it may be under way. A person
  may then send it again: it is looked up first, so a submission Evatra
  holds is taken, not sent twice. The workflow runs its steps on a context
  the engine's cancellation does not reach (as the depreciation run's), so
  a call is never cut off; the cancellation cuts its wait short, and the
  steps go on until one ends the submission. A send cancelled before any
  workflow took it is started all the same (`QueuedOf` lists it), and its
  first step ends it; the in-process sender lists the cancelled ones too.
  Chosen over refusing `:cancel` for sends (review round 1's option B):
  the generic cancel stays uniform, and the guard the step already has
  (the version, the look-up) makes ending it at a step boundary safe.
- **Audit and events.** Every change of the submission's status or of
  Evatra's status writes `einvoicing.outgoing.transmission` and
  OutgoingEInvoiceTransmissionChanged, as the system principal
  `einvoicing.Sender`; a status read that finds nothing new writes
  neither. No content and no TRN in either.
- **The 14 business days** (plan default 9; MoF Guidelines v2.0 as the
  plan cites them): `listEInvoiceTransmissionDeadlines` lists a UAE
  company's posted customer invoices and credit memos (receivables'
  `IssuedInvoices`; a reversed document and a debit memo are not listed)
  issued from `issued_from` (default 90 days before the key date, at most
  366) through the key date whose e-invoice is not `completed`: none
  rendered, invalid, or not completed at Evatra; each with its deadline,
  the 14th business day of the company's calendar after its issue date
  (its weekend, by default Saturday and Sunday, and holidays, ADR-0058 §3;
  the context reads the calendar through a `Settings` port, `internal/layout`
  allowing it `config` as closing's), the business days left on the key
  date (negative once past; overdue when the deadline is before the key
  date, so also on a key date after it with no business day between,
  review round 1), its e-invoice's and transmission's status,
  ordered by deadline, with the totals of every row, paged in memory.
- **Agents** never send (§13): the operation is `x-agent: false`.
- **The walkthrough.** `step_einvoice_send` (step 146, after
  `step_einvoice_render`, before `step_close`): Ruth sends the tax
  invoice through the Evatra connector, which completes, Evatra holding
  its bytes; the credit note, which Evatra's stand-in fails to send once
  and then reports PERMANENT_FAILED, is sent again (looked up first) and
  fails onto the worklist; the deadlines worklist lists it and not the
  invoice.
- **For the specialist and Evatra (not in Evatra's docs as read):** the
  answer forms of `send-document-async` (`trackingId`, `status` assumed),
  `document-status` (`status`) and `document-process/search` (a page of
  `content`, each with `trackingId`, `status` and `filename`, assumed);
  which filename Evatra files a `base64Document` under (the plan says our
  document number; the adapter sends none, and the stand-in takes the
  XML's IBT-001); whether FAILED differs from PERMANENT_FAILED for a
  resend; Evatra's limits on send and status calls; whether the 14
  business days count UAE public holidays (the company's calendar is
  used) and whether a reversed invoice still needs its e-invoice (not
  listed here).

### E4: delivery status and FTA reporting (po:2026-11-19, plan einvoicing-evatra.md)

Built as §9 for what comes back: migration 000422 (on
`einvoicing.outgoing_submission`, once completed: `delivery_status`,
`delivery_detail`, `delivered_at`, `tax_data_status`, `tax_data_at`,
`tax_data_request`, `tax_data_attempts`, `follow_polls`,
`follow_check_at`; the transition trigger allows completed to itself and
keeps a completed one's transmission), the e-invoice's `transmission`
showing them with `delivery_outcome`, the list's `delivery_outcome`
filter, `POST /v1/outgoing-e-invoices/{id}:retryTaxData`
(`retryOutgoingEInvoiceTaxData`) and `:withdrawTaxData`
(`withdrawOutgoingEInvoiceTaxData`), human-only, an Operation of kind
`einvoicing.tax_data`, the audit record `einvoicing.outgoing.tax_data` (the
person's request), and OutgoingEInvoiceTransmissionChanged carrying
`delivery_status`, `tax_data_status` and `tax_data_request` (optional,
added within v1). The port gains `Delivery` and `TaxData` (§3); the
stand-in's own protocol refuses both, as it refuses Send; Evatra's
emulation answers the four calls below, scripted per filename
(`<number>.delivery` in the walkthrough). Decided by default:

- **Following after completion.** COMPLETED no longer ends the send's
  steps: the same step (`AdvanceSubmission`, the workflow `SendEInvoice`,
  the in-process sender) reads, from 30 seconds after completion doubling
  to an hour (E3's backoff), what Peppol and the authority report, until
  the message level response is final (any but ACKNOWLEDGING) and the tax
  data document's status is final (any but none, TDD_SEND and
  WITHDRAW_SEND), or 500 reads were made (about three weeks; then it is
  no longer followed, and fails nothing: the transmission is done). The
  send's Operation succeeds at completion as in E3. A read that fails, or
  that the connector or the provider refuses, records nothing but the
  count. Each change of either status, of the request or of the status
  awaited after one (also when the 500th read clears it; review round 4)
  is audited
  (`einvoicing.outgoing.transmission`) and published; a read that finds
  nothing new writes neither. In a cell without a workflow engine the
  in-process sender finds the tenants to follow through
  `einvoicing.FollowedTenants` (the worker role reads `tenant_id` and
  `follow_check_at` of the submissions across tenants, as the e-invoice
  gauge reads the documents) besides the tenants of send Operations,
  since a completed send's Operation has finished and a request's is of
  another kind (review round 1). Each read moves the transmission's
  version, and so the ETag (said in the contract). The detail is shown
  on the e-invoice, not
  carried in the event or the audit record (it is the provider's free
  text).
- **Which response.** Of the message level responses Evatra lists, the
  last final one is taken (an ACKNOWLEDGING before it is not), else the
  last listed; its description, trimmed and clipped to 1,000 bytes, is
  the detail.
- **The worklist to correct by credit note.** `delivery_outcome` says what
  the statuses mean, computed at the read and stored nowhere: rejected for
  REJECTION, BUSINESS_RULE_FATAL and SYNTAX_VIOLATION (the buyer's access
  point refusing the document) and for a tax data document TDD_REJECTED
  (the authority refusing it); corrected once a credit note of the
  company naming it (IBT-025, its `reference_invoice_number`; a rejected
  credit note by the invoice it names), rendered after it, is
  transmitted (completed); undelivered for DELIVERY_FAILURE (not the
  document's fault, so not on the correction worklist); accepted for
  ACCEPTANCE and BUSINESS_RULE_WARNING; pending otherwise. The worklist is
  `listOutgoingEInvoices?delivery_outcome=rejected`; the tax data
  documents to send again are
  `listOutgoingEInvoices?tax_data_status=TDD_FAILED` (review round 1).
- **Retry and withdrawal, people only.** A person asks for a failed tax
  data document (TDD_FAILED) to be sent again, or for one reported
  (TDD_SEND, TDD_APPROVED, TDD_ACKNOWLEDGED, or WITHDRAW_FAILED to ask
  again) to be withdrawn, with `einvoicing.outgoing.send` (the person who
  transmits; no permission of its own) at the e-invoice's ETag; the
  connector checked as a send's is. The request writes itself on the
  submission with its Operation, which takes over `operation_id` (so a
  send workflow still following ends at its next step, not owning it,
  and the dispatcher starts `SendEInvoice` for the new Operation; one
  request at a time, 409 `einvoicing.tax_data_request_pending`), audited
  and published, the outbox of the call (ADR-0007). Its step claims the
  call as a send's (counted, held for five call timeouts), reads the tax
  data document first and calls only if it does not show the request made
  (a retry once no longer TDD_FAILED, a withdrawal once WITHDRAW_SEND), so
  a step run twice, or one whose claim lapsed after the call, asks once
  (Evatra has no idempotency key); a retry of a document Evatra shows
  none of is not taken as made (review round 1). After a call it made,
  the step keeps the status it found as awaited (`tax_data_awaited`) and
  goes on reading until the status moves from it, bounded by the 500
  reads, so an outcome Evatra shows only later (a TDD_REJECTED after the
  retry) is still read (review round 1); the transmission shows it
  (`tax_data_awaited`), another request is refused meanwhile (409
  `einvoicing.tax_data_request_pending`), the `tax_data_status` filter
  leaves it out, and it is cleared once no longer followed (review
  round 2). A call that fails is tried again after
  1, 5, 15 and 60 minutes; a 4xx other than 401 and 429 refuses it
  (`einvoicing.tax_data_refused`), the fifth failure fails it
  (`einvoicing.provider_unavailable`), a connector refused since fails it
  with that refusal. However it ends, the request is cleared, the status
  found kept, the delivery read again at once with its reads counted
  afresh, and the Operation succeeded or failed. Cancelling it drops a
  request not yet made once no claim or backoff runs (the Operation stays
  cancelled). No reason is asked for a withdrawal (none in Evatra's call
  as read); the audit record names who asked.
- **Agents** never retry or withdraw (§13): both operations are
  `x-agent: false`.
- **The walkthrough.** `step_einvoice_send` (step 146) goes on: the
  invoice, completed, is refused by the buyer's access point on a fatal
  rule and its tax data document fails; it is on the rejected worklist
  (its credit note failed, so not corrected); a withdrawal of the failed
  document is refused, Ruth has it sent again (TDD_APPROVED) and then
  withdraws it (WITHDRAW_SEND).
- **For the specialist and Evatra (not in Evatra's docs as read):** the
  parameters and answers of `GET /peppol/invoice-mls-list` and
  `/peppol/get-tdd` (assumed: `companyId` and `trackingId` as query
  parameters; a list under `content` of `status` and `description`; a
  tax data document's `status`, 404 while none) and the bodies of `POST
  /peppol/tdd/retry-failed` and `/peppol/tdd/withdraw` (assumed:
  `companyId`, `trackingId`); `/peppol/tdd-mls-list` (not read); which
  statuses are final (WITHDRAW_SEND and TDD_ACKNOWLEDGED taken as written
  above) and whether a withdrawal succeeded shows as a status of its own;
  what a withdrawal means at the FTA and when it is due or allowed; whether
  DELIVERY_FAILURE calls for a resend or a credit note; whether a
  rejected tax data document (TDD_REJECTED) is corrected by credit note
  as a Peppol rejection is.

### T65c: validation and refusals (adr:ADR-0061 · po:2026-11-09)

Built as §4's Process and §5: migrations 000416 (the processing's
columns, the status `refused`, the checks NOT VALID, the trigger allowing
`received` to `refused` only and fixing what processing read and decided,
the partial index `einvoice_unprocessed`) and 000417 (the checks
validated); the activity `ProcessEInvoices` (`process.go`) and the
receiving side's reading of PINT AE (`receive.go`, `Received`) on E2's
parser (`ParsePINTAE`, which gains `FieldError`, the order reference
IBT-013 and the order line IBT-132). Decided by default:

- **Where it runs.** After a fetch records its messages, the workflow
  `FetchEInvoices` (a version marker, `einvoice-process`, so a fetch
  started before it ends without the step) and the in-process fetcher
  process every document of the connector not processed yet, oldest
  received first, whichever fetch recorded it: each one's bytes read from
  the store with no transaction open and compared with the recorded
  SHA-256 (other bytes fail the step), then, in a transaction of its own,
  the row read `FOR UPDATE` where it is still `received` and unprocessed
  (another fetch's processing meanwhile is skipped, tested), validated
  and written. The step is idempotent (run-twice test): a processed
  document is never processed again, and the fetch's `refused` count is
  read from the rows (`processed_fetch_operation_id`), so a step run
  again after its commit counts the same. One document's failure does not
  stop the others (review round 1): a document whose stored bytes are gone
  or differ is left unprocessed and the step then fails with
  `einvoicing.document_changed` naming each (no retry repairs it, so the
  fetch fails with it at once); any other failure fails the step for its
  five tries, then the fetch with `platform.unavailable`, the documents
  recorded and left for the next fetch; a connector refused since fails
  it with its problem.
- **What a valid document keeps.** It stays `received` (T65d enters it),
  with `processed_at`, the processing fetch, the release validated
  against (`PINTAERelease`, "PINT AE Billing 1.0.4", the release the
  coordinator's plan read on 2026-11-19; C2 cites 1.0.0 and 1.0.1, so the
  release is for the specialist to confirm before go-live) and what was
  read: number (IBT-001), kind, issue date, currency, gross (IBT-112) and
  its specification identifier (IBT-024). A valid document's processing
  is audited (`einvoicing.einvoice.validated`) and publishes no event, as
  its status does not move; a refusal is audited
  (`einvoicing.einvoice.refused`) and publishes EInvoiceChanged with
  `problem_code` (added within v1), never the detail, which may quote the
  document.
- **The checks, in order** (§5, C3-C6; each field by its business term,
  as the specialist cited them): `unreadable` (not well-formed, a DTD or
  an entity: Go's strict decoder after a scan refusing any directive, so
  no entity is ever expanded); `unsupported_document` (the root not a UBL
  2.1 `Invoice` with type 380 or `CreditNote` with 381, or a
  CustomizationID other than PINT AE's: a self-billed 389 and the other
  codes stay refused, C4); `missing_field`, naming every term missing or
  malformed (IBT-024, IBT-003, IBT-001 at most 200 characters, IBT-002,
  IBT-005, IBT-031 and IBT-048 as 15 digits, IBT-049 with its scheme,
  IBT-106, IBT-109, IBT-110, IBT-112, IBT-115, the VAT breakdown IBG-23
  with IBT-116 to IBT-119, a line, and per line IBT-153, IBT-154,
  IBT-129, IBT-130, IBT-131, IBT-151 and IBT-152; IBT-006 and IBT-111 when
  IBT-005 is not AED, C5); `amount_precision` (more decimals than the
  currency's minor unit, zeros included; fewer are an XML Schema
  decimal's and read); `totals_inconsistent` (the lines' IBT-131 to
  IBT-106, IBT-109 + IBT-110 to IBT-112, the subtotals' IBT-117 to
  IBT-110, naming the figures); `not_addressed_to_company` (C6: listed for
  none of the connector's companies; the buyer's IBT-049 endpoint not the
  connector's participant of the company it was listed for; or IBT-048,
  checked apart, not the number of the company's FTA registration on the
  issue date, a tax group's for a member as the registration holds it). A
  gap in the ERP's own setup refuses no supplier's document for good
  (review round 1): when the connector no longer names a participant of
  the company, or the company holds no FTA registration on the issue
  date, the document is left unprocessed (no write) and a fetch after the
  setup is completed processes it; the fetch's result counts the
  connector's documents still `waiting` (review round 2), so the gap is
  seen.
- **The other mandatory fields of C3 are not refused**: IBT-034 (the
  seller's electronic address), the seller's and buyer's names and
  postal addresses (line 1, city, subdivision, country), IBT-023 and
  IBT-081 are the Ministry's list for the issuer, which the issuer's
  provider validates before delivery; the ERP neither reads them to enter
  the invoice (T65d) nor decides on them, so a document lacking them is
  received, not refused. Decided by default (refusing a statutory document
  over a field the buyer does not use was judged worse), for the product
  owner to overrule; the published Schematron (C7) would check them.
  Allowances, charges and prepaid amounts (IBT-107, IBT-108, IBT-113)
  are not read, so IBT-109 is not compared with IBT-106 nor IBT-115 with
  IBT-112; the order reference, a credit note's invoice reference and the
  order-line reference are read but not required (C4: a credit note
  naming no invoice goes to a person, T65e). A detail quotes at most 80
  characters of any value the document gives and never a TRN in clear
  (the last four of the stated one).
- **The round trip E2 deferred.** What `RenderPINTAE` writes,
  `Received` accepts (a tax invoice in USD with its VAT in AED and a tax
  credit note in AED, `TestReceivedRoundTrip`); E2's parser now reads an
  amount of fewer decimals than the currency's (an XML Schema decimal),
  which E2 never renders.
- **A document too large is not recorded** (§5 asked it recorded refused
  without its bytes): `sha256` and `size` are required in the contract's
  EInvoice and the event, so recording one without them would break
  their consumers within /v1; the fetch keeps failing on it by name
  (`messages.{id}` `einvoicing.document_too_large`), which a person takes
  up with the provider. Recorded here for the product owner to overrule.
- **Not built:** the published Schematron (C7: its licence not read; no
  network in the lane), so validation is the ERP's own rules above;
  entering (T65d).
- **Budget.** `TestProcessEInvoiceBudget`: one document processed beside
  10,000 processed before, p95 250 ms (measured median 22 ms, p95 34 ms
  at scale 1 and 52 ms at scale 2, load 34).
- **The walkthrough.** `step_einvoice` (step 43): GOI-2001, now a PINT AE
  tax invoice to 5000's participant `0235:` and its TIN, is validated and
  stays received with what was read; GOI-2003 cut off in transit and
  GOI-2004 to another TRN are refused (`einvoicing.unreadable`,
  `einvoicing.not_addressed_to_company`), counted `refused` 2, no TRN in
  their details. `step_evatra_inbound` (step 144): Evatra's GOI-2002 is
  validated through the same path.
- **For the specialist:** the release to validate against (1.0.4 per the
  plan; C2's 1.0.0/1.0.1); whether IBT-154 (description) is mandatory on
  received documents as the Ministry's list says (C3) though optional in
  EN 16931; the UBL paths of BTAE-08/BTAE-10 (C5), not read; the
  Schematron's licence (C7).

### T65d: entering e-invoices as supplier invoices (adr:ADR-0061 · po:2026-11-09)

Built as §6 and the entering half of §4's Process and §7: migrations 000423
(the statuses `entered` and `held`, `entry_at`, `entry_fetch_operation_id`,
`supplier_invoice_id` and `supplier_id`, the checks NOT VALID, the trigger
allowing a processed `received` document to move to either and fixing what
the entering decided; payables' source `einvoice`, whose invoice keeps its
document's hash as a captured one does), 000424 (the checks validated),
000425 (`einvoice_to_enter`), 000426 (`einvoice_held`, cell-wide, for
the gauge `erp_einvoices_held`) and 000427 (`einvoice_by_entry_fetch`); `payables.EnterEInvoice` (`einvoice.go`)
behind the port `einvoicing.Payables` (`enter.go`), adapted in
`internal/app/einvoicing.go`. Decided by default:

- **Where it runs.** In the processing transaction, right after a document
  is found valid (§4: check it and enter it), so the fetch that records a
  tax invoice enters it; a tax invoice validated by a fetch before T65d
  (processed, still `received`) is entered by the next fetch without being
  validated again, and one whose bytes the rules refuse by then is held with
  that refusal rather than refused after its validation (both tested). Tax
  credit notes stay `received` for T65e. The step stays idempotent
  (run-twice test): the row is read `FOR UPDATE` where it is still
  `received`, and the fetch's `entered` and `held` counts are read from the
  rows (`entry_fetch_operation_id`). The list's `fetch_operation_id` filter,
  the fetch's `resource`, matches the entering fetch too (migration 000427,
  `einvoice_by_entry_fetch`), so a fetch's `entered` and `held` lead to
  their documents, also those an earlier fetch recorded (review round 1);
  since review:batch86 it matches the processing fetch as well
  (`processed_fetch_operation_id`, migration 000432,
  `einvoice_by_processed_fetch`), so the `refused` count leads to the
  documents it refused that an earlier fetch recorded.
  A retryable problem (a lock timeout or serialization failure, one with a
  Retry-After) fails the step rather than holding the document, and the
  audit entries buffered inside the savepoint (a rule's hold of the new
  invoice) are dropped with its rollback (`audit.DiscardFrom`; review
  round 1). The trigger lets only an unprocessed document be refused, as
  000416 did.
- **The savepoint.** `payables.EnterEInvoice` runs in `SAVEPOINT
  einvoice_entry`: a problem below 500 (`problem.From`) rolls it back and
  holds the document with that problem's code and detail (at most 2,000
  characters), in the outer transaction, which commits; any other error
  fails the step for its retries, the document staying validated and
  received for the next fetch (tested with payables failing). Tested with
  the refusal the database raises, `supplier_invoice_no_duplicates`
  (`payables.duplicate_invoice`), beside capture's own refusals. Entering a
  parked invoice opens no approval, so nothing in the savepoint registers
  an after-commit step its rollback would leave behind.
- **Capture's code.** The document is read as capture reads an extraction
  (`extractionOf`: a tax invoice, its amounts in its currency's digits, the
  order line IBT-132 as the item number, confidence high) and entered with
  `capturedInput`, `capturedTax`, `capturedExtensions` and `captureStatus`,
  then `insert` with source `einvoice`. An IBT-132 that is not an item
  number holds it (`payables.unreadable_document`), as capture refuses a
  line naming no item. Its document is the e-invoice's bytes as kept: the
  invoice's `capture_object_ref` and `capture_sha256` are the e-invoice's
  key and hash (the same object, no copy), so `getSupplierInvoiceDocument`
  serves the XML.
- **The seller.** The order's supplier must hold the seller's TRN (IBT-031)
  as a tax identifier, whatever its scheme, compared by its keyed
  fingerprint (`partner.Payees.HasTaxNumber`, never opening one); otherwise
  the document is held with `payables.einvoice_seller_unknown`, the detail
  naming the TRN's last four characters only.
- **Tax codes by category.** `capturedTax` takes each line's rate with its
  VAT category (IBT-151): only the input codes of that category are
  considered, so the order item's code stands when it has the line's
  category and charges its rate; else the one code that does is proposed
  (the invoice parked with it); with none or several the invoice is entered
  `on_hold` naming them (V5 and VB5 both charge S at 5% in the UAE pack, so
  an order item without a code holds it, tested). A reverse-charge line
  (AE) is matched among the AE codes by the same rule: `charged` leaves out
  the self-assessed components, so an AE line's rate is compared with what
  the supplier charges. Capture's own documents state no category and are
  matched as before.
- **A tax invoice not in AED is held** (`einvoicing.tax_currency_not_entered`)
  rather than entered: C5 asks its VAT be kept in AED as it states it
  (IBT-111, reconciled per line with BTAE-08, whose UBL path is not read
  yet) for the VAT 201, and a supplier invoice converts its tax at the tax
  date's rate (ADR-0049 §8) with no field for a stated local tax. A person
  enters it by hand (T65f links it). The rest of C5 is the tracker leaf
  "E-invoices not in AED entered with their VAT in AED" (split:T65d).
- **The fetch's company access** (security posture, for the product owner
  to overrule). The fetch's system principal (`einvoicing.Fetcher`, no
  principal row) acts, while it enters a document, for the company the
  document is addressed to and no other: `authz.ActingFor`, which
  `authz.Companies` answers for a system principal only (a person's or
  agent's access is never widened by it); payables' reads and writes then
  apply company access as for any caller.
- **The list and the event.** `GET /v1/e-invoices` and `/{id}` carry, once
  entered, the invoice's id, number, supplier reference and status and its
  supplier's id and name, read live per page (payables' `Invoices.Sources`,
  partner's `Names`), and `entered_at` with the entering fetch for entered
  and held documents; the filter `supplier_id` (the stored supplier of an
  entered document). EInvoiceChanged is published for `entered` and `held`
  (status_before `received`) and carries `supplier_invoice_id` (added within
  v1); the audit records are `einvoicing.einvoice.entered` and `.held`;
  SupplierInvoiceCaptured is published as today, with source `einvoice`.
  The tax codes proposed on entering are on the invoice's items; their
  warnings are not kept apart.
- **The gauge.** `erp_einvoices_held` counts the documents held, cell-wide
  as the worker role, reading `status` alone.
- **The walkthrough.** `step_einvoice` (step 43): Alex records Gulf Office
  Interiors' TRN, approved by Carla; Ben orders eight desks with V5 for 5000
  and receives them; GOI-2001, naming the order and its line 1, is validated
  and entered on the first fetch, a parked invoice of source `einvoice` with
  V5, its document the XML (hash compared), matched clean and posted;
  GOI-2005, naming no order, is held with `payables.capture_needs_order`.
  `step_evatra_inbound` (step 144): Evatra's GOI-2002, the other four
  desks, is entered as a parked invoice through the same path.
- **For the specialist:** whether IBT-152 on a reverse-charge (AE) line is
  0 (as EN 16931's BR-AE-05 has it, from memory) or the rate self-assessed;
  BTAE-08's and BTAE-10's UBL paths (C5), which the entering of documents
  not in AED needs.

### T65e: e-invoice credit notes (adr:ADR-0061 · po:2026-11-09)

Built as §6's credit memo rule and §5 C4: no column changes; migrations
000433 (`einvoice_to_process`, the received documents of a connector by
status alone) and 000434 (`einvoice_to_enter`, tax invoices only, dropped
as its contract step: the new index serves the previous version's
condition too, which implies it). Decided by default:

- **What is entered.** A valid tax credit note (381) is entered in the
  processing transaction as a tax invoice is (T65d), through the same
  port and savepoint, as a supplier credit memo of source `einvoice`
  (`payables.EnterEInvoice` with the kind `credit_memo` and the cited
  number, IBT-025): capture's code names as its reference the posted
  invoice of the supplier in the company and the note's currency whose
  supplier reference is that number (`creditedInvoice`, 03 §3.9), and the
  memo is parked; a number that is no such invoice (not posted yet, or
  none) enters it `on_hold`, its hold reason naming the number, for a
  person to enter the reference rather than having it dropped. Its
  items, tax codes by category and rate, the duplicate guard and every
  other refusal are T65d's; its tax date is the credited invoice's when
  it names one (`taxDateOf`, P6).
- **A credit note naming no invoice is held**
  (`einvoicing.credit_note_names_no_invoice`), not handed to payables:
  capture's rule would enter a paper memo naming none as correcting none,
  but a UAE tax credit note names the invoice it corrects (IBG-03,
  IBT-025; the UAE tax specialist, C4 [M]), and routing one that names
  none to a person is safer than entering it. Decided in the einvoicing
  context beside the hold of a document not in AED, which now applies to
  credit notes as well (C5).
- **A credit note naming several invoices is held**
  (`einvoicing.credit_note_names_several_invoices`, the detail naming
  them): the mandate lets one e-credit note cover several invoices, but a
  credit memo names one invoice and its credit limit is that invoice's
  gross, so a person enters one memo per invoice rather than the note
  being entered against the first alone. The parser now reads every
  IBG-03 (`Document.Cited`, each number once, blanks left out; before,
  the last one silently won). A tax invoice may state a preceding invoice
  in IBG-03 too: only a credit note's is handed to payables as the
  invoice it credits, so such an invoice is entered as before (review
  round 1).
- **Credit notes left received by T65d.** A fetch lists every `received`
  document of its connector (`toProcess` is the status alone: every
  processed document still received is a tax invoice or tax credit note),
  so a credit note validated before T65e is entered by the next fetch
  without being validated again (tested), as T65d did for tax invoices.
  The step stays idempotent (run twice, tested).
- **The fetch's documents** (review:batch86, migration 000432,
  `einvoice_by_processed_fetch`): `fetch_operation_id` matches the
  processing fetch too, so the `refused` count leads to the documents it
  refused that an earlier fetch recorded.
- **Not built:** the credit note's reason (BTAE-03) is read but not kept
  on the memo, and its credited invoice's date (IBT-026) is not read.
- **The walkthrough.** `step_einvoice` (step 43, `einvoice_credit_notes`):
  GOI-2101, a tax credit note correcting GOI-2001 (one damaged desk, AED
  262.50), is entered by the next fetch as a parked credit memo of source
  `einvoice` whose `reference_invoice_id` is GOI-2001's posted invoice,
  with V5; GOI-2102, naming no invoice, is held with
  `einvoicing.credit_note_names_no_invoice`.
- **For the specialist:** whether a tax credit note correcting several
  invoices repeats IBG-03 (several `BillingReference`) in PINT AE, as
  assumed here from the mandate's summary [secondary, plan
  einvoicing-evatra.md], and so whether entering one memo per invoice is
  what the VAT 201 expects.

### Batch 88: credit notes as one memo, VAT in AED as stated (po:2026-11-21 · review:batch87)

The product owner approved on 2026-11-21 the coordinator's research of
the open UAE questions (sources: OpenPeppol's PINT AE Billing pages, the
AE specific rules and the business process, read 21 Nov; recorded in the
coordinator's memory po-2026-11-21-uae-vat-research). Built:

- **A volume discount may name no invoice (IBR-055-AE).** A received tax
  credit note whose reason (BTAE-03) is `VD` and that cites no invoice is
  valid and entered as a supplier credit memo without a reference (it
  corrects no one invoice; its tax date is its own, `taxDateOf`); only a
  credit note for another reason citing none is held
  (`einvoicing.credit_note_names_no_invoice`, its detail naming the VD
  exception). The outgoing rule (`einvoicing.pint_ae.credited_invoice`)
  lets a VD tax credit note name no invoice too. This corrects T65e,
  which held every one (C4 had assumed IBG-03 always mandatory).
- **Several invoices, one memo.** A credit note citing several invoices
  (IBG-03 repeated, which PINT AE's business process allows) is entered
  as ONE credit memo citing them all, rather than held
  (`einvoicing.credit_note_names_several_invoices` is no longer given;
  documents held with it before stay held, and the value stays in the
  contract's open enumeration). Its `reference_invoice_id` is none, since
  no one invoice is what it corrects: its tax date is its own date, the
  VAT 201 reporting it by the credit note's date with no per-invoice
  split; it posts within no single invoice's credit limit (the limit,
  product owner 2026-11-10, is one named invoice's gross, and a memo
  naming none has none); the accounts payable clerk clears it against the
  invoices. Decided by default, for the product owner to overrule.
- **What the memo keeps.** `payables.supplier_invoice_citation`
  (migration 000439, insert-only): each cited invoice's number (IBT-025),
  its issue date when stated (IBT-026, `cac:BillingReference/
  cac:InvoiceDocumentReference/cbc:IssueDate`, path from memory [M]) and
  the supplier's posted invoice it matched; and the reason's code and
  words (`credit_reason_code`, `credit_reason_text`, BTAE-03, words beyond
  1,000 characters shortened, a code beyond 100 or more than 100 cited
  invoices not read: a hold). The resource shows them as `credit_reason`
  and `cited_invoices` (with the matched invoice's `document_number`).
  Not copied into audit details or events: the reason's words are the
  supplier's free text (ADR-0079).
- **Matching.** Each cited number is matched to the supplier's posted
  invoice in the company and the memo's currency by its supplier
  reference, and by its invoice date when the note states IBT-026
  (`citedInvoices`); a memo citing one names it as `reference_invoice_id`
  as before; any number matching none enters the memo `on_hold`, its hold
  reason naming the numbers. A number repeated is cited once, with the
  first date stated for it.
- **VAT in AED as stated (split:T65d; C5).** A received tax invoice or
  credit note not in AED that states each line's VAT in AED (BTAE-08,
  `cac:InvoiceLine/cac:TaxTotal/cbc:TaxAmount` in AED; BTAE-10 the line's
  amount with VAT in AED, `cac:InvoiceLine/cac:ItemPriceExtension/
  cbc:Amount`; both paths from memory [M], for the specialist) is
  entered rather than held: the invoice keeps the lines' sum as its
  `stated_local_tax` in AED (migrations 000441, 000442), which its
  posting takes as the local amount of the tax the supplier charges
  (`statedLocalTax`: the difference to the tax date's conversion shared
  out among the charged components by their converted tax,
  `money.ShareOut`, which moved from payables; self-assessed components
  keep their conversion; the supplier line's conversion at the posting
  date leaves the exchange difference), so the VAT 201 reports the VAT as
  the document states it. An exempt line states none (IBR-163-AE); one
  without it, on any other line, holds the document
  (`einvoicing.tax_currency_not_entered`); the lines' VAT in AED not
  summing to IBT-111 (or, in AED, to IBT-110) refuses it as
  `einvoicing.totals_inconsistent`. A change of a parked invoice's
  currency or tax drops the stated tax, audited
  (`stated_local_tax_dropped`); a stated tax its charged codes cannot take
  is 422 `payables.local_tax_mismatch` at posting. Decided by default: the
  stated tax is shared by the components' converted tax rather than
  matched per line (UAE input codes have one component, so the two agree
  to the fils for a code's lines).
- **Sending: BTAE-08 and BTAE-10 on every line (IBR-194-AE).** E2's
  `not_rendered` findings for a document not in AED are gone: each line
  states its VAT in AED and its amount with VAT in AED, from the posted
  entry's output tax items per code in the company's currency
  (`Sale.LocalCodes`: the code's local base and tax shared among its
  lines by their net and tax), so the lines' VAT in AED adds up to
  IBT-111; a document in AED states its own tax and net plus tax. An
  exempt line states no BTAE-08; zero-rated lines state 0 (IBR-162-AE,
  IBR-165-AE as researched; reverse-charge lines are not rendered yet).
  IBT-152 stays the code's rate, 0 for zero-rated lines. The VAT in AED
  (IBT-111, BTAE-08, BTAE-10) is now written with AED's two digits
  whatever the document's currency's digits (before, IBT-111 took the
  document's). A line whose code has no posted local amounts (an
  unposted document) keeps them out, which a finding names.
- **The CHECK review:batch87 asked for.** `einvoice_read_processed`
  (migrations 000437, 000438): a processed document not refused states its
  `document_kind`, which the entering branches on.
- **The walkthrough.** `step_einvoice` (step 43, `einvoice_credit_notes`):
  GOI-2103, a VD credit note naming no invoice, is entered as a parked
  credit memo without a reference; GOI-2104, citing GOI-2001 (dated) and
  GOI-2005 (no posted invoice), is entered as one memo citing both, on
  hold naming GOI-2005; GOI-2102 (reason other than VD, naming none) is
  held as before. `einvoice_not_in_aed`: GOI-2201, a tax invoice in USD
  on a dollar order stating its VAT in AED on its line (BTAE-08, AED
  183.63), is entered with that `stated_local_tax`. `step_einvoice_send`
  (step 146, `einvoice_render_usd`): a UAE customer invoice in USD is
  rendered valid, its line stating BTAE-08 and BTAE-10.
- **A partly recovering code** takes its share of the stated tax's
  difference between its recovered part and the part not recovered as
  its converted local tax divides (review round 1), as the recoverable
  share of its code says.
- **For the specialist:** the BTAE-03 code list (the walkthrough's and
  tests' `DL` is illustrative; only `VD` is acted on); the UBL paths of
  IBT-026, BTAE-08 and BTAE-10 (from memory); whether IBR-162/163/165-AE
  say what the coordinator's research summarised (zero-rated and
  reverse-charge lines 0, exempt lines none).

### Batch 89: a volume discount without an order (po:2026-11-21)

Batch 88 entered a volume discount's tax credit note naming no invoice
only when it named an order: capture's code takes the supplier, the
currency and the items from the order, so one of a supplier the company
buys from without orders was still held with
`payables.capture_needs_order`. The product owner's rule of 2026-11-21 is
that such a credit note is entered as a credit memo without a reference,
and a credit memo needs no order. Built (`payables.enteredInput`,
`rebateInput`):

- **Which documents.** A tax credit note whose reason (BTAE-03) is `VD`
  and that names no order reference (IBT-013) is entered without an
  order, whether or not it cites invoices (its citations are matched as
  batch 88 matches them). Every other document naming no order, a credit
  note for another reason among them, is held with
  `payables.capture_needs_order` as before: its account is the corrected
  invoice's, which an order or a person gives (Later: take it from the
  cited invoice's items). Decided by default.
- **The supplier.** The one supplier of the company (the supplier role,
  with supplier data in the company) holding the seller's TRN (IBT-031)
  as a tax identifier, found by its keyed fingerprint without opening one
  (`partner.Payees.HoldersOfTaxNumber`, FindCustomers' lookup by a tax
  number alone; pending partners excluded). None holds the document with
  `payables.einvoice_seller_unknown`; several with
  `payables.einvoice_seller_ambiguous`, naming the suppliers' numbers for
  a person to keep the TRN on one; both details name the TRN's last four
  only.
- **The items.** Each line is an item of quantity 1 `EA` with the line's
  net amount, as a discount is credited by amount and the document's
  units are not mapped to the ERP's (§6; the send side writes each line
  as 1 C62 for the same reason, As built E2), its description the line's.
  The account is the account determination's `cash_discount`, the
  discounts received from suppliers (SAP SKE; the AE pack's 4800
  "Purchase discounts received", a revenue account): a volume discount is
  a reduction of the price of past purchases received from the supplier,
  of the same nature, and no key of its own exists (SAP books vendor
  rebates through its own account key, BO1, from memory [M]; a key of
  our own is a backlog item for the product owner). The clerk may change
  the account while the memo is parked. A company without the key holds
  the document with `payables.capture_needs_account`. Decided by default.
- **The tax as stated.** The memo's tax and gross are the document's;
  each line's code is found from its category and rate by capturedTax's
  rule, so where two input codes charge the rate (the AE pack's V5 and
  the blocked VB5, Executive Regulation Art. 53) the memo is entered
  `on_hold` naming them, for the accounts payable clerk to choose, as any
  line without an order item's code is; a non-AED document keeps its
  stated VAT in AED (batch 88). The e-invoice is `entered` either way:
  the memo, not the document, waits for the person.
- **The walkthrough.** `step_evatra_inbound` (step 144,
  `evatra_volume_discount`): Alex sets up Emirates Workspace Supplies, a
  supplier of the UAE company with its TRN and no purchase order; its
  volume discount EWS-501 (AED 250.00 and 12.50 VAT, no invoice, no
  order) is received through Evatra and entered by the fetch as a credit
  memo of source `einvoice` without an order or a reference, its item
  1 EA on 4800, on hold for its code; Alex releases it, chooses V5 and
  posts it.
- **For the specialist:** whether a volume discount's credit note
  correcting purchases whose input tax was blocked (VB5) needs the
  blocked code rather than V5 (the walkthrough assumes the supplies'
  VAT was recovered).
