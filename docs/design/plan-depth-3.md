# Depth plan 3: third variant slices (18 November 2026)

**Status:** plan (product owner, 16–17 November 2026: breadth is built;
the variants go on, pilot-relevant first). It follows `plan-depth-1.md`
and `plan-depth-2.md`, whose rules it does not repeat: each lane splits
two named variants of its placeholder into slices in the form of
`plan-breadth-week3.md` (Adds, Acceptance, Later) and builds them.
`docs/tracker.json` stays the only plan (07 §7.5); each leaf's `r` names
its section below. Each lane writes its own `## <lane>` section.

## price

Placeholder: "later: price determination variants"
(`plan-breadth-week2.md` §T80a, Later; ADR-0067 §10, §11). Two variants
are split here; the rest stay in the placeholder. SAP S/4HANA's condition
technique is the reference (from memory, not a primary source: condition
records with scales, scale basis quantity or value, scale type "from";
the customer price group of the customer's sales area data, an access
sequence that reads the most specific key first); nothing below depends
on a fact of SAP's beyond that shape.

### T80e Scales on a condition record, by quantity or by value

**Capability.** A pricing analyst records a price or a discount whose
rate falls or rises with what is bought: a price per each that is lower
from 1000 pieces and lower again from 5000, or a customer's discount that
is larger from an order value of 1000.00; a quotation or order item priced
from the record takes the rate of the scale level its quantity or value
reaches, and shows the level.

- **Reuses:** T80a's record and its determination (`validRecord`, the
  customer's before the general one, the latest `valid_from`), T80b's
  procedure (`conditioned`), `money.Scale`, `money.PositiveQuantity`,
  `currency.ParseAmount`, the `ref` units and currencies.
- **Adds:** on `POST /v1/sales-prices` `scale_basis` (`quantity` or
  `value`) and `scale[]`, at most 20 levels, each `from_quantity` (a
  `Quantity`) or `from_amount` (a `Money`) by the basis, more than zero and
  strictly rising, with its rate as the record's kind gives one:
  `unit_price` (per the record's `price_unit`, in its currency) for a
  price, `percent` for a discount. The record's own rate applies below
  the first level (level 0); an item takes the rate of the highest level
  whose `from` its basis reaches ("from" scale). A price's scale is by
  quantity, in the record's unit; a discount's by quantity in the unit
  its levels state, which an item in another unit does not take, or by
  value in the currency they state, the value before the discount, which
  a document in another currency does not take (nothing is converted, as
  §10 converts no price). `SalesPrice` shows `scale_basis` and `scale[]`
  with each level's number; a condition on an item shows its `scale`
  (basis, the level reached, its `from`). `SalesPriceChanged` carries the
  basis and the levels. Migration: `scale_basis`, `scale_uom_code`,
  `scale_currency` on `sales.price`; `sales.price_scale` (insert-only).
- **Walkthrough:** `step_price_scales`: Paula records BOLT-M10 at 0.80 CAD
  each, 0.70 from 1000 and 0.60 from 5000, and Northwind's discount on
  it, one percent, two from an order value of 1000.00 CAD and three from
  5000.00; Sally's quotation of 6000 bolts takes 0.60 (level 2), 3600.00,
  and two percent off it (level 1), 72.00, net 3528.00; one of 500 takes
  0.80 (level 0) and one percent; a scale whose levels do not rise, and a
  price with a value scale, are refused naming the level or the basis.
- **Acceptance:** the level reached is exact (a quantity compared as a
  decimal, a value in minor units); a scaled record's rate below the first
  level is its own; a scale with no basis, a basis with no levels, a level
  with the other basis's `from`, a `from` of zero or less or not above
  the one before, a level's rate of the other kind, a quantity level in
  another unit than the price's, levels in two units or currencies are
  each refused naming the field (422 `sales.invalid_scale`); a level's
  price not more than zero (`sales.amount_not_positive`), in another
  currency than the record's (`sales.currency_mismatch`), a level's
  percent out of range (`sales.invalid_percent`), an unknown unit or
  currency of a discount's scale (`sales.unknown_unit`,
  `sales.unknown_currency`), each naming the level's field; a price with a
  value scale is refused (`sales.invalid_scale`, `scale_basis`); a record
  without a scale prices as before; an item priced once keeps its rate
  and level, as §10 fixes a price.

### T80f Prices per customer price group

**Capability.** A configuration administrator defines customer price
groups (wholesale, retail); a pricing analyst puts a customer in one in
its sales data for a company and records prices and discounts for the
group; a quotation or order item for the customer takes the customer's
own record first, then its group's, then the general one, and shows which
step of the access found it.

- **Reuses:** T80b's configuration section `pricing`, its validation and
  transitions (`internal/config/pricing.go`, `diff.go`), T80a's records
  and determination, `partner.Postings.PostingBlock` (the customer, as a
  price record's customer is checked), `etag`, audit, the outbox, cursor
  paging.
- **Adds:** `pricing.price_groups[]` (a code and a name; never removed
  once activated; tier 2, a name tier 1); table `sales.customer_sales_data`
  (a customer's sales data in a company: its `price_group`, versioned);
  `POST /v1/customer-sales-data`, `GET /v1/customer-sales-data`,
  `GET /v1/customer-sales-data/{id}`, `PATCH /v1/customer-sales-data/{id}`
  (`If-Match`); event `CustomerSalesDataChanged`; `price_group` on a price
  record (`sales.price.price_group`, not with `customer_id`) and a filter
  of `GET /v1/sales-prices`; the access sequence customer → price group →
  general for prices and discounts alike; `access` (`customer`,
  `price_group`, `general`) and `price_group` on an item's conditions.
  Permissions `sales.price.read` and `.write` (no new ones: the sales data
  holds the pricing group alone, which only decides prices).
- **Walkthrough:** `step_price_groups`: Ada adds the price groups 01
  Wholesale and 02 Retail through a governed change; Paula puts Contoso in
  01 and records 01's price for BOLT-M10, 0.75; Sally's quotation to
  Contoso takes it (`access` `price_group`), Northwind's the general one;
  Paula records Contoso's own price, which then wins; she moves Contoso to
  02, which has no record, and the next item takes the general price; a
  second sales data for Contoso, an unknown group, a record with both a
  customer and a group and a stale `If-Match` are refused.
- **Acceptance:** the customer's record before its group's before the
  general one, then the latest `valid_from`, then the one recorded last;
  a customer without sales data, or without a group, has the general
  record; a group the configuration does not have is refused naming the
  field (422 `sales.unknown_price_group`); a record with a customer and a
  group (422 `sales.customer_or_group`, `price_group`); a second sales
  data of the customer in the company (409
  `sales.customer_sales_data_exists`); a change that changes nothing (422
  `sales.nothing_to_change`); a stale `If-Match` 412; two changes racing
  end with one; the sales data's customer is named by id, number and
  name, its company by code and name; items priced before keep their
  price when the group changes.

**Later (still in the placeholder):** prices per price list type or sales
organization; surcharges and freight; minimum prices and manual price
approval; currency conversion of a price; repricing a document; mass
changes and price uploads; prices taken over from SAP. Configurable access
sequences stay in "later: pricing configuration variants".
