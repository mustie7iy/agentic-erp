DELETE FROM ref.permission WHERE code IN ('consolidation.difference.accept', 'consolidation.run.finalize');
DROP TRIGGER run_difference_transition ON consolidation.run_difference;
DROP FUNCTION consolidation.check_run_difference();
REVOKE UPDATE (status, accepted_by, accepted_at, reason, version) ON consolidation.run_difference FROM erp_app;
ALTER TABLE consolidation.run_difference DROP CONSTRAINT run_difference_version;
ALTER TABLE consolidation.run_difference DROP CONSTRAINT run_difference_accepted;
ALTER TABLE consolidation.run_difference DROP CONSTRAINT run_difference_status;
ALTER TABLE consolidation.run_difference ADD CONSTRAINT run_difference_status CHECK (CASE status WHEN 'matched' THEN difference = 0
  WHEN 'within_tolerance' THEN difference <> 0 AND abs(difference) <= tolerance WHEN 'open' THEN abs(difference) > tolerance ELSE true END);
ALTER TABLE consolidation.run_difference DROP CONSTRAINT run_difference_pair;
ALTER TABLE consolidation.run_difference ADD CONSTRAINT run_difference_pair CHECK (CASE WHEN status IN ('matched', 'within_tolerance', 'open')
  THEN partner_amount IS NOT NULL AND tolerance IS NOT NULL AND difference IS NOT DISTINCT FROM amount + partner_amount
  ELSE partner_amount IS NULL AND difference IS NULL AND tolerance IS NULL END);
ALTER TABLE consolidation.run_difference DROP CONSTRAINT run_difference_status_check;
ALTER TABLE consolidation.run_difference ADD CONSTRAINT run_difference_status_check
  CHECK (status IN ('matched', 'within_tolerance', 'open', 'outside_group', 'no_trading_partner'));
ALTER TABLE consolidation.run_difference DROP COLUMN version, DROP COLUMN reason, DROP COLUMN accepted_at, DROP COLUMN accepted_by;
