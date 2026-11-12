-- The credit block of a sales order (T77b; ADR-0066 and ADR-0067, As
-- built T77b; 03-data-model §3.29). When an order is confirmed its gross
-- value, in the company's currency, is checked against the customer's
-- credit limit less its exposure: an order that does not fit is
-- confirmed with a credit block, takes no delivery while it is blocked,
-- and is released once by a person holding sales.order.credit_release.
-- `credit_block` is `none` (within the limit, or the customer has none),
-- `blocked` or `released`, the last with who released it, their name as
-- it was then, and when.
-- Expand only (06 §6.4): four columns the previous version neither
-- reads nor writes, the first with a default, so an order it confirms is
-- confirmed without a block as before; a permission.
ALTER TABLE sales.sales_order
  ADD COLUMN credit_block text NOT NULL DEFAULT 'none'
    CONSTRAINT sales_order_credit_block_check CHECK (credit_block IN ('none', 'blocked', 'released')),
  ADD COLUMN credit_released_by uuid,          -- identity context
  -- The principal's display name as identity holds it then, whatever it
  -- is: identity's rule for a name is the only one, so a release is never
  -- refused for the name of who made it.
  ADD COLUMN credit_released_by_name text,
  ADD COLUMN credit_released_at timestamptz,
  -- A released block names who released it and when; nothing else does.
  ADD CONSTRAINT sales_order_credit_release CHECK ((credit_block = 'released') = (credit_released_by IS NOT NULL)
    AND (credit_released_by IS NULL) = (credit_released_at IS NULL)
    AND (credit_released_by IS NULL) = (credit_released_by_name IS NULL)),
  -- A draft is not checked, so it has no block.
  ADD CONSTRAINT sales_order_credit_block_confirmed CHECK (status = 'confirmed' OR credit_block = 'none');

-- The application's own changes of an order: a draft is confirmed, once,
-- with or without a credit block (never already released), and a blocked
-- order's block is released, once. A confirmed order's version still
-- moves from a trigger of this schema alone (a delivery's, a billing
-- document's; 000292), which leaves its credit block as it is.
CREATE OR REPLACE FUNCTION sales.check_sales_order_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'confirmed' AND NEW.status = 'confirmed' AND NEW.credit_block = OLD.credit_block AND pg_trigger_depth() > 1 THEN
    RETURN NEW;
  END IF;
  IF OLD.status = 'confirmed' AND NEW.status = 'confirmed' AND OLD.credit_block = 'blocked' AND NEW.credit_block = 'released' THEN
    RETURN NEW;
  END IF;
  IF OLD.status <> 'draft' OR NEW.status <> 'confirmed' OR NEW.credit_block = 'released' THEN
    RAISE EXCEPTION 'sales order %: only a draft changes, and only by being confirmed; a credit block is released once', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_confirmed_once';
  END IF;
  RETURN NEW;
END $$;
GRANT UPDATE (credit_block, credit_released_by, credit_released_by_name, credit_released_at) ON sales.sales_order TO erp_app;

-- An order blocked for credit takes no delivery (and so nothing of it is
-- billed, since billing bills what is delivered): the delivery's trigger
-- (000292) finds a confirmed order that is not blocked.
CREATE OR REPLACE FUNCTION sales.record_delivery() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE sales.sales_order SET version = version + 1, updated_by = NEW.created_by
  WHERE tenant_id = NEW.tenant_id AND id = NEW.sales_order_id AND status = 'confirmed' AND credit_block <> 'blocked';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'delivery %: its sales order is not confirmed, or is blocked for credit', NEW.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'delivery_order_confirmed';
  END IF;
  RETURN NULL;
END $$;

-- Releasing a credit block (ADR-0066, As built T77b): a credit
-- manager's, apart from confirming the order.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('sales.order.credit_release', 'Release the credit block of a sales order', false);
