-- Stock by location and goods movements (S1c; ADR-0082 §1, §7 and As
-- built; 03-data-model §3.35): the inventory context's schema with its
-- outbox and inbox, a material document (SAP MKPF) numbered from its
-- company's material_document range, its movements (MSEG: one row per
-- stock effect, a transfer's issuing and receiving lines apart) and the
-- stock per material, plant and storage location (MARD's quantity).
-- material_id is the materials context's, plants, storage locations and
-- legal_entity_id the tenancy context's and cost_center_id the ledger's,
-- by id: no foreign key leaves the schema but ref's (02 §2.1). Documents
-- and movements are insert-only; a mistaken document is reversed by
-- another. The stock is the sum of its movements and never below zero:
-- inventory.movement_stock alone changes it. Expand only: a new schema
-- and two permissions the previous version neither reads nor writes
-- (06 §6.4).
CREATE SCHEMA inventory;
GRANT USAGE ON SCHEMA inventory TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('inventory');
SELECT platform.secure_table('inventory.outbox'::regclass);
SELECT platform.secure_table('inventory.inbox'::regclass);
GRANT UPDATE (published_at) ON inventory.outbox TO erp_app;
GRANT USAGE ON SCHEMA inventory TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON inventory.outbox TO erp_relay;

-- A material document: one movement type, posted on its company's date
-- and numbered in that company's range of the posting date's fiscal year
-- (ADR-0004). A reversal names the document it reverses, once, with the
-- reversal movement type of the original's (material_document_reversal).
CREATE TABLE inventory.material_document (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,    -- tenancy context: the company of its plants
  fiscal_year      smallint NOT NULL,
  number           text NOT NULL CONSTRAINT material_document_number_check CHECK (length(number) BETWEEN 1 AND 40),
  movement_type    text NOT NULL CONSTRAINT material_document_movement_type_check
                     CHECK (movement_type IN ('501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302')),
  posting_date     date NOT NULL,
  header_text      text CONSTRAINT material_document_header_text_check CHECK (length(header_text) BETWEEN 1 AND 200 AND length(trim(header_text)) > 0),
  reversal_of_id   uuid,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT material_document_number_once UNIQUE (tenant_id, legal_entity_id, fiscal_year, number),
  CONSTRAINT material_document_reversed_once UNIQUE (tenant_id, reversal_of_id),
  CONSTRAINT material_document_reversal_of_fkey FOREIGN KEY (tenant_id, reversal_of_id) REFERENCES inventory.material_document (tenant_id, id),
  -- A reversal movement type names the document it reverses; another
  -- names none.
  CONSTRAINT material_document_reversal_check
    CHECK ((reversal_of_id IS NOT NULL) = (movement_type IN ('502', '562', '202', '552', '312', '302')))
);
CREATE TRIGGER material_document_insert_only BEFORE UPDATE OR DELETE ON inventory.material_document
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
-- A reversal is of its original's company, with its movement type's
-- reversal (501 by 502, 561 by 562, 201 by 202, 551 by 552, 311 by 312,
-- 301 by 302).
CREATE FUNCTION inventory.material_document_reversal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.reversal_of_id IS NOT NULL AND NOT EXISTS (SELECT FROM inventory.material_document o
      WHERE o.tenant_id = NEW.tenant_id AND o.id = NEW.reversal_of_id AND o.legal_entity_id = NEW.legal_entity_id
        AND NEW.movement_type = (o.movement_type::integer + 1)::text AND o.reversal_of_id IS NULL) THEN
    RAISE EXCEPTION 'a reversal is of its original''s company, with its movement type''s reversal'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'material_document_reversal';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION inventory.material_document_reversal() OWNER TO erp_owner;
CREATE TRIGGER material_document_reversal BEFORE INSERT ON inventory.material_document
  FOR EACH ROW EXECUTE FUNCTION inventory.material_document_reversal();
-- The list: the tenant's documents, the latest first, in the order its pages are read.
CREATE INDEX material_document_by_created_at ON inventory.material_document (tenant_id, created_at, id);
SELECT platform.secure_table('inventory.material_document'::regclass);

-- A movement: one stock effect of a document's item in the material's
-- base unit, signed (a receipt positive, an issue negative), with the
-- quantity and unit the item was entered in; an issue to a cost center
-- or a scrapping names its cost center.
CREATE TABLE inventory.movement (
  tenant_id             uuid NOT NULL,
  material_document_id  uuid NOT NULL,
  line_no               smallint NOT NULL CONSTRAINT movement_line_no_check CHECK (line_no BETWEEN 1 AND 200),
  item_no               smallint NOT NULL CONSTRAINT movement_item_no_check CHECK (item_no BETWEEN 1 AND 100),
  material_id           uuid NOT NULL,    -- materials context
  plant_id              uuid NOT NULL,    -- tenancy context: an org unit of kind plant
  storage_location_id   uuid NOT NULL,    -- tenancy context: an org unit of kind storage_location under the plant
  quantity              numeric(20,6) NOT NULL CONSTRAINT movement_quantity_check CHECK (quantity <> 0),
  entry_quantity        numeric(20,6) NOT NULL CONSTRAINT movement_entry_quantity_check CHECK (entry_quantity > 0),
  entry_uom_code        text NOT NULL REFERENCES ref.unit_of_measure,
  cost_center_id        uuid,             -- ledger context
  created_at            timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, material_document_id, line_no),
  CONSTRAINT movement_document_fkey FOREIGN KEY (tenant_id, material_document_id) REFERENCES inventory.material_document (tenant_id, id)
);
CREATE TRIGGER movement_insert_only BEFORE UPDATE OR DELETE ON inventory.movement
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
-- A stock's movements, for its quantity as at a date and a material's documents.
CREATE INDEX movement_by_stock ON inventory.movement (tenant_id, material_id, plant_id, storage_location_id);
SELECT platform.secure_table('inventory.movement'::regclass);

