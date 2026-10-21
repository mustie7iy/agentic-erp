-- Canadian bank accounts (T45d; ADR-0038 amendment): an account in Canada
-- has no IBAN but an institution number (3 digits), a transit (branch)
-- number (5 digits) and an account number (7-12 digits). Partner accounts
-- take the scheme 'ca', whose institution and transit numbers sit beside
-- the sealed account number; they identify the bank, as a BIC does, so they
-- are kept in clear text. A house bank with both numbers is a Canadian
-- account, whose account number iban_cipher then holds. The checks are
-- added NOT VALID and validated by 000118 (06 §6.4): every row before them
-- is an IBAN or domestic account without the new numbers. The previous
-- version writes neither; it reads a 'ca' partner account's scheme as an
-- unknown value of the open enumeration and pays it with its account
-- number in Othr, which the bank refuses for want of the clearing member.
ALTER TABLE partner.bank_account
  ADD COLUMN institution_number text,
  ADD COLUMN transit_number     text;
ALTER TABLE partner.bank_account DROP CONSTRAINT bank_account_account_scheme_check;
ALTER TABLE partner.bank_account ADD CONSTRAINT bank_account_account_scheme_check
  CHECK (account_scheme IN ('iban', 'domestic', 'ca')) NOT VALID;
ALTER TABLE partner.bank_account ADD CONSTRAINT bank_account_ca_routing
  CHECK (CASE WHEN account_scheme = 'ca'
    THEN institution_number IS NOT NULL AND institution_number ~ '^[0-9]{3}$' AND transit_number IS NOT NULL AND transit_number ~ '^[0-9]{5}$'
    ELSE institution_number IS NULL AND transit_number IS NULL END) NOT VALID;
ALTER TABLE payments.house_bank
  ADD COLUMN institution_number text,
  ADD COLUMN transit_number     text;
ALTER TABLE payments.house_bank ADD CONSTRAINT house_bank_ca_routing
  CHECK ((institution_number IS NULL AND transit_number IS NULL)
    OR (institution_number IS NOT NULL AND institution_number ~ '^[0-9]{3}$' AND transit_number IS NOT NULL AND transit_number ~ '^[0-9]{5}$')) NOT VALID;
