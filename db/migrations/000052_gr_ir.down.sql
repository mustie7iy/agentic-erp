ALTER TABLE procurement.goods_movement DROP CONSTRAINT goods_movement_entry_pair,
  DROP COLUMN journal_entry_id, DROP COLUMN journal_entry_year;

DROP TRIGGER purchase_order_valuation_fixed ON procurement.purchase_order;
DROP FUNCTION procurement.check_valuation_fixed();
ALTER TABLE procurement.purchase_order DROP CONSTRAINT purchase_order_draft_direct, DROP COLUMN valuation;

DROP TRIGGER journal_line_gr_ir ON ledger.journal_line;
DROP FUNCTION ledger.check_gr_ir_line();
DROP INDEX ledger.journal_line_order_item;
ALTER TABLE ledger.journal_line DROP CONSTRAINT journal_line_order_item_quantity, DROP CONSTRAINT journal_line_order_item_pair,
  DROP COLUMN po_item_no, DROP COLUMN purchase_order_id;

DELETE FROM ledger.account_determination WHERE key = 'price_difference';
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'cash_discount', 'fx_gain', 'fx_loss', 'rounding'));