-- The stock of a material at a storage location of a plant (MARD's
-- unrestricted quantity) in its base unit, with the plant's company, kept
-- so a read shows only the companies the caller may act in. The
-- application writes a row with no quantity before the first movement of
-- it (and locks it); its quantity is only ever the sum of its movements.
CREATE TABLE inventory.stock (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  legal_entity_id      uuid NOT NULL,    -- tenancy context: the plant's company
  material_id          uuid NOT NULL,    -- materials context
  plant_id             uuid NOT NULL,    -- tenancy context
  storage_location_id  uuid NOT NULL,    -- tenancy context
  quantity             numeric(20,6) NOT NULL DEFAULT 0 CONSTRAINT stock_not_negative CHECK (quantity >= 0),
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT stock_once UNIQUE (tenant_id, material_id, plant_id, storage_location_id)
);
-- The list: the tenant's stock in the order it was first moved.
CREATE INDEX stock_by_created_at ON inventory.stock (tenant_id, created_at, id);
-- A plant's stock, for the list's plant filter.
CREATE INDEX stock_by_plant ON inventory.stock (tenant_id, plant_id, material_id);
SELECT platform.secure_table('inventory.stock'::regclass);
-- The application locks a stock row (FOR UPDATE needs a column's UPDATE
-- privilege) but never sets its quantity.
GRANT UPDATE (updated_at) ON inventory.stock TO erp_app;

-- A new stock row holds nothing; a movement adds its quantity to its
-- stock's, which must exist (the application writes it first) and never
-- falls below zero (stock_not_negative). Security definer: the
-- application may not update a stock's quantity, so nothing but a
-- movement changes it.
CREATE FUNCTION inventory.stock_empty() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.quantity <> 0 THEN
    RAISE EXCEPTION 'a stock row starts with no quantity; movements add to it' USING ERRCODE = 'check_violation', CONSTRAINT = 'stock_from_movements';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION inventory.stock_empty() OWNER TO erp_owner;
CREATE TRIGGER stock_from_movements BEFORE INSERT ON inventory.stock FOR EACH ROW EXECUTE FUNCTION inventory.stock_empty();
CREATE FUNCTION inventory.movement_stock() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE inventory.stock SET quantity = quantity + NEW.quantity, updated_at = now()
  WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id AND storage_location_id = NEW.storage_location_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'a movement adds to a stock row written before it' USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'movement_stock';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION inventory.movement_stock() OWNER TO erp_owner;
CREATE TRIGGER movement_stock AFTER INSERT ON inventory.movement FOR EACH ROW EXECUTE FUNCTION inventory.movement_stock();

-- Posting goods movements and reading stock and material documents
-- (ADR-0082 §7).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('inventory.movement.post', 'Post and reverse goods movements', false),
  ('inventory.stock.read', 'View stock by location and material documents', false);
