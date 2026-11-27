-- A house bank coded longer than five characters, which the previous
-- version cannot hold, is refused rather than dropped (06 §6.4: a down
-- migration loses no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM payments.house_bank WHERE length(code) > 5) THEN
    RAISE EXCEPTION 'house banks coded by account (more than five characters) exist; the previous version cannot hold them'
      USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
END $$;
ALTER TABLE payments.house_bank
  DROP CONSTRAINT house_bank_code_check,
  ADD CONSTRAINT house_bank_code_check CHECK (code ~ '^[A-Z0-9]{1,5}$');
