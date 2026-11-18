-- The revert refuses while an RFQ is cancelled or closed (the previous
-- version knows neither status): the status check restored below would
-- fail on it.
DO $$
BEGIN
  IF EXISTS (SELECT FROM procurement.rfq WHERE status IN ('cancelled', 'closed') OR closing_reason IS NOT NULL) THEN
    RAISE EXCEPTION 'cancelled or closed RFQs exist: 000369 cannot be reverted' USING ERRCODE = 'check_violation';
  END IF;
END $$;
-- check_rfq as 000361 left it.
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
  IF OLD.status = 'awarded' AND (NEW.status, NEW.contract_id, NEW.purchase_order_id, NEW.award_reason)
      IS DISTINCT FROM (OLD.status, OLD.contract_id, OLD.purchase_order_id, OLD.award_reason) THEN
    RAISE EXCEPTION 'RFQ % stays awarded to its contract or purchase order', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_stays_awarded';
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
      WHERE q.tenant_id = NEW.tenant_id AND q.rfq_id = NEW.id AND q.status = 'awarded' AND NEW.requisition_id IS NULL
        AND c.supplier_id = q.supplier_id AND c.legal_entity_id = NEW.legal_entity_id AND c.currency = NEW.currency) AND NOT EXISTS (
      SELECT FROM procurement.supplier_quotation q
      JOIN procurement.purchase_order o ON o.tenant_id = q.tenant_id AND o.id = NEW.purchase_order_id
      WHERE q.tenant_id = NEW.tenant_id AND q.rfq_id = NEW.id AND q.status = 'awarded'
        AND o.supplier_id = q.supplier_id AND o.legal_entity_id = NEW.legal_entity_id AND o.currency = NEW.currency
        AND o.requisition_id IS NOT DISTINCT FROM NEW.requisition_id) THEN
    RAISE EXCEPTION 'RFQ % is awarded to one of its quotations and a contract (not from a requisition) or a purchase order (naming its requisition) of its company, in its currency, with that quotation''s supplier', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_award_of_quotation';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE procurement.rfq
  DROP CONSTRAINT rfq_closing_reason_check,
  DROP CONSTRAINT rfq_status_check,
  ADD CONSTRAINT rfq_status_check CHECK (status IN ('draft', 'published', 'awarded')),
  DROP COLUMN closing_reason;
