-- A house bank's code is per account (product owner 2026-11-10; ADR-0038
-- As built, a house bank's account by payment method): a house bank of
-- one SAP account keeps its code (HBKID, at most five characters), and an
-- account of a house bank of several is coded house bank and account ID
-- (HBKID + HKTID), which T042I names per payment method and currency. So
-- the code takes up to ten upper-case letters or digits.
-- Expand only: the check is widened NOT VALID (validated by 000498); every
-- code the previous version wrote holds under it.
ALTER TABLE payments.house_bank
  DROP CONSTRAINT house_bank_code_check,
  ADD CONSTRAINT house_bank_code_check CHECK (code ~ '^[A-Z0-9]{1,10}$') NOT VALID;
