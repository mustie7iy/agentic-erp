-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000490's state by adding it back NOT VALID.
ALTER TABLE payments.house_bank
  DROP CONSTRAINT house_bank_code_check,
  ADD CONSTRAINT house_bank_code_check CHECK (code ~ '^[A-Z0-9]{1,10}$') NOT VALID;
