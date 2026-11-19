-- Outbound deliveries and picking (S2a; ADR-0083 §1-§5; 03-data-model
-- §3.36): the shipping context's schema with its outbox and inbox, an
-- outbound delivery (SAP LIKP) of one confirmed sales order from one
-- plant and storage location, numbered from its company's
-- outbound_delivery range, and its items (LIPS), each an order item's
-- quantity of a material kept in stock with what is picked of it. The
-- sales order, the customer and the material are the sales, partner and
-- materials contexts', plants, storage locations and legal_entity_id the
-- tenancy context's, by id: no foreign key leaves the schema but ref's
-- (02 §2.1). A delivery is never deleted and changes only while it is
-- created: picking sets its items' picked quantities, the goods issue
-- (S2b1) its status, once.
--
-- In the sales context, what deliveries hold of each order item
-- (ADR-0083 §4): sales.delivery_assignment, insert-only signed rows, adds
-- to sales_order_item.in_delivery_quantity, which with what is delivered
-- never exceeds what was ordered, so no order change takes an item below
-- it, removes it or cancels its order while a delivery holds it.
--
-- Expand only (06 §6.4): a new schema, a new table, a column with a
-- default the previous version neither reads nor writes (it stays 0
-- there, so its checks always hold), a cancel rule its orders always pass
-- and three permissions.
CREATE SCHEMA shipping;
GRANT USAGE ON SCHEMA shipping TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('shipping');
SELECT platform.secure_table('shipping.outbox'::regclass);
SELECT platform.secure_table('shipping.inbox'::regclass);
GRANT UPDATE (published_at) ON shipping.outbox TO erp_app;
GRANT USAGE ON SCHEMA shipping TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON shipping.outbox TO erp_relay;

-- An outbound delivery: the company of its plant, numbered in that
-- company's range of its creation day's fiscal year (ADR-0004); the sales
-- order and its customer by id and number, which never change. Once its
-- goods issue posts it names the day and the material document.
CREATE TABLE shipping.outbound_delivery (
  tenant_id                 uuid NOT NULL,
  id                        uuid NOT NULL,
  legal_entity_id           uuid NOT NULL,    -- tenancy context: the company of the order and the plant
  fiscal_year               smallint NOT NULL,
  number                    text NOT NULL CONSTRAINT outbound_delivery_number_check CHECK (length(number) BETWEEN 1 AND 40),
  sales_order_id            uuid NOT NULL,    -- sales context
  sales_order_number        text NOT NULL CONSTRAINT outbound_delivery_sales_order_number_check CHECK (length(sales_order_number) BETWEEN 1 AND 40),
  customer_id               uuid NOT NULL,    -- partner context
  customer_number           text NOT NULL CONSTRAINT outbound_delivery_customer_number_check CHECK (length(customer_number) BETWEEN 1 AND 40),
  plant_id                  uuid NOT NULL,    -- tenancy context: an org unit of kind plant
  storage_location_id       uuid NOT NULL,    -- tenancy context: an org unit of kind storage_location under the plant
  status                    text NOT NULL DEFAULT 'created' CONSTRAINT outbound_delivery_status_check CHECK (status IN ('created', 'goods_issued')),
  goods_issue_date          date,
  material_document_id      uuid,             -- inventory context: the goods issue's 601
  material_document_number  text CONSTRAINT outbound_delivery_material_document_number_check CHECK (length(material_document_number) BETWEEN 1 AND 40),
  currency                  text CONSTRAINT outbound_delivery_currency_fkey REFERENCES ref.currency,  -- its items' cost's, the company's
  version                   integer NOT NULL DEFAULT 1 CONSTRAINT outbound_delivery_version_check CHECK (version >= 1),
  created_at                timestamptz NOT NULL DEFAULT now(),
  created_by                uuid NOT NULL,
  updated_at                timestamptz NOT NULL DEFAULT now(),
  updated_by                uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT outbound_delivery_number_once UNIQUE (tenant_id, legal_entity_id, fiscal_year, number),
  -- A delivery whose goods issue posted names its day, material document and cost's currency; one created, none.
  CONSTRAINT outbound_delivery_goods_issue CHECK ((status = 'goods_issued') = (goods_issue_date IS NOT NULL)
    AND (status = 'goods_issued') = (material_document_id IS NOT NULL) AND (status = 'goods_issued') = (material_document_number IS NOT NULL)
    AND (status = 'goods_issued') = (currency IS NOT NULL))
);
CREATE TRIGGER outbound_delivery_touch BEFORE UPDATE ON shipping.outbound_delivery FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- A delivery changes only while created, and then only by its goods issue
-- (the grant leaves its other columns out); one whose goods issue posted
-- changes no more.
CREATE FUNCTION shipping.outbound_delivery_open() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'created' THEN
    RAISE EXCEPTION 'outbound delivery %: its goods issue is posted, so it changes no more', OLD.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'outbound_delivery_issued_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION shipping.outbound_delivery_open() OWNER TO erp_owner;
