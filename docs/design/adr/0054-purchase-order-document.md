# ADR-0054: The purchase order document: plain text, rendered from the stored order on read

**Status:** accepted (decided by default under ADR-0053 d, T72g, 2026-10-28).

## Context

The product owner's pilot triage (2026-10-21, T72g) needs a purchase
order document produced for the supplier through the API: "a text/PDF
form". Email and EDI transmission stay post-pilot. The prototype removed
in T6 rendered an order as text, or as an X12 850 when the configured
channel was `edi`, and stored the result as an output record at
creation. Nothing since has produced a document for a supplier.

What is held, and so what a document can show:

- the order: document number, revision (ADR-0034), order date, currency,
  payment term, purchasing organisation, status, and its live items with
  description, quantity, unit, price per price unit and tax code; no
  delivery date (when this was decided: T53g added an item's optional
  one, As built below), delivery address or plant;
- the requisition an order was made from, with the date its goods are
  needed by;
- the buyer's legal entity: code, name and country, no postal address;
- the supplier: name, number, country, and effective-dated addresses by
  purpose (`legal`, `remit_to`, `order_from`, `ship_from`); migrated SAP
  suppliers have none, the mock extract holding no street;
- the payment term's description.

The configuration's `output.purchase_order_channel` (`email`, `edi`,
`print`) names how an order is to be transmitted; no transmission is
built. The data model has had a table for it since 000010,
`integration.output_message` (object, channel, recipient, subject, a
reference to the body, the transmitted message), which nothing writes
yet. The module has no PDF library, and the brief allows no new
third-party module.

No ADR settled the output's format, where it is rendered, whether it is
stored, or whether producing it is audited.

## Decision

1. **A sub-resource that is a document.** `GET
   /v1/purchase-orders/{id}/document` (`getPurchaseOrderDocument`)
   answers the order's document as `text/plain; charset=utf-8`, as
   `GET /v1/supplier-invoices/{id}/document` and `GET
   /v1/bank-files/{id}/content` answer theirs as files (04 §4.11). It
   needs `procurement.order.read` (403 `identity.permission_missing`
   without it) and the order's company, as reading the order does: a
   caller without the order's company gets 404, so the order's existence
   does not leak.
2. **Only an order that may go to the supplier.** A `released` or
   `closed` order has a document; a `draft`, `pending_approval` or
   `cancelled` one is refused (409 `procurement.invalid_status`). A
   closed order's document says that nothing more is to be delivered on
   it, so a copy sent again is not taken for a new order.
3. **Rendered from stored data on each read, and not stored.** The
   document is the order's current revision with today's master data:
   the supplier's `order_from` address valid today, else its `legal`
   one, else a line saying neither is held; the buyer company's name,
   code and country; the purchasing organisation's code and name; the
   payment term's code and description; the requisition's needed-by
   date. A later revision is marked as replacing the one before it.
   Nothing is kept: no output record, no object in storage.
   `integration.output_message` stays the record of a transmission, to
   be written when one is built, with the body it sent.
4. **A read: no audit record, no event.** Producing the document changes
   no state (ADR-0011 records business transactions; reads are not
   audited, as downloading a bank file or a captured invoice is not).
   Sending it is the state change, and is recorded when transmission is
   built.
5. **Amounts as the API writes them.** Every amount carries the order's
   currency code and has the currency's minor-unit digits without
   grouping (ADR-0005, 04 §4.4). Item amounts are quantity × price /
   price unit, rounded as the order's total is; an order whose amounts
   are out of range is refused (422 `procurement.amount_too_large`), not
   printed with a wrong total.
6. **Texts cannot forge lines.** Names, addresses and item descriptions
   are typed by people or migrated. Control characters, Unicode line and
   paragraph separators and the bidirectional formatting characters
   (marks, embeddings, overrides and isolates) in them are printed as
   spaces, so no value starts a line of
   its own (a second "Total", a bank account) or reorders one.
7. **The channel is not consulted.** The document is the printed form
   whatever `output.purchase_order_channel` says; the channel will choose
   the transmission (email, EDI) when it is built.

## Alternatives

- **A PDF.** Rejected for now: rendering one needs either a third-party
  module, which the step may not add, or a PDF writer of our own. A
  writer limited to the standard fonts could print Latin text only; the
  UAE pilot's names in Arabic need an embedded font with those glyphs and
  right-to-left, joined shaping. What a PDF would need is listed below
  for a decision.
- **The prototype's channel switch** (X12 850 when the channel is `edi`).
  Rejected: X12 850 is post-pilot (PO 2026-10-21), and an EDI message is
  transmitted, not fetched by a buyer.
- **Render once at release and store the bytes, as bank files are**
  (ADR-0016). Rejected: a bank file is evidence of what was sent to a
  bank and is checked against its hash; nothing is sent here yet. When
  transmission is built, what was sent is stored with its record then.
- **A JSON representation.** Rejected: `getPurchaseOrder` with
  `expand=supplier` already serves integrations; the document is for the
  supplier, a person reading it.
