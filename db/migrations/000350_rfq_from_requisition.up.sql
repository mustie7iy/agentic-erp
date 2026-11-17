-- An RFQ created from a requisition (T82e; ADR-0069 As built,
-- plan-depth-1 §T82e): a buyer asks suppliers to quote for what an
-- approved requisition asks for. The RFQ names the requisition
-- (requisition_id) and each of its items the requisition item it was
-- taken from (requisition_item_no), with the requisition's need date as
-- the item's delivery date; the requisition is `in_sourcing` from then
-- until an award orders it (T82f). A buyer's own RFQ may give each item a
-- delivery date too.
-- Expand only: nullable columns and a status added to an open
-- enumeration, which the previous version neither writes nor relies on
-- (06 §6.4). The wider status check and the foreign key are added NOT
-- VALID here and validated by 000351: every row already there holds
-- them.
ALTER TABLE procurement.requisition
  DROP CONSTRAINT requisition_status_check,
  ADD CONSTRAINT requisition_status_check
    CHECK (status IN ('draft', 'pending_approval', 'approved', 'rejected', 'cancelled', 'ordered', 'in_sourcing')) NOT VALID;

ALTER TABLE procurement.rfq
  ADD COLUMN requisition_id uuid,
  ADD CONSTRAINT rfq_requisition_fkey FOREIGN KEY (tenant_id, requisition_id) REFERENCES procurement.requisition (tenant_id, id) NOT VALID;
-- The list's filter and the requisition's RFQ (requisitionSelect).
CREATE INDEX rfq_by_requisition ON procurement.rfq (tenant_id, requisition_id, created_at) WHERE requisition_id IS NOT NULL;

ALTER TABLE procurement.rfq_item
  ADD COLUMN delivery_date date,
  ADD COLUMN requisition_item_no integer;   -- an item of the RFQ's requisition (check_rfq_item_requisition)

-- check_rfq as 000297 left it, and: an RFQ from a requisition is created
-- only while that requisition is in sourcing, of the RFQ's company and in
-- its currency, and no other RFQ of the requisition is still open (not
-- awarded); an RFQ keeps its requisition. The requisition is read FOR
-- SHARE, so it does not move on beside the insert.
CREATE OR REPLACE FUNCTION procurement.check_rfq() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'draft' THEN
      RAISE EXCEPTION 'RFQ % starts as a draft', NEW.id USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_starts_draft';
    END IF;
    IF NEW.requisition_id IS NOT NULL AND NOT EXISTS (
        SELECT FROM procurement.requisition r
        WHERE r.tenant_id = NEW.tenant_id AND r.id = NEW.requisition_id AND r.status = 'in_sourcing'
          AND r.legal_entity_id = NEW.legal_entity_id AND r.currency = NEW.currency
        FOR SHARE) THEN
      RAISE EXCEPTION 'RFQ % is created from a requisition in sourcing, of its company and in its currency', NEW.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_of_requisition_in_sourcing';
    END IF;
    IF NEW.requisition_id IS NOT NULL AND EXISTS (
        SELECT FROM procurement.rfq o
        WHERE o.tenant_id = NEW.tenant_id AND o.requisition_id = NEW.requisition_id AND o.id <> NEW.id AND o.status <> 'awarded') THEN
      RAISE EXCEPTION 'requisition % is sourced by one open RFQ at a time', NEW.requisition_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_one_open_per_requisition';
    END IF;
    RETURN NEW;
  END IF;
  IF (NEW.legal_entity_id, NEW.document_number, NEW.currency, NEW.due_date, NEW.requisition_id)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.document_number, OLD.currency, OLD.due_date, OLD.requisition_id) THEN
    RAISE EXCEPTION 'RFQ % keeps its company, number, currency and due date, and its requisition', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_identity_kept';
  END IF;
  IF OLD.status = 'awarded' AND (NEW.status, NEW.contract_id) IS DISTINCT FROM (OLD.status, OLD.contract_id) THEN
    RAISE EXCEPTION 'RFQ % stays awarded to its contract', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_stays_awarded';
  END IF;
  IF OLD.status = 'published' AND NEW.status NOT IN ('published', 'awarded') THEN
    RAISE EXCEPTION 'RFQ % stays published', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_stays_published';
  END IF;
  IF OLD.status = 'draft' AND NEW.status = 'awarded' THEN
    RAISE EXCEPTION 'RFQ % is a draft: only a published RFQ is awarded', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_awarded_from_published';
  END IF;
  IF OLD.status = 'published' AND NEW.status = 'awarded' AND NOT EXISTS (
      SELECT FROM procurement.supplier_quotation q
      JOIN procurement.contract c ON c.tenant_id = q.tenant_id AND c.id = NEW.contract_id
      WHERE q.tenant_id = NEW.tenant_id AND q.rfq_id = NEW.id AND q.status = 'awarded'
        AND c.supplier_id = q.supplier_id AND c.legal_entity_id = NEW.legal_entity_id AND c.currency = NEW.currency) THEN
    RAISE EXCEPTION 'RFQ % is awarded to one of its quotations and a contract of its company, in its currency, with that quotation''s supplier', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_award_of_quotation';
  END IF;
  RETURN NEW;
END $$;

-- An item of an RFQ from a requisition names an item of that requisition,
-- live, and an item of any other RFQ names none.
CREATE FUNCTION procurement.check_rfq_item_requisition() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  requisition uuid;
BEGIN
  SELECT requisition_id INTO requisition FROM procurement.rfq WHERE tenant_id = NEW.tenant_id AND id = NEW.rfq_id;
  IF (requisition IS NULL) <> (NEW.requisition_item_no IS NULL) OR (requisition IS NOT NULL AND NOT EXISTS (
      SELECT FROM procurement.requisition_item i
      WHERE i.tenant_id = NEW.tenant_id AND i.requisition_id = requisition AND i.item_no = NEW.requisition_item_no AND NOT i.is_removed)) THEN
    RAISE EXCEPTION 'item % of RFQ % names an item of the RFQ''s requisition exactly when the RFQ has one', NEW.item_no, NEW.rfq_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_item_of_requisition';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_rfq_item_requisition() OWNER TO erp_owner;
CREATE TRIGGER rfq_item_of_requisition BEFORE INSERT ON procurement.rfq_item
  FOR EACH ROW EXECUTE FUNCTION procurement.check_rfq_item_requisition();