CREATE TRIGGER outbound_delivery_issued_fixed BEFORE UPDATE ON shipping.outbound_delivery
  FOR EACH ROW EXECUTE FUNCTION shipping.outbound_delivery_open();
CREATE TRIGGER outbound_delivery_no_delete BEFORE DELETE ON shipping.outbound_delivery
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
-- The list: the tenant's deliveries, the latest first, in the order its pages are read.
CREATE INDEX outbound_delivery_by_created_at ON shipping.outbound_delivery (tenant_id, created_at, id);
-- An order's deliveries, for the list's order filter.
CREATE INDEX outbound_delivery_by_order ON shipping.outbound_delivery (tenant_id, sales_order_id, created_at, id);
SELECT platform.secure_table('shipping.outbound_delivery'::regclass);
GRANT UPDATE (status, goods_issue_date, material_document_id, material_document_number, currency, version, updated_by)
  ON shipping.outbound_delivery TO erp_app;

-- An item: a quantity of an order item, in its unit, of a material kept
-- in stock, the order item once per delivery; picked up to its quantity.
-- Once the goods issue posts, its cost: what the quantity issued, the
-- picked one, took from the stock's value, in the company's currency.
CREATE TABLE shipping.outbound_delivery_item (
  tenant_id             uuid NOT NULL,
  outbound_delivery_id  uuid NOT NULL,
  item_no               integer NOT NULL CONSTRAINT outbound_delivery_item_no_check CHECK (item_no > 0),
  order_item_no         integer NOT NULL CONSTRAINT outbound_delivery_item_order_item_no_check CHECK (order_item_no > 0),
  material_id           uuid NOT NULL,    -- materials context
  description           text NOT NULL CONSTRAINT outbound_delivery_item_description_check CHECK (length(description) BETWEEN 1 AND 200),
  quantity              numeric(20,6) NOT NULL CONSTRAINT outbound_delivery_item_quantity_check CHECK (quantity > 0),
  uom_code              text NOT NULL REFERENCES ref.unit_of_measure,
  picked_quantity       numeric(20,6) NOT NULL DEFAULT 0
                          CONSTRAINT outbound_delivery_item_picked_check CHECK (picked_quantity >= 0 AND picked_quantity <= quantity),
  cost_amount           bigint CONSTRAINT outbound_delivery_item_cost_check CHECK (cost_amount >= 0),
  PRIMARY KEY (tenant_id, outbound_delivery_id, item_no),
  CONSTRAINT outbound_delivery_item_order_item_once UNIQUE (tenant_id, outbound_delivery_id, order_item_no),
  CONSTRAINT outbound_delivery_item_delivery_fkey FOREIGN KEY (tenant_id, outbound_delivery_id) REFERENCES shipping.outbound_delivery (tenant_id, id)
);
-- An item changes only while its delivery is created (picking, and the
-- goods issue's cost); never once its goods issue posted.
CREATE FUNCTION shipping.outbound_delivery_item_open() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT FROM shipping.outbound_delivery d
      WHERE d.tenant_id = OLD.tenant_id AND d.id = OLD.outbound_delivery_id AND d.status = 'created') THEN
    RAISE EXCEPTION 'outbound delivery %: its goods issue is posted, so its items change no more', OLD.outbound_delivery_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'outbound_delivery_item_issued_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION shipping.outbound_delivery_item_open() OWNER TO erp_owner;
CREATE TRIGGER outbound_delivery_item_issued_fixed BEFORE UPDATE ON shipping.outbound_delivery_item
  FOR EACH ROW EXECUTE FUNCTION shipping.outbound_delivery_item_open();
CREATE TRIGGER outbound_delivery_item_no_delete BEFORE DELETE ON shipping.outbound_delivery_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('shipping.outbound_delivery_item'::regclass);
GRANT UPDATE (picked_quantity, cost_amount) ON shipping.outbound_delivery_item TO erp_app;

