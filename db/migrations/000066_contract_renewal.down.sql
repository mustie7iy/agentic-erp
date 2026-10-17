DROP TRIGGER contract_renewal ON procurement.contract;
DROP FUNCTION procurement.check_contract_renewal();
DROP INDEX procurement.contract_renewed_once;
ALTER TABLE procurement.contract
  DROP CONSTRAINT contract_not_its_own_renewal,
  DROP CONSTRAINT contract_renewal_fk,
  DROP COLUMN expiry_alerted_on,
  DROP COLUMN renewal_of_id;
