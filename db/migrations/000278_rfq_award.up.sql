-- The award of an RFQ (T82d; ADR-0069 §7, plan-breadth-week2 §T82d): a
-- published RFQ is awarded once, to one of its quotations as a whole,
-- and a draft contract with that quotation's supplier is created from
-- it. The RFQ becomes `awarded` and names the contract (contract_id; the
-- contract's own representation reads its RFQ through it); the winning
-- quotation becomes `awarded` and the RFQ's others `not_awarded`.
-- Expand only: a nullable column, statuses added to two open
-- enumerations and a permission, which the previous version neither
-- writes nor relies on (06 §6.4); it shows an awarded RFQ by its status
-- and records no quotation for it, as for any RFQ that is not published.
ALTER TABLE procurement.rfq
  ADD COLUMN contract_id uuid,
  DROP CONSTRAINT rfq_status_check,
  ADD CONSTRAINT rfq_status_check CHECK (status IN ('draft', 'published', 'awarded')),
  -- An awarded RFQ names the contract the award created, and no other does.
  ADD CONSTRAINT rfq_awarded_to_contract CHECK ((status = 'awarded') = (contract_id IS NOT NULL)),
  ADD CONSTRAINT rfq_contract_fkey FOREIGN KEY (tenant_id, contract_id) REFERENCES procurement.contract (tenant_id, id),
  -- A contract comes from one RFQ at most; the index also finds a
  -- contract's RFQ (contractSelect).
  ADD CONSTRAINT rfq_contract_once UNIQUE (tenant_id, contract_id);

-- An RFQ is awarded from published, once, and stays awarded to its
-- contract. The award names one of the RFQ's quotations, marked awarded
-- before the RFQ moves, and a contract of the RFQ's company, in its
-- currency, with that quotation's supplier.
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

-- A quotation's outcome: recorded when written, then awarded or not
-- awarded, once, while its RFQ is still published (the award sets the
-- outcomes before it moves the RFQ; check_rfq_is_published reads the RFQ
-- FOR SHARE). One quotation of an RFQ is awarded.
ALTER TABLE procurement.supplier_quotation
  DROP CONSTRAINT supplier_quotation_status_check,
  ADD CONSTRAINT supplier_quotation_status_check CHECK (status IN ('recorded', 'awarded', 'not_awarded'));
CREATE UNIQUE INDEX supplier_quotation_one_awarded ON procurement.supplier_quotation (tenant_id, rfq_id) WHERE status = 'awarded';
CREATE FUNCTION procurement.check_supplier_quotation_outcome() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'recorded' THEN
      RAISE EXCEPTION 'supplier quotation % starts as recorded', NEW.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'supplier_quotation_starts_recorded';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.status <> 'recorded' OR NEW.status NOT IN ('awarded', 'not_awarded') THEN
    RAISE EXCEPTION 'supplier quotation % is % : its outcome is set once, awarded or not awarded', OLD.id, OLD.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'supplier_quotation_outcome_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_supplier_quotation_outcome() OWNER TO erp_owner;
CREATE TRIGGER supplier_quotation_outcome BEFORE INSERT OR UPDATE ON procurement.supplier_quotation
  FOR EACH ROW EXECUTE FUNCTION procurement.check_supplier_quotation_outcome();
CREATE TRIGGER supplier_quotation_outcome_of_published BEFORE UPDATE ON procurement.supplier_quotation
  FOR EACH ROW EXECUTE FUNCTION procurement.check_rfq_is_published();
-- What an award sets, and nothing else of a quotation (000277 revoked
-- UPDATE until this slice).
GRANT UPDATE (status, version, updated_by) ON procurement.supplier_quotation TO erp_app;

-- Awarding an RFQ commits the company to a supplier: a permission of its
-- own beside procurement.rfq.manage (ADR-0069 §6).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('procurement.rfq.award', 'Award a request for quotation to a supplier quotation, creating a draft contract', false);
