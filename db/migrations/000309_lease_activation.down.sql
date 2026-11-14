-- The revert is refused while an active contract exists: the previous
-- version knows drafts alone, and an activation that posted is not undone
-- silently (and an active contract's journal entry names the source
-- context leases). The keys the activation reads leave the account
-- determination, whose restored check refuses them.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM leases.contract WHERE status = 'active') THEN
    RAISE EXCEPTION 'leases.contract holds active contracts: reverting would drop what their activation posted'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM identity.sod_rule WHERE permission_a = 'leases.contract.activate' OR permission_b = 'leases.contract.activate';
DELETE FROM identity.role_permission WHERE permission_code = 'leases.contract.activate';
DELETE FROM ref.permission WHERE code = 'leases.contract.activate';
DELETE FROM ledger.account_determination WHERE key IN ('right_of_use_asset', 'lease_liability');
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings'));
-- The previous version posts no lease entry; the narrower check is
-- restored NOT VALID, as 000280's down restores its own: it holds for
-- every new row and reads none of the old.
ALTER TABLE ledger.journal_entry DROP CONSTRAINT journal_entry_source_context_check;
ALTER TABLE ledger.journal_entry ADD CONSTRAINT journal_entry_source_context_check
  CHECK (source_context IN ('ledger', 'payables', 'payments', 'procurement', 'receivables', 'assets', 'integration', 'controlling')) NOT VALID;
DROP TABLE leases.schedule_line;
REVOKE UPDATE (status, version, updated_by, lease_liability, right_of_use_amount, journal_entry_id, journal_entry_year, journal_entry_number)
  ON leases.contract FROM erp_app;
DROP TRIGGER lease_contract_fixed_once_active ON leases.contract;
DROP FUNCTION leases.check_contract_change();
ALTER TABLE leases.contract
  DROP CONSTRAINT contract_right_of_use_covers_liability,
  DROP CONSTRAINT contract_activation_check,
  DROP COLUMN journal_entry_number,
  DROP COLUMN journal_entry_year,
  DROP COLUMN journal_entry_id,
  DROP COLUMN right_of_use_amount,
  DROP COLUMN lease_liability,
  DROP CONSTRAINT contract_status_check,
  ADD CONSTRAINT contract_status_check CHECK (status IN ('draft'));
