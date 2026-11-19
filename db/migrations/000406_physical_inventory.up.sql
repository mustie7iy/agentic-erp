-- Physical inventory count documents (S2c; ADR-0082 As built S2c;
-- 03-data-model §3.35): a count document (SAP IKPF) of one plant's
-- storage location, numbered from its company's physical_inventory range
-- of its date's fiscal year, and its items (ISEG), one per material, each
-- with the book quantity frozen as the document was created and the
-- quantity counted, entered once. While an item is open its material
-- takes no goods movement at that storage location (the posting block,
-- count_posting_block), and a material has one open count per storage
-- location (count_item_one_open). The statuses S2d moves a document
-- through are here from the start; S2d adds what posting writes.
-- material_id is the materials context's, plants, storage locations and
-- legal_entity_id the tenancy context's, by id (02 §2.1). Expand only: new
-- tables, a check on movements that refuses nothing while no count is
-- open, and a permission the previous version neither reads nor writes
-- (06 §6.4).
CREATE TABLE inventory.count_document (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  legal_entity_id      uuid NOT NULL,    -- tenancy context: the plant's company
  fiscal_year          smallint NOT NULL,
  number               text NOT NULL CONSTRAINT count_document_number_check CHECK (length(number) BETWEEN 1 AND 40),
  plant_id             uuid NOT NULL,    -- tenancy context: an org unit of kind plant
  storage_location_id  uuid NOT NULL,    -- tenancy context: a storage location under the plant
  document_date        date NOT NULL,
  status               text NOT NULL DEFAULT 'counting' CONSTRAINT count_document_status_check
                         CHECK (status IN ('counting', 'counted', 'pending_approval', 'posted', 'rejected', 'withdrawn')),
  version              integer NOT NULL DEFAULT 1 CONSTRAINT count_document_version_check CHECK (version >= 1),
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT count_document_number_once UNIQUE (tenant_id, legal_entity_id, fiscal_year, number),
  -- The key its items name their location by (count_item_location_fkey).
  CONSTRAINT count_document_location UNIQUE (tenant_id, id, plant_id, storage_location_id)
);
-- The list: the tenant's documents, the latest first, in the order its pages are read.
CREATE INDEX count_document_by_created_at ON inventory.count_document (tenant_id, created_at, id);
CREATE TRIGGER count_document_touch BEFORE UPDATE ON inventory.count_document FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('inventory.count_document'::regclass);
REVOKE UPDATE ON inventory.count_document FROM erp_app;
GRANT UPDATE (status, version, updated_by) ON inventory.count_document TO erp_app;
-- A document moves forward only: counting to counted once every item is
-- counted; counted to posted, or to pending_approval when its differences
-- wait for approval; pending_approval to posted, rejected or withdrawn.
CREATE FUNCTION inventory.count_document_status_flow() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status <> OLD.status AND NOT ((OLD.status, NEW.status) IN (('counting', 'counted'), ('counted', 'pending_approval'), ('counted', 'posted'),
      ('pending_approval', 'posted'), ('pending_approval', 'rejected'), ('pending_approval', 'withdrawn'))) THEN
    RAISE EXCEPTION 'count_document_status_flow: count document %: % does not become %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'count_document_status_flow';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER count_document_status_flow BEFORE UPDATE OF status ON inventory.count_document
  FOR EACH ROW EXECUTE FUNCTION inventory.count_document_status_flow();

