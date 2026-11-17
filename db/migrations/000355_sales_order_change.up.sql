-- A sales order changed and cancelled (T78d; ADR-0067, As built T78d;
-- 03-data-model §3.29). An order that is not cancelled is changed in
-- place, as SAP's VA02 changes one: an item's quantity and price and the
-- order's requested delivery date. Each change is kept as a numbered
-- change with what it changed, before and after (SAP's change documents),
-- and moves the order's change count, which is how the order's change
-- rule tells a change from a second confirmation. An order of which
-- nothing is delivered is cancelled once, with a reason, by whom and
-- when. An item's quantity and price change only while nothing of it is
-- billed, and its quantity never falls below what is delivered
-- (sales_order_item_quantities, 000291).
--
-- Expand only (06 §6.4): nullable columns, a column with a default, a
-- status more, a table and the update grants a change needs; the
-- previous version neither reads nor writes any of them, and the orders
-- it creates and confirms stay valid.
ALTER TABLE sales.sales_order
  ADD COLUMN requested_delivery_date date,
  ADD COLUMN changes             integer NOT NULL DEFAULT 0 CONSTRAINT sales_order_changes_check CHECK (changes >= 0),
  ADD COLUMN cancellation_reason text CONSTRAINT sales_order_cancellation_reason_check
    CHECK (btrim(cancellation_reason) <> '' AND length(cancellation_reason) <= 2000),
  ADD COLUMN cancelled_at        timestamptz,
  ADD COLUMN cancelled_by        uuid,    -- identity context
  -- The principal's display name as identity holds it then, as a credit
  -- release keeps its releaser's (000300).
  ADD COLUMN cancelled_by_name   text,
  DROP CONSTRAINT sales_order_status_check,
  ADD CONSTRAINT sales_order_status_check CHECK (status IN ('draft', 'confirmed', 'cancelled')),
  -- A cancelled order names its reason, who cancelled it and when; no
  -- other order does.
  ADD CONSTRAINT sales_order_cancelled CHECK ((status = 'cancelled') = (cancellation_reason IS NOT NULL)
    AND (cancellation_reason IS NULL) = (cancelled_at IS NULL)
    AND (cancellation_reason IS NULL) = (cancelled_by IS NULL)
    AND (cancellation_reason IS NULL) = (cancelled_by_name IS NULL)),
  -- Delivery is asked for on the order's date or later.
  ADD CONSTRAINT sales_order_requested_delivery CHECK (requested_delivery_date >= order_date),
  -- A draft is not checked for credit, so it has no block; a cancelled
  -- order keeps the block it had when it was cancelled (000300 asked
  -- every order but a confirmed one to have none).
  DROP CONSTRAINT sales_order_credit_block_confirmed,
  ADD CONSTRAINT sales_order_credit_block_confirmed CHECK (status <> 'draft' OR credit_block = 'none');

-- The application's own changes of an order (T78d adds the last two):
-- a draft is confirmed, once, with or without a credit block (never
-- already released); a blocked order's block is released, once; an order
-- that is not cancelled is changed, its status kept, its change count
-- moved by one to a change kept in sales.sales_order_change, and a
-- confirmed one may be blocked for credit again; an order that is not
-- cancelled and of which nothing is delivered is cancelled. A confirmed
-- order's version still moves from a trigger of this schema alone (a
-- delivery's, a billing document's; 000292), which leaves its credit
-- block and its change count as they are. A cancelled order changes no
-- more.
CREATE OR REPLACE FUNCTION sales.check_sales_order_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'confirmed' AND NEW.status = 'confirmed' AND NEW.credit_block = OLD.credit_block AND NEW.changes = OLD.changes
      AND pg_trigger_depth() > 1 THEN
    RETURN NEW;
  END IF;
  IF OLD.status = 'confirmed' AND NEW.status = 'confirmed' AND OLD.credit_block = 'blocked' AND NEW.credit_block = 'released'
      AND NEW.changes = OLD.changes THEN
    RETURN NEW;
  END IF;
  IF OLD.status = 'draft' AND NEW.status = 'confirmed' AND NEW.credit_block <> 'released' AND NEW.changes = OLD.changes THEN
    RETURN NEW;
  END IF;
  IF OLD.status IN ('draft', 'confirmed') AND NEW.status = OLD.status AND NEW.changes = OLD.changes + 1
      AND (NEW.credit_block = OLD.credit_block OR (OLD.status = 'confirmed' AND NEW.credit_block = 'blocked'))
      AND EXISTS (SELECT FROM sales.sales_order_change c
        WHERE c.tenant_id = NEW.tenant_id AND c.sales_order_id = NEW.id AND c.change_no = NEW.changes) THEN
    RETURN NEW;
  END IF;
  IF OLD.status IN ('draft', 'confirmed') AND NEW.status = 'cancelled' AND NEW.credit_block = OLD.credit_block AND NEW.changes = OLD.changes THEN
    IF EXISTS (SELECT FROM sales.sales_order_item i
        WHERE i.tenant_id = NEW.tenant_id AND i.sales_order_id = NEW.id AND i.delivered_quantity > 0) THEN
      RAISE EXCEPTION 'sales order %: something of it is delivered, so it is not cancelled', OLD.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_cancelled_undelivered';
    END IF;
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'sales order %: only a draft changes by being confirmed, once; a block is released once; an order not cancelled is changed by a kept change or cancelled', OLD.id
    USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_confirmed_once';
