-- Contract renewal and expiry alerts (T47g, 03-data-model §3.8). A
-- contract is renewed by a draft successor that continues it: from the
-- day after it ends, in its legal entity, with its supplier and currency;
-- a contract has one live renewal at a time (a cancelled one does not
-- count). An active contract nearing its end without a renewal is
-- alerted once (ContractExpiring): expiry_alerted_on is set once and
-- never moves.
ALTER TABLE procurement.contract
  ADD COLUMN renewal_of_id uuid,
  ADD COLUMN expiry_alerted_on date,
  ADD CONSTRAINT contract_renewal_fk FOREIGN KEY (tenant_id, renewal_of_id) REFERENCES procurement.contract (tenant_id, id),
  ADD CONSTRAINT contract_not_its_own_renewal CHECK (renewal_of_id <> id);
CREATE UNIQUE INDEX contract_renewed_once ON procurement.contract (tenant_id, renewal_of_id)
  WHERE renewal_of_id IS NOT NULL AND status <> 'cancelled';

CREATE FUNCTION procurement.check_contract_renewal() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE p procurement.contract%ROWTYPE;
BEGIN
  IF TG_OP = 'UPDATE' AND OLD.expiry_alerted_on IS NOT NULL AND NEW.expiry_alerted_on IS DISTINCT FROM OLD.expiry_alerted_on THEN
    RAISE EXCEPTION 'contract % was alerted of its expiry once', OLD.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'contract_alerted_once';
  END IF;
  IF NEW.renewal_of_id IS NULL OR NEW.renewal_of_id = NEW.id THEN
    RETURN NEW;
  END IF;
  SELECT * INTO p FROM procurement.contract WHERE tenant_id = NEW.tenant_id AND id = NEW.renewal_of_id;
  IF NOT FOUND THEN
    RETURN NEW; -- contract_renewal_fk refuses it
  END IF;
  IF (NEW.legal_entity_id, NEW.supplier_id, NEW.currency) IS DISTINCT FROM (p.legal_entity_id, p.supplier_id, p.currency)
     OR NEW.valid_from <> p.valid_to + 1 THEN
    RAISE EXCEPTION 'contract % renews % from the day after it ends, with its supplier and currency', NEW.document_number, p.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'contract_renewal_continues';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_contract_renewal() OWNER TO erp_owner;
CREATE TRIGGER contract_renewal BEFORE INSERT OR UPDATE ON procurement.contract
  FOR EACH ROW EXECUTE FUNCTION procurement.check_contract_renewal();

-- The expirer finds the tenants with contracts to alert across the cell.
GRANT SELECT (expiry_alerted_on) ON procurement.contract TO erp_worker;