-- A count item: a material at the document's storage location, its book
-- quantity in its base unit as the document was created, and once
-- counted the quantity counted in the base unit with the quantity and
-- unit it was entered in, when and by whom. open is true until its
-- document is posted, rejected or withdrawn (S2d).
CREATE TABLE inventory.count_item (
  tenant_id            uuid NOT NULL,
  count_document_id    uuid NOT NULL,
  item_no              smallint NOT NULL CONSTRAINT count_item_item_no_check CHECK (item_no BETWEEN 1 AND 100),
  material_id          uuid NOT NULL,    -- materials context
  plant_id             uuid NOT NULL,    -- tenancy context: the document's plant
  storage_location_id  uuid NOT NULL,    -- tenancy context: the document's storage location
  book_quantity        numeric(20,6) NOT NULL CONSTRAINT count_item_book_quantity_check CHECK (book_quantity >= 0),
  counted_quantity     numeric(20,6) CONSTRAINT count_item_counted_quantity_check CHECK (counted_quantity >= 0),
  entry_quantity       numeric(20,6) CONSTRAINT count_item_entry_quantity_check CHECK (entry_quantity >= 0),
  entry_uom_code       text REFERENCES ref.unit_of_measure,
  counted_at           timestamptz,
  counted_by           uuid,
  open                 boolean NOT NULL DEFAULT true,
  PRIMARY KEY (tenant_id, count_document_id, item_no),
  CONSTRAINT count_item_location_fkey FOREIGN KEY (tenant_id, count_document_id, plant_id, storage_location_id)
    REFERENCES inventory.count_document (tenant_id, id, plant_id, storage_location_id),
  CONSTRAINT count_item_material_once UNIQUE (tenant_id, count_document_id, material_id),
  -- A count is entered whole: its quantity, entry, time and person together.
  CONSTRAINT count_item_count_check CHECK ((counted_quantity IS NULL) = (entry_quantity IS NULL) AND (counted_quantity IS NULL) = (entry_uom_code IS NULL)
    AND (counted_quantity IS NULL) = (counted_at IS NULL) AND (counted_quantity IS NULL) = (counted_by IS NULL))
);
-- One open count of a material at a storage location; also what the
-- posting block looks a movement's location up by.
CREATE UNIQUE INDEX count_item_one_open ON inventory.count_item (tenant_id, material_id, plant_id, storage_location_id) WHERE open;
-- The list's material filter.
CREATE INDEX count_item_by_material ON inventory.count_item (tenant_id, material_id);
SELECT platform.secure_table('inventory.count_item'::regclass);
REVOKE UPDATE ON inventory.count_item FROM erp_app;
GRANT UPDATE (counted_quantity, entry_quantity, entry_uom_code, counted_at, counted_by, open) ON inventory.count_item TO erp_app;
-- An item is counted once (a recount is a new document) and closed once;
-- what it counts and its book quantity never change.
CREATE FUNCTION inventory.count_item_once() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.counted_quantity IS NOT NULL AND (NEW.counted_quantity, NEW.entry_quantity, NEW.entry_uom_code, NEW.counted_at, NEW.counted_by)
        IS DISTINCT FROM (OLD.counted_quantity, OLD.entry_quantity, OLD.entry_uom_code, OLD.counted_at, OLD.counted_by))
      OR (NOT OLD.open AND NEW.open)
      OR (NEW.material_id, NEW.plant_id, NEW.storage_location_id, NEW.book_quantity, NEW.item_no, NEW.count_document_id)
        IS DISTINCT FROM (OLD.material_id, OLD.plant_id, OLD.storage_location_id, OLD.book_quantity, OLD.item_no, OLD.count_document_id) THEN
    RAISE EXCEPTION 'count_item_once: count document % item %: a count is entered once and an item closed once', OLD.count_document_id, OLD.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'count_item_once';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER count_item_once BEFORE UPDATE ON inventory.count_item FOR EACH ROW EXECUTE FUNCTION inventory.count_item_once();

-- The posting block (SAP MI01's indicator, always set here): no goods
-- movement moves a material at a storage location while a count of it
-- there is open, whatever posts it. The application refuses it first,
-- naming the document; this refuses what passes it.
CREATE FUNCTION inventory.count_posting_block() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT FROM inventory.count_item i WHERE i.tenant_id = NEW.tenant_id AND i.material_id = NEW.material_id AND i.plant_id = NEW.plant_id
      AND i.storage_location_id = NEW.storage_location_id AND i.open) THEN
    RAISE EXCEPTION 'count_posting_block: material % is being counted at storage location %', NEW.material_id, NEW.storage_location_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'count_posting_block';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER count_posting_block BEFORE INSERT ON inventory.movement FOR EACH ROW EXECUTE FUNCTION inventory.count_posting_block();

-- Creating count documents and entering counts (ADR-0082 As built S2c);
-- reading them is inventory.stock.read's.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('inventory.count.write', 'Create physical inventory count documents, which block postings of their materials, and enter counts', false);
