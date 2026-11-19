DELETE FROM identity.sod_rule WHERE permission_a = 'shipping.goods_issue.post' OR permission_b = 'shipping.goods_issue.post';
DELETE FROM identity.role_permission WHERE permission_code = 'shipping.goods_issue.post';
DELETE FROM ref.permission WHERE code = 'shipping.goods_issue.post';
-- A goods issue's rows the previous version cannot hold are refused
-- rather than dropped (06 §6.4: a down migration loses no data).
DO $$
BEGIN
  IF EXISTS (SELECT FROM inventory.material_document WHERE movement_type IN ('601', '602'))
     OR EXISTS (SELECT FROM ledger.account_determination WHERE key LIKE 'inventory_cost_of_goods_sold:%') THEN
    RAISE EXCEPTION 'goods issues of outbound deliveries exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
ALTER TABLE sales.delivery_item DROP CONSTRAINT delivery_item_cost, DROP COLUMN cost_currency, DROP COLUMN cost_amount;
ALTER TABLE sales.delivery DROP CONSTRAINT delivery_outbound_delivery_once, DROP CONSTRAINT delivery_goods_issue,
  DROP COLUMN material_document_number, DROP COLUMN material_document_id, DROP COLUMN outbound_delivery_number, DROP COLUMN outbound_delivery_id;
-- The checks go back to 000408's state: validated, with 000407's (S2d)
-- movement types 701 and 702 and key inventory_difference:<class>.
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability', 'lease_interest', 'right_of_use_depreciation',
                 'right_of_use_accumulated_depreciation', 'payment_difference_expense', 'payment_difference_revenue')
         OR key ~ '^inventory_(stock|receipt|initial_entry|consumption|scrapping|price_difference|revaluation|difference):[0-9A-Z]{1,4}$');
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302', '701', '702')),
  DROP CONSTRAINT material_document_reversal_check,
  ADD CONSTRAINT material_document_reversal_check
    CHECK ((reversal_of_id IS NOT NULL) = (movement_type IN ('102', '502', '562', '202', '552', '312', '302'))),
  DROP CONSTRAINT material_document_reference,
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102')));