-- What outbound deliveries hold of an order item (ADR-0083 §4): never
-- below zero, and with what is delivered never above what was ordered; a
-- removed item holds none.
ALTER TABLE sales.sales_order_item
  ADD COLUMN in_delivery_quantity numeric(20,6) NOT NULL DEFAULT 0,
  -- (with nothing in delivery, sales_order_item_quantities alone rules)
  ADD CONSTRAINT sales_order_item_in_delivery CHECK (in_delivery_quantity >= 0
    AND (in_delivery_quantity = 0 OR delivered_quantity + in_delivery_quantity <= quantity)),
  ADD CONSTRAINT sales_order_item_removed_unshipped CHECK (removed_in_change IS NULL OR in_delivery_quantity = 0);

-- A quantity of an order item an outbound delivery takes (positive, at
-- its creation) or gives back (negative, at its goods issue), by the
-- delivery's id and number. Insert-only; in_delivery_quantity is the sum
-- of an item's rows.
CREATE TABLE sales.delivery_assignment (
  tenant_id                 uuid NOT NULL,
  id                        uuid NOT NULL,
  sales_order_id            uuid NOT NULL,
  order_item_no             integer NOT NULL,
  outbound_delivery_id      uuid NOT NULL,    -- shipping context
  outbound_delivery_number  text NOT NULL CONSTRAINT delivery_assignment_number_check CHECK (length(outbound_delivery_number) BETWEEN 1 AND 40),
  quantity                  numeric(20,6) NOT NULL CONSTRAINT delivery_assignment_quantity_check CHECK (quantity <> 0),
  created_at                timestamptz NOT NULL DEFAULT now(),
  created_by                uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT delivery_assignment_order_item_fkey FOREIGN KEY (tenant_id, sales_order_id, order_item_no)
    REFERENCES sales.sales_order_item (tenant_id, sales_order_id, item_no)
);
-- An order item's assignments, for its quantity in delivery.
CREATE INDEX delivery_assignment_by_order_item ON sales.delivery_assignment (tenant_id, sales_order_id, order_item_no);
CREATE TRIGGER delivery_assignment_immutable BEFORE UPDATE OR DELETE ON sales.delivery_assignment
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.delivery_assignment'::regclass);

-- An assignment adds its quantity to the order item's in delivery and
-- moves the order's version, as a delivery does (000292): only a
-- confirmed order takes one. Security definer: the application updates
-- neither a confirmed order nor its items' quantities.
CREATE FUNCTION sales.record_delivery_assignment() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE sales.sales_order SET version = version + 1, updated_by = NEW.created_by
  WHERE tenant_id = NEW.tenant_id AND id = NEW.sales_order_id AND status = 'confirmed';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'delivery assignment %: its sales order is not confirmed', NEW.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'delivery_assignment_order_confirmed';
  END IF;
  UPDATE sales.sales_order_item SET in_delivery_quantity = in_delivery_quantity + NEW.quantity
  WHERE tenant_id = NEW.tenant_id AND sales_order_id = NEW.sales_order_id AND item_no = NEW.order_item_no;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.record_delivery_assignment() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION sales.record_delivery_assignment() FROM PUBLIC;
CREATE TRIGGER delivery_assignment_recorded AFTER INSERT ON sales.delivery_assignment
  FOR EACH ROW EXECUTE FUNCTION sales.record_delivery_assignment();

-- An order an outbound delivery holds is not cancelled (000355's rule
-- otherwise unchanged).
CREATE FUNCTION sales.check_sales_order_unshipped() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT FROM sales.sales_order_item i
      WHERE i.tenant_id = NEW.tenant_id AND i.sales_order_id = NEW.id AND i.in_delivery_quantity > 0) THEN
    RAISE EXCEPTION 'sales order %: an outbound delivery holds some of it, so it is not cancelled', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_cancelled_unshipped';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION sales.check_sales_order_unshipped() OWNER TO erp_owner;
CREATE TRIGGER sales_order_cancelled_unshipped BEFORE UPDATE OF status ON sales.sales_order
  FOR EACH ROW WHEN (NEW.status = 'cancelled' AND OLD.status <> 'cancelled')
  EXECUTE FUNCTION sales.check_sales_order_unshipped();

-- Creating outbound deliveries, recording picking and reading them
-- (ADR-0083 §5).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('shipping.delivery.read', 'View outbound deliveries', false),
  ('shipping.delivery.write', 'Create outbound deliveries and record their picking', false);
