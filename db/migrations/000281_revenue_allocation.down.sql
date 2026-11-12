-- The revert is refused while an over-time obligation or an active
-- contract exists: the previous version writes point-in-time obligations
-- of drafts alone, and what a person judged or activated is not undone
-- silently (an over-time obligation would lose its service period). The
-- restored status check refuses an active contract; the timing has no
-- check that would refuse over time, so it is looked for here.
DO $$
BEGIN
  IF EXISTS (SELECT FROM revenue.performance_obligation WHERE timing = 'over_time') THEN
    RAISE EXCEPTION 'revenue.performance_obligation holds over-time obligations: reverting would drop their service periods'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM identity.sod_rule WHERE permission_a = 'revenue.contract.activate' OR permission_b = 'revenue.contract.activate';
DELETE FROM identity.role_permission WHERE permission_code = 'revenue.contract.activate';
DELETE FROM ref.permission WHERE code = 'revenue.contract.activate';
REVOKE UPDATE (timing, service_from, service_to, standalone_selling_price, allocated_amount) ON revenue.performance_obligation FROM erp_app;
DROP TRIGGER performance_obligation_fixed_once_active ON revenue.performance_obligation;
DROP FUNCTION revenue.check_obligation_change();
REVOKE UPDATE (status, version, updated_by) ON revenue.contract FROM erp_app;
DROP TRIGGER contract_fixed_once_active ON revenue.contract;
DROP FUNCTION revenue.check_contract_change();
ALTER TABLE revenue.performance_obligation
  DROP CONSTRAINT performance_obligation_service_period,
  DROP COLUMN allocated_amount,
  DROP COLUMN service_to,
  DROP COLUMN service_from;
ALTER TABLE revenue.contract
  DROP CONSTRAINT contract_status_check,
  ADD CONSTRAINT contract_status_check CHECK (status IN ('draft'));
