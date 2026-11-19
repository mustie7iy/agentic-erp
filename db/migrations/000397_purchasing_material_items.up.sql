-- Purchasing items by material and the catalog's keys mapped (S1f2;
-- ADR-0082 As built S1f; 03-data-model §3.9). An RFQ's item and a catalog
-- item may name a material of the tenant, whose number is then their key;
-- a requisition's item could always name one (000006) and is now checked
-- as it is entered, keeping the material's number. The material is the
-- materials context's, by id: no foreign key leaves a schema but ref's
-- (02 §2.1). An RFQ's items are
-- immutable (000285). A catalog item's material is set when it is loaded
-- or once by a mapping of the company's keys, and never changed after:
-- the application may update the column (beside 000287's), and
-- catalog_item_material_fixed refuses a changed one, for the tables'
-- owner too. Expand only (06 §6.4): nullable columns the previous version
-- neither reads nor writes, a grant it does not use and a rule it never
-- meets.
ALTER TABLE procurement.rfq_item ADD COLUMN material_id uuid;      -- materials context
-- A requisition item keeps the number of the material it names, as SAP's
-- item keeps MATNR: the number a reader and an RFQ from it name the
-- material by. Numbers never change (000373); an item that names no
-- material has none, and one entered before S1f2 may name a material
-- without it.
ALTER TABLE procurement.requisition_item ADD COLUMN material_number text
  CONSTRAINT requisition_item_material_number CHECK (material_number IS NULL OR (material_id IS NOT NULL AND length(material_number) BETWEEN 1 AND 40));
ALTER TABLE procurement.catalog_item ADD COLUMN material_id uuid;  -- materials context
GRANT UPDATE (material_id) ON procurement.catalog_item TO erp_app;

CREATE FUNCTION procurement.check_catalog_item_material() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.material_id IS NOT NULL AND NEW.material_id IS DISTINCT FROM OLD.material_id THEN
    RAISE EXCEPTION 'catalog item % keeps its material %', OLD.id, OLD.material_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'catalog_item_material_fixed';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER catalog_item_material_fixed BEFORE UPDATE OF material_id ON procurement.catalog_item
  FOR EACH ROW EXECUTE FUNCTION procurement.check_catalog_item_material();
