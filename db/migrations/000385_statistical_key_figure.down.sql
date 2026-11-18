-- Cycles by key figure go with their runs and receivers, which a version
-- without key figures cannot read; their entries stay in the journal.
DELETE FROM identity.sod_rule WHERE permission_a IN ('controlling.key_figure.read', 'controlling.key_figure.write')
  OR permission_b IN ('controlling.key_figure.read', 'controlling.key_figure.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('controlling.key_figure.read', 'controlling.key_figure.write');
DELETE FROM ref.permission WHERE code IN ('controlling.key_figure.read', 'controlling.key_figure.write');
DELETE FROM controlling.allocation_run WHERE (tenant_id, cycle_id) IN (SELECT tenant_id, id FROM controlling.allocation_cycle WHERE share_basis <> 'fixed_percentages');
DELETE FROM controlling.allocation_receiver WHERE (tenant_id, cycle_id) IN (SELECT tenant_id, id FROM controlling.allocation_cycle WHERE share_basis <> 'fixed_percentages');
DELETE FROM controlling.allocation_cycle WHERE share_basis <> 'fixed_percentages';

-- 000283's rule.
CREATE OR REPLACE FUNCTION controlling.allocation_shares_whole(tenant uuid, cycle uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  total bigint;
  sender uuid;
BEGIN
  SELECT sender_cost_center_id INTO sender FROM controlling.allocation_cycle WHERE tenant_id = tenant AND id = cycle;
  SELECT coalesce(sum(share_bp), 0) INTO total FROM controlling.allocation_receiver WHERE tenant_id = tenant AND cycle_id = cycle;
  IF total <> 10000 THEN
    RAISE EXCEPTION 'the shares of allocation cycle % add up to % basis points, not 10000', cycle, total
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_shares_whole';
  END IF;
  IF EXISTS (SELECT 1 FROM controlling.allocation_receiver WHERE tenant_id = tenant AND cycle_id = cycle AND cost_center_id = sender) THEN
    RAISE EXCEPTION 'allocation cycle % names its sender as a receiver', cycle
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_receiver_not_sender';
  END IF;
END $$;
ALTER TABLE controlling.allocation_receiver ALTER COLUMN share_bp SET NOT NULL;

-- 000284's function.
CREATE OR REPLACE FUNCTION controlling.check_allocation_cycle() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.legal_entity_id, NEW.code, NEW.kind, NEW.sender_cost_center_id, NEW.assessment_account_id)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.code, OLD.kind, OLD.sender_cost_center_id, OLD.assessment_account_id) THEN
    RAISE EXCEPTION 'allocation cycle % keeps its company, code, kind, sender and assessment account', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_cycle_identity_kept';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE controlling.allocation_cycle DROP CONSTRAINT allocation_cycle_key_figure_company, DROP CONSTRAINT allocation_cycle_key_figure,
  DROP CONSTRAINT allocation_cycle_share_basis, DROP COLUMN statistical_key_figure_id, DROP COLUMN share_basis;
DROP TABLE controlling.statistical_key_figure_value;
DROP TABLE controlling.statistical_key_figure_posting;
DROP TABLE controlling.statistical_key_figure;
DROP FUNCTION controlling.check_statistical_key_figure();
