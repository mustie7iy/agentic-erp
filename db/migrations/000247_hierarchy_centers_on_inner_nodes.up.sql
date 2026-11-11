-- Centers on inner nodes (ADR-0050 §3 as amended for the SAP specialist's
-- answer A10(d), po:2026-11-09): an SAP set may hold values beside its
-- subsets, and a center's standard group (CSKS-KHINR, CEPC-KHINR) need not
-- be a lowest-level one, so a node may hold centers and child nodes. The
-- tree rules of 000139 lose their two refusals of that: a node below a node
-- holding centers (check_hierarchy_node) and a center at a node with child
-- nodes (check_hierarchy_member). Every other rule is unchanged: a node
-- with child nodes or centers is not removed, a center sits at a node that
-- is not removed. Expand only: every tree valid before stays valid.

CREATE OR REPLACE FUNCTION ledger.check_hierarchy_node() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  parent_level integer;
  height integer;
BEGIN
  PERFORM FROM ledger.hierarchy WHERE tenant_id = NEW.tenant_id AND id = NEW.hierarchy_id FOR UPDATE;
  IF TG_OP = 'UPDATE' THEN
    IF OLD.removed_by_change_id IS NOT NULL THEN
      RAISE EXCEPTION 'hierarchy_node: a removed node is fixed' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.hierarchy_id IS DISTINCT FROM OLD.hierarchy_id OR (NEW.parent_id IS NULL) <> (OLD.parent_id IS NULL) THEN
      RAISE EXCEPTION 'hierarchy_node: a node stays in its hierarchy, and a root stays the root' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.removed_by_change_id IS NOT NULL AND (
         EXISTS (SELECT FROM ledger.hierarchy_node c WHERE c.tenant_id = NEW.tenant_id AND c.parent_id = NEW.id AND c.removed_by_change_id IS NULL)
         OR EXISTS (SELECT FROM ledger.hierarchy_member m WHERE m.tenant_id = NEW.tenant_id AND m.node_id = NEW.id AND m.removed_by_change_id IS NULL)) THEN
      RAISE EXCEPTION 'hierarchy_node: a node with child nodes or centers cannot be removed' USING ERRCODE = 'check_violation';
    END IF;
  END IF;
  IF NEW.parent_id IS NULL OR NEW.removed_by_change_id IS NOT NULL
     OR (TG_OP = 'UPDATE' AND NEW.parent_id = OLD.parent_id) THEN
    RETURN NEW;
  END IF;
  IF NOT EXISTS (SELECT FROM ledger.hierarchy_node p WHERE p.tenant_id = NEW.tenant_id AND p.id = NEW.parent_id AND p.removed_by_change_id IS NULL) THEN
    RAISE EXCEPTION 'hierarchy_node: the parent node is removed' USING ERRCODE = 'check_violation';
  END IF;
  -- The parent's ancestors, the parent included; reaching the node itself
  -- is a cycle.
  WITH RECURSIVE up (id, parent_id, level) AS (
    SELECT n.id, n.parent_id, 1 FROM ledger.hierarchy_node n WHERE n.tenant_id = NEW.tenant_id AND n.id = NEW.parent_id
    UNION
    SELECT n.id, n.parent_id, up.level + 1 FROM up JOIN ledger.hierarchy_node n ON n.tenant_id = NEW.tenant_id AND n.id = up.parent_id
    WHERE up.id <> NEW.id
  )
  SELECT CASE WHEN bool_or(id = NEW.id) THEN NULL ELSE max(level) END INTO parent_level FROM up;
  IF parent_level IS NULL THEN
    RAISE EXCEPTION 'hierarchy_node: the node would be its own ancestor' USING ERRCODE = 'check_violation';
  END IF;
  WITH RECURSIVE down (id, level) AS (
    SELECT NEW.id, 1
    UNION
    SELECT n.id, down.level + 1 FROM down JOIN ledger.hierarchy_node n
      ON n.tenant_id = NEW.tenant_id AND n.parent_id = down.id AND n.removed_by_change_id IS NULL
  )
  SELECT max(level) INTO height FROM down;
  IF parent_level + height > 12 THEN
    RAISE EXCEPTION 'hierarchy_node: a hierarchy has at most 12 levels' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION ledger.check_hierarchy_member() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM FROM ledger.hierarchy WHERE tenant_id = NEW.tenant_id AND id = NEW.hierarchy_id FOR UPDATE;
  IF TG_OP = 'UPDATE' AND (OLD.removed_by_change_id IS NOT NULL
     OR (NEW.hierarchy_id, NEW.kind, NEW.cost_center_id, NEW.profit_center_id) IS DISTINCT FROM (OLD.hierarchy_id, OLD.kind, OLD.cost_center_id, OLD.profit_center_id)) THEN
    RAISE EXCEPTION 'hierarchy_member: a place changes only its node, until it is removed' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.removed_by_change_id IS NULL
     AND NOT EXISTS (SELECT FROM ledger.hierarchy_node n WHERE n.tenant_id = NEW.tenant_id AND n.id = NEW.node_id AND n.removed_by_change_id IS NULL) THEN
    RAISE EXCEPTION 'hierarchy_member: a center sits at a node that is not removed' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
