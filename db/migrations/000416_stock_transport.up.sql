-- Stock transport orders within a company (S2h; ADR-0082 As built S2h;
-- 03-data-model §3.9, §3.35). A planner orders a material from a storage
-- location of one plant to one of another plant of the same company
-- (SAP's stock transport order, purchasing document type UB, from
-- memory). Its goods issue (351) takes the stock out of the supplying
-- storage location and puts it in transit at the receiving plant: a
-- movement line in_transit, which adds to inventory.transit_stock and the
-- receiving plant's valuation, not to the storage location's stock. Its
-- goods receipt (101, the order's number as its reference) takes it out
-- of transit into the receiving storage location. Stock in transit is
-- therefore neither plant's unrestricted stock, and the two movements add
-- up: what the issue puts in transit the receipt takes out. plant,
-- storage location and material ids are tenancy's and materials', the
-- material documents inventory's, by id: no foreign key leaves a schema
-- but ref's (02 §2.1). Expand only (06 §6.4): new tables, a column with a
-- default the previous version never sets, checks widened NOT VALID
-- (validated by 000417), a movement_stock that does for every movement
-- the previous version writes what it did, a count block that refuses
-- what it refused, and permissions the previous version neither reads nor
-- writes.

-- A movement line in transit: stock on its way to its storage location
-- of its plant, the receiving one.
ALTER TABLE inventory.movement ADD COLUMN in_transit boolean NOT NULL DEFAULT false;

-- Stock in transit to a storage location of a plant (SAP MARC's UMLMC,
-- here per storage location received at), in the material's base unit,
-- with the plant's company. As inventory.stock: the application writes a
-- row with no quantity before its first movement and locks it; only its
-- movements change its quantity, never below zero.
CREATE TABLE inventory.transit_stock (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  legal_entity_id      uuid NOT NULL,    -- tenancy context: the plant's company
  material_id          uuid NOT NULL,    -- materials context
  plant_id             uuid NOT NULL,    -- tenancy context: the receiving plant
  storage_location_id  uuid NOT NULL,    -- tenancy context: the receiving storage location
  quantity             numeric(20,6) NOT NULL DEFAULT 0 CONSTRAINT transit_stock_not_negative CHECK (quantity >= 0),
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT transit_stock_once UNIQUE (tenant_id, material_id, plant_id, storage_location_id)
);
SELECT platform.secure_table('inventory.transit_stock'::regclass);
GRANT UPDATE (updated_at) ON inventory.transit_stock TO erp_app;
CREATE TRIGGER transit_stock_from_movements BEFORE INSERT ON inventory.transit_stock FOR EACH ROW EXECUTE FUNCTION inventory.stock_empty();

-- A movement adds its quantity to its stock row, or to its transit stock
-- row when it is in transit, and its quantity and value to its
-- material's valuation in the plant as 000377 has it: stock in transit is
-- valued in the receiving plant.
CREATE OR REPLACE FUNCTION inventory.movement_stock() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF NEW.in_transit THEN
    UPDATE inventory.transit_stock SET quantity = quantity + NEW.quantity, updated_at = now()
    WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id AND storage_location_id = NEW.storage_location_id;
  ELSE
    UPDATE inventory.stock SET quantity = quantity + NEW.quantity, updated_at = now()
    WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id AND storage_location_id = NEW.storage_location_id;
  END IF;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'a movement adds to a stock row written before it' USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'movement_stock';
  END IF;
  UPDATE inventory.valuation SET quantity = quantity + NEW.quantity, value = value + NEW.amount, version = version + 1,
    price = CASE WHEN price_control = 'moving_average' AND quantity + NEW.quantity > 0
      THEN div(2 * (value::numeric + NEW.amount) * price_unit + (quantity + NEW.quantity), 2 * (quantity + NEW.quantity))::bigint ELSE price END
  WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'a movement is valued by its material''s valuation in the plant' USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'movement_valuation';
  END IF;
  RETURN NULL;
END $$;

-- A count blocks the stock at its storage location, not what is in
-- transit to it (S2c).
DROP TRIGGER count_posting_block ON inventory.movement;
CREATE TRIGGER count_posting_block BEFORE INSERT ON inventory.movement FOR EACH ROW WHEN (NOT NEW.in_transit)
  EXECUTE FUNCTION inventory.count_posting_block();

-- The goods issue of a stock transport order (351) names the order's
-- number, as its goods receipt (101) does; neither is reversed yet.
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '351', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302', '601', '602', '701',
                             '702')) NOT VALID,
  DROP CONSTRAINT material_document_reference,
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102', '351', '601', '602'))) NOT VALID;

