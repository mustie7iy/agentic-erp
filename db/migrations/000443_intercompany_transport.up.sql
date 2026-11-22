-- Intercompany stock transport orders (S2i1; ADR-0082 As built S2i;
-- 03-data-model §3.8, §3.35). Between plants of two companies the order is
-- the receiving company's purchase order naming the plant that supplies it
-- (SAP's cross-company stock transport order, a purchasing document with a
-- supplying plant, from memory): it has a supplier (the supplying
-- company's business partner), prices (the transfer price) and GR/IR,
-- which S2h's order within a company has not. The supplying company posts
-- its goods issue (643, out of its stock against cost of goods sold) once
-- for the whole order; the receiving company receives it as any order
-- (101 against GR/IR), up to what was issued. Expand only (06 §6.4):
-- columns the previous version never sets, checks widened NOT VALID
-- (validated by 000444), a trigger on what the previous version never
-- changes.

-- The supplying plant (tenancy context: a plant of another company of
-- the tenant), fixed at creation, and the goods issue's material document
-- (inventory context) and day, once posted.
ALTER TABLE procurement.purchase_order
  ADD COLUMN supplying_plant_id uuid,
  ADD COLUMN issue_document_id uuid,
  ADD COLUMN issue_document_number text CONSTRAINT purchase_order_issue_document_number_check CHECK (length(issue_document_number) BETWEEN 1 AND 40),
  ADD COLUMN issue_date date,
  -- An order's goods issue is all three or none, and only an order with
  -- a supplying plant has one.
  ADD CONSTRAINT purchase_order_issue CHECK ((issue_document_id IS NULL) = (issue_document_number IS NULL)
    AND (issue_document_id IS NULL) = (issue_date IS NULL) AND (issue_document_id IS NULL OR supplying_plant_id IS NOT NULL));

-- The supplying plant never changes, and a goods issue once posted stays
-- as posted.
CREATE FUNCTION procurement.purchase_order_supply_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.supplying_plant_id IS DISTINCT FROM OLD.supplying_plant_id
     OR (OLD.issue_document_id IS NOT NULL AND (NEW.issue_document_id, NEW.issue_document_number, NEW.issue_date)
       IS DISTINCT FROM (OLD.issue_document_id, OLD.issue_document_number, OLD.issue_date)) THEN
    RAISE EXCEPTION 'purchase_order_supply_fixed: order %: its supplying plant and goods issue stay as set', OLD.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_supply_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.purchase_order_supply_fixed() OWNER TO erp_owner;
CREATE TRIGGER purchase_order_supply_fixed BEFORE UPDATE ON procurement.purchase_order FOR EACH ROW
  EXECUTE FUNCTION procurement.purchase_order_supply_fixed();

-- The goods issue of an intercompany stock transport order (643) names the
-- order's number.
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '351', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302', '601', '602', '643',
                             '701', '702')) NOT VALID,
  DROP CONSTRAINT material_document_reference,
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102', '351', '601', '602', '643'))) NOT VALID;
