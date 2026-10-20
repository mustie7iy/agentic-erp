-- Canadian accounts cannot be kept by the previous schema; the down
-- refuses rather than drop the bank of an account money is paid to. The
-- narrower scheme check comes back NOT VALID, as 000102's down does.
SET LOCAL row_security = off;
DO $$
BEGIN
  IF EXISTS (SELECT FROM partner.bank_account WHERE account_scheme = 'ca')
    OR EXISTS (SELECT FROM payments.house_bank WHERE institution_number IS NOT NULL) THEN
    RAISE EXCEPTION 'Canadian bank accounts exist';
  END IF;
END $$;
ALTER TABLE payments.house_bank DROP CONSTRAINT house_bank_ca_routing;
ALTER TABLE payments.house_bank DROP COLUMN transit_number, DROP COLUMN institution_number;
ALTER TABLE partner.bank_account DROP CONSTRAINT bank_account_ca_routing, DROP CONSTRAINT bank_account_account_scheme_check;
ALTER TABLE partner.bank_account ADD CONSTRAINT bank_account_account_scheme_check CHECK (account_scheme IN ('iban', 'domestic')) NOT VALID;
ALTER TABLE partner.bank_account DROP COLUMN transit_number, DROP COLUMN institution_number;
