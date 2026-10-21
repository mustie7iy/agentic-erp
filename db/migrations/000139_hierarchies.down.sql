UPDATE ref.permission SET description = 'Approve center changes: creation, validity, locks, profit center, segment and companies'
WHERE code = 'ledger.cost_center.approve';
DROP TRIGGER hierarchy_member_placed ON ledger.hierarchy_member;
DROP TRIGGER profit_center_placed ON ledger.profit_center;
DROP TRIGGER cost_center_placed ON ledger.cost_center;
DROP FUNCTION ledger.check_center_placed();
DROP TABLE ledger.hierarchy_member;
DROP TABLE ledger.hierarchy_node;
DELETE FROM ledger.center_change WHERE hierarchy_id IS NOT NULL;
CREATE OR REPLACE FUNCTION ledger.check_center_change_decision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.cost_center_id, NEW.profit_center_id, NEW.kind, NEW.payload, NEW.reason, NEW.requested_by)
     IS DISTINCT FROM (OLD.cost_center_id, OLD.profit_center_id, OLD.kind, OLD.payload, OLD.reason, OLD.requested_by) THEN
    RAISE EXCEPTION 'center_change: what a change asks for is fixed' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.status IS DISTINCT FROM OLD.status AND OLD.status <> 'pending' THEN
    RAISE EXCEPTION 'center_change: a change cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.workflow_request_id IS DISTINCT FROM OLD.workflow_request_id AND OLD.workflow_request_id IS NOT NULL THEN
    RAISE EXCEPTION 'center_change: the approval request of a change is fixed' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
DROP INDEX ledger.center_change_by_hierarchy;
ALTER TABLE ledger.center_change DROP CONSTRAINT center_change_hierarchy, DROP CONSTRAINT center_change_one_center,
  ADD CONSTRAINT center_change_one_center CHECK (num_nonnulls(cost_center_id, profit_center_id) = 1);
ALTER TABLE ledger.center_change DROP CONSTRAINT center_change_kind_check,
  ADD CONSTRAINT center_change_kind_check CHECK (kind IN ('create', 'redate', 'lock', 'move', 'assign'));
ALTER TABLE ledger.center_change DROP COLUMN hierarchy_id;
DROP TABLE ledger.hierarchy;
DROP FUNCTION ledger.check_hierarchy_member();
DROP FUNCTION ledger.check_hierarchy_node();
DROP FUNCTION ledger.check_hierarchy_fixed();
