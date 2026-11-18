-- The revert is refused while a valued movement or a price change exists:
-- the previous version neither values nor posts them, and what they
-- posted is not undone silently (their journal entries name the source
-- context inventory). The keys of the valuation classes leave the
-- account determination, whose restored check refuses them.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM inventory.material_document WHERE journal_entry_id IS NOT NULL) OR EXISTS (SELECT FROM inventory.price_change) THEN
    RAISE EXCEPTION 'inventory holds valued documents or price changes: reverting would drop what they posted' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM identity.sod_rule WHERE permission_a IN ('inventory.valuation.read', 'inventory.valuation.write')
  OR permission_b IN ('inventory.valuation.read', 'inventory.valuation.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('inventory.valuation.read', 'inventory.valuation.write');
DELETE FROM ref.permission WHERE code IN ('inventory.valuation.read', 'inventory.valuation.write');
CREATE OR REPLACE FUNCTION inventory.movement_stock() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE inventory.stock SET quantity = quantity + NEW.quantity, updated_at = now()
  WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id AND storage_location_id = NEW.storage_location_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'a movement adds to a stock row written before it' USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'movement_stock';
  END IF;
  RETURN NULL;
END $$;
ALTER TABLE inventory.material_document DROP CONSTRAINT material_document_entry,
  DROP COLUMN journal_entry_id, DROP COLUMN journal_entry_year, DROP COLUMN journal_entry_number, DROP COLUMN currency;
ALTER TABLE inventory.movement DROP COLUMN amount, DROP COLUMN difference;
DROP TABLE inventory.price_change;
DROP FUNCTION inventory.price_change_valuation();
DROP TABLE inventory.valuation;
DROP FUNCTION inventory.valuation_fixed();
DELETE FROM ledger.account_determination WHERE key LIKE 'inventory\_%';
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability', 'lease_interest', 'right_of_use_depreciation',
                 'right_of_use_accumulated_depreciation', 'payment_difference_expense', 'payment_difference_revenue'));
-- The previous version posts no inventory entry; the narrower check is
-- restored NOT VALID, as 000327's down restores its own.
ALTER TABLE ledger.journal_entry DROP CONSTRAINT journal_entry_source_context_check;
ALTER TABLE ledger.journal_entry ADD CONSTRAINT journal_entry_source_context_check
  CHECK (source_context IN ('ledger', 'payables', 'payments', 'procurement', 'receivables', 'assets', 'integration', 'controlling', 'revenue',
                            'leases')) NOT VALID;