-- A stock transport order: from a supplying plant to a receiving plant of
-- its company, numbered from its company's stock_transport_order range of
-- the fiscal year it was created in; open until its goods issue, in
-- transit until its goods receipt, then received. Each step names its
-- material document and day.
CREATE TABLE procurement.stock_transport_order (
  tenant_id                uuid NOT NULL,
  id                       uuid NOT NULL,
  legal_entity_id          uuid NOT NULL,    -- tenancy context
  fiscal_year              smallint NOT NULL,
  number                   text NOT NULL CONSTRAINT stock_transport_order_number_check CHECK (length(number) BETWEEN 1 AND 40),
  supplying_plant_id       uuid NOT NULL,    -- tenancy context: an org unit of kind plant
  receiving_plant_id       uuid NOT NULL,    -- tenancy context: another plant of the company
  order_date               date NOT NULL,
  status                   text NOT NULL DEFAULT 'open' CONSTRAINT stock_transport_order_status_check CHECK (status IN ('open', 'in_transit', 'received')),
  issue_document_id        uuid,             -- inventory context: the 351
  issue_document_number    text CONSTRAINT stock_transport_order_issue_document_number_check CHECK (length(issue_document_number) BETWEEN 1 AND 40),
  issue_date               date,
  receipt_document_id      uuid,             -- inventory context: the 101
  receipt_document_number  text CONSTRAINT stock_transport_order_receipt_document_number_check CHECK (length(receipt_document_number) BETWEEN 1 AND 40),
  receipt_date             date,
  version                  integer NOT NULL DEFAULT 1 CONSTRAINT stock_transport_order_version_check CHECK (version >= 1),
  created_at               timestamptz NOT NULL DEFAULT now(),
  created_by               uuid NOT NULL,
  updated_at               timestamptz NOT NULL DEFAULT now(),
  updated_by               uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT stock_transport_order_number_once UNIQUE (tenant_id, legal_entity_id, fiscal_year, number),
  CONSTRAINT stock_transport_order_plants CHECK (supplying_plant_id <> receiving_plant_id),
  -- Issued once it is in transit or received, received once received.
  CONSTRAINT stock_transport_order_issued CHECK ((status <> 'open') = (issue_document_id IS NOT NULL)
    AND (issue_document_id IS NULL) = (issue_document_number IS NULL) AND (issue_document_id IS NULL) = (issue_date IS NULL)),
  CONSTRAINT stock_transport_order_received CHECK ((status = 'received') = (receipt_document_id IS NOT NULL)
    AND (receipt_document_id IS NULL) = (receipt_document_number IS NULL) AND (receipt_document_id IS NULL) = (receipt_date IS NULL))
);
CREATE TRIGGER stock_transport_order_touch BEFORE UPDATE ON procurement.stock_transport_order FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- What an order is and the steps it took stay as they are; its status
-- moves forward one step at a time.
CREATE FUNCTION procurement.stock_transport_order_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.legal_entity_id, NEW.fiscal_year, NEW.number, NEW.supplying_plant_id, NEW.receiving_plant_id, NEW.order_date, NEW.created_at, NEW.created_by)
       IS DISTINCT FROM (OLD.legal_entity_id, OLD.fiscal_year, OLD.number, OLD.supplying_plant_id, OLD.receiving_plant_id, OLD.order_date, OLD.created_at,
         OLD.created_by)
     OR (OLD.status, NEW.status) NOT IN (('open', 'in_transit'), ('in_transit', 'received'))
     OR (OLD.issue_document_id IS NOT NULL AND (NEW.issue_document_id, NEW.issue_document_number, NEW.issue_date)
       IS DISTINCT FROM (OLD.issue_document_id, OLD.issue_document_number, OLD.issue_date)) THEN
    RAISE EXCEPTION 'stock_transport_order_fixed: order %: an order stays as created and moves from open to in transit to received', OLD.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'stock_transport_order_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.stock_transport_order_fixed() OWNER TO erp_owner;
CREATE TRIGGER stock_transport_order_fixed BEFORE UPDATE ON procurement.stock_transport_order FOR EACH ROW
  EXECUTE FUNCTION procurement.stock_transport_order_fixed();
-- The list: the tenant's orders, the latest first, in the order its pages are read.
CREATE INDEX stock_transport_order_by_created_at ON procurement.stock_transport_order (tenant_id, created_at, id);
SELECT platform.secure_table('procurement.stock_transport_order'::regclass);
REVOKE UPDATE ON procurement.stock_transport_order FROM erp_app;
GRANT UPDATE (status, issue_document_id, issue_document_number, issue_date, receipt_document_id, receipt_document_number, receipt_date, version,
  updated_by) ON procurement.stock_transport_order TO erp_app;

-- An item: a quantity of a material in one of its units (and in its base
-- unit, exactly), from a storage location of the supplying plant to one
-- of the receiving plant. Insert-only: the order moves it whole.
CREATE TABLE procurement.stock_transport_order_item (
  tenant_id                      uuid NOT NULL,
  order_id                       uuid NOT NULL,
  item_no                        smallint NOT NULL CONSTRAINT stock_transport_order_item_no_check CHECK (item_no BETWEEN 1 AND 100),
  material_id                    uuid NOT NULL,    -- materials context
  quantity                       numeric(20,6) NOT NULL CONSTRAINT stock_transport_order_item_quantity_check CHECK (quantity > 0),
  uom_code                       text NOT NULL REFERENCES ref.unit_of_measure,
  base_quantity                  numeric(20,6) NOT NULL CONSTRAINT stock_transport_order_item_base_quantity_check CHECK (base_quantity > 0),
  supplying_storage_location_id  uuid NOT NULL,    -- tenancy context: under the supplying plant
  receiving_storage_location_id  uuid NOT NULL,    -- tenancy context: under the receiving plant
  PRIMARY KEY (tenant_id, order_id, item_no),
  CONSTRAINT stock_transport_order_item_order_fkey FOREIGN KEY (tenant_id, order_id) REFERENCES procurement.stock_transport_order (tenant_id, id),
  -- A material is ordered once per order.
  CONSTRAINT stock_transport_order_item_material_once UNIQUE (tenant_id, order_id, material_id)
);
CREATE TRIGGER stock_transport_order_item_insert_only BEFORE UPDATE OR DELETE ON procurement.stock_transport_order_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('procurement.stock_transport_order_item'::regclass);

-- Creating and reading stock transport orders; posting their goods
-- issues and receipts.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('procurement.stock_transport.read', 'View stock transport orders between plants', false),
  ('procurement.stock_transport.write', 'Create stock transport orders between plants', false),
  ('procurement.stock_transport.post', 'Post the goods issue and goods receipt of stock transport orders, which move their stock', false);
