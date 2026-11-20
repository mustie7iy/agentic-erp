-- Available-to-promise on the sales order (S2j; ADR-0082 As built S2j).
-- A sales order may name the plant it delivers from (tenancy context, a
-- plant of its company), entered with it and never changed: the
-- application's column grants on sales.sales_order (000291, 000355) do
-- not include it. Confirming an order that names one checks each item of
-- a material kept in stock against what the plant has available, its
-- stock less what other confirmed orders promised of it and have not yet
-- delivered, and records the item's confirmed quantity, in the item's
-- unit: what its outbound deliveries may take. An item's confirmed
-- quantity is set only while its order is confirmed and names a plant,
-- only on an item of a material, never above the item's quantity, never
-- cleared once set, and never below what is delivered and in delivery of
-- it. Expand only (06 §6.4): nullable columns, null on every existing
-- order and item, which the previous version neither reads nor writes.
ALTER TABLE sales.sales_order
  ADD COLUMN plant_id uuid;    -- tenancy context
ALTER TABLE sales.sales_order_item
  ADD COLUMN confirmed_quantity numeric(20,6),
  ADD CONSTRAINT sales_order_item_confirmed CHECK (confirmed_quantity IS NULL
    OR (confirmed_quantity >= 0 AND delivered_quantity + in_delivery_quantity <= confirmed_quantity));

CREATE FUNCTION sales.check_sales_order_item_confirmed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.confirmed_quantity IS NOT NULL THEN
      RAISE EXCEPTION 'sales order % item %: an item is entered unconfirmed; its confirmation sets what is confirmed', NEW.sales_order_id, NEW.item_no
        USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_item_confirmed_by_check';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.confirmed_quantity IS NOT DISTINCT FROM OLD.confirmed_quantity THEN
    RETURN NEW;
  END IF;
  IF NEW.confirmed_quantity IS NULL OR NEW.material_id IS NULL OR NEW.confirmed_quantity > NEW.quantity
      OR NOT EXISTS (SELECT FROM sales.sales_order o
        WHERE o.tenant_id = NEW.tenant_id AND o.id = NEW.sales_order_id AND o.status = 'confirmed' AND o.plant_id IS NOT NULL) THEN
    RAISE EXCEPTION 'sales order % item %: a confirmed quantity is set on an item of a material of a confirmed order with a plant, at most its quantity, and never cleared',
        NEW.sales_order_id, NEW.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_item_confirmed_by_check';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION sales.check_sales_order_item_confirmed() OWNER TO erp_owner;
CREATE TRIGGER sales_order_item_confirmed_by_check BEFORE INSERT OR UPDATE OF confirmed_quantity ON sales.sales_order_item
  FOR EACH ROW EXECUTE FUNCTION sales.check_sales_order_item_confirmed();
GRANT UPDATE (confirmed_quantity) ON sales.sales_order_item TO erp_app;

-- What confirmed orders promised of a material at a plant and have not
-- delivered: the availability check's demand.
CREATE INDEX sales_order_by_plant ON sales.sales_order (tenant_id, plant_id) WHERE status = 'confirmed' AND plant_id IS NOT NULL;
CREATE INDEX sales_order_item_promised ON sales.sales_order_item (tenant_id, material_id)
  WHERE confirmed_quantity > delivered_quantity AND removed_in_change IS NULL;