- **Audit each rendering.** Rejected: it is a read, and reads are not
  audited (4).

## Consequences

- A buyer, an agent or an integration can fetch the document of any
  released order and send it on by hand; the walkthrough's step 4 shows
  it.
- The document shows what is held. Missing from what a supplier usually
  needs, and not held anywhere: the buyer company's postal address and
  tax registration, a delivery address per item, and a contact person
  (a delivery date per item is held since T53g and printed, and the
  buyer's postal address is configuration since 2026-11-04: As built).
  Adding them is new master data, a product owner decision.
- The contract an order is placed against, and a contract item's
  material key (often the supplier's own part number), are held but not
  printed: T72g's acceptance does not name them. Printing them is a small
  addition if suppliers ask for it.
- A reprint after the supplier's address changed shows the new address;
  the order's own fields are its current revision.

## As built

- **An item's delivery date (podate, 2026-11-04; decided by default under
  §3, which prints what is held).** Order items carry an optional
  `delivery_date` since T53g (ADR-0051 As built, migration 000205), copied
  from the requisition's needed-by date when the order is created from
  one and the item names none. The document prints it under the item's
  amount as `Delivery date YYYY-MM-DD`, in the item lines' wording (a
  label and its value, no colon). The requisition's date stays in the
  terms block for the items without a date of their own: `Needed by:
  YYYY-MM-DD` as before when no item has one (an order written before
  000205), `Needed by: YYYY-MM-DD, for the items without a delivery date
  of their own` when some do, and not printed when every item has its
  own, so a supplier never reads two dates for one item without being
  told which applies. Dates are formatted by the renderer, not typed
  text, so §6's escaping has nothing to add.
- **The buyer's postal address and where to deliver (podocb, 2026-11-04;
  product owner 2026-11-04, decision sheet question 2, option B without
  the full registration number).** A company's postal address is
  configuration, `companies[].postal_address`: `line1`, `line2`, `city`,
  `postal_code`, `region`, `country_code`, the shape of a business
  partner's address (the contract's `Address`, its lengths and required
  fields) without purpose or validity, a configuration version being its
  history. Decided by default: it is set through a change request as every
  configuration key is, audited as each change request's steps are, and a
  change is **tier 2 (operational)**: it posts nothing, so it is not tier
  3, and it decides where suppliers deliver, a route, so it is not tier 1
  as the company's name is. It is **optional**, as nothing requires it
  today (making it required, as `time_zone` became, is the product
  owner's call). The document prints it in the buyer block (lines as the
  supplier's address), and again under `Deliver to`, after the supplier,
  with the buyer's name: no item names a delivery address of its own (none
  is held per item; plants and delivery locations are out of scope), so
  the buyer's address is every item's. It is read from the active
  configuration at each read (§3), and its texts are printed as the other
  texts are (§6). **Without one** the document is still issued, decided by
  default as the supplier's missing address already is (§3): refusing it
  would turn every released order's document of a tenant without the key
  into a 409, and the document never prints an address nobody gave. The
  buyer block then prints `Country <code> (no postal address configured)`
  and the delivery block `No delivery address held: ask the buyer where to
  deliver.` A legal entity the configuration has no company for reads as
  one without an address. The buyer's tax registration number is not
  printed (product owner: it stays as it is, the last four characters
  only where shown).
- **The supplier's street from SAP (podocb).** The document prints the
  supplier's address lines as before (street lines, city with region and
  postal code, country; an empty optional line is no longer printed as a
  blank one). Migrated suppliers now have one: the extract's LFA1 carries
  optional STRAS, ORT01, PSTLZ and REGIO (field names from memory, pending
  the SAP specialist, who is to say whether the address is taken from
  LFA1 or the central address ADRC by ADRNR; docs/migration.md "What to
  check on a real SAP system"), and the load gives a vendor with a street
  and a city its `legal` address, valid from the day of the run. An
  extract without the fields (an OData one) loads suppliers without an
  address; a street without a city, a city without a street, or a text
  longer than an address holds is warned of by the quality gate and the
  supplier loads without one: the run never fails on it. A supplier an
  earlier run created is only extended with companies, so it keeps the
  address it has. The configuration draft generated from SAP (sapconfig)
  carries no company address: its reviewers enter it in the draft (where
  SAP keeps a company code's address is not read; no tracker item, as the
  product owner placed the address in configuration).
- **No version of its own.** The document has no ETag and no cache
  header: it is rendered on every read (§3). A released order's item
  changes its delivery date only through an order change (ADR-0034),
  whose application raises the order's revision, which the document
  prints as replacing the one before; a draft has no document.

## What a PDF would need

- A PDF library (a third-party Go module) or a writer of our own, with a
  font embedded for every script the tenants' names and addresses use
  (Latin, and Arabic for the UAE pilot, with right-to-left shaping).
- A second media type on the same operation (`application/pdf`, chosen by
  `Accept`), or a `format` parameter; the text stays the default.
- A form layout per document type, which the tracker holds as "Form
  templates per document type" (post-pilot).
