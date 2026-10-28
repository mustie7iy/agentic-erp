DELETE FROM identity.sod_rule WHERE permission_a IN ('procurement.gr_ir.clear', 'procurement.gr_ir.clear.approve')
  OR permission_b IN ('procurement.gr_ir.clear', 'procurement.gr_ir.clear.approve');
DELETE FROM identity.role_permission WHERE permission_code IN ('procurement.gr_ir.clear', 'procurement.gr_ir.clear.approve');
DELETE FROM ref.permission WHERE code IN ('procurement.gr_ir.clear', 'procurement.gr_ir.clear.approve');
DROP TABLE procurement.gr_ir_clearing_item;
DROP TABLE procurement.gr_ir_clearing;
DROP FUNCTION procurement.check_gr_ir_clearing();
DROP TABLE ledger.gr_ir_clearing_entry;
ALTER TABLE ledger.gr_ir_position ALTER COLUMN is_open
  SET EXPRESSION AS (received <> invoiced OR amount_tc <> 0 OR amount_lc <> 0 OR amount_gc <> 0);
ALTER TABLE ledger.gr_ir_position DROP COLUMN cleared;
