-- Reversed runs and their later attempts, and cycles by fixed amounts with
-- their runs, go: a version without them would read a reversed run as in
-- force, or a cycle without its amounts. Their entries stay in the
-- journal.
DROP TRIGGER allocation_run_in_force_once ON controlling.allocation_run;
DROP FUNCTION controlling.check_allocation_run_attempt();
DELETE FROM controlling.allocation_run_reversal;
DELETE FROM controlling.allocation_run r WHERE r.attempt > 1 OR EXISTS (SELECT FROM controlling.allocation_cycle c
  WHERE c.tenant_id = r.tenant_id AND c.id = r.cycle_id AND c.share_basis = 'fixed_amounts');
DELETE FROM controlling.allocation_receiver WHERE (tenant_id, cycle_id) IN (SELECT tenant_id, id FROM controlling.allocation_cycle WHERE share_basis = 'fixed_amounts');
DELETE FROM controlling.allocation_cycle WHERE share_basis = 'fixed_amounts';
DROP TABLE controlling.allocation_run_reversal;
ALTER TABLE controlling.allocation_run DROP COLUMN attempt;
DELETE FROM controlling.allocation_run WHERE account_count = 0;
ALTER TABLE controlling.allocation_run DROP CONSTRAINT allocation_run_account_count;
ALTER TABLE controlling.allocation_run ADD CONSTRAINT allocation_run_account_count_check CHECK (account_count > 0);

-- 000385's rule.
CREATE OR REPLACE FUNCTION controlling.allocation_shares_whole(tenant uuid, cycle uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  total bigint;
  shared bigint;
  receivers bigint;
  sender uuid;
  basis text;
BEGIN
  SELECT sender_cost_center_id, share_basis INTO sender, basis FROM controlling.allocation_cycle WHERE tenant_id = tenant AND id = cycle;
  SELECT coalesce(sum(share_bp), 0), count(share_bp), count(*) INTO total, shared, receivers
    FROM controlling.allocation_receiver WHERE tenant_id = tenant AND cycle_id = cycle;
  IF basis = 'fixed_percentages' AND (total <> 10000 OR shared <> receivers) THEN
    RAISE EXCEPTION 'the shares of allocation cycle % add up to % basis points, not 10000, or a receiver has none', cycle, total
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_shares_whole';
  END IF;
  IF basis <> 'fixed_percentages' AND (shared <> 0 OR receivers = 0) THEN
    RAISE EXCEPTION 'allocation cycle % takes its shares from %, so it has receivers and none has a share of its own', cycle, basis
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_shares_by_basis';
  END IF;
  IF EXISTS (SELECT 1 FROM controlling.allocation_receiver WHERE tenant_id = tenant AND cycle_id = cycle AND cost_center_id = sender) THEN
    RAISE EXCEPTION 'allocation cycle % names its sender as a receiver', cycle
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_not_sender';
  END IF;
END $$;
ALTER TABLE controlling.allocation_receiver DROP CONSTRAINT allocation_receiver_amount, DROP COLUMN amount;
ALTER TABLE controlling.allocation_cycle DROP CONSTRAINT allocation_cycle_fixed_amounts, DROP CONSTRAINT allocation_cycle_share_basis;
ALTER TABLE controlling.allocation_cycle ADD CONSTRAINT allocation_cycle_share_basis
  CHECK (share_basis IN ('fixed_percentages', 'statistical_key_figure'));
