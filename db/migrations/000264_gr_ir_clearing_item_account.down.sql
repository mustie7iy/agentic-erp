ALTER TABLE procurement.gr_ir_clearing_item
  DROP CONSTRAINT gr_ir_clearing_item_account,
  DROP COLUMN offset_account_name,
  DROP COLUMN offset_account_number,
  DROP COLUMN offset_account_id;
