-- The revert is refused while a lease run exists: its entries are posted
-- in the ledger, and dropping the runs would lose which schedule lines
-- they posted, so the next run would post them again. The keys the run
-- reads leave the account determination, whose restored check refuses
-- them.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM leases.run) THEN
    RAISE EXCEPTION 'leases.run holds runs whose entries are posted: reverting would post their lines again'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM identity.sod_rule WHERE 'leases.run.post' IN (permission_a, permission_b);
DELETE FROM identity.role_permission WHERE permission_code = 'leases.run.post';
DELETE FROM ref.permission WHERE code = 'leases.run.post';
DROP TABLE leases.run_posting;
DROP FUNCTION leases.check_run_posting();
DROP TABLE leases.run;
DROP FUNCTION leases.check_run();
DROP INDEX leases.contract_by_company;
DELETE FROM ledger.account_determination WHERE key IN ('lease_interest', 'right_of_use_depreciation', 'right_of_use_accumulated_depreciation');
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability'));