END $$;
GRANT UPDATE (requested_delivery_date, net_amount, changes, cancellation_reason, cancelled_at, cancelled_by, cancelled_by_name) ON sales.sales_order TO erp_app;

-- An order item's quantity and price change only while the order is not
-- cancelled and nothing of the item is billed; what it names, its unit,
-- its account and its tax code never change (the grant leaves them out).
-- The rule runs on an update that sets the quantity or the pricing, so
-- the quantities delivered, billed and returned, which the triggers of
-- this schema move, are left to their own rules.
CREATE FUNCTION sales.check_sales_order_item_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.billed_quantity <> 0 THEN
    RAISE EXCEPTION 'sales order % item %: something of it is billed, so its quantity and price stay', OLD.sales_order_id, OLD.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_item_billed_fixed';
  END IF;
  IF NOT EXISTS (SELECT FROM sales.sales_order o
      WHERE o.tenant_id = NEW.tenant_id AND o.id = NEW.sales_order_id AND o.status IN ('draft', 'confirmed')) THEN
    RAISE EXCEPTION 'sales order % item %: the order is cancelled', OLD.sales_order_id, OLD.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_item_order_open';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION sales.check_sales_order_item_change() OWNER TO erp_owner;
CREATE TRIGGER sales_order_item_change
  BEFORE UPDATE OF quantity, unit_price, price_unit, price_source, conditions, net_amount ON sales.sales_order_item
  FOR EACH ROW EXECUTE FUNCTION sales.check_sales_order_item_change();
GRANT UPDATE (quantity, unit_price, price_unit, price_source, conditions, net_amount) ON sales.sales_order_item TO erp_app;

-- A change of a sales order (T78d): numbered from 1 within its order,
-- with the order's version after it, an optional reason, and what it
-- changed with its values before and after: the net value, the
-- requested delivery date, the credit block and, as a JSON array, each
-- item changed (its number with its quantity, unit price, price unit and
-- net value before and after). Written once and never changed.
CREATE TABLE sales.sales_order_change (
  tenant_id                       uuid NOT NULL,
  id                              uuid NOT NULL,
  sales_order_id                  uuid NOT NULL,
  change_no                       integer NOT NULL CONSTRAINT sales_order_change_no_positive CHECK (change_no > 0),
  order_version                   integer NOT NULL CONSTRAINT sales_order_change_version_positive CHECK (order_version > 1),
  reason                          text CONSTRAINT sales_order_change_reason_check CHECK (btrim(reason) <> '' AND length(reason) <= 500),
  net_amount_before               bigint NOT NULL CONSTRAINT sales_order_change_net_before_positive CHECK (net_amount_before > 0),
  net_amount_after                bigint NOT NULL CONSTRAINT sales_order_change_net_after_positive CHECK (net_amount_after > 0),
  requested_delivery_date_before  date,
  requested_delivery_date_after   date,
  credit_block_before             text NOT NULL CONSTRAINT sales_order_change_block_before_check
    CHECK (credit_block_before IN ('none', 'blocked', 'released')),
  credit_block_after              text NOT NULL CONSTRAINT sales_order_change_block_after_check
    CHECK (credit_block_after IN ('none', 'blocked', 'released')),
  items                           jsonb NOT NULL CONSTRAINT sales_order_change_items_check CHECK (jsonb_typeof(items) = 'array'),
  created_at                      timestamptz NOT NULL DEFAULT now(),
  created_by                      uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT sales_order_change_once UNIQUE (tenant_id, sales_order_id, change_no),
  CONSTRAINT sales_order_change_order_fkey FOREIGN KEY (tenant_id, sales_order_id) REFERENCES sales.sales_order (tenant_id, id)
);
CREATE TRIGGER sales_order_change_immutable BEFORE UPDATE OR DELETE ON sales.sales_order_change
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.sales_order_change'::regclass);
