-- An RFQ cancelled or closed without an award (T82g; ADR-0069 As built,
-- plan-depth-2 §T82g): a buyer cancels a draft or published RFQ that is
-- no longer needed, or closes a published one whose quotations do not
-- lead to an award, with a reason (closing_reason). Its quotations are all
-- not awarded first (T82d's outcome), and a requisition it was created
-- from is approved again by the application in the same transaction; an
-- RFQ cancelled or closed no longer counts as the requisition's open RFQ.
-- Expand only: a nullable column and statuses added to an open
-- enumeration, which the previous version neither writes nor relies on
-- (06 §6.4). The wider status check and the reason's check are added NOT
-- VALID and validated by 000370: every row already there holds them.
ALTER TABLE procurement.rfq
  ADD COLUMN closing_reason text,
  DROP CONSTRAINT rfq_status_check,
  ADD CONSTRAINT rfq_status_check CHECK (status IN ('draft', 'published', 'awarded', 'cancelled', 'closed')) NOT VALID,
  -- A cancelled or closed RFQ keeps why, and no other RFQ has a reason
  -- of this kind.
  ADD CONSTRAINT rfq_closing_reason_check CHECK ((closing_reason IS NOT NULL) = (status IN ('cancelled', 'closed'))
    AND (closing_reason IS NULL OR (btrim(closing_reason) <> '' AND length(closing_reason) <= 500))) NOT VALID;

-- check_rfq as 000361 left it, and: a draft or published RFQ is cancelled,
-- a published one closed, each only once every quotation of it is not
-- awarded, and it stays so with its reason; only a draft or published
-- RFQ is a requisition's open RFQ.
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
        WHERE o.tenant_id = NEW.tenant_id AND o.requisition_id = NEW.requisition_id AND o.id <> NEW.id AND o.status IN ('draft', 'published')) THEN
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
  IF OLD.status IN ('cancelled', 'closed') AND (NEW.status, NEW.closing_reason, NEW.contract_id, NEW.purchase_order_id, NEW.award_reason)
      IS DISTINCT FROM (OLD.status, OLD.closing_reason, OLD.contract_id, OLD.purchase_order_id, OLD.award_reason) THEN
    RAISE EXCEPTION 'RFQ % stays % with its reason', OLD.id, OLD.status USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_stays_ended';
  END IF;
  IF OLD.status = 'awarded' AND (NEW.status, NEW.contract_id, NEW.purchase_order_id, NEW.award_reason)
      IS DISTINCT FROM (OLD.status, OLD.contract_id, OLD.purchase_order_id, OLD.award_reason) THEN
    RAISE EXCEPTION 'RFQ % stays awarded to its contract or purchase order', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_stays_awarded';
  END IF;
  IF OLD.status = 'published' AND NEW.status NOT IN ('published', 'awarded', 'cancelled', 'closed') THEN
    RAISE EXCEPTION 'RFQ % stays published', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_stays_published';
  END IF;
  IF OLD.status = 'draft' AND NEW.status = 'awarded' THEN
    RAISE EXCEPTION 'RFQ % is a draft: only a published RFQ is awarded', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_awarded_from_published';
  END IF;
  IF OLD.status = 'draft' AND NEW.status = 'closed' THEN
    RAISE EXCEPTION 'RFQ % is a draft: only a published RFQ is closed without an award, a draft is cancelled', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_closed_from_published';
  END IF;
  IF OLD.status = 'published' AND NEW.status IN ('cancelled', 'closed') AND EXISTS (
      SELECT FROM procurement.supplier_quotation q
      WHERE q.tenant_id = NEW.tenant_id AND q.rfq_id = NEW.id AND q.status <> 'not_awarded') THEN
    RAISE EXCEPTION 'RFQ % ends without an award: each of its quotations is not awarded first', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_ended_without_award';
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
