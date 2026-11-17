-- The revert refuses while an RFQ from a requisition exists (the previous
-- version would show it without its requisition and the requisition in
-- sourcing for good): the requisition status check restored below fails
-- on a requisition in sourcing, and this on an RFQ that names one.
DO $$
BEGIN
  IF EXISTS (SELECT FROM procurement.rfq WHERE requisition_id IS NOT NULL) THEN
    RAISE EXCEPTION 'RFQs created from requisitions exist: 000350 cannot be reverted' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TRIGGER rfq_item_of_requisition ON procurement.rfq_item;
DROP FUNCTION procurement.check_rfq_item_requisition();
-- check_rfq as 000297 left it.
CREATE OR REPLACE FUNCTION procurement.check_rfq() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'draft' THEN
      RAISE EXCEPTION 'RFQ % starts as a draft', NEW.id USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_starts_draft';
    END IF;
    RETURN NEW;
  END IF;
  IF (NEW.legal_entity_id, NEW.document_number, NEW.currency, NEW.due_date)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.document_number, OLD.currency, OLD.due_date) THEN
    RAISE EXCEPTION 'RFQ % keeps its company, number, currency and due date', OLD.id
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
ALTER TABLE procurement.rfq_item
  DROP COLUMN requisition_item_no,
  DROP COLUMN delivery_date;
DROP INDEX procurement.rfq_by_requisition;
ALTER TABLE procurement.rfq
  DROP CONSTRAINT rfq_requisition_fkey,
  DROP COLUMN requisition_id;
ALTER TABLE procurement.requisition
  DROP CONSTRAINT requisition_status_check,
  ADD CONSTRAINT requisition_status_check CHECK (status IN ('draft', 'pending_approval', 'approved', 'rejected', 'cancelled', 'ordered'));
