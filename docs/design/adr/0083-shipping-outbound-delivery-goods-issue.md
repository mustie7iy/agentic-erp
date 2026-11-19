# ADR-0083: Shipping: outbound delivery, picking and goods issue

**Status:** accepted (ship lane, S2a and S2b, 2026-11-19; decided by
default under ADR-0053 d, as `docs/design/plan-supply-chain.md` asks:
"Shipping … decided by its own ADR when built"). It decides the context
of the slices S2a, S2b1 and S2b2 of the supply chain plan. **The SAP
behaviour named here is stated from memory** (no primary source in the
repository); the plan's "Facts for the specialist" lists it.

## Context

The sales chain confirms a delivery by hand (T78c, ADR-0067 §8): a
person records that quantities of a confirmed order's items were
delivered on a date, in the sales context (`sales.delivery`); it moves no
stock and posts nothing, and billing bills what is delivered and not yet
billed (ADR-0067 §9). Since S1f1 an order item may name a material, and
since S1c-S1e materials are kept in stock, valued (ADR-0082 §7-§8) and
moved by material documents posted through the inventory context.

`internal/delivery` is not shipping: it holds the implementation
delivery agents (`configagent`, `migrationagent`, `testagent`) and the
SAP customizing generator (`sapconfig`), the delivery of the ERP to a
client, not of goods to a customer. Nothing in the repository builds
outbound deliveries.

What the design already settles: contexts own their schema and read one
another through ports the composition root wires, in the callee's
published types (ADR-0001, `internal/layout`); RLS with `tenant_id`
leading every key (ADR-0003); numbers from the configuration's ranges per
company, document type and fiscal year (ADR-0004); no floats (ADR-0005);
the outbox (ADR-0007), row locks and state guards (ADR-0008), audit
(ADR-0011); financial effects through the ledger's posting port alone;
stock moved only by inventory's material documents, valued by inventory
(ADR-0082 §2: `inventory.Movements` is the port "shipping's goods issue"
posts through).

## Decision

### 1. A new leaf context `shipping`

