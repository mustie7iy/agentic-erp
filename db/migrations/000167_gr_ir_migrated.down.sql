ALTER TABLE ledger.gr_ir_position ALTER COLUMN is_open
  SET EXPRESSION AS (received <> invoiced + cleared OR amount_tc <> 0 OR amount_lc <> 0 OR amount_gc <> 0);
ALTER TABLE ledger.gr_ir_position DROP COLUMN migrated;
