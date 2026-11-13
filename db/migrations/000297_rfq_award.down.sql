DELETE FROM identity.sod_rule WHERE permission_a = 'procurement.rfq.award' OR permission_b = 'procurement.rfq.award';
DELETE FROM identity.role_permission WHERE permission_code = 'procurement.rfq.award';
DELETE FROM ref.permission WHERE code = 'procurement.rfq.award';
REVOKE UPDATE (status, version, updated_by) ON procurement.supplier_quotation FROM erp_app;
DROP TRIGGER supplier_quotation_outcome_of_published ON procurement.supplier_quotation;
DROP TRIGGER supplier_quotation_outcome ON procurement.supplier_quotation;
DROP FUNCTION procurement.check_supplier_quotation_outcome();
DROP INDEX procurement.supplier_quotation_one_awarded;
-- The status checks restored refuse the revert while an awarded RFQ or a
-- quotation with an outcome exists: the previous version knows neither,
-- and the contract an award created stays.
ALTER TABLE procurement.supplier_quotation
  DROP CONSTRAINT supplier_quotation_status_check,
  ADD CONSTRAINT supplier_quotation_status_check CHECK (status IN ('recorded'));
-- check_rfq as 000285 created it.
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
  IF OLD.status = 'published' AND NEW.status <> 'published' THEN
    RAISE EXCEPTION 'RFQ % stays published', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'rfq_stays_published';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE procurement.rfq
  DROP CONSTRAINT rfq_contract_once,
  DROP CONSTRAINT rfq_contract_fkey,
  DROP CONSTRAINT rfq_awarded_to_contract,
  DROP CONSTRAINT rfq_status_check,
  ADD CONSTRAINT rfq_status_check CHECK (status IN ('draft', 'published')),
  DROP COLUMN contract_id;