`shipping` (schema `shipping`, `internal/shipping`) owns outbound
deliveries, their picking and their goods issue. It imports the
published APIs of `sales` (the order, and the order's delivery bookkeeping,
§4), `inventory` (the goods issue and the stock a pick checks, §5),
`materials` (a material's number, plants, storage locations and unit
conversions), `partner` (its customers' names), `config` and `tenancy`,
through ports the composition root
wires, and the ledger's fiscal calendar through a port in its own types.
It is a leaf: no context imports it. It does not import the ledger's
posting port: the goods issue's value is posted by inventory as every
material document's is (ADR-0082 §8), so shipping posts nothing itself.

The new directions are `shipping → sales`, `shipping → inventory`,
`shipping → materials`, `shipping → partner`. No cycle: sales,
inventory, materials and partner import nothing of shipping.

### 2. Data model

- `shipping.outbound_delivery` (SAP LIKP, from memory): the company, its
  number from the company's `outbound_delivery` range of the fiscal year
  of its creation day, the sales order by id and number, the customer by
  id and number, the plant and storage location it ships from, its
  status (`created`, `goods_issued`), once issued the goods issue date,
  the material document by id and number and the costs' currency, a
  version, who created and changed it.
- Its picking status (`not_picked`, `partly_picked`, `picked`; KOSTK A,
  B, C) is computed from its items.
- `shipping.outbound_delivery_item` (LIPS): the order item it delivers,
  its material, description, quantity and unit (the order item's), the
  picked quantity (0 up to the quantity) and, once issued, the cost of
  the quantity issued in the company's currency.
- A delivery is never deleted and changes only while `created`: picking
  sets picked quantities; the goods issue sets its status once; a
  database rule refuses anything else.

### 3. What a delivery takes

A delivery is of one sales order that is `confirmed` and not blocked for
credit, from one active plant of the order's company and an active
storage location under it. Its items are the order's items not removed
that name a material kept in stock (`materials.Material.Stocked`)
extended to the plant and kept at the storage location, each once, with a
positive quantity in the order item's unit of at most what is open:
ordered less delivered less what other deliveries not yet issued hold.
Without items the delivery takes every such item's open quantity.

### 4. Sales keeps what deliveries hold

So that an order change cannot take an item below what a delivery holds,
sales keeps it: `sales.delivery_assignment` (insert-only, signed rows per
outbound delivery and order item) adds to
`sales.sales_order_item.in_delivery_quantity`, and
`delivered_quantity + in_delivery_quantity <= quantity`. A delivery's
creation assigns its quantities; its goods issue gives them all back and
delivers what was issued, in one transaction. An order change refuses a
quantity below delivered plus in delivery, the removal of an item a
delivery holds, and the cancellation of an order a delivery holds (the
codes of T78d and T78e, their messages naming the quantity in delivery).
Sales publishes this as methods of its endpoints (`Ship`, `Issue`), which
lock the order `FOR UPDATE` as T78c's confirmation does.

### 5. Picking and goods issue

- **Picking** (`:pick`, `If-Match`) sets each named item's picked
  quantity, in its unit, at most its delivery quantity and, converted to
  the base unit, at most what the storage location holds of the material
  now (inventory's stock). Picking reserves nothing (SAP without WM or
  EWM, from memory); the goods issue checks the stock again under
  inventory's locks.
- **Goods issue** (`:post-goods-issue`, `If-Match`) posts movement type
  601 (goods issue for a delivery) for each item's picked quantity as one
  material document, through `inventory.Endpoints.IssueForDelivery`, on
  the company's date today, with the delivery's number as the document's
  reference. Inventory values it as every issue (ADR-0082 §8: at a
  standard price the stock left at the price, at a moving price the value
  in proportion) and posts it in one WA entry: the class's stock account
  credited, its `cost_of_goods_sold` account (SAP GBB VAX, from memory)
  debited. Each item's cost is what its line took from the stock. The
  sales order's items are delivered by it: sales writes its delivery
  (T78c's record) naming the outbound delivery and the material document,
  with each item's issued quantity and cost, and billing, unchanged, bills
  what is delivered and not billed. The goods issue issues what is
  picked; the delivery keeps its delivery quantity, and what was not
  picked goes back to the order, open for another delivery (where SAP
  would have the delivery quantity changed to the picked one before the
  goods issue, from memory); a delivery with nothing picked is refused.
  It posts once: the delivery's row lock and status.
- **T78c for stock**: a person no longer confirms the delivery of an
  item of a material kept in stock (422 `sales.delivery_by_goods_issue`):
  its delivery is a goods issue. Service and free-text items, and items
  of a material kept in no stock (`non_stock`, `service`), keep T78c.
- 601 is posted only by shipping and not reversed by a person (409
  `inventory.reverse_goods_issue`): its reversal (602) with the billing
  cancelled is Later.

### 6. Cost of sales by profitability segment (S2b2)

Sales' delivery items carry the goods issue's cost, so `Orders.Segments`
adds each segment's cost of sales from the goods issues posted in the
period, by the order item's customer and item key; the report gains
`cost_of_sales` and `margin` per row and in its totals and
`includes_cost` becomes true. A goods issue counts on its posting date,
the date its cost is posted; one of an order in a currency other than the
company's is left out and counted, as its revenue is (ADR-0075 §6).

### 7. SAP behaviour followed, and where it is not

Followed (from memory): the delivery as its own document after the
order, with picking and a goods issue that posts movement 601 against the
cost of goods sold; billing from the goods-issued delivery; the picking
status A/B/C.

Not followed: the delivery quantity is not changed to the picked one
before the goods issue (SAP VL02N, from memory): the delivery keeps it,
issues what is picked and gives the rest back to the order. Nor are
shipping points, routes, delivery split and grouping, packing and
handling units, proof of delivery and delivery notes;
deliveries from several plants in one document; an availability check or
reservation of stock (S2j); goods issue reversal (602), returns
deliveries and partial goods issue: all Later in the plan.

## Consequences

- The sales order's delivered quantity of a stock item now moves only by
  a goods issue, so billing a stock item needs its stock: what the
  walkthrough and a pilot used to confirm by hand now ships.
- Every stock company names a `cost_of_goods_sold` account for its
  valuation classes before it ships (422 `inventory.no_inventory_account`
  otherwise).
- Shipping depends on sales' and inventory's published APIs; a change of
  either is a change of shipping's port.

## Alternatives

- **Deliveries inside sales.** Rejected: sales would import inventory,
  and the delivery, picking and goods issue are a context of their own in
  every reference ERP (SAP LE-SHP). The order's bookkeeping of what is in
  delivery stays in sales, which owns the order.
- **Shipping reads open quantities from its own deliveries.** Rejected:
  an order change could then take an item below what a delivery holds,
  since sales cannot read shipping (a cycle).
- **Shipping posts the cost of goods sold through the ledger's port.**
  Rejected: the material document's value is inventory's (ADR-0082 §8);
  two entries would split one movement.

## As built

**S2a** (lane ship, 2026-11-19; migration 000406, walkthrough step 141,
`step_outbound_delivery`). Decided by default while building:

- **Ports.** `shipping` imports `config`, `inventory` (from S2b1, for its
  goods issue's types), `materials`, `partner` (its customers' names, read
  at the response as a sales document's are, through `partner.Payees`),
  `sales` and `tenancy`
  (`internal/layout`, a leaf); the ledger's fiscal calendar and the
  configuration's ranges and company dates are ports in its own types.
  Sales' published API is `sales.Orders` (the order, read without a lock
  to find its company and items' materials) and `Endpoints.Ship`, which
  takes the quantities under the order's row lock and answers the order
  as read with its customer's number. Each item a request names is judged
  eligible on that locked order, so an item an order change added after
  the first read is checked as any other.
- **Customer and order by number.** The delivery stores the order's and
  customer's numbers, which never change; plants, storage locations and
  materials are named at the read.
- **Picking status is computed** from the items, not stored, so it cannot
  disagree with them.
- **Items without a request's items** are every standing order item of an
  active material kept in stock at the location with something open; a
  named item is refused when it names no material, a material kept in no
  stock (`shipping.item_not_stocked`), a material not active
  (`shipping.material_not_usable`) or not kept at the location
  (`shipping.material_not_at_location`); an item the order lacks or has
  removed is sales' refusal.
- **Picking replaces** an item's picked quantity (zero allowed: the pick is
  put back, `money.QuantityOrZero`); the check against the stock adds the
  delivery's items of one material in the base unit and reads
  `inventory.stock` without a lock (`inventory.Endpoints.Held`).
- **What deliveries hold** is checked by the database only while above
  zero (`sales_order_item_in_delivery`), so with nothing in delivery the
  order's own rule (`sales_order_item_quantities`) answers as before.
- **Refusals reuse sales' codes** for the order (`sales.order_not_confirmed`,
  `sales.order_credit_blocked`, `sales.exceeds_open_quantity`,
  `sales.unknown_order_item`, `sales.item_repeated`, `sales.unit_mismatch`,
  `sales.order_item_removed`), adding `sales.nothing_to_deliver`; an order
  change's refusals (`sales.quantity_below_delivered`,
  `sales.order_item_delivered`, `sales.order_delivered`) name what is in
  outbound deliveries beside what is delivered.

**S2b1** (migrations 000407, 000408; walkthrough step 142,
`step_goods_issue`, and step 135, which now ships its stock item):

- **Movement types 601 and 602** in inventory (`movementType.delivery`):
  601 posted by `inventory.Endpoints.IssueForDelivery` alone, valued and
  posted as any issue in one WA entry against the class's
  `cost_of_goods_sold` account (`config.InventoryCostOfGoodsSold`, an
  expense; `inventory_cost_of_goods_sold:<class>`); 602 is in the
  database's rules for the Later reversal and posted by no one;
  `reverseMaterialDocument` refuses a 601 (409
  `inventory.reverse_goods_issue`).
- **The goods issue** locks the delivery, then inventory's valuations and
  stock, then the order (sales' `Endpoints.Issue`); nothing takes them in
  another order. An item picked at nothing issues nothing and costs
  nothing; a delivery with nothing picked is 409 `shipping.nothing_picked`.
  An order blocked for credit since the delivery was made is refused
  (409 `sales.order_credit_blocked`).
- **The sales delivery** a goods issue writes is T78c's record with the
  outbound delivery, the material document and each item's cost (in the
  company's currency); `SalesOrderDelivered` carries them (not required,
  ADR-0041). T78c refuses an item of a material kept in stock (422
  `sales.delivery_by_goods_issue`).
- **The delivery's cost** is each item's and their sum
  (`cost_of_goods_sold`), in the currency the delivery keeps with its goods
  issue.

**S2b2**: `sales.Orders.Segments` adds each segment's goods issues' cost
(`Segment.Cost`, `Margin`); the report's rows and totals carry
`cost_of_sales` and `margin`, `includes_cost` is true (ADR-0075 As
built), and a goods issue of an order in another currency counts in
`foreign_currency_documents`.
